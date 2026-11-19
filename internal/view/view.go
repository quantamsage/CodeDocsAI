// Package view renders HTML pages from templates.
//
// Template layout on disk:
//
//	layouts/*.html   shared layout, defines "base" which renders block "content"
//	partials/*.html  reusable fragments available to every page
//	pages/*.html     one file per page; each defines "title" and "content"
//
// Each page is parsed into its own template set (layout + partials + page) so
// pages can define the same block names without clobbering each other.
package view

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// Page is the data passed to every page template.
type Page struct {
	// Title is used in the <title> element; pages may override it.
	Title string
	// RequestID is shown on error pages to correlate with server logs.
	RequestID string
	// User is the signed-in user, or nil for anonymous visitors.
	User *User
	// CSRFToken must be included as the "csrf_token" field of every
	// state-changing form submitted by a signed-in user.
	CSRFToken string
	// Flash is a one-shot status message (e.g. "Project created.").
	Flash string
	// Data is page-specific data.
	Data any
}

// User is the signed-in user as seen by templates.
type User struct {
	ID          int64
	Email       string
	DisplayName string
}

// Renderer holds parsed page templates.
type Renderer struct {
	pages map[string]*template.Template
}

// New parses all templates from fsys.
func New(fsys fs.FS) (*Renderer, error) {
	base := template.New("").Funcs(template.FuncMap{
		"year": func() int { return time.Now().Year() },
		// date formats a timestamp for display; the zero time renders as "—".
		"date": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.UTC().Format("2 Jan 2006, 15:04 UTC")
		},
		// isodate formats a timestamp for a <time datetime="..."> attribute.
		"isodate": func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		// bytes formats a byte count with a binary unit.
		"bytes": formatBytes,
		// dict builds a map from key/value pairs, to pass several values to a
		// sub-template: {{template "x" (dict "a" 1 "b" .Name)}}.
		"dict": dict,
	})

	for _, pattern := range []string{"layouts/*.html", "partials/*.html"} {
		matches, err := fs.Glob(fsys, pattern)
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			if pattern == "layouts/*.html" {
				return nil, fmt.Errorf("no layout templates found")
			}
			continue
		}
		if base, err = base.ParseFS(fsys, pattern); err != nil {
			return nil, fmt.Errorf("parse %s: %w", pattern, err)
		}
	}

	pageFiles, err := fs.Glob(fsys, "pages/*.html")
	if err != nil {
		return nil, err
	}
	r := &Renderer{pages: make(map[string]*template.Template, len(pageFiles))}
	for _, file := range pageFiles {
		name := strings.TrimSuffix(path.Base(file), ".html")
		t, err := template.Must(base.Clone()).ParseFS(fsys, file)
		if err != nil {
			return nil, fmt.Errorf("parse page %s: %w", file, err)
		}
		if t.Lookup("base") == nil {
			return nil, fmt.Errorf("page %s: layout does not define \"base\"", file)
		}
		r.pages[name] = t
	}
	return r, nil
}

// Has reports whether a page with the given name exists.
func (r *Renderer) Has(name string) bool {
	_, ok := r.pages[name]
	return ok
}

// Render executes the named page into w with the given status code. The page is
// rendered into a buffer first so a template error never produces a partial
// response; in that case nothing is written and the error is returned.
func (r *Renderer) Render(w http.ResponseWriter, status int, name string, data Page) error {
	t, ok := r.pages[name]
	if !ok {
		return fmt.Errorf("view: unknown page %q", name)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", data); err != nil {
		return fmt.Errorf("view: render %q: %w", name, err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, err := buf.WriteTo(w)
	return err
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func dict(pairs ...any) (map[string]any, error) {
	if len(pairs)%2 != 0 {
		return nil, fmt.Errorf("dict: odd number of arguments")
	}
	m := make(map[string]any, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		key, ok := pairs[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %v is not a string", pairs[i])
		}
		m[key] = pairs[i+1]
	}
	return m, nil
}
