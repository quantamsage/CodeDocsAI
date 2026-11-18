package files

import (
	"archive/zip"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var testLimits = Limits{MaxFileBytes: 64 << 10, MaxFiles: 50, MaxTotalBytes: 1 << 20, MaxCompressionRatio: 100}

type zipEntry struct {
	name   string
	body   string
	header func(*zip.FileHeader)
}

func buildZip(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if strings.HasSuffix(e.name, "/") {
			h.SetMode(fs.ModeDir | 0o755)
		} else {
			h.SetMode(0o644)
		}
		if e.header != nil {
			e.header(h)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newTestBatch(t *testing.T, limits Limits) (*Batch, string) {
	t.Helper()
	dir := t.TempDir()
	return NewBatch(NewBlobStore(dir), 7, limits), dir
}

func addZip(b *Batch, data []byte) error {
	return b.AddZip(bytes.NewReader(data), int64(len(data)))
}

func stagedPaths(b *Batch) []string {
	var out []string
	for _, s := range b.Staged() {
		out = append(out, s.Path)
	}
	sort.Strings(out)
	return out
}

func skippedReasons(b *Batch) map[string]string {
	skipped, _ := b.Skipped()
	out := map[string]string{}
	for _, s := range skipped {
		out[s.Path] = s.Reason
	}
	return out
}

func TestAddZipPreservesStructureAndStripsCommonRoot(t *testing.T) {
	b, dir := newTestBatch(t, testLimits)
	data := buildZip(t,
		zipEntry{name: "myapp/"},
		zipEntry{name: "myapp/go.mod", body: "module example.com/myapp\n"},
		zipEntry{name: "myapp/cmd/server/main.go", body: "package main\n"},
		zipEntry{name: "myapp/internal/store/store.go", body: "package store\n"},
		zipEntry{name: "myapp/web/static/app.js", body: "console.log(1)\n"},
		zipEntry{name: "myapp/README.md", body: "# My app\n"},
		zipEntry{name: "__MACOSX/myapp/._main.go", body: "junk"},
	)
	if err := addZip(b, data); err != nil {
		t.Fatalf("AddZip: %v", err)
	}
	want := []string{"README.md", "cmd/server/main.go", "go.mod", "internal/store/store.go", "web/static/app.js"}
	if got := stagedPaths(b); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("staged = %v, want %v", got, want)
	}

	byPath := map[string]Staged{}
	for _, s := range b.Staged() {
		byPath[s.Path] = s
	}
	main := byPath["cmd/server/main.go"]
	if main.Language != "Go" || main.Kind != KindSource || main.Size != int64(len("package main\n")) || len(main.SHA256) != 64 {
		t.Errorf("main.go staged as %+v", main)
	}
	if byPath["go.mod"].Kind != KindManifest || byPath["README.md"].Kind != KindDocumentation {
		t.Errorf("kinds: go.mod=%s README=%s", byPath["go.mod"].Kind, byPath["README.md"].Kind)
	}
	content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(main.StoragePath)))
	if err != nil || string(content) != "package main\n" {
		t.Errorf("stored content = %q, %v", content, err)
	}
	if strings.Contains(main.StoragePath, "main.go") {
		t.Errorf("storage path %q is derived from the uploaded name", main.StoragePath)
	}
}

func TestAddZipKeepsMultipleTopLevelEntries(t *testing.T) {
	b, _ := newTestBatch(t, testLimits)
	data := buildZip(t,
		zipEntry{name: "src/main.go", body: "package main\n"},
		zipEntry{name: "go.mod", body: "module x\n"},
	)
	if err := addZip(b, data); err != nil {
		t.Fatal(err)
	}
	if got := stagedPaths(b); strings.Join(got, ",") != "go.mod,src/main.go" {
		t.Errorf("staged = %v", got)
	}
}

func TestAddZipSkipsUnsupportedContent(t *testing.T) {
	b, _ := newTestBatch(t, testLimits)
	elf := string(append([]byte{0x7f, 'E', 'L', 'F'}, make([]byte, 80)...))
	data := buildZip(t,
		zipEntry{name: "main.go", body: "package main\n"},
		zipEntry{name: "node_modules/left-pad/index.js", body: "module.exports = 1\n"},
		zipEntry{name: ".git/config", body: "[core]\n"},
		zipEntry{name: ".env", body: "SECRET=hunter2\n"},
		zipEntry{name: "keys/server.pem", body: "-----BEGIN PRIVATE KEY-----\n"},
		zipEntry{name: "logo.png", body: "\x89PNG\r\n\x1a\n"},
		zipEntry{name: "tool.exe", body: "MZ"},
		zipEntry{name: "build/run.sh", body: elf},
		zipEntry{name: "vendor.zip", body: "PK"},
		zipEntry{name: "data.txt", body: "a\x00b"},
		zipEntry{name: "big.go", body: strings.Repeat("// padding\n", 7000)},
		zipEntry{name: "secret.go", body: "package secret\n", header: func(h *zip.FileHeader) { h.Flags |= 0x1 }},
		zipEntry{name: ".DS_Store", body: "x"},
	)
	if err := addZip(b, data); err != nil {
		t.Fatalf("AddZip: %v", err)
	}
	if got := stagedPaths(b); strings.Join(got, ",") != "main.go" {
		t.Errorf("staged = %v, want only main.go", got)
	}
	want := map[string]string{
		"node_modules/left-pad/index.js": ReasonIgnoredDir,
		".git/config":                    ReasonIgnoredDir,
		".env":                           ReasonSecret,
		"keys/server.pem":                ReasonSecret,
		"logo.png":                       ReasonUnsupported,
		"tool.exe":                       ReasonExecutable,
		"build/run.sh":                   ReasonExecutable,
		"vendor.zip":                     ReasonArchive,
		"data.txt":                       ReasonBinary,
		"big.go":                         ReasonTooLarge,
		"secret.go":                      ReasonEncrypted,
		".DS_Store":                      ReasonSystemFile,
	}
	got := skippedReasons(b)
	for p, reason := range want {
		if got[p] != reason {
			t.Errorf("skipped[%q] = %q, want %q", p, got[p], reason)
		}
	}
	if _, total := b.Skipped(); total != len(want) {
		t.Errorf("skipped total = %d, want %d", total, len(want))
	}
}

func TestAddZipRejectsUnsafeArchives(t *testing.T) {
	cases := map[string][]zipEntry{
		"parent traversal":       {{name: "../../etc/cron.d/evil.sh", body: "x"}},
		"nested traversal":       {{name: "src/../../evil.go", body: "x"}},
		"backslash traversal":    {{name: `src\..\..\evil.go`, body: "x"}},
		"absolute path":          {{name: "/etc/passwd", body: "x"}},
		"windows drive":          {{name: `C:\Windows\evil.go`, body: "x"}},
		"control character":      {{name: "evil\x1b[31m.go", body: "x"}},
		"reserved device name":   {{name: "src/CON.go", body: "x"}},
		"symlink":                {{name: "link.go", body: "/etc/passwd", header: func(h *zip.FileHeader) { h.SetMode(fs.ModeSymlink | 0o777) }}},
		"traversal after a good": {{name: "ok.go", body: "package ok\n"}, {name: "../evil.go", body: "x"}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			b, _ := newTestBatch(t, testLimits)
			err := addZip(b, buildZip(t, entries...))
			if !errors.Is(err, ErrRejected) || !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("AddZip error = %v, want unsafe path rejection", err)
			}
			if strings.ContainsAny(err.Error(), "\x1b\x00") {
				t.Errorf("error message contains control characters: %q", err)
			}
			b.Abort()
		})
	}
}

func TestAddZipRejectsZipBomb(t *testing.T) {
	limits := testLimits
	limits.MaxFileBytes = 8 << 20
	limits.MaxTotalBytes = 16 << 20
	b, _ := newTestBatch(t, limits)
	data := buildZip(t, zipEntry{name: "bomb.txt", body: strings.Repeat("A", 4<<20)})
	if len(data) > 64<<10 {
		t.Fatalf("test archive unexpectedly large: %d bytes", len(data))
	}
	err := addZip(b, data)
	if !errors.Is(err, ErrArchiveBomb) {
		t.Fatalf("AddZip error = %v, want ErrArchiveBomb", err)
	}
	if len(b.Staged()) != 0 {
		t.Error("bomb entry was staged")
	}
}

func TestAddZipRejectsTotalSizeOverLimit(t *testing.T) {
	limits := testLimits
	limits.MaxTotalBytes = 100 << 10
	b, _ := newTestBatch(t, limits)
	var entries []zipEntry
	for i := range 5 {
		entries = append(entries, zipEntry{name: "f" + string(rune('a'+i)) + ".go", body: randomText(30 << 10)})
	}
	if err := addZip(b, buildZip(t, entries...)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("AddZip error = %v, want ErrTooLarge", err)
	}
}

func TestAddZipRejectsTooManyFiles(t *testing.T) {
	limits := testLimits
	limits.MaxFiles = 3
	b, _ := newTestBatch(t, limits)
	var entries []zipEntry
	for i := range 4 {
		entries = append(entries, zipEntry{name: "f" + string(rune('a'+i)) + ".go", body: "package f\n"})
	}
	if err := addZip(b, buildZip(t, entries...)); !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("AddZip error = %v, want ErrTooManyFiles", err)
	}
}

func TestAddZipRejectsTooManyEntries(t *testing.T) {
	limits := testLimits
	limits.MaxFiles = 1
	b, _ := newTestBatch(t, limits)
	entries := make([]zipEntry, minEntryLimit+1)
	for i := range entries {
		entries[i] = zipEntry{name: "img" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + ".png", body: ""}
	}
	if err := addZip(b, buildZip(t, entries...)); !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("AddZip error = %v, want ErrTooManyFiles", err)
	}
}

func TestAddZipRejectsCorruptArchives(t *testing.T) {
	for name, data := range map[string][]byte{
		"not a zip": []byte("this is not a zip file"),
		"truncated": buildZip(t, zipEntry{name: "main.go", body: "package main\n"})[:30],
		"empty":     {},
	} {
		b, _ := newTestBatch(t, testLimits)
		if err := addZip(b, data); !errors.Is(err, ErrBadArchive) {
			t.Errorf("%s: AddZip error = %v, want ErrBadArchive", name, err)
		}
	}
}

func TestAddDirectFiles(t *testing.T) {
	b, _ := newTestBatch(t, testLimits)
	for p, body := range map[string]string{
		"main.go":   "package main\n",
		"lib/a.py":  "print('hi')\n",
		"photo.jpg": "\xff\xd8\xff",
	} {
		if err := b.Add(p, strings.NewReader(body), -1); err != nil {
			t.Fatalf("Add(%q): %v", p, err)
		}
	}
	if err := b.Add("main.go", strings.NewReader("package dup\n"), -1); err != nil {
		t.Fatal(err)
	}
	if got := stagedPaths(b); strings.Join(got, ",") != "lib/a.py,main.go" {
		t.Errorf("staged = %v", got)
	}
	reasons := skippedReasons(b)
	if reasons["photo.jpg"] != ReasonUnsupported || reasons["main.go"] != ReasonDuplicate {
		t.Errorf("skipped = %v", reasons)
	}

	if err := b.Add("../escape.go", strings.NewReader("x"), -1); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("Add(../escape.go) error = %v, want ErrUnsafePath", err)
	}
}

func TestAbortRemovesBlobs(t *testing.T) {
	b, dir := newTestBatch(t, testLimits)
	if err := b.Add("main.go", strings.NewReader("package main\n"), -1); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(dir, filepath.FromSlash(b.Staged()[0].StoragePath))
	if _, err := os.Stat(blob); err != nil {
		t.Fatalf("blob not written: %v", err)
	}
	b.Abort()
	if _, err := os.Stat(blob); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("blob still exists after Abort: %v", err)
	}
}

func TestCommonRoot(t *testing.T) {
	cases := []struct {
		paths []string
		want  string
	}{
		{[]string{"app/a.go", "app/b/c.go"}, "app/"},
		{[]string{"app/a.go", "__MACOSX/app/._a.go"}, "app/"},
		{[]string{"app/a.go", "lib/b.go"}, ""},
		{[]string{"app/a.go", "README.md"}, ""},
		{[]string{"main.go"}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := commonRoot(c.paths); got != c.want {
			t.Errorf("commonRoot(%v) = %q, want %q", c.paths, got, c.want)
		}
	}
}

// randomText returns n bytes of poorly compressible ASCII text.
func randomText(n int) string {
	var sb strings.Builder
	x := uint32(2463534242)
	for sb.Len() < n {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		sb.WriteByte(byte('a' + x%26))
		if x%61 == 0 {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func TestBlobStoreRefusesForeignPaths(t *testing.T) {
	blobs := NewBlobStore(t.TempDir())
	for _, p := range []string{
		"../app.db", "uploads/1/../../app.db", "uploads/1/abc.blob", "/etc/passwd",
		"uploads/0/0123456789abcdef0123456789abcdef.blob",
	} {
		if _, err := blobs.Open(p); !errors.Is(err, errBadStoragePath) {
			t.Errorf("Open(%q) error = %v, want errBadStoragePath", p, err)
		}
		if err := blobs.Remove(p); !errors.Is(err, errBadStoragePath) {
			t.Errorf("Remove(%q) error = %v, want errBadStoragePath", p, err)
		}
	}
}

func TestBlobStoreReadTruncates(t *testing.T) {
	blobs := NewBlobStore(t.TempDir())
	sp, err := blobs.Write(3, []byte("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	data, truncated, err := blobs.Read(sp, 4)
	if err != nil || string(data) != "0123" || !truncated {
		t.Errorf("Read(4) = %q, %v, %v", data, truncated, err)
	}
	data, truncated, err = blobs.Read(sp, 10)
	if err != nil || string(data) != "0123456789" || truncated {
		t.Errorf("Read(10) = %q, %v, %v", data, truncated, err)
	}
	if err := blobs.RemoveProject(3); err != nil {
		t.Fatal(err)
	}
	if _, err := blobs.Open(sp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("blob survives RemoveProject: %v", err)
	}
}
