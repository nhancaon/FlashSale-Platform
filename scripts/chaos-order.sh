#!/usr/bin/env bash
# Simple chaos test: kill the inventory container, check that orders fail fast (breaker opens, no hanging
# threads), restart it, and check that the system heals by itself.
# Needs: make up, make db-migrate, make up-apps (INVENTORY_IMPL=go|java).
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/.."

ORDER=http://localhost:8085
if curl -sf localhost:8083/readyz > /dev/null; then IMPL=go; INV_PORT=8083
elif curl -sf localhost:8084/readyz > /dev/null; then IMPL=java; INV_PORT=8084
else echo "no inventory service answers (run make up-apps)" >&2; exit 1; fi
CONTAINER="flashsale-inventory-$IMPL-1"
echo "chaos target: $CONTAINER"

fail() { echo "FAIL: $*" >&2; docker start "$CONTAINER" > /dev/null 2>&1 || true; exit 1; }
order() { # prints "<http-code> <ms>" for a fresh order
  curl -s -o /tmp/chaos-body.json -w '%{http_code} %{time_total}' -X POST "$ORDER/v1/orders" \
    -H 'Content-Type: application/json' -H "X-User-Id: chaos-$RANDOM" -H "Idempotency-Key: k-$RANDOM$RANDOM" \
    -d '{"items":[{"sku":"SKU-HEADSET","qty":1}]}'
}
breaker_state() {
  curl -s "$ORDER/metrics" | grep -E 'resilience4j_circuitbreaker_state\{name="inventory",state="open"\}' | awk '{print $2}'
}

echo "== 1. healthy baseline"
read -r code t <<< "$(order)"
[ "$code" = 201 ] || fail "baseline order expected 201, got $code"
echo "   201 in ${t}s"

echo "== 2. kill inventory"
docker stop "$CONTAINER" > /dev/null
codes=""
for i in $(seq 1 12); do read -r code t <<< "$(order)"; codes="$codes $code"; done
echo "   status codes while inventory is down:$codes"
for c in $codes; do [ "$c" = 503 ] || fail "every order must fail with 503 while inventory is down (got $c)"; done
[ "$(breaker_state)" = "1.0" ] || fail "circuit breaker should be OPEN"
echo "   breaker is OPEN"

echo "== 3. open breaker fails fast and the order service stays responsive"
read -r code t <<< "$(order)"
[ "$code" = 503 ] || fail "expected 503, got $code"
awk -v t="$t" 'BEGIN { exit !(t < 0.5) }' || fail "an order took ${t}s with the breaker open (should fail fast)"
curl -sf "$ORDER/healthz" > /dev/null || fail "order service stopped answering /healthz"
echo "   503 in ${t}s, /healthz still answers"

echo "== 4. restart inventory and wait for self healing"
docker start "$CONTAINER" > /dev/null
for _ in $(seq 1 60); do curl -sf "localhost:$INV_PORT/readyz" > /dev/null && break; sleep 1; done
curl -sf "localhost:$INV_PORT/readyz" > /dev/null || fail "inventory did not come back"
healed=no
for _ in $(seq 1 30); do   # breaker waits 10 s in OPEN, then half-open probes close it
  read -r code t <<< "$(order)"
  if [ "$code" = 201 ]; then healed=yes; break; fi
  sleep 1
done
[ "$healed" = yes ] || fail "orders did not recover after inventory came back"
[ "$(breaker_state)" = "0.0" ] || fail "breaker should be CLOSED again"
echo "   201 again, breaker CLOSED"

echo "CHAOS OK ($IMPL inventory)"
