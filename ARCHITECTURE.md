# Architecture

CodeDocs AI is a single Go binary. It serves server-rendered HTML pages, stores its data in SQLite, and (from Phase 4 on) calls an LLM API through a provider interface. Templates and static assets are embedded, so a deployment is the binary plus a data directory.

## Layout

```
cmd/server/            entry point: config → logger → DB open + migrate → HTTP server
internal/config/       environment-variable configuration (LoadFrom is injectable for tests)
internal/database/     SQLite connection (Open) and embedded migrations (Migrate)
  migrations/          NNNN_description.sql, applied in order, each in its own transaction
internal/auth/         accounts, argon2id password hashing, login throttling, server-side sessions
internal/projects/     owner-scoped project store (create, get, list, delete)
internal/view/         HTML template renderer (layout + partials + per-page sets)
internal/server/       routing, middleware, centralized error handling, handlers
web/                   embedded assets: templates/{layouts,partials,pages}, static/{css,js}
```

New feature areas (auth, projects, uploads, analysis, docs, chat) are added as their own `internal/<area>` packages. Each one owns its domain logic and its SQL. `internal/server` stays a thin HTTP layer that calls into them.

## Request flow

```
request → requestID → logRequests → recoverPanics → securityHeaders → crossOrigin → loadSession → ServeMux → handler
```

- **requestID** generates an ID on the server; client-supplied IDs are ignored. It also attaches a request-scoped `slog` logger to the context (`loggerFrom(ctx, fallback)`).
- **logRequests** logs one line per request (method, path, status, bytes, duration), at warn level for 4xx and error level for 5xx.
- **recoverPanics** turns a panic into a logged 500 error page, unless response bytes were already written.
- **securityHeaders** sets a strict CSP (`'self'` only, no inline scripts or styles), nosniff, frame denial, a referrer policy, and HSTS in production.

- **crossOrigin** is Go's `http.CrossOriginProtection`: it rejects state-changing browser requests whose `Sec-Fetch-Site` or `Origin` shows they came from another site. This protects every form, including the anonymous login and registration forms.
- **loadSession** resolves the session cookie into the session and user and puts them in the request context. It skips `/static/` and `/healthz`, clears the cookie when the session is unknown or expired, and adds `user_id` to the request logger.

Handlers are usually `func(w, r) error` wrapped by `s.handle(...)`. Returning an `*HTTPError{Status, Message, Err}` renders `Message` to the user and logs `Err`. Any other error becomes a generic 500 page, and the detail only goes to the logs.

Routes use Go 1.22+ `ServeMux` patterns (`"GET /path/{id}"`). The catch-all `GET /` renders the HTML 404 page. A wrong method on a known path gets a 405 from the mux.

## Authentication and authorization

- **Passwords** are hashed with argon2id (OWASP parameters: 19 MiB, t=2, p=1) and stored in PHC format, so the parameters travel with each hash. If the parameters change, the hash is upgraded on the user's next successful login. A semaphore caps concurrent hash computations so a burst of logins can't exhaust memory.
- **Login** returns the same error, in about the same time, for an unknown email and a wrong password (a dummy hash is checked for unknown emails). Failed logins are throttled in memory: 5 per email and 50 per client IP per 15 minutes. The response is a 429 with `Retry-After`. The client IP is the TCP peer; forwarding headers are ignored.
- **Sessions** are server-side rows in `sessions`. The cookie holds a 256-bit random token, and the database stores only its SHA-256 hash. Sessions have an absolute lifetime (`APP_SESSION_LIFETIME`) and an idle timeout (`APP_SESSION_IDLE_TIMEOUT`). `last_seen_at` is written at most once a minute. Every login or registration issues a new token and deletes any token the request carried, which prevents session fixation. Logout deletes the row. A background loop purges expired rows every 15 minutes.
- **Cookie:** `HttpOnly`, `SameSite=Lax` and `Path=/`. In production it is also `Secure` and named with the `__Host-` prefix.
- **CSRF:** besides the cross-origin check, every state-changing request from a signed-in user must carry the session's synchronizer token, either as the `csrf_token` form field or as the `X-CSRF-Token` header. `requireAuth` checks it.
- **Route guards:** `requireAuth` redirects anonymous GETs to `/login?next=…`, where `next` is checked to be a local path to prevent open redirects. It sets `Cache-Control: no-store` and verifies CSRF. `requireGuest` sends signed-in users from `/login` and `/register` to `/dashboard`.
- **Ownership:** every `projects.Store` method takes the owner's user ID and puts it in the `WHERE` clause. A project owned by someone else returns `ErrNotFound`, which becomes the same 404 as a missing project, so project IDs can't be probed. Future feature packages must follow the same pattern.
- **Flash messages** are stored on the session row. `s.page(...)` shows them once and then clears them. Error pages never consume them.

## Templates

`view.New` parses `layouts/*` and `partials/*` once, then clones that set for every file in `pages/`. This lets each page define its own `title` and `content` blocks without clashing with other pages. `Render` executes into a buffer, so a template error never sends a half-written page. Every page receives `view.Page{Title, RequestID, User, CSRFToken, Flash, Data}`; handlers build it with `s.page(r, title, data)`. Forms use the `csrf` partial (or `$csrf` inside `with` blocks). Template helpers: `date`, `isodate` and `bytes`.

## Database

- Driver: `modernc.org/sqlite` (pure Go, so no cgo or C toolchain is needed).
- Every pooled connection gets `foreign_keys=ON`, `journal_mode=WAL`, `busy_timeout=5000` and `synchronous=NORMAL` via DSN pragmas. Transactions use `BEGIN IMMEDIATE` (`_txlock=immediate`) to avoid lock-upgrade deadlocks.
- Migrations are embedded SQL files. Applied versions are recorded in `schema_migrations`. A migration and its record are committed together, so a failure leaves no partial schema. **Never edit an applied migration; add a new file instead.**
- Timestamps are UTC RFC 3339 text.

### Schema (migration 0001)

| Table | Purpose |
|---|---|
| `users` | Accounts. Email is unique and case-insensitive. Stores the password hash. |
| `projects` | Owned by a user. Holds the detected languages, a lifecycle `status` (`empty → uploaded → analyzing → analyzed → documenting → documented`, or `failed`), file counts, and upload and documentation timestamps for the dashboard. |
| `project_files` | One row per uploaded file: relative path (unique per project), language, kind (`source`, `config`, `manifest`, `documentation`, `other`), size, sha256 and storage location. |
| `jobs` | Background job queue: type, status, progress (current, total, message), attempts and max attempts, `run_after` for retry back-off, payload and error. |
| `analysis_results` | Parser-extracted `structure`, AI per-`file` and per-`batch` analyses, and the synthesized `project` analysis, stored as JSON with token usage. |
| `documentation` | Versioned Markdown for each (`project`, `module` or `file` scope, `scope_key`). The highest `version` is the current one. |

Migration 0002 adds `sessions` (token hash, CSRF token, flash, user agent, remote address, created, last seen and expiry), `users.last_login_at`, and a unique index on `(user_id, name COLLATE NOCASE)` so each user's project names are unique.

Everything a user owns hangs off `users.id` through `projects.user_id`, so authorization checks are always a join on `user_id`. Deleting a project cascades to its files, jobs, analyses and documentation.
