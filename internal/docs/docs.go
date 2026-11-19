// Package docs generates technical documentation from stored analysis
// results and keeps every version.
//
// Documentation is written from the analysis (internal/analysis), never
// from raw source code: each document's prompt contains only the part of the
// analysis it needs. Documents have a scope:
//
//   - "project": the sections overview, architecture, structure, api,
//     database, dependencies, configuration and usage (api and database only
//     when the project has endpoints or data models);
//   - "module": one per directory — files, functions and methods (with
//     parameters, return values, side effects and errors), types and
//     relationships;
//   - "file": a single file in depth, generated on request.
//
// Each generation inserts a new version; the highest version is current.
package docs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Scopes.
const (
	ScopeProject = "project"
	ScopeModule  = "module"
	ScopeFile    = "file"
)

// Section describes one project-level section.
type Section struct {
	Key   string
	Title string
	// Focus tells the model what belongs in this section (and what doesn't).
	Focus string
}

// Sections in reading order.
var Sections = []Section{
	{"overview", "Project Overview", "the project's purpose, main functionality, technologies used and key features. Do not describe individual files or functions."},
	{"architecture", "Architecture", "the overall architecture, major components and their responsibilities, how components interact, and the important workflows step by step. Refer to modules and files by path; leave per-function detail to the module documents."},
	{"structure", "Directory and File Documentation", "each directory's purpose and each file's responsibility, and how files relate (imports, calls). One short entry per file; do not document individual functions."},
	{"api", "API Documentation", "every HTTP endpoint: method, path, handler, request parameters and payloads, response structure, status codes and authentication requirements, as far as the analysis shows them. Use one subsection or table row per endpoint."},
	{"database", "Database Documentation", "tables and models with their fields, relationships between them, and important database operations."},
	{"dependencies", "Dependencies", "the external libraries: what each is used for and where it is used. Distinguish declared dependencies from ones only seen in code."},
	{"configuration", "Configuration and Setup", "environment variables and configuration options, how to install dependencies and how to build and run the project, based on the manifests, configuration files and entry points in the analysis."},
	{"usage", "Usage Examples", "short, realistic examples of using the important functions, modules, commands or API endpoints. Only use names, parameters and endpoints that appear in the analysis, and label examples as illustrative."},
}

// SectionByKey returns the section with the given key.
func SectionByKey(key string) (Section, bool) {
	for _, s := range Sections {
		if s.Key == key {
			return s, true
		}
	}
	return Section{}, false
}

// Doc is one stored document version.
type Doc struct {
	ID        int64
	ProjectID int64
	Scope     string
	Key       string
	Title     string
	Markdown  string
	Version   int
	Model     string
	JobID     int64
	CreatedAt time.Time
}

// ErrNotFound is returned for missing documents (or other users' projects).
var ErrNotFound = errors.New("docs: not found")

// Store reads and writes the documentation table. Reads are owner-scoped.
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

// add inserts the next version of a document.
func (s *Store) add(ctx context.Context, d Doc) (Doc, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Doc{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	var version int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) + 1 FROM documentation WHERE project_id = ? AND scope = ? AND scope_key = ?`,
		d.ProjectID, d.Scope, d.Key).Scan(&version); err != nil {
		return Doc{}, err
	}
	d.Version = version
	d.CreatedAt = s.now().UTC().Truncate(time.Second)
	var fileID any
	if d.Scope == ScopeFile {
		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM project_files WHERE project_id = ? AND path = ?`, d.ProjectID, d.Key).Scan(&id); err == nil {
			fileID = id
		}
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO documentation (project_id, file_id, job_id, scope, scope_key, title, content_md, version, model, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ProjectID, fileID, nullable(d.JobID), d.Scope, d.Key, d.Title, d.Markdown, d.Version, d.Model, d.CreatedAt.Format(time.RFC3339))
	if err != nil {
		return Doc{}, fmt.Errorf("insert documentation: %w", err)
	}
	d.ID, _ = res.LastInsertId()
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET documentation_updated_at = ?, updated_at = ? WHERE id = ?`,
		d.CreatedAt.Format(time.RFC3339), d.CreatedAt.Format(time.RFC3339), d.ProjectID); err != nil {
		return Doc{}, err
	}
	return d, tx.Commit()
}

func nullable(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// generatedBy returns the scope/key pairs this job already generated.
func (s *Store) generatedBy(ctx context.Context, projectID, jobID int64) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT scope, scope_key FROM documentation WHERE project_id = ? AND job_id = ?`, projectID, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	done := map[string]bool{}
	for rows.Next() {
		var scope, key string
		if err := rows.Scan(&scope, &key); err != nil {
			return nil, err
		}
		done[scope+":"+key] = true
	}
	return done, rows.Err()
}

const docColumns = `d.id, d.project_id, d.scope, d.scope_key, d.title, d.content_md, d.version, d.model, COALESCE(d.job_id, 0), d.created_at`

func scanDoc(row interface{ Scan(...any) error }) (Doc, error) {
	var d Doc
	var created string
	err := row.Scan(&d.ID, &d.ProjectID, &d.Scope, &d.Key, &d.Title, &d.Markdown, &d.Version, &d.Model, &d.JobID, &created)
	if err != nil {
		return Doc{}, err
	}
	d.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return d, nil
}

// Current returns the latest version of every document, in reading order:
// project sections, then modules and files by path.
func (s *Store) Current(ctx context.Context, userID, projectID int64) ([]Doc, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+docColumns+` FROM documentation d JOIN projects p ON p.id = d.project_id
		WHERE d.project_id = ? AND p.user_id = ? AND d.version = (
		  SELECT MAX(version) FROM documentation x WHERE x.project_id = d.project_id AND x.scope = d.scope AND x.scope_key = d.scope_key)`,
		projectID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Doc
	for rows.Next() {
		d, err := scanDoc(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	SortDocs(out)
	return out, nil
}

// SortDocs orders documents for reading: project sections in their
// defined order, then modules, then files, each by path.
func SortDocs(docs []Doc) {
	scopeRank := map[string]int{ScopeProject: 0, ScopeModule: 1, ScopeFile: 2}
	sectionRank := map[string]int{}
	for i, s := range Sections {
		sectionRank[s.Key] = i
	}
	sort.SliceStable(docs, func(i, j int) bool {
		a, b := docs[i], docs[j]
		if a.Scope != b.Scope {
			return scopeRank[a.Scope] < scopeRank[b.Scope]
		}
		if a.Scope == ScopeProject {
			return sectionRank[a.Key] < sectionRank[b.Key]
		}
		return a.Key < b.Key
	})
}

// Get returns a document version (version 0 = current).
func (s *Store) Get(ctx context.Context, userID, projectID int64, scope, key string, version int) (Doc, error) {
	query := `SELECT ` + docColumns + ` FROM documentation d JOIN projects p ON p.id = d.project_id
		WHERE d.project_id = ? AND p.user_id = ? AND d.scope = ? AND d.scope_key = ?`
	args := []any{projectID, userID, scope, key}
	if version > 0 {
		query += ` AND d.version = ?`
		args = append(args, version)
	}
	d, err := scanDoc(s.db.QueryRowContext(ctx, query+` ORDER BY d.version DESC LIMIT 1`, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Doc{}, ErrNotFound
	}
	return d, err
}

// Versions lists every version of a document, newest first (without content).
func (s *Store) Versions(ctx context.Context, userID, projectID int64, scope, key string) ([]Doc, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.id, d.project_id, d.scope, d.scope_key, d.title, '', d.version, d.model, COALESCE(d.job_id, 0), d.created_at
		FROM documentation d JOIN projects p ON p.id = d.project_id
		WHERE d.project_id = ? AND p.user_id = ? AND d.scope = ? AND d.scope_key = ? ORDER BY d.version DESC`,
		projectID, userID, scope, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Doc
	for rows.Next() {
		d, err := scanDoc(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
