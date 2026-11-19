package server

import (
	"bytes"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/quantamsage/ai-code-documentation-tool/internal/analysis"
	"github.com/quantamsage/ai-code-documentation-tool/internal/docs"
	"github.com/quantamsage/ai-code-documentation-tool/internal/jobs"
	"github.com/quantamsage/ai-code-documentation-tool/internal/markdown"
	"github.com/quantamsage/ai-code-documentation-tool/internal/projects"
)

// docURL is the viewer URL of a document (optionally a specific version).
func docURL(projectID int64, scope, key string, version int) string {
	q := url.Values{"key": {key}}
	if version > 0 {
		q.Set("version", strconv.Itoa(version))
	}
	return projectURL(projectID) + "/docs/" + url.PathEscape(scope) + "?" + q.Encode()
}

// docDownloadURL is the Markdown download URL of a document version.
func docDownloadURL(projectID int64, scope, key string, version int) string {
	q := url.Values{"key": {key}}
	if version > 0 {
		q.Set("version", strconv.Itoa(version))
	}
	return projectURL(projectID) + "/docs/" + url.PathEscape(scope) + "/download?" + q.Encode()
}

// generateDocs queues documentation generation: everything, one section,
// one module or one file.
func (s *Server) generateDocs(w http.ResponseWriter, r *http.Request) error {
	p, err := s.projectFromPath(r)
	if err != nil {
		return err
	}
	if err := parseForm(w, r); err != nil {
		return err
	}
	if s.ai == nil {
		return &HTTPError{Status: http.StatusServiceUnavailable, Message: "Documentation generation is not configured on this server. Set ANTHROPIC_API_KEY and restart it."}
	}
	req := docs.Request{Scope: r.PostForm.Get("scope"), Key: r.PostForm.Get("key")}
	if req.Scope == "" {
		req.Scope = "all"
	}
	if req.Scope != "all" && req.Scope != docs.ScopeProject && req.Scope != docs.ScopeModule && req.Scope != docs.ScopeFile {
		return &HTTPError{Status: http.StatusBadRequest, Message: "Unknown documentation scope."}
	}
	pa, _, err := s.analysis.Project(r.Context(), p.UserID, p.ID)
	if errors.Is(err, analysis.ErrNotFound) {
		return &HTTPError{Status: http.StatusConflict, Message: "Analyze the project before generating documentation."}
	}
	if err != nil {
		return err
	}
	files, err := s.analysis.Files(r.Context(), p.UserID, p.ID)
	if err != nil {
		return err
	}
	if _, err := docs.Plan(req, pa, files); err != nil {
		if outcome, ok := docs.Classify(err); ok {
			return &HTTPError{Status: http.StatusBadRequest, Message: outcome.Message, Err: err}
		}
		return err
	}
	if active, err := s.jobs.HasActive(r.Context(), p.ID); err != nil {
		return err
	} else if active {
		return &HTTPError{Status: http.StatusConflict, Message: "This project already has a job in progress. Wait for it to finish or cancel it."}
	}
	job, err := s.jobs.Enqueue(r.Context(), p.UserID, p.ID, docs.JobType, req, 3)
	if errors.Is(err, jobs.ErrActive) {
		return &HTTPError{Status: http.StatusConflict, Message: "Documentation is already being generated for this project."}
	}
	if err != nil {
		return err
	}
	loggerFrom(r.Context(), s.log).Info("documentation queued", slog.Int64("project_id", p.ID), slog.Int64("job_id", job.ID), slog.String("scope", req.Scope))
	s.flash(r, "Documentation generation started. It runs in the background.")
	target := projectURL(p.ID) + "#analysis"
	if back := r.PostForm.Get("return"); back != "" {
		target = safeNext(back, target)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
	return nil
}

type docEntry struct {
	Scope       string
	Key         string
	Title       string
	Version     int
	CreatedAt   time.Time
	URL         string
	DownloadURL string
}

type docsIndexView struct {
	Project     projects.Project
	Sections    []docEntry
	Modules     []docEntry
	Files       []docEntry
	HasAnalysis bool
	AIEnabled   bool
	Job         *jobJSON
	Missing     []docs.Section // sections not generated yet that apply
	CanGenerate bool
}

// docsIndex lists the project's current documents.
func (s *Server) docsIndex(w http.ResponseWriter, r *http.Request) error {
	p, err := s.projectFromPath(r)
	if err != nil {
		return err
	}
	current, err := s.docs.Current(r.Context(), p.UserID, p.ID)
	if err != nil {
		return err
	}
	v := docsIndexView{Project: p, AIEnabled: s.ai != nil}
	for _, d := range current {
		entry := docEntry{Scope: d.Scope, Key: d.Key, Title: d.Title, Version: d.Version, CreatedAt: d.CreatedAt,
			URL: docURL(p.ID, d.Scope, d.Key, 0), DownloadURL: docDownloadURL(p.ID, d.Scope, d.Key, 0)}
		switch d.Scope {
		case docs.ScopeProject:
			v.Sections = append(v.Sections, entry)
		case docs.ScopeModule:
			v.Modules = append(v.Modules, entry)
		default:
			v.Files = append(v.Files, entry)
		}
	}
	if pa, _, err := s.analysis.Project(r.Context(), p.UserID, p.ID); err == nil {
		v.HasAnalysis = true
		have := map[string]bool{}
		for _, e := range v.Sections {
			have[e.Key] = true
		}
		for _, sec := range docs.Sections {
			if !have[sec.Key] && ((sec.Key != "api" && sec.Key != "database") || (sec.Key == "api" && pa.HasAPI()) || (sec.Key == "database" && pa.HasDatabase())) {
				v.Missing = append(v.Missing, sec)
			}
		}
	} else if !errors.Is(err, analysis.ErrNotFound) {
		return err
	}
	if job, err := s.jobs.Latest(r.Context(), p.UserID, p.ID); err == nil {
		j := toJobJSON(job)
		v.Job = &j
	}
	v.CanGenerate = v.HasAnalysis && v.AIEnabled && (v.Job == nil || !v.Job.Active)
	return s.views.Render(w, http.StatusOK, "docs", s.page(r, "Documentation · "+p.Name, v))
}

type docView struct {
	Project     projects.Project
	Doc         docs.Doc
	Versions    []docEntry
	Current     bool
	Nav         []docEntry
	Prev, Next  *docEntry
	Content     any // sanitized HTML from markdown.Render
	TOC         []markdown.Heading
	DownloadURL string
}

// docFromRequest loads the document version named by the URL: the scope
// path value and the key and optional version query parameters.
func (s *Server) docFromRequest(r *http.Request) (projects.Project, docs.Doc, error) {
	p, err := s.projectFromPath(r)
	if err != nil {
		return p, docs.Doc{}, err
	}
	version := 0
	if raw := r.URL.Query().Get("version"); raw != "" {
		if version, err = strconv.Atoi(raw); err != nil || version < 1 {
			return p, docs.Doc{}, errNotFound()
		}
	}
	d, err := s.docs.Get(r.Context(), p.UserID, p.ID, r.PathValue("scope"), r.URL.Query().Get("key"), version)
	if errors.Is(err, docs.ErrNotFound) {
		return p, d, errNotFound()
	}
	return p, d, err
}

// showDoc renders one document version.
func (s *Server) showDoc(w http.ResponseWriter, r *http.Request) error {
	p, d, err := s.docFromRequest(r)
	if err != nil {
		return err
	}
	scope, key := d.Scope, d.Key
	history, err := s.docs.Versions(r.Context(), p.UserID, p.ID, scope, key)
	if err != nil {
		return err
	}
	current, err := s.docs.Current(r.Context(), p.UserID, p.ID)
	if err != nil {
		return err
	}
	rendered, err := markdown.Render(d.Markdown)
	if err != nil {
		return err
	}
	v := docView{Project: p, Doc: d, Current: len(history) > 0 && history[0].Version == d.Version,
		Content: rendered.HTML, TOC: rendered.TOC, DownloadURL: docDownloadURL(p.ID, scope, key, d.Version)}
	for _, h := range history {
		v.Versions = append(v.Versions, docEntry{Version: h.Version, CreatedAt: h.CreatedAt, URL: docURL(p.ID, scope, key, h.Version)})
	}
	for i, c := range current {
		v.Nav = append(v.Nav, docEntry{Scope: c.Scope, Key: c.Key, Title: c.Title, URL: docURL(p.ID, c.Scope, c.Key, 0)})
		if c.Scope == scope && c.Key == key {
			if i > 0 {
				prev := current[i-1]
				v.Prev = &docEntry{Title: prev.Title, URL: docURL(p.ID, prev.Scope, prev.Key, 0)}
			}
			if i+1 < len(current) {
				next := current[i+1]
				v.Next = &docEntry{Title: next.Title, URL: docURL(p.ID, next.Scope, next.Key, 0)}
			}
		}
	}
	return s.views.Render(w, http.StatusOK, "doc", s.page(r, d.Title+" · "+p.Name, v))
}

// attachment sets download headers. The file name is passed through
// mime.FormatMediaType, which quotes or RFC 2231-encodes it as needed.
func attachment(w http.ResponseWriter, contentType, filename string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	h.Set("X-Content-Type-Options", "nosniff")
}

// downloadDoc sends one document version as a Markdown file.
func (s *Server) downloadDoc(w http.ResponseWriter, r *http.Request) error {
	p, d, err := s.docFromRequest(r)
	if err != nil {
		return err
	}
	body := d.Markdown
	if len(body) > 0 && body[len(body)-1] != '\n' {
		body += "\n"
	}
	attachment(w, "text/markdown; charset=utf-8", docs.DownloadName(p.Name, d))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, err = w.Write([]byte(body))
	return err
}

// exportDocs sends the current version of every document as a ZIP archive.
func (s *Server) exportDocs(w http.ResponseWriter, r *http.Request) error {
	p, err := s.projectFromPath(r)
	if err != nil {
		return err
	}
	current, err := s.docs.Current(r.Context(), p.UserID, p.ID)
	if err != nil {
		return err
	}
	if len(current) == 0 {
		return &HTTPError{Status: http.StatusNotFound, Message: "This project has no documentation to export yet."}
	}
	// Build in memory so a failure can still produce an error page; the
	// archive is bounded by the documents' capped size.
	var buf bytes.Buffer
	if err := docs.WriteZip(&buf, p.Name, current, s.now()); err != nil {
		return err
	}
	attachment(w, "application/zip", docs.Slug(p.Name)+"-docs.zip")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	_, err = w.Write(buf.Bytes())
	return err
}

// now is the server clock (fake in tests).
func (s *Server) now() time.Time {
	if s.authOpts.Now != nil {
		return s.authOpts.Now()
	}
	return time.Now()
}
