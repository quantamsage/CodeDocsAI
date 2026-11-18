package files

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quantamsage/ai-code-documentation-tool/internal/database"
)

type storeEnv struct {
	db      *sql.DB
	store   *Store
	dataDir string
	alice   int64
	bob     int64
	project int64 // owned by alice
}

func newStoreEnv(t *testing.T) *storeEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := database.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	env := &storeEnv{db: db, dataDir: dir}
	env.alice = insertID(t, db, `INSERT INTO users (email, password_hash) VALUES ('alice@example.com', 'x')`)
	env.bob = insertID(t, db, `INSERT INTO users (email, password_hash) VALUES ('bob@example.com', 'x')`)
	env.project = insertID(t, db, `INSERT INTO projects (user_id, name) VALUES (?, 'demo')`, env.alice)
	clock := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	env.store = NewStore(db, NewBlobStore(dir), func() time.Time { return clock })
	return env
}

func insertID(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	res, err := db.Exec(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (e *storeEnv) batch(t *testing.T, limits Limits, files map[string]string) *Batch {
	t.Helper()
	b := NewBatch(e.store.Blobs(), e.project, limits)
	for p, body := range files {
		if err := b.Add(p, strings.NewReader(body), -1); err != nil {
			t.Fatalf("Add(%q): %v", p, err)
		}
	}
	return b
}

type projectRow struct {
	status, primary, langs string
	files                  int
	size                   int64
	uploadedAt             sql.NullString
}

func (e *storeEnv) projectRow(t *testing.T) projectRow {
	t.Helper()
	var r projectRow
	err := e.db.QueryRow(`SELECT status, primary_language, languages_json, file_count, total_size_bytes, uploaded_at
		FROM projects WHERE id = ?`, e.project).Scan(&r.status, &r.primary, &r.langs, &r.files, &r.size, &r.uploadedAt)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *storeEnv) blobCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(e.dataDir, "uploads", "1"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestCommitRecordsFilesAndProjectStats(t *testing.T) {
	e := newStoreEnv(t)
	ctx := context.Background()
	b := e.batch(t, testLimits, map[string]string{
		"main.go":            "package main\n",
		"internal/db/db.go":  "package db\n",
		"scripts/seed.py":    "print(1)\n",
		"go.mod":             "module x\n",
		"config/app.yaml":    "a: 1\n",
		"README.md":          "# Demo\n",
		"assets/picture.png": "\x89PNG",
	})
	res, err := e.store.Commit(ctx, e.alice, e.project, b, CommitOptions{})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if res.Added != 6 || res.Updated != 0 || res.FileCount != 6 {
		t.Errorf("result = %+v", res)
	}

	row := e.projectRow(t)
	if row.status != "uploaded" || row.files != 6 || row.primary != "Go" || !row.uploadedAt.Valid {
		t.Errorf("project row = %+v", row)
	}
	if row.langs != `{"Go":2,"Python":1}` {
		t.Errorf("languages_json = %s", row.langs)
	}
	if row.size != res.TotalBytes || row.size == 0 {
		t.Errorf("total size = %d, result %d", row.size, res.TotalBytes)
	}

	files, err := e.store.List(ctx, e.alice, e.project)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	if strings.Join(paths, ",") != "README.md,config/app.yaml,go.mod,internal/db/db.go,main.go,scripts/seed.py" {
		t.Errorf("paths = %v", paths)
	}

	f, err := e.store.Get(ctx, e.alice, e.project, "internal/db/db.go")
	if err != nil {
		t.Fatal(err)
	}
	if f.Language != "Go" || f.Kind != KindSource || f.Name() != "db.go" || f.Dir() != "internal/db" {
		t.Errorf("file = %+v", f)
	}
	content, truncated, err := e.store.Content(f, 1<<20)
	if err != nil || string(content) != "package db\n" || truncated {
		t.Errorf("content = %q, %v, %v", content, truncated, err)
	}
}

func TestCommitOverwritesAndReplaces(t *testing.T) {
	e := newStoreEnv(t)
	ctx := context.Background()
	if _, err := e.store.Commit(ctx, e.alice, e.project, e.batch(t, testLimits, map[string]string{
		"main.go": "package main\n", "util.go": "package main\n// util\n",
	}), CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	res, err := e.store.Commit(ctx, e.alice, e.project, e.batch(t, testLimits, map[string]string{
		"main.go": "package main\n// v2\n", "lib.py": "x = 1\n",
	}), CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 1 || res.Updated != 1 || res.FileCount != 3 {
		t.Errorf("merge result = %+v", res)
	}
	main, _ := e.store.Get(ctx, e.alice, e.project, "main.go")
	if content, _, _ := e.store.Content(main, 100); string(content) != "package main\n// v2\n" {
		t.Errorf("main.go not overwritten: %q", content)
	}
	if n := e.blobCount(t); n != 3 {
		t.Errorf("blobs on disk = %d, want 3 (old main.go blob removed)", n)
	}

	res, err = e.store.Commit(ctx, e.alice, e.project, e.batch(t, testLimits, map[string]string{
		"app.rb": "puts 1\n",
	}), CommitOptions{Replace: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 3 || res.Added != 1 || res.FileCount != 1 {
		t.Errorf("replace result = %+v", res)
	}
	if row := e.projectRow(t); row.primary != "Ruby" || row.langs != `{"Ruby":1}` {
		t.Errorf("after replace: %+v", row)
	}
	if n := e.blobCount(t); n != 1 {
		t.Errorf("blobs on disk = %d, want 1", n)
	}
}

func TestCommitIntoDirectory(t *testing.T) {
	e := newStoreEnv(t)
	ctx := context.Background()
	if _, err := e.store.Commit(ctx, e.alice, e.project, e.batch(t, testLimits, map[string]string{
		"main.go": "package main\n", "sub/x.go": "package sub\n",
	}), CommitOptions{Dir: "cmd/tool"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"cmd/tool/main.go", "cmd/tool/sub/x.go"} {
		if _, err := e.store.Get(ctx, e.alice, e.project, p); err != nil {
			t.Errorf("Get(%q): %v", p, err)
		}
	}
}

func TestCommitRejectsWithoutChanges(t *testing.T) {
	e := newStoreEnv(t)
	ctx := context.Background()
	if _, err := e.store.Commit(ctx, e.alice, e.project, e.batch(t, testLimits, map[string]string{
		"a.go": "package a\n", "b.go": "package b\n",
	}), CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	before := e.projectRow(t)

	cases := map[string]struct {
		limits Limits
		files  map[string]string
		user   int64
		want   error
	}{
		"no supported files": {testLimits, map[string]string{"x.png": "\x89PNG"}, e.alice, ErrNoFiles},
		"project file limit": {Limits{MaxFileBytes: 1 << 10, MaxFiles: 3, MaxTotalBytes: 1 << 20, MaxCompressionRatio: 100},
			map[string]string{"c.go": "package c\n", "d.go": "package d\n"}, e.alice, ErrTooManyFiles},
		"project size limit": {Limits{MaxFileBytes: 1 << 10, MaxFiles: 10, MaxTotalBytes: 25, MaxCompressionRatio: 100},
			map[string]string{"c.go": "package c\n"}, e.alice, ErrTooLarge},
		"other user's project": {testLimits, map[string]string{"c.go": "package c\n"}, e.bob, ErrNotFound},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			b := e.batch(t, c.limits, c.files)
			if _, err := e.store.Commit(ctx, c.user, e.project, b, CommitOptions{}); !errors.Is(err, c.want) {
				t.Fatalf("Commit error = %v, want %v", err, c.want)
			}
			if after := e.projectRow(t); after != before {
				t.Errorf("project changed: %+v -> %+v", before, after)
			}
			if n := e.blobCount(t); n != 2 {
				t.Errorf("blobs on disk = %d, want 2 (rejected batch cleaned up)", n)
			}
		})
	}
}

func TestReadsAreOwnerScoped(t *testing.T) {
	e := newStoreEnv(t)
	ctx := context.Background()
	if _, err := e.store.Commit(ctx, e.alice, e.project, e.batch(t, testLimits, map[string]string{"main.go": "package main\n"}), CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if files, err := e.store.List(ctx, e.bob, e.project); err != nil || len(files) != 0 {
		t.Errorf("bob List = %v, %v; want nothing", files, err)
	}
	if _, err := e.store.Get(ctx, e.bob, e.project, "main.go"); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob Get error = %v, want ErrNotFound", err)
	}
	if _, err := e.store.Get(ctx, e.alice, e.project, "missing.go"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) error = %v, want ErrNotFound", err)
	}
}

func TestRemoveProjectData(t *testing.T) {
	e := newStoreEnv(t)
	ctx := context.Background()
	if _, err := e.store.Commit(ctx, e.alice, e.project, e.batch(t, testLimits, map[string]string{"main.go": "package main\n"}), CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`DELETE FROM projects WHERE id = ?`, e.project); err != nil {
		t.Fatal(err)
	}
	if err := e.store.RemoveProjectData(e.project); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, "uploads", "1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("project upload directory still exists: %v", err)
	}
	var rows int
	e.db.QueryRow(`SELECT COUNT(*) FROM project_files`).Scan(&rows)
	if rows != 0 {
		t.Errorf("project_files rows = %d after project delete", rows)
	}
}

func TestBuildTree(t *testing.T) {
	files := []File{
		{Path: "main.go", Size: 10},
		{Path: "internal/db/db.go", Size: 20},
		{Path: "internal/db/migrate.go", Size: 5},
		{Path: "internal/api/api.go", Size: 7},
		{Path: "README.md", Size: 3},
		{Path: "web/static/app.js", Size: 1},
	}
	root := BuildTree(files)
	if root.FileCount != 6 || root.Size != 46 {
		t.Errorf("root totals = %d files, %d bytes", root.FileCount, root.Size)
	}
	if names(root) != "internal,web|README.md,main.go" {
		t.Errorf("root = %s", names(root))
	}
	internal := root.Dirs[0]
	if internal.Path != "internal" || internal.FileCount != 3 || internal.Size != 32 {
		t.Errorf("internal = %+v", internal)
	}
	if names(internal) != "api,db|" {
		t.Errorf("internal = %s", names(internal))
	}
	db := internal.Dirs[1]
	if db.Path != "internal/db" || names(db) != "|db.go,migrate.go" {
		t.Errorf("internal/db = %s (%s)", names(db), db.Path)
	}
	if web := root.Dirs[1]; web.Dirs[0].Path != "web/static" || web.Dirs[0].Files[0].Path != "web/static/app.js" {
		t.Errorf("web subtree wrong: %+v", web.Dirs[0])
	}

	if empty := BuildTree(nil); empty.FileCount != 0 || len(empty.Dirs) != 0 {
		t.Errorf("empty tree = %+v", empty)
	}
}

func names(d *TreeDir) string {
	var dirs, files []string
	for _, c := range d.Dirs {
		dirs = append(dirs, c.Name)
	}
	for _, f := range d.Files {
		files = append(files, f.Name())
	}
	return strings.Join(dirs, ",") + "|" + strings.Join(files, ",")
}

func TestCountKinds(t *testing.T) {
	got := CountKinds([]File{{Kind: KindSource}, {Kind: KindSource}, {Kind: KindConfig}, {Kind: KindManifest}, {Kind: KindDocumentation}, {Kind: KindOther}})
	if got != (KindCounts{Source: 2, Config: 1, Manifest: 1, Documentation: 1, Other: 1}) {
		t.Errorf("CountKinds = %+v", got)
	}
}
