#!/usr/bin/env bash
# End-to-end through the public entry point: gateway -> order -> inventory -> Oracle.
# Needs: make up, make db-migrate, make up-apps.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/.."
. scripts/lib.sh
load_env
set -a; . ./.env; set +a

GW=http://localhost:8088
fail() { echo "FAIL: $*" >&2; exit 1; }
json() { node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{const o=JSON.parse(s);const v=process.argv[1].split(".").reduce((a,k)=>a?.[k],o);console.log(v??"")})' "$1"; }
status() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

curl -sf "$GW/healthz" > /dev/null || fail "gateway is not running on 8088 (make up-apps)"

echo "== 1. readiness aggregates the dependencies"
ready=$(curl -s "$GW/readyz")
[ "$(echo "$ready" | json status)" = ok ] || fail "gateway not ready: $ready"
echo "   $ready"

echo "== 2. no token, no access"
[ "$(status "$GW/api/orders/x")" = 401 ] || fail "expected 401 without a token"
[ "$(status -H 'Authorization: Bearer not.a.jwt' "$GW/api/orders/x")" = 401 ] || fail "expected 401 for a bad token"
echo "   401 without a token, 401 with a bad token"

echo "== 3. login"
[ "$(status -X POST "$GW/auth/login" -H 'Content-Type: application/json' -d '{"username":"alice","password":"wrong"}')" = 401 ] || fail "wrong password must be 401"
user="gw-$RANDOM$RANDOM"
token=$(curl -s -X POST "$GW/auth/login" -H 'Content-Type: application/json' -d "{\"username\":\"$user\",\"password\":\"$DEMO_PASSWORD\"}" | json accessToken)
[ -n "$token" ] || fail "login returned no token"
echo "   token for $user issued"

echo "== 4. create an order through the gateway (idempotent)"
key="gw-key-$RANDOM$RANDOM"
body='{"items":[{"sku":"SKU-HEADSET","qty":1}]}'
create() { curl -s -w ' %{http_code}' -X POST "$GW/api/orders" -H "Authorization: Bearer $token" -H "Idempotency-Key: $key" -H 'Content-Type: application/json' -H 'X-User-Id: someone-else' -d "$body"; }
out=$(create); code="${out##* }"; resp="${out% *}"
[ "$code" = 201 ] || fail "expected 201, got $code: $resp"
id=$(echo "$resp" | json id)
out=$(create); code="${out##* }"; resp="${out% *}"
[ "$code" = 200 ] && [ "$(echo "$resp" | json id)" = "$id" ] || fail "replay must be 200 with the same order: $code $resp"
echo "   201 then 200 replay, order $id"

echo "== 5. the order belongs to the token's user, not to the spoofed X-User-Id"
owner=$(printf 'CONNECT %s/%s@//localhost:1521/%s\nSET HEADING OFF FEEDBACK OFF PAGESIZE 0\nSELECT user_id FROM orders WHERE id = '"'"'%s'"'"';\nEXIT\n' "$APP_USER" "$APP_USER_PASSWORD" "${ORACLE_SERVICE:-FREEPDB1}" "$id" \
  | docker exec -i flashsale-oracle sqlplus -s /nolog | tr -d '[:space:]')
[ "$owner" = "$user" ] || fail "order owner is '$owner', expected '$user'"
[ "$(status -H "Authorization: Bearer $token" "$GW/api/orders/$id")" = 200 ] || fail "owner cannot read the order"
echo "   owner = $owner"

echo "== 6. read stock, but never change it from outside"
[ "$(status -H "Authorization: Bearer $token" "$GW/api/inventory/SKU-HEADSET")" = 200 ] || fail "GET stock should work"
[ "$(status -X POST -H "Authorization: Bearer $token" -H 'Content-Type: application/json' -d '{"orderId":"x","sku":"SKU-HEADSET","qty":1}' "$GW/api/inventory/reserve")" = 405 ] || fail "reserve must not be exposed"
echo "   GET 200, POST reserve 405"

echo "== 7. rate limiting: a burst from one user is throttled with 429 + Retry-After"
# Parallel, so the burst is faster than the refill rate (sequential curl calls take about 50 ms each).
export GW token
codes=$(seq 1 60 | xargs -P 30 -I{} curl -s -o /dev/null -w '%{http_code}
' -H "Authorization: Bearer $token" "$GW/api/inventory/SKU-HEADSET" || true)
n429=$(echo "$codes" | tr ' ' '\n' | grep -c '^429$' || true)
[ "$n429" -gt 0 ] || fail "no request was throttled in a burst of 60 (limit is ${RL_USER_LIMIT:-20}/s): $(echo $codes)"
# Read Retry-After from the headers of a throttled response of another parallel burst.
retry=$(seq 1 40 | xargs -P 40 -I{} sh -c 'curl -s -D - -o /dev/null -H "Authorization: Bearer $token" "$GW/api/inventory/SKU-HEADSET"' 2>/dev/null \
  | tr -d '\r' | awk -F': ' 'tolower($1)=="retry-after"{print $2; exit}' || true)
[ -n "$retry" ] || fail "a 429 response must carry a Retry-After header"
echo "   $n429 of 60 requests got 429 (Retry-After: ${retry:-n/a} s)"

echo "== 8. metrics"
curl -s "$GW/metrics" | grep -E '^gateway_ratelimit_rejected_total|^gateway_upstream_breaker_state' | head -4 | sed 's/^/   /'

echo "E2E GATEWAY OK"
