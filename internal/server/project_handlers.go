package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/quantamsage/ai-code-documentation-tool/internal/projects"
)

// projectFromPath loads the {id} project for the signed-in user. Projects
// owned by other users produce the same 404 as missing ones, so IDs cannot
// be probed.
func (s *Server) projectFromPath(r *http.Request) (projects.Project, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return projects.Project{}, errNotFound()
	}
	p, err := s.projects.Get(r.Context(), sessionFrom(r.Context()).user.ID, id)
	if errors.Is(err, projects.ErrNotFound) {
		return projects.Project{}, errNotFound()
	}
	return p, err
}

type dashboardView struct {
	Projects     []projects.Project
	Documented   int
	TotalFiles   int
	NeedsUploads int
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) error {
	list, err := s.projects.List(r.Context(), sessionFrom(r.Context()).user.ID)
	if err != nil {
		return err
	}
	d := dashboardView{Projects: list}
	for _, p := range list {
		d.TotalFiles += p.FileCount
		switch p.Status {
		case projects.StatusDocumented:
			d.Documented++
		case projects.StatusEmpty:
			d.NeedsUploads++
		}
	}
	return s.views.Render(w, http.StatusOK, "dashboard", s.page(r, "Dashboard", d))
}

func (s *Server) newProjectForm(w http.ResponseWriter, r *http.Request) error {
	return s.views.Render(w, http.StatusOK, "project_new", s.page(r, "New project", newFormView()))
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) error {
	if err := parseForm(w, r); err != nil {
		return err
	}
	in := projects.CreateInput{Name: r.PostForm.Get("name"), Description: r.PostForm.Get("description")}
	form := newFormView()
	form.Values["name"] = in.Name
	form.Values["description"] = in.Description

	p, err := s.projects.Create(r.Context(), sessionFrom(r.Context()).user.ID, in)
	var verr projects.ValidationError
	switch {
	case errors.As(err, &verr):
		form.Errors = verr
		return s.views.Render(w, http.StatusUnprocessableEntity, "project_new", s.page(r, "New project", form))
	case errors.Is(err, projects.ErrNameTaken):
		form.Errors["name"] = "You already have a project with this name."
		return s.views.Render(w, http.StatusConflict, "project_new", s.page(r, "New project", form))
	case err != nil:
		return err
	}

	loggerFrom(r.Context(), s.log).Info("project created", slog.Int64("project_id", p.ID))
	s.flash(r, "Project “"+p.Name+"” created.")
	http.Redirect(w, r, "/projects/"+strconv.FormatInt(p.ID, 10), http.StatusSeeOther)
	return nil
}

func (s *Server) showProject(w http.ResponseWriter, r *http.Request) error {
	p, err := s.projectFromPath(r)
	if err != nil {
		return err
	}
	return s.views.Render(w, http.StatusOK, "project", s.page(r, p.Name, p))
}

func (s *Server) confirmDeleteProject(w http.ResponseWriter, r *http.Request) error {
	p, err := s.projectFromPath(r)
	if err != nil {
		return err
	}
	return s.views.Render(w, http.StatusOK, "project_delete", s.page(r, "Delete "+p.Name, p))
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) error {
	p, err := s.projectFromPath(r)
	if err != nil {
		return err
	}
	if err := s.projects.Delete(r.Context(), p.UserID, p.ID); err != nil {
		if errors.Is(err, projects.ErrNotFound) {
			return errNotFound()
		}
		return err
	}
	loggerFrom(r.Context(), s.log).Info("project deleted", slog.Int64("project_id", p.ID))
	s.flash(r, "Project “"+p.Name+"” deleted.")
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	return nil
}
