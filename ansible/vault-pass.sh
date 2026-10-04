#!/bin/sh
# Vault password from the environment (lab: .env, CI: a repository secret). Never stored in the repository.
if [ -z "${FLASHSALE_VAULT_PASSWORD:-}" ]; then
  echo "FLASHSALE_VAULT_PASSWORD is not set" >&2
  exit 1
fi
printf '%s\n' "$FLASHSALE_VAULT_PASSWORD"
