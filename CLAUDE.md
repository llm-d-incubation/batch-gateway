# CLAUDE.md — batch-gateway

OpenAI-compatible batch API gateway for llm-d. Three binaries: apiserver, batch-processor, batch-gc.

## General Principles

- **Think before coding**: State assumptions explicitly. If uncertain, ask. Don't guess silently.
- **Simplicity first**: No features beyond what was asked. No abstractions for single-use code. No error handling for impossible scenarios.
- **Surgical changes**: Only touch what you must. Don't "improve" adjacent code, comments, or formatting. Match existing style.

## Code Conventions

- **Logging**: Use `logr.Logger` — not stdlib `log` (banned in non-test code) or `klog` logging functions.
- **Interfaces**: Verify compliance at compile time: `var _ Interface = (*Impl)(nil)`. This project uses this pattern extensively.
- **Errors**: Wrap with `%w` (not `%v`) so callers can use `errors.Is`/`errors.As`. Handle errors once — return OR log, never both. Only wrap when adding context the caller doesn't have — keep wrap messages short (function/operation name). Exported error vars use `Err` prefix (`var ErrNotFound = errors.New(...)`).
- **Tests**: Table-driven with subtests. Group tests for the same function under one `TestXxx` using `t.Run()`, not separate `TestXxx_Case1`, `TestXxx_Case2` functions. Use `name` field for test case description.
- **Structs**: Always use field names in initialization. Use `var s T` for zero values, not `s := T{}`.
- **Goroutines**: Never fire-and-forget. Always provide a shutdown signal mechanism. Prefer small channel buffers (0 or 1). Larger buffers need justification.
- **Early return**: Handle errors first and return early to reduce nesting.
- **No panic**: Never use `panic()` in production code. Return errors instead.
- **No os.Exit outside main**: `os.Exit` and `log.Fatal` belong only in `main()`. Everywhere else, return errors.
- **No mutable globals**: Avoid package-level mutable variables. Use dependency injection. Exceptions: compiled regexps, SQL schemas, error sentinels.
- **Type assertions**: Always use comma-ok form `v, ok := x.(T)` to avoid panics.
- **No init()**: Avoid `func init()`. Pass dependencies explicitly.
- **Defer for cleanup**: Use `defer` to release resources (files, locks, HTTP bodies).

## Build & Verify

- `make build` — compile all binaries
- `make tidy` — run after modifying go.mod/go.sum
- `make pre-commit` — run before committing (formatting, vet, lint, unit tests, security scan)

## Local Testing

- Unit tests: `make test`
- Integration tests: `make test-integration` (or `make test-all` for unit + integration)
- E2E: `make dev-deploy` to deploy to a local Kind cluster, then `make test-e2e`
