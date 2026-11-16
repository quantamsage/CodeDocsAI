package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "nested", "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenSetsPragmas(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	var fk int
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Errorf("foreign_keys = %d, err %v; want 1", fk, err)
	}
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q, err %v; want wal", mode, err)
	}
	var timeout int
	if err := db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout); err != nil || timeout != 5000 {
		t.Errorf("busy_timeout = %d, err %v; want 5000", timeout, err)
	}
}

func TestMigrateCreatesSchema(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	applied, err := Migrate(ctx, db)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	all, _ := Migrations()
	if len(applied) != len(all) || len(all) == 0 {
		t.Fatalf("applied %v, want all %d migrations", applied, len(all))
	}

	for _, table := range []string{"users", "projects", "project_files", "documentation", "analysis_results", "jobs", "schema_migrations"} {
		var name string
		err := db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	again, err := Migrate(ctx, db)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second Migrate applied %v, want none", again)
	}
	versions, err := AppliedVersions(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := Migrations()
	for _, m := range all {
		if !versions[m.Version] {
			t.Errorf("version %d not recorded", m.Version)
		}
	}
}

func TestMigrateAppliesOnlyNewMigrations(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	first := []Migration{{Version: 1, Name: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER);"}}
	if _, err := apply(ctx, db, first); err != nil {
		t.Fatal(err)
	}
	second := append(first, Migration{Version: 2, Name: "0002_b.sql", SQL: "CREATE TABLE b (id INTEGER);"})
	done, err := apply(ctx, db, second)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 || done[0] != 2 {
		t.Errorf("applied %v, want [2]", done)
	}
}

func TestFailedMigrationIsRolledBack(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	bad := []Migration{{Version: 1, Name: "0001_bad.sql", SQL: "CREATE TABLE ok (id INTEGER); THIS IS NOT SQL;"}}
	if _, err := apply(ctx, db, bad); err == nil || !strings.Contains(err.Error(), "0001_bad.sql") {
		t.Fatalf("expected error naming migration, got %v", err)
	}
	var n int
	db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name = 'ok'`).Scan(&n)
	if n != 0 {
		t.Error("partial migration was not rolled back")
	}
	versions, _ := AppliedVersions(ctx, db)
	if versions[1] {
		t.Error("failed migration recorded as applied")
	}
}

func TestLoadMigrationsValidation(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"bad name": {"m/abc.sql": {Data: []byte("SELECT 1;")}},
		"duplicate": {
			"m/0001_a.sql": {Data: []byte("SELECT 1;")},
			"m/1_b.sql":    {Data: []byte("SELECT 1;")},
		},
	}
	for name, fsys := range cases {
		if _, err := loadMigrations(fsys, "m"); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}

	ok := fstest.MapFS{
		"m/0002_b.sql": {Data: []byte("B")},
		"m/0001_a.sql": {Data: []byte("A")},
		"m/README.txt": {Data: []byte("ignored")},
	}
	ms, err := loadMigrations(ok, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].Version != 1 || ms[1].Version != 2 {
		t.Errorf("unexpected order: %+v", ms)
	}
}

func migratedDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openTestDB(t)
	if _, err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSchemaEnforcesForeignKeys(t *testing.T) {
	db := migratedDB(t)
	_, err := db.Exec(`INSERT INTO projects (user_id, name) VALUES (999, 'orphan')`)
	if err == nil {
		t.Fatal("expected foreign key violation for project without user")
	}
}

func TestSchemaCascadesProjectDeletion(t *testing.T) {
	db := migratedDB(t)
	mustExec := func(q string, args ...any) int64 {
		t.Helper()
		res, err := db.Exec(q, args...)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		id, _ := res.LastInsertId()
		return id
	}

	uid := mustExec(`INSERT INTO users (email, password_hash) VALUES ('a@example.com', 'x')`)
	pid := mustExec(`INSERT INTO projects (user_id, name) VALUES (?, 'p')`, uid)
	fid := mustExec(`INSERT INTO project_files (project_id, path, kind) VALUES (?, 'main.go', 'source')`, pid)
	jid := mustExec(`INSERT INTO jobs (user_id, project_id, type) VALUES (?, ?, 'analyze_project')`, uid, pid)
	mustExec(`INSERT INTO analysis_results (project_id, file_id, job_id, kind) VALUES (?, ?, ?, 'file')`, pid, fid, jid)
	mustExec(`INSERT INTO documentation (project_id, scope, scope_key) VALUES (?, 'project', 'overview')`, pid)

	mustExec(`DELETE FROM projects WHERE id = ?`, pid)

	for _, table := range []string{"project_files", "jobs", "analysis_results", "documentation"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s has %d rows after project deletion", table, n)
		}
	}
}

func TestSchemaConstraints(t *testing.T) {
	db := migratedDB(t)
	if _, err := db.Exec(`INSERT INTO users (email, password_hash) VALUES ('Dup@Example.com', 'x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users (email, password_hash) VALUES ('dup@example.com', 'x')`); err == nil {
		t.Error("emails should be unique case-insensitively")
	}
	if _, err := db.Exec(`INSERT INTO projects (user_id, name, status) VALUES (1, 'p', 'bogus')`); err == nil {
		t.Error("invalid project status should be rejected")
	}
	if _, err := db.Exec(`INSERT INTO projects (user_id, name) VALUES (1, 'p')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO documentation (project_id, scope, scope_key, version) VALUES (1, 'project', 'overview', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO documentation (project_id, scope, scope_key, version) VALUES (1, 'project', 'overview', 1)`); err == nil {
		t.Error("duplicate documentation version should be rejected")
	}
}
