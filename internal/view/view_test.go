package view

import (
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/quantamsage/ai-code-documentation-tool/web"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"layouts/base.html": {Data: []byte(`{{define "base"}}<title>{{block "title" .}}{{.Title}}{{end}}</title>{{template "nav" .}}<main>{{block "content" .}}{{end}}</main>{{end}}`)},
		"partials/nav.html": {Data: []byte(`{{define "nav"}}<nav>nav</nav>{{end}}`)},
		"pages/a.html":      {Data: []byte(`{{define "content"}}page A: {{.Data}}{{end}}`)},
		"pages/b.html":      {Data: []byte(`{{define "title"}}Custom B{{end}}{{define "content"}}page B{{end}}`)},
		"pages/broken.html": {Data: []byte(`{{define "content"}}{{.Data.Missing.Field}}{{end}}`)},
	}
}

func TestRenderPagesAreIsolated(t *testing.T) {
	r, err := New(testFS())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	if err := r.Render(rec, 200, "a", Page{Title: "A", Data: "<script>"}); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "page A") || strings.Contains(body, "page B") {
		t.Errorf("page A body wrong: %s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("data not HTML-escaped: %s", body)
	}
	if !strings.Contains(body, "<nav>nav</nav>") {
		t.Error("partial not rendered")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}

	rec = httptest.NewRecorder()
	if err := r.Render(rec, 201, "b", Page{Title: "ignored"}); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), "<title>Custom B</title>") {
		t.Errorf("page B: code %d body %s", rec.Code, rec.Body.String())
	}
}

func TestRenderErrorsWriteNothing(t *testing.T) {
	r, err := New(testFS())
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := r.Render(rec, 200, "missing", Page{}); err == nil {
		t.Error("expected error for unknown page")
	}
	if err := r.Render(rec, 200, "broken", Page{Data: 42}); err == nil {
		t.Error("expected execution error")
	}
	if rec.Body.Len() != 0 || len(rec.Header()) != 0 {
		t.Errorf("failed render wrote output: %q", rec.Body.String())
	}
}

func TestNewRequiresLayout(t *testing.T) {
	if _, err := New(fstest.MapFS{"pages/a.html": {Data: []byte(`x`)}}); err == nil {
		t.Error("expected error without layouts")
	}
}

// TestEmbeddedTemplatesParse guards against syntax errors in the real templates.
func TestEmbeddedTemplatesParse(t *testing.T) {
	r, err := New(web.Templates())
	if err != nil {
		t.Fatalf("embedded templates: %v", err)
	}
	for _, page := range []string{"home", "error"} {
		if !r.Has(page) {
			t.Errorf("missing page %q", page)
		}
	}
}
