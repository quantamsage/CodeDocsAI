# Project Status

## Phases

| Phase | Title | Status |
|---|---|---|
| 1 | Project Foundation and Architecture | ✅ Complete (approved) |
| 2 | Authentication and User Dashboard | ✅ Complete (awaiting approval) |
| 3 | Code Upload and Project Processing | Not started |
| 4 | AI Integration and Codebase Analysis | Not started |
| 5 | Documentation Generation | Not started |
| 6 | Documentation Viewer and Export | Not started |
| 7 | Interactive AI Code Assistant | Not started |
| 8 | Final Integration, Security, and Testing | Not started |

## Phase 1 — completed

- Go module `github.com/quantamsage/ai-code-documentation-tool` with a `cmd/` + `internal/` + `web/` layout.
- Configuration from environment variables, with validation (`internal/config`).
- SQLite via `modernc.org/sqlite`, with WAL, foreign keys and busy timeout (`internal/database`).
- Embedded, ordered, transactional migrations tracked in `schema_migrations`.
- Initial schema: `users`, `projects`, `project_files`, `jobs`, `analysis_results` and `documentation`.
- HTTP server with timeouts and graceful shutdown on SIGINT/SIGTERM.
- Routing on `net/http.ServeMux` method patterns. Middleware for request IDs, structured request logging (`log/slog`), panic recovery and security headers (CSP, etc.).
- Centralized error handling (`HTTPError`): user-safe messages, with details logged only.
- Template renderer with a shared layout and partials, per-page template sets, and buffered rendering.
- Responsive layout (mobile-first CSS, light and dark mode, a JS nav toggle that works without JS), a home page and an error page.
- `GET /healthz` with a database ping.

### Tests

`go test ./...` covers configuration defaults, overrides and validation; database pragmas; migration apply, idempotency, incremental runs, rollback on failure and filename validation; schema foreign keys, cascades and constraints; template isolation, escaping and failure behavior, plus parsing the embedded templates; routes, the 404 and 405 responses, static assets and traversal, security headers, request IDs, the health check (healthy and DB down), error-leak prevention, panic recovery, and live serve with graceful shutdown.

I also ran a manual smoke test with the built binary: it started, migrated a fresh DB, served `/`, `/healthz`, static assets and a 404, and applied no migrations on restart.

## Phase 2 — completed

- **Registration** with email, optional display name and password with confirmation. Emails are normalized (trimmed, lowercased) and unique regardless of case. Passwords must be 8–128 characters, not blank and not equal to the email. Errors are shown per field, and values other than passwords are kept on re-render.
- **Login and logout.** Wrong-password and unknown-email failures look the same and take about the same time. Failed logins are throttled per email and per IP (429 with `Retry-After`). Logins can return to a local `next` path, which is checked to prevent open redirects.
- **Password hashing** with argon2id in PHC format (OWASP parameters). Hashes are upgraded on login when the parameters change, and concurrent hashing is capped.
- **Server-side sessions** (migration 0002). The cookie holds a random token and the database stores only its SHA-256 hash. Sessions have an absolute lifetime and an idle timeout, both configurable. Tokens rotate on every login, rows are deleted on logout, and expired sessions are purged in the background. The cookie is `HttpOnly` and `SameSite=Lax`; in production it is also `Secure` and uses the `__Host-` prefix.
- **Middleware:** `crossOrigin` (Go's `CrossOriginProtection`) and `loadSession`. Route guards: `requireAuth` (login redirect, `no-store`, CSRF synchronizer token check) and `requireGuest`.
- **Authorization:** `internal/projects` scopes every query by owner. Another user's project returns the same 404 as a missing one.
- **Dashboard** listing each project's name (and description), languages, upload date, documentation status and last documentation update, plus summary counts and an empty state.
- **Projects:** create (validated, names unique per user regardless of case), view (a details page) and delete (a confirmation page, then a POST that cascades to files, jobs, analyses and docs). Flash messages confirm each action.
- Nav, home page and error pages adapt to the signed-in user. The responsive table becomes cards on small screens.
- New config: `APP_SESSION_LIFETIME` (default 168h) and `APP_SESSION_IDLE_TIMEOUT` (default 24h).

### Tests

`go test ./...` passes (`go vet` and `gofmt` are clean). New coverage:

- `internal/auth`: registration validation, normalization and duplicates; the stored hash is never plaintext; authentication success and failure; throttling per email and per IP, window expiry and reset after success; rehashing outdated parameters; hash format, salts, mismatch, rejection of malformed or tampered hashes, and context cancellation when hashing is saturated; session create, lookup and delete; the raw token is never stored; idle timeout with touch; absolute expiry; purge; flash; deleting a user ends their sessions.
- `internal/projects`: validation and normalization; create, get and list order; per-user name uniqueness; cross-user get and delete return `ErrNotFound`; delete cascades; the dashboard fields that later phases will fill in.
- `internal/server` (HTTP level, with a cookie-keeping test client): the full register → dashboard → logout → login flow; cookie attributes (dev and production); a one-shot flash; old cookies rejected after logout; validation and duplicate errors without echoing passwords; generic login errors; throttling with `Retry-After`; safe `next` handling; session rotation on login; idle expiry; guest and auth redirects; cross-origin POSTs rejected; CSRF missing, forged or taken from another session rejected, and the header token accepted; oversized and wrong-type forms; `no-store`; the project create → view → confirm → delete flow; validation and duplicate names; **cross-user isolation** (view, confirm-delete and delete all return 404, with no name leak, and dashboards are separate); invalid IDs; XSS escaping; dashboard metadata rendering; the signed-in nav on error pages.

I also checked the ownership tests by temporarily breaking the owner filter in the SQL. Both the store tests and the HTTP tests failed as expected.

I ran a manual smoke test with the built binary: a fresh DB migrated `[1 2]`; register → create project → dashboard → view → cross-site delete rejected (403) → delete → logout → dashboard redirects to login; the login cookie had the expected attributes; no errors were logged.

## Architectural decisions

- **Pure-Go SQLite (`modernc.org/sqlite`)** rather than `mattn/go-sqlite3`, so builds need no cgo or C toolchain. As a result, `go test -race` (which needs cgo) is not available on machines without a C compiler.
- **Standard library routing** (Go 1.22+ `ServeMux`) instead of a router dependency.
- **Embedded assets** (`embed`) so the app ships as a single binary.
- **Handlers return errors** and are rendered centrally. Feature logic will live in `internal/<area>` packages, with `internal/server` kept as a thin HTTP layer.
- **Server-generated request IDs only**, so clients can't spoof log correlation.
- **Sessions table deferred to Phase 2**, as a new migration. This is also the first real use of incremental migrations.
- **Timestamps stored as UTC RFC 3339 text** for readable, sortable values in SQLite.
- **argon2id via `golang.org/x/crypto`** (the only new dependency). The standard library has no memory-hard password hash.
- **Server-side sessions rather than signed cookies,** so logout and expiry take effect immediately and sessions can be revoked.
- **Two layers of CSRF protection:** the standard library's `CrossOriginProtection` covers every form, including anonymous ones, and a per-session synchronizer token covers authenticated state changes. The token can also be sent as the `X-CSRF-Token` header, so Phase 3's multipart uploads don't need to parse the body before the check.
- **Owner-scoped queries return "not found" rather than "forbidden",** so the existence of other users' resources isn't revealed.
- **In-memory login throttling.** It suits the single-binary deployment, but the counters reset on restart.
- **`server.New` takes functional options** (`WithAuthOptions`) so tests can inject a fast hasher and a fake clock without changing the production wiring.

## Outstanding tasks

- Phase 3: uploads (single files, multiple files and ZIP), safe extraction, language and kind detection, storage and the project explorer. Deleting a project must also remove its stored files from disk. The store currently only deletes rows, because nothing is stored on disk yet.

## Known issues / limitations

- Login throttling is in memory, so it resets on restart and isn't shared between instances.
- Registering with an email that already exists says so, which reveals that the account exists. This is a deliberate usability trade-off; login doesn't reveal it.
- There's no password reset, email verification or account settings page, because the spec doesn't require them. Users can't edit a project's name or description yet.
- The dashboard's language, upload and documentation fields show empty or "Not yet" values until Phases 3–5 fill them in.
- Templates are embedded at build time, so template changes need a restart (no hot reload).
- The race detector can't run in the current dev environment (no cgo).
