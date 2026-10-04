#!/usr/bin/env bash
# Phase 6 benchmark: the same flash sale against inventory-go and inventory-java, same machine, same container limits,
# repeated, with a reconcile of the database after every run.
#
#   make up && make db-migrate && bash loadtest/run-benchmark.sh
#
# Knobs (environment): IMPLS="go java"  ROUNDS=3  VUS=1000  RAMP=20s  HOLD=40s  STOCK=100  APP_CPUS=2  APP_MEM=1g
#                      STOCK_STRATEGY=atomic  OUT=loadtest/results/phase6
# Runs alternate between the implementations (go, java, go, java, ...) so slow drift of the machine does not favour one.
# Tracing is off during the benchmark; the gateway IP limit is raised because every request comes from one k6 container.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/.."
. scripts/lib.sh
load_env
set -a; . ./.env; set +a

IMPLS="${IMPLS:-go java}"
ROUNDS="${ROUNDS:-3}"
VUS="${VUS:-1000}"
RAMP="${RAMP:-20s}"
HOLD="${HOLD:-40s}"
STOCK="${STOCK:-100}"
OUT="${OUT:-loadtest/results/phase6}"
mkdir -p "$OUT/runs" "$OUT/stats"
bash db/tune-redo.sh # 10 MB redo logs stall Oracle under load ("log file switch (checkpoint incomplete)")

# Same limits for every application container, both variants.
export APP_CPUS="${APP_CPUS:-2}" APP_MEM="${APP_MEM:-1g}" STOCK_STRATEGY="${STOCK_STRATEGY:-atomic}"
export RL_IP_LIMIT=100000000 RL_USER_LIMIT=1000
export OTEL_ENDPOINT= JAVA_TRACING_OPTS=      # empty = tracing off (keep the measurement free of tracing overhead)

sql() {
  printf 'CONNECT %s/%s@//localhost:1521/%s\nSET HEADING OFF FEEDBACK OFF PAGESIZE 0\n%s\nEXIT\n' "$APP_USER" "$APP_USER_PASSWORD" "${ORACLE_SERVICE:-FREEPDB1}" "$1" \
    | docker exec -i flashsale-oracle sqlplus -s /nolog | tr -s '[:space:]' ' ' | sed 's/^ //; s/ $//'
}

switch_to() {
  local impl="$1"
  echo ">>> switching the stack to inventory-$impl (cpus=$APP_CPUS mem=$APP_MEM strategy=$STOCK_STRATEGY)"
  make -s down-apps > /dev/null 2>&1 || true
  INVENTORY_IMPL=$impl make -s up-apps > /dev/null
  for _ in $(seq 1 60); do curl -sf localhost:8088/readyz > /dev/null && break; sleep 1; done
  curl -sf localhost:8088/readyz > /dev/null || { echo "gateway not ready" >&2; exit 1; }
}

run_k6() { # name vus ramp hold sku
  docker run --rm --network flashsale_default -v "$PWD/loadtest:/scripts:ro" -v "$PWD/$OUT/runs:/out" \
    -e BASE_URL=http://gateway:8080 -e SKU="$4" -e DEMO_PASSWORD="$DEMO_PASSWORD" -e USERS="$VUS" -e VUS="$2" \
    -e RAMP="$3" -e HOLD="$HOLD" -e RUN_ID="$1" \
    grafana/k6 run --quiet --summary-export="/out/$1.k6.json" /scripts/flashsale.js > "$OUT/runs/$1.k6.log" 2>&1
}

new_sale() { # sku stock
  sql "INSERT INTO product (sku, name, price) VALUES ('$1', 'benchmark', 100);
INSERT INTO stock (product_id, available) SELECT id, $2 FROM product WHERE sku = '$1';
COMMIT;" > /dev/null
}

sample_stats() { # file containers...
  local file="$1"; shift
  ( while :; do docker stats --no-stream --format '{{.Name}},{{.CPUPerc}},{{.MemUsage}}' "$@" 2>/dev/null | sed "s/^/$(date +%s),/" >> "$file"; sleep 1; done ) &
  STATS_PID=$!
}

for round in $(seq 1 "$ROUNDS"); do
  for impl in $IMPLS; do
    switch_to "$impl"
    name="${impl}-r${round}"
    stamp=$(date +%s)

    echo "--- warm-up ($impl, round $round)"
    warm_sku="FSW-$impl-$stamp"
    new_sale "$warm_sku" "$STOCK"
    run_k6 "warm-$name" 300 10s "$warm_sku" || true
    KEEP_DATA=0 bash loadtest/reconcile.sh "$warm_sku" "$STOCK" > /dev/null 2>&1 || true

    echo "--- measured run $name ($VUS VUs, ramp $RAMP, hold $HOLD)"
    sku="FS-$impl-$stamp"
    new_sale "$sku" "$STOCK"
    stats="$OUT/stats/$name.csv"; : > "$stats"
    sample_stats "$stats" "flashsale-inventory-$impl-1" flashsale-order-1 flashsale-gateway-1 flashsale-oracle
    k6_exit=0
    run_k6 "$name" "$VUS" "$RAMP" "$sku" || k6_exit=$?
    kill "$STATS_PID" 2> /dev/null || true; wait "$STATS_PID" 2> /dev/null || true
    echo "k6 exit code: $k6_exit (non-zero means a threshold was crossed)"

    bash loadtest/reconcile.sh "$sku" "$STOCK" "$OUT/runs/$name.reconcile.json" | tee "$OUT/runs/$name.reconcile.txt" || echo "RECONCILE FAILED for $name"
    printf '{"impl":"%s","round":%s,"vus":%s,"ramp":"%s","hold":"%s","stock":%s,"k6Exit":%s,"cpus":"%s","mem":"%s","strategy":"%s"}\n' \
      "$impl" "$round" "$VUS" "$RAMP" "$HOLD" "$STOCK" "$k6_exit" "$APP_CPUS" "$APP_MEM" "$STOCK_STRATEGY" > "$OUT/runs/$name.meta.json"
  done
done

# ---- one-off facts per implementation: image size, cold start, idle memory ----
echo "--- startup time and idle memory"
: > "$OUT/static.jsonl"
for impl in $IMPLS; do
  switch_to "$impl"
  size=$(docker image inspect "flashsale/inventory-$impl" --format '{{.Size}}')
  times=()
  for _ in 1 2 3 4 5; do
    start=$(date +%s%N)
    docker restart "flashsale-inventory-$impl-1" > /dev/null
    until curl -sf "localhost:$([ "$impl" = go ] && echo 8083 || echo 8084)/readyz" > /dev/null; do sleep 0.1; done
    times+=("$(( ($(date +%s%N) - start) / 1000000 ))")
  done
  sleep 20
  mem=$(docker stats --no-stream --format '{{.MemUsage}}' "flashsale-inventory-$impl-1" | awk '{print $1}')
  echo "{\"impl\":\"$impl\",\"imageBytes\":$size,\"startupMs\":[$(IFS=,; echo "${times[*]}")],\"idleMem\":\"$mem\"}" >> "$OUT/static.jsonl"
done
echo "done: results in $OUT (aggregate with: node loadtest/aggregate.mjs)"
