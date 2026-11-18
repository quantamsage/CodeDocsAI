package files

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"time"
)

// ErrNotFound is returned for files or projects that do not exist or are not
// owned by the requesting user.
var ErrNotFound = errors.New("files: not found")

// ErrNoFiles rejects an upload in which no file could be accepted.
var ErrNoFiles = fmt.Errorf("%w: no supported files", ErrRejected)

// File is a stored project file.
type File struct {
	ID          int64
	ProjectID   int64
	Path        string
	Language    string
	Kind        string
	Size        int64
	SHA256      string
	StoragePath string
	CreatedAt   time.Time
}

// Name is the file's base name.
func (f File) Name() string { return path.Base(f.Path) }

// Dir is the file's directory ("" for the project root).
func (f File) Dir() string {
	if d := path.Dir(f.Path); d != "." {
		return d
	}
	return ""
}

// Store records uploaded files. Every method is scoped by the owning user's
// ID, joined through projects.user_id, so users can only see and change
// files of their own projects.
type Store struct {
	db    *sql.DB
	blobs *BlobStore
	now   func() time.Time
}

// NewStore returns a Store. now may be nil (time.Now).
func NewStore(db *sql.DB, blobs *BlobStore, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{db: db, blobs: blobs, now: now}
}

// Blobs returns the store's blob storage (used to start upload batches).
func (s *Store) Blobs() *BlobStore { return s.blobs }

// CommitOptions control how an upload is merged into a project.
type CommitOptions struct {
	// Replace removes all existing project files first. Otherwise files are
	// added, and existing files with the same path are overwritten.
	Replace bool
	// Dir places every uploaded file under this cleaned directory ("" = root).
	Dir string
}

// CommitResult summarizes a committed upload.
type CommitResult struct {
	Added      int
	Updated    int
	Removed    int
	FileCount  int
	TotalBytes int64
}

// Commit records the batch's files in project projectID owned by userID and
// refreshes the project's file count, size, language breakdown and status.
// On any error the batch's blobs are removed and nothing is recorded. Blobs
// of overwritten or replaced files are deleted after the transaction commits.
func (s *Store) Commit(ctx context.Context, userID, projectID int64, b *Batch, opts CommitOptions) (res CommitResult, err error) {
	defer func() {
		if err != nil {
			b.Abort()
		}
	}()
	if b.projectID != projectID {
		return res, errors.New("files: batch belongs to another project")
	}
	if len(b.staged) == 0 {
		return res, reject(ErrNoFiles, "No supported source, configuration or documentation files were found in the upload.")
	}

	paths := make([]string, len(b.staged))
	for i, f := range b.staged {
		p := f.Path
		if opts.Dir != "" {
			if p, err = CleanPath(opts.Dir + "/" + f.Path); err != nil {
				return res, &RejectedError{Message: "The target folder makes a file path invalid: " + err.Error() + ".", Err: ErrUnsafePath}
			}
		}
		paths[i] = p
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var owned int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id = ? AND user_id = ?`, projectID, userID).Scan(&owned)
	if errors.Is(err, sql.ErrNoRows) {
		return res, ErrNotFound
	}
	if err != nil {
		return res, err
	}

	var obsolete []string
	now := s.now().UTC().Format(time.RFC3339)

	if opts.Replace {
		if obsolete, err = storagePaths(ctx, tx, projectID); err != nil {
			return res, err
		}
		del, err := tx.ExecContext(ctx, `DELETE FROM project_files WHERE project_id = ?`, projectID)
		if err != nil {
			return res, fmt.Errorf("remove existing files: %w", err)
		}
		n, _ := del.RowsAffected()
		res.Removed = int(n)
	}

	for i, f := range b.staged {
		var id int64
		var old string
		err := tx.QueryRowContext(ctx,
			`SELECT id, storage_path FROM project_files WHERE project_id = ? AND path = ?`, projectID, paths[i]).Scan(&id, &old)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.ExecContext(ctx,
				`INSERT INTO project_files (project_id, path, language, kind, size_bytes, sha256, storage_path, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				projectID, paths[i], f.Language, f.Kind, f.Size, f.SHA256, f.StoragePath, now)
			if err != nil {
				return res, fmt.Errorf("insert file: %w", err)
			}
			res.Added++
		case err != nil:
			return res, err
		default:
			_, err = tx.ExecContext(ctx,
				`UPDATE project_files SET language = ?, kind = ?, size_bytes = ?, sha256 = ?, storage_path = ?, created_at = ?
				 WHERE id = ?`,
				f.Language, f.Kind, f.Size, f.SHA256, f.StoragePath, now, id)
			if err != nil {
				return res, fmt.Errorf("update file: %w", err)
			}
			obsolete = append(obsolete, old)
			res.Updated++
		}
	}

	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(size_bytes), 0) FROM project_files WHERE project_id = ?`, projectID,
	).Scan(&res.FileCount, &res.TotalBytes)
	if err != nil {
		return res, err
	}
	if res.FileCount > b.limits.MaxFiles {
		return res, reject(ErrTooManyFiles,
			"This upload would give the project %d files; the limit is %d. Remove files or replace the project's contents instead.",
			res.FileCount, b.limits.MaxFiles)
	}
	if res.TotalBytes > b.limits.MaxTotalBytes {
		return res, reject(ErrTooLarge,
			"This upload would make the project %s; the limit is %s. Replace the project's contents instead.",
			formatSize(res.TotalBytes), formatSize(b.limits.MaxTotalBytes))
	}

	langs, primary, err := languageBreakdown(ctx, tx, projectID)
	if err != nil {
		return res, err
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE projects SET file_count = ?, total_size_bytes = ?, languages_json = ?, primary_language = ?,
		        status = 'uploaded', uploaded_at = ?, updated_at = ?
		 WHERE id = ? AND user_id = ?`,
		res.FileCount, res.TotalBytes, langs, primary, now, now, projectID, userID)
	if err != nil {
		return res, fmt.Errorf("update project: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}

	for _, sp := range obsolete {
		s.blobs.Remove(sp) //nolint:errcheck // best effort; the rows no longer reference them
	}
	return res, nil
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func storagePaths(ctx context.Context, q queryer, projectID int64) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT storage_path FROM project_files WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sp string
		if err := rows.Scan(&sp); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// languageBreakdown counts source files per language and returns the JSON
// for projects.languages_json and the most common language.
func languageBreakdown(ctx context.Context, q queryer, projectID int64) (string, string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT language, COUNT(*) FROM project_files
		 WHERE project_id = ? AND kind = 'source' AND language != ''
		 GROUP BY language ORDER BY COUNT(*) DESC, language`, projectID)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	counts := map[string]int{}
	primary := ""
	for rows.Next() {
		var lang string
		var n int
		if err := rows.Scan(&lang, &n); err != nil {
			return "", "", err
		}
		if primary == "" {
			primary = lang
		}
		counts[lang] = n
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	raw, err := json.Marshal(counts)
	return string(raw), primary, err
}

const fileColumns = `f.id, f.project_id, f.path, f.language, f.kind, f.size_bytes, f.sha256, f.storage_path, f.created_at`

// List returns the files of projectID (owned by userID), sorted by path.
func (s *Store) List(ctx context.Context, userID, projectID int64) ([]File, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+fileColumns+` FROM project_files f JOIN projects p ON p.id = f.project_id
		 WHERE f.project_id = ? AND p.user_id = ? ORDER BY f.path`, projectID, userID)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Get returns the file at filePath in projectID owned by userID.
func (s *Store) Get(ctx context.Context, userID, projectID int64, filePath string) (File, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+fileColumns+` FROM project_files f JOIN projects p ON p.id = f.project_id
		 WHERE f.project_id = ? AND p.user_id = ? AND f.path = ?`, projectID, userID, filePath)
	f, err := scanFile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, ErrNotFound
	}
	return f, err
}

// Content returns up to max bytes of f's content and whether it was cut off.
// f must come from List or Get, which enforce ownership.
func (s *Store) Content(f File, max int64) ([]byte, bool, error) {
	return s.blobs.Read(f.StoragePath, max)
}

// RemoveProjectData deletes the stored content of a project. Call it after
// the project row has been deleted (which cascades to its file rows).
func (s *Store) RemoveProjectData(projectID int64) error {
	return s.blobs.RemoveProject(projectID)
}

type scanner interface{ Scan(dest ...any) error }

func scanFile(sc scanner) (File, error) {
	var f File
	var created string
	err := sc.Scan(&f.ID, &f.ProjectID, &f.Path, &f.Language, &f.Kind, &f.Size, &f.SHA256, &f.StoragePath, &created)
	if err != nil {
		return File{}, err
	}
	f.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return f, nil
}

// TreeDir is a directory in a project's file tree.
type TreeDir struct {
	Name  string // base name ("" for the root)
	Path  string // slash-separated path ("" for the root)
	Dirs  []*TreeDir
	Files []File
	// FileCount and Size include everything below this directory.
	FileCount int
	Size      int64
}

// BuildTree arranges files into a directory tree. Directories come before
// files at each level; both are sorted by name.
func BuildTree(files []File) *TreeDir {
	root := &TreeDir{}
	index := map[string]*TreeDir{"": root}
	var dirFor func(p string) *TreeDir
	dirFor = func(p string) *TreeDir {
		if d, ok := index[p]; ok {
			return d
		}
		parentPath := path.Dir(p)
		if parentPath == "." {
			parentPath = ""
		}
		parent := dirFor(parentPath)
		d := &TreeDir{Name: path.Base(p), Path: p}
		parent.Dirs = append(parent.Dirs, d)
		index[p] = d
		return d
	}
	for _, f := range files {
		d := dirFor(f.Dir())
		d.Files = append(d.Files, f)
		for p := f.Dir(); ; p = parentOf(p) {
			index[p].FileCount++
			index[p].Size += f.Size
			if p == "" {
				break
			}
		}
	}
	var sortDir func(d *TreeDir)
	sortDir = func(d *TreeDir) {
		sort.Slice(d.Dirs, func(i, j int) bool { return d.Dirs[i].Name < d.Dirs[j].Name })
		sort.Slice(d.Files, func(i, j int) bool { return d.Files[i].Name() < d.Files[j].Name() })
		for _, c := range d.Dirs {
			sortDir(c)
		}
	}
	sortDir(root)
	return root
}

func parentOf(p string) string {
	if d := path.Dir(p); d != "." {
		return d
	}
	return ""
}

// KindCounts tallies files by kind.
type KindCounts struct {
	Source, Config, Manifest, Documentation, Other int
}

// CountKinds tallies files by kind.
func CountKinds(files []File) KindCounts {
	var c KindCounts
	for _, f := range files {
		switch f.Kind {
		case KindSource:
			c.Source++
		case KindConfig:
			c.Config++
		case KindManifest:
			c.Manifest++
		case KindDocumentation:
			c.Documentation++
		default:
			c.Other++
		}
	}
	return c
}
