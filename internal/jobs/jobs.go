// Package jobs runs background work (analysis, documentation generation)
// from a queue stored in the jobs table.
//
// Jobs survive restarts: a job that was running when the process stopped is
// put back in the queue on startup. Workers claim jobs with a single atomic
// UPDATE, so a job never runs twice at once. Handlers report progress, which
// the UI polls. Failures are retried with exponential backoff (or the delay
// a provider asked for) until a job's attempts are used up.
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// Status values (mirror the CHECK constraint on jobs.status).
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCanceled  = "canceled"
)

// Errors.
var (
	ErrNotFound      = errors.New("jobs: not found")
	ErrActive        = errors.New("jobs: a job of this type is already queued or running for the project")
	ErrNotCancelable = errors.New("jobs: job has already finished")
)

// Job is a unit of background work.
type Job struct {
	ID              int64
	UserID          int64
	ProjectID       int64
	Type            string
	Status          string
	ProgressCurrent int
	ProgressTotal   int
	ProgressMessage string
	Attempts        int
	MaxAttempts     int
	Payload         json.RawMessage
	Error           string
	RunAfter        time.Time
	StartedAt       time.Time
	FinishedAt      time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Active reports whether the job is queued or running.
func (j Job) Active() bool { return j.Status == StatusQueued || j.Status == StatusRunning }

// Percent is the progress as 0-100.
func (j Job) Percent() int {
	if j.Status == StatusSucceeded {
		return 100
	}
	if j.ProgressTotal <= 0 {
		return 0
	}
	p := j.ProgressCurrent * 100 / j.ProgressTotal
	return min(max(p, 0), 100)
}

// Progress lets a handler report how far it got.
type Progress interface {
	Update(current, total int, message string) error
}

// Definition describes how to run one job type.
type Definition struct {
	// Run does the work. It should honour ctx cancellation.
	Run func(ctx context.Context, job Job, progress Progress) error
	// OnFailed is called once when a job will not be retried (failed or
	// canceled), e.g. to reset the project's status. Optional.
	OnFailed func(ctx context.Context, job Job)
}

// Outcome decides what happens after a handler error.
type Outcome struct {
	Retry      bool
	RetryAfter time.Duration
	// Message is shown to the user; it must not contain internal details.
	Message string
}

// Options configure a Queue.
type Options struct {
	Now          func() time.Time
	Log          *slog.Logger
	PollInterval time.Duration
	// Classify maps handler errors to outcomes. The default never retries.
	Classify func(err error) Outcome
	// BaseBackoff is the delay before the first retry; it doubles per attempt.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

// Queue stores and runs jobs.
type Queue struct {
	db   *sql.DB
	opts Options
	defs map[string]Definition
	wake chan struct{}

	mu      sync.Mutex
	running map[int64]context.CancelFunc
}

// New returns a queue. Register job types before calling Run.
func New(db *sql.DB, opts Options) *Queue {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 2 * time.Second
	}
	if opts.Classify == nil {
		opts.Classify = func(error) Outcome { return Outcome{Message: "The job failed."} }
	}
	if opts.BaseBackoff <= 0 {
		opts.BaseBackoff = 30 * time.Second
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 15 * time.Minute
	}
	return &Queue{db: db, opts: opts, defs: map[string]Definition{}, wake: make(chan struct{}, 1), running: map[int64]context.CancelFunc{}}
}

// Register adds a job type.
func (q *Queue) Register(jobType string, def Definition) { q.defs[jobType] = def }

func (q *Queue) now() string { return q.opts.Now().UTC().Format(time.RFC3339) }

const columns = `id, user_id, COALESCE(project_id, 0), type, status, progress_current, progress_total, progress_message,
	attempts, max_attempts, payload_json, error, run_after, started_at, finished_at, created_at, updated_at`

// Enqueue adds a job. Only one queued or running job of a type may exist per
// project; a second returns ErrActive.
func (q *Queue) Enqueue(ctx context.Context, userID, projectID int64, jobType string, payload any, maxAttempts int) (Job, error) {
	if _, ok := q.defs[jobType]; !ok {
		return Job{}, fmt.Errorf("jobs: unknown job type %q", jobType)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Job{}, err
	}
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	var existing int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM jobs WHERE project_id = ? AND type = ? AND status IN ('queued', 'running') LIMIT 1`,
		projectID, jobType).Scan(&existing)
	if err == nil {
		return Job{}, ErrActive
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Job{}, err
	}
	now := q.now()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO jobs (user_id, project_id, type, status, max_attempts, payload_json, run_after, created_at, updated_at, progress_message)
		 VALUES (?, ?, ?, 'queued', ?, ?, ?, ?, ?, 'Waiting to start')`,
		userID, projectID, jobType, maxAttempts, string(raw), now, now, now)
	if err != nil {
		return Job{}, fmt.Errorf("insert job: %w", err)
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return Job{}, err
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return q.get(ctx, `id = ?`, id)
}

// Get returns job id if userID owns it.
func (q *Queue) Get(ctx context.Context, userID, id int64) (Job, error) {
	return q.get(ctx, `id = ? AND user_id = ?`, id, userID)
}

// Latest returns userID's most recent job for a project, optionally limited
// to some job types.
func (q *Queue) Latest(ctx context.Context, userID, projectID int64, types ...string) (Job, error) {
	where := `project_id = ? AND user_id = ?`
	args := []any{projectID, userID}
	if len(types) > 0 {
		where += ` AND type IN (?` + repeat(",?", len(types)-1) + `)`
		for _, t := range types {
			args = append(args, t)
		}
	}
	return q.get(ctx, where+` ORDER BY id DESC LIMIT 1`, args...)
}

// HasActive reports whether any job is queued or running for the project.
func (q *Queue) HasActive(ctx context.Context, projectID int64) (bool, error) {
	var n int
	err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE project_id = ? AND status IN ('queued', 'running')`, projectID).Scan(&n)
	return n > 0, err
}

func repeat(s string, n int) string {
	out := ""
	for range n {
		out += s
	}
	return out
}

func (q *Queue) get(ctx context.Context, where string, args ...any) (Job, error) {
	row := q.db.QueryRowContext(ctx, `SELECT `+columns+` FROM jobs WHERE `+where, args...)
	j, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return j, err
}

func scan(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var payload, runAfter, created, updated string
	var started, finished sql.NullString
	err := row.Scan(&j.ID, &j.UserID, &j.ProjectID, &j.Type, &j.Status, &j.ProgressCurrent, &j.ProgressTotal,
		&j.ProgressMessage, &j.Attempts, &j.MaxAttempts, &payload, &j.Error, &runAfter, &started, &finished, &created, &updated)
	if err != nil {
		return Job{}, err
	}
	j.Payload = json.RawMessage(payload)
	j.RunAfter, j.CreatedAt, j.UpdatedAt = parse(runAfter), parse(created), parse(updated)
	if started.Valid {
		j.StartedAt = parse(started.String)
	}
	if finished.Valid {
		j.FinishedAt = parse(finished.String)
	}
	return j, nil
}

func parse(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// Cancel stops a queued or running job owned by userID.
func (q *Queue) Cancel(ctx context.Context, userID, id int64) error {
	job, err := q.Get(ctx, userID, id)
	if err != nil {
		return err
	}
	if !job.Active() {
		return ErrNotCancelable
	}
	now := q.now()
	if _, err := q.db.ExecContext(ctx,
		`UPDATE jobs SET status = 'canceled', progress_message = 'Canceled', finished_at = ?, updated_at = ?
		 WHERE id = ? AND status IN ('queued', 'running')`, now, now, id); err != nil {
		return err
	}
	q.mu.Lock()
	cancel := q.running[id]
	q.mu.Unlock()
	if cancel != nil {
		cancel()
	} else if def := q.defs[job.Type]; def.OnFailed != nil {
		job.Status = StatusCanceled
		def.OnFailed(ctx, job)
	}
	return nil
}

// Recover re-queues jobs left running by a previous process.
func (q *Queue) Recover(ctx context.Context) (int, error) {
	res, err := q.db.ExecContext(ctx,
		`UPDATE jobs SET status = 'queued', run_after = ?, updated_at = ?, progress_message = 'Resuming after a restart'
		 WHERE status = 'running'`, q.now(), q.now())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Run starts workers and blocks until ctx is canceled and they have stopped.
func (q *Queue) Run(ctx context.Context, workers int) {
	if n, err := q.Recover(ctx); err != nil {
		q.opts.Log.Error("recover jobs", slog.String("error", err.Error()))
	} else if n > 0 {
		q.opts.Log.Info("re-queued interrupted jobs", slog.Int("count", n))
	}
	var wg sync.WaitGroup
	for range max(workers, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				ran, err := q.RunOnce(ctx)
				if err != nil && ctx.Err() == nil {
					q.opts.Log.Error("job worker", slog.String("error", err.Error()))
				}
				if ctx.Err() != nil {
					return
				}
				if ran {
					continue
				}
				select {
				case <-ctx.Done():
					return
				case <-q.wake:
				case <-time.After(q.opts.PollInterval):
				}
			}
		}()
	}
	wg.Wait()
}

// RunOnce claims and runs one due job. It reports whether a job ran.
func (q *Queue) RunOnce(ctx context.Context) (bool, error) {
	job, ok, err := q.claim(ctx)
	if err != nil || !ok {
		return false, err
	}
	q.execute(ctx, job)
	return true, nil
}

func (q *Queue) claim(ctx context.Context) (Job, bool, error) {
	now := q.now()
	row := q.db.QueryRowContext(ctx,
		`UPDATE jobs SET status = 'running', attempts = attempts + 1, started_at = ?, updated_at = ?, error = ''
		 WHERE status = 'queued' AND id = (
		   SELECT id FROM jobs WHERE status = 'queued' AND run_after <= ? ORDER BY run_after, id LIMIT 1)
		 RETURNING `+columns, now, now, now)
	job, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("claim job: %w", err)
	}
	return job, true, nil
}

func (q *Queue) execute(parent context.Context, job Job) {
	log := q.opts.Log.With(slog.Int64("job_id", job.ID), slog.String("job_type", job.Type), slog.Int64("project_id", job.ProjectID))
	def, ok := q.defs[job.Type]
	if !ok {
		q.finish(parent, job, StatusFailed, "This kind of job is no longer supported.")
		return
	}
	ctx, cancel := context.WithCancel(parent)
	q.mu.Lock()
	q.running[job.ID] = cancel
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		delete(q.running, job.ID)
		q.mu.Unlock()
		cancel()
	}()

	log.Info("job started", slog.Int("attempt", job.Attempts))
	err := q.safeRun(ctx, def, job)

	// Shutdown: leave the job for Recover to re-queue on the next start.
	if parent.Err() != nil {
		log.Info("job interrupted by shutdown")
		return
	}
	current, getErr := q.get(parent, `id = ?`, job.ID)
	if getErr == nil && current.Status == StatusCanceled {
		log.Info("job canceled")
		if def.OnFailed != nil {
			def.OnFailed(parent, current)
		}
		return
	}
	if err == nil {
		q.finish(parent, job, StatusSucceeded, "")
		log.Info("job succeeded")
		return
	}

	outcome := q.opts.Classify(err)
	if outcome.Message == "" {
		outcome.Message = "The job failed."
	}
	log.Warn("job attempt failed", slog.String("error", err.Error()), slog.Bool("retry", outcome.Retry))
	if outcome.Retry && job.Attempts < job.MaxAttempts {
		delay := q.backoff(job.Attempts)
		if outcome.RetryAfter > delay {
			delay = outcome.RetryAfter
		}
		runAfter := q.opts.Now().Add(delay).UTC().Format(time.RFC3339)
		_, dbErr := q.db.ExecContext(parent,
			`UPDATE jobs SET status = 'queued', run_after = ?, error = ?, updated_at = ?,
			        progress_message = ? WHERE id = ? AND status = 'running'`,
			runAfter, outcome.Message, q.now(), "Will retry: "+outcome.Message, job.ID)
		if dbErr != nil {
			log.Error("re-queue job", slog.String("error", dbErr.Error()))
		}
		return
	}
	q.finish(parent, job, StatusFailed, outcome.Message)
	if def.OnFailed != nil {
		job.Status, job.Error = StatusFailed, outcome.Message
		def.OnFailed(parent, job)
	}
}

func (q *Queue) safeRun(ctx context.Context, def Definition, job Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("job panicked: %v\n%s", r, debug.Stack())
		}
	}()
	return def.Run(ctx, job, &progress{q: q, id: job.ID})
}

func (q *Queue) backoff(attempt int) time.Duration {
	d := q.opts.BaseBackoff
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= q.opts.MaxBackoff {
			return q.opts.MaxBackoff
		}
	}
	return d
}

func (q *Queue) finish(ctx context.Context, job Job, status, message string) {
	now := q.now()
	progressMessage := message
	if status == StatusSucceeded {
		progressMessage = "Finished"
	}
	_, err := q.db.ExecContext(ctx,
		`UPDATE jobs SET status = ?, error = ?, finished_at = ?, updated_at = ?, progress_message = ?,
		        progress_current = CASE WHEN ? = 'succeeded' THEN progress_total ELSE progress_current END
		 WHERE id = ? AND status = 'running'`,
		status, message, now, now, progressMessage, status, job.ID)
	if err != nil {
		q.opts.Log.Error("finish job", slog.Int64("job_id", job.ID), slog.String("error", err.Error()))
	}
}

type progress struct {
	q  *Queue
	id int64
}

func (p *progress) Update(current, total int, message string) error {
	if len(message) > 300 {
		message = message[:300]
	}
	_, err := p.q.db.Exec(
		`UPDATE jobs SET progress_current = ?, progress_total = ?, progress_message = ?, updated_at = ?
		 WHERE id = ? AND status = 'running'`, current, total, message, p.q.now(), p.id)
	return err
}
