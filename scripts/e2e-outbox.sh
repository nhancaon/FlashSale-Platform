#!/usr/bin/env bash
# Phase 4 acceptance, on the real stack: orders -> outbox_events -> 3 outbox workers -> Kafka -> notification.
# One worker is killed (SIGKILL) while events are flowing. Expected: no event is lost, and every ORDER_CONFIRMED event
# produces exactly one notification even if a crash made the worker publish some events twice.
# Needs: make up, make db-migrate, make up-apps (3 outbox-worker replicas).
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/.."
. scripts/lib.sh
load_env

ORDERS=${ORDERS:-150}
ORDER=http://localhost:8085
fail() { echo "FAIL: $*" >&2; exit 1; }
sql() {
  printf 'CONNECT %s/%s@//localhost:1521/%s\nSET HEADING OFF FEEDBACK OFF PAGESIZE 0\n%s\nEXIT\n' "$APP_USER" "$APP_USER_PASSWORD" "${ORACLE_SERVICE:-FREEPDB1}" "$1" \
    | docker exec -i flashsale-oracle sqlplus -s /nolog | tr -s '[:space:]' ' ' | sed 's/^ //; s/ $//'
}

curl -sf "$ORDER/readyz" > /dev/null || fail "order service is not ready (run make up-apps)"
mapfile -t WORKERS < <(docker ps --filter "name=flashsale-outbox-worker" --format '{{.Names}}')
[ "${#WORKERS[@]}" -ge 3 ] || fail "expected 3 outbox-worker containers, found ${#WORKERS[@]} (make up-apps OUTBOX_REPLICAS=3)"
echo "outbox workers: ${WORKERS[*]}"

# A dedicated product and user keep this run isolated from other data.
RUN="$RANDOM$RANDOM"
SKU="E2E-$RUN"
USER_ID="e2e-outbox-$RUN"
sql "INSERT INTO product (sku, name, price) VALUES ('$SKU', 'e2e outbox', 1);
INSERT INTO stock (product_id, available) SELECT id, $((ORDERS * 2)) FROM product WHERE sku = '$SKU';
COMMIT;" > /dev/null

echo "== 1. create $ORDERS orders (20 in parallel)"
create_one() {
  curl -s -o /dev/null -w '%{http_code}\n' -X POST "$ORDER/v1/orders" -H 'Content-Type: application/json' \
    -H "X-User-Id: $USER_ID" -H "Idempotency-Key: $USER_ID-$1" -d "{\"items\":[{\"sku\":\"$SKU\",\"qty\":1}]}"
}
export -f create_one; export ORDER USER_ID SKU
(
  # While orders are created and events are relayed, kill one worker hard.
  sleep 2
  echo "   >>> killing ${WORKERS[0]} (SIGKILL)"
  docker kill "${WORKERS[0]}" > /dev/null
) &
KILLER=$!
codes=$(seq 1 "$ORDERS" | xargs -P "${PARALLEL:-20}" -I{} bash -c "create_one {} || echo curl-failed" || true)
wait "$KILLER"
created=$(echo "$codes" | grep -c '^201$' || true)
echo "   $created orders created (201)"
[ "$created" = "$ORDERS" ] || fail "expected $ORDERS orders, got $created ($(echo "$codes" | sort | uniq -c | tr '\n' ' '))"

echo "== 2. wait for the surviving workers to relay everything"
total_events=$((ORDERS * 2)) # ORDER_CREATED + ORDER_CONFIRMED per order
for _ in $(seq 1 90); do
  sent=$(sql "SELECT COUNT(*) FROM outbox_events e JOIN orders o ON o.id = e.aggregate_id WHERE o.user_id = '$USER_ID' AND e.status = 'SENT';")
  [ "$sent" = "$total_events" ] && break
  sleep 1
done
[ "$sent" = "$total_events" ] || fail "only $sent of $total_events events were SENT: events were lost or stuck"
echo "   all $total_events events SENT (none lost, none FAILED)"
failed=$(sql "SELECT COUNT(*) FROM outbox_events e JOIN orders o ON o.id = e.aggregate_id WHERE o.user_id = '$USER_ID' AND e.status = 'FAILED';")
[ "$failed" = 0 ] || fail "$failed events are FAILED"

echo "== 3. wait for the notification service"
for _ in $(seq 1 60); do
  notified=$(sql "SELECT COUNT(DISTINCT n.event_id) FROM notifications n JOIN orders o ON o.id = n.order_id WHERE o.user_id = '$USER_ID';")
  [ "$notified" = "$ORDERS" ] && break
  sleep 1
done
rows=$(sql "SELECT COUNT(*) FROM notifications n JOIN orders o ON o.id = n.order_id WHERE o.user_id = '$USER_ID';")
[ "$notified" = "$ORDERS" ] || fail "$notified distinct notified events, expected $ORDERS"
[ "$rows" = "$ORDERS" ] || fail "$rows notification rows for $ORDERS orders: a duplicate was sent"
echo "   $rows notifications for $ORDERS confirmed orders: exactly one each"

dups=$(docker exec flashsale-notification-1 wget -qO- http://localhost:8080/metrics 2>/dev/null | grep 'notification_processed_total{result="duplicate"}' | awk '{print $2}' || true)
echo "   duplicate deliveries absorbed by the notification inbox (all runs so far): ${dups:-n/a}"

echo "== 4. restart the killed worker"
docker start "${WORKERS[0]}" > /dev/null
echo "E2E OUTBOX OK ($ORDERS orders, 3 workers, one killed mid-flight)"
