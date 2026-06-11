# Functional Tests

Functional tests validate feature-level behavior through the real HTTP stack — routing, middleware, and handlers — but with in-memory mock backends. No Kubernetes cluster, database, or Redis instance is required.

## What they cover

- **Batch workflows**: create, retrieve, list, cancel, validation errors
- **File workflows**: upload, download, retrieve, list, delete, validation errors
- **Multi-tenant isolation**: resources created by one tenant are invisible to another
- **Cross-cutting behavior**: security headers, request ID propagation, 404 JSON format

## Prerequisites

- Go 1.25+

## Run the tests

```bash
make test-functional
```

Or directly:

```bash
go test -race -v -tags=functional -count=1 ./test/functional/...
```

## How they work

Each test calls `newTestServer(t)` which spins up an `httptest.Server` wired with:

- The same `http.ServeMux` and middleware chain as the production server (`Recovery → RequestMiddleware → SecurityHeaders`)
- In-memory mock DB clients (batch and file metadata)
- A local-filesystem file store under `t.TempDir()`

Tests make real HTTP requests and validate responses as a client would. No state persists between tests — each `newTestServer` call creates a fresh, isolated instance.

## Build tag

All files carry `//go:build functional`. They are excluded from `make test` (unit-only) and `make test-integration`. Run them explicitly with `make test-functional`.
