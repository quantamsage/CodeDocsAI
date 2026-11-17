package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestProjectCreateViewDelete(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	c.register("alice@example.com", "correct horse")

	wantStatus(t, c.get("/projects/new"), http.StatusOK)

	rec := c.postCSRF("/projects", url.Values{"name": {"Payments API"}, "description": {"Handles card payments."}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create status = %d\n%s", rec.Code, rec.Body)
	}
	path := rec.Header().Get("Location")

	rec = c.get(path)
	wantStatus(t, rec, http.StatusOK)
	wantBody(t, rec, "<h1>Payments API</h1>", "Handles card payments.", "Project “Payments API” created.", "No files uploaded", path+"/delete")

	rec = c.get("/dashboard")
	wantBody(t, rec, `href="`+path+`"`, "Payments API", "No files uploaded", "Not yet", "Never")
	if strings.Contains(rec.Body.String(), "No projects yet") {
		t.Error("dashboard shows empty state with a project present")
	}

	rec = c.get(path + "/delete")
	wantStatus(t, rec, http.StatusOK)
	wantBody(t, rec, "Delete “Payments API”?", `action="`+path+`/delete"`, `name="csrf_token"`)

	wantRedirect(t, c.postCSRF(path+"/delete", nil), "/dashboard")
	wantBody(t, c.get("/dashboard"), "Project “Payments API” deleted.", "No projects yet")
	wantStatus(t, c.get(path), http.StatusNotFound)
	wantStatus(t, c.postCSRF(path+"/delete", nil), http.StatusNotFound)
}

func TestProjectValidationAndDuplicates(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	c.register("alice@example.com", "correct horse")

	rec := c.postCSRF("/projects", url.Values{"name": {"   "}, "description": {"kept"}})
	wantStatus(t, rec, http.StatusUnprocessableEntity)
	wantBody(t, rec, "Enter a project name.", ">kept</textarea>")

	rec = c.postCSRF("/projects", url.Values{"name": {strings.Repeat("x", 101)}})
	wantStatus(t, rec, http.StatusUnprocessableEntity)
	wantBody(t, rec, "at most 100 characters")

	c.createProject("Shop")
	rec = c.postCSRF("/projects", url.Values{"name": {"shop"}})
	wantStatus(t, rec, http.StatusConflict)
	wantBody(t, rec, "already have a project with this name")
}

func TestProjectOwnershipIsolation(t *testing.T) {
	env := newTestEnv(t)
	alice := env.client(t)
	alice.register("alice@example.com", "correct horse")
	bob := env.client(t)
	bob.register("bob@example.com", "correct horse")

	alicePath := alice.createProject("Alice Secret Project")
	bobPath := bob.createProject("Bob Project")

	// Bob cannot see, confirm-delete or delete Alice's project, and gets the
	// same 404 as for a project that does not exist.
	for _, path := range []string{alicePath, alicePath + "/delete", "/projects/999999"} {
		rec := bob.get(path)
		wantStatus(t, rec, http.StatusNotFound)
		if strings.Contains(rec.Body.String(), "Alice Secret Project") {
			t.Errorf("%s leaked Alice's project name", path)
		}
	}
	wantStatus(t, bob.postCSRF(alicePath+"/delete", nil), http.StatusNotFound)

	// Alice's project survives, and each dashboard lists only its owner's projects.
	wantStatus(t, alice.get(alicePath), http.StatusOK)
	aliceDash, bobDash := alice.get("/dashboard").Body.String(), bob.get("/dashboard").Body.String()
	if !strings.Contains(aliceDash, "Alice Secret Project") || strings.Contains(aliceDash, "Bob Project") {
		t.Error("alice's dashboard shows the wrong projects")
	}
	if !strings.Contains(bobDash, "Bob Project") || strings.Contains(bobDash, "Alice Secret Project") {
		t.Error("bob's dashboard shows the wrong projects")
	}

	// Alice can still delete her own project; Bob's is untouched.
	wantRedirect(t, alice.postCSRF(alicePath+"/delete", nil), "/dashboard")
	wantStatus(t, bob.get(bobPath), http.StatusOK)
}

func TestInvalidProjectIDs(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	c.register("alice@example.com", "correct horse")
	for _, path := range []string{"/projects/abc", "/projects/0", "/projects/-1", "/projects/99999999999999999999"} {
		wantStatus(t, c.get(path), http.StatusNotFound)
	}
}

func TestProjectContentIsEscaped(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	c.register("alice@example.com", "correct horse")

	path := c.createProject(`<script>alert("x")</script>`)
	for _, p := range []string{path, "/dashboard"} {
		body := c.get(p).Body.String()
		if strings.Contains(body, `<script>alert(`) {
			t.Errorf("%s: unescaped project name rendered", p)
		}
		if !strings.Contains(body, "&lt;script&gt;") {
			t.Errorf("%s: escaped name not found", p)
		}
	}
}

func TestDashboardShowsProjectMetadata(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	c.register("alice@example.com", "correct horse")
	path := c.createProject("Documented One")
	c.createProject("Fresh One")

	// Simulate the state later phases (upload, analysis, docs) produce.
	_, err := env.db.Exec(`UPDATE projects SET status = 'documented', primary_language = 'Go',
		languages_json = '{"Go": 10, "SQL": 3, "JavaScript": 2, "CSS": 1}', file_count = 16, total_size_bytes = 20480,
		uploaded_at = '2026-02-01T10:00:00Z', documentation_updated_at = '2026-02-03T09:15:00Z'
		WHERE name = 'Documented One'`)
	if err != nil {
		t.Fatal(err)
	}

	rec := c.get("/dashboard")
	wantBody(t, rec,
		`<span class="tag">Go</span>`, `<span class="tag">SQL</span>`, "+1 more",
		"1 Feb 2026, 10:00 UTC", `datetime="2026-02-01T10:00:00Z"`,
		"3 Feb 2026, 09:15 UTC", "status-documented", ">Documented<",
		"Fresh One", "No files uploaded",
		`<span class="stat-value">2</span><span class="stat-label">Projects</span>`,
		`<span class="stat-value">1</span><span class="stat-label">Documented</span>`,
		`<span class="stat-value">16</span><span class="stat-label">Files uploaded</span>`,
	)

	rec = c.get(path)
	wantBody(t, rec, "Go <span class=\"muted\">(10)</span>", "20.0 KiB", "3 Feb 2026, 09:15 UTC")
}

func TestErrorPageKeepsSignedInNav(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	c.register("alice@example.com", "correct horse")
	rec := c.get("/no-such-page")
	wantStatus(t, rec, http.StatusNotFound)
	wantBody(t, rec, `action="/logout"`, `href="/dashboard"`)
}
