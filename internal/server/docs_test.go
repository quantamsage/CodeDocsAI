package server

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/quantamsage/ai-code-documentation-tool/internal/ai"
)

var reDocTitle = regexp.MustCompile(`Document title: (.+)`)

// fakeDocsAI answers analysis requests like fakeAnalysisAI and writes a
// small Markdown document for documentation requests. Each document
// contains raw HTML so tests can check it is never rendered.
func fakeDocsAI() *ai.Fake {
	analysisAI := fakeAnalysisAI()
	return &ai.Fake{Respond: func(req ai.Request) (any, error) {
		if strings.Contains(req.System, "technical writer") {
			title := "Untitled"
			if m := reDocTitle.FindStringSubmatch(req.Prompt); m != nil {
				title = m[1]
			}
			md := "# " + title + "\n\n## Details\n\nGenerated text for " + title + ".\n\n<img src=x onerror=alert(1)>\n\n> **Assumption:** Something inferred.\n\n```go\nfunc main() {}\n```\n"
			return map[string]any{"markdown": md}, nil
		}
		return analysisAI.Respond(req)
	}}
}

// setupDocumented analyzes a project and generates all its documentation.
func setupDocumented(t *testing.T) (*testEnv, *client, string) {
	t.Helper()
	env, c, project := setupAnalyzed(t, fakeDocsAI())
	wantRedirect(t, c.postCSRF(project+"/analyze", nil), project+"#analysis")
	env.runJobs(t)
	wantRedirect(t, c.postCSRF(project+"/docs/generate", url.Values{"scope": {"all"}}), project+"#analysis")
	env.runJobs(t)
	if job := latestJob(t, c, project); job["status"] != "succeeded" || job["type"] != "generate_docs" {
		t.Fatalf("docs job = %v", job)
	}
	return env, c, project
}

func TestGenerateDocumentationEndToEnd(t *testing.T) {
	env, c, project := setupDocumented(t)

	wantBody(t, c.get(project), "View documentation", "Regenerate documentation")
	wantBody(t, c.get("/dashboard"), "Documented")

	index := c.get(project + "/docs")
	wantStatus(t, index, http.StatusOK)
	wantBody(t, index, "Project documentation", "Project Overview", "Architecture", "API Documentation", "Modules", "Regenerate all",
		`href="`+project+`/docs/project?key=overview"`, `href="`+project+`/docs/module?key=."`)
	if strings.Contains(index.Body.String(), "Database Documentation") {
		t.Error("database section listed for a project without data models")
	}

	doc := c.get(project + "/docs/project?key=overview")
	wantStatus(t, doc, http.StatusOK)
	wantBody(t, doc, "Generated text for Project Overview", "Version 1", "by fake-model", `aria-current="page"`)
	if strings.Contains(doc.Body.String(), "<img src=x") {
		t.Fatal("raw HTML from AI output rendered")
	}

	// Regenerate one section: a new version, the old one stays readable.
	wantRedirect(t, c.postCSRF(project+"/docs/generate", url.Values{"scope": {"project"}, "key": {"overview"}, "return": {project + "/docs"}}), project+"/docs")
	env.runJobs(t)
	doc = c.get(project + "/docs/project?key=overview")
	wantBody(t, doc, "Version 2", "Versions (2)", `href="`+project+`/docs/project?key=overview&amp;version=1"`)
	old := c.get(project + "/docs/project?key=overview&version=1")
	wantStatus(t, old, http.StatusOK)
	wantBody(t, old, "Version 1", "an older version")
	wantStatus(t, c.get(project+"/docs/project?key=overview&version=9"), http.StatusNotFound)
	wantStatus(t, c.get(project+"/docs/project?key=overview&version=abc"), http.StatusNotFound)
	wantStatus(t, c.get(project+"/docs/project?key=nope"), http.StatusNotFound)

	// Single module and single file.
	wantRedirect(t, c.postCSRF(project+"/docs/generate", url.Values{"scope": {"module"}, "key": {"."}}), project+"#analysis")
	env.runJobs(t)
	wantBody(t, c.get(project+"/docs/module?key=."), "Version 2", "Module: project root")

	filePage := c.get(project + "/files/main.go")
	wantBody(t, filePage, "Generate file documentation", `name="key" value="main.go"`)
	wantRedirect(t, c.postCSRF(project+"/docs/generate", url.Values{"scope": {"file"}, "key": {"main.go"}, "return": {project + "/files/main.go"}}), project+"/files/main.go")
	env.runJobs(t)
	wantBody(t, c.get(project+"/files/main.go"), "View file documentation", "Regenerate file documentation")
	wantBody(t, c.get(project+"/docs/file?key=main.go"), "File: main.go", "Generated text for File: main.go")
	wantBody(t, c.get(project+"/docs"), `href="`+project+`/docs/file?key=main.go"`)
}

func TestGenerateDocumentationRejections(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		env := newTestEnv(t)
		c := env.client(t)
		c.register("alice@example.com", "correct horse")
		project := c.createProject("Service")
		rec := c.postCSRF(project+"/docs/generate", nil)
		wantStatus(t, rec, http.StatusServiceUnavailable)
		wantBody(t, rec, "ANTHROPIC_API_KEY")
	})
	t.Run("no analysis", func(t *testing.T) {
		_, c, project := setupAnalyzed(t, fakeDocsAI())
		wantStatus(t, c.postCSRF(project+"/docs/generate", nil), http.StatusConflict)
		wantBody(t, c.get(project+"/docs"), "No analysis yet")
	})
	t.Run("bad requests", func(t *testing.T) {
		env, c, project := setupAnalyzed(t, fakeDocsAI())
		wantRedirect(t, c.postCSRF(project+"/analyze", nil), project+"#analysis")
		env.runJobs(t)
		for _, form := range []url.Values{
			{"scope": {"everything"}},
			{"scope": {"project"}, "key": {"nope"}},
			{"scope": {"project"}, "key": {"database"}}, // no data models
			{"scope": {"module"}, "key": {"missing"}},
			{"scope": {"file"}, "key": {"../etc/passwd"}},
		} {
			if rec := c.postCSRF(project+"/docs/generate", form); rec.Code != http.StatusBadRequest {
				t.Errorf("%v: status %d", form, rec.Code)
			}
		}
		wantStatus(t, c.post(project+"/docs/generate", nil), http.StatusForbidden)
	})
	t.Run("job active", func(t *testing.T) {
		env, c, project := setupAnalyzed(t, fakeDocsAI())
		wantRedirect(t, c.postCSRF(project+"/analyze", nil), project+"#analysis")
		env.runJobs(t)
		wantRedirect(t, c.postCSRF(project+"/docs/generate", nil), project+"#analysis")
		wantStatus(t, c.postCSRF(project+"/docs/generate", nil), http.StatusConflict)
		wantStatus(t, c.postCSRF(project+"/analyze", nil), http.StatusConflict)
		page := c.get(project + "/docs")
		wantBody(t, page, "Documentation job queued")
		if strings.Contains(page.Body.String(), "Regenerate all") || strings.Contains(page.Body.String(), "Generate documentation</button>") {
			t.Error("generate button shown while a job is active")
		}
	})
	t.Run("unsafe return", func(t *testing.T) {
		env, c, project := setupAnalyzed(t, fakeDocsAI())
		wantRedirect(t, c.postCSRF(project+"/analyze", nil), project+"#analysis")
		env.runJobs(t)
		wantRedirect(t, c.postCSRF(project+"/docs/generate", url.Values{"return": {"https://evil.example/"}}), project+"#analysis")
	})
}

func TestDocumentationOwnerIsolation(t *testing.T) {
	env, _, project := setupDocumented(t)
	mallory := env.client(t)
	mallory.register("mallory@example.com", "correct horse")
	for _, path := range []string{"/docs", "/docs/project?key=overview", "/docs/project?key=overview&version=1", "/docs/module?key=."} {
		wantStatus(t, mallory.get(project+path), http.StatusNotFound)
	}
	wantStatus(t, mallory.postCSRF(project+"/docs/generate", nil), http.StatusNotFound)
}
