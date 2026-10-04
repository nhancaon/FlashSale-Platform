#!/usr/bin/env bash
# Replaces the tiny redo logs of the gvenzl Oracle Free image (2 x 10 MB) by 3 x 512 MB groups.
# With 10 MB logs a load test switches logs every few seconds and every session ends up waiting on
# "log file switch (checkpoint incomplete)": the database stalls and the benchmark measures that, not the services.
# Idempotent: does nothing once every group is at least 512 MB. Only touches the flashsale-oracle container.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/.."
. scripts/lib.sh
load_env

SIZE_MB=512
DIR=/opt/oracle/oradata/FREE
sys() { # runs SQL as SYSDBA in the container database root
  printf 'CONNECT sys/%s@//localhost:1521/FREE AS SYSDBA\nSET HEADING OFF FEEDBACK OFF PAGESIZE 0\nWHENEVER SQLERROR EXIT 1\n%s\nEXIT\n' "$ORACLE_PASSWORD" "$1" \
    | docker exec -i flashsale-oracle sqlplus -s /nolog | tr -s '[:space:]' ' ' | sed 's/^ //; s/ $//'
}

small=$(sys "SELECT COUNT(*) FROM v\$log WHERE bytes < $SIZE_MB * 1024 * 1024;")
if [ "$small" = 0 ]; then
  echo "redo logs already >= ${SIZE_MB} MB"
  exit 0
fi

for g in 3 4 5; do
  exists=$(sys "SELECT COUNT(*) FROM v\$log WHERE group# = $g;")
  [ "$exists" = 0 ] && sys "ALTER DATABASE ADD LOGFILE GROUP $g ('$DIR/redo0$g.log') SIZE ${SIZE_MB}M REUSE;" > /dev/null
done

# Drop the small groups once they are neither CURRENT nor ACTIVE (needed for crash recovery).
for g in $(sys "SELECT group# FROM v\$log WHERE bytes < $SIZE_MB * 1024 * 1024 ORDER BY group#;"); do
  for _ in $(seq 1 10); do
    st=$(sys "SELECT status FROM v\$log WHERE group# = $g;")
    [ "$st" = INACTIVE ] || [ "$st" = UNUSED ] && break
    sys "ALTER SYSTEM SWITCH LOGFILE;" > /dev/null
    sys "ALTER SYSTEM CHECKPOINT;" > /dev/null
  done
  file=$(sys "SELECT member FROM v\$logfile WHERE group# = $g;")
  sys "ALTER DATABASE DROP LOGFILE GROUP $g;" > /dev/null
  docker exec flashsale-oracle rm -f "$file"
done

sys "SELECT 'group ' || group# || ': ' || bytes / 1024 / 1024 || ' MB ' || status FROM v\$log ORDER BY group#;"
