package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/quantamsage/ai-code-documentation-tool/internal/analysis"
	"github.com/quantamsage/ai-code-documentation-tool/internal/docs"
	"github.com/quantamsage/ai-code-documentation-tool/internal/files"
	"github.com/quantamsage/ai-code-documentation-tool/internal/jobs"
	"github.com/quantamsage/ai-code-documentation-tool/internal/projects"
)

// Display limits for the file viewer.
const (
	maxViewBytes      = 512 << 10
	maxFieldBytes     = 1 << 10
	maxSkippedInFlash = 5
	openTreeMaxFiles  = 300
)

// uploadLimits converts the configured limits for the files package.
func (s *Server) uploadLimits() files.Limits {
	u := s.cfg.Upload
	return files.Limits{
		MaxFileBytes:        u.MaxFileBytes,
		MaxFiles:            u.MaxFiles,
		MaxTotalBytes:       u.MaxTotalBytes,
		MaxCompressionRatio: u.MaxCompressionRatio,
	}
}

// acceptUpload prepares a route for a streamed multipart upload: it caps the
// body at the configured request size, extends the connection deadlines to
// the upload timeout and lets requireAuth read the CSRF token from the
// multipart body. It must wrap requireAuth.
func (s *Server) acceptUpload(h handlerFunc) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if r.ContentLength > s.cfg.Upload.MaxRequestBytes {
			return errUploadTooLarge(s.cfg.Upload.MaxRequestBytes, nil)
		}
		rc := http.NewResponseController(w)
		deadline := time.Now().Add(s.cfg.Upload.Timeout)
		// Not every ResponseWriter supports deadlines (e.g. in tests).
		rc.SetReadDeadline(deadline)  //nolint:errcheck
		rc.SetWriteDeadline(deadline) //nolint:errcheck

		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Upload.MaxRequestBytes)
		ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Upload.Timeout)
		defer cancel()
		ctx = context.WithValue(ctx, ctxKeyMultipartAllowed, true)
		return h(w, r.WithContext(ctx))
	}
}

func errUploadTooLarge(limit int64, err error) error {
	return &HTTPError{
		Status:  http.StatusRequestEntityTooLarge,
		Message: "The upload is larger than the " + formatSize(limit) + " limit. Upload a smaller ZIP archive or fewer files at a time.",
		Err:     err,
	}
}

type uploadView struct {
	Project    projects.Project
	Error      string
	Directory  string
	Replace    bool
	Limits     files.Limits
	MaxRequest int64
}

func (s *Server) uploadView(p projects.Project) uploadView {
	return uploadView{
		Project:    p,
		Limits:     s.uploadLimits(),
		MaxRequest: s.cfg.Upload.MaxRequestBytes,
	}
}

func (s *Server) uploadForm(w http.ResponseWriter, r *http.Request) error {
	p, err := s.projectFromPath(r)
	if err != nil {
		return err
	}
	return s.views.Render(w, http.StatusOK, "project_upload", s.page(r, "Upload to "+p.Name, s.uploadView(p)))
}

// uploadFiles receives a multipart upload of individual files, files picked
// from a folder (with their relative paths) and/or ZIP archives, validates
// and stores them, and records them in the project.
func (s *Server) uploadFiles(w http.ResponseWriter, r *http.Request) error {
	p, err := s.projectFromPath(r)
	if err != nil {
		return err
	}
	if active, err := s.jobs.HasActive(r.Context(), p.ID); err != nil {
		return err
	} else if active {
		return &HTTPError{Status: http.StatusConflict, Message: "The project is being analyzed or documented. Wait for the job to finish (or cancel it) before uploading."}
	}
	mr, _ := r.Context().Value(ctxKeyMultipart).(*multipart.Reader)
	if mr == nil {
		if mr, err = r.MultipartReader(); err != nil {
			return &HTTPError{Status: http.StatusBadRequest, Message: "The upload could not be read. Please choose files and try again.", Err: err}
		}
	}

	batch := files.NewBatch(s.files.Blobs(), p.ID, s.uploadLimits())
	committed := false
	defer func() {
		if !committed {
			batch.Abort()
		}
	}()

	form := s.uploadView(p)
	var replace bool
	var directory string
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return s.uploadReadError(err)
		}
		switch part.FormName() {
		case "replace":
			v, err := readField(part)
			if err != nil {
				return s.uploadReadError(err)
			}
			replace = v == "1"
		case "directory":
			if directory, err = readField(part); err != nil {
				return s.uploadReadError(err)
			}
		case "files", "folder":
			name := partFileName(part)
			if name == "" {
				break // an empty file input still sends a part
			}
			if part.FormName() == "folder" {
				name = stripFirstSegment(name)
			}
			if err := s.addUploadedFile(batch, name, part); err != nil {
				part.Close()
				var rej *files.RejectedError
				if errors.As(err, &rej) {
					form.Error, form.Directory, form.Replace = rej.Message, directory, replace
					return s.views.Render(w, http.StatusUnprocessableEntity, "project_upload", s.page(r, "Upload to "+p.Name, form))
				}
				return s.uploadReadError(err)
			}
		}
		part.Close()
	}
	form.Directory, form.Replace = directory, replace

	opts := files.CommitOptions{Replace: replace}
	if directory = strings.TrimSpace(directory); directory != "" {
		if opts.Dir, err = files.CleanPath(directory); err != nil {
			form.Error = "The target folder is not a valid path. Use a relative path such as src/utils, without “..”."
			return s.views.Render(w, http.StatusUnprocessableEntity, "project_upload", s.page(r, "Upload to "+p.Name, form))
		}
	}

	res, err := s.files.Commit(r.Context(), p.UserID, p.ID, batch, opts)
	committed = true // Commit cleans up after itself on failure
	var rej *files.RejectedError
	switch {
	case errors.As(err, &rej):
		form.Error = rej.Message
		if skipped, total := batch.Skipped(); total > 0 && errors.Is(err, files.ErrNoFiles) {
			form.Error += " " + skippedSummary(skipped, total)
		}
		return s.views.Render(w, http.StatusUnprocessableEntity, "project_upload", s.page(r, "Upload to "+p.Name, form))
	case errors.Is(err, files.ErrNotFound):
		return errNotFound()
	case err != nil:
		return err
	}

	skipped, skippedTotal := batch.Skipped()
	loggerFrom(r.Context(), s.log).Info("files uploaded",
		slog.Int64("project_id", p.ID), slog.Int("added", res.Added), slog.Int("updated", res.Updated),
		slog.Int("removed", res.Removed), slog.Int("skipped", skippedTotal))
	s.flash(r, uploadSummary(res, skipped, skippedTotal))
	http.Redirect(w, r, "/projects/"+strconv.FormatInt(p.ID, 10), http.StatusSeeOther)
	return nil
}

// addUploadedFile adds one uploaded file part to the batch. ZIP archives are
// spooled to a temporary file (the central directory is at the end) and
// extracted; other archive formats are rejected.
func (s *Server) addUploadedFile(batch *files.Batch, name string, part io.Reader) error {
	ext := strings.ToLower(filepath.Ext(name))
	switch {
	case ext == ".zip":
		return s.addZipPart(batch, part)
	case files.IsArchive(name):
		return &files.RejectedError{
			Message: fmt.Sprintf("%q is a %s archive. Only .zip archives are supported.", filepath.Base(strings.ReplaceAll(name, `\`, "/")), ext),
			Err:     files.ErrBadArchive,
		}
	}
	return batch.Add(name, part, -1)
}

func (s *Server) addZipPart(batch *files.Batch, part io.Reader) error {
	tmpDir := filepath.Join(s.cfg.DataDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(tmpDir, "upload-*.zip")
	if err != nil {
		return err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	size, err := io.Copy(tmp, part)
	if err != nil {
		return err
	}
	return batch.AddZip(tmp, size)
}

func (s *Server) uploadReadError(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return errUploadTooLarge(s.cfg.Upload.MaxRequestBytes, err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return &HTTPError{Status: http.StatusRequestTimeout, Message: "The upload took too long. Try a smaller upload or a faster connection.", Err: err}
	}
	return &HTTPError{Status: http.StatusBadRequest, Message: "The upload could not be read. Please try again.", Err: err}
}

// readField reads a small text form field.
func readField(part *multipart.Part) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(part, maxFieldBytes+1))
	if err != nil {
		return "", err
	}
	if len(raw) > maxFieldBytes {
		return "", &HTTPError{Status: http.StatusBadRequest, Message: "A form field in the upload is too long."}
	}
	return string(raw), nil
}

// partFileName returns the file name as sent by the browser, including any
// relative directory (folder uploads send "dir/sub/file.go").
// multipart.Part.FileName keeps only the base name, so the header is parsed
// directly. The result is validated by files.CleanPath before use.
func partFileName(part *multipart.Part) string {
	_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err != nil {
		return part.FileName()
	}
	return params["filename"]
}

// stripFirstSegment removes the selected folder's own name from a folder
// upload path ("myproject/src/main.go" → "src/main.go").
func stripFirstSegment(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	if _, rest, ok := strings.Cut(name, "/"); ok && rest != "" {
		return rest
	}
	return name
}

func uploadSummary(res files.CommitResult, skipped []files.Skipped, skippedTotal int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Uploaded %s", plural(res.Added+res.Updated, "file"))
	if res.Updated > 0 {
		fmt.Fprintf(&b, " (%d new, %d replaced)", res.Added, res.Updated)
	}
	if res.Removed > 0 {
		fmt.Fprintf(&b, " after removing %s", plural(res.Removed, "existing file"))
	}
	b.WriteString(". The project now has " + plural(res.FileCount, "file") + ".")
	if skippedTotal > 0 {
		b.WriteString(" " + skippedSummary(skipped, skippedTotal))
	}
	return b.String()
}

func skippedSummary(skipped []files.Skipped, total int) string {
	parts := make([]string, 0, maxSkippedInFlash)
	for i, sk := range skipped {
		if i == maxSkippedInFlash {
			break
		}
		parts = append(parts, sk.Path+" ("+sk.Reason+")")
	}
	msg := "Skipped " + plural(total, "file") + ": " + strings.Join(parts, "; ")
	if total > len(parts) {
		msg += fmt.Sprintf("; and %d more", total-len(parts))
	}
	return msg + "."
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// formatSize formats a byte count with a binary unit.
func formatSize(n int64) string {
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

// --- Project explorer ---

// treeView is a directory tree prepared for templates, with link URLs
// computed and directories marked open.
type treeView struct {
	Name      string
	Path      string
	Open      bool
	FileCount int
	Size      int64
	Dirs      []treeView
	Files     []treeFile
}

type treeFile struct {
	Name     string
	Path     string
	URL      string
	Language string
	Kind     string
	Size     int64
	Current  bool
}

// fileURL returns the explorer URL of a file, escaping each path segment.
func fileURL(projectID int64, p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/files/" + strings.Join(segs, "/")
}

// buildTreeView converts a files tree. Directories on the path to current
// are open; small projects are fully expanded.
func buildTreeView(projectID int64, d *files.TreeDir, current string, expandAll bool) treeView {
	v := treeView{
		Name: d.Name, Path: d.Path, FileCount: d.FileCount, Size: d.Size,
		Open: expandAll || d.Path == "" || strings.HasPrefix(current, d.Path+"/"),
	}
	for _, c := range d.Dirs {
		v.Dirs = append(v.Dirs, buildTreeView(projectID, c, current, expandAll))
	}
	for _, f := range d.Files {
		v.Files = append(v.Files, treeFile{
			Name: f.Name(), Path: f.Path, URL: fileURL(projectID, f.Path),
			Language: f.Language, Kind: f.Kind, Size: f.Size, Current: f.Path == current,
		})
	}
	return v
}

type projectView struct {
	projects.Project
	Tree  treeView
	Kinds files.KindCounts
	// Analysis section.
	AIEnabled   bool
	Job         *jobJSON
	HasAnalysis bool
	Purpose     string
	AnalyzedAt  time.Time
}

func (s *Server) showProject(w http.ResponseWriter, r *http.Request) error {
	p, err := s.projectFromPath(r)
	if err != nil {
		return err
	}
	list, err := s.files.List(r.Context(), p.UserID, p.ID)
	if err != nil {
		return err
	}
	v := projectView{
		Project:   p,
		Tree:      buildTreeView(p.ID, files.BuildTree(list), "", len(list) <= openTreeMaxFiles),
		Kinds:     files.CountKinds(list),
		AIEnabled: s.ai != nil,
	}
	if job, err := s.jobs.Latest(r.Context(), p.UserID, p.ID); err == nil {
		j := toJobJSON(job)
		v.Job = &j
	} else if !errors.Is(err, jobs.ErrNotFound) {
		return err
	}
	if pa, meta, err := s.analysis.Project(r.Context(), p.UserID, p.ID); err == nil {
		v.HasAnalysis, v.Purpose, v.AnalyzedAt = true, pa.Purpose, meta.CreatedAt
	} else if !errors.Is(err, analysis.ErrNotFound) {
		return err
	}
	return s.views.Render(w, http.StatusOK, "project", s.page(r, p.Name, v))
}

type crumb struct {
	Name string
	URL  string // empty for the current item
}

type fileView struct {
	Project   projects.Project
	File      files.File
	Crumbs    []crumb
	Lines     []string
	Truncated bool
	ViewLimit int64
	Tree      treeView
	Analysis  *analysis.FileAnalysis
	DocURL    string // the file's generated documentation, if any
	AIEnabled bool
}

// showFile renders one stored file with line numbers. Content is treated as
// untrusted text: it is HTML-escaped by the template and never executed.
func (s *Server) showFile(w http.ResponseWriter, r *http.Request) error {
	p, err := s.projectFromPath(r)
	if err != nil {
		return err
	}
	raw := r.PathValue("path")
	if raw == "" {
		http.Redirect(w, r, "/projects/"+strconv.FormatInt(p.ID, 10)+"#files", http.StatusSeeOther)
		return nil
	}
	filePath, err := files.CleanPath(raw)
	if err != nil {
		return errNotFound()
	}
	f, err := s.files.Get(r.Context(), p.UserID, p.ID, filePath)
	if errors.Is(err, files.ErrNotFound) {
		return errNotFound()
	}
	if err != nil {
		return err
	}
	content, truncated, err := s.files.Content(f, maxViewBytes)
	if err != nil {
		return fmt.Errorf("read file %d: %w", f.ID, err)
	}
	list, err := s.files.List(r.Context(), p.UserID, p.ID)
	if err != nil {
		return err
	}

	v := fileView{
		Project:   p,
		File:      f,
		Lines:     splitLines(content, truncated),
		Truncated: truncated,
		ViewLimit: maxViewBytes,
		Tree:      buildTreeView(p.ID, files.BuildTree(list), f.Path, false),
	}
	if stored, err := s.analysis.File(r.Context(), p.UserID, p.ID, f.ID); err == nil {
		v.Analysis = stored.Analysis
	} else if !errors.Is(err, analysis.ErrNotFound) {
		return err
	}
	v.AIEnabled = s.ai != nil
	if _, err := s.docs.Get(r.Context(), p.UserID, p.ID, docs.ScopeFile, f.Path, 0); err == nil {
		v.DocURL = docURL(p.ID, docs.ScopeFile, f.Path, 0)
	} else if !errors.Is(err, docs.ErrNotFound) {
		return err
	}
	v.Crumbs = append(v.Crumbs,
		crumb{Name: "Dashboard", URL: "/dashboard"},
		crumb{Name: p.Name, URL: "/projects/" + strconv.FormatInt(p.ID, 10) + "#files"})
	segs := strings.Split(f.Path, "/")
	for _, seg := range segs[:len(segs)-1] {
		v.Crumbs = append(v.Crumbs, crumb{Name: seg})
	}
	v.Crumbs = append(v.Crumbs, crumb{Name: segs[len(segs)-1]})
	return s.views.Render(w, http.StatusOK, "project_file", s.page(r, f.Path+" · "+p.Name, v))
}

// splitLines splits text into lines for display. A cut-off final line is
// dropped if the content was truncated mid-line, and a trailing newline does
// not produce an empty last line.
func splitLines(content []byte, truncated bool) []string {
	text := strings.TrimPrefix(string(content), "\xef\xbb\xbf") // UTF-8 BOM
	text = strings.ToValidUTF8(text, string(utf8.RuneError))
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	} else if truncated && len(lines) > 1 {
		lines = lines[:len(lines)-1]
	}
	return lines
}
