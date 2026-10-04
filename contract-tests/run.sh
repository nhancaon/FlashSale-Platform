#!/usr/bin/env bash
# Run the inventory contract tests against each service and strategy (starts and stops the services).
#   bash contract-tests/run.sh                       # go and java, all three strategies
#   SERVICES=java STRATEGIES=atomic bash contract-tests/run.sh
# Needs `make up` + `make db-migrate`. Builds the Go binary and the Java jar if missing.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
. scripts/lib.sh
load_env

SERVICES="${SERVICES:-go java}"
STRATEGIES="${STRATEGIES:-atomic pessimistic optimistic}"
BUILD="$ROOT/.build"
mkdir -p "$BUILD"

if [[ " $SERVICES " == *" go "* ]]; then
  (cd services/inventory-go && go build -o "$BUILD/inventory-go.exe" ./cmd/inventory)
fi
if [[ " $SERVICES " == *" java "* ]]; then
  (cd services/inventory-java && ./mvnw -B -q -DskipTests package)
fi

status=0
for svc in $SERVICES; do
  port=8083; [ "$svc" = java ] && port=8084
  for strategy in $STRATEGIES; do
    stop_port "$port"; sleep 1
    echo "=== inventory-$svc / $strategy"
    if [ "$svc" = go ]; then
      PORT=$port STOCK_STRATEGY=$strategy REDIS_ADDR="localhost:$REDIS_HOST_PORT" "$BUILD/inventory-go.exe" > "$BUILD/inventory-go-$strategy.log" 2>&1 &
    else
      PORT=$port STOCK_STRATEGY=$strategy DB_POOL_TIMEOUT_MS=30000 REDIS_PORT=$REDIS_HOST_PORT \
        java -jar services/inventory-java/target/inventory-java-0.0.1-SNAPSHOT.jar > "$BUILD/inventory-java-$strategy.log" 2>&1 &
    fi
    wait_ready "$port"
    (cd contract-tests && BASE_URL="http://localhost:$port" STRATEGY="$strategy" go test -count=1 ./... ) || status=1
    stop_port "$port"
  done
done
exit $status
