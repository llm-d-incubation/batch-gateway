# Regression Tests

Regression tests validate backward compatibility of the OpenAI-compatible API surface. They catch accidental breaking changes to JSON schemas, field names, and serialization behavior.

## What they cover

- **Schema compatibility**: Golden-file round-trip tests that marshal/unmarshal API types (`Batch`, `FileObject`, `ListBatchResponse`, `CreateBatchRequest`, `ErrorResponse`, etc.) and verify the JSON key structure matches a pinned snapshot
- **Serialization guards**: Verify `omitempty` behavior on optional fields to prevent unintended wire-format changes (e.g. a zero-value `Batch` must not emit `"model"`)

## Run the tests

```bash
make test-regression
```

Or directly:

```bash
go test -v -count=1 ./test/regression/...
```

## How they work

Each schema test loads a golden JSON file from `testdata/`, unmarshals it into the corresponding Go struct, re-marshals it, and recursively compares the JSON key structure. This detects added, removed, or renamed fields without being sensitive to value changes.

These tests have no build tag — they run as part of `go test ./...` alongside unit tests, so schema breaks are caught locally before push.
