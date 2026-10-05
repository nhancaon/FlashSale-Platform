# Concurrency lab: Go vs Java

The same ten experiments in Go (`go/`) and Java 25 (`java/`), with the same flags and the same output: one JSON line per
measured repetition. Results and the side-by-side discussion: [docs/concurrency-comparison.md](../../docs/concurrency-comparison.md).

```bash
make lab-go       # every Go experiment in a golang:1.26 container   (--cpus 2 --memory 3g)
make lab-java     # every Java experiment in an eclipse-temurin:25-jdk container, same limits
make lab-report   # results/*.jsonl -> docs/concurrency-comparison.md + docs/img/lab-*.svg
```

Knobs: `LAB_CPUS=2 LAB_MEM=3g REPS=5`. Stop other heavy stacks first (`make down`, `make lab-down`): the lab measures memory.

| # | Experiment | Go | Java |
|---|---|---|---|
| 1 | N tasks sleeping 100 ms (1k..1M) | goroutine | platform thread, virtual thread |
| 2 | M CPU-bound jobs (SHA-256 x 2000) | worker goroutines + channel | fixed thread pool, ForkJoinPool |
| 3 | 10k simulated I/O calls, 50–200 ms | goroutine + WaitGroup + context | virtual threads, CompletableFuture + delayed executor |
| 4 | 100 threads x 100k increments | Mutex, atomic, channel owner | synchronized, ReentrantLock, AtomicLong, LongAdder |
| 5 | Data race on purpose, then the fix | `go test -race` (race/) | repeated runs counting lost updates |
| 6 | Bounded queue, 4 producers / 4 consumers | buffered channel: block / select-default drop | ArrayBlockingQueue: put / offer |
| 7 | Cancel 10k running tasks after 100 ms | context.WithTimeout | Future.cancel(true), StructuredTaskScope with timeout (preview) |
| 8 | Deadlock and leak on purpose, diagnosis | goroutine dump (pprof), NumGoroutine | ThreadMXBean.findDeadlockedThreads, thread count |
| 9 | Pipeline parse → transform → write, 1M lines | channel stages | sequential stream, parallel stream, queues + virtual threads |
| 10 | Rate limiter of Phase 1 | services/ratelimiter-go | services/ratelimiter-java (code and test lines, by report.mjs) |

Every run is a separate process (peak memory per run, a crash is recorded as a result with its exit code). Each
process does one warm-up repetition, then `REPS` measured ones. Experiments 2 and 3 also record a CPU profile
(pprof / JFR) that `report.mjs` turns into flame graphs.

Run one experiment by hand (inside the containers, or on Linux):

```bash
cd go && go build -o lab . && ./lab -exp 3 -n 10000 -reps 5
cd java && ./mvnw -q package && java --enable-preview -jar target/lab.jar -exp 3 -variant virtual -n 10000
```
