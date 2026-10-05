#!/usr/bin/env bash
# Runs every experiment of one language in a container with the same limits for both languages.
#   bash labs/concurrency-lab/run.sh go|java        (make lab-go / make lab-java)
# Output: labs/concurrency-lab/results/<lang>.jsonl (one JSON line per repetition, same shape for both languages),
#         results/profiles/ (CPU profiles of experiments 2 and 3), results/env-<lang>.txt (versions, flags, limits).
# Knobs: LAB_CPUS=2 LAB_MEM=3g REPS=5
set -euo pipefail
export MSYS_NO_PATHCONV=1
LANG_="${1:?go|java}"
cd "$(dirname "$0")"
CPUS="${LAB_CPUS:-2}"; MEM="${LAB_MEM:-3g}"; REPS="${REPS:-5}"
mkdir -p results/profiles
OUT="results/$LANG_.jsonl"; : > "$OUT"

# One line per run: experiment variant n. Each run is its own process, so peak memory and crashes stay separate.
MATRIX_GO="
1 goroutine 1000
1 goroutine 10000
1 goroutine 100000
1 goroutine 1000000
2 pool 5000
3 fanout 10000
4 mutex 100000
4 atomic 100000
4 channel 100000
6 block 200000
6 drop 200000
7 context 10000
8 deadlock 0
8 leak 0
9 pipeline 1000000"

MATRIX_JAVA="
1 platform 1000
1 platform 10000
1 platform 100000
1 platform 1000000
1 virtual 1000
1 virtual 10000
1 virtual 100000
1 virtual 1000000
2 fixed-pool 5000
2 forkjoin 5000
3 virtual 10000
3 completablefuture 10000
4 synchronized 100000
4 reentrantlock 100000
4 atomiclong 100000
4 longadder 100000
5 race 100
6 block 200000
6 drop 200000
7 future-cancel 10000
7 structured 10000
8 deadlock 0
8 leak 0
9 stream 1000000
9 parallel-stream 1000000
9 queue-virtual 1000000"

inner() { # runs inside the container
  local prefix="$1" suffix="$2" matrix="$3" profile_flag="$4"
  echo "$matrix" | while read -r exp variant n; do
    [ -z "$exp" ] && continue
    extra=""
    if [ "$exp" = 2 ] || [ "$exp" = 3 ]; then extra="$(printf -- "$profile_flag" "$exp-$variant")"; fi
    start=$(date +%s%N)
    set +e
    # shellcheck disable=SC2086
    $prefix $extra $suffix -exp "$exp" -variant "$variant" -n "$n" -reps "$REPS" -warmup 1 >> "/out/$LANG_.jsonl" 2> /tmp/err.txt
    rc=$?
    set -e
    secs=$(( ($(date +%s%N) - start) / 1000000000 ))
    if [ "$rc" != 0 ]; then # crash (OOM kill = 137, JVM error...): recorded as a result, not hidden
      reason=$(tail -c 300 /tmp/err.txt | tr '\n"\\' '   ')
      printf '{"exp":%s,"lang":"%s","variant":"%s","param":"%s","rep":0,"metrics":{"crashed":1,"exitCode":%s},"notes":"%s"}\n' \
        "$exp" "$LANG_" "$variant" "$n" "$rc" "$reason" >> "/out/$LANG_.jsonl"
    fi
    echo "exp $exp $variant n=$n: exit $rc, ${secs}s"
  done
}

COMMON=(--rm --cpus "$CPUS" --memory "$MEM" --memory-swap "$MEM" -v "$PWD/results:/out" -e REPS="$REPS" -e LANG_="$LANG_")
case "$LANG_" in
  go)
    docker run "${COMMON[@]}" -v "$PWD/go:/src" -v flashsale-gomod:/go/pkg/mod -v flashsale-gocache:/root/.cache -w /src \
      -e MATRIX="$MATRIX_GO" golang:1.26 bash -c "$(declare -f inner)
      set -euo pipefail
      go build -o /usr/local/bin/lab .
      { go version; echo 'GOMAXPROCS: runtime default, cgroup-aware since Go 1.25 (effective value: metrics.workers of experiment 2)'; echo GOGC=\${GOGC:-100}; echo cpus=$CPUS mem=$MEM; } > /out/env-go.txt
      inner lab '' \"\$MATRIX\" '-cpuprofile /out/profiles/go-%s.pprof'
      # experiment 5: the race detector on a deliberate race, then on the fix
      t=\$(date +%s%N); LAB_RACE=1 go test -race -count=1 -run TestRacy ./race > /tmp/race.txt 2>&1 || true
      ms=\$(( (\$(date +%s%N) - t) / 1000000 )); det=\$(grep -c 'WARNING: DATA RACE' /tmp/race.txt || true)
      go test -race -count=1 -run TestSafe ./race > /dev/null 2>&1 && fixed=1 || fixed=0
      printf '{\"exp\":5,\"lang\":\"go\",\"variant\":\"go-test-race\",\"param\":\"\",\"rep\":1,\"metrics\":{\"detected\":%s,\"raceReports\":%s,\"wallMs\":%s,\"fixPasses\":%s}}\n' \$(( det > 0 )) \$det \$ms \$fixed >> /out/go.jsonl
      cp /tmp/race.txt /out/profiles/go-race-report.txt
      # flame graph input: one line per sampled stack
      for f in /out/profiles/go-*.pprof; do go tool pprof -traces /usr/local/bin/lab \$f > \${f%.pprof}.traces.txt 2>/dev/null || true; done"
    ;;
  java)
    docker run "${COMMON[@]}" -v "$PWD/java:/src" -v flashsale-m2:/root/.m2 -w /src \
      -e MATRIX="$MATRIX_JAVA" eclipse-temurin:25-jdk bash -c "$(declare -f inner)
      set -euo pipefail
      sh ./mvnw -B -q package -DskipTests
      { java -version 2>&1; echo flags: --enable-preview, default GC \$(java -XX:+PrintCommandLineFlags -version 2>&1 | grep -o 'Use[A-Za-z0-9]*GC'); echo cpus=$CPUS mem=$MEM; } > /out/env-java.txt
      inner 'java --enable-preview' '-jar target/lab.jar' \"\$MATRIX\" '-Xlog:jfr+startup=warning -XX:StartFlightRecording:filename=/out/profiles/java-%s.jfr,settings=profile'
      for f in /out/profiles/java-*.jfr; do jfr print --stack-depth 64 --events jdk.ExecutionSample \$f > \${f%.jfr}.samples.txt 2>/dev/null || true; done"
    ;;
  *) echo "usage: $0 go|java" >&2; exit 2 ;;
esac
echo "results: labs/concurrency-lab/$OUT"
