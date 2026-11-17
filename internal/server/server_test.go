package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quantamsage/ai-code-documentation-tool/internal/auth"
	"github.com/quantamsage/ai-code-documentation-tool/internal/config"
	"github.com/quantamsage/ai-code-documentation-tool/internal/database"
)

type testEnv struct {
	srv   *Server
	db    *sql.DB
	logs  *bytes.Buffer
	clock *fakeClock
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnvWith(t, nil)
}

// newTestEnvWith builds a server with a fast password hasher and a fake
// clock; tweak (if non-nil) may adjust the configuration first.
func newTestEnvWith(t *testing.T, tweak func(*config.Config)) *testEnv {
	t.Helper()
	cfg, err := config.LoadFrom(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	cfg.ShutdownTimeout = 2 * time.Second
	if tweak != nil {
		tweak(&cfg)
	}

	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	clock := newClock()
	srv, err := New(cfg, db, log, WithAuthOptions(auth.Options{
		Hasher:              auth.NewHasher(auth.Argon2Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}, 4),
		MaxFailuresPerEmail: 3,
		Now:                 clock.Now,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return &testEnv{srv: srv, db: db, logs: logs, clock: clock}
}

func (e *testEnv) do(t *testing.T, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestHomePageRendersLayout(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(t, http.MethodGet, "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<!doctype html>",
		`<meta name="viewport"`,
		`href="/static/css/app.css"`,
		`<header class="site-header">`,
		"Understand any codebase",
		`<footer class="site-footer">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("home page missing %q", want)
		}
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Errorf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
}

func TestSecurityHeadersAndRequestID(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(t, http.MethodGet, "/")

	h := rec.Header()
	for _, name := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
		if h.Get(name) == "" {
			t.Errorf("missing header %s", name)
		}
	}
	if h.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS must not be sent in development")
	}

	id := h.Get(RequestIDHeader)
	if len(id) != 16 {
		t.Errorf("request ID = %q", id)
	}
	if other := env.do(t, http.MethodGet, "/").Header().Get(RequestIDHeader); other == id {
		t.Error("request IDs should be unique")
	}
	if !strings.Contains(env.logs.String(), "request_id="+id) {
		t.Errorf("request log missing request id; logs:\n%s", env.logs)
	}
}

func TestClientRequestIDIgnored(t *testing.T) {
	env := newTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, "spoofed")
	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, req)
	if rec.Header().Get(RequestIDHeader) == "spoofed" {
		t.Error("client-supplied request ID must not be trusted")
	}
}

func TestNotFoundRendersHTMLPage(t *testing.T) {
	env := newTestEnv(t)
	for _, path := range []string{"/does-not-exist", "/static/", "/static/css/", "/static/missing.css"} {
		rec := env.do(t, http.MethodGet, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d", path, rec.Code)
		}
	}
	rec := env.do(t, http.MethodGet, "/does-not-exist")
	if !strings.Contains(rec.Body.String(), `class="error-page"`) {
		t.Errorf("404 page not rendered from template: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), rec.Header().Get(RequestIDHeader)) {
		t.Error("error page should display the request ID")
	}
}

func TestMethodNotAllowed(t *testing.T) {
	env := newTestEnv(t)
	if rec := env.do(t, http.MethodPost, "/healthz"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz status = %d", rec.Code)
	}
}

func TestStaticAssets(t *testing.T) {
	env := newTestEnv(t)
	cases := map[string]string{
		"/static/css/app.css": "text/css",
		"/static/js/app.js":   "javascript",
	}
	for path, ctype := range cases {
		rec := env.do(t, http.MethodGet, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d", path, rec.Code)
			continue
		}
		if !strings.Contains(rec.Header().Get("Content-Type"), ctype) {
			t.Errorf("%s: Content-Type = %q", path, rec.Header().Get("Content-Type"))
		}
		if rec.Header().Get("Cache-Control") == "" {
			t.Errorf("%s: missing Cache-Control", path)
		}
	}
	// Path traversal out of the static tree must not succeed.
	if rec := env.do(t, http.MethodGet, "/static/../web.go"); rec.Code == http.StatusOK {
		t.Error("path traversal served a file")
	}
}

func TestHealthz(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(t, http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" || body["database"] != "ok" {
		t.Errorf("body = %v", body)
	}

	env.db.Close()
	rec = env.do(t, http.MethodGet, "/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status with closed DB = %d", rec.Code)
	}
}

func TestHandlerErrors(t *testing.T) {
	env := newTestEnv(t)

	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantText   string
		hiddenText string
	}{
		{"http error", &HTTPError{Status: http.StatusBadRequest, Message: "Bad input.", Err: errors.New("detail")}, 400, "Bad input.", "detail"},
		{"internal error", errors.New("db password is hunter2"), 500, "Something went wrong", "hunter2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := env.srv.handle(func(http.ResponseWriter, *http.Request) error { return tc.err })
			rec := httptest.NewRecorder()
			chain(h, env.srv.requestID).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			body := rec.Body.String()
			if !strings.Contains(body, tc.wantText) {
				t.Errorf("body missing %q", tc.wantText)
			}
			if strings.Contains(body, tc.hiddenText) {
				t.Errorf("internal detail %q leaked to client", tc.hiddenText)
			}
		})
	}
	if !strings.Contains(env.logs.String(), "hunter2") {
		t.Error("internal error detail should be logged server-side")
	}
}

func TestPanicRecovery(t *testing.T) {
	env := newTestEnv(t)
	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	h := chain(panicky, env.srv.requestID, env.srv.logRequests, env.srv.recoverPanics, env.srv.securityHeaders)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `class="error-page"`) {
		t.Error("panic should render the error page")
	}
	logs := env.logs.String()
	if !strings.Contains(logs, "panic serving request") || !strings.Contains(logs, "status=500") {
		t.Errorf("panic not logged as 500:\n%s", logs)
	}
}

func TestServeAndGracefulShutdown(t *testing.T) {
	env := newTestEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- env.srv.Serve(ctx, ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("live server status = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
}
