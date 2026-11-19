package server

import (
	"archive/zip"
	"bytes"
	"io"
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

func TestDocumentViewerRendersMarkdown(t *testing.T) {
	_, c, project := setupDocumented(t)
	rec := c.get(project + "/docs/project?key=architecture")
	wantStatus(t, rec, http.StatusOK)
	wantBody(t, rec,
		`<h1 id="doc-architecture">Architecture</h1>`, `<h2 id="doc-details">Details</h2>`,
		`On this page`, `<a href="#doc-details">Details</a>`,
		`<blockquote>`, `<strong>Assumption:</strong>`,
		`<pre class="code-block hl" data-lang="go"><code><span class="hl-kd">func</span>`,
		`<!-- raw HTML omitted -->`,
		`rel="prev"`, `Project Overview`, `rel="next"`, `Directory and File Documentation`,
		`href="`+project+`/docs/project/download?key=architecture&amp;version=1"`,
		`href="`+project+`/docs/export.zip"`,
		`/static/css/highlight.css`,
	)
	body := rec.Body.String()
	if strings.Contains(body, "<img src=x") || strings.Contains(body, "markdown-source") {
		t.Error("document not rendered safely")
	}
	// The first document has no "previous" link.
	if strings.Contains(c.get(project+"/docs/project?key=overview").Body.String(), `rel="prev"`) {
		t.Error("first document has a previous link")
	}
	wantBody(t, c.get(project+"/docs"), `href="`+project+`/docs/project/download?key=overview"`, "Download all (ZIP)")
}

func TestDownloadMarkdown(t *testing.T) {
	_, c, project := setupDocumented(t)
	rec := c.get(project + "/docs/project/download?key=overview")
	wantStatus(t, rec, http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Errorf("content type %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "attachment; filename=service-project-overview.md" {
		t.Errorf("content disposition %q", cd)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("headers %v", rec.Header())
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "# Project Overview\n\n## Details\n") || !strings.Contains(body, "```go\nfunc main() {}\n```\n") {
		t.Errorf("markdown = %q", body)
	}
	wantStatus(t, c.get(project+"/docs/project/download?key=overview&version=1"), http.StatusOK)
	wantStatus(t, c.get(project+"/docs/project/download?key=overview&version=2"), http.StatusNotFound)
	wantStatus(t, c.get(project+"/docs/module/download?key=nope"), http.StatusNotFound)
}

func TestExportZip(t *testing.T) {
	env, c, project := setupDocumented(t)
	wantRedirect(t, c.postCSRF(project+"/docs/generate", url.Values{"scope": {"file"}, "key": {"main.go"}}), project+"#analysis")
	env.runJobs(t)

	rec := c.get(project + "/docs/export.zip")
	wantStatus(t, rec, http.StatusOK)
	if rec.Header().Get("Content-Type") != "application/zip" || rec.Header().Get("Content-Disposition") != "attachment; filename=service-docs.zip" {
		t.Errorf("headers %v", rec.Header())
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		entries[f.Name] = string(b)
	}
	for _, name := range []string{
		"service-docs/README.md", "service-docs/sections/01-overview.md", "service-docs/sections/02-architecture.md",
		"service-docs/sections/04-api.md", "service-docs/modules/root.md", "service-docs/files/main.go.md",
	} {
		if _, ok := entries[name]; !ok {
			t.Errorf("missing %s in %v", name, entries)
		}
	}
	if _, ok := entries["service-docs/sections/05-database.md"]; ok {
		t.Error("database section exported for a project without one")
	}
	if !strings.Contains(entries["service-docs/README.md"], "- [Project Overview](sections/01-overview.md) (version 1)") {
		t.Errorf("README = %s", entries["service-docs/README.md"])
	}
	if !strings.Contains(entries["service-docs/files/main.go.md"], "```go\nfunc main() {}\n```") {
		t.Errorf("file doc = %q", entries["service-docs/files/main.go.md"])
	}
}

func TestExportWithoutDocs(t *testing.T) {
	_, c, project := setupAnalyzed(t, fakeDocsAI())
	wantStatus(t, c.get(project+"/docs/export.zip"), http.StatusNotFound)
	if strings.Contains(c.get(project+"/docs").Body.String(), "Download all") {
		t.Error("export offered without documents")
	}
}

func TestDocumentDownloadsOwnerIsolation(t *testing.T) {
	env, _, project := setupDocumented(t)
	mallory := env.client(t)
	mallory.register("mallory@example.com", "correct horse")
	for _, path := range []string{"/docs/export.zip", "/docs/project/download?key=overview", "/docs/project/download?key=overview&version=1"} {
		rec := mallory.get(project + path)
		wantStatus(t, rec, http.StatusNotFound)
		if rec.Header().Get("Content-Disposition") != "" {
			t.Errorf("%s: sent an attachment", path)
		}
	}
	anon := env.client(t)
	if rec := anon.get(project + "/docs/export.zip"); rec.Code != http.StatusSeeOther {
		t.Errorf("anonymous export: %d", rec.Code)
	}
}
