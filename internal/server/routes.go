package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/quantamsage/ai-code-documentation-tool/web"
)

// routes registers every route. Patterns use Go 1.22+ method/path matching.
func (s *Server) routes(mux *http.ServeMux) {
	mux.Handle("GET /static/", s.static())
	mux.HandleFunc("GET /healthz", s.healthz)

	mux.Handle("GET /{$}", s.handle(s.home))

	// Authentication.
	mux.Handle("GET /register", s.handle(s.requireGuest(s.registerForm)))
	mux.Handle("POST /register", s.handle(s.requireGuest(s.register)))
	mux.Handle("GET /login", s.handle(s.requireGuest(s.loginForm)))
	mux.Handle("POST /login", s.handle(s.login))
	mux.Handle("POST /logout", s.handle(s.requireAuth(s.logout)))

	// Dashboard and projects (signed-in users only; every lookup is owner-scoped).
	mux.Handle("GET /dashboard", s.handle(s.requireAuth(s.dashboard)))
	mux.Handle("GET /projects", http.RedirectHandler("/dashboard", http.StatusSeeOther))
	mux.Handle("GET /projects/new", s.handle(s.requireAuth(s.newProjectForm)))
	mux.Handle("POST /projects", s.handle(s.requireAuth(s.createProject)))
	mux.Handle("GET /projects/{id}", s.handle(s.requireAuth(s.showProject)))
	mux.Handle("GET /projects/{id}/delete", s.handle(s.requireAuth(s.confirmDeleteProject)))
	mux.Handle("POST /projects/{id}/delete", s.handle(s.requireAuth(s.deleteProject)))

	// Uploads and the project explorer.
	mux.Handle("GET /projects/{id}/upload", s.handle(s.requireAuth(s.uploadForm)))
	mux.Handle("POST /projects/{id}/upload", s.handle(s.acceptUpload(s.requireAuth(s.uploadFiles))))
	mux.Handle("GET /projects/{id}/files/{path...}", s.handle(s.requireAuth(s.showFile)))

	// AI analysis (background jobs).
	mux.Handle("POST /projects/{id}/analyze", s.handle(s.requireAuth(s.analyzeProject)))
	mux.Handle("GET /projects/{id}/analysis", s.handle(s.requireAuth(s.showAnalysis)))
	mux.Handle("GET /projects/{id}/jobs/latest", s.handle(s.requireAuth(s.latestJob)))
	mux.Handle("POST /projects/{id}/jobs/{jobID}/cancel", s.handle(s.requireAuth(s.cancelJob)))

	// Documentation.
	mux.Handle("POST /projects/{id}/docs/generate", s.handle(s.requireAuth(s.generateDocs)))
	mux.Handle("GET /projects/{id}/docs", s.handle(s.requireAuth(s.docsIndex)))
	mux.Handle("GET /projects/{id}/docs/{scope}", s.handle(s.requireAuth(s.showDoc)))
	mux.Handle("GET /projects/{id}/docs/{scope}/download", s.handle(s.requireAuth(s.downloadDoc)))
	mux.Handle("GET /projects/{id}/docs/export.zip", s.handle(s.requireAuth(s.exportDocs)))

	// Any unmatched GET renders the HTML 404 page. It is GET-only so that a
	// known path requested with the wrong method gets the mux's 405 response.
	mux.Handle("GET /", s.handle(func(http.ResponseWriter, *http.Request) error { return errNotFound() }))
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) error {
	return s.views.Render(w, http.StatusOK, "home", s.page(r, "", nil))
}

// healthz reports liveness and database connectivity as JSON.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	status, dbStatus, code := "ok", "ok", http.StatusOK
	if err := s.db.PingContext(ctx); err != nil {
		loggerFrom(r.Context(), s.log).Error("health check: database unreachable", "error", err)
		status, dbStatus, code = "degraded", "unavailable", http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"status": status, "database": dbStatus}) //nolint:errcheck
}

// static serves embedded assets. Directory listings are disabled.
func (s *Server) static() http.Handler {
	files := http.StripPrefix("/static/", http.FileServerFS(web.Static()))
	cacheControl := "no-cache"
	if s.cfg.IsProduction() {
		cacheControl = "public, max-age=3600"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			s.handleError(w, r, errNotFound())
			return
		}
		w.Header().Set("Cache-Control", cacheControl)
		files.ServeHTTP(w, r)
	})
}
