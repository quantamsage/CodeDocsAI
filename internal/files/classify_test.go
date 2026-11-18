package files

import (
	"errors"
	"strings"
	"testing"
)

func TestCleanPathAccepts(t *testing.T) {
	cases := map[string]string{
		"main.go":                  "main.go",
		"src/app/main.go":          "src/app/main.go",
		`src\app\main.go`:          "src/app/main.go",
		"./src//app/./main.go":     "src/app/main.go",
		"src/app/":                 "src/app",
		"docs/über café.md":        "docs/über café.md",
		".github/workflows/ci.yml": ".github/workflows/ci.yml",
		"a/b.c.d/e.tar.gz":         "a/b.c.d/e.tar.gz",
		"console.go":               "console.go", // only exact device names are reserved
		"src/" + strings.Repeat("a", MaxSegmentLen): "src/" + strings.Repeat("a", MaxSegmentLen),
	}
	for in, want := range cases {
		got, err := CleanPath(in)
		if err != nil || got != want {
			t.Errorf("CleanPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestCleanPathRejects(t *testing.T) {
	cases := []string{
		"",
		".",
		"./",
		"/etc/passwd",
		`\Windows\system32`,
		"C:/Windows/win.ini",
		`C:\Windows\win.ini`,
		"c:file.txt",
		"../secret",
		"src/../../secret",
		`src\..\..\secret`,
		"src/..",
		"file\x00.go",
		"line\nbreak.go",
		"tab\tname.go",
		"bad\xffutf8.go",
		"what?.go",
		"pipe|name.go",
		"star*.go",
		`quote".go`,
		"angle<.go",
		"trailing.",
		"trailing /x.go",
		"CON",
		"aux.txt",
		"src/nul.go",
		"Com1.json",
		"LPT9",
		strings.Repeat("a", MaxSegmentLen+1),
		strings.Repeat("a/", MaxPathDepth) + "x.go",
		strings.Repeat("abcdefgh/", MaxPathLen/9+1),
	}
	for _, in := range cases {
		got, err := CleanPath(in)
		if err == nil {
			t.Errorf("CleanPath(%q) = %q, want error", in, got)
			continue
		}
		if !errors.Is(err, ErrUnsafePath) {
			t.Errorf("CleanPath(%q) error %v does not wrap ErrUnsafePath", in, err)
		}
		if strings.ContainsAny(err.Error(), "\x00\n\t") {
			t.Errorf("CleanPath(%q) error message contains raw control characters: %q", in, err)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]Classification{
		"main.go":          {"Go", KindSource},
		"app/models.py":    {"Python", KindSource},
		"web/app.js":       {"JavaScript", KindSource},
		"web/App.JSX":      {"JavaScript", KindSource},
		"src/index.ts":     {"TypeScript", KindSource},
		"src/view.tsx":     {"TypeScript", KindSource},
		"src/Main.java":    {"Java", KindSource},
		"lib/util.c":       {"C", KindSource},
		"lib/util.h":       {"C", KindSource},
		"lib/engine.cpp":   {"C++", KindSource},
		"lib/engine.hpp":   {"C++", KindSource},
		"src/lib.rs":       {"Rust", KindSource},
		"public/index.php": {"PHP", KindSource},
		"app/user.rb":      {"Ruby", KindSource},
		"Program.cs":       {"C#", KindSource},
		"scripts/build.sh": {"Shell", KindSource},
		"db/schema.sql":    {"SQL", KindSource},
		"go.mod":           {"Go", KindManifest},
		"go.sum":           {"Go", KindManifest},
		"package.json":     {"JavaScript", KindManifest},
		"requirements.txt": {"Python", KindManifest},
		"pyproject.toml":   {"Python", KindManifest},
		"Pipfile":          {"Python", KindManifest},
		"pom.xml":          {"Java", KindManifest},
		"build.gradle":     {"Java", KindManifest},
		"Cargo.toml":       {"Rust", KindManifest},
		"composer.json":    {"PHP", KindManifest},
		"Gemfile":          {"Ruby", KindManifest},
		"App.csproj":       {"C#", KindManifest},
		"CMakeLists.txt":   {"CMake", KindManifest},
		"config/app.yaml":  {"YAML", KindConfig},
		"tsconfig.json":    {"JSON", KindConfig},
		"settings.toml":    {"TOML", KindConfig},
		"Dockerfile":       {"Dockerfile", KindConfig},
		"Dockerfile.prod":  {"Dockerfile", KindConfig},
		"Makefile":         {"Makefile", KindConfig},
		".gitignore":       {"gitignore", KindConfig},
		".env.example":     {"dotenv", KindConfig},
		"README.md":        {"Markdown", KindDocumentation},
		"docs/guide.rst":   {"reStructuredText", KindDocumentation},
		"LICENSE":          {"Text", KindDocumentation},
		"notes.txt":        {"Text", KindDocumentation},
	}
	for p, want := range cases {
		got, ok := Classify(p)
		if !ok || got != want {
			t.Errorf("Classify(%q) = %+v, %v; want %+v", p, got, ok, want)
		}
	}
}

func TestClassifyRejectsUnsupported(t *testing.T) {
	for _, p := range []string{
		"logo.png", "photo.jpg", "font.woff2", "app.exe", "lib.so", "archive.zip", "data.bin",
		"video.mp4", "doc.pdf", "unknown.xyz", "noextension", "Main.class",
	} {
		if c, ok := Classify(p); ok {
			t.Errorf("Classify(%q) = %+v, want unsupported", p, c)
		}
	}
}

func TestIsSecretFile(t *testing.T) {
	secret := []string{".env", "config/.env", ".env.local", ".env.production", "server.pem", "tls.key", "id_rsa", ".netrc", "certs/store.p12"}
	for _, p := range secret {
		if !IsSecretFile(p) {
			t.Errorf("IsSecretFile(%q) = false", p)
		}
	}
	safe := []string{".env.example", ".env.sample", ".env.template", "env.go", "keys.go", "README.md"}
	for _, p := range safe {
		if IsSecretFile(p) {
			t.Errorf("IsSecretFile(%q) = true", p)
		}
	}
}

func TestIgnoredDirAndFile(t *testing.T) {
	cases := map[string]string{
		"node_modules/react/index.js":   "node_modules",
		"web/node_modules/x/y.js":       "node_modules",
		".git/config":                   ".git",
		"__MACOSX/src/._main.go":        "__MACOSX",
		"app/__pycache__/m.cpython.pyc": "__pycache__",
		"src/main.go":                   "",
		"node_modules":                  "", // a file named like the dir is not inside it
	}
	for p, want := range cases {
		if got := IgnoredDir(p); got != want {
			t.Errorf("IgnoredDir(%q) = %q, want %q", p, got, want)
		}
	}
	for _, p := range []string{".DS_Store", "src/Thumbs.db", "src/._main.go"} {
		if !IsIgnoredFile(p) {
			t.Errorf("IsIgnoredFile(%q) = false", p)
		}
	}
	if IsIgnoredFile("src/main.go") {
		t.Error("IsIgnoredFile(src/main.go) = true")
	}
}

func TestContentChecks(t *testing.T) {
	if !IsText([]byte("package main\n\nfunc main() {}\n")) {
		t.Error("Go source not detected as text")
	}
	if !IsText([]byte("\xef\xbb\xbfcafé")) {
		t.Error("UTF-8 with BOM not detected as text")
	}
	if !IsText(nil) {
		t.Error("empty file not detected as text")
	}
	for _, data := range [][]byte{{'a', 0, 'b'}, []byte("latin1 \xe9t\xe9")} {
		if IsText(data) {
			t.Errorf("IsText(%q) = true", data)
		}
	}

	elf := append([]byte{0x7f, 'E', 'L', 'F', 2, 1, 1}, make([]byte, 60)...)
	pe := append([]byte("MZ"), make([]byte, 100)...)
	wasm := []byte{0, 'a', 's', 'm', 1, 0, 0, 0}
	for name, data := range map[string][]byte{"elf": elf, "pe": pe, "wasm": wasm} {
		if !IsExecutableContent(data) {
			t.Errorf("IsExecutableContent(%s) = false", name)
		}
	}
	if IsExecutableContent([]byte("MZ is how this README starts, and it is plain text that goes on for a while.")) {
		t.Error("text starting with MZ treated as executable")
	}

	if !IsArchive("vendor/lib.jar") || !IsArchive("x.tar.gz") || IsArchive("main.go") {
		t.Error("IsArchive misclassified")
	}
	if !IsExecutableName("tool.EXE") || !IsExecutableName("lib.dll") || IsExecutableName("main.go") {
		t.Error("IsExecutableName misclassified")
	}
}
