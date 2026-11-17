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

Configuration comes from environment variables. See [.env.example](.env.example) for every option and its default.

To build a binary:

```sh
go build -o bin/codedocs ./cmd/server
```

## Tests

```sh
go test ./...                                         # all tests
go test ./internal/database -run '^TestMigrate' -v    # a subset
go vet ./...
```
