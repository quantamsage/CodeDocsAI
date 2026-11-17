// Package auth implements user accounts, password verification, login
// throttling and server-side sessions.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Errors returned by Service.
var (
	ErrInvalidCredentials = errors.New("auth: invalid email or password")
	ErrEmailTaken         = errors.New("auth: email already registered")
	ErrNoSession          = errors.New("auth: no valid session")
)

// ThrottledError is returned when too many failed logins were made recently.
type ThrottledError struct{ RetryAfter time.Duration }

func (e *ThrottledError) Error() string {
	return fmt.Sprintf("auth: too many failed login attempts; retry after %s", e.RetryAfter)
}

// ValidationError maps form field names to user-facing messages.
type ValidationError map[string]string

func (v ValidationError) Error() string {
	parts := make([]string, 0, len(v))
	for f, m := range v {
		parts = append(parts, f+": "+m)
	}
	return "auth: invalid input: " + strings.Join(parts, "; ")
}

// Input limits.
const (
	MinPasswordLen = 8
	MaxPasswordLen = 128
	MaxEmailLen    = 254
	MaxNameLen     = 80
)

// User is an account.
type User struct {
	ID          int64
	Email       string
	DisplayName string
	CreatedAt   time.Time
	LastLoginAt time.Time // zero if never logged in
}

// Session is a server-side login session.
type Session struct {
	ID         int64
	UserID     int64
	CSRFToken  string
	Flash      string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// Options configures a Service. Zero values select defaults.
type Options struct {
	// Hasher hashes passwords; defaults to argon2id with DefaultArgon2Params.
	Hasher *Hasher
	// SessionLifetime is the absolute session lifetime (default 7 days).
	SessionLifetime time.Duration
	// IdleTimeout ends sessions not used for this long (default 24h).
	IdleTimeout time.Duration
	// MaxFailuresPerEmail and MaxFailuresPerIP limit failed logins within
	// ThrottleWindow (defaults 5, 50 and 15 minutes).
	MaxFailuresPerEmail int
	MaxFailuresPerIP    int
	ThrottleWindow      time.Duration
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
	// Log receives background maintenance messages.
	Log *slog.Logger
}

// Service manages accounts and sessions.
type Service struct {
	db       *sql.DB
	hasher   *Hasher
	lifetime time.Duration
	idle     time.Duration
	now      func() time.Time
	log      *slog.Logger

	emailThrottle *Throttle
	ipThrottle    *Throttle

	dummyOnce sync.Once
	dummyHash string
}

// touchInterval limits how often last_seen_at is written for an active session.
const touchInterval = time.Minute

// NewService returns a Service backed by db.
func NewService(db *sql.DB, opts Options) *Service {
	if opts.Hasher == nil {
		opts.Hasher = NewHasher(DefaultArgon2Params, 4)
	}
	if opts.SessionLifetime <= 0 {
		opts.SessionLifetime = 7 * 24 * time.Hour
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 24 * time.Hour
	}
	if opts.MaxFailuresPerEmail <= 0 {
		opts.MaxFailuresPerEmail = 5
	}
	if opts.MaxFailuresPerIP <= 0 {
		opts.MaxFailuresPerIP = 50
	}
	if opts.ThrottleWindow <= 0 {
		opts.ThrottleWindow = 15 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Service{
		db:            db,
		hasher:        opts.Hasher,
		lifetime:      opts.SessionLifetime,
		idle:          opts.IdleTimeout,
		now:           opts.Now,
		log:           opts.Log,
		emailThrottle: NewThrottle(opts.MaxFailuresPerEmail, opts.ThrottleWindow, opts.Now),
		ipThrottle:    NewThrottle(opts.MaxFailuresPerIP, opts.ThrottleWindow, opts.Now),
	}
}

// SessionLifetime returns the absolute session lifetime (for cookie Max-Age).
func (s *Service) SessionLifetime() time.Duration { return s.lifetime }

// NormalizeEmail trims and lower-cases an email address.
func NormalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// RegisterInput is the data needed to create an account.
type RegisterInput struct {
	Email           string
	DisplayName     string
	Password        string
	PasswordConfirm string
}

// Validate checks the input and returns a ValidationError (or nil).
func (in *RegisterInput) Validate() error {
	in.Email = NormalizeEmail(in.Email)
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	errs := ValidationError{}

	switch addr, err := mail.ParseAddress(in.Email); {
	case in.Email == "":
		errs["email"] = "Enter your email address."
	case len(in.Email) > MaxEmailLen:
		errs["email"] = fmt.Sprintf("Email must be at most %d characters.", MaxEmailLen)
	case err != nil || addr.Address != in.Email || !strings.Contains(in.Email[strings.LastIndexByte(in.Email, '@')+1:], "."):
		errs["email"] = "Enter a valid email address, like name@example.com."
	}

	if n := utf8.RuneCountInString(in.DisplayName); n > MaxNameLen {
		errs["display_name"] = fmt.Sprintf("Name must be at most %d characters.", MaxNameLen)
	} else if hasControl(in.DisplayName) {
		errs["display_name"] = "Name contains characters that are not allowed."
	}

	switch n := utf8.RuneCountInString(in.Password); {
	case !utf8.ValidString(in.Password):
		errs["password"] = "Password contains invalid characters."
	case n < MinPasswordLen:
		errs["password"] = fmt.Sprintf("Password must be at least %d characters.", MinPasswordLen)
	case n > MaxPasswordLen:
		errs["password"] = fmt.Sprintf("Password must be at most %d characters.", MaxPasswordLen)
	case strings.TrimSpace(in.Password) == "":
		errs["password"] = "Password cannot be only spaces."
	case strings.EqualFold(in.Password, in.Email):
		errs["password"] = "Password must not be the same as your email."
	}
	if _, bad := errs["password"]; !bad && in.Password != in.PasswordConfirm {
		errs["password_confirm"] = "Passwords do not match."
	}

	if len(errs) > 0 {
		return errs
	}
	if in.DisplayName == "" {
		in.DisplayName = in.Email[:strings.IndexByte(in.Email, '@')]
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// Register validates in and creates an account.
func (s *Service) Register(ctx context.Context, in RegisterInput) (User, error) {
	if err := in.Validate(); err != nil {
		return User{}, err
	}
	hash, err := s.hasher.Hash(ctx, in.Password)
	if err != nil {
		return User{}, err
	}
	now := fmtTime(s.now())
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (email, display_name, password_hash, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		in.Email, in.DisplayName, hash, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrEmailTaken
		}
		return User{}, fmt.Errorf("insert user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	return s.UserByID(ctx, id)
}

// Authenticate checks an email and password. remoteIP is used for throttling.
// It returns ErrInvalidCredentials for an unknown email or a wrong password
// (indistinguishably, and in similar time) and *ThrottledError when blocked.
func (s *Service) Authenticate(ctx context.Context, email, password, remoteIP string) (User, error) {
	email = NormalizeEmail(email)
	emailKey, ipKey := "email:"+email, "ip:"+remoteIP

	for _, t := range []struct {
		th  *Throttle
		key string
	}{{s.emailThrottle, emailKey}, {s.ipThrottle, ipKey}} {
		if blocked, wait := t.th.Blocked(t.key); blocked {
			return User{}, &ThrottledError{RetryAfter: wait}
		}
	}

	fail := func() (User, error) {
		s.emailThrottle.Fail(emailKey)
		s.ipThrottle.Fail(ipKey)
		return User{}, ErrInvalidCredentials
	}

	if email == "" || password == "" || len(password) > 4*MaxPasswordLen {
		return fail()
	}

	var id int64
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT id, password_hash FROM users WHERE email = ?`, email).Scan(&id, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		// Spend the same work as a real check so response time does not reveal
		// whether the email is registered.
		s.hasher.Verify(ctx, password, s.dummy(ctx)) //nolint:errcheck
		return fail()
	}
	if err != nil {
		return User{}, fmt.Errorf("query user: %w", err)
	}

	needsRehash, err := s.hasher.Verify(ctx, password, hash)
	if errors.Is(err, ErrMismatchedPassword) {
		return fail()
	}
	if err != nil {
		return User{}, fmt.Errorf("verify password for user %d: %w", id, err)
	}
	s.emailThrottle.Reset(emailKey)

	now := fmtTime(s.now())
	if needsRehash {
		if newHash, err := s.hasher.Hash(ctx, password); err == nil {
			if _, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, newHash, now, id); err != nil {
				s.log.Warn("rehash password", slog.Int64("user_id", id), slog.String("error", err.Error()))
			}
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, now, id); err != nil {
		return User{}, fmt.Errorf("record login: %w", err)
	}
	return s.UserByID(ctx, id)
}

func (s *Service) dummy(ctx context.Context) string {
	s.dummyOnce.Do(func() {
		h, err := s.hasher.Hash(ctx, "dummy password for timing equalization")
		if err == nil {
			s.dummyHash = h
		}
	})
	return s.dummyHash
}

// UserByID loads a user.
func (s *Service) UserByID(ctx context.Context, id int64) (User, error) {
	var u User
	var created string
	var lastLogin sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, email, display_name, created_at, last_login_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Email, &u.DisplayName, &created, &lastLogin)
	if err != nil {
		return User{}, fmt.Errorf("load user %d: %w", id, err)
	}
	u.CreatedAt = parseTime(created)
	if lastLogin.Valid {
		u.LastLoginAt = parseTime(lastLogin.String)
	}
	return u, nil
}

// CreateSession starts a session for userID and returns the cookie token. The
// token is returned only here; the database keeps its SHA-256 hash.
func (s *Service) CreateSession(ctx context.Context, userID int64, userAgent, remoteAddr string) (string, Session, error) {
	token, err := randomToken(32)
	if err != nil {
		return "", Session{}, err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return "", Session{}, err
	}
	now := s.now().UTC().Truncate(time.Second)
	sess := Session{UserID: userID, CSRFToken: csrf, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(s.lifetime)}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, csrf_token, user_agent, remote_addr, created_at, last_seen_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		hashToken(token), userID, csrf, truncate(userAgent, 512), truncate(remoteAddr, 64),
		fmtTime(now), fmtTime(now), fmtTime(sess.ExpiresAt))
	if err != nil {
		return "", Session{}, fmt.Errorf("insert session: %w", err)
	}
	if sess.ID, err = res.LastInsertId(); err != nil {
		return "", Session{}, err
	}
	return token, sess, nil
}

// LookupSession returns the session and user for a cookie token. Expired or
// idle sessions are deleted and reported as ErrNoSession.
func (s *Service) LookupSession(ctx context.Context, token string) (Session, User, error) {
	if token == "" || len(token) > 128 {
		return Session{}, User{}, ErrNoSession
	}
	var sess Session
	var created, lastSeen, expires string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, csrf_token, flash, created_at, last_seen_at, expires_at FROM sessions WHERE token_hash = ?`,
		hashToken(token)).Scan(&sess.ID, &sess.UserID, &sess.CSRFToken, &sess.Flash, &created, &lastSeen, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, User{}, ErrNoSession
	}
	if err != nil {
		return Session{}, User{}, fmt.Errorf("query session: %w", err)
	}
	sess.CreatedAt, sess.LastSeenAt, sess.ExpiresAt = parseTime(created), parseTime(lastSeen), parseTime(expires)

	now := s.now()
	if !now.Before(sess.ExpiresAt) || !now.Before(sess.LastSeenAt.Add(s.idle)) {
		if err := s.deleteSessionID(ctx, sess.ID); err != nil {
			return Session{}, User{}, err
		}
		return Session{}, User{}, ErrNoSession
	}
	if now.Sub(sess.LastSeenAt) >= touchInterval {
		if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id = ?`, fmtTime(now), sess.ID); err != nil {
			return Session{}, User{}, fmt.Errorf("touch session: %w", err)
		}
		sess.LastSeenAt = now.UTC().Truncate(time.Second)
	}

	user, err := s.UserByID(ctx, sess.UserID)
	if err != nil {
		return Session{}, User{}, err
	}
	return sess, user, nil
}

// DeleteSession ends the session identified by a cookie token. Unknown tokens
// are not an error.
func (s *Service) DeleteSession(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
	return err
}

func (s *Service) deleteSessionID(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// SetFlash stores a one-shot message on the session.
func (s *Service) SetFlash(ctx context.Context, sessionID int64, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET flash = ? WHERE id = ?`, truncate(msg, 500), sessionID)
	return err
}

// ClearFlash removes the session's flash message once it has been shown.
func (s *Service) ClearFlash(ctx context.Context, sessionID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET flash = '' WHERE id = ?`, sessionID)
	return err
}

// PurgeExpired deletes sessions past their absolute or idle expiry.
func (s *Service) PurgeExpired(ctx context.Context) (int64, error) {
	now := s.now()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at <= ? OR last_seen_at <= ?`,
		fmtTime(now), fmtTime(now.Add(-s.idle)))
	if err != nil {
		return 0, fmt.Errorf("purge sessions: %w", err)
	}
	return res.RowsAffected()
}

// RunPurge calls PurgeExpired every interval until ctx is canceled.
func (s *Service) RunPurge(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.PurgeExpired(ctx)
			if err != nil && ctx.Err() == nil {
				s.log.Error("purge expired sessions", slog.String("error", err.Error()))
			} else if n > 0 {
				s.log.Info("purged expired sessions", slog.Int64("count", n))
			}
		}
	}
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// fmtTime formats t as the UTC RFC 3339 text used throughout the schema.
func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
