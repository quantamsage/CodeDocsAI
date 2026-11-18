package server

import (
	"archive/zip"
	"bytes"
	"errors"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quantamsage/ai-code-documentation-tool/internal/config"
)

// formPart is one part of a multipart upload.
type formPart struct {
	field    string
	filename string // empty for plain fields
	content  string
}

func field(name, value string) formPart { return formPart{field: name, content: value} }
func file(field, name, content string) formPart {
	return formPart{field: field, filename: name, content: content}
}
func zipPart(name string, data []byte) formPart {
	return formPart{field: "files", filename: name, content: string(data)}
}
func multipartBody(t *testing.T, parts ...formPart) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		var w interface{ Write([]byte) (int, error) }
		var err error
		if p.filename == "" {
			w, err = mw.CreateFormField(p.field)
		} else {
			// Written by hand so relative folder paths are kept, as browsers send them.
			h := textproto.MIMEHeader{}
			h.Set("Content-Disposition", `form-data; name="`+p.field+`"; filename="`+p.filename+`"`)
			h.Set("Content-Type", "application/octet-stream")
			w, err = mw.CreatePart(h)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(p.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

// upload posts a multipart form with the CSRF token as the first field.
func (c *client) upload(target string, parts ...formPart) *httptest.ResponseRecorder {
	c.t.Helper()
	return c.uploadRaw(target, append([]formPart{field("csrf_token", c.csrf())}, parts...)...)
}

func (c *client) uploadRaw(target string, parts ...formPart) *httptest.ResponseRecorder {
	c.t.Helper()
	body, contentType := multipartBody(c.t, parts...)
	req := httptest.NewRequest(http.MethodPost, target, body)
	req.Header.Set("Content-Type", contentType)
	return c.send(req)
}

func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func signedInWithProject(t *testing.T, env *testEnv, email string) (*client, string) {
	t.Helper()
	c := env.client(t)
	c.register(email, "correct horse")
	return c, c.createProject("Demo")
}

func TestUploadSingleFile(t *testing.T) {
	env := newTestEnv(t)
	c, project := signedInWithProject(t, env, "alice@example.com")

	wantStatus(t, c.get(project+"/upload"), http.StatusOK)
	wantBody(t, c.get(project+"/upload"), `enctype="multipart/form-data"`, `name="csrf_token"`, `name="files"`, `webkitdirectory`)

	rec := c.upload(project+"/upload", file("files", "main.go", "package main\n\nfunc main() {}\n"))
	wantRedirect(t, rec, project)

	rec = c.get(project)
	wantBody(t, rec, "Uploaded 1 file. The project now has 1 file.", `href="`+project+`/files/main.go"`, "Go", "Not analyzed")
	if strings.Contains(rec.Body.String(), "No files uploaded yet") {
		t.Error("project page still shows the empty state")
	}
	wantBody(t, c.get("/dashboard"), "Go", "Not analyzed")

	rec = c.get(project + "/files/main.go")
	wantStatus(t, rec, http.StatusOK)
	wantBody(t, rec, `<h1 class="file-path">main.go</h1>`, `<span class="line">package main</span>`, `<span class="line">func main() {}</span>`, "3 lines")
}

func TestUploadMultipleFilesAndFolder(t *testing.T) {
	env := newTestEnv(t)
	c, project := signedInWithProject(t, env, "alice@example.com")

	rec := c.upload(project+"/upload",
		file("files", "app.py", "print('hi')\n"),
		file("files", "requirements.txt", "flask==3.0\n"),
		file("folder", "myproject/src/utils/strings.ts", "export const x = 1;\n"),
		file("folder", `myproject\lib\Main.java`, "class Main {}\n"),
		file("folder", "myproject/node_modules/pkg/index.js", "module.exports = 1\n"),
		file("files", "logo.png", "\x89PNG\r\n"),
		file("files", "", ""), // empty file input
	)
	wantRedirect(t, rec, project)

	page := c.get(project).Body.String()
	for _, p := range []string{"app.py", "requirements.txt", "src/utils/strings.ts", "lib/Main.java"} {
		if !strings.Contains(page, `href="`+project+`/files/`+p+`"`) {
			t.Errorf("project page has no link to %s", p)
		}
	}
	for _, want := range []string{
		"Uploaded 4 files.", "Skipped 2 files:", "logo.png (unsupported file type)",
		"node_modules/pkg/index.js (dependency, build or VCS directory)",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("project page missing %q", want)
		}
	}
	wantStatus(t, c.get(project+"/files/node_modules/pkg/index.js"), http.StatusNotFound)
}

func TestUploadZipPreservesStructure(t *testing.T) {
	env := newTestEnv(t)
	c, project := signedInWithProject(t, env, "alice@example.com")

	archive := makeZip(t, map[string]string{
		"shop/go.mod":                  "module example.com/shop\n",
		"shop/cmd/api/main.go":         "package main\n",
		"shop/internal/cart/cart.go":   "package cart\n",
		"shop/web/static/js/app.js":    "document.title = '<b>x</b>';\n",
		"shop/web/templates/page.html": "<script>alert('xss')</script>\n",
		"shop/config/settings.yaml":    "port: 8080\n",
		"shop/README.md":               "# Shop\n",
		"shop/.env":                    "API_KEY=secret\n",
	})
	wantRedirect(t, c.upload(project+"/upload", zipPart("shop.zip", archive)), project)

	page := c.get(project)
	wantBody(t, page, "Uploaded 7 files.", ".env (may contain secrets)",
		`<span class="tree-name">cmd/</span>`, `<span class="tree-name">internal/</span>`, `<span class="tree-name">cart/</span>`,
		`href="`+project+`/files/internal/cart/cart.go"`, `href="`+project+`/files/web/templates/page.html"`)
	if strings.Contains(page.Body.String(), "shop/") {
		t.Error("common root directory was not stripped")
	}

	rec := c.get(project + "/files/web/templates/page.html")
	wantStatus(t, rec, http.StatusOK)
	wantBody(t, rec, "&lt;script&gt;alert(&#39;xss&#39;)&lt;/script&gt;", `aria-current="page"`)
	if strings.Contains(rec.Body.String(), "<script>alert") {
		t.Fatal("uploaded HTML rendered unescaped")
	}
	wantStatus(t, c.get(project+"/files/.env"), http.StatusNotFound)

	var count int
	env.db.QueryRow(`SELECT file_count FROM projects`).Scan(&count)
	if count != 7 {
		t.Errorf("file_count = %d, want 7", count)
	}
}

func TestUploadRejections(t *testing.T) {
	env := newTestEnv(t)
	c, project := signedInWithProject(t, env, "alice@example.com")

	cases := map[string]struct {
		parts []formPart
		want  string
	}{
		"zip path traversal": {
			[]formPart{zipPart("evil.zip", makeZip(t, map[string]string{"../../etc/cron.d/x.sh": "echo pwned\n"}))},
			"unsafe path",
		},
		"unsafe folder path": {
			[]formPart{file("folder", "proj/../../../outside.go", "package x\n")},
			"unsafe file path",
		},
		"corrupt zip": {
			[]formPart{zipPart("broken.zip", []byte("PK\x03\x04 definitely not a zip"))},
			"could not be read",
		},
		"tarball": {
			[]formPart{file("files", "project.tar.gz", "\x1f\x8b")},
			"Only .zip archives are supported",
		},
		"nothing supported": {
			[]formPart{file("files", "photo.jpg", "\xff\xd8\xff"), file("files", "app.exe", "MZ")},
			"No supported source",
		},
		"no files at all": {
			nil,
			"No supported source",
		},
		"invalid target folder": {
			[]formPart{field("directory", "../escape"), file("files", "a.go", "package a\n")},
			"target folder is not a valid path",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := c.upload(project+"/upload", tc.parts...)
			wantStatus(t, rec, http.StatusUnprocessableEntity)
			wantBody(t, rec, tc.want, `enctype="multipart/form-data"`)
		})
	}

	var rows int
	env.db.QueryRow(`SELECT COUNT(*) FROM project_files`).Scan(&rows)
	if rows != 0 {
		t.Errorf("rejected uploads stored %d files", rows)
	}
	entries, _ := os.ReadDir(filepath.Join(env.srv.cfg.DataDir, "uploads", "1"))
	if len(entries) != 0 {
		t.Errorf("rejected uploads left %d blobs on disk", len(entries))
	}
}

func TestUploadCSRF(t *testing.T) {
	env := newTestEnv(t)
	c, project := signedInWithProject(t, env, "alice@example.com")
	token := c.csrf()
	src := file("files", "main.go", "package main\n")

	wantStatus(t, c.uploadRaw(project+"/upload", src), http.StatusForbidden)
	wantStatus(t, c.uploadRaw(project+"/upload", field("csrf_token", "forged"), src), http.StatusForbidden)
	wantStatus(t, c.uploadRaw(project+"/upload", src, field("csrf_token", token)), http.StatusForbidden)
	wantStatus(t, c.uploadRaw(project+"/upload", file("csrf_token", "token.txt", token), src), http.StatusForbidden)

	// Scripted clients may send the token as a header instead.
	body, ct := multipartBody(t, src)
	req := httptest.NewRequest(http.MethodPost, project+"/upload", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-CSRF-Token", token)
	wantRedirect(t, c.send(req), project)

	// Multipart bodies are only accepted on upload routes.
	rec := c.uploadRaw("/projects", field("csrf_token", token), field("name", "Other"))
	wantStatus(t, rec, http.StatusUnsupportedMediaType)
}

func TestUploadRequiresSignIn(t *testing.T) {
	env := newTestEnv(t)
	alice, project := signedInWithProject(t, env, "alice@example.com")
	_ = alice
	anon := env.client(t)
	rec := anon.uploadRaw(project+"/upload", file("files", "main.go", "package main\n"))
	wantRedirect(t, rec, "/login")
	rec = anon.get(project + "/upload")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login?next=") {
		t.Errorf("anonymous upload form: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestUploadAndExplorerAreOwnerScoped(t *testing.T) {
	env := newTestEnv(t)
	alice, project := signedInWithProject(t, env, "alice@example.com")
	wantRedirect(t, alice.upload(project+"/upload", file("files", "secret.go", "package secret\n")), project)

	bob := env.client(t)
	bob.register("bob@example.com", "correct horse")
	wantStatus(t, bob.get(project+"/upload"), http.StatusNotFound)
	wantStatus(t, bob.get(project+"/files/secret.go"), http.StatusNotFound)
	wantStatus(t, bob.upload(project+"/upload", file("files", "evil.go", "package evil\n")), http.StatusNotFound)

	var rows int
	env.db.QueryRow(`SELECT COUNT(*) FROM project_files WHERE path = 'evil.go'`).Scan(&rows)
	if rows != 0 {
		t.Error("another user's upload was stored")
	}
}

func TestUploadReplaceAndTargetDirectory(t *testing.T) {
	env := newTestEnv(t)
	c, project := signedInWithProject(t, env, "alice@example.com")
	wantRedirect(t, c.upload(project+"/upload", file("files", "old.go", "package old\n")), project)

	wantRedirect(t, c.upload(project+"/upload", field("directory", "pkg/util"), file("files", "strings.go", "package util\n")), project)
	wantStatus(t, c.get(project+"/files/pkg/util/strings.go"), http.StatusOK)
	wantStatus(t, c.get(project+"/files/old.go"), http.StatusOK)

	rec := c.upload(project+"/upload", field("replace", "1"), file("files", "new.rb", "puts 1\n"))
	wantRedirect(t, rec, project)
	wantBody(t, c.get(project), "after removing 2 existing files", "Ruby")
	wantStatus(t, c.get(project+"/files/old.go"), http.StatusNotFound)
	wantStatus(t, c.get(project+"/files/new.rb"), http.StatusOK)
}

func TestUploadRequestSizeLimit(t *testing.T) {
	env := newTestEnvWith(t, func(cfg *config.Config) { cfg.Upload.MaxRequestBytes = 2 << 10 })
	c, project := signedInWithProject(t, env, "alice@example.com")

	rec := c.upload(project+"/upload", file("files", "big.go", "package big\n"+strings.Repeat("// x\n", 1000)))
	wantStatus(t, rec, http.StatusRequestEntityTooLarge)
	wantBody(t, rec, "larger than the 2.0 KiB limit")
}

func TestUploadProjectFileLimit(t *testing.T) {
	env := newTestEnvWith(t, func(cfg *config.Config) { cfg.Upload.MaxFiles = 2 })
	c, project := signedInWithProject(t, env, "alice@example.com")
	wantRedirect(t, c.upload(project+"/upload", file("files", "a.go", "package a\n"), file("files", "b.go", "package b\n")), project)

	rec := c.upload(project+"/upload", file("files", "c.go", "package c\n"))
	wantStatus(t, rec, http.StatusUnprocessableEntity)
	wantBody(t, rec, "the limit is 2")
}

func TestDeleteProjectRemovesStoredFiles(t *testing.T) {
	env := newTestEnv(t)
	c, project := signedInWithProject(t, env, "alice@example.com")
	wantRedirect(t, c.upload(project+"/upload", file("files", "main.go", "package main\n")), project)

	dir := filepath.Join(env.srv.cfg.DataDir, "uploads", strings.TrimPrefix(project, "/projects/"))
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Fatalf("upload dir before delete: %v entries, %v", len(entries), err)
	}
	wantRedirect(t, c.postCSRF(project+"/delete", nil), "/dashboard")
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("upload dir still exists after project delete: %v", err)
	}
}

func TestFileViewPaths(t *testing.T) {
	env := newTestEnv(t)
	c, project := signedInWithProject(t, env, "alice@example.com")
	wantRedirect(t, c.upload(project+"/upload",
		file("folder", "p/docs/getting started#1.md", "# Hi\n"),
		file("files", "empty.go", ""),
	), project)

	wantBody(t, c.get(project), `href="`+project+`/files/docs/getting%20started%231.md"`)
	rec := c.get(project + "/files/docs/getting%20started%231.md")
	wantStatus(t, rec, http.StatusOK)
	wantBody(t, rec, "# Hi")
	wantBody(t, c.get(project+"/files/empty.go"), "This file is empty.")

	wantStatus(t, c.get(project+"/files/missing.go"), http.StatusNotFound)
	wantStatus(t, c.get(project+"/files/docs%2F..%2F..%2Fapp.db"), http.StatusNotFound)
	wantRedirect(t, c.get(project+"/files/"), project+"#files")
}

func TestSplitLines(t *testing.T) {
	cases := []struct {
		in        string
		truncated bool
		want      []string
	}{
		{"", false, nil},
		{"a\nb\n", false, []string{"a", "b"}},
		{"a\r\nb", false, []string{"a", "b"}},
		{"\xef\xbb\xbfpackage x\n", false, []string{"package x"}},
		{"a\nb\npartial", true, []string{"a", "b"}},
		{"onlypartial", true, []string{"onlypartial"}},
		{"bad \xff byte", false, []string{"bad � byte"}},
	}
	for _, c := range cases {
		got := splitLines([]byte(c.in), c.truncated)
		if strings.Join(got, "|") != strings.Join(c.want, "|") || len(got) != len(c.want) {
			t.Errorf("splitLines(%q, %v) = %q, want %q", c.in, c.truncated, got, c.want)
		}
	}
}

func TestFileURLEscapesSegments(t *testing.T) {
	if got := fileURL(4, "src/my file#2?.go"); got != "/projects/4/files/src/my%20file%232%3F.go" {
		t.Errorf("fileURL = %q", got)
	}
}
