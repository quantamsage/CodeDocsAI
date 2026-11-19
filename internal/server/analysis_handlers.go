package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/quantamsage/ai-code-documentation-tool/internal/analysis"
	"github.com/quantamsage/ai-code-documentation-tool/internal/docs"
	"github.com/quantamsage/ai-code-documentation-tool/internal/jobs"
	"github.com/quantamsage/ai-code-documentation-tool/internal/projects"
)

func projectURL(id int64) string { return "/projects/" + strconv.FormatInt(id, 10) }

// analyzeProject starts a background analysis of the project.
func (s *Server) analyzeProject(w http.ResponseWriter, r *http.Request) error {
	p, err := s.projectFromPath(r)
	if err != nil {
		return err
	}
	if s.ai == nil {
		return &HTTPError{Status: http.StatusServiceUnavailable, Message: "AI analysis is not configured on this server. Set ANTHROPIC_API_KEY and restart it."}
	}
	if p.FileCount == 0 {
		return &HTTPError{Status: http.StatusConflict, Message: "Upload the project's files before analyzing it."}
	}
	if active, err := s.jobs.HasActive(r.Context(), p.ID); err != nil {
		return err
	} else if active {
		return &HTTPError{Status: http.StatusConflict, Message: "This project already has a job in progress. Wait for it to finish or cancel it."}
	}
	job, err := s.jobs.Enqueue(r.Context(), p.UserID, p.ID, analysis.JobType, nil, 3)
	if errors.Is(err, jobs.ErrActive) {
		return &HTTPError{Status: http.StatusConflict, Message: "An analysis is already running for this project."}
	}
	if err != nil {
		return err
	}
	loggerFrom(r.Context(), s.log).Info("analysis queued", slog.Int64("project_id", p.ID), slog.Int64("job_id", job.ID))
	s.flash(r, "Analysis started. It runs in the background, so you can leave this page and come back later.")
	http.Redirect(w, r, projectURL(p.ID)+"#analysis", http.StatusSeeOther)
	return nil
}

// jobJSON is the job status polled by the project page.
type jobJSON struct {
	ID        int64     `json:"id"`
	Type      string    `json:"type"`
	Label     string    `json:"label"`
	Status    string    `json:"status"`
	Active    bool      `json:"active"`
	Percent   int       `json:"percent"`
	Current   int       `json:"current"`
	Total     int       `json:"total"`
	Message   string    `json:"message"`
	Error     string    `json:"error"`
	Attempts  int       `json:"attempts"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toJobJSON(j jobs.Job) jobJSON {
	return jobJSON{
		ID: j.ID, Type: j.Type, Label: jobLabel(j.Type), Status: j.Status, Active: j.Active(), Percent: j.Percent(), Current: j.ProgressCurrent,
		Total: j.ProgressTotal, Message: j.ProgressMessage, Error: j.Error, Attempts: j.Attempts, UpdatedAt: j.UpdatedAt,
	}
}

func jobLabel(jobType string) string {
	switch jobType {
	case analysis.JobType:
		return "Analysis"
	case docs.JobType:
		return "Documentation"
	}
	return jobType
}

// latestJob returns the project's most recent job as JSON.
func (s *Server) latestJob(w http.ResponseWriter, r *http.Request) error {
	p, err := s.projectFromPath(r)
	if err != nil {
		return err
	}
	job, err := s.jobs.Latest(r.Context(), p.UserID, p.ID)
	w.Header().Set("Content-Type", "application/json")
	if errors.Is(err, jobs.ErrNotFound) {
		w.WriteHeader(http.StatusOK)
		_, err = w.Write([]byte(`{"job":null}`))
		return err
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(map[string]any{"job": toJobJSON(job)})
}

// cancelJob cancels a queued or running job.
func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) error {
	p, err := s.projectFromPath(r)
	if err != nil {
		return err
	}
	jobID, err := strconv.ParseInt(r.PathValue("jobID"), 10, 64)
	if err != nil || jobID <= 0 {
		return errNotFound()
	}
	job, err := s.jobs.Get(r.Context(), p.UserID, jobID)
	if errors.Is(err, jobs.ErrNotFound) || (err == nil && job.ProjectID != p.ID) {
		return errNotFound()
	}
	if err != nil {
		return err
	}
	switch err := s.jobs.Cancel(r.Context(), p.UserID, jobID); {
	case errors.Is(err, jobs.ErrNotCancelable):
		s.flash(r, "That job had already finished.")
	case err != nil:
		return err
	default:
		s.flash(r, "The job was canceled.")
	}
	http.Redirect(w, r, projectURL(p.ID)+"#analysis", http.StatusSeeOther)
	return nil
}

type analysisView struct {
	Project  projects.Project
	Analysis analysis.ProjectAnalysis
	Meta     analysis.Meta
	Files    []analysisFile
}

type analysisFile struct {
	Path     string
	URL      string
	Language string
	Summary  string
	Missing  bool
	Parts    int
}

// showAnalysis renders the project's latest analysis.
func (s *Server) showAnalysis(w http.ResponseWriter, r *http.Request) error {
	p, err := s.projectFromPath(r)
	if err != nil {
		return err
	}
	pa, meta, err := s.analysis.Project(r.Context(), p.UserID, p.ID)
	if errors.Is(err, analysis.ErrNotFound) {
		return &HTTPError{Status: http.StatusNotFound, Message: "This project has not been analyzed yet."}
	}
	if err != nil {
		return err
	}
	stored, err := s.analysis.Files(r.Context(), p.UserID, p.ID)
	if err != nil {
		return err
	}
	v := analysisView{Project: p, Analysis: pa, Meta: meta}
	for _, f := range stored {
		af := analysisFile{Path: f.Path, URL: fileURL(p.ID, f.Path), Language: f.Language}
		if f.Analysis != nil {
			af.Summary, af.Missing, af.Parts = f.Analysis.Summary, f.Analysis.Missing, f.Analysis.Parts
		}
		v.Files = append(v.Files, af)
	}
	return s.views.Render(w, http.StatusOK, "analysis", s.page(r, "Analysis · "+p.Name, v))
}
