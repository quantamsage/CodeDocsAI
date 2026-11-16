package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/quantamsage/ai-code-documentation-tool/internal/view"
)

// HTTPError is an error that carries an HTTP status and a message that is safe
// to show to the user. The wrapped Err (if any) is logged but never displayed.
type HTTPError struct {
	Status  int
	Message string
	Err     error
}

func (e *HTTPError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%d %s: %v", e.Status, e.Message, e.Err)
	}
	return fmt.Sprintf("%d %s", e.Status, e.Message)
}

func (e *HTTPError) Unwrap() error { return e.Err }

// Error helpers for handlers.
func errNotFound() error {
	return &HTTPError{Status: http.StatusNotFound, Message: "The page you were looking for does not exist."}
}

// handlerFunc is an HTTP handler that may return an error. Returning an error
// lets handlers stay linear; the error is rendered and logged centrally.
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// handle adapts a handlerFunc into an http.Handler with centralized error handling.
func (s *Server) handle(h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			s.handleError(w, r, err)
		}
	})
}

// handleError logs err and renders an error page. Unknown errors become a 500
// with a generic message so internal details are never leaked.
func (s *Server) handleError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	message := "Something went wrong on our side. Please try again later."

	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		status = httpErr.Status
		message = httpErr.Message
	}

	log := loggerFrom(r.Context(), s.log)
	if status >= 500 {
		log.Error("request failed", slog.Int("status", status), slog.String("error", err.Error()))
	} else {
		log.Debug("request rejected", slog.Int("status", status), slog.String("error", err.Error()))
	}

	s.renderError(w, r, status, message)
}

// renderError renders the HTML error page, falling back to plain text if the
// error template itself fails.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, message string) {
	page := view.Page{
		Title:     http.StatusText(status),
		RequestID: requestIDFrom(r.Context()),
		Data: map[string]any{
			"Status":  status,
			"Heading": http.StatusText(status),
			"Message": message,
		},
	}
	if err := s.views.Render(w, status, "error", page); err != nil {
		loggerFrom(r.Context(), s.log).Error("render error page", slog.String("error", err.Error()))
		http.Error(w, message, status)
	}
}
