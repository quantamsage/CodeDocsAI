package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/quantamsage/ai-code-documentation-tool/internal/view"
	"github.com/quantamsage/ai-code-documentation-tool/web"
)

// routes registers every route. Patterns use Go 1.22+ method/path matching.
func (s *Server) routes(mux *http.ServeMux) {
	mux.Handle("GET /static/", s.static())
	mux.HandleFunc("GET /healthz", s.healthz)

	mux.Handle("GET /{$}", s.handle(s.home))

	// Any unmatched GET renders the HTML 404 page. It is GET-only so that a
	// known path requested with the wrong method gets the mux's 405 response.
	mux.Handle("GET /", s.handle(func(http.ResponseWriter, *http.Request) error { return errNotFound() }))
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) error {
	return s.views.Render(w, http.StatusOK, "home", view.Page{RequestID: requestIDFrom(r.Context())})
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
