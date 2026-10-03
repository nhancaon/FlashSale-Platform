# ADR 0006: Outbox relay with SELECT ... FOR UPDATE SKIP LOCKED

Status: accepted (Phase 4)

## Context
The order service writes `outbox_events` in the same transaction as the order (ADR 0005). Something must move those
rows to Kafka, many workers at once, without sending an event twice, losing one, or reordering one order's events.

## Decision
A pool of competing workers (goroutines in one process, and several processes/containers). Each worker loops:
**claim** a batch (`FOR UPDATE SKIP LOCKED`), **publish** it to Kafka, set `SENT`, **commit**. The database is the queue.

**Claiming under Oracle's rules** (measured on Oracle 23ai, not assumed):
- `FETCH FIRST n ROWS ONLY ... FOR UPDATE` is rejected (ORA-02014).
- `WHERE ROWNUM <= n ... FOR UPDATE SKIP LOCKED` works but is applied *before* locked rows are skipped: with another
  session holding rows 1-5, a second session got **0 rows** although more were free. Workers would starve each other.
- Oracle locks rows **as they are fetched**: a cursor that fetches 5 rows locks 5 rows, and a second session gets rows 6-10.
  So the worker runs the plain `... ORDER BY id FOR UPDATE SKIP LOCKED`, reads exactly `batch` rows and closes the cursor.
  The driver prefetch is set to the batch size (`PREFETCH_ROWS`) so the driver does not fetch (lock) more than that.
  Verified by `TestConcurrentClaimsAreDisjoint`: two workers claiming at once get disjoint batches of the full size.

**Per-order ordering.** A row is claimable only if no *older NEW* event of the same aggregate exists. An older event that
is being sent (locked, still NEW) or waiting for its retry therefore holds back the newer one, so `ORDER_CONFIRMED` can
never overtake `ORDER_CREATED`. Kafka messages are keyed by `aggregate_id`, so one order stays in one partition.
(Index `ix_outbox_aggregate`, migration V4.)

**Delivery is at-least-once.** If a worker dies after Kafka accepted a batch but before its commit, the locks vanish, the
rows are NEW again and are sent again. Consumers must be idempotent (ADR 0007). Test: a crash is injected after the
publish; no event is lost and the topic shows the duplicates (32 of 92 messages in one run).

**Retry, backoff, give up.** A failed event gets `attempts + 1` and `next_attempt_at = now + backoff` (1 s doubling to 60 s).
At 20 attempts (about 15 minutes of outage) it becomes `FAILED` with `last_error`, instead of retrying forever;
a `FAILED` event does not block newer events of the same order. Re-sending a FAILED event is a manual `UPDATE ... SET status = 'NEW'`.
The batch is published in one Kafka write; broker errors are mapped back to individual events.

**Graceful shutdown.** On SIGTERM a worker finishes the batch it already claimed (the in-flight work runs on a context that
ignores the cancellation, bounded by 30 s) and starts no new one. Cancelling mid-batch would roll the transaction back
after Kafka already took the messages, causing duplicates for nothing.

**Topic readiness.** `EnsureTopic` creates the topic (3 partitions) and waits until every partition has a leader. Without
the wait, the first writes after creation fail with "leader not available" and burn delivery attempts (this made a test flaky).

## Trade-offs
- A database transaction (and its row locks) stays open during the Kafka write, a few milliseconds per batch. The alternative
  (claim with a `PROCESSING` status + lease, then publish outside the transaction) needs a reaper for abandoned claims.
- One event per aggregate per batch (the ordering gate), so a single very busy order is relayed one event per round trip.
- Metrics: `outbox_published_total`, `outbox_failed_attempts_total`, `outbox_gave_up_total`, `outbox_batch_duration_seconds`,
  `outbox_pending_events` (relay lag).

## Verified
Unit tests (fakes, `-race`), integration tests on real Oracle + Kafka (3 workers: each of 180 events exactly once and in
order per aggregate; crash after publish loses nothing; failing events end FAILED), and `make e2e-outbox`: 150 orders, 3 worker
containers, one killed with SIGKILL mid-flight, all 300 events SENT, 150 notifications.
