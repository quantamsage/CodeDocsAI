# Project Status

## Phases

| Phase | Title | Status |
|---|---|---|
| 1 | Project Foundation and Architecture | ✅ Complete (approved) |
| 2 | Authentication and User Dashboard | ✅ Complete (approved) |
| 3 | Code Upload and Project Processing | ✅ Complete (awaiting approval) |
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

## Phase 3 — completed

- **Uploads** (`GET/POST /projects/{id}/upload`): individual files, multiple files, a whole folder (with its relative paths, via `webkitdirectory`) and `.zip` archives, in any combination. Optional target folder, and a choice between adding to (and overwriting within) the project or replacing all of its files. Each upload ends with a summary flash message: added, replaced and removed counts, plus skipped files with reasons.
- **Safe ZIP extraction:** rejects path traversal, absolute and drive-letter paths, symlinks and special files, malicious names (control characters, reserved device names, invalid characters) and ZIP bombs (compression ratio, total uncompressed size, entry count). Skips encrypted entries and unsupported compression methods. Never trusts declared sizes. Preserves directory structure and strips a single wrapping folder.
- **File processing:** language detection for Go, Python, JavaScript, TypeScript, Java, C, C++, Rust, PHP, Ruby and C#, plus other common languages. Identifies source files, configuration files, dependency manifests (by exact name, such as `go.mod`, `package.json`, `requirements.txt`, `Cargo.toml`, `pom.xml`, `*.csproj`) and documentation. Skips binaries and executables (by extension and magic number), unsupported types, nested archives, likely secrets and dependency/VCS folders.
- **Configurable limits:** `APP_UPLOAD_MAX_REQUEST_SIZE`, `APP_UPLOAD_MAX_FILE_SIZE`, `APP_UPLOAD_MAX_FILES`, `APP_UPLOAD_MAX_TOTAL_SIZE`, `APP_UPLOAD_MAX_COMPRESSION_RATIO` and `APP_UPLOAD_TIMEOUT`. Sizes accept `KiB`/`MiB`/`GiB` suffixes.
- **Secure storage:** contents go under server-generated names in `data/uploads/<project>/`, with paths kept only as metadata. Writes are transactional, and old blobs are cleaned up on overwrite, replace, failure and project deletion.
- **Project stats:** the dashboard and project page now show the detected languages, file count, size, upload date and an "uploaded / not analyzed" status.
- **Project explorer:** a collapsible directory tree (works without JS) with per-directory file counts and sizes, file-kind counts, and a file viewer with line numbers, metadata (language, kind, size, SHA-256, upload time), breadcrumbs and a sidebar tree.
- **CSRF for streamed uploads:** the token is read from the first multipart part, or from the `X-CSRF-Token` header. Multipart is accepted only on the upload route.
- **Progressive enhancement:** the upload page JS shows the selected file count and size, warns when the request limit would be exceeded, drops `node_modules`/`.git`/… from folder selections before sending, and disables the button while uploading. Without JS the form still works.

### Tests

`go test ./...` passes, and `go vet` and `gofmt` are clean. New coverage:

- `internal/files`: path cleaning (accepted and rejected forms, including traversal, absolute, drive, control characters, reserved names, length and depth); classification of every required language plus manifests, config and docs; rejection of unsupported types; secret, ignored-dir and system-file detection; text, binary and executable sniffing; ZIP extraction preserving structure and stripping the common root; skipping each unsupported category with the right reason; rejecting traversal, absolute, drive, control-character, reserved-name, symlink and mixed archives; ZIP bomb detection; total-size, file-count and entry-count limits; corrupt archives; abort cleanup; blob store path validation, truncation and project removal; and the store's commit stats and language breakdown, merge, overwrite, replace, target folder, limit and ownership rejections (with no changes and no leftover blobs), owner-scoped reads, project data removal, tree building and kind counts.
- `internal/server`: uploading single files, multiple files plus a folder, and a ZIP (structure preserved, `.env` skipped, uploaded HTML shown escaped); every rejection path (422 and nothing stored); CSRF (missing, forged, not first, sent as a file, header alternative, multipart refused on other routes); sign-in required; cross-user isolation for the upload form, the upload POST and the file view; replace and target folder; the request-size limit (413) and the project file limit; project deletion removing stored files; file URLs with spaces, `#` and encoded traversal; empty files; line splitting.
- `internal/config`: upload limit defaults, overrides, validation and size parsing.

I also ran a manual smoke test with the built binary over real HTTP (curl): register → create project → upload a ZIP of this repository's `internal/` folder plus `go.mod` → 40 files stored with their structure, Go and SQL detected on the dashboard, and the file viewer returned 200, with no errors logged.

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
- **Uploaded content lives on disk, not in SQLite.** It goes under random names in `data/uploads/<project>/`, with only metadata in `project_files`. User-supplied paths can never choose a disk location, and the database stays small.
- **Streamed multipart parsing,** with the CSRF token as the first part, so uploads never need to be buffered and the form works without JavaScript.
- **Skip rather than reject** for individual unsupported files (reported to the user), but **reject the whole upload** for anything unsafe (path traversal, symlinks, ZIP bombs) or over a limit. Real projects contain images and lock files, but an unsafe archive should never be partially imported.
- **Likely secrets are never stored** (`.env`, private keys), so they can't leak into Phase 4 prompts or exports.
- **Any upload sets the project status to `uploaded`,** because earlier analysis no longer matches the code.

## Outstanding tasks

- Phase 4: AI provider interface, structural parsing, the batched analysis pipeline and background jobs. When analysis is running, uploads should be blocked or cancel the job; Phase 3 has no jobs yet, so it always resets the status to `uploaded`.

## Known issues / limitations

- Login throttling is in memory, so it resets on restart and isn't shared between instances.
- Registering with an email that already exists says so, which reveals that the account exists. This is a deliberate usability trade-off; login doesn't reveal it.
- There's no password reset, email verification or account settings page, because the spec doesn't require them. Users can't edit a project's name or description yet.
- The dashboard's documentation fields show "Not yet" values until Phases 4–5 fill them in.
- Uploads are processed within the request (no progress bar beyond the button state). Large ZIPs take as long as validation and storage need, bounded by `APP_UPLOAD_TIMEOUT`.
- If the process crashes between writing blobs and committing the transaction, those blobs are orphaned on disk. Nothing references them, and there's no garbage collector yet.
- Only UTF-8 (or ASCII) text is accepted. Source files in other encodings, such as UTF-16 or Latin-1, are skipped as "not a UTF-8 text file".
- `.h` headers are always counted as C, even in C++ projects.
- The explorer has no per-file delete or rename. Re-upload a file to overwrite it, or use "Replace all existing project files".
- Folder uploads rely on the `webkitdirectory` attribute, which the major browsers support. Without JavaScript, files in `node_modules`/`.git` are still sent (and then skipped by the server), so very large folders can hit the request size limit; zipping the project avoids this.
- Shortened or nested archive formats (`.tar.gz`, `.7z`, `.rar`) aren't extracted; only `.zip` is supported.
- Templates are embedded at build time, so template changes need a restart (no hot reload).
- The race detector can't run in the current dev environment (no cgo).
