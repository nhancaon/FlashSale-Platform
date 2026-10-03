#!/usr/bin/env bash
# Append to .env every key that .env.example has and .env lacks (never changes existing values).
# Lets new phases add settings (JWT_SECRET, ...) without breaking an existing .env.
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f .env ] || { cp .env.example .env; exit 0; }
added=0
while IFS= read -r line; do
  case "$line" in ''|\#*) continue ;; esac
  key="${line%%=*}"
  if ! grep -q "^${key}=" .env; then
    printf '%s\n' "$line" >> .env
    echo "added $key to .env"
    added=1
  fi
done < .env.example
exit 0
