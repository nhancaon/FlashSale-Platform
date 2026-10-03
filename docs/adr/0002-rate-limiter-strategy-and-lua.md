# ADR 0002: Rate limiter as Strategy + Redis Lua

Status: accepted (Phase 1)

## Context
The gateway must limit per user and per IP across several instances, so state must be shared and each
decision atomic. Go and Java must behave identically.

## Decision
- Three algorithms behind one interface (Strategy), built by a factory from `RATELIMIT_ALGORITHM`:
  `token_bucket`, `sliding_window` (sorted-set log), `fixed_window`.
- Each decision is one Lua script run in Redis, so the read-modify-write is atomic with no locks.
- The **same three `.lua` files** are used by both services (`make check-lua` fails if they drift).
- Time is passed to the script as `now_ms` from an injectable clock (not Redis `TIME`). This makes tests
  deterministic with a fake clock. Cost: instance clocks must be roughly in sync (NTP); skew only shifts
  window edges by the skew.
- Sliding-window members are `instanceId-sequence`, unique even within one millisecond or across instances.
- Redis errors surface as HTTP 503 `BACKEND_UNAVAILABLE` (fail closed at this service). The caller (gateway)
  decides whether to fail open.

## Trade-offs
| Algorithm | Memory | Boundary burst | Notes |
|---|---|---|---|
| fixed_window | 1 counter | up to 2x limit around a boundary (tested) | cheapest |
| sliding_window | O(limit) per key | none (tested) | exact, heaviest |
| token_bucket | 1 hash | allows bursts up to capacity | smooth refill; default |

## Verified by tests (both languages)
Exactly `limit` of 1000 concurrent requests pass for every algorithm; `retryAfterMs` is honest (waiting that
long lets one request through); keys expire after the window so Redis state does not leak.
