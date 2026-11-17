package server

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// client is a minimal browser: it keeps cookies between requests made
// directly against the server handler.
type client struct {
	t       *testing.T
	env     *testEnv
	cookies map[string]*http.Cookie
	ip      string
}

func (e *testEnv) client(t *testing.T) *client {
	return &client{t: t, env: e, cookies: map[string]*http.Cookie{}, ip: "192.0.2.10"}
}

func (c *client) send(req *http.Request) *httptest.ResponseRecorder {
	c.t.Helper()
	req.RemoteAddr = c.ip + ":5555"
	for _, ck := range c.cookies {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	c.env.srv.ServeHTTP(rec, req)
	for _, ck := range rec.Result().Cookies() {
		if ck.MaxAge < 0 {
			delete(c.cookies, ck.Name)
		} else {
			c.cookies[ck.Name] = ck
		}
	}
	return rec
}

func (c *client) get(target string) *httptest.ResponseRecorder {
	c.t.Helper()
	return c.send(httptest.NewRequest(http.MethodGet, target, nil))
}

func (c *client) post(target string, form url.Values) *httptest.ResponseRecorder {
	c.t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.send(req)
}

// postCSRF posts form with the CSRF token scraped from the page at tokenFrom.
func (c *client) postCSRF(target string, form url.Values) *httptest.ResponseRecorder {
	c.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf_token", c.csrf())
	return c.post(target, form)
}

var csrfRe = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

// csrf loads the dashboard and extracts the CSRF token from the nav's logout form.
func (c *client) csrf() string {
	c.t.Helper()
	rec := c.get("/dashboard")
	m := csrfRe.FindStringSubmatch(rec.Body.String())
	if m == nil {
		c.t.Fatalf("no CSRF token on dashboard (status %d)", rec.Code)
	}
	return html.UnescapeString(m[1])
}

func (c *client) sessionCookie() *http.Cookie {
	return c.cookies[c.env.srv.cookieName()]
}

// register signs up a new account through the HTTP flow.
func (c *client) register(email, password string) {
	c.t.Helper()
	rec := c.post("/register", url.Values{
		"email": {email}, "password": {password}, "password_confirm": {password},
	})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/dashboard" {
		c.t.Fatalf("register %s: status %d, location %q\n%s", email, rec.Code, rec.Header().Get("Location"), rec.Body)
	}
}

// createProject creates a project and returns its URL path.
func (c *client) createProject(name string) string {
	c.t.Helper()
	rec := c.postCSRF("/projects", url.Values{"name": {name}})
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/projects/") {
		c.t.Fatalf("create project: status %d location %q\n%s", rec.Code, loc, rec.Body)
	}
	return loc
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status = %d, want %d\nbody: %s", rec.Code, code, rec.Body)
	}
}

func wantRedirect(t *testing.T, rec *httptest.ResponseRecorder, location string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != location {
		t.Fatalf("got %d → %q, want 303 → %q", rec.Code, rec.Header().Get("Location"), location)
	}
}

func wantBody(t *testing.T, rec *httptest.ResponseRecorder, substrs ...string) {
	t.Helper()
	for _, s := range substrs {
		if !strings.Contains(rec.Body.String(), s) {
			t.Errorf("body missing %q", s)
		}
	}
}
