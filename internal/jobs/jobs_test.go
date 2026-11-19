package jobs

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quantamsage/ai-code-documentation-tool/internal/database"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }

type env struct {
	db      *sql.DB
	q       *Queue
	clock   *clock
	user    int64
	other   int64
	project int64
	failed  []Job
	mu      sync.Mutex
}

var errRetry = errors.New("temporary")

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	e := &env{db: db, clock: &clock{t: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)}}
	e.user = insert(t, db, `INSERT INTO users (email, password_hash) VALUES ('a@example.com', 'x')`)
	e.other = insert(t, db, `INSERT INTO users (email, password_hash) VALUES ('b@example.com', 'x')`)
	e.project = insert(t, db, `INSERT INTO projects (user_id, name) VALUES (?, 'p')`, e.user)
	e.q = New(db, Options{
		Now:          e.clock.Now,
		PollInterval: 10 * time.Millisecond,
		BaseBackoff:  time.Minute,
		MaxBackoff:   10 * time.Minute,
		Classify: func(err error) Outcome {
			if errors.Is(err, errRetry) {
				return Outcome{Retry: true, Message: "Temporary problem."}
			}
			return Outcome{Message: "It broke."}
		},
	})
	return e
}

func insert(t *testing.T, db *sql.DB, q string, args ...any) int64 {
	t.Helper()
	res, err := db.Exec(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (e *env) register(name string, run func(ctx context.Context, job Job, p Progress) error) {
	e.q.Register(name, Definition{Run: run, OnFailed: func(_ context.Context, job Job) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.failed = append(e.failed, job)
	}})
}

func (e *env) job(t *testing.T, id int64) Job {
	t.Helper()
	j, err := e.q.Get(context.Background(), e.user, id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestSuccessRecordsProgress(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.register("analyze", func(ctx context.Context, job Job, p Progress) error {
		if string(job.Payload) != `{"scope":"all"}` {
			t.Errorf("payload = %s", job.Payload)
		}
		p.Update(1, 4, "Analyzing batch 1 of 4") //nolint:errcheck
		return p.Update(3, 4, "Analyzing batch 3 of 4")
	})
	job, err := e.q.Enqueue(ctx, e.user, e.project, "analyze", map[string]string{"scope": "all"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != StatusQueued || job.MaxAttempts != 3 || job.ProgressMessage != "Waiting to start" || !job.Active() {
		t.Errorf("enqueued = %+v", job)
	}

	ran, err := e.q.RunOnce(ctx)
	if !ran || err != nil {
		t.Fatalf("RunOnce = %v, %v", ran, err)
	}
	got := e.job(t, job.ID)
	if got.Status != StatusSucceeded || got.Attempts != 1 || got.ProgressCurrent != 4 || got.ProgressTotal != 4 || got.Percent() != 100 {
		t.Errorf("finished = %+v", got)
	}
	if got.StartedAt.IsZero() || got.FinishedAt.IsZero() || got.ProgressMessage != "Finished" {
		t.Errorf("timestamps/message = %+v", got)
	}
	if ran, _ := e.q.RunOnce(ctx); ran {
		t.Error("nothing else should be queued")
	}
}

func TestOneActiveJobPerTypeAndProject(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.register("analyze", func(context.Context, Job, Progress) error { return nil })
	e.register("docs", func(context.Context, Job, Progress) error { return nil })
	if _, err := e.q.Enqueue(ctx, e.user, e.project, "analyze", nil, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.q.Enqueue(ctx, e.user, e.project, "analyze", nil, 1); !errors.Is(err, ErrActive) {
		t.Errorf("duplicate enqueue err = %v", err)
	}
	if _, err := e.q.Enqueue(ctx, e.user, e.project, "docs", nil, 1); err != nil {
		t.Errorf("other type: %v", err)
	}
	if active, _ := e.q.HasActive(ctx, e.project); !active {
		t.Error("HasActive = false")
	}
	if _, err := e.q.Enqueue(ctx, e.user, e.project, "unknown", nil, 1); err == nil {
		t.Error("unknown type accepted")
	}
}

func TestPermanentFailure(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.register("analyze", func(context.Context, Job, Progress) error { return errors.New("internal detail: db password") })
	job, _ := e.q.Enqueue(ctx, e.user, e.project, "analyze", nil, 3)
	e.q.RunOnce(ctx) //nolint:errcheck
	got := e.job(t, job.ID)
	if got.Status != StatusFailed || got.Error != "It broke." || got.Attempts != 1 {
		t.Errorf("failed job = %+v", got)
	}
	if len(e.failed) != 1 || e.failed[0].Error != "It broke." {
		t.Errorf("OnFailed calls = %+v", e.failed)
	}
}

func TestRetriesWithBackoffUntilAttemptsRunOut(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var calls atomic.Int32
	e.register("analyze", func(context.Context, Job, Progress) error { calls.Add(1); return errRetry })
	job, _ := e.q.Enqueue(ctx, e.user, e.project, "analyze", nil, 3)

	e.q.RunOnce(ctx) //nolint:errcheck
	got := e.job(t, job.ID)
	if got.Status != StatusQueued || got.Error != "Temporary problem." || !got.RunAfter.Equal(e.clock.Now().Add(time.Minute)) {
		t.Fatalf("after attempt 1 = %+v", got)
	}
	if got.ProgressMessage != "Will retry: Temporary problem." {
		t.Errorf("progress message = %q", got.ProgressMessage)
	}
	if ran, _ := e.q.RunOnce(ctx); ran {
		t.Fatal("job ran before its retry time")
	}

	e.clock.Add(time.Minute)
	e.q.RunOnce(ctx) //nolint:errcheck
	got = e.job(t, job.ID)
	if got.Status != StatusQueued || !got.RunAfter.Equal(e.clock.Now().Add(2*time.Minute)) {
		t.Fatalf("after attempt 2 = %+v (backoff should double)", got)
	}

	e.clock.Add(2 * time.Minute)
	e.q.RunOnce(ctx) //nolint:errcheck
	got = e.job(t, job.ID)
	if got.Status != StatusFailed || got.Attempts != 3 || calls.Load() != 3 {
		t.Errorf("after attempt 3 = %+v, calls %d", got, calls.Load())
	}
	if len(e.failed) != 1 {
		t.Errorf("OnFailed calls = %d", len(e.failed))
	}
}

func TestProviderRetryAfterWins(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.q.opts.Classify = func(error) Outcome {
		return Outcome{Retry: true, RetryAfter: 7 * time.Minute, Message: "Rate limited."}
	}
	e.register("analyze", func(context.Context, Job, Progress) error { return errRetry })
	job, _ := e.q.Enqueue(ctx, e.user, e.project, "analyze", nil, 3)
	e.q.RunOnce(ctx) //nolint:errcheck
	if got := e.job(t, job.ID); !got.RunAfter.Equal(e.clock.Now().Add(7 * time.Minute)) {
		t.Errorf("run_after = %v", got.RunAfter)
	}
}

func TestPanicsFailTheJob(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.register("analyze", func(context.Context, Job, Progress) error { panic("boom") })
	job, _ := e.q.Enqueue(ctx, e.user, e.project, "analyze", nil, 1)
	e.q.RunOnce(ctx) //nolint:errcheck
	if got := e.job(t, job.ID); got.Status != StatusFailed || got.Error != "It broke." {
		t.Errorf("panicking job = %+v", got)
	}
}

func TestCancelQueuedAndRunningJobs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	started := make(chan struct{})
	e.register("analyze", func(ctx context.Context, _ Job, _ Progress) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	e.register("docs", func(context.Context, Job, Progress) error { t.Error("canceled job ran"); return nil })

	queued, _ := e.q.Enqueue(ctx, e.user, e.project, "docs", nil, 1)
	if err := e.q.Cancel(ctx, e.other, queued.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("other user cancel err = %v", err)
	}
	if err := e.q.Cancel(ctx, e.user, queued.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.job(t, queued.ID); got.Status != StatusCanceled {
		t.Errorf("queued job after cancel = %+v", got)
	}

	running, _ := e.q.Enqueue(ctx, e.user, e.project, "analyze", nil, 3)
	done := make(chan struct{})
	go func() { e.q.RunOnce(ctx); close(done) }() //nolint:errcheck
	<-started
	if err := e.q.Cancel(ctx, e.user, running.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("running job did not stop")
	}
	if got := e.job(t, running.ID); got.Status != StatusCanceled {
		t.Errorf("running job after cancel = %+v", got)
	}
	if len(e.failed) != 2 {
		t.Errorf("OnFailed calls = %d, want 2", len(e.failed))
	}
	if err := e.q.Cancel(ctx, e.user, running.ID); !errors.Is(err, ErrNotCancelable) {
		t.Errorf("second cancel err = %v", err)
	}
}

func TestRecoverRequeuesInterruptedJobs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.register("analyze", func(context.Context, Job, Progress) error { return nil })
	job, _ := e.q.Enqueue(ctx, e.user, e.project, "analyze", nil, 3)
	e.db.Exec(`UPDATE jobs SET status = 'running', attempts = 1 WHERE id = ?`, job.ID) //nolint:errcheck

	n, err := e.q.Recover(ctx)
	if err != nil || n != 1 {
		t.Fatalf("Recover = %d, %v", n, err)
	}
	if got := e.job(t, job.ID); got.Status != StatusQueued || got.ProgressMessage != "Resuming after a restart" {
		t.Errorf("recovered = %+v", got)
	}
	e.q.RunOnce(ctx) //nolint:errcheck
	if got := e.job(t, job.ID); got.Status != StatusSucceeded || got.Attempts != 2 {
		t.Errorf("after rerun = %+v", got)
	}
}

func TestOwnerScopingAndLatest(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.register("analyze", func(context.Context, Job, Progress) error { return nil })
	e.register("docs", func(context.Context, Job, Progress) error { return nil })
	a, _ := e.q.Enqueue(ctx, e.user, e.project, "analyze", nil, 1)
	d, _ := e.q.Enqueue(ctx, e.user, e.project, "docs", nil, 1)

	if _, err := e.q.Get(ctx, e.other, a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("other user Get err = %v", err)
	}
	if latest, err := e.q.Latest(ctx, e.user, e.project); err != nil || latest.ID != d.ID {
		t.Errorf("Latest = %+v, %v", latest, err)
	}
	if latest, err := e.q.Latest(ctx, e.user, e.project, "analyze"); err != nil || latest.ID != a.ID {
		t.Errorf("Latest(analyze) = %+v, %v", latest, err)
	}
	if _, err := e.q.Latest(ctx, e.other, e.project); !errors.Is(err, ErrNotFound) {
		t.Errorf("other user Latest err = %v", err)
	}
}

func TestRunLoopProcessesJobs(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.register("analyze", func(context.Context, Job, Progress) error { return nil })
	stopped := make(chan struct{})
	go func() { e.q.Run(ctx, 2); close(stopped) }()

	job, _ := e.q.Enqueue(context.Background(), e.user, e.project, "analyze", nil, 1)
	deadline := time.Now().Add(5 * time.Second)
	for e.job(t, job.ID).Status != StatusSucceeded {
		if time.Now().After(deadline) {
			t.Fatal("job was not processed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("workers did not stop")
	}
}

func TestPercent(t *testing.T) {
	cases := []struct {
		job  Job
		want int
	}{
		{Job{ProgressCurrent: 1, ProgressTotal: 4}, 25},
		{Job{ProgressCurrent: 9, ProgressTotal: 4}, 100},
		{Job{ProgressTotal: 0}, 0},
		{Job{Status: StatusSucceeded}, 100},
	}
	for _, c := range cases {
		if got := c.job.Percent(); got != c.want {
			t.Errorf("Percent(%+v) = %d, want %d", c.job, got, c.want)
		}
	}
}

func TestCancelActive(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.register("analyze", func(context.Context, Job, Progress) error { return nil })
	e.register("docs", func(context.Context, Job, Progress) error { return nil })
	a, _ := e.q.Enqueue(ctx, e.user, e.project, "analyze", nil, 1)
	d, _ := e.q.Enqueue(ctx, e.user, e.project, "docs", nil, 1)
	if err := e.q.CancelActive(ctx, e.other, e.project); err != nil {
		t.Fatal(err)
	}
	if e.job(t, a.ID).Status != StatusQueued {
		t.Error("another user canceled the jobs")
	}
	if err := e.q.CancelActive(ctx, e.user, e.project); err != nil {
		t.Fatal(err)
	}
	if e.job(t, a.ID).Status != StatusCanceled || e.job(t, d.ID).Status != StatusCanceled {
		t.Error("jobs not canceled")
	}
	if active, _ := e.q.HasActive(ctx, e.project); active {
		t.Error("project still has active jobs")
	}
}
