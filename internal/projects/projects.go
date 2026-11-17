// Package projects manages user-owned projects. Every query is scoped by the
// owning user's ID, so one user can never read or change another's projects:
// a project owned by someone else is indistinguishable from one that does not
// exist.
package projects

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Errors returned by Store.
var (
	ErrNotFound  = errors.New("projects: not found")
	ErrNameTaken = errors.New("projects: name already used")
)

// ValidationError maps form field names to user-facing messages.
type ValidationError map[string]string

func (v ValidationError) Error() string {
	parts := make([]string, 0, len(v))
	for f, m := range v {
		parts = append(parts, f+": "+m)
	}
	sort.Strings(parts)
	return "projects: invalid input: " + strings.Join(parts, "; ")
}

// Input limits.
const (
	MaxNameLen        = 100
	MaxDescriptionLen = 2000
)

// Status values (mirrors the CHECK constraint on projects.status).
const (
	StatusEmpty       = "empty"
	StatusUploaded    = "uploaded"
	StatusAnalyzing   = "analyzing"
	StatusAnalyzed    = "analyzed"
	StatusDocumenting = "documenting"
	StatusDocumented  = "documented"
	StatusFailed      = "failed"
)

// LanguageShare is one entry of a project's language breakdown.
type LanguageShare struct {
	Language string
	Files    int
}

// Project is a user-owned codebase.
type Project struct {
	ID                     int64
	UserID                 int64
	Name                   string
	Description            string
	PrimaryLanguage        string
	Languages              []LanguageShare // sorted by file count, descending
	Status                 string
	FileCount              int
	TotalSizeBytes         int64
	UploadedAt             time.Time // zero if nothing uploaded yet
	DocumentationUpdatedAt time.Time // zero if never documented
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// StatusLabel is a human-readable documentation status for the dashboard.
func (p Project) StatusLabel() string {
	switch p.Status {
	case StatusEmpty:
		return "No files uploaded"
	case StatusUploaded:
		return "Not analyzed"
	case StatusAnalyzing:
		return "Analyzing"
	case StatusAnalyzed:
		return "Analyzed, not documented"
	case StatusDocumenting:
		return "Generating documentation"
	case StatusDocumented:
		return "Documented"
	case StatusFailed:
		return "Failed"
	}
	return p.Status
}

// CreateInput is the data needed to create a project.
type CreateInput struct {
	Name        string
	Description string
}

// Validate normalizes and checks the input.
func (in *CreateInput) Validate() error {
	in.Name = strings.Join(strings.Fields(in.Name), " ")
	in.Description = strings.TrimSpace(strings.ReplaceAll(in.Description, "\r\n", "\n"))
	errs := ValidationError{}

	switch n := utf8.RuneCountInString(in.Name); {
	case !utf8.ValidString(in.Name):
		errs["name"] = "Name contains invalid characters."
	case n == 0:
		errs["name"] = "Enter a project name."
	case n > MaxNameLen:
		errs["name"] = fmt.Sprintf("Name must be at most %d characters.", MaxNameLen)
	case strings.ContainsFunc(in.Name, unicode.IsControl):
		errs["name"] = "Name contains characters that are not allowed."
	}

	switch {
	case !utf8.ValidString(in.Description):
		errs["description"] = "Description contains invalid characters."
	case utf8.RuneCountInString(in.Description) > MaxDescriptionLen:
		errs["description"] = fmt.Sprintf("Description must be at most %d characters.", MaxDescriptionLen)
	case strings.ContainsFunc(in.Description, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }):
		errs["description"] = "Description contains characters that are not allowed."
	}

	if len(errs) > 0 {
		return errs
	}
	return nil
}

// Store reads and writes projects.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// NewStore returns a Store. now may be nil (time.Now).
func NewStore(db *sql.DB, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{db: db, now: now}
}

const projectColumns = `id, user_id, name, description, primary_language, languages_json, status,
	file_count, total_size_bytes, uploaded_at, documentation_updated_at, created_at, updated_at`

// Create validates in and creates a project owned by userID.
func (s *Store) Create(ctx context.Context, userID int64, in CreateInput) (Project, error) {
	if err := in.Validate(); err != nil {
		return Project{}, err
	}
	now := s.now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO projects (user_id, name, description, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		userID, in.Name, in.Description, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Project{}, ErrNameTaken
		}
		return Project{}, fmt.Errorf("insert project: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Project{}, err
	}
	return s.Get(ctx, userID, id)
}

// Get returns project id if it is owned by userID, else ErrNotFound.
func (s *Store) Get(ctx context.Context, userID, id int64) (Project, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE id = ? AND user_id = ?`, id, userID)
	p, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	return p, err
}

// List returns userID's projects, most recently updated first.
func (s *Store) List(ctx context.Context, userID int64) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE user_id = ? ORDER BY updated_at DESC, id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Delete removes project id if it is owned by userID (cascading to its files,
// jobs, analyses and documentation), else returns ErrNotFound.
func (s *Store) Delete(ctx context.Context, userID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

type scanner interface{ Scan(dest ...any) error }

func scanProject(sc scanner) (Project, error) {
	var p Project
	var langs, created, updated string
	var uploaded, documented sql.NullString
	err := sc.Scan(&p.ID, &p.UserID, &p.Name, &p.Description, &p.PrimaryLanguage, &langs, &p.Status,
		&p.FileCount, &p.TotalSizeBytes, &uploaded, &documented, &created, &updated)
	if err != nil {
		return Project{}, err
	}
	p.Languages = parseLanguages(langs)
	p.CreatedAt, p.UpdatedAt = parseTime(created), parseTime(updated)
	if uploaded.Valid {
		p.UploadedAt = parseTime(uploaded.String)
	}
	if documented.Valid {
		p.DocumentationUpdatedAt = parseTime(documented.String)
	}
	return p, nil
}

// parseLanguages decodes languages_json ({"Go": 12, "SQL": 2}) into a
// breakdown sorted by file count. Malformed JSON yields an empty breakdown.
func parseLanguages(raw string) []LanguageShare {
	var m map[string]int
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	out := make([]LanguageShare, 0, len(m))
	for lang, n := range m {
		if lang != "" {
			out = append(out, LanguageShare{Language: lang, Files: n})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Files != out[j].Files {
			return out[i].Files > out[j].Files
		}
		return out[i].Language < out[j].Language
	})
	return out
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}
