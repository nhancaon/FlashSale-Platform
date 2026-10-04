| Measure | inventory-go | inventory-java |
|---|---:|---:|
| Orders answered per second (req/s, higher is better) | 266 [264–266] | 265 [263–267] |
| Order latency p50 (ms, lower is better) | 19.7 [19.5–20.4] | 18.2 [17.7–19.3] |
| Order latency p95 (ms, lower is better) | 33 [31–34] | 32 [28–33] |
| Order latency p99 (ms, lower is better) | 50 [42–74] | 53 [38–59] |
| Inventory CPU while busy (% of 1 core, lower is better) | 39 [37–40] | 38 [32–40] |
| Inventory peak memory (MiB, lower is better) | 26 [17–32] | 281 [272–305] |
| Cold start to ready (ms, 5 restarts) | 614 [604–679] | 5633 [5143–5920] |
| Image size (MiB) | 14.2 [14.2–14.2] | 133.8 [133.8–133.8] |
| Idle memory after start (MiB) | 6.2 [6.2–6.2] | 169.7 [169.7–169.7] |

| Run | orders/s | p50 ms | p95 ms | p99 ms | created | sold out | replays | 429 | unexpected | reconcile | inventory CPU % | order CPU % | Oracle CPU % |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---:|---:|---:|
| go-r1 | 264 | 19.7 | 33 | 74 | 100 | 16564 | 2459 | 0 | 0 | PASS | 40 | 82 | 123 |
| go-r2 | 266 | 19.5 | 31 | 42 | 100 | 16579 | 2496 | 0 | 0 | PASS | 37 | 74 | 118 |
| go-r3 | 266 | 20.4 | 34 | 50 | 100 | 16545 | 2604 | 0 | 0 | PASS | 39 | 78 | 129 |
| java-r1 | 263 | 19.3 | 33 | 53 | 100 | 16584 | 2454 | 0 | 0 | PASS | 40 | 86 | 123 |
| java-r2 | 265 | 18.2 | 32 | 59 | 100 | 16605 | 2514 | 0 | 0 | PASS | 38 | 76 | 104 |
| java-r3 | 267 | 17.7 | 28 | 38 | 100 | 16701 | 2540 | 0 | 0 | PASS | 32 | 72 | 107 |
