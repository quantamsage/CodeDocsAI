package docs

import (
	"archive/zip"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Slug turns a title or name into a lowercase, filesystem-safe name.
func Slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)), r == '.' && b.Len() > 0 && !dash:
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-.")
	if len(out) > 80 {
		out = strings.Trim(out[:80], "-.")
	}
	if out == "" {
		return "untitled"
	}
	return out
}

// DownloadName is the file name for downloading one document.
func DownloadName(projectName string, d Doc) string {
	return Slug(projectName) + "-" + Slug(d.Title) + ".md"
}

// safeSegments cleans a stored path for use inside an archive: every
// segment is reduced to safe characters, so no entry can escape the
// export root whatever the key contains.
func safeSegments(p string) string {
	var parts []string
	for _, seg := range strings.Split(strings.ReplaceAll(p, "\\", "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		var b strings.Builder
		for _, r := range seg {
			if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_') {
				b.WriteRune(r)
			} else {
				b.WriteByte('_')
			}
		}
		s := strings.TrimLeft(b.String(), ".")
		if s == "" {
			s = "_"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "/")
}

// ExportPath is a document's path inside the ZIP export, relative to the
// export root:
//
//	README.md                               index of all documents
//	sections/NN-<section>.md                project sections, in reading order
//	modules/<directory>.md                  one per module (root.md for ".")
//	files/<file path>.md                    one per documented file
func ExportPath(d Doc) string {
	switch d.Scope {
	case ScopeProject:
		n := len(Sections) + 1
		for i, s := range Sections {
			if s.Key == d.Key {
				n = i + 1
			}
		}
		return fmt.Sprintf("sections/%02d-%s.md", n, Slug(d.Key))
	case ScopeModule:
		p := safeSegments(d.Key)
		if p == "" {
			p = "root"
		}
		return "modules/" + p + ".md"
	default:
		p := safeSegments(d.Key)
		if p == "" {
			p = "untitled"
		}
		return "files/" + p + ".md"
	}
}

// WriteZip writes the documents as a ZIP archive with a top-level folder
// named after the project and a README.md index linking every document.
func WriteZip(w io.Writer, projectName string, docs []Doc, now time.Time) error {
	root := Slug(projectName) + "-docs"
	sorted := append([]Doc(nil), docs...)
	order := map[string]int{ScopeProject: 0, ScopeModule: 1, ScopeFile: 2}
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if order[a.Scope] != order[b.Scope] {
			return order[a.Scope] < order[b.Scope]
		}
		return ExportPath(a) < ExportPath(b)
	})

	zw := zip.NewWriter(w)
	used := map[string]bool{}
	var index strings.Builder
	fmt.Fprintf(&index, "# %s documentation\n\nGenerated documentation exported on %s.\n", escapeMarkdown(projectName), now.UTC().Format("2006-01-02 15:04 UTC"))
	heading := ""
	for _, d := range sorted {
		name := ExportPath(d)
		for i := 2; used[name]; i++ { // distinct keys can sanitize to the same path
			name = strings.TrimSuffix(ExportPath(d), ".md") + fmt.Sprintf("-%d.md", i)
		}
		used[name] = true
		if h := scopeHeading(d.Scope); h != heading {
			heading = h
			fmt.Fprintf(&index, "\n## %s\n\n", h)
		}
		fmt.Fprintf(&index, "- [%s](%s) (version %d)\n", escapeMarkdown(d.Title), linkPath(name), d.Version)
		if err := writeEntry(zw, path.Join(root, name), d.Markdown, d.CreatedAt); err != nil {
			return err
		}
	}
	if len(sorted) == 0 {
		index.WriteString("\nNo documentation has been generated yet.\n")
	}
	if err := writeEntry(zw, path.Join(root, "README.md"), index.String(), now); err != nil {
		return err
	}
	return zw.Close()
}

func writeEntry(zw *zip.Writer, name, content string, modified time.Time) error {
	f, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: modified.UTC()})
	if err != nil {
		return err
	}
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	_, err = io.WriteString(f, content)
	return err
}

func scopeHeading(scope string) string {
	switch scope {
	case ScopeProject:
		return "Project"
	case ScopeModule:
		return "Modules"
	}
	return "Files"
}

// linkPath escapes characters that would break a Markdown link target.
func linkPath(p string) string {
	r := strings.NewReplacer(" ", "%20", "(", "%28", ")", "%29", "<", "%3C", ">", "%3E")
	return r.Replace(p)
}

// escapeMarkdown neutralizes characters with meaning in link text.
func escapeMarkdown(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "[", `\[`, "]", `\]`, "*", `\*`, "_", `\_`, "`", "\\`", "<", `\<`, ">", `\>`)
	return r.Replace(s)
}
