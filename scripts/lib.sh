# Helpers shared by the scripts that start services as host processes (Windows + Git Bash).

# stop_port <port>: stop whatever listens on the port (the services we started).
stop_port() {
  powershell -NoProfile -Command "Get-NetTCPConnection -LocalPort $1 -State Listen -ErrorAction SilentlyContinue | ForEach-Object { Stop-Process -Id \$_.OwningProcess -Force }" || true
}

# wait_ready <port>: wait up to 60 s for /readyz to answer 200.
wait_ready() {
  for _ in $(seq 1 60); do
    [ "$(curl -s -o /dev/null -w '%{http_code}' "localhost:$1/readyz")" = 200 ] && return 0
    sleep 1
  done
  echo "service on :$1 did not become ready" >&2
  return 1
}

# load_env: export DB_PASSWORD and REDIS_HOST_PORT from .env (or .env.example).
load_env() {
  local f=.env
  [ -f "$f" ] || f=.env.example
  set -a; . "./$f"; set +a
  export DB_PASSWORD="$APP_USER_PASSWORD"
  export REDIS_HOST_PORT="${REDIS_HOST_PORT:-6380}"
}
