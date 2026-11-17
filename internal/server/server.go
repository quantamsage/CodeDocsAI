// Package server wires HTTP routing, middleware, and handlers.
package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/quantamsage/ai-code-documentation-tool/internal/auth"
	"github.com/quantamsage/ai-code-documentation-tool/internal/config"
	"github.com/quantamsage/ai-code-documentation-tool/internal/projects"
	"github.com/quantamsage/ai-code-documentation-tool/internal/view"
	"github.com/quantamsage/ai-code-documentation-tool/web"
)

// Server is the HTTP application. It owns shared dependencies that handlers use.
type Server struct {
	cfg     config.Config
	db      *sql.DB
	log     *slog.Logger
	views   *view.Renderer
	handler http.Handler

	authOpts auth.Options
	auth     *auth.Service
	projects *projects.Store
}

// Option customizes a Server (mainly for tests).
type Option func(*Server)

// WithAuthOptions overrides the auth service options (hasher cost, clock,
// throttling). Session timeouts from the config are applied when the option
// leaves them unset.
func WithAuthOptions(o auth.Options) Option {
	return func(s *Server) { s.authOpts = o }
}

// sessionPurgeInterval is how often expired sessions are deleted.
const sessionPurgeInterval = 15 * time.Minute

// New builds a Server with all routes and middleware configured.
func New(cfg config.Config, db *sql.DB, log *slog.Logger, opts ...Option) (*Server, error) {
	views, err := view.New(web.Templates())
	if err != nil {
		return nil, fmt.Errorf("load templates: %w", err)
	}
	s := &Server{cfg: cfg, db: db, log: log, views: views}
	for _, o := range opts {
		o(s)
	}

	if s.authOpts.SessionLifetime == 0 {
		s.authOpts.SessionLifetime = cfg.SessionLifetime
	}
	if s.authOpts.IdleTimeout == 0 {
		s.authOpts.IdleTimeout = cfg.SessionIdleTimeout
	}
	if s.authOpts.Log == nil {
		s.authOpts.Log = log
	}
	s.auth = auth.NewService(db, s.authOpts)
	s.projects = projects.NewStore(db, s.authOpts.Now)

	mux := http.NewServeMux()
	s.routes(mux)
	// Order matters: request ID first so every later layer can log with it;
	// recovery inside logging so panics are still logged as 500s; the
	// cross-origin check runs before any session lookup or handler work.
	s.handler = chain(mux, s.requestID, s.logRequests, s.recoverPanics, s.securityHeaders, s.crossOrigin, s.loadSession)
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// Run listens on cfg.Addr and serves until ctx is canceled, then shuts down
// gracefully within cfg.ShutdownTimeout.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.Addr, err)
	}
	return s.Serve(ctx, ln)
}

// Serve serves on ln until ctx is canceled.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	go s.auth.RunPurge(ctx, sessionPurgeInterval)

	errCh := make(chan error, 1)
	go func() {
		s.log.Info("http server listening", slog.String("addr", ln.Addr().String()))
		errCh <- srv.Serve(ln)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	s.log.Info("shutting down http server")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
