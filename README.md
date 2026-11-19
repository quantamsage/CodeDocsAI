# CodeDocs AI

CodeDocs AI is a web application that generates technical documentation from uploaded source code using an LLM. It's built with Go, server-rendered HTML templates, vanilla JavaScript and SQLite.

> Status: under active development. See [PROJECT_STATUS.md](PROJECT_STATUS.md) for what is implemented, and [ARCHITECTURE.md](ARCHITECTURE.md) for the design.

## Prerequisites

- Go 1.26 or newer. No C compiler is needed, because the SQLite driver is pure Go.

## Running

```sh
go run ./cmd/server
```

The server listens on `http://localhost:8080`. On first start it creates `data/app.db` and applies database migrations automatically. The health check is at `GET /healthz`.

Open the site, choose **Sign up** to create an account, and you're taken to your dashboard, where you can create, view and delete projects.

Open a project and choose **Upload files** to add code. You can upload individual files, a whole project folder, or a `.zip` archive. The project page then shows the directory tree, the detected languages and file kinds, and each file can be opened in a viewer with line numbers. Uploaded code is stored as text under `data/uploads/` and is never executed. Dependency and version-control folders, binaries, nested archives and files that may hold secrets (such as `.env` and private keys) are skipped. Archives with unsafe paths, symbolic links or ZIP-bomb compression ratios are rejected. Upload limits are configurable (`APP_UPLOAD_*` in `.env.example`).

Configuration comes from environment variables. See [.env.example](.env.example) for every option and its default.

To build a binary:

```sh
go build -o bin/codedocs ./cmd/server
```

## AI analysis

Set `ANTHROPIC_API_KEY` (from https://platform.claude.com/) before starting the server. Then open a project with uploaded files and choose **Analyze project**: the analysis runs as a background job (you can leave the page), extracts each file's structure (Go with the Go parser, other languages with language-specific patterns), analyzes the code in batches with the AI, and builds a project overview. Results appear on the project's **Analysis** page and on each file's page.

Model and limits are configurable with `APP_AI_MODEL` (default `claude-opus-5-5`), `APP_AI_EFFORT`, `APP_AI_TIMEOUT`, `APP_AI_BATCH_TOKENS` and `APP_JOB_WORKERS` (see `.env.example`). Without a key, everything except AI features keeps working. Automated tests never call the AI; they use a fake provider.

## Documentation

After analysis, choose **Generate documentation** on the project page. A background job writes the project sections (overview, architecture, directory and file guide, API and database references when the project has them, dependencies, configuration and setup, usage examples) and one document per module, from the stored analysis. Open **View documentation** to browse them. Each document can be regenerated on its own, and every regeneration keeps the previous versions. From a file's page you can also generate documentation for that single file.

Documents are rendered with headings, tables, lists, links and syntax-highlighted code, with an "On this page" table of contents and previous/next navigation. Use **Download Markdown** for a single document or **Download all (ZIP)** for the whole set, organized as `README.md` (index), `sections/`, `modules/` and `files/`.

## Tests

```sh
go test ./...                                         # all tests
go test ./internal/database -run '^TestMigrate' -v    # a subset
go vet ./...
```
