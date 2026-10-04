#!/usr/bin/env bash
# Short flash sale through the gateway: k6 thresholds (unexpected answers < 1%, p95 < 3 s) + database reconcile.
# Exit code is non-zero when a threshold is crossed or the reconcile finds a problem.
#   make up && make db-migrate && make up-apps && bash loadtest/smoke.sh
# Knobs: VUS=50 RAMP=5s HOLD=20s STOCK=20
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/.."
. scripts/lib.sh
load_env
set -a; . ./.env; set +a

VUS="${VUS:-50}"; RAMP="${RAMP:-5s}"; HOLD="${HOLD:-20s}"; STOCK="${STOCK:-20}"
OUT="${OUT:-loadtest/results/smoke}"
mkdir -p "$OUT"
sku="SMOKE-$(date +%s)"

printf 'CONNECT %s/%s@//localhost:1521/%s\nSET FEEDBACK OFF\nINSERT INTO product (sku, name, price) VALUES (%s, %s, 100);\nINSERT INTO stock (product_id, available) SELECT id, %s FROM product WHERE sku = %s;\nCOMMIT;\nEXIT\n' \
  "$APP_USER" "$APP_USER_PASSWORD" "${ORACLE_SERVICE:-FREEPDB1}" "'$sku'" "'smoke test'" "$STOCK" "'$sku'" \
  | docker exec -i flashsale-oracle sqlplus -s /nolog > /dev/null

echo "smoke: $VUS VUs, ramp $RAMP, hold $HOLD, stock $STOCK, sku $sku"
k6_exit=0
docker run --rm --network flashsale_default -v "$PWD/loadtest:/scripts:ro" -v "$PWD/$OUT:/out" \
  -e BASE_URL=http://gateway:8080 -e SKU="$sku" -e DEMO_PASSWORD="$DEMO_PASSWORD" -e USERS="$VUS" -e VUS="$VUS" \
  -e RAMP="$RAMP" -e HOLD="$HOLD" -e RUN_ID=smoke \
  grafana/k6 run --quiet --summary-export=/out/smoke.k6.json /scripts/flashsale.js || k6_exit=$?

rec_exit=0
bash loadtest/reconcile.sh "$sku" "$STOCK" "$OUT/smoke.reconcile.json" || rec_exit=$?
echo "k6 exit=$k6_exit reconcile exit=$rec_exit"
[ "$k6_exit" = 0 ] && [ "$rec_exit" = 0 ]
