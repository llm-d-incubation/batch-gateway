# System-Level Benchmarks

Go benchmarks for batch-gateway subsystems: preprocessor ingestion, executor dispatch, progress tracking, queue operations, and file I/O.

## Running

```bash
# all benchmarks
make test-bench

# single category
go test -tags=bench -bench=BenchmarkPreprocessor -benchmem ./test/benchmark/preprocessor/

# adjust duration (default 1s)
make test-bench BENCHTIME=5s
```

The `-tags=bench` flag is required for preprocessor and executor benchmarks (they depend on exported test helpers gated behind a build tag). The Makefile target includes it automatically.

## Comparing results

Use [benchstat](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat) to compare runs before and after a change.

```bash
# install benchstat (one-time)
go install golang.org/x/perf/cmd/benchstat@latest

# capture baseline
go test -tags=bench -bench=. -benchmem -count=6 ./test/benchmark/... > old.txt

# make your changes, then capture again
go test -tags=bench -bench=. -benchmem -count=6 ./test/benchmark/... > new.txt

# compare
benchstat old.txt new.txt
```

Use `-count=6` (or higher) so benchstat has enough samples to compute statistical significance. It will flag changes with a p-value and confidence interval:

```
                          │   old.txt   │              new.txt               │
                          │   sec/op    │   sec/op     vs base               │
Preprocessor/1K_lines-18   3.524m ± 2%   3.891m ± 3%  +10.42% (p=0.002 n=6)
```

A `~` means the difference is not statistically significant. Only act on changes marked with `+` or `-` and a low p-value.

## Benchmark categories

| Package | What it measures | Key metrics |
|---|---|---|
| `preprocessor/` | JSONL parsing, validation, FNV hashing, plan file building | lines/sec, MB/sec, allocs/op |
| `executor/` | Inference dispatch with semaphore hierarchy, output writing | req/sec, p50/p99 latency, allocs/op |
| `progress/` | ProgressTracker mutex contention, StatusUpdater throughput | ns/op (serial vs parallel) |
| `queue/` | Redis sorted set enqueue (miniredis), dequeue, mixed r/w | ns/op, allocs/op |
| `filestore/` | Filesystem store/retrieve at 10MB, 100MB, 500MB | MB/sec, allocs/op |
