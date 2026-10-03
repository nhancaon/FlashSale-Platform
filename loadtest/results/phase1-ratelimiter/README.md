# Phase 1 — preliminary rate limiter benchmark

**This is a sanity check, not a Go-vs-Java comparison.** Do not quote these numbers as a result.

Setup: both services ran as plain host processes (Windows 11) against the Redis from
`make up`; k6 ran in a Docker container and reached them through `host.docker.internal`.
100 VUs, 15 s after an 8 s warm-up, 1000 distinct keys, limit high enough that nothing is
rejected. Script: `loadtest/ratelimiter-check.js`, driver: `loadtest/run-ratelimiter-bench.sh`.

| Service | Algorithm | req/s | p95 ms | p99 ms | failed | longest iteration |
|---|---|---:|---:|---:|---:|---:|
| go | token_bucket | 10631 | 16.0 | 22.4 | 0.00% | 73 ms |
| go | sliding_window | 11047 | 15.6 | 22.2 | 0.00% | 94 ms |
| go | fixed_window | 10720 | 16.1 | 22.6 | 0.00% | 102 ms |
| java | token_bucket | 2778 | 17.1 | 22.7 | 0.07% | **30002 ms** |
| java | sliding_window | 6412 | 25.0 | 36.2 | 0.00% | 83 ms |
| java | fixed_window | 2194 | 23.1 | 30.4 | 0.01% | **30001 ms** |

## Why the Java throughput numbers are not trustworthy

- In 2 of 3 Java runs some k6 virtual users sat stuck for 30 s (`dial: i/o timeout`), so
  they stopped contributing load for most of the 15 s window. That alone explains the low
  req/s; the latency percentiles of requests that did complete look similar to Go.
- The `sliding_window` Java run had no stalled user and reached 6412 req/s, the same code
  path with the same Redis, so the stalls come from the connection path, not the algorithm.
- Raising `server.tomcat.accept-count` to 1024 did **not** remove the stalls.
- Suspect: container-to-host connection handling on Docker Desktop for Windows. Not proven.

## What Phase 6 must do differently

Run both services in containers on one Docker network with the same CPU/memory limits,
drive them with k6 over that network (no `host.docker.internal`), repeat at least 3 times,
and discard or investigate any run with a stalled iteration.

## What these runs do show

- Both services return identical answers for the same request (verified by hand and by tests).
- Go decision latency through the host path: p95 about 16 ms at 100 VUs, mostly network.
- The three algorithms cost about the same per request in Go (all one Lua call).
