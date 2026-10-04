# ADR 0008: Gateway chain, JWT and rate limiting

Status: accepted (Phase 5)

## Context
The gateway is the only public entry point. It must authenticate callers, throttle abusers, and keep a failing service
from taking everything down, without letting clients influence what the internal services trust.

## Decision
**Chain of Responsibility** (each link may answer the request itself):
`request id -> logging + metrics -> IP rate limit -> JWT -> user rate limit -> circuit breaker -> reverse proxy`.
The IP limit runs *before* the JWT check so an unauthenticated flood (or a brute force on `/auth/login`) is rejected
cheaply; the user limit needs the identity the JWT step established.

**JWT (simulated identity provider).** `POST /auth/login` accepts any well formed username (`[A-Za-z0-9_.-]{1,64}`) with the
demo password and returns an HS256 token (15 min). Verification accepts **only HS256** with the right secret, requires `exp`,
`sub` and the issuer, so `alg: none` and algorithm confusion are rejected (tested). The secret must be at least 32 bytes and
comes from the environment. Wrong user and wrong password give the same answer; the password is compared in constant time.

**Trust boundary.** Upstream services trust `X-User-Id`, so the gateway deletes any client supplied `X-User-Id` and
`Authorization` header and sets `X-User-Id` from the verified token only (test: a spoofed header is ignored, the order
belongs to the token's user). `X-Forwarded-For` is honoured only with `TRUST_PROXY=true`, otherwise a client could pick its
own rate limit key. Only what clients need is exposed: orders (GET, POST) and stock (GET). Reserve, release and confirm
are internal (405 at the gateway).

**Rate limiting** through the `limiter.Limiter` interface of ratelimiter-go, in two interchangeable modes (`RATELIMIT_MODE`):
- `embedded` (default): the `ratelimiter-go/pkg/limiter` library runs inside the gateway with Redis as shared state.
  No extra network hop; cost is coupling at build time (a `go.mod` replace; Docker build context is `services/`).
- `remote`: calls the standalone ratelimiter service (`POST /v1/check`, Go or Java). One more hop and a new failure mode,
  but one place to change limits and to operate.
Defaults: 20 requests per second per user, 100 per second per IP. A rejection answers **429** with `Retry-After` (whole
seconds, rounded up, at least 1) and `X-RateLimit-Limit/Remaining`.
**Fail open by default** (`RATELIMIT_FAIL_OPEN=true`): if Redis or the limiter is down the request is let through and an error
is logged and counted. For a flash sale, losing sales because the limiter is down is worse than losing a little protection
for a while; set it to `false` for endpoints where abuse matters more (it then answers 503).

**Circuit breaker per upstream** (sony/gobreaker): opens when at least half of at least 10 recent requests failed, stays open 10 s,
then lets 5 probes through. A failure is a transport error, a timeout or an HTTP 5xx other than 503; **4xx, including business 409s such as
OUT_OF_STOCK, are healthy answers** (tested with 50 consecutive 409s). **503 is a healthy answer too**: a fast, deliberate
refusal by a service that is up (load shedding, or its own dependency is down). Counting it let order's inventory breaker
open the gateway breaker as well and block every order route (cascade seen in the Phase 6b load test; regression test
`TestUpstream503DoesNotTripTheBreaker`). A 5xx is still relayed to the client while it is counted.
Open breaker: 503 `UPSTREAM_UNAVAILABLE` with `Retry-After`, without calling the upstream. Timeout: 504; unreachable: 502.
Each upstream has its own breaker, so a dead inventory does not block orders.

**Health aggregation.** `/healthz` is liveness. `/readyz` checks order, inventory and Redis (or the ratelimiter) concurrently
with a 1 s timeout and answers 200 `ok` or 503 `degraded` with a status per component.

## Consequences
- A request rejected by a middleware never reaches the inner route, so metrics count it under the group pattern (`/api/*`).
- The response carries exactly one `X-Request-Id`: `ReverseProxy` adds upstream headers on top of the gateway's, so the
  upstream's copy is removed.
- No retries at the gateway: order creation is idempotent, so the client retries with the same key.
- JWT is HS256 with one shared secret and no rotation or revocation; fine for a simulated IdP, not for production.
- Verified: unit tests per link, a full chain test, a test on real Redis (exactly the limit passes), and `make e2e-gateway`
  (login, 401, idempotent order, ownership, read-only stock, 429 under a parallel burst).
