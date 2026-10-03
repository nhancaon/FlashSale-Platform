# Phase 2: reserve latency, 3 stock strategies x 2 languages

Raw data: `results.jsonl` (one line per run). Reproduce: `make up && make db-migrate`, build both services,
then `GO_BIN=<inventory-go binary> bash loadtest/run-inventory-bench.sh`.

Every run, every combination: **no oversell, no HTTP errors, `available + reserved` equals the initial stock and
one RESERVED row per successful reply** (checked in Oracle after each run).

## Results (median of 3 runs, 2000 reserve requests, 64 in flight)

**sellout**: 100 units, 2000 buyers, so 1900 get OUT_OF_STOCK. Heavy contention on one stock row.

| Service | Strategy | req/s | p50 ms | p95 ms | p99 ms |
|---|---|---:|---:|---:|---:|
| go | atomic | 1343 | 36.3 | 94.8 | 174.2 |
| go | pessimistic | 822 | 66.1 | 169.3 | 248.3 |
| go | optimistic | 1327 | 36.9 | 113.9 | 224.4 |
| java | atomic | 1438 | 36.1 | 94.8 | 161.1 |
| java | pessimistic | 890 | 60.9 | 152.1 | 216.7 |
| java | optimistic | 1227 | 36.7 | 155.8 | 216.4 |

**plenty**: 100000 units, so every reserve succeeds. Pure write cost under contention on one row.

| Service | Strategy | req/s | p50 ms | p95 ms | p99 ms |
|---|---|---:|---:|---:|---:|
| go | atomic | 854 | 67.0 | 142.1 | 188.3 |
| go | pessimistic | 545 | 103.8 | 225.9 | 293.1 |
| go | optimistic | 366 | 153.4 | 358.8 | 466.5 |
| java | atomic | 989 | 52.9 | 140.8 | 187.8 |
| java | pessimistic | 594 | 90.9 | 228.3 | 323.4 |
| java | optimistic | 463 | 112.6 | 319.9 | 432.5 |

The Go optimistic "plenty" runs gave up on 1 request (503 CONTENTION) in at least one run; overselling never happened.

## What the numbers say

- **Strategy matters much more than language.** `atomic` is fastest everywhere; `pessimistic` costs about 1.6x the
  throughput of `atomic` (two round trips and the lock is held across both); `optimistic` is as fast as `atomic` when
  most requests find the stock gone (cheap read, no write) but the slowest when every request must win the version
  check (about 2.3x slower than `atomic`, plus the retry storms).
- In the contended single-row case the database is the bottleneck, so Go and Java land close together. Java was
  8% to 26% faster in the all-success scenario and about equal in sellout. I did not test whether that gap is larger
  than run-to-run noise (3 runs only). A plausible but unproven cause is the driver (Oracle's ojdbc vs the pure Go
  `go-ora`), not the languages themselves.

## Limits: read before quoting

- One laptop, services as host processes, Oracle Free (slim) in a container with its default limits, shared with the
  client. Not the controlled benchmark; Phase 6 repeats this in limited containers.
- Both services use a pool of 20 connections; the client keeps 64 requests in flight (more than that gets connections
  refused by inventory-java on this Windows host, see the Phase 1 notes).
- A single hot row is the worst case on purpose. With stock spread over many SKUs the strategies converge.
