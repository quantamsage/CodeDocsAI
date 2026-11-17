package auth

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quantamsage/ai-code-documentation-tool/internal/database"
)

// fastParams keep tests quick; production uses DefaultArgon2Params.
var fastParams = Argon2Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

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

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}

func newTestService(t *testing.T) (*Service, *sql.DB, *fakeClock) {
	t.Helper()
	db := newTestDB(t)
	clock := newClock()
	svc := NewService(db, Options{
		Hasher:              NewHasher(fastParams, 4),
		SessionLifetime:     7 * 24 * time.Hour,
		IdleTimeout:         2 * time.Hour,
		MaxFailuresPerEmail: 3,
		MaxFailuresPerIP:    10,
		ThrottleWindow:      15 * time.Minute,
		Now:                 clock.Now,
	})
	return svc, db, clock
}

func register(t *testing.T, svc *Service, email, password string) User {
	t.Helper()
	u, err := svc.Register(context.Background(), RegisterInput{Email: email, Password: password, PasswordConfirm: password})
	if err != nil {
		t.Fatalf("Register(%s): %v", email, err)
	}
	return u
}

func TestRegisterInputValidation(t *testing.T) {
	cases := []struct {
		name      string
		in        RegisterInput
		wantField string // "" means valid
	}{
		{"valid", RegisterInput{Email: "a@example.com", Password: "correct horse", PasswordConfirm: "correct horse"}, ""},
		{"missing email", RegisterInput{Password: "correct horse", PasswordConfirm: "correct horse"}, "email"},
		{"bad email", RegisterInput{Email: "not-an-email", Password: "correct horse", PasswordConfirm: "correct horse"}, "email"},
		{"email with name part", RegisterInput{Email: "Bob <bob@example.com>", Password: "correct horse", PasswordConfirm: "correct horse"}, "email"},
		{"email without dot in domain", RegisterInput{Email: "bob@localhost", Password: "correct horse", PasswordConfirm: "correct horse"}, "email"},
		{"email too long", RegisterInput{Email: strings.Repeat("a", 250) + "@example.com", Password: "correct horse", PasswordConfirm: "correct horse"}, "email"},
		{"short password", RegisterInput{Email: "a@example.com", Password: "short", PasswordConfirm: "short"}, "password"},
		{"long password", RegisterInput{Email: "a@example.com", Password: strings.Repeat("x", 129), PasswordConfirm: strings.Repeat("x", 129)}, "password"},
		{"blank password", RegisterInput{Email: "a@example.com", Password: "         ", PasswordConfirm: "         "}, "password"},
		{"password equals email", RegisterInput{Email: "abc@example.com", Password: "ABC@example.com", PasswordConfirm: "ABC@example.com"}, "password"},
		{"mismatch", RegisterInput{Email: "a@example.com", Password: "correct horse", PasswordConfirm: "correct horsf"}, "password_confirm"},
		{"name too long", RegisterInput{Email: "a@example.com", DisplayName: strings.Repeat("n", 81), Password: "correct horse", PasswordConfirm: "correct horse"}, "display_name"},
		{"name control chars", RegisterInput{Email: "a@example.com", DisplayName: "bad\x00name", Password: "correct horse", PasswordConfirm: "correct horse"}, "display_name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			err := in.Validate()
			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var verr ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("err = %v, want ValidationError", err)
			}
			if _, ok := verr[tc.wantField]; !ok {
				t.Errorf("errors = %v, want field %q", verr, tc.wantField)
			}
		})
	}
}

func TestRegisterNormalizesAndDefaultsName(t *testing.T) {
	svc, _, _ := newTestService(t)
	u := register(t, svc, "  Alice@Example.COM ", "correct horse")
	if u.Email != "alice@example.com" {
		t.Errorf("email = %q", u.Email)
	}
	if u.DisplayName != "alice" {
		t.Errorf("display name = %q, want default from email", u.DisplayName)
	}
	if u.ID == 0 || u.CreatedAt.IsZero() {
		t.Errorf("user not populated: %+v", u)
	}
}

func TestRegisterStoresHashNotPassword(t *testing.T) {
	svc, db, _ := newTestService(t)
	u := register(t, svc, "alice@example.com", "correct horse battery")

	var hash string
	if err := db.QueryRow(`SELECT password_hash FROM users WHERE id = ?`, u.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hash, "correct horse") || !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("stored hash looks wrong: %q", hash)
	}
}

func TestRegisterDuplicateEmailCaseInsensitive(t *testing.T) {
	svc, _, _ := newTestService(t)
	register(t, svc, "alice@example.com", "correct horse")
	_, err := svc.Register(context.Background(), RegisterInput{Email: "ALICE@example.com", Password: "another pass", PasswordConfirm: "another pass"})
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("err = %v, want ErrEmailTaken", err)
	}
}

func TestAuthenticate(t *testing.T) {
	svc, _, clock := newTestService(t)
	ctx := context.Background()
	u := register(t, svc, "alice@example.com", "correct horse")

	got, err := svc.Authenticate(ctx, " ALICE@example.com", "correct horse", "10.0.0.1")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("got user %d, want %d", got.ID, u.ID)
	}
	if !got.LastLoginAt.Equal(clock.Now()) {
		t.Errorf("last login = %v, want %v", got.LastLoginAt, clock.Now())
	}

	for _, tc := range []struct{ email, password string }{
		{"alice@example.com", "wrong password"},
		{"nobody@example.com", "correct horse"},
		{"", "correct horse"},
		{"alice@example.com", ""},
	} {
		if _, err := svc.Authenticate(ctx, tc.email, tc.password, "10.0.0.2"); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("Authenticate(%q, %q) err = %v, want ErrInvalidCredentials", tc.email, tc.password, err)
		}
	}
}

func TestAuthenticateThrottlesPerEmail(t *testing.T) {
	svc, _, clock := newTestService(t)
	ctx := context.Background()
	register(t, svc, "alice@example.com", "correct horse")

	for i := 0; i < 3; i++ {
		if _, err := svc.Authenticate(ctx, "alice@example.com", "wrong", "10.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// Blocked now, even with the right password and from another IP.
	_, err := svc.Authenticate(ctx, "alice@example.com", "correct horse", "10.9.9.9")
	var throttled *ThrottledError
	if !errors.As(err, &throttled) {
		t.Fatalf("err = %v, want ThrottledError", err)
	}
	if throttled.RetryAfter <= 0 || throttled.RetryAfter > 15*time.Minute {
		t.Errorf("RetryAfter = %v", throttled.RetryAfter)
	}

	clock.Advance(15 * time.Minute)
	if _, err := svc.Authenticate(ctx, "alice@example.com", "correct horse", "10.0.0.1"); err != nil {
		t.Fatalf("after window: %v", err)
	}
}

func TestAuthenticateThrottlesPerIP(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	register(t, svc, "victim@example.com", "correct horse")

	// Spraying many different emails from one IP trips the IP limit (10).
	for i := 0; i < 10; i++ {
		svc.Authenticate(ctx, "user"+string(rune('a'+i))+"@example.com", "guess", "203.0.113.5") //nolint:errcheck
	}
	_, err := svc.Authenticate(ctx, "victim@example.com", "correct horse", "203.0.113.5")
	var throttled *ThrottledError
	if !errors.As(err, &throttled) {
		t.Fatalf("err = %v, want ThrottledError", err)
	}
	// Other IPs are unaffected.
	if _, err := svc.Authenticate(ctx, "victim@example.com", "correct horse", "198.51.100.1"); err != nil {
		t.Fatalf("other IP: %v", err)
	}
}

func TestSuccessfulLoginResetsEmailFailures(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	register(t, svc, "alice@example.com", "correct horse")

	for round := 0; round < 3; round++ {
		for i := 0; i < 2; i++ {
			svc.Authenticate(ctx, "alice@example.com", "wrong", "10.0.0.1") //nolint:errcheck
		}
		if _, err := svc.Authenticate(ctx, "alice@example.com", "correct horse", "10.0.0.1"); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
}

func TestAuthenticateRehashesOutdatedParams(t *testing.T) {
	svc, db, _ := newTestService(t)
	ctx := context.Background()
	u := register(t, svc, "alice@example.com", "correct horse")

	// Simulate a hash created with older, different parameters.
	old := NewHasher(Argon2Params{Memory: 32, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}, 1)
	oldHash, err := old.Hash(ctx, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, oldHash, u.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Authenticate(ctx, "alice@example.com", "correct horse", "10.0.0.1"); err != nil {
		t.Fatal(err)
	}
	var stored string
	db.QueryRow(`SELECT password_hash FROM users WHERE id = ?`, u.ID).Scan(&stored) //nolint:errcheck
	if stored == oldHash || !strings.Contains(stored, "m=64,") {
		t.Errorf("hash not upgraded: %q", stored)
	}
	if _, err := svc.Authenticate(ctx, "alice@example.com", "correct horse", "10.0.0.1"); err != nil {
		t.Fatalf("login after rehash: %v", err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	svc, db, _ := newTestService(t)
	ctx := context.Background()
	u := register(t, svc, "alice@example.com", "correct horse")

	token, sess, err := svc.CreateSession(ctx, u.ID, "test-agent", "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(token) < 40 || sess.CSRFToken == "" || sess.CSRFToken == token {
		t.Fatalf("weak tokens: token=%q csrf=%q", token, sess.CSRFToken)
	}

	// Only the hash of the token is stored.
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash = ?`, token).Scan(&n) //nolint:errcheck
	if n != 0 {
		t.Error("raw session token stored in database")
	}

	gotSess, gotUser, err := svc.LookupSession(ctx, token)
	if err != nil {
		t.Fatalf("LookupSession: %v", err)
	}
	if gotUser.ID != u.ID || gotSess.ID != sess.ID || gotSess.CSRFToken != sess.CSRFToken {
		t.Errorf("lookup mismatch: %+v %+v", gotSess, gotUser)
	}

	for _, bad := range []string{"", "nope", token + "x", strings.Repeat("a", 500)} {
		if _, _, err := svc.LookupSession(ctx, bad); !errors.Is(err, ErrNoSession) {
			t.Errorf("LookupSession(%q) err = %v", bad, err)
		}
	}

	if err := svc.DeleteSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.LookupSession(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Errorf("after delete err = %v", err)
	}
	if err := svc.DeleteSession(ctx, token); err != nil {
		t.Errorf("deleting twice: %v", err)
	}
}

func TestSessionIdleTimeoutAndTouch(t *testing.T) {
	svc, _, clock := newTestService(t)
	ctx := context.Background()
	u := register(t, svc, "alice@example.com", "correct horse")
	token, _, _ := svc.CreateSession(ctx, u.ID, "", "")

	// Activity within the idle window keeps the session alive indefinitely
	// (up to the absolute lifetime).
	for i := 0; i < 5; i++ {
		clock.Advance(90 * time.Minute)
		if _, _, err := svc.LookupSession(ctx, token); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}

	clock.Advance(2*time.Hour + time.Second)
	if _, _, err := svc.LookupSession(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("idle session err = %v, want ErrNoSession", err)
	}
}

func TestSessionAbsoluteExpiry(t *testing.T) {
	svc, db, clock := newTestService(t)
	ctx := context.Background()
	u := register(t, svc, "alice@example.com", "correct horse")
	token, _, _ := svc.CreateSession(ctx, u.ID, "", "")

	// Keep it active, but past the 7-day absolute lifetime it must end.
	for elapsed := time.Duration(0); elapsed < 7*24*time.Hour; elapsed += time.Hour {
		clock.Advance(time.Hour)
		svc.LookupSession(ctx, token) //nolint:errcheck
	}
	if _, _, err := svc.LookupSession(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("expired session err = %v", err)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n) //nolint:errcheck
	if n != 0 {
		t.Errorf("expired session row not deleted (%d rows)", n)
	}
}

func TestPurgeExpired(t *testing.T) {
	svc, db, clock := newTestService(t)
	ctx := context.Background()
	u := register(t, svc, "alice@example.com", "correct horse")
	svc.CreateSession(ctx, u.ID, "", "") //nolint:errcheck
	clock.Advance(3 * time.Hour)
	fresh, _, _ := svc.CreateSession(ctx, u.ID, "", "")

	n, err := svc.PurgeExpired(ctx)
	if err != nil || n != 1 {
		t.Fatalf("purged %d, err %v; want 1", n, err)
	}
	if _, _, err := svc.LookupSession(ctx, fresh); err != nil {
		t.Errorf("fresh session purged: %v", err)
	}
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count) //nolint:errcheck
	if count != 1 {
		t.Errorf("sessions remaining = %d", count)
	}
}

func TestFlash(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	u := register(t, svc, "alice@example.com", "correct horse")
	token, sess, _ := svc.CreateSession(ctx, u.ID, "", "")

	if err := svc.SetFlash(ctx, sess.ID, "Saved."); err != nil {
		t.Fatal(err)
	}
	got, _, _ := svc.LookupSession(ctx, token)
	if got.Flash != "Saved." {
		t.Errorf("flash = %q", got.Flash)
	}
	svc.ClearFlash(ctx, sess.ID) //nolint:errcheck
	got, _, _ = svc.LookupSession(ctx, token)
	if got.Flash != "" {
		t.Errorf("flash after clear = %q", got.Flash)
	}
}

func TestDeletingUserEndsSessions(t *testing.T) {
	svc, db, _ := newTestService(t)
	ctx := context.Background()
	u := register(t, svc, "alice@example.com", "correct horse")
	token, _, _ := svc.CreateSession(ctx, u.ID, "", "")

	if _, err := db.Exec(`DELETE FROM users WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.LookupSession(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Errorf("err = %v, want ErrNoSession", err)
	}
}
