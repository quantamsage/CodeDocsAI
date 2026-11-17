package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/quantamsage/ai-code-documentation-tool/internal/config"
)

func TestRegisterLoginLogoutFlow(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)

	wantStatus(t, c.get("/register"), http.StatusOK)
	c.register("alice@example.com", "correct horse")

	ck := c.sessionCookie()
	if ck == nil {
		t.Fatal("no session cookie after registration")
	}
	if !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.Path != "/" || ck.Secure {
		t.Errorf("cookie attributes = %+v", ck)
	}
	if ck.MaxAge != int((7 * 24 * time.Hour).Seconds()) {
		t.Errorf("cookie MaxAge = %d", ck.MaxAge)
	}

	rec := c.get("/dashboard")
	wantStatus(t, rec, http.StatusOK)
	wantBody(t, rec, "Welcome to CodeDocs AI, alice!", "No projects yet", "alice@example.com", `action="/logout"`)
	if strings.Contains(c.get("/dashboard").Body.String(), "Welcome to CodeDocs AI") {
		t.Error("flash message should only be shown once")
	}

	// Logging out needs the CSRF token.
	wantStatus(t, c.post("/logout", nil), http.StatusForbidden)
	oldCookie := *c.sessionCookie()
	wantRedirect(t, c.postCSRF("/logout", nil), "/")
	if c.sessionCookie() != nil {
		t.Error("session cookie not cleared on logout")
	}
	var n int
	env.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n) //nolint:errcheck
	if n != 0 {
		t.Errorf("%d session rows remain after logout", n)
	}
	wantRedirect(t, c.get("/dashboard"), "/login?next=%2Fdashboard")

	// A copied cookie from before logout is useless.
	c.cookies[oldCookie.Name] = &oldCookie
	wantRedirect(t, c.get("/dashboard"), "/login?next=%2Fdashboard")

	// Log back in.
	rec = c.post("/login", url.Values{"email": {"ALICE@example.com"}, "password": {"correct horse"}})
	wantRedirect(t, rec, "/dashboard")
	wantStatus(t, c.get("/dashboard"), http.StatusOK)
}

func TestRegisterValidationErrors(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)

	rec := c.post("/register", url.Values{
		"email": {"not-an-email"}, "display_name": {"Al"}, "password": {"short"}, "password_confirm": {"short"},
	})
	wantStatus(t, rec, http.StatusUnprocessableEntity)
	wantBody(t, rec, "Enter a valid email address", "at least 8 characters", `value="not-an-email"`, `value="Al"`)
	if strings.Contains(rec.Body.String(), `value="short"`) {
		t.Error("password must never be echoed back")
	}

	rec = c.post("/register", url.Values{"email": {"a@example.com"}, "password": {"correct horse"}, "password_confirm": {"different!"}})
	wantStatus(t, rec, http.StatusUnprocessableEntity)
	wantBody(t, rec, "Passwords do not match")

	if c.sessionCookie() != nil {
		t.Error("failed registration must not create a session")
	}
	var n int
	env.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n) //nolint:errcheck
	if n != 0 {
		t.Errorf("%d users created by invalid registrations", n)
	}
}

func TestRegisterDuplicateEmail(t *testing.T) {
	env := newTestEnv(t)
	env.client(t).register("alice@example.com", "correct horse")

	rec := env.client(t).post("/register", url.Values{
		"email": {"Alice@Example.com"}, "password": {"other password"}, "password_confirm": {"other password"},
	})
	wantStatus(t, rec, http.StatusConflict)
	wantBody(t, rec, "already exists")
}

func TestLoginFailuresAreGenericAndThrottled(t *testing.T) {
	env := newTestEnv(t)
	env.client(t).register("alice@example.com", "correct horse")
	c := env.client(t)

	wrong := c.post("/login", url.Values{"email": {"alice@example.com"}, "password": {"nope nope"}})
	unknown := c.post("/login", url.Values{"email": {"bob@example.com"}, "password": {"nope nope"}})
	for _, rec := range []*httptest.ResponseRecorder{wrong, unknown} {
		wantStatus(t, rec, http.StatusUnauthorized)
		wantBody(t, rec, "Incorrect email or password.")
	}
	if strings.Contains(wrong.Body.String(), "nope nope") {
		t.Error("password echoed back")
	}
	wantBody(t, wrong, `value="alice@example.com"`)

	// The test env allows 3 failures per email.
	c.post("/login", url.Values{"email": {"alice@example.com"}, "password": {"nope 2"}})
	c.post("/login", url.Values{"email": {"alice@example.com"}, "password": {"nope 3"}})
	rec := c.post("/login", url.Values{"email": {"alice@example.com"}, "password": {"correct horse"}})
	wantStatus(t, rec, http.StatusTooManyRequests)
	wantBody(t, rec, "Too many failed login attempts")
	if rec.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After header")
	}
	if c.sessionCookie() != nil {
		t.Error("throttled login must not create a session")
	}

	env.clock.Advance(16 * time.Minute)
	wantRedirect(t, c.post("/login", url.Values{"email": {"alice@example.com"}, "password": {"correct horse"}}), "/dashboard")
}

func TestLoginRedirectsToSafeNext(t *testing.T) {
	env := newTestEnv(t)
	env.client(t).register("alice@example.com", "correct horse")

	c := env.client(t)
	rec := c.get("/projects/new")
	wantRedirect(t, rec, "/login?next=%2Fprojects%2Fnew")
	rec = c.get(rec.Header().Get("Location"))
	wantBody(t, rec, `name="next" value="/projects/new"`)

	rec = c.post("/login", url.Values{"email": {"alice@example.com"}, "password": {"correct horse"}, "next": {"/projects/new"}})
	wantRedirect(t, rec, "/projects/new")

	for _, evil := range []string{"https://evil.example", "//evil.example", "/\\evil.example", "javascript:alert(1)"} {
		c := env.client(t)
		rec := c.post("/login", url.Values{"email": {"alice@example.com"}, "password": {"correct horse"}, "next": {evil}})
		wantRedirect(t, rec, "/dashboard")
	}
}

func TestSafeNext(t *testing.T) {
	cases := map[string]string{
		"":                     "/fallback",
		"/dashboard":           "/dashboard",
		"/projects/1?tab=docs": "/projects/1?tab=docs",
		"//evil.com":           "/fallback",
		"/\\evil.com":          "/fallback",
		"https://evil.com/":    "/fallback",
		"evil.com":             "/fallback",
		"/ok\r\nSet-Cookie:x":  "/fallback",
		"/a\\b":                "/fallback",
	}
	for in, want := range cases {
		if got := safeNext(in, "/fallback"); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoginRotatesSession(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	c.register("alice@example.com", "correct horse")
	before := c.sessionCookie().Value

	// Posting the login form while holding a session (e.g. a planted one)
	// must issue a new token and invalidate the old one.
	wantRedirect(t, c.post("/login", url.Values{"email": {"alice@example.com"}, "password": {"correct horse"}}), "/dashboard")
	after := c.sessionCookie().Value
	if after == before {
		t.Fatal("session token not rotated on login")
	}
	stale := env.client(t)
	stale.cookies[env.srv.cookieName()] = &http.Cookie{Name: env.srv.cookieName(), Value: before}
	wantRedirect(t, stale.get("/dashboard"), "/login?next=%2Fdashboard")
}

func TestSessionIdleExpiry(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	c.register("alice@example.com", "correct horse")

	env.clock.Advance(23 * time.Hour)
	wantStatus(t, c.get("/dashboard"), http.StatusOK)

	env.clock.Advance(24*time.Hour + time.Minute)
	wantRedirect(t, c.get("/dashboard"), "/login?next=%2Fdashboard")
	if c.sessionCookie() != nil {
		t.Error("expired session cookie should be cleared")
	}
}

func TestGuestPagesRedirectSignedInUsers(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	c.register("alice@example.com", "correct horse")
	wantRedirect(t, c.get("/login"), "/dashboard")
	wantRedirect(t, c.get("/register"), "/dashboard")

	rec := c.get("/")
	wantStatus(t, rec, http.StatusOK)
	wantBody(t, rec, "Go to your dashboard", `href="/dashboard"`)
}

func TestAnonymousAccessIsRedirected(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)

	for _, path := range []string{"/dashboard", "/projects/new", "/projects/1", "/projects/1/delete"} {
		wantRedirect(t, c.get(path), "/login?next="+url.QueryEscape(path))
	}
	for _, path := range []string{"/projects", "/projects/1/delete", "/logout"} {
		wantRedirect(t, c.post(path, url.Values{"name": {"x"}}), "/login")
	}
	var n int
	env.db.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&n) //nolint:errcheck
	if n != 0 {
		t.Error("anonymous request created a project")
	}

	rec := c.get("/")
	wantBody(t, rec, `href="/login"`, `href="/register"`)
}

func TestCrossOriginPostsRejected(t *testing.T) {
	env := newTestEnv(t)
	env.client(t).register("alice@example.com", "correct horse")

	form := url.Values{"email": {"alice@example.com"}, "password": {"correct horse"}}
	for name, set := range map[string]func(*http.Request){
		"sec-fetch-site": func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"origin":         func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			set(req)
			rec := httptest.NewRecorder()
			env.srv.ServeHTTP(rec, req)
			wantStatus(t, rec, http.StatusForbidden)
			wantBody(t, rec, "Cross-origin requests are not allowed.")
			if len(rec.Result().Cookies()) != 0 {
				t.Error("cross-origin login set a cookie")
			}
		})
	}

	// Same-origin browser requests pass.
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, req)
	wantRedirect(t, rec, "/dashboard")
}

func TestCSRFTokenRequiredForAuthenticatedPosts(t *testing.T) {
	env := newTestEnv(t)
	alice := env.client(t)
	alice.register("alice@example.com", "correct horse")
	bob := env.client(t)
	bob.register("bob@example.com", "correct horse")

	cases := map[string]url.Values{
		"missing":       {"name": {"x"}},
		"wrong":         {"name": {"x"}, "csrf_token": {"forged"}},
		"other session": {"name": {"x"}, "csrf_token": {bob.csrf()}},
	}
	for name, form := range cases {
		t.Run(name, func(t *testing.T) {
			rec := alice.post("/projects", form)
			wantStatus(t, rec, http.StatusForbidden)
			wantBody(t, rec, "Your form has expired")
		})
	}

	// The header form of the token is accepted (for future JS clients).
	req := httptest.NewRequest(http.MethodPost, "/projects", strings.NewReader("name=Via+header"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", alice.csrf())
	wantStatus(t, alice.send(req), http.StatusSeeOther)

	var n int
	env.db.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&n) //nolint:errcheck
	if n != 1 {
		t.Errorf("projects = %d, want 1", n)
	}
}

func TestOversizedAndWrongTypeForms(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)

	big := url.Values{"email": {strings.Repeat("a", maxFormBytes)}, "password": {"x"}}
	wantStatus(t, c.post("/login", big), http.StatusRequestEntityTooLarge)

	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"email":"a"}`))
	req.Header.Set("Content-Type", "application/json")
	wantStatus(t, c.send(req), http.StatusUnsupportedMediaType)
}

func TestAuthenticatedPagesAreNotCached(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	c.register("alice@example.com", "correct horse")
	if cc := c.get("/dashboard").Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
}

func TestProductionSessionCookie(t *testing.T) {
	env := newTestEnvWith(t, func(c *config.Config) { c.Env = config.EnvProduction })
	c := env.client(t)
	c.register("alice@example.com", "correct horse")

	ck := c.sessionCookie()
	if ck == nil || ck.Name != "__Host-codedocs_session" || !ck.Secure || !ck.HttpOnly || ck.Domain != "" {
		t.Fatalf("production cookie = %+v", ck)
	}
}
