package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/quantamsage/ai-code-documentation-tool/internal/auth"
	"github.com/quantamsage/ai-code-documentation-tool/internal/view"
)

// currentSession is the authenticated state attached to a request context.
type currentSession struct {
	token   string
	session auth.Session
	user    auth.User
}

func sessionFrom(ctx context.Context) *currentSession {
	cs, _ := ctx.Value(ctxKeySession).(*currentSession)
	return cs
}

// maxFormBytes bounds urlencoded form bodies (login, register, project forms).
const maxFormBytes = 64 << 10

// cookieName returns the session cookie name. In production the __Host-
// prefix makes browsers insist on Secure, Path=/ and no Domain attribute.
func (s *Server) cookieName() string {
	if s.cfg.IsProduction() {
		return "__Host-codedocs_session"
	}
	return "codedocs_session"
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    token,
		Path:     "/",
		MaxAge:   int(s.auth.SessionLifetime() / time.Second),
		HttpOnly: true,
		Secure:   s.cfg.IsProduction(),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cfg.IsProduction(),
		SameSite: http.SameSiteLaxMode,
	})
}

// loadSession resolves the session cookie (if any) and attaches the session
// and user to the request context. Static assets and the health check skip
// the database lookup.
func (s *Server) loadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		c, err := r.Cookie(s.cookieName())
		if err != nil || c.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		sess, user, err := s.auth.LookupSession(r.Context(), c.Value)
		switch {
		case errors.Is(err, auth.ErrNoSession):
			s.clearSessionCookie(w)
			next.ServeHTTP(w, r)
		case err != nil:
			s.handleError(w, r, err)
		default:
			cs := &currentSession{token: c.Value, session: sess, user: user}
			ctx := context.WithValue(r.Context(), ctxKeySession, cs)
			ctx = context.WithValue(ctx, ctxKeyLogger, loggerFrom(r.Context(), s.log).With(slog.Int64("user_id", user.ID)))
			next.ServeHTTP(w, r.WithContext(ctx))
		}
	})
}

// crossOrigin rejects cross-origin state-changing browser requests (CSRF
// defence for every form, including anonymous login and registration).
func (s *Server) crossOrigin(next http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleError(w, r, &HTTPError{Status: http.StatusForbidden, Message: "Cross-origin requests are not allowed."})
	}))
	return cop.Handler(next)
}

// requireAuth only runs h for signed-in users. Anonymous GET requests are
// redirected to the login page (returning afterwards); other anonymous
// requests are sent to the login page without a return path. For
// state-changing methods it also verifies the session's CSRF token.
func (s *Server) requireAuth(h handlerFunc) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		cs := sessionFrom(r.Context())
		if cs == nil {
			target := "/login"
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				target += "?next=" + url.QueryEscape(r.URL.RequestURI())
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
			return nil
		}
		// Pages for signed-in users must not be stored by shared caches or
		// shown from the back/forward cache after logout.
		w.Header().Set("Cache-Control", "no-store")

		if !isSafeMethod(r.Method) {
			var err error
			if r, err = s.verifyCSRF(w, r, cs); err != nil {
				return err
			}
		}
		return h(w, r)
	}
}

// requireGuest sends signed-in users to their dashboard instead of h.
func (s *Server) requireGuest(h handlerFunc) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if sessionFrom(r.Context()) != nil {
			http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
			return nil
		}
		return h(w, r)
	}
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

var errCSRF = &HTTPError{Status: http.StatusForbidden, Message: "Your form has expired. Please go back, reload the page and try again."}

// verifyCSRF checks the synchronizer token from the X-CSRF-Token header or,
// for urlencoded forms, the csrf_token field. On routes that accept streamed
// multipart uploads (see acceptUpload), the token must be the first part of
// the body; the returned request then carries the multipart reader,
// positioned after that part, for the handler to continue from.
func (s *Server) verifyCSRF(w http.ResponseWriter, r *http.Request, cs *currentSession) (*http.Request, error) {
	token := r.Header.Get("X-CSRF-Token")
	if token == "" {
		if multipartAllowed(r) && isMultipart(r) {
			mr, err := r.MultipartReader()
			if err != nil {
				return r, &HTTPError{Status: http.StatusBadRequest, Message: "The upload could not be read.", Err: err}
			}
			if token, err = readCSRFPart(mr); err != nil {
				return r, err
			}
			r = r.WithContext(context.WithValue(r.Context(), ctxKeyMultipart, mr))
		} else {
			if err := parseForm(w, r); err != nil {
				return r, err
			}
			token = r.PostForm.Get("csrf_token")
		}
	}
	if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(cs.session.CSRFToken)) != 1 {
		return r, errCSRF
	}
	return r, nil
}

// readCSRFPart reads the csrf_token field, which must be the first part of a
// multipart body (browsers send fields in document order).
func readCSRFPart(mr *multipart.Reader) (string, error) {
	part, err := mr.NextPart()
	if err != nil {
		return "", errCSRF
	}
	defer part.Close()
	if part.FormName() != "csrf_token" || part.FileName() != "" {
		return "", errCSRF
	}
	raw, err := io.ReadAll(io.LimitReader(part, 256))
	if err != nil {
		return "", errCSRF
	}
	return string(raw), nil
}

func isMultipart(r *http.Request) bool {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return ct == "multipart/form-data"
}

func multipartAllowed(r *http.Request) bool {
	ok, _ := r.Context().Value(ctxKeyMultipartAllowed).(bool)
	return ok
}

// parseForm parses an application/x-www-form-urlencoded body with a size cap.
// It is idempotent.
func parseForm(w http.ResponseWriter, r *http.Request) error {
	if r.PostForm != nil {
		return nil
	}
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/x-www-form-urlencoded" {
		return &HTTPError{Status: http.StatusUnsupportedMediaType, Message: "Unsupported form submission."}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return &HTTPError{Status: http.StatusRequestEntityTooLarge, Message: "The submitted form is too large.", Err: err}
		}
		return &HTTPError{Status: http.StatusBadRequest, Message: "The submitted form could not be read.", Err: err}
	}
	return nil
}

// page builds the view.Page for a request, including the signed-in user and
// any pending flash message (which is consumed).
func (s *Server) page(r *http.Request, title string, data any) view.Page {
	p := view.Page{Title: title, RequestID: requestIDFrom(r.Context()), Data: data}
	cs := sessionFrom(r.Context())
	if cs == nil {
		return p
	}
	p.User = &view.User{ID: cs.user.ID, Email: cs.user.Email, DisplayName: cs.user.DisplayName}
	p.CSRFToken = cs.session.CSRFToken
	if cs.session.Flash != "" {
		p.Flash = cs.session.Flash
		if err := s.auth.ClearFlash(r.Context(), cs.session.ID); err != nil {
			loggerFrom(r.Context(), s.log).Warn("clear flash", slog.String("error", err.Error()))
		}
		cs.session.Flash = ""
	}
	return p
}

// flash stores a message to show on the next page the user sees.
func (s *Server) flash(r *http.Request, msg string) {
	cs := sessionFrom(r.Context())
	if cs == nil {
		return
	}
	if err := s.auth.SetFlash(r.Context(), cs.session.ID, msg); err != nil {
		loggerFrom(r.Context(), s.log).Warn("set flash", slog.String("error", err.Error()))
	}
}

// safeNext returns next if it is a local path on this site, else fallback.
// It rejects absolute URLs, scheme-relative "//host" and "/\host" forms that
// browsers treat as off-site.
func safeNext(next, fallback string) string {
	if next == "" || len(next) > 2048 || next[0] != '/' {
		return fallback
	}
	if len(next) > 1 && (next[1] == '/' || next[1] == '\\') {
		return fallback
	}
	if strings.ContainsAny(next, "\r\n\t") || strings.Contains(next, "\\") {
		return fallback
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return fallback
	}
	return next
}

// clientIP returns the TCP peer address. Forwarding headers are deliberately
// ignored because they are client-controlled unless a trusted proxy sets them.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
