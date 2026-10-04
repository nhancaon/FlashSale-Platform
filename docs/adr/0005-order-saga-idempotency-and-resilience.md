# ADR 0005: Order saga, idempotency and resilience toward inventory

Status: accepted (Phase 3)

## Context
`POST /v1/orders` touches two systems that cannot share a transaction: Oracle (orders) and the inventory service
(stock). Clients retry, inventory can be slow or down, and a half done order must never leak stock or lose money.

## Decision
**Idempotency.** `Idempotency-Key` + `X-User-Id` map to the unique constraint `uq_order_idem (user_id, idempotency_key)`.
- Known key, same items: if the order is finished return **200** with the stored result; if the first request is
  still running return **409 REQUEST_IN_PROGRESS** (with `Retry-After`); different items: **409 IDEMPOTENCY_KEY_REUSED**.
- Two requests racing on a new key: one inserts, the other gets ORA-00001 and takes the replay path.
- A PENDING order untouched for `ORDER_STALE_AFTER_MS` (30 s) counts as abandoned: a retry with the same key
  **resumes** the saga. Whoever resumes first *claims* the order (`UPDATE orders SET updated_at = SYSTIMESTAMP WHERE id = :id
  AND status = 'PENDING' AND updated_at < now - stale`): one winner per stale period, the others get 409 in progress.
- **Recovery job** (`PendingOrderRecovery`, added in Phase 6b): every `ORDER_RECOVERY_INTERVAL_MS` (10 s) it claims up to
  `ORDER_RECOVERY_BATCH` (50) stale PENDING orders, oldest first, and resumes them. The 2000-VU load test showed why it is
  needed: clients that gave up on a timeout never retried, and 962 orders stayed PENDING for ever (no final event, no
  notification). Index `ix_orders_status_updated` (V5) keeps the scan cheap.

**Orchestration saga**, items processed in sku order: reserve each item, charge (simulated), confirm each item.
- Before payment succeeds, any failure compensates (`release` every reserved item) and the order becomes **FAILED**
  with a reason (`OUT_OF_STOCK`, `PAYMENT_DECLINED`, `INVENTORY_UNAVAILABLE`).
- **Payment is the pivot.** After it succeeds nothing is released; if confirm then fails the order stays **PENDING**
  (HTTP 202) and a retry finishes it forward. If a release fails during compensation the order also stays PENDING.
- All inventory operations and the payment are idempotent, so resuming and retrying are always safe.
- No database connection is held while calling other services. Transaction 1 saves order + items + `ORDER_CREATED`;
  the saga runs; transaction 2 sets the final status + `ORDER_CONFIRMED` / `ORDER_FAILED`. Only the request that actually
  moves the order out of PENDING writes the event (`UPDATE ... WHERE status = 'PENDING'`), so events are never duplicated.

**Resilience toward inventory** (Resilience4j 2.4 core, configured in code, ADR 0001 notes the Spring Boot 4 starter
exists but is not needed):
| Mechanism | Setting | Why |
|---|---|---|
| Timeout | connect 500 ms, read 2 s (JDK HttpClient) | a slow dependency must not hold callers |
| Retry | 3 attempts, exponential backoff 50 ms x2 with jitter, only transient errors | safe because calls are idempotent; jitter avoids retry storms |
| Circuit breaker | 20-call window, opens at 50% technical failures, 10 s open, then 5 probes | stop hammering a dead service, fail in milliseconds |
| Bulkhead | 50 concurrent calls, 200 ms wait | bound the load on inventory and the work piling up |
Business answers (409 OUT_OF_STOCK, other 4xx) are not retried and count as **successes** for the breaker: inventory
answered correctly. (The first version *ignored* them. The load test exposed the bug: after the sell-out nearly every
call is OUT_OF_STOCK, the window held only a handful of real failures, half of those opened the breaker against a
healthy inventory, and the gateway breaker followed: about 21 000 503s in a 300-VU run. Regression test in
`InventoryGatewayTests`.) Each retry attempt passes
through the breaker, so failures are counted per attempt.

## Consequences
- Verified by tests (unit + Oracle + fake inventory) and by `make chaos` against the real stack: with inventory killed,
  12 of 12 orders answer 503 `INVENTORY_UNAVAILABLE`, the breaker opens, an order fails in about 90 ms, `/healthz` keeps
  answering, and the system heals by itself after the restart.
- A crash between "payment succeeded" and the final transaction leaves a PENDING order for at most the stale period plus
  one recovery interval (about 40 s); then the recovery job (or a client retry, whichever comes first) finishes it.
- The payment gateway is simulated in-process (`PAYMENT_FAIL_RATE`, deterministic per order id); the saga code only
  depends on the `PaymentGateway` port.
- Migration V3 adds `orders.failure_reason` and `orders.updated_at`. TIMESTAMP columns hold the database server wall
  clock (UTC in our containers); Java reads them as `LocalDateTime` interpreted as UTC. Reading them as
  `java.sql.Timestamp` would apply the JVM time zone and shift values by hours (this bug made every order look stale
  during development: the staleness check now compares `CAST(SYSTIMESTAMP AS TIMESTAMP)` inside the database).
- `order_requests_total` is deliberately not named `order_created_total`: the Prometheus client reserves the `_created`
  suffix and silently renamed the metric.
