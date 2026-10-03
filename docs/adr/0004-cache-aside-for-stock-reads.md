# ADR 0004: Cache-aside for stock reads

Status: accepted (Phase 2)

## Context
`GET /v1/inventory/{sku}` is read far more often than stock changes during a sale. Oracle must stay the source
of truth.

## Decision
Cache-aside in Redis, key `inv:stock:{sku}`, value `available:reserved`, TTL 2 s (`CACHE_TTL_MS`).
- Read: Redis hit returns; miss reads Oracle and fills the cache.
- Write: after the transaction **commits**, delete the key (a rolled back transaction changes nothing, so the
  cache is left alone).
- Redis is best effort: any Redis error counts as a miss (reads) or a warning (writes). Redis down never fails a request.
- Redis timeouts are short (100 ms read/write) so a slow Redis cannot slow the API.
- Disable with `CACHE_ENABLED=false`.

## Trade-offs
Delete-after-commit leaves a small window: a reader that loaded the old row just before the commit can re-fill the
old value after our delete. The TTL bounds that staleness to 2 s. Reads are therefore *eventually* consistent;
reserve/release/confirm never read the cache, so the correctness of stock is unaffected.

## Verified
Contract test `TestReserveIsIdempotent` reads through the API right after a reserve and sees the new numbers
(the invalidation works); unit tests check that the cache is invalidated only after a successful commit.
