#!/usr/bin/env bash
# Run a command with R2 credentials from Bitwarden (item "Cloudflare R2"),
# falling back to <repo>/.env.local. Nothing is written to disk.
#
#   scripts/r2-env.sh -- go test ./pkg/storage -run R2Integration
set -euo pipefail
REPO="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=lib/bitwarden.sh
. "$REPO/scripts/lib/bitwarden.sh"
[ "${1:-}" = "--" ] && shift
[ $# -gt 0 ] || { echo "usage: $0 -- command [args...]" >&2; exit 2; }

item="${BW_R2_ITEM:-Cloudflare R2}"
if bw_installed && bw_unlock 2>/dev/null; then
  R2_ACCOUNT_ID="$(bw_field "$item" account_id)"
  R2_ACCESS_KEY_ID="$(bw_field "$item" access_key_id)"
  R2_SECRET_ACCESS_KEY="$(bw_field "$item" secret_access_key)"
  R2_BUCKET="$(bw_field "$item" bucket)"
fi
if [ -z "${R2_SECRET_ACCESS_KEY:-}" ] && [ -f "$REPO/.env.local" ]; then
  echo "(R2 keys not in Bitwarden; using .env.local)" >&2
  while IFS='=' read -r k v; do
    case "$k" in R2_ACCOUNT_ID|R2_ACCESS_KEY_ID|R2_SECRET_ACCESS_KEY|R2_BUCKET) printf -v "$k" '%s' "$v" ;; esac
  done < "$REPO/.env.local"
fi
[ -n "${R2_SECRET_ACCESS_KEY:-}" ] || { echo "R2 credentials not found in Bitwarden ($item) or .env.local" >&2; exit 1; }
export R2_ACCOUNT_ID R2_ACCESS_KEY_ID R2_SECRET_ACCESS_KEY R2_BUCKET="${R2_BUCKET:-taskengine}"
exec "$@"
