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
internal/files/        uploads: path cleaning, language/kind detection, safe ZIP extraction,
                       blob storage on disk, owner-scoped file store and directory tree
internal/ai/           LLM provider interface, Claude implementation (official Go SDK), fake for tests
internal/structure/    language-aware structure extraction (go/ast for Go, patterns for others)
internal/jobs/         SQLite-backed background job queue (progress, retries, cancel, recovery)
internal/analysis/     AI analysis pipeline (selection, batching, validation, synthesis, import graph)
internal/docs/         documentation generator (from stored analysis), versioned document store
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

## Uploads and the project explorer

Routes: `GET/POST /projects/{id}/upload` and `GET /projects/{id}/files/{path...}`. The project page (`GET /projects/{id}`) shows the file tree.

**Request handling.** `acceptUpload` wraps `requireAuth` on the upload route. It caps the body at `APP_UPLOAD_MAX_REQUEST_SIZE` (an over-sized `Content-Length` is rejected at once), extends the connection's read and write deadlines and the request context to `APP_UPLOAD_TIMEOUT`, and marks the request as allowed to carry multipart. The body is streamed with `multipart.Reader`; it is never buffered whole or spooled by `ParseMultipartForm`. Because the CSRF token can't be read without consuming the stream, `verifyCSRF` requires it to be the **first** part (browsers send fields in document order, so the hidden input sits first in the form). The reader, now past that part, is passed to the handler through the request context. Scripted clients may send `X-CSRF-Token` instead. Other routes still reject multipart bodies with 415.

**Parts.** `files` parts are individual files or `.zip` archives; `folder` parts come from a `webkitdirectory` input and carry relative paths. `multipart.Part.FileName` drops directories, so the raw `Content-Disposition` filename is used, and the selected folder's own name is stripped. Optional `directory` (target folder) and `replace` fields apply to the whole upload. Other archive formats are rejected.

**Validation (`files.Batch`).** Each file is checked and written to storage one at a time, so memory is bounded by the per-file limit:

1. `CleanPath` normalizes separators and rejects absolute paths, drive letters, `..`, control characters, invalid UTF-8, Windows reserved names and characters, and over-long or over-deep paths. An unsafe path **rejects the whole upload**.
2. These are **skipped** and reported to the user: files in dependency, build or VCS folders (`node_modules`, `.git`, `.venv`, …); OS metadata files; likely secrets (`.env`, `.env.*` except templates such as `.env.example`, private keys, `.netrc`); nested archives; executables by extension or magic number (ELF, PE, Mach-O, Java class, Wasm); unknown types; non-UTF-8 or NUL-containing content; and files over the per-file limit.
3. Exceeding the file-count or total-size limit **rejects the upload**.

**ZIP archives** are spooled to a temporary file under `data/tmp` (the central directory is at the end) and read with `archive/zip`. Symbolic links, special files and unsafe names reject the archive. The number of entries is capped (10× the file limit, at least 1,000). Encrypted or unusually compressed entries are skipped. The declared size is used only to skip obviously oversized entries; actual reads are always limited. An entry that expands more than `APP_UPLOAD_MAX_COMPRESSION_RATIO` times its compressed size (after 256 KiB) rejects the archive as a ZIP bomb, and the total-size limit bounds the whole archive. If every file sits under one top-level folder, that folder is removed; the rest of the structure is preserved.

**Classification.** `Classify` assigns a language and a kind (`source`, `config`, `manifest`, `documentation`) by exact file name first (`go.mod`, `package.json`, `Cargo.toml`, `Dockerfile`, `README`, …) and then by extension. It covers Go, Python, JavaScript, TypeScript, Java, C, C++, Rust, PHP, Ruby and C#, and several more. `.h` counts as C. The project's language breakdown counts source files only.

**Storage.** Content is written to `data/uploads/<projectID>/<random>.blob` with `0600` permissions. Paths from the upload never select a location on disk; they are only metadata in `project_files`, and the blob store refuses any storage path that doesn't match its own naming pattern. `files.Store.Commit` records a batch in one transaction: it checks ownership, deletes existing rows (replace mode) or overwrites rows with the same path (merge mode), enforces the project-wide file and size limits, and updates the project's `file_count`, `total_size_bytes`, `languages_json`, `primary_language`, `status = 'uploaded'` and `uploaded_at`. Blobs of overwritten or replaced files are deleted after the commit; on failure the batch's new blobs are deleted. Deleting a project removes its upload directory after the rows are gone.

**Explorer.** Every file query joins `projects` on `user_id`, so another user's files return 404. The tree is built in memory from the sorted file list and rendered with nested `<details>` (no JavaScript needed). Small projects are fully expanded; on file pages, the directories leading to the current file are open. The file viewer shows up to 512 KiB with CSS line numbers. Content is HTML-escaped by `html/template` and served as part of the page, never as a raw same-origin document.

## AI analysis

**Provider.** `ai.Provider.GenerateJSON` takes a system prompt, a user prompt and a JSON schema. `ai.Claude` calls the Messages API (beta endpoint, for server-side refusal fallbacks) with `output_config: {effort, format: json_schema}`; thinking is left to the model's default. Errors wrap `ErrNotConfigured`, `ErrRateLimited`, `ErrTransient`, `ErrInvalidOutput` or `ErrRequestRejected`; `ai.Retryable` and `ai.RetryAfter` drive job retries. Swapping vendors means another `Provider`.

**Untrusted input.** Uploaded code is only placed in the user prompt, inside `<file>`/`<content>` delimiters with line numbers; system prompts say the files are data and that instructions inside them must be ignored. Model output is never executed and is validated before storage.

**Jobs.** `jobs.Queue` stores work in `jobs`. Workers (`APP_JOB_WORKERS`) claim the oldest due job with one `UPDATE … RETURNING` statement, run the registered handler with a progress reporter, then mark it succeeded, re-queue it with backoff (`run_after`) when the error is retryable and attempts remain, or mark it failed and call the type's `OnFailed`. Cancel sets the row to `canceled` and cancels the handler's context. On startup, jobs left `running` are re-queued. Only one queued/running job per type and project is allowed.

**Pipeline** (`analysis.Analyzer`, job type `analyze_project`):

```
files ─▶ select relevant ─▶ structure.Extract ─▶ plan batches (≤ budget, ≤ 8 files, split large files)
      ─▶ AI per batch ─▶ validate (paths, names in code, caps) ─▶ store "file" results
      ─▶ aggregate + import graph ─▶ [module summaries if large] ─▶ AI project synthesis ─▶ store "project"
```

Results live in `analysis_results` (`structure` and `file` per file, `batch` with token usage, `project`), tagged with the job id; earlier runs are deleted when a new run starts, while the current run's rows let a retry skip finished files. Reads (`analysis.Store.Project/Files/File`) join `projects.user_id`.

## Documentation generation

`docs.Generator` (job type `generate_docs`) reads the stored analysis, never the code. `docs.Plan` turns a request (`all`, or one `project` section / `module` directory / `file`) into tasks; the API and Database sections are planned only when the analysis has endpoints or data models. Each task is one AI request whose prompt holds only the analysis data for that document, inside `<analysis>` delimiters. The response (`{"markdown": …}`) is checked, stripped of active HTML tags and stored as a new version in `documentation` (`scope`, `key`, `version`, `job_id`). A retried job skips documents it already stored. `docs.Store` reads are owner-scoped: `Current` (latest version of every document), `Get` (one version) and `Versions`.

## Templates

`view.New` parses `layouts/*` and `partials/*` once, then clones that set for every file in `pages/`. This lets each page define its own `title` and `content` blocks without clashing with other pages. `Render` executes into a buffer, so a template error never sends a half-written page. Every page receives `view.Page{Title, RequestID, User, CSRFToken, Flash, Data}`; handlers build it with `s.page(r, title, data)`. Forms use the `csrf` partial (or `$csrf` inside `with` blocks). Template helpers: `date`, `isodate`, `bytes` and `dict` (builds a map, for passing several values to a partial).

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

Analysis rows (`analysis_results`) reference their job; jobs and results cascade with the project.

Migration 0002 adds `sessions` (token hash, CSRF token, flash, user agent, remote address, created, last seen and expiry), `users.last_login_at`, and a unique index on `(user_id, name COLLATE NOCASE)` so each user's project names are unique.

Everything a user owns hangs off `users.id` through `projects.user_id`, so authorization checks are always a join on `user_id`. Deleting a project cascades to its files, jobs, analyses and documentation.
