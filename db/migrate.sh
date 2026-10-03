#!/usr/bin/env bash
# Áp dụng db/migrations/V*.sql lên Oracle trong container, theo thứ tự tên file.
# Bảng schema_migrations ghi lại file đã chạy nên chạy lại nhiều lần vẫn an toàn.
set -euo pipefail

# Git Bash trên Windows sẽ đổi "/nolog" thành đường dẫn Windows nếu không tắt.
export MSYS_NO_PATHCONV=1

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

ENV_FILE=.env
[ -f "$ENV_FILE" ] || ENV_FILE=.env.example
set -a
# shellcheck disable=SC1090
. "./$ENV_FILE"
set +a

CONTAINER="${ORACLE_CONTAINER:-flashsale-oracle}"
SERVICE_NAME="${ORACLE_SERVICE:-FREEPDB1}"

# Mật khẩu đi qua stdin (CONNECT) để không lộ trong danh sách tiến trình và tránh lỗi convert path của Git Bash.
sqlplus_run() {
  { printf 'CONNECT %s/%s@//localhost:1521/%s\n' "$APP_USER" "$APP_USER_PASSWORD" "$SERVICE_NAME"; cat; } \
    | docker exec -i "$CONTAINER" sqlplus -s /nolog
}

scalar() {
  local out
  out="$(printf 'SET HEADING OFF FEEDBACK OFF PAGESIZE 0 VERIFY OFF\n%s\nEXIT\n' "$1" | sqlplus_run | tr -d '[:space:]')"
  case "$out" in
    ''|*[!0-9]*) echo "lỗi: truy vấn không trả về số: $out" >&2; return 1 ;;
  esac
  echo "$out"
}

has_history="$(scalar "SELECT COUNT(*) FROM user_tables WHERE table_name = 'SCHEMA_MIGRATIONS';")"
if [ "$has_history" = "0" ]; then
  printf 'WHENEVER SQLERROR EXIT FAILURE\nCREATE TABLE schema_migrations (version VARCHAR2(128) PRIMARY KEY, applied_at TIMESTAMP DEFAULT SYSTIMESTAMP NOT NULL);\nEXIT\n' | sqlplus_run
fi

applied=0
for f in db/migrations/V*.sql; do
  name="$(basename "$f")"
  done_count="$(scalar "SELECT COUNT(*) FROM schema_migrations WHERE version = '$name';")"
  if [ "$done_count" != "0" ]; then
    echo "skip   $name"
    continue
  fi
  echo "apply  $name"
  {
    printf 'WHENEVER SQLERROR EXIT FAILURE ROLLBACK\n'
    cat "$f"
    printf "\nINSERT INTO schema_migrations (version) VALUES ('%s');\nCOMMIT;\nEXIT\n" "$name"
  } | sqlplus_run
  applied=$((applied + 1))
done

echo "done: $applied migration(s) applied"
