#!/usr/bin/env bash
# Phase 2: reserve latency, 3 stock strategies x 2 languages, two scenarios. Services run as host processes
# (Oracle/Redis from `make up`); the client is a native Go program, so there is no Docker networking in the path.
# This is a comparison on one laptop, not the controlled benchmark of Phase 6 (limited containers).
#   GO_BIN=<built inventory-go binary> bash loadtest/run-inventory-bench.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
. scripts/lib.sh
load_env

GO_BIN="${GO_BIN:?path to the built inventory-go binary}"
JAVA_JAR="${JAVA_JAR:-services/inventory-java/target/inventory-java-0.0.1-SNAPSHOT.jar}"
RUNS="${RUNS:-3}"
OUT="loadtest/results/phase2-inventory"
mkdir -p "$OUT"
: > "$OUT/results.jsonl"

BENCH="$(mktemp -d)/strategybench.exe"
(cd contract-tests && go build -o "$BENCH" ./cmd/strategybench)

for svc in go java; do
  port=8083; [ "$svc" = java ] && port=8084
  for strategy in atomic pessimistic optimistic; do
    stop_port "$port"; sleep 1
    if [ "$svc" = go ]; then
      PORT=$port STOCK_STRATEGY=$strategy DB_POOL_MAX=20 REDIS_ADDR="localhost:$REDIS_HOST_PORT" "$GO_BIN" > /dev/null 2>&1 &
    else
      PORT=$port STOCK_STRATEGY=$strategy DB_POOL_MAX=20 DB_POOL_TIMEOUT_MS=30000 REDIS_PORT=$REDIS_HOST_PORT java -jar "$JAVA_JAR" > /dev/null 2>&1 &
    fi
    wait_ready "$port"
    echo "== $svc / $strategy"
    export BASE_URL="http://localhost:$port" STRATEGY=$strategy
    "$BENCH" -service $svc -scenario sellout -runs 1 -requests 1000 > /dev/null   # warm-up (JIT, pool)
    for scenario in sellout plenty; do
      "$BENCH" -service $svc -scenario $scenario -runs "$RUNS" >> "$OUT/results.jsonl"
    done
    stop_port "$port"
  done
done
echo "results in $OUT/results.jsonl"
