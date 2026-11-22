# shellcheck shell=bash
# Bitwarden helpers (sourced). Requires the Bitwarden CLI (`bw`) and python3.
#
# Vault layout used by the homelab scripts (folder "Homelab"):
#   TaskEngine      login: URI = server URL, password = API key,
#                   hidden field "worker_token"
#   Cloudflare R2   hidden fields: account_id, access_key_id,
#                   secret_access_key, bucket
#
# Unlocking asks for the master password with a hidden prompt; the session
# key lives only in this process (BW_SESSION) and is never written to disk.
#
# The scripts use a dedicated *homelab* Bitwarden account (separate email and
# master password from your personal vault). Its CLI state lives in its own
# profile directory, so these scripts can never log in to or unlock your
# personal vault; use scripts/hbw (make bitwarden-login) to manage it.

BW_FOLDER="${BW_FOLDER:-Homelab}"
export BITWARDENCLI_APPDATA_DIR="${HOMELAB_BW_DIR:-$HOME/.config/bitwarden-homelab}"
mkdir -p "$BITWARDENCLI_APPDATA_DIR" && chmod 700 "$BITWARDENCLI_APPDATA_DIR"
# Homebrew's bin is missing from non-login shells (e.g. ssh) on macOS.
case ":$PATH:" in *:/opt/homebrew/bin:*) ;; *) [ -d /opt/homebrew/bin ] && PATH="$PATH:/opt/homebrew/bin" ;; esac

bw_installed() { command -v bw >/dev/null 2>&1; }

_bw_status() {
  bw status ${BW_SESSION:+--session "$BW_SESSION"} 2>/dev/null |
    python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",""))' 2>/dev/null
}

# bw_unlock: make sure the vault is unlocked for this process. Returns 1 (with
# a hint on stderr) when bw is missing, not logged in, or can't prompt.
bw_unlock() {
  bw_installed || { echo "Bitwarden CLI (bw) not installed" >&2; return 1; }
  case "$(_bw_status)" in
    unlocked) ;;
    locked)
      [ -t 0 ] || { echo "Bitwarden is locked and there is no terminal to unlock it" >&2; return 1; }
      local pw session
      read -r -s -p "Bitwarden master password: " pw; echo >&2
      session="$(BW_PASSWORD="$pw" bw unlock --passwordenv BW_PASSWORD --raw 2>/dev/null)" || session=""
      unset pw
      [ -n "$session" ] || { echo "Bitwarden unlock failed" >&2; return 1; }
      export BW_SESSION="$session"
      ;;
    unauthenticated) echo "Bitwarden: homelab account not logged in on this machine (run: make bitwarden-login)" >&2; return 1 ;;
    *) echo "Bitwarden: cannot read vault status" >&2; return 1 ;;
  esac
  if [ -z "${_BW_SYNCED:-}" ]; then
    bw sync --session "$BW_SESSION" >/dev/null 2>&1 || true
    export _BW_SYNCED=1
  fi
}

# bw_folder_id: id of $BW_FOLDER (empty if it doesn't exist).
bw_folder_id() {
  bw list folders --session "$BW_SESSION" 2>/dev/null | python3 -c '
import json, sys
print(next((f["id"] for f in json.load(sys.stdin) if f.get("name") == sys.argv[1]), ""))' "$BW_FOLDER"
}

# bw_item_json NAME: the item with exactly this name in $BW_FOLDER (empty if none).
bw_item_json() {
  local folder; folder="$(bw_folder_id)"
  [ -n "$folder" ] || return 0
  bw list items --folderid "$folder" --search "$1" --session "$BW_SESSION" 2>/dev/null | python3 -c '
import json, sys
name = sys.argv[1]
items = [i for i in json.load(sys.stdin) if i.get("name") == name]
if len(items) > 1:
    sys.exit("more than one Bitwarden item named %r" % name)
if items:
    print(json.dumps(items[0]))
' "$1"
}

# bw_field ITEM FIELD: print a value. FIELD is password, username, uri, notes
# or the name of a custom field. Empty output if the item/field is missing.
bw_field() {
  bw_unlock || return 1
  bw_item_json "$1" | python3 -c '
import json, sys
data = sys.stdin.read().strip()
if not data:
    sys.exit(0)
item, field = json.loads(data), sys.argv[1]
login = item.get("login") or {}
if field == "password":
    v = login.get("password")
elif field == "username":
    v = login.get("username")
elif field == "uri":
    v = ((login.get("uris") or [{}])[0] or {}).get("uri")
elif field == "notes":
    v = item.get("notes")
else:
    v = next((f.get("value") for f in item.get("fields") or [] if f.get("name") == field), None)
if v:
    sys.stdout.write(v)
' "$2"
}
