# Benchmarks

`bench_test.go` holds benchmarks for the request hot paths. They exist to catch
regressions, not to chase micro-optimizations. Measure before changing code:
several plausible "wins" here turned out to be neutral or worse when measured
(see Avoiding benchmark traps).

## Running

```sh
make bench                                   # quick run
go test -run '^$' -bench . -benchmem -count=10 ./...   # comparable run
```

`-run '^$'` skips the unit tests. Always use `-benchmem`.

## Baseline

Recorded on linux/amd64, Go 1.26, 16 CPUs; re-measure on DragonFly before
trusting these. Numbers vary with CPU, Go version, and filesystem.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkStoreGet` | ~5200 | 1120 | 15 |
| `BenchmarkRedirectHandler` | ~7800 | 2320 | 28 |
| `BenchmarkMiddlewareChain` | ~1800 | 1240 | 17 |
| `BenchmarkRateKey/ipv4` | ~32 | 16 | 1 |
| `BenchmarkRateKey/ipv6` | ~123 | 32 | 2 |
| `BenchmarkWriteJSON` | ~1150 | 272 | 7 |
| `BenchmarkAccessLogEnqueue` | — | — | 0 |

`BoltStore.Get` (bbolt + `json.Unmarshal`) is ~two thirds of a redirect. The
middleware figure includes `httptest.NewRecorder` overhead, so treat its alloc
count as an upper bound; profile a real server to attribute per-request cost.

## Comparing two revisions

Save each run and compare with `benchstat` (from
`golang.org/x/perf/cmd/benchstat`):

```sh
go test -run '^$' -bench . -benchmem -count=10 ./... > old.txt
# ... change code ...
go test -run '^$' -bench . -benchmem -count=10 ./... > new.txt
benchstat old.txt new.txt
```

Use `-count=10` (or more) and `benchstat`; a single run is not evidence. Look at
both ns/op and allocs/op, and check that the delta exceeds run-to-run noise.

## Profiling

```sh
go test -run '^$' -bench BenchmarkRedirectHandler -cpuprofile cpu.out -memprofile mem.out ./...
go tool pprof -top -nodecount=20 cpu.out
go tool pprof -list='handleRedirect' cpu.out
go tool pprof -alloc_space -top mem.out
go tool pprof -http=:0 cpu.out      # flame graph in a browser
```

For a whole-server profile, add `net/http/pprof` on a **separate, non-public**
listener (never expose it on the public port) and drive load with `wrk`, `hey`,
or `vegeta`. Profile on the deployment platform: allocator and scheduler
behavior differ between Linux and DragonFly.

## Escape analysis

To see what the compiler moves to the heap:

```sh
go build -gcflags=-m ./... 2>&1 | grep -E 'escapes to heap|leaking param'
```

A "leaking param" is not automatically a heap allocation. When a function with
a **value** parameter stores that parameter somewhere that outlives the call
(for example, sending it on a channel), the value is copied into the
destination; the caller's variable need not escape. Taking the address of the
parameter (`&e`) does force it to escape. This is why
`AccessLogger.Log(e AccessEntry)` takes the entry by value.

## Avoiding benchmark traps

The benchmark must exercise the code the way the program does.

- **Do not let the compiler elide the work.** Return or store the result (a
  package-level sink, or `b.ReportAllocs`), and keep the measured function
  non-inlinable where the real call is (`//go:noinline`).
- **Beware loop-invariant operands.** An early version of an `json.Marshal(e)`
  vs `json.Marshal(&e)` benchmark built the entry once outside the loop. The
  compiler hoisted the escape, making the pointer form look like it saved an
  allocation. With the entry built per iteration (as it is in the real
  `AccessLogger` writer), both forms cost 3 allocs/721 B — no difference.
- **Reset the timer** after setup and **report allocations**.
- **Compare like with like:** include the same header writes/setup in both
  variants; a difference in call site is a difference in allocation.
- **Repeat and compare:** `-count` plus `benchstat`, not one run.
- **Watch for allocator effects:** a larger `B/op` with fewer allocs can still
  be faster; judge ns/op and allocs/op together.

## Ground rules

- Optimize the dominant term first; here that is `BoltStore.Get`, not string
  formatting (~0.4% of a redirect).
- Prefer removing allocations over clever CPU tricks; the GC is usually the
  bigger cost.
- Keep the code simple. If a change does not show a clear, repeatable win, drop
  it.
