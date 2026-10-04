# Benchmark report: inventory-go vs inventory-java (Phase 6b)

**Short answer.** At the same load, through the whole system (gateway → order → inventory → Oracle → outbox → Kafka →
notification), the two inventory services are **indistinguishable in throughput, latency and CPU**. They differ clearly
in **footprint**: Go uses about 10x less memory, starts about 9x faster and ships a 9x smaller image. Every run sold
exactly the stock, and reconciled. The most valuable output of the load test was not the comparison but **three real
bugs it found**, fixed in this phase.

![Go vs Java, median of 3 rounds, whiskers = min–max](img/benchmark.svg)

## Method

| | |
|---|---|
| Scenario | Flash sale of one SKU with **100 units**; `USERS` logged-in users through the gateway (`loadtest/flashsale.js`) |
| Load | k6, **600 VUs**, ramp 20 s, hold 40 s; each VU posts an order, then sleeps 1–3 s; 15% of orders are retried with the same `Idempotency-Key` |
| Order of runs | warm-up (300 VUs) then measured run; implementations alternate go, java, go, java, go, java (3 rounds each) |
| Same conditions | same compose stack and images, every app container limited to **2 CPUs / 1 GiB** (`APP_CPUS`, `APP_MEM`), stock strategy `atomic`, tracing off, gateway IP limit raised (all traffic comes from one k6 container) |
| Correctness after each run | `loadtest/reconcile.sh`: stock conserved, reserved = 0, confirmed = 100, no PENDING order, no duplicate key, 2 SENT events per order, 1 notification per order |
| Machine | Windows 11 laptop, 24 logical CPUs, 15.7 GB RAM; Docker Desktop VM with 7.6 GB; Oracle Free (limited to 2 CPU threads by its license) |
| Reproduce | `make up && make db-migrate && bash loadtest/run-benchmark.sh && node loadtest/aggregate.mjs` |

Raw data: `loadtest/results/phase6/` (k6 summaries, `docker stats` samples every second, reconcile output, `summary.json`).

## Results (median of 3 rounds, [min–max])

| Measure | inventory-go | inventory-java |
|---|---:|---:|
| Orders answered per second | 266 [264–266] | 265 [263–267] |
| Order latency p50 (ms) | 19.7 [19.5–20.4] | 18.2 [17.7–19.3] |
| Order latency p95 (ms) | 33 [31–34] | 32 [28–33] |
| Order latency p99 (ms) | 50 [42–74] | 53 [38–59] |
| Inventory CPU while busy (% of one core) | 39 [37–40] | 38 [32–40] |
| Inventory peak memory under load (MiB) | 26 [17–32] | 281 [272–305] |
| Cold start to ready (ms, 5 restarts) | 614 [604–679] | 5633 [5143–5920] |
| Image size (MiB) | 14.2 | 133.8 |
| Idle memory after start (MiB) | 6.2 | 169.7 |

| Run | orders/s | p95 ms | created | sold out | replays | unexpected | reconcile |
|---|---:|---:|---:|---:|---:|---:|---|
| go-r1 | 264 | 33 | 100 | 16564 | 2459 | 0 | PASS |
| java-r1 | 263 | 33 | 100 | 16584 | 2454 | 0 | PASS |
| go-r2 | 266 | 31 | 100 | 16579 | 2496 | 0 | PASS |
| java-r2 | 265 | 32 | 100 | 16605 | 2514 | 0 | PASS |
| go-r3 | 266 | 34 | 100 | 16545 | 2604 | 0 | PASS |
| java-r3 | 267 | 28 | 100 | 16701 | 2540 | 0 | PASS |

About 16 700 orders per run, 100 confirmed, the rest correctly refused with `OUT_OF_STOCK`; about 33 000 outbox events
per run, all relayed, and one notification per order. Zero oversell, zero duplicate, zero stuck order in all 6 runs.

## How to read it

- **Throughput and latency are equal because inventory is not the bottleneck.** k6 runs a closed loop (each VU waits
  for its answer, then thinks 1–3 s), so 600 VUs offer about 265 orders/s whichever service answers. Inventory used
  under 40% of one core of its 2-core budget in both languages; the latency is mostly the order service and Oracle.
  A p50 difference of 1.5 ms is within the noise of a laptop.
- **Where is the ceiling?** A 2000-VU run (`loadtest/results/overload-2000vu/`, before the fixes below) saturated the
  **order service (Java, 200% CPU of 2) and Oracle Free (2 threads)** first. With this stack the maximum is set by
  order + Oracle, not by the inventory language. A pure inventory comparison at saturation is the Phase 2 contract/bench
  suite (`loadtest/results/phase2-inventory/`).
- **What does differ** is cost per instance: memory (26 vs 281 MiB under load, 6 vs 170 MiB idle), cold start
  (0.6 vs 5.6 s) and image size (14 vs 134 MiB). That matters for autoscaling on a flash sale (new replicas must be
  ready in seconds), for density (replicas per node) and for pull time. The JVM numbers are a default Spring Boot
  4 / JDK 25 image without tuning (no CDS/AOT cache, no GraalVM native image, default heap sizing).

## Bugs the load test found (all fixed, with regression tests)

1. **Circuit breaker cascade after the sell-out** (order + gateway). Order's inventory breaker *ignored* business answers
   such as `OUT_OF_STOCK`. After the sell-out nearly every call is one, so the window held only a few real failures,
   half of them opened the breaker against a healthy inventory, and the gateway breaker, which counted the resulting
   503s, opened too: about 21 000 503s in a 300-VU trial. Fix: business answers count as successes; the gateway does
   not count 503 (a deliberate refusal) as an upstream failure. ADR 0005, ADR 0008.
2. **Orders stuck PENDING for ever** (order). A PENDING order only moved on when its client retried; clients that gave
   up on a timeout left 962 orders PENDING in the 2000-VU run. Fix: `PendingOrderRecovery` resumes abandoned orders
   every 10 s; one claimer per order (conditional `UPDATE`), index V5. ADR 0005.
3. **Oracle session churn in the Go services** (inventory-go, notification, outbox-worker). `SetMaxIdleConns(5)` was
   read as Hikari's "minimum idle", but `database/sql` closes every connection returned above it. Under a burst
   inventory-go opened and closed Oracle sessions non-stop until the listener refused them (`ORA-12516`): 56% of
   orders failed with 503 and p95 was 2.8 s, while Java (Hikari keeps up to 20 open) had 0 errors
   (`loadtest/results/pool-churn-600vu/`). Fix: idle connections = max connections, closed after 10 idle minutes.
   After the fix: 0 errors, p95 33 ms. This is exactly the "same behaviour in Go and Java" rule of the project.

Environment finding: the gvenzl Oracle Free image ships **2 x 10 MB redo logs**. Under load every session waited on
`log file switch (checkpoint incomplete)`. `make db-tune` (`db/tune-redo.sh`) replaces them by 3 x 512 MB;
`run-benchmark.sh` runs it first.

## Limits

- One laptop: k6, all services, Oracle and Kafka share the same CPUs and Docker VM; numbers are relative, not absolute.
- One load level for the comparison (600 VUs, under capacity), one stock strategy (`atomic`), one SKU.
- Three rounds per implementation: enough to see the spread, not for a statistical test.
- The JVM is measured untuned; tuning (CDS, smaller heap, native image) would shrink but not close the footprint gap.
