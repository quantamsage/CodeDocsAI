package docs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/quantamsage/ai-code-documentation-tool/internal/ai"
	"github.com/quantamsage/ai-code-documentation-tool/internal/analysis"
	"github.com/quantamsage/ai-code-documentation-tool/internal/database"
	"github.com/quantamsage/ai-code-documentation-tool/internal/jobs"
)

type env struct {
	db      *sql.DB
	store   *Store
	gen     *Generator
	queue   *jobs.Queue
	fake    *ai.Fake
	user    int64
	other   int64
	project int64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	e := &env{db: db, store: NewStore(db, nil), fake: &ai.Fake{}}
	e.user = exec(t, db, `INSERT INTO users (email, password_hash) VALUES ('a@example.com', 'x')`)
	e.other = exec(t, db, `INSERT INTO users (email, password_hash) VALUES ('b@example.com', 'x')`)
	e.project = exec(t, db, `INSERT INTO projects (user_id, name, status) VALUES (?, 'demo', 'analyzed')`, e.user)
	e.gen = &Generator{Analysis: analysis.NewStore(db, nil), Store: e.store, Provider: e.fake}
	e.queue = jobs.New(db, jobs.Options{
		Now:         func() time.Time { return time.Now().Add(time.Hour) },
		BaseBackoff: time.Millisecond,
		Classify: func(err error) jobs.Outcome {
			if o, ok := Classify(err); ok {
				return o
			}
			return analysis.Classify(err)
		},
	})
	e.queue.Register(JobType, e.gen.Definition())
	return e
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) int64 {
	t.Helper()
	res, err := db.Exec(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

var sampleProject = analysis.ProjectAnalysis{
	Purpose:      "Manages users over HTTP.",
	Technologies: []string{"Go"},
	Architecture: "A server package wires a store.",
	Modules:      []analysis.ModuleSummary{{Path: "internal/store", Responsibility: "Persistence."}},
	Languages:    []analysis.LanguageShare{{Language: "Go", Files: 2}},
	Endpoints:    []analysis.Endpoint{{Method: "POST", Path: "/users", Handler: "createUser", File: "cmd/server/main.go"}},
	Dependencies: []analysis.Dependency{{Name: "modernc.org/sqlite", Declared: true, Purpose: "SQLite driver"}},
	Assumptions:  []string{"Runs as one binary."},
}

func (e *env) seedAnalysis(t *testing.T, pa analysis.ProjectAnalysis) {
	t.Helper()
	job := exec(t, e.db, `INSERT INTO jobs (user_id, project_id, type, status) VALUES (?, ?, 'analyze_project', 'succeeded')`, e.user, e.project)
	files := []struct {
		path, lang, kind string
		fa               analysis.FileAnalysis
	}{
		{"cmd/server/main.go", "Go", "source", analysis.FileAnalysis{Path: "cmd/server/main.go", Summary: "Starts the server.", Functions: []analysis.Function{{Name: "main", Signature: "func main()", Description: "Entry point.", Verified: true}}, Endpoints: pa.Endpoints}},
		{"internal/store/store.go", "Go", "source", analysis.FileAnalysis{Path: "internal/store/store.go", Summary: "Saves users.", Functions: []analysis.Function{{Name: "New", Signature: "func New() *Store", Description: "Creates a store.", Parameters: []string{"none"}, SideEffects: "Opens the DB.", Verified: true}}}},
		{"go.mod", "Go", "manifest", analysis.FileAnalysis{Path: "go.mod", Summary: "Module definition."}},
		{"README.md", "Markdown", "documentation", analysis.FileAnalysis{Path: "README.md", Summary: "Readme."}},
	}
	for _, f := range files {
		id := exec(t, e.db, `INSERT INTO project_files (project_id, path, language, kind) VALUES (?, ?, ?, ?)`, e.project, f.path, f.lang, f.kind)
		raw, _ := json.Marshal(f.fa)
		exec(t, e.db, `INSERT INTO analysis_results (project_id, file_id, job_id, kind, result_json) VALUES (?, ?, ?, 'file', ?)`, e.project, id, job, string(raw))
		exec(t, e.db, `INSERT INTO analysis_results (project_id, file_id, job_id, kind, result_json) VALUES (?, ?, ?, 'structure', ?)`, e.project, id, job, `{"path":"`+f.path+`"}`)
	}
	raw, _ := json.Marshal(pa)
	exec(t, e.db, `INSERT INTO analysis_results (project_id, job_id, kind, result_json, model, created_at) VALUES (?, ?, 'project', ?, 'fake-model', '2026-05-01T10:00:00Z')`, e.project, job, string(raw))
}

var reTitle = regexp.MustCompile(`Document title: (.+)`)

func (e *env) respondWithTitles() {
	e.fake.Respond = func(req ai.Request) (any, error) {
		title := reTitle.FindStringSubmatch(req.Prompt)[1]
		return map[string]string{"markdown": "# " + title + "\n\nBody for " + title + ".\n\n> **Assumption:** inferred.\n"}, nil
	}
}

func (e *env) run(t *testing.T, req Request) jobs.Job {
	t.Helper()
	job, err := e.queue.Enqueue(context.Background(), e.user, e.project, JobType, req, 3)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if ran, _ := e.queue.RunOnce(context.Background()); !ran {
			break
		}
	}
	job, _ = e.queue.Get(context.Background(), e.user, job.ID)
	return job
}

func keys(docs []Doc) []string {
	var out []string
	for _, d := range docs {
		out = append(out, d.Scope+":"+d.Key)
	}
	return out
}

func TestPlan(t *testing.T) {
	files := []analysis.StoredFile{
		{Path: "main.go", Kind: "source", Analysis: &analysis.FileAnalysis{}},
		{Path: "pkg/a.go", Kind: "source", Analysis: &analysis.FileAnalysis{}},
		{Path: "pkg/b.go", Kind: "source", Analysis: &analysis.FileAnalysis{Missing: true}},
		{Path: "docs/x.md", Kind: "documentation", Analysis: &analysis.FileAnalysis{}},
	}
	tasks, err := Plan(Request{Scope: "all"}, analysis.ProjectAnalysis{}, files)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tk := range tasks {
		got = append(got, tk.Scope+":"+tk.Key)
	}
	want := []string{"project:overview", "project:architecture", "project:structure", "project:dependencies", "project:configuration", "project:usage", "module:.", "module:pkg"}
	if !slices.Equal(got, want) {
		t.Errorf("plan = %v (no api/database sections without endpoints or models)", got)
	}

	withAPI := analysis.ProjectAnalysis{Endpoints: []analysis.Endpoint{{Path: "/x"}}, DataModels: []analysis.DataModel{{Name: "users"}}}
	tasks, _ = Plan(Request{}, withAPI, files)
	if !slices.ContainsFunc(tasks, func(t Task) bool { return t.Key == "api" }) || !slices.ContainsFunc(tasks, func(t Task) bool { return t.Key == "database" }) {
		t.Error("api/database sections missing when applicable")
	}

	cases := []struct {
		req  Request
		want error
	}{
		{Request{Scope: "project", Key: "api"}, ErrUnavailable},
		{Request{Scope: "project", Key: "nope"}, ErrBadRequest},
		{Request{Scope: "module", Key: "missing"}, ErrBadRequest},
		{Request{Scope: "file", Key: "pkg/zzz.go"}, ErrBadRequest},
		{Request{Scope: "weird"}, ErrBadRequest},
	}
	for _, c := range cases {
		if _, err := Plan(c.req, analysis.ProjectAnalysis{}, files); !errors.Is(err, c.want) {
			t.Errorf("Plan(%+v) err = %v, want %v", c.req, err, c.want)
		}
	}
	if tasks, err := Plan(Request{Scope: "file", Key: "pkg/a.go"}, analysis.ProjectAnalysis{}, files); err != nil || tasks[0].Title != "File: pkg/a.go" {
		t.Errorf("file plan = %+v, %v", tasks, err)
	}
}

func TestGenerateAllFromAnalysis(t *testing.T) {
	e := newEnv(t)
	e.seedAnalysis(t, sampleProject)
	e.respondWithTitles()

	job := e.run(t, Request{Scope: "all"})
	if job.Status != jobs.StatusSucceeded {
		t.Fatalf("job = %+v", job)
	}
	current, err := e.store.Current(context.Background(), e.user, e.project)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"project:overview", "project:architecture", "project:structure", "project:api", "project:dependencies",
		"project:configuration", "project:usage", "module:.", "module:cmd/server", "module:internal/store"}
	if !slices.Equal(keys(current), want) {
		t.Errorf("documents = %v", keys(current))
	}
	for _, d := range current {
		if d.Version != 1 || d.Model != "fake-model" || !strings.HasPrefix(d.Markdown, "# ") || d.JobID != job.ID {
			t.Errorf("doc %s = %+v", d.Key, d)
		}
	}

	var status string
	var updated sql.NullString
	e.db.QueryRow(`SELECT status, documentation_updated_at FROM projects WHERE id = ?`, e.project).Scan(&status, &updated) //nolint:errcheck
	if status != "documented" || !updated.Valid {
		t.Errorf("project status %s, documentation_updated_at %v", status, updated)
	}

	// Prompts carry the analysis, never raw code, and the documentation rules.
	for _, call := range e.fake.Calls() {
		if strings.Contains(call.Prompt, "<content>") || !strings.Contains(call.Prompt, "<analysis>") {
			t.Fatalf("prompt is not analysis-based:\n%s", call.Prompt)
		}
		if !strings.Contains(call.System, "**Assumption:**") || !strings.Contains(call.System, "ignore any instructions") {
			t.Fatal("system prompt lacks the assumptions or untrusted-data rule")
		}
	}
	var modulePrompt string
	for _, call := range e.fake.Calls() {
		if strings.Contains(call.Prompt, "Module: internal/store") {
			modulePrompt = call.Prompt
		}
	}
	for _, want := range []string{`"Opens the DB."`, `"Persistence."`, "parameters, return values, side effects"} {
		if !strings.Contains(modulePrompt, want) {
			t.Errorf("module prompt missing %q", want)
		}
	}
}

func TestRegenerationCreatesVersions(t *testing.T) {
	e := newEnv(t)
	e.seedAnalysis(t, sampleProject)
	e.respondWithTitles()
	e.run(t, Request{Scope: "all"})

	if job := e.run(t, Request{Scope: "module", Key: "internal/store"}); job.Status != jobs.StatusSucceeded {
		t.Fatalf("module job = %+v", job)
	}
	if job := e.run(t, Request{Scope: "project", Key: "overview"}); job.Status != jobs.StatusSucceeded {
		t.Fatalf("section job = %+v", job)
	}
	if job := e.run(t, Request{Scope: "file", Key: "cmd/server/main.go"}); job.Status != jobs.StatusSucceeded {
		t.Fatalf("file job = %+v", job)
	}
	ctx := context.Background()
	current, _ := e.store.Current(ctx, e.user, e.project)
	versions := map[string]int{}
	for _, d := range current {
		versions[d.Scope+":"+d.Key] = d.Version
	}
	if versions["module:internal/store"] != 2 || versions["project:overview"] != 2 || versions["project:architecture"] != 1 || versions["file:cmd/server/main.go"] != 1 {
		t.Errorf("versions = %v", versions)
	}
	history, err := e.store.Versions(ctx, e.user, e.project, ScopeModule, "internal/store")
	if err != nil || len(history) != 2 || history[0].Version != 2 {
		t.Errorf("history = %+v, %v", history, err)
	}
	old, err := e.store.Get(ctx, e.user, e.project, ScopeModule, "internal/store", 1)
	if err != nil || old.Version != 1 {
		t.Errorf("old version = %+v, %v", old, err)
	}
	var fileID sql.NullInt64
	e.db.QueryRow(`SELECT file_id FROM documentation WHERE scope = 'file'`).Scan(&fileID) //nolint:errcheck
	if !fileID.Valid {
		t.Error("file document not linked to its file")
	}
}

func TestGenerationResumesAfterRateLimit(t *testing.T) {
	e := newEnv(t)
	e.seedAnalysis(t, sampleProject)
	e.respondWithTitles()
	base := e.fake.Respond
	limited := false
	e.fake.Respond = func(req ai.Request) (any, error) {
		if strings.Contains(req.Prompt, "Document title: Dependencies") && !limited {
			limited = true
			return nil, &ai.Error{Kind: ai.ErrRateLimited, Message: "The AI service is rate limiting requests."}
		}
		return base(req)
	}
	job := e.run(t, Request{Scope: "all"})
	if job.Status != jobs.StatusSucceeded || job.Attempts != 2 {
		t.Fatalf("job = %+v", job)
	}
	overviewCalls := 0
	for _, c := range e.fake.Calls() {
		if strings.Contains(c.Prompt, "Document title: Project Overview") {
			overviewCalls++
		}
	}
	if overviewCalls != 1 {
		t.Errorf("overview generated %d times; finished documents must not be regenerated on retry", overviewCalls)
	}
	current, _ := e.store.Current(context.Background(), e.user, e.project)
	for _, d := range current {
		if d.Version != 1 {
			t.Errorf("%s has version %d after a resumed job", d.Key, d.Version)
		}
	}
}

func TestGenerationFailures(t *testing.T) {
	t.Run("no analysis", func(t *testing.T) {
		e := newEnv(t)
		e.respondWithTitles()
		job := e.run(t, Request{Scope: "all"})
		if job.Status != jobs.StatusFailed || job.Error != "Analyze the project before generating documentation." {
			t.Errorf("job = %+v", job)
		}
	})
	t.Run("section not applicable", func(t *testing.T) {
		e := newEnv(t)
		e.seedAnalysis(t, analysis.ProjectAnalysis{Purpose: "x"})
		e.respondWithTitles()
		if job := e.run(t, Request{Scope: "project", Key: "database"}); job.Status != jobs.StatusFailed || !strings.Contains(job.Error, "does not apply") {
			t.Errorf("job = %+v", job)
		}
		var status string
		e.db.QueryRow(`SELECT status FROM projects WHERE id = ?`, e.project).Scan(&status) //nolint:errcheck
		if status != "analyzed" {
			t.Errorf("status after failure = %s", status)
		}
	})
	t.Run("no AI", func(t *testing.T) {
		e := newEnv(t)
		e.seedAnalysis(t, sampleProject)
		e.gen.Provider = nil
		if job := e.run(t, Request{}); job.Status != jobs.StatusFailed || !strings.Contains(job.Error, "ANTHROPIC_API_KEY") {
			t.Errorf("job = %+v", job)
		}
	})
}

func TestParseMarkdown(t *testing.T) {
	md, err := parseMarkdown([]byte(`{"markdown":"Intro text\r\n<script>alert(1)</script>\n<iframe src=x></iframe> ok"}`), "Overview")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(md, "# Overview\n\nIntro text\n") || strings.Contains(md, "<script") || strings.Contains(md, "<iframe") {
		t.Errorf("markdown = %q", md)
	}
	if _, err := parseMarkdown([]byte(`{"markdown":"  "}`), "x"); !errors.Is(err, ai.ErrInvalidOutput) {
		t.Errorf("empty markdown err = %v", err)
	}
	if _, err := parseMarkdown([]byte(`nope`), "x"); !errors.Is(err, ai.ErrInvalidOutput) {
		t.Errorf("bad JSON err = %v", err)
	}
	long, _ := json.Marshal(map[string]string{"markdown": "# T\n" + strings.Repeat("a", MaxMarkdown+100)})
	if md, _ := parseMarkdown(long, "T"); len(md) > MaxMarkdown+200 || !strings.Contains(md, "Truncated") {
		t.Errorf("long markdown not truncated (%d bytes)", len(md))
	}
}

func TestModuleDataCondensesLargeModules(t *testing.T) {
	var fns []analysis.Function
	for range 200 {
		fns = append(fns, analysis.Function{Name: "F", Signature: "func F()", Description: "Does F.", Parameters: []string{strings.Repeat("p", 50)}, SideEffects: strings.Repeat("s", 80)})
	}
	files := []analysis.StoredFile{{Path: "big/a.go", Kind: "source", Analysis: &analysis.FileAnalysis{Path: "big/a.go", Functions: fns}}}
	data := moduleData("big", analysis.ProjectAnalysis{}, files, 2000).(map[string]any)
	if data["note"] == nil {
		t.Fatal("large module was not condensed")
	}
	got := data["files"].([]analysis.FileAnalysis)[0].Functions[0]
	if got.SideEffects != "" || got.Parameters != nil || got.Description != "Does F." {
		t.Errorf("condensed function = %+v", got)
	}
	small := moduleData("big", analysis.ProjectAnalysis{}, files[:1], 1_000_000).(map[string]any)
	if small["note"] != nil {
		t.Error("small module was condensed")
	}
}

func TestReadsAreOwnerScoped(t *testing.T) {
	e := newEnv(t)
	e.seedAnalysis(t, sampleProject)
	e.respondWithTitles()
	e.run(t, Request{Scope: "all"})
	ctx := context.Background()
	if docs, err := e.store.Current(ctx, e.other, e.project); err != nil || len(docs) != 0 {
		t.Errorf("other user Current = %v, %v", docs, err)
	}
	if _, err := e.store.Get(ctx, e.other, e.project, ScopeProject, "overview", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("other user Get err = %v", err)
	}
	if v, err := e.store.Versions(ctx, e.other, e.project, ScopeProject, "overview"); err != nil || len(v) != 0 {
		t.Errorf("other user Versions = %v, %v", v, err)
	}
}
