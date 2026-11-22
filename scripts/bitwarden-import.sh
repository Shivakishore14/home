#!/usr/bin/env bash
# Copy homelab secrets that already exist on this machine into the homelab
# Bitwarden account (see scripts/lib/bitwarden.sh)
# (folder "Homelab"), so other machines and devices can get them from there.
#
#   scripts/bitwarden-import.sh      (make bitwarden-import)
#
# Sources (whatever exists here):
#   ~/.config/taskengine/worker.env -> item "TaskEngine"
#       TASKENGINE_URL -> URI, TASKENGINE_PRODUCER_TOKEN (API key) -> password,
#       TASKENGINE_TOKEN -> hidden field worker_token
#   <repo>/.env.local (R2_*)        -> item "Cloudflare R2" (hidden fields)
#
# Existing items are updated field by field; values that differ from the vault
# are listed (masked) and need confirmation. Secrets are passed through
# environment variables and stdin, never command-line arguments.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=lib/bitwarden.sh
. "$REPO/scripts/lib/bitwarden.sh"

die() { echo "ERROR: $*" >&2; exit 1; }

# read_env FILE KEY: literal KEY=VALUE lookup (files are never sourced).
read_env() {
  [ -f "$1" ] || return 0
  python3 - "$1" "$2" <<'EOF'
import sys
path, key = sys.argv[1], sys.argv[2]
for line in open(path):
    k, sep, v = line.rstrip("\r\n").partition("=")
    if sep and k.strip() == key:
        v = v.strip()
        if len(v) >= 2 and v[0] == v[-1] and v[0] in "\"'":
            v = v[1:-1]
        sys.stdout.write(v)
        break
EOF
}

bw_unlock || die "Bitwarden must be installed, logged in (bw login) and unlockable"

folder_id() {
  local id
  id="$(bw list folders --session "$BW_SESSION" | python3 -c '
import json, sys
print(next((f["id"] for f in json.load(sys.stdin) if f["name"] == sys.argv[1]), ""))' "$BW_FOLDER")"
  if [ -z "$id" ]; then
    id="$(printf '{"name":"%s"}' "$BW_FOLDER" | bw encode | bw create folder --session "$BW_SESSION" |
      python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
    echo "==> Created Bitwarden folder $BW_FOLDER" >&2
  fi
  echo "$id"
}

# upsert NAME TYPE: values come from env vars V_URI, V_PASSWORD and V_FIELD_<name>.
# TYPE is "login" or "note".
upsert() {
  local name="$1" type="$2" existing merged
  existing="$(bw_item_json "$name")"
  merged="$(EXISTING="$existing" FOLDER_ID="$FOLDER_ID" python3 - "$name" "$type" <<'EOF'
import json, os, sys
name, kind = sys.argv[1], sys.argv[2]
item = json.loads(os.environ["EXISTING"]) if os.environ.get("EXISTING") else {
    "organizationId": None, "folderId": os.environ["FOLDER_ID"], "type": 1 if kind == "login" else 2,
    "name": name, "notes": None, "favorite": False, "fields": [], "reprompt": 0,
    "login": {"uris": [], "username": None, "password": None, "totp": None} if kind == "login" else None,
    "secureNote": {"type": 0} if kind == "note" else None,
}
changes = []
def mask(v):
    return "(empty)" if not v else (v[:4] + "..." + v[-4:] if len(v) >= 16 else "****")
def setv(label, old, new, secret=True):
    if new and new != old:
        changes.append("%s: %s -> %s" % (label, mask(old) if secret else old or "(empty)", mask(new) if secret else new))
        return new
    return old
login = item.get("login")
if login is not None:
    uris = login.get("uris") or []
    old_uri = uris[0]["uri"] if uris else None
    new_uri = setv("uri", old_uri, os.environ.get("V_URI"), secret=False)
    if new_uri != old_uri:
        login["uris"] = [{"match": None, "uri": new_uri}] + uris[1:]
    login["password"] = setv("password", login.get("password"), os.environ.get("V_PASSWORD"))
fields = item.setdefault("fields", []) or []
item["fields"] = fields
for key, value in sorted(os.environ.items()):
    if not key.startswith("V_FIELD_"):
        continue
    fname = key[len("V_FIELD_"):]
    f = next((f for f in fields if f.get("name") == fname), None)
    if f is None:
        f = {"name": fname, "value": None, "type": 1}
        fields.append(f)
    f["value"] = setv("field " + fname, f.get("value"), value)
print(json.dumps({"item": item, "changes": changes, "new": not os.environ.get("EXISTING")}))
EOF
)"
  local new changes
  new="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["new"])' <<<"$merged")"
  changes="$(python3 -c 'import json,sys; print("\n".join(json.load(sys.stdin)["changes"]))' <<<"$merged")"
  if [ -z "$changes" ]; then
    echo "==> $name: already up to date"
    return
  fi
  echo "==> $name: $( [ "$new" = True ] && echo "create" || echo "update")"
  sed 's/^/      /' <<<"$changes"
  if [ "$new" != True ]; then
    [ -t 0 ] || die "refusing to overwrite '$name' without a terminal to confirm"
    local ok; read -r -p "    Apply these changes to '$name'? [y/N] " ok
    [[ "$ok" =~ ^[Yy]$ ]] || { echo "    skipped"; return; }
  fi
  local id
  # Encoded item goes to bw on stdin, never as an argument (visible in ps).
  if [ "$new" = True ]; then
    python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin)["item"]))' <<<"$merged" |
      bw encode | bw create item --session "$BW_SESSION" >/dev/null
  else
    id="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["item"]["id"])' <<<"$merged")"
    python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin)["item"]))' <<<"$merged" |
      bw encode | bw edit item "$id" --session "$BW_SESSION" >/dev/null
  fi
  echo "    saved"
}

FOLDER_ID="$(folder_id)"
export FOLDER_ID

WENV="$HOME/.config/taskengine/worker.env"
if [ -f "$WENV" ]; then
  V_URI="$(read_env "$WENV" TASKENGINE_URL)"; V_URI="${V_URI%/}"
  V_PASSWORD="$(read_env "$WENV" TASKENGINE_PRODUCER_TOKEN)"
  V_FIELD_worker_token="$(read_env "$WENV" TASKENGINE_TOKEN)"
  export V_URI V_PASSWORD V_FIELD_worker_token
  upsert TaskEngine login
  unset V_URI V_PASSWORD V_FIELD_worker_token
else
  echo "==> TaskEngine: no $WENV on this machine, skipping"
fi

RENV="$REPO/.env.local"
if [ -f "$RENV" ] && [ -n "$(read_env "$RENV" R2_SECRET_ACCESS_KEY)" ]; then
  V_FIELD_account_id="$(read_env "$RENV" R2_ACCOUNT_ID)"
  V_FIELD_access_key_id="$(read_env "$RENV" R2_ACCESS_KEY_ID)"
  V_FIELD_secret_access_key="$(read_env "$RENV" R2_SECRET_ACCESS_KEY)"
  V_FIELD_bucket="$(read_env "$RENV" R2_BUCKET)"
  export V_FIELD_account_id V_FIELD_access_key_id V_FIELD_secret_access_key V_FIELD_bucket
  upsert "Cloudflare R2" note
else
  echo "==> Cloudflare R2: no R2 keys in $RENV, skipping"
fi
