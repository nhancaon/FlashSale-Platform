# ADR 0003: Stock strategies and idempotent reservations

Status: accepted (Phase 2)

## Context
Flash sale: thousands of buyers race for a few units. The database must never oversell, and a retried request
(same order) must not take stock twice. inventory-go and inventory-java share one table set and one contract.

## Decision
**Reservation, not decrement.** `reserve` inserts a `reservation` row (unique on `(order_id, product_id)`) and moves
units `available -> reserved` in one transaction. `release` moves them back, `confirm` removes them from `reserved`
(sold). Invariant, checked by tests and benchmarks: `available + reserved + confirmed = initial`.

**Idempotency comes from the unique constraint.** A replay (same orderId + sku) finds the existing row and returns
its reservation id without touching stock. Same key with a different qty is `409 IDEMPOTENCY_CONFLICT`; a replay of
a released reservation is `409 INVALID_STATE`. If two identical requests race, one hits ORA-00001; it rolls back
and answers with the winner's reservation. Release of an unknown reservation succeeds (`NOT_FOUND`), so saga
compensation is always safe.

**Three strategies behind one interface (`STOCK_STRATEGY`)**, same in Go and Java:

| Strategy | How | Strength | Weakness |
|---|---|---|---|
| `atomic` (default) | `UPDATE ... WHERE available >= :n`, check rows affected | one round trip, no lock held across calls | needs a condition expressible in one statement |
| `pessimistic` | `SELECT ... FOR UPDATE`, check, `UPDATE` | easy to extend with complex rules | 2 round trips, lock held longer (about 1.6x slower here) |
| `optimistic` | read `version`, `UPDATE ... WHERE version = :v`, bounded retry with jitter | no lock while thinking | retry storms on a hot row (about 2.3x slower), can give up with 503 `CONTENTION` |

Oracle READ COMMITTED re-evaluates the `WHERE` after waiting for a row lock, which is why the `atomic` UPDATE is
correct without raising the isolation level.

**Races between release and confirm** are handled by conditional status updates (`WHERE status = 'RESERVED'`) and a
re-read when the update affects 0 rows: whoever loses gets `INVALID_STATE`, stock moves exactly once.

**Oracle errors map to meaningful codes**: ORA-00001 is the replay path; ORA-00060 and lock timeouts are
`409 CONFLICT_RETRY`; pool exhaustion, query timeout and unreachable DB are `503 DB_UNAVAILABLE`.

## Consequences
- One hot row serializes all buyers of a SKU. Fine for this project; scaling further means sharding stock across
  rows or buckets (not done).
- `optimistic` can fail a request while stock remains (503 CONTENTION). The oversell test therefore allows
  CONTENTION for that strategy only; it still requires no oversell and a consistent database.
- Driver: Spring Initializr picked `ojdbc17` for JDK 25 (spec said `ojdbc11`); go-ora v2 is the pure Go driver.
