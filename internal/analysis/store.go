package analysis

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/quantamsage/ai-code-documentation-tool/internal/structure"
)

// ErrNotFound is returned when a project has no analysis (or is not owned
// by the requesting user).
var ErrNotFound = errors.New("analysis: not found")

// Store reads and writes analysis_results. Read methods are owner-scoped.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// NewStore returns a Store. now may be nil.
func NewStore(db *sql.DB, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{db: db, now: now}
}

func (s *Store) ts() string { return s.now().UTC().Format(time.RFC3339) }

// clearOtherRuns removes results from earlier analysis runs, keeping this
// job's (partial) results so a retried job can resume.
func (s *Store) clearOtherRuns(ctx context.Context, projectID, jobID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM analysis_results WHERE project_id = ? AND (job_id IS NULL OR job_id != ?)`, projectID, jobID)
	return err
}

func (s *Store) save(ctx context.Context, projectID, jobID int64, fileID *int64, kind string, value any, model string, in, out int) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO analysis_results (project_id, file_id, job_id, kind, result_json, model, input_tokens, output_tokens, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		projectID, fileID, jobID, kind, string(data), model, in, out, s.ts())
	if err != nil {
		return fmt.Errorf("save %s result: %w", kind, err)
	}
	return nil
}

// jobResults loads this job's results of a kind, keyed by file id.
func (s *Store) jobResults(ctx context.Context, projectID, jobID int64, kind string, decode func(fileID int64, raw []byte) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT COALESCE(file_id, 0), result_json FROM analysis_results WHERE project_id = ? AND job_id = ? AND kind = ? ORDER BY id`,
		projectID, jobID, kind)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		if err := decode(id, []byte(raw)); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) setProjectStatus(ctx context.Context, projectID int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE projects SET status = ?, updated_at = ? WHERE id = ?`, status, s.ts(), projectID)
	return err
}

// Meta describes a stored project analysis.
type Meta struct {
	CreatedAt    time.Time
	Model        string
	JobID        int64
	InputTokens  int
	OutputTokens int
}

// Project returns the latest project analysis for a project owned by userID.
func (s *Store) Project(ctx context.Context, userID, projectID int64) (ProjectAnalysis, Meta, error) {
	var raw, created, model string
	var jobID sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT r.result_json, r.created_at, r.model, r.job_id FROM analysis_results r JOIN projects p ON p.id = r.project_id
		 WHERE r.project_id = ? AND p.user_id = ? AND r.kind = 'project' ORDER BY r.id DESC LIMIT 1`,
		projectID, userID).Scan(&raw, &created, &model, &jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectAnalysis{}, Meta{}, ErrNotFound
	}
	if err != nil {
		return ProjectAnalysis{}, Meta{}, err
	}
	var pa ProjectAnalysis
	if err := json.Unmarshal([]byte(raw), &pa); err != nil {
		return ProjectAnalysis{}, Meta{}, fmt.Errorf("decode project analysis: %w", err)
	}
	meta := Meta{Model: model, JobID: jobID.Int64}
	meta.CreatedAt, _ = time.Parse(time.RFC3339, created)
	err = s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0) FROM analysis_results WHERE project_id = ? AND job_id = ?`,
		projectID, meta.JobID).Scan(&meta.InputTokens, &meta.OutputTokens)
	return pa, meta, err
}

// Files returns every analyzed file of the latest run, sorted by path.
func (s *Store) Files(ctx context.Context, userID, projectID int64) ([]StoredFile, error) {
	return s.files(ctx, userID, projectID, 0)
}

// File returns one analyzed file.
func (s *Store) File(ctx context.Context, userID, projectID, fileID int64) (StoredFile, error) {
	list, err := s.files(ctx, userID, projectID, fileID)
	if err != nil {
		return StoredFile{}, err
	}
	if len(list) == 0 {
		return StoredFile{}, ErrNotFound
	}
	return list[0], nil
}

func (s *Store) files(ctx context.Context, userID, projectID, onlyFile int64) ([]StoredFile, error) {
	query := `SELECT f.id, f.path, f.language, f.kind, r.kind, r.result_json
		FROM analysis_results r
		JOIN project_files f ON f.id = r.file_id
		JOIN projects p ON p.id = r.project_id
		WHERE r.project_id = ? AND p.user_id = ? AND r.kind IN ('structure', 'file')`
	args := []any{projectID, userID}
	if onlyFile > 0 {
		query += ` AND f.id = ?`
		args = append(args, onlyFile)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY f.path, r.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[int64]*StoredFile{}
	var order []int64
	for rows.Next() {
		var sf StoredFile
		var kind, raw string
		if err := rows.Scan(&sf.FileID, &sf.Path, &sf.Language, &sf.Kind, &kind, &raw); err != nil {
			return nil, err
		}
		entry, ok := byID[sf.FileID]
		if !ok {
			entry = &sf
			byID[sf.FileID] = entry
			order = append(order, sf.FileID)
		}
		switch kind {
		case "structure":
			var fs structure.FileStructure
			if json.Unmarshal([]byte(raw), &fs) == nil {
				entry.Structure = fs
			}
		case "file":
			var fa FileAnalysis
			if json.Unmarshal([]byte(raw), &fa) == nil {
				entry.Analysis = &fa
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]StoredFile, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}
