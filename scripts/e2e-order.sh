#!/usr/bin/env bash
# End-to-end check through the real stack: order -> inventory (go or java) -> Oracle -> outbox.
# Needs: make up, make db-migrate, make up-apps (INVENTORY_IMPL=go|java).
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/.."
. scripts/lib.sh
load_env

ORDER=http://localhost:8085
if curl -sf localhost:8083/readyz > /dev/null; then INV=http://localhost:8083; IMPL=go
elif curl -sf localhost:8084/readyz > /dev/null; then INV=http://localhost:8084; IMPL=java
else echo "no inventory service answers on 8083/8084 (run make up-apps)" >&2; exit 1; fi
curl -sf "$ORDER/readyz" > /dev/null || { echo "order service is not ready on 8085" >&2; exit 1; }
echo "inventory implementation: $IMPL"

fail() { echo "FAIL: $*" >&2; exit 1; }
json() { node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{const o=JSON.parse(s);const v=process.argv[1].split(".").reduce((a,k)=>a?.[k],o);console.log(v??"")})' "$1"; }
sql() { # run one query as the app user, print the raw rows
  printf 'CONNECT %s/%s@//localhost:1521/%s\nSET HEADING OFF FEEDBACK OFF PAGESIZE 0\n%s\nEXIT\n' "$APP_USER" "$APP_USER_PASSWORD" "${ORACLE_SERVICE:-FREEPDB1}" "$1" \
    | docker exec -i flashsale-oracle sqlplus -s /nolog | tr -s '[:space:]' ' ' | sed 's/^ //; s/ $//'
}
post_order() { # key body -> prints "<http-code> <body>"
  curl -s -w ' %{http_code}' -X POST "$ORDER/v1/orders" -H 'Content-Type: application/json' \
    -H "X-User-Id: $USER_ID" -H "Idempotency-Key: $1" -d "$2"
}

USER_ID="e2e-$RANDOM$RANDOM"
KEY="key-$RANDOM$RANDOM"
BODY='{"items":[{"sku":"SKU-IPHONE","qty":2},{"sku":"SKU-HEADSET","qty":1}]}'

before=$(curl -s "$INV/v1/inventory/SKU-IPHONE" | json available)
echo "SKU-IPHONE available before: $before"

echo "== 1. create order"
out=$(post_order "$KEY" "$BODY"); code="${out##* }"; resp="${out% *}"
[ "$code" = 201 ] || fail "expected 201, got $code: $resp"
[ "$(echo "$resp" | json status)" = CONFIRMED ] || fail "order not CONFIRMED: $resp"
ORDER_ID=$(echo "$resp" | json id)
echo "   201 CONFIRMED, order $ORDER_ID, total $(echo "$resp" | json total)"

echo "== 2. replay with the same Idempotency-Key"
out=$(post_order "$KEY" "$BODY"); code="${out##* }"; resp="${out% *}"
[ "$code" = 200 ] || fail "replay expected 200, got $code: $resp"
[ "$(echo "$resp" | json id)" = "$ORDER_ID" ] || fail "replay returned another order: $resp"
echo "   200 same order id"

echo "== 3. exactly one order row and the right stock"
[ "$(sql "SELECT COUNT(*) FROM orders WHERE user_id = '$USER_ID' AND idempotency_key = '$KEY';")" = 1 ] || fail "expected one order row"
after=$(curl -s "$INV/v1/inventory/SKU-IPHONE" | json available)
[ "$((before - after))" = 2 ] || fail "stock should drop by 2 (before $before, after $after)"
echo "   1 order row, SKU-IPHONE $before -> $after"

echo "== 4. outbox events written in the same transaction"
events=$(sql "SELECT LISTAGG(event_type, ',') WITHIN GROUP (ORDER BY id) FROM outbox_events WHERE aggregate_id = '$ORDER_ID';")
[ "$events" = "ORDER_CREATED,ORDER_CONFIRMED" ] || fail "unexpected outbox events: $events"
echo "   $events"

echo "== 5. out of stock compensates and fails the order"
laptop_before=$(curl -s "$INV/v1/inventory/SKU-LAPTOP" | json available)
out=$(post_order "key-oos-$RANDOM" '{"items":[{"sku":"SKU-HEADSET","qty":1},{"sku":"SKU-LAPTOP","qty":999}]}'); code="${out##* }"; resp="${out% *}"
[ "$code" = 409 ] || fail "expected 409, got $code: $resp"
[ "$(echo "$resp" | json code)" = OUT_OF_STOCK ] || fail "expected OUT_OF_STOCK: $resp"
[ "$(curl -s "$INV/v1/inventory/SKU-LAPTOP" | json available)" = "$laptop_before" ] || fail "laptop stock changed"
failed_events=$(sql "SELECT LISTAGG(event_type, ',') WITHIN GROUP (ORDER BY id) FROM outbox_events WHERE aggregate_id = '$(echo "$resp" | json orderId)';")
[ "$failed_events" = "ORDER_CREATED,ORDER_FAILED" ] || fail "unexpected outbox events: $failed_events"
echo "   409 OUT_OF_STOCK, stock untouched, events $failed_events"

echo "E2E OK ($IMPL inventory)"
