#!/usr/bin/env bash
# Preliminary rate limiter benchmark (Phase 1): Go vs Java x 3 algorithms.
# Both services run as host processes against the Redis from `make up`, so this is a
# sanity check, NOT the controlled benchmark (that one uses limited containers, Phase 6).
#   Prereq: make up; go build + ./mvnw package done (see GO_BIN / JAVA_JAR).
set -euo pipefail
export MSYS_NO_PATHCONV=1

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
GO_BIN="${GO_BIN:?path to the built ratelimiter-go binary}"
JAVA_JAR="${JAVA_JAR:-services/ratelimiter-java/target/ratelimiter-java-0.0.1-SNAPSHOT.jar}"
OUT="loadtest/results/phase1-ratelimiter"
VUS="${VUS:-100}"
DURATION="${DURATION:-15s}"
REDIS_PORT="${REDIS_HOST_PORT:-6380}"
mkdir -p "$OUT"

stop_port() {
  powershell -NoProfile -Command "Get-NetTCPConnection -LocalPort $1 -State Listen -ErrorAction SilentlyContinue | ForEach-Object { Stop-Process -Id \$_.OwningProcess -Force }" || true
}

wait_ready() {
  for _ in $(seq 1 60); do
    [ "$(curl -s -o /dev/null -w '%{http_code}' "localhost:$1/readyz")" = 200 ] && return 0
    sleep 1
  done
  echo "service on :$1 did not become ready" >&2
  return 1
}

k6run() { # name port summary-file [duration]
  docker run --rm -v "$ROOT/loadtest:/scripts" -v "$ROOT/$OUT:/out" \
    -e BASE_URL="http://host.docker.internal:$2" -e VUS="$VUS" -e DURATION="${4:-$DURATION}" \
    grafana/k6 run --quiet --summary-export="/out/$3" /scripts/ratelimiter-check.js > /dev/null 2>&1 || true
}

for algo in token_bucket sliding_window fixed_window; do
  for svc in go java; do
    port=8081; [ "$svc" = java ] && port=8082
    stop_port "$port"
    if [ "$svc" = go ]; then
      PORT=$port RATELIMIT_ALGORITHM=$algo REDIS_ADDR="localhost:$REDIS_PORT" "$GO_BIN" > /dev/null 2>&1 &
    else
      PORT=$port RATELIMIT_ALGORITHM=$algo REDIS_PORT=$REDIS_PORT java -jar "$JAVA_JAR" > /dev/null 2>&1 &
    fi
    wait_ready "$port"
    echo "== $svc / $algo: warm-up"
    k6run "$svc" "$port" "warmup.json" 8s
    echo "== $svc / $algo: measure ($VUS VUs, $DURATION)"
    k6run "$svc" "$port" "$svc-$algo.json"
    stop_port "$port"
  done
done
rm -f "$OUT/warmup.json"
echo "results in $OUT"
