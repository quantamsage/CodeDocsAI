package projects

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quantamsage/ai-code-documentation-tool/internal/database"
)

type env struct {
	db    *sql.DB
	store *Store
	now   time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "projects.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	e := &env{db: db, now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	e.store = NewStore(db, func() time.Time { return e.now })
	return e
}

func (e *env) user(t *testing.T, email string) int64 {
	t.Helper()
	res, err := e.db.Exec(`INSERT INTO users (email, password_hash) VALUES (?, 'x')`, email)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestCreateInputValidation(t *testing.T) {
	cases := []struct {
		name      string
		in        CreateInput
		wantField string
	}{
		{"valid", CreateInput{Name: "My API", Description: "Line one\nLine two\twith tab"}, ""},
		{"empty name", CreateInput{Name: "   "}, "name"},
		{"long name", CreateInput{Name: strings.Repeat("n", MaxNameLen+1)}, "name"},
		{"control char in name", CreateInput{Name: "bad\x07name"}, "name"},
		{"invalid utf8 name", CreateInput{Name: "bad\xffname"}, "name"},
		{"long description", CreateInput{Name: "ok", Description: strings.Repeat("d", MaxDescriptionLen+1)}, "description"},
		{"control char in description", CreateInput{Name: "ok", Description: "nul\x00byte"}, "description"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			err := in.Validate()
			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("unexpected: %v", err)
				}
				return
			}
			var verr ValidationError
			if !errors.As(err, &verr) || verr[tc.wantField] == "" {
				t.Fatalf("err = %v, want error on %q", err, tc.wantField)
			}
		})
	}

	in := CreateInput{Name: "  spaced \t  out  name ", Description: "  text\r\nmore  "}
	if err := in.Validate(); err != nil {
		t.Fatal(err)
	}
	if in.Name != "spaced out name" || in.Description != "text\nmore" {
		t.Errorf("normalized = %q / %q", in.Name, in.Description)
	}
}

func TestCreateGetList(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.user(t, "alice@example.com")

	p, err := e.store.Create(ctx, alice, CreateInput{Name: "First", Description: "desc"})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID == 0 || p.UserID != alice || p.Name != "First" || p.Status != StatusEmpty {
		t.Errorf("created = %+v", p)
	}
	if !p.CreatedAt.Equal(e.now) || !p.UploadedAt.IsZero() || !p.DocumentationUpdatedAt.IsZero() {
		t.Errorf("timestamps = %+v", p)
	}
	if p.StatusLabel() != "No files uploaded" {
		t.Errorf("label = %q", p.StatusLabel())
	}

	e.now = e.now.Add(time.Hour)
	second, err := e.store.Create(ctx, alice, CreateInput{Name: "Second"})
	if err != nil {
		t.Fatal(err)
	}

	list, err := e.store.List(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != p.ID {
		t.Errorf("list order wrong: %+v", list)
	}

	got, err := e.store.Get(ctx, alice, p.ID)
	if err != nil || got.Name != "First" || got.Description != "desc" {
		t.Errorf("Get = %+v, %v", got, err)
	}
}

func TestNameUniquePerUser(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice, bob := e.user(t, "alice@example.com"), e.user(t, "bob@example.com")

	if _, err := e.store.Create(ctx, alice, CreateInput{Name: "Shop"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Create(ctx, alice, CreateInput{Name: "  SHOP "}); !errors.Is(err, ErrNameTaken) {
		t.Errorf("duplicate err = %v, want ErrNameTaken", err)
	}
	if _, err := e.store.Create(ctx, bob, CreateInput{Name: "Shop"}); err != nil {
		t.Errorf("same name for another user should be allowed: %v", err)
	}
}

func TestOwnershipIsolation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice, bob := e.user(t, "alice@example.com"), e.user(t, "bob@example.com")

	ap, _ := e.store.Create(ctx, alice, CreateInput{Name: "Alice's"})
	bp, _ := e.store.Create(ctx, bob, CreateInput{Name: "Bob's"})

	if _, err := e.store.Get(ctx, bob, ap.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob Get(alice's) err = %v, want ErrNotFound", err)
	}
	if err := e.store.Delete(ctx, bob, ap.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob Delete(alice's) err = %v, want ErrNotFound", err)
	}
	if _, err := e.store.Get(ctx, alice, ap.ID); err != nil {
		t.Errorf("alice's project must survive bob's delete attempt: %v", err)
	}

	list, _ := e.store.List(ctx, bob)
	if len(list) != 1 || list[0].ID != bp.ID {
		t.Errorf("bob's list = %+v", list)
	}
	if _, err := e.store.Get(ctx, alice, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing project err = %v", err)
	}
}

func TestDeleteCascades(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.user(t, "alice@example.com")
	p, _ := e.store.Create(ctx, alice, CreateInput{Name: "Doomed"})

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := e.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO project_files (project_id, path) VALUES (?, 'main.go')`, p.ID)
	mustExec(`INSERT INTO jobs (user_id, project_id, type) VALUES (?, ?, 'analyze_project')`, alice, p.ID)
	mustExec(`INSERT INTO documentation (project_id, scope, scope_key) VALUES (?, 'project', 'overview')`, p.ID)

	if err := e.store.Delete(ctx, alice, p.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"projects", "project_files", "jobs", "documentation"} {
		var n int
		e.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n) //nolint:errcheck
		if n != 0 {
			t.Errorf("%s has %d rows after delete", table, n)
		}
	}
	if err := e.store.Delete(ctx, alice, p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete err = %v", err)
	}
}

func TestDashboardFieldsFromLaterPhases(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.user(t, "alice@example.com")
	p, _ := e.store.Create(ctx, alice, CreateInput{Name: "Documented"})

	_, err := e.db.Exec(`UPDATE projects SET status = 'documented', primary_language = 'Go',
		languages_json = '{"Go": 12, "SQL": 2, "JavaScript": 12}', file_count = 26, total_size_bytes = 4096,
		uploaded_at = '2026-02-01T10:00:00Z', documentation_updated_at = '2026-02-02T11:30:00Z' WHERE id = ?`, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.store.Get(ctx, alice, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []LanguageShare{{"Go", 12}, {"JavaScript", 12}, {"SQL", 2}}
	if len(got.Languages) != len(want) {
		t.Fatalf("languages = %+v", got.Languages)
	}
	for i := range want {
		if got.Languages[i] != want[i] {
			t.Errorf("languages[%d] = %+v, want %+v", i, got.Languages[i], want[i])
		}
	}
	if got.UploadedAt.Format(time.RFC3339) != "2026-02-01T10:00:00Z" ||
		got.DocumentationUpdatedAt.Format(time.RFC3339) != "2026-02-02T11:30:00Z" {
		t.Errorf("dates = %v / %v", got.UploadedAt, got.DocumentationUpdatedAt)
	}
	if got.StatusLabel() != "Documented" || got.FileCount != 26 {
		t.Errorf("got %+v", got)
	}

	if langs := parseLanguages("not json"); len(langs) != 0 {
		t.Errorf("malformed JSON should yield no languages, got %+v", langs)
	}
}
