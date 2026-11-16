# Architecture

CodeDocs AI is a single Go binary. It serves server-rendered HTML pages, stores its data in SQLite, and (from Phase 4 on) calls an LLM API through a provider interface. Templates and static assets are embedded, so a deployment is the binary plus a data directory.

## Layout

```
cmd/server/            entry point: config → logger → DB open + migrate → HTTP server
internal/config/       environment-variable configuration (LoadFrom is injectable for tests)
internal/database/     SQLite connection (Open) and embedded migrations (Migrate)
  migrations/          NNNN_description.sql, applied in order, each in its own transaction
internal/view/         HTML template renderer (layout + partials + per-page sets)
internal/server/       routing, middleware, centralized error handling, handlers
web/                   embedded assets: templates/{layouts,partials,pages}, static/{css,js}
```

New feature areas (auth, projects, uploads, analysis, docs, chat) are added as their own `internal/<area>` packages. Each one owns its domain logic and its SQL. `internal/server` stays a thin HTTP layer that calls into them.

## Request flow

```
request → requestID → logRequests → recoverPanics → securityHeaders → ServeMux → handler
```

- **requestID** generates an ID on the server; client-supplied IDs are ignored. It also attaches a request-scoped `slog` logger to the context (`loggerFrom(ctx, fallback)`).
- **logRequests** logs one line per request (method, path, status, bytes, duration), at warn level for 4xx and error level for 5xx.
- **recoverPanics** turns a panic into a logged 500 error page, unless response bytes were already written.
- **securityHeaders** sets a strict CSP (`'self'` only, no inline scripts or styles), nosniff, frame denial, a referrer policy, and HSTS in production.

Handlers are usually `func(w, r) error` wrapped by `s.handle(...)`. Returning an `*HTTPError{Status, Message, Err}` renders `Message` to the user and logs `Err`. Any other error becomes a generic 500 page, and the detail only goes to the logs.

Routes use Go 1.22+ `ServeMux` patterns (`"GET /path/{id}"`). The catch-all `GET /` renders the HTML 404 page. A wrong method on a known path gets a 405 from the mux.

## Templates

`view.New` parses `layouts/*` and `partials/*` once, then clones that set for every file in `pages/`. This lets each page define its own `title` and `content` blocks without clashing with other pages. `Render` executes into a buffer, so a template error never sends a half-written page. Every page receives `view.Page{Title, RequestID, Data}`.

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

Everything a user owns hangs off `users.id` through `projects.user_id`, so authorization checks are always a join on `user_id`. Deleting a project cascades to its files, jobs, analyses and documentation.
