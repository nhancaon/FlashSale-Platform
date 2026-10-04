#!/usr/bin/env bash
# Reconciliation after a flash sale run: the database must tell a consistent story.
#   bash loadtest/reconcile.sh <sku> <initial-stock> [json-output-file]
# Checks (exit 1 if any fails):
#   1. available + reserved + confirmed quantity = initial stock           (nothing created or lost)
#   2. available >= 0 and reserved = 0 once the run is over                (no oversell, nothing stuck reserved)
#   3. confirmed orders = initial stock when demand exceeds it             (every unit sold exactly once)
#   4. no PENDING order is left                                            (every saga finished)
#   5. no duplicate (user, Idempotency-Key)                                (retries never created a second order)
#   6. every order has its outbox events, all SENT, and exactly one notification per order
# Afterwards the run's data is deleted (KEEP_DATA=1 keeps it).
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/.."
. scripts/lib.sh
load_env

SKU="${1:?sku}"; INITIAL="${2:?initial stock}"; OUT="${3:-}"
q() { # one scalar
  printf 'CONNECT %s/%s@//localhost:1521/%s\nSET HEADING OFF FEEDBACK OFF PAGESIZE 0 LINESIZE 200\n%s\nEXIT\n' "$APP_USER" "$APP_USER_PASSWORD" "${ORACLE_SERVICE:-FREEPDB1}" "$1" \
    | docker exec -i flashsale-oracle sqlplus -s /nolog | tr -s '[:space:]' ' ' | sed 's/^ //; s/ $//'
}
ORDERS_OF_SKU="SELECT DISTINCT oi.order_id FROM order_item oi JOIN product p ON p.id = oi.product_id WHERE p.sku = '$SKU'"

echo "waiting for the outbox to drain..."
for _ in $(seq 1 180); do
  new=$(q "SELECT COUNT(*) FROM outbox_events WHERE status = 'NEW' AND aggregate_id IN ($ORDERS_OF_SKU);")
  [ "$new" = 0 ] && break
  sleep 1
done

available=$(q "SELECT s.available FROM stock s JOIN product p ON p.id = s.product_id WHERE p.sku = '$SKU';")
reserved=$(q "SELECT s.reserved FROM stock s JOIN product p ON p.id = s.product_id WHERE p.sku = '$SKU';")
confirmed_qty=$(q "SELECT NVL(SUM(r.qty), 0) FROM reservation r JOIN product p ON p.id = r.product_id WHERE p.sku = '$SKU' AND r.status = 'CONFIRMED';")
orders=$(q "SELECT COUNT(*) FROM orders WHERE id IN ($ORDERS_OF_SKU);")
confirmed=$(q "SELECT COUNT(*) FROM orders WHERE status = 'CONFIRMED' AND id IN ($ORDERS_OF_SKU);")
failed=$(q "SELECT COUNT(*) FROM orders WHERE status = 'FAILED' AND id IN ($ORDERS_OF_SKU);")
pending=$(q "SELECT COUNT(*) FROM orders WHERE status = 'PENDING' AND id IN ($ORDERS_OF_SKU);")
dups=$(q "SELECT COUNT(*) FROM (SELECT user_id, idempotency_key FROM orders WHERE id IN ($ORDERS_OF_SKU) GROUP BY user_id, idempotency_key HAVING COUNT(*) > 1);")
events=$(q "SELECT COUNT(*) FROM outbox_events WHERE aggregate_id IN ($ORDERS_OF_SKU);")
events_sent=$(q "SELECT COUNT(*) FROM outbox_events WHERE status = 'SENT' AND aggregate_id IN ($ORDERS_OF_SKU);")
# Every finished order notifies the customer (EMAIL when confirmed, SMS when failed). Asynchronous: wait for it.
# Both counts come from one query (one snapshot); keep waiting while the consumer still makes progress.
notified=0; notif_rows=0; last=-1; idle=0
for _ in $(seq 1 600); do
  read -r notif_rows notified <<< "$(q "SELECT COUNT(*) || ' ' || COUNT(DISTINCT event_id) FROM notifications WHERE order_id IN ($ORDERS_OF_SKU);")"
  [ "$notified" = "$orders" ] && break
  if [ "$notif_rows" = "$last" ]; then idle=$((idle + 1)); [ "$idle" -ge 15 ] && break; else idle=0; fi
  last=$notif_rows; sleep 1
done

problems=()
[ "$((available + reserved + confirmed_qty))" = "$INITIAL" ] || problems+=("available($available)+reserved($reserved)+confirmed($confirmed_qty) != initial($INITIAL)")
[ "$available" -ge 0 ] || problems+=("negative stock: $available")
[ "$reserved" = 0 ] || problems+=("$reserved units still reserved")
[ "$confirmed" = "$INITIAL" ] || problems+=("confirmed orders $confirmed != initial stock $INITIAL (not sold out, or oversold)")
[ "$pending" = 0 ] || problems+=("$pending orders still PENDING")
[ "$dups" = 0 ] || problems+=("$dups duplicated idempotency keys")
[ "$events" = "$((orders * 2))" ] || problems+=("$events outbox events for $orders orders (expected 2 each)")
[ "$events_sent" = "$events" ] || problems+=("only $events_sent of $events outbox events SENT")
[ "$notified" = "$orders" ] && [ "$notif_rows" = "$orders" ] || problems+=("notifications: $notif_rows rows / $notified distinct for $orders finished orders")

status=PASS; [ ${#problems[@]} -eq 0 ] || status=FAIL
printf 'reconcile %s: initial=%s available=%s reserved=%s confirmed_qty=%s | orders=%s confirmed=%s failed=%s pending=%s | duplicates=%s | events=%s sent=%s | notifications=%s\n' \
  "$status" "$INITIAL" "$available" "$reserved" "$confirmed_qty" "$orders" "$confirmed" "$failed" "$pending" "$dups" "$events" "$events_sent" "$notif_rows"
for p in "${problems[@]:-}"; do [ -n "$p" ] && echo "  PROBLEM: $p"; done

if [ -n "$OUT" ]; then
  printf '{"status":"%s","initial":%s,"available":%s,"reserved":%s,"confirmedQty":%s,"orders":%s,"confirmed":%s,"failed":%s,"pending":%s,"duplicates":%s,"events":%s,"eventsSent":%s,"notifications":%s}\n' \
    "$status" "$INITIAL" "$available" "$reserved" "$confirmed_qty" "$orders" "$confirmed" "$failed" "$pending" "$dups" "$events" "$events_sent" "$notif_rows" > "$OUT"
fi

if [ "${KEEP_DATA:-0}" != 1 ]; then
  q "DELETE FROM notifications WHERE order_id IN ($ORDERS_OF_SKU);
DELETE FROM outbox_events WHERE aggregate_id IN ($ORDERS_OF_SKU);
DELETE FROM order_item WHERE order_id IN ($ORDERS_OF_SKU);
DELETE FROM orders WHERE id NOT IN (SELECT order_id FROM order_item) AND user_id LIKE '${USER_PREFIX:-bench}-%';
DELETE FROM reservation WHERE product_id = (SELECT id FROM product WHERE sku = '$SKU');
DELETE FROM stock WHERE product_id = (SELECT id FROM product WHERE sku = '$SKU');
DELETE FROM product WHERE sku = '$SKU';
COMMIT;" > /dev/null
fi
[ "$status" = PASS ]
