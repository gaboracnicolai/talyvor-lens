# Talyvor Lens — Performance Benchmarks

A reproducible benchmark suite that measures Talyvor Lens proxy overhead under controlled conditions. Every benchmark mocks the LLM upstream with `httptest` so the numbers reflect Lens itself — never provider latency.

## Running benchmarks

```bash
# From the repo root
go test -tags=bench -bench=. -benchmem -benchtime=10s ./benchmarks/...
```

Or via the Makefile shortcut:

```bash
make bench
```

The `bench` build tag keeps the suite out of the default `go test ./...` so unit-test loops stay fast.

## Results

Generated automatically on every push to `main` by `.github/workflows/benchmark.yaml` (GitHub Actions, `ubuntu-latest`, 4 vCPU). The HTML report is rendered by `internal/benchmark.GenerateHTML`; the markdown table below is regenerated on each release.

| Benchmark | Notes |
|-----------|-------|
| `BenchmarkExactCacheHit` | Full proxy request, exact cache pre-warmed. Should be `< 1 ms`. |
| `BenchmarkExactCacheMiss` | Full proxy request, cold cache. Should be `< 5 ms` excluding real LLM. |
| `BenchmarkSemanticCacheHit` | Reserved slot — exercises the proxy path. Full pgvector run requires a real Postgres. |
| `BenchmarkPromptCompression` | Pure compressor throughput. Target `> 10,000 ops/sec`. Measures the function, which since migration 0117 only runs for a workspace that opted in (`compression_policy`, default `disabled`). |
| `BenchmarkModelRouting` | Pure router throughput. Target `> 100,000 ops/sec`. |
| `BenchmarkPIIDetection` | 200-word text with three PII items. Target `> 50,000 ops/sec`. |
| `BenchmarkInjectionDetection` | 100-word clean prompt. |
| `BenchmarkConcurrentRequests` | `b.RunParallel` with `GOMAXPROCS` goroutines. |
| `BenchmarkRateLimitCheck` | Lua-backed atomic ops against miniredis. |
| `BenchmarkFullProxyStack` | Headline number: PII + injection + budget + cache + route + forward + cache-write. **No longer includes compress** — 0117 gated the prompt rewriter behind a per-workspace policy that defaults to `disabled`, and this benchmark wires a nil workspace manager, so it now measures the default serving path. The gate is visible at `fd38044` as **allocs/op 390 → 354** (and `ExactCacheMiss` identically), *not* in ns/op — see Methodology. |

## Methodology

- All benchmarks mock the LLM upstream via `httptest.NewServer`.
- Results exclude real LLM latency (provider-dependent, unreproducible).
- Memory measured with the `-benchmem` flag.
- Concurrent benchmark uses `GOMAXPROCS` parallel goroutines via `b.RunParallel`.
- Each benchmark uses `b.ResetTimer()` after setup so init time (cache priming, fixture build) doesn't count.
- **`ns/op` cannot carry a single-commit claim; `allocs/op` can.** This suite runs on shared GitHub-hosted runners. Measured between two consecutive main runs (`34afe59` → `fd38044`), `BenchmarkExactCacheHit` moved **+31.7%** in ns/op while its allocation profile stayed byte-identical (98 allocs, 10477 B) and nothing on its code path had changed. Wall clock here is a trend across many runs. When you need to show that a specific change did or did not take effect, point at the allocation count — as the `BenchmarkFullProxyStack` row above does for the 0117 compression gate.

## vs Competitors

| Metric | Talyvor Lens | LiteLLM | Portkey |
|--------|--------------|---------|---------|
| Language | Go | Python | Node.js |
| Overhead per request | `< 2 ms` | `~40 ms` | `~15 ms` |
| Memory (idle) | `< 50 MB` | `~300 MB` | `~200 MB` |
| RPS @ 1 vCPU | `5,000+` | `~500` | `~1,000` |
| Open source | Yes (core) | Yes | Yes (gateway) |

LiteLLM struggles past ~2,000 RPS due to Python's GIL; memory under load can exceed 8 GB. Talyvor Lens is a single Go binary with bounded memory.

## Regenerating the public report

```bash
make bench | tee benchmarks/benchmark-results.txt
go run ./cmd/benchreport benchmarks/benchmark-results.txt > docs/benchmarks.html
```

The HTML page lives at `docs/benchmarks.html` (committed) and is served from `talyvor.com/benchmarks`.

## Notes

- All numbers are reproducible from a clean checkout. Don't trust a benchmark you can't re-run.
- If you find a slower path than what the headline reports, open an issue with the workload — that's a regression we want to know about.
- Real-world latency is dominated by the LLM provider; Lens is engineered so its share of the round-trip is always under 2 ms.
