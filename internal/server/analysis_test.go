package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/quantamsage/ai-code-documentation-tool/internal/ai"
	"github.com/quantamsage/ai-code-documentation-tool/internal/jobs"
)

var reFilePath = regexp.MustCompile(`<file path="([^"]+)"`)

// fakeAnalysisAI answers every analysis request deterministically. Its
// output includes markup to check that AI text is always escaped.
func fakeAnalysisAI() *ai.Fake {
	empty := []any{}
	return &ai.Fake{Respond: func(req ai.Request) (any, error) {
		if strings.Contains(req.System, "analyzing source code") {
			var out []map[string]any
			for _, m := range reFilePath.FindAllStringSubmatch(req.Prompt, -1) {
				entry := map[string]any{
					"path": m[1], "summary": "Summary of " + m[1] + " <script>alert(1)</script>", "responsibilities": []string{"Serves HTTP"},
					"functions": empty, "types": empty, "dependencies": empty, "api_endpoints": empty, "data_models": empty,
					"configuration": empty, "error_handling": "", "logging": "", "relationships": empty, "assumptions": []string{"Assumed detail."},
				}
				if strings.HasSuffix(m[1], "main.go") {
					entry["functions"] = []map[string]any{{"name": "main", "signature": "func main()", "description": "Starts the server.", "parameters": empty, "returns": "", "side_effects": "Listens on :8080.", "errors": ""}}
					entry["api_endpoints"] = []map[string]any{{"method": "GET", "path": "/health", "handler": "health", "description": "Health check.", "auth": "none"}}
				}
				out = append(out, entry)
			}
			return map[string]any{"files": out}, nil
		}
		return map[string]any{
			"purpose": "Serves a health endpoint <b>bold</b>.", "main_functionality": []string{"Health checks"}, "technologies": []string{"Go"},
			"architecture": "One binary.", "components": empty, "workflows": empty, "entry_points": []string{"main.go"},
			"interactions": empty, "error_handling": "Returns errors.", "logging": "None.", "dependency_purposes": empty, "assumptions": empty,
		}, nil
	}}
}

func setupAnalyzed(t *testing.T, provider ai.Provider) (*testEnv, *client, string) {
	t.Helper()
	env := newTestEnvWith(t, nil, WithAIProvider(provider))
	c := env.client(t)
	c.register("alice@example.com", "correct horse")
	project := c.createProject("Service")
	wantRedirect(t, c.upload(project+"/upload",
		file("files", "main.go", "package main\n\nfunc main() {\n\thttp.HandleFunc(\"GET /health\", health)\n}\n"),
		file("files", "README.md", "# Service\n"),
	), project)
	return env, c, project
}

func (e *testEnv) runJobs(t *testing.T) {
	t.Helper()
	for range 10 {
		ran, err := e.srv.jobs.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ran {
			return
		}
	}
}

func latestJob(t *testing.T, c *client, project string) map[string]any {
	t.Helper()
	rec := c.get(project + "/jobs/latest")
	wantStatus(t, rec, http.StatusOK)
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", rec.Header())
	}
	var body struct {
		Job map[string]any `json:"job"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Job
}

func TestAnalyzeProjectEndToEnd(t *testing.T) {
	env, c, project := setupAnalyzed(t, fakeAnalysisAI())

	if job := latestJob(t, c, project); job != nil {
		t.Errorf("job before analysis = %v", job)
	}
	wantBody(t, c.get(project), "Analyze project", `action="`+project+`/analyze"`)

	wantRedirect(t, c.postCSRF(project+"/analyze", nil), project+"#analysis")
	page := c.get(project)
	wantBody(t, page, "Analysis started.", "data-job-status", `data-active="true"`, "Waiting to start", "<progress", "Cancel")
	if strings.Contains(page.Body.String(), "Analyze project</button>") {
		t.Error("analyze button shown while a job is active")
	}
	if job := latestJob(t, c, project); job["status"] != "queued" || job["active"] != true {
		t.Errorf("queued job = %v", job)
	}

	env.runJobs(t)
	job := latestJob(t, c, project)
	if job["status"] != "succeeded" || job["percent"] != float64(100) || job["active"] != false {
		t.Errorf("finished job = %v", job)
	}
	page = c.get(project)
	wantBody(t, page, "View analysis", "Serves a health endpoint &lt;b&gt;bold&lt;/b&gt;.", "Re-analyze project")
	wantBody(t, c.get("/dashboard"), "Analyzed, not documented")

	analysisPage := c.get(project + "/analysis")
	wantStatus(t, analysisPage, http.StatusOK)
	wantBody(t, analysisPage, "Analysis of “Service”", "by fake-model", "2 of 2 files analyzed",
		"<code>/health</code>", "Health check.", "One binary.", `href="`+project+`/files/main.go"`,
		"Summary of main.go &lt;script&gt;alert(1)&lt;/script&gt;")
	if strings.Contains(analysisPage.Body.String(), "<script>alert") || strings.Contains(analysisPage.Body.String(), "<b>bold") {
		t.Fatal("AI output rendered unescaped")
	}

	filePage := c.get(project + "/files/main.go")
	wantBody(t, filePage, "AI analysis", "<code>func main()</code>", "Starts the server.", "Listens on :8080.", "Assumed detail.")
}

func TestAnalyzeRequiresAIFilesAndNoActiveJob(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		env := newTestEnv(t)
		c := env.client(t)
		c.register("alice@example.com", "correct horse")
		project := c.createProject("Service")
		wantRedirect(t, c.upload(project+"/upload", file("files", "main.go", "package main\n")), project)
		wantBody(t, c.get(project), "AI analysis is not configured on this server")
		rec := c.postCSRF(project+"/analyze", nil)
		wantStatus(t, rec, http.StatusServiceUnavailable)
		wantBody(t, rec, "ANTHROPIC_API_KEY")
	})
	t.Run("no files", func(t *testing.T) {
		env := newTestEnvWith(t, nil, WithAIProvider(fakeAnalysisAI()))
		c := env.client(t)
		c.register("alice@example.com", "correct horse")
		project := c.createProject("Empty")
		wantStatus(t, c.postCSRF(project+"/analyze", nil), http.StatusConflict)
	})
	t.Run("already running", func(t *testing.T) {
		_, c, project := setupAnalyzed(t, fakeAnalysisAI())
		wantRedirect(t, c.postCSRF(project+"/analyze", nil), project+"#analysis")
		wantStatus(t, c.postCSRF(project+"/analyze", nil), http.StatusConflict)
		rec := c.upload(project+"/upload", file("files", "other.go", "package main\n"))
		wantStatus(t, rec, http.StatusConflict)
		wantBody(t, rec, "Wait for the job to finish")
	})
}

func TestCancelAnalysis(t *testing.T) {
	env, c, project := setupAnalyzed(t, fakeAnalysisAI())
	wantRedirect(t, c.postCSRF(project+"/analyze", nil), project+"#analysis")
	job := latestJob(t, c, project)
	jobID := int64(job["id"].(float64))
	cancelURL := project + "/jobs/" + itoa(jobID) + "/cancel"

	wantRedirect(t, c.postCSRF(cancelURL, nil), project+"#analysis")
	wantBody(t, c.get(project), "The job was canceled.")
	if job := latestJob(t, c, project); job["status"] != jobs.StatusCanceled {
		t.Errorf("job = %v", job)
	}
	wantBody(t, c.get("/dashboard"), "Not analyzed")
	env.runJobs(t)
	wantRedirect(t, c.postCSRF(cancelURL, nil), project+"#analysis")
	wantBody(t, c.get(project), "That job had already finished.")
	wantStatus(t, c.post(cancelURL, url.Values{}), http.StatusForbidden) // CSRF still required
}

func TestFailedAnalysisIsShown(t *testing.T) {
	failing := &ai.Fake{Respond: func(ai.Request) (any, error) {
		return nil, &ai.Error{Kind: ai.ErrNotConfigured, Message: "The AI service rejected the API key."}
	}}
	env, c, project := setupAnalyzed(t, failing)
	wantRedirect(t, c.postCSRF(project+"/analyze", nil), project+"#analysis")
	env.runJobs(t)
	page := c.get(project)
	wantBody(t, page, "The AI service rejected the API key. Ask the administrator to set ANTHROPIC_API_KEY.", "Analyze project")
	wantBody(t, c.get("/dashboard"), "Failed")
	wantStatus(t, c.get(project+"/analysis"), http.StatusNotFound)
}

func TestAnalysisIsOwnerScoped(t *testing.T) {
	env, alice, project := setupAnalyzed(t, fakeAnalysisAI())
	wantRedirect(t, alice.postCSRF(project+"/analyze", nil), project+"#analysis")
	jobID := int64(latestJob(t, alice, project)["id"].(float64))
	env.runJobs(t)

	bob := env.client(t)
	bob.register("bob@example.com", "correct horse")
	wantStatus(t, bob.get(project+"/analysis"), http.StatusNotFound)
	wantStatus(t, bob.get(project+"/jobs/latest"), http.StatusNotFound)
	wantStatus(t, bob.postCSRF(project+"/analyze", nil), http.StatusNotFound)
	wantStatus(t, bob.postCSRF(project+"/jobs/"+itoa(jobID)+"/cancel", nil), http.StatusNotFound)

	// Bob cannot cancel Alice's job through his own project either.
	other := bob.createProject("Mine")
	wantStatus(t, bob.postCSRF(other+"/jobs/"+itoa(jobID)+"/cancel", nil), http.StatusNotFound)
}

func TestDeletingAProjectCancelsItsJobs(t *testing.T) {
	env, c, project := setupAnalyzed(t, fakeAnalysisAI())
	wantRedirect(t, c.postCSRF(project+"/analyze", nil), project+"#analysis")
	wantRedirect(t, c.postCSRF(project+"/delete", nil), "/dashboard")
	var active int
	env.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status IN ('queued', 'running')`).Scan(&active) //nolint:errcheck
	if active != 0 {
		t.Errorf("active jobs after delete = %d", active)
	}
	env.runJobs(t) // nothing left to run, and no panic
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
