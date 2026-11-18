package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyLogger
	ctxKeySession
	ctxKeyMultipart
	ctxKeyMultipartAllowed
)

// RequestIDHeader is the response header carrying the request ID.
const RequestIDHeader = "X-Request-ID"

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

// loggerFrom returns the request-scoped logger, or fallback outside a request.
func loggerFrom(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if l, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok {
		return l
	}
	return fallback
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// middleware wraps an http.Handler.
type middleware func(http.Handler) http.Handler

// chain applies middlewares so that the first one listed is the outermost.
func chain(h http.Handler, mws ...middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// requestID assigns a server-generated ID to each request and attaches a
// request-scoped logger to the context. Client-supplied IDs are ignored so log
// correlation cannot be spoofed.
func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set(RequestIDHeader, id)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		ctx = context.WithValue(ctx, ctxKeyLogger, s.log.With(slog.String("request_id", id)))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// statusRecorder captures the status code and bytes written for logging.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (rec *statusRecorder) WriteHeader(code int) {
	if !rec.wroteHeader {
		rec.status = code
		rec.wroteHeader = true
	}
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *statusRecorder) Write(b []byte) (int, error) {
	if !rec.wroteHeader {
		rec.WriteHeader(http.StatusOK)
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer (flush, deadlines).
func (rec *statusRecorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// logRequests logs one line per request with method, path, status and duration.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		} else if rec.status >= 400 {
			level = slog.LevelWarn
		}
		loggerFrom(r.Context(), s.log).LogAttrs(r.Context(), level, "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int("bytes", rec.bytes),
			slog.Duration("duration", time.Since(start)),
		)
	})
}

// recoverPanics converts a handler panic into a logged 500 response instead of
// a dropped connection. http.ErrAbortHandler is re-panicked as intended.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			loggerFrom(r.Context(), s.log).Error("panic serving request",
				slog.Any("panic", v),
				slog.String("stack", string(debug.Stack())),
			)
			if rec.wroteHeader {
				// Too late to send an error page; the response is already partially written.
				return
			}
			w.Header().Set("Connection", "close")
			s.renderError(w, r, http.StatusInternalServerError, "Something went wrong on our side. Please try again later.")
		}()
		next.ServeHTTP(rec, r)
	})
}

// securityHeaders sets defensive HTTP headers on every response.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	csp := "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
		"object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if s.cfg.IsProduction() {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}
