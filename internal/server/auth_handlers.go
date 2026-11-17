package server

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"github.com/quantamsage/ai-code-documentation-tool/internal/auth"
)

// formView is the data for a page containing a form: submitted values (never
// passwords), per-field errors and a form-level error.
type formView struct {
	Values map[string]string
	Errors map[string]string
	Error  string
	Next   string
}

func newFormView() *formView {
	return &formView{Values: map[string]string{}, Errors: map[string]string{}}
}

func (s *Server) registerForm(w http.ResponseWriter, r *http.Request) error {
	return s.views.Render(w, http.StatusOK, "register", s.page(r, "Create account", newFormView()))
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) error {
	if err := parseForm(w, r); err != nil {
		return err
	}
	in := auth.RegisterInput{
		Email:           r.PostForm.Get("email"),
		DisplayName:     r.PostForm.Get("display_name"),
		Password:        r.PostForm.Get("password"),
		PasswordConfirm: r.PostForm.Get("password_confirm"),
	}
	form := newFormView()
	form.Values["email"] = in.Email
	form.Values["display_name"] = in.DisplayName

	user, err := s.auth.Register(r.Context(), in)
	var verr auth.ValidationError
	switch {
	case errors.As(err, &verr):
		form.Errors = verr
		return s.views.Render(w, http.StatusUnprocessableEntity, "register", s.page(r, "Create account", form))
	case errors.Is(err, auth.ErrEmailTaken):
		form.Errors["email"] = "An account with this email already exists. Try logging in instead."
		return s.views.Render(w, http.StatusConflict, "register", s.page(r, "Create account", form))
	case err != nil:
		return err
	}

	loggerFrom(r.Context(), s.log).Info("user registered", slog.Int64("user_id", user.ID))
	if err := s.startSession(w, r, user, "Welcome to CodeDocs AI, "+user.DisplayName+"! Create your first project to get started."); err != nil {
		return err
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	return nil
}

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) error {
	form := newFormView()
	form.Next = safeNext(r.URL.Query().Get("next"), "")
	return s.views.Render(w, http.StatusOK, "login", s.page(r, "Log in", form))
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) error {
	if err := parseForm(w, r); err != nil {
		return err
	}
	email := r.PostForm.Get("email")
	form := newFormView()
	form.Values["email"] = email
	form.Next = safeNext(r.PostForm.Get("next"), "")

	user, err := s.auth.Authenticate(r.Context(), email, r.PostForm.Get("password"), clientIP(r))
	var throttled *auth.ThrottledError
	switch {
	case errors.As(err, &throttled):
		minutes := int(math.Ceil(throttled.RetryAfter.Minutes()))
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(throttled.RetryAfter.Seconds()))))
		form.Error = fmt.Sprintf("Too many failed login attempts. Please wait %d minute(s) and try again.", minutes)
		loggerFrom(r.Context(), s.log).Warn("login throttled", slog.String("ip", clientIP(r)))
		return s.views.Render(w, http.StatusTooManyRequests, "login", s.page(r, "Log in", form))
	case errors.Is(err, auth.ErrInvalidCredentials):
		form.Error = "Incorrect email or password."
		return s.views.Render(w, http.StatusUnauthorized, "login", s.page(r, "Log in", form))
	case err != nil:
		return err
	}

	loggerFrom(r.Context(), s.log).Info("user logged in", slog.Int64("user_id", user.ID))
	if err := s.startSession(w, r, user, ""); err != nil {
		return err
	}
	http.Redirect(w, r, safeNext(form.Next, "/dashboard"), http.StatusSeeOther)
	return nil
}

// startSession issues a fresh session for user, replacing any session the
// request already carried (preventing session fixation).
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, user auth.User, flash string) error {
	if c, err := r.Cookie(s.cookieName()); err == nil {
		if err := s.auth.DeleteSession(r.Context(), c.Value); err != nil {
			return err
		}
	}
	token, sess, err := s.auth.CreateSession(r.Context(), user.ID, r.UserAgent(), clientIP(r))
	if err != nil {
		return err
	}
	if flash != "" {
		if err := s.auth.SetFlash(r.Context(), sess.ID, flash); err != nil {
			return err
		}
	}
	s.setSessionCookie(w, token)
	return nil
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) error {
	cs := sessionFrom(r.Context())
	if err := s.auth.DeleteSession(r.Context(), cs.token); err != nil {
		return err
	}
	s.clearSessionCookie(w)
	loggerFrom(r.Context(), s.log).Info("user logged out")
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return nil
}
