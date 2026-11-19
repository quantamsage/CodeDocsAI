package docs

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func TestSlugAndDownloadName(t *testing.T) {
	for in, want := range map[string]string{
		"Project Overview":         "project-overview",
		"File: internal/main.go":   "file-internal-main.go",
		"  ../../etc/passwd  ":     "etc-passwd",
		"Ünïcode & <b>":            "n-code-b",
		"":                         "untitled",
		"...":                      "untitled",
		strings.Repeat("ab ", 100): strings.Trim(strings.Repeat("ab-", 27), "-"),
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
	if got := DownloadName("My Service", Doc{Title: "API Documentation"}); got != "my-service-api-documentation.md" {
		t.Errorf("DownloadName = %q", got)
	}
}

func TestExportPath(t *testing.T) {
	for _, tc := range []struct {
		d    Doc
		want string
	}{
		{Doc{Scope: ScopeProject, Key: "overview"}, "sections/01-overview.md"},
		{Doc{Scope: ScopeProject, Key: "usage"}, "sections/08-usage.md"},
		{Doc{Scope: ScopeModule, Key: "."}, "modules/root.md"},
		{Doc{Scope: ScopeModule, Key: "internal/server"}, "modules/internal/server.md"},
		{Doc{Scope: ScopeFile, Key: "cmd/server/main.go"}, "files/cmd/server/main.go.md"},
		{Doc{Scope: ScopeFile, Key: "../../evil/../x y.go"}, "files/evil/x_y.go.md"},
		{Doc{Scope: ScopeFile, Key: "/abs\\win\\.hidden"}, "files/abs/win/hidden.md"},
		{Doc{Scope: ScopeFile, Key: ".."}, "files/untitled.md"},
	} {
		if got := ExportPath(tc.d); got != tc.want {
			t.Errorf("ExportPath(%s %q) = %q, want %q", tc.d.Scope, tc.d.Key, got, tc.want)
		}
	}
}

func TestWriteZip(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC)
	docs := []Doc{
		{Scope: ScopeFile, Key: "cmd/main.go", Title: "File: cmd/main.go", Version: 1, Markdown: "# File\n\n```go\nfunc main() {}\n```", CreatedAt: now},
		{Scope: ScopeModule, Key: ".", Title: "Module: project root", Version: 2, Markdown: "# Root\n", CreatedAt: now},
		{Scope: ScopeProject, Key: "architecture", Title: "Architecture", Version: 1, Markdown: "# Architecture\n\n| a | b |\n|---|---|\n", CreatedAt: now},
		{Scope: ScopeProject, Key: "overview", Title: "Project Overview [v2]", Version: 3, Markdown: "# Overview\n", CreatedAt: now},
		{Scope: ScopeFile, Key: "a b.go", Title: "File: a b.go", Version: 1, Markdown: "# one", CreatedAt: now},
		{Scope: ScopeFile, Key: "a_b.go", Title: "File: a_b.go", Version: 1, Markdown: "# two", CreatedAt: now},
	}
	var buf bytes.Buffer
	if err := WriteZip(&buf, "My Service", docs, now); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	var names []string
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		files[f.Name] = string(b)
		names = append(names, f.Name)
		if !strings.HasPrefix(f.Name, "my-service-docs/") || strings.Contains(f.Name, "..") {
			t.Errorf("unsafe entry %q", f.Name)
		}
	}
	want := []string{
		"my-service-docs/sections/01-overview.md",
		"my-service-docs/sections/02-architecture.md",
		"my-service-docs/modules/root.md",
		"my-service-docs/files/a_b.go.md",
		"my-service-docs/files/a_b.go-2.md",
		"my-service-docs/files/cmd/main.go.md",
		"my-service-docs/README.md",
	}
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Errorf("entries:\n%s\nwant:\n%s", strings.Join(names, "\n"), strings.Join(want, "\n"))
	}
	if got := files["my-service-docs/files/cmd/main.go.md"]; got != "# File\n\n```go\nfunc main() {}\n```\n" {
		t.Errorf("file doc = %q", got)
	}
	readme := files["my-service-docs/README.md"]
	for _, s := range []string{
		"# My Service documentation", "2026-03-04 05:06 UTC", "## Project", "## Modules", "## Files",
		`- [Project Overview \[v2\]](sections/01-overview.md) (version 3)`,
		"- [Module: project root](modules/root.md) (version 2)",
		"(files/cmd/main.go.md)",
	} {
		if !strings.Contains(readme, s) {
			t.Errorf("README missing %q:\n%s", s, readme)
		}
	}
}

func TestWriteZipEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteZip(&buf, "x", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil || len(zr.File) != 1 || zr.File[0].Name != "x-docs/README.md" {
		t.Fatalf("empty export = %v, %v", zr, err)
	}
}
