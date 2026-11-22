#!/usr/bin/env bash
# Install TaskEngine services on this machine as boot-time services.
#
#   scripts/install-worker.sh install <worker-id>     make worker-install WORKER_ID=...
#   scripts/install-worker.sh install-producer        make producer-install
#   scripts/install-worker.sh install-comfyui <dir>   make comfyui-install COMFYUI_DIR=...   (Linux)
#   scripts/install-worker.sh uninstall [worker|producer|comfyui]
#
#   macOS: LaunchAgents com.home.taskengine-{worker,producer}, started through
#          ~/.local/bin/taskengine-launcher (keeps Full Disk Access across
#          rebuilds). Start at boot when the user auto-logs in. Installing the
#          worker retires the old local server agent (com.home.taskengine-server).
#   Linux: systemd user units taskengine-{worker,producer}.service and
#          comfyui.service (start at boot with lingering: loginctl enable-linger $USER).
#
# Server URL and tokens live in ~/.config/taskengine/worker.env (mode 600),
# prompted for on first use and never written to the repo:
#   TASKENGINE_URL=https://...
#   TASKENGINE_TOKEN=...            worker token (workers)
#   TASKENGINE_PRODUCER_TOKEN=...   API key / client token (producer creates tasks)
set -euo pipefail

ACTION="${1:-}"
ARG="${2:-}"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$REPO/src/taskengine/bin/taskengine"
ENV_DIR="$HOME/.config/taskengine"
ENV_FILE="$ENV_DIR/worker.env"
OS="$(uname -s)"
MAC_AGENTS="$HOME/Library/LaunchAgents"
MAC_LAUNCHER="$HOME/.local/bin/taskengine-launcher"
MAC_OLD_SERVER="com.home.taskengine-server"
LINUX_UNIT_DIR="$HOME/.config/systemd/user"
PRODUCER_INTERVAL="${PRODUCER_INTERVAL:-60s}"

die() { echo "ERROR: $*" >&2; exit 1; }
usage() {
  sed -n '4,7p' "$0" | sed 's/^# */usage: /' >&2
  exit 2
}

# ---------------------------------------------------------------- env file

# get_env KEY: value of KEY from the env file, parsed literally (never sourced).
get_env() {
  [ -f "$ENV_FILE" ] || return 0
  local key value
  while IFS='=' read -r key value || [ -n "$key" ]; do
    [ "$key" = "$1" ] || continue
    value="${value%$'\r'}"
    if [[ "$value" =~ ^\"(.*)\"$ || "$value" =~ ^\'(.*)\'$ ]]; then value="${BASH_REMATCH[1]}"; fi
    printf '%s' "$value"
    return 0
  done < "$ENV_FILE"
}

# require_env KEY PROMPT [secret]: read KEY, prompting and saving it if missing.
# preview VALUE [secret]: full value, or a masked one for secrets (abcd…wxyz).
preview() {
  local v="$1"
  if [ -z "${2:-}" ]; then printf '%s' "$v"
  elif [ "${#v}" -ge 16 ]; then printf '%s…%s' "${v:0:4}" "${v: -4}"
  else printf '****'; fi
}

# set_env KEY VALUE: replace KEY's line in the env file (mode 600).
set_env() {
  mkdir -p "$ENV_DIR"
  local tmp
  tmp="$(umask 077; mktemp "$ENV_DIR/.worker.env.XXXXXX")"
  if [ -f "$ENV_FILE" ]; then grep -v "^$1=" "$ENV_FILE" > "$tmp" || true; fi
  printf '%s=%s\n' "$1" "$2" >> "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$ENV_FILE"
}

# require_env KEY PROMPT [secret]: print KEY's value. In a terminal, always
# asks, showing the stored value (masked for secrets) and keeping it on Enter;
# without a terminal, uses the stored value. Prompts go to stderr because the
# function runs inside $(...).
require_env() {
  local key="$1" prompt="$2" secret="${3:-}" stored input
  stored="$(get_env "$key")"
  if [ -t 0 ]; then
    if [ -n "$stored" ]; then
      prompt="$prompt [$(preview "$stored" "$secret"), Enter to keep]"
    fi
    if [ -n "$secret" ]; then
      read -r -s -p "$prompt: " input; echo >&2
    else
      read -r -p "$prompt: " input
    fi
    if [ -n "$input" ] && [ "$input" != "$stored" ]; then
      set_env "$key" "$input"
      stored="$input"
    fi
  fi
  [ -n "$stored" ] || die "$key missing in $ENV_FILE (add '$key=...', mode 600) or rerun in a terminal to be prompted."
  chmod 600 "$ENV_FILE"
  printf '%s' "$stored"
}

load_url() { TASKENGINE_URL="$(require_env TASKENGINE_URL "TaskEngine server URL (https://...)")"; TASKENGINE_URL="${TASKENGINE_URL%/}"; }

check_status() { # check_status <expected> <code> <what>
  case "$2" in
    "$1") echo "==> $3 accepted by $TASKENGINE_URL" ;;
    401)  die "$3 rejected by $TASKENGINE_URL (401)" ;;
    *)    die "unexpected response '$2' from $TASKENGINE_URL while checking $3" ;;
  esac
}

verify_worker_token() {
  # A claim for a plugin nobody has returns 204 with a valid worker/client token.
  check_status 204 "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $1" \
    -d '{"worker_id":"__probe__","plugins":["__probe__"]}' "$TASKENGINE_URL/api/v1/tasks/claim" || true)" "worker token"
}

verify_client_token() {
  check_status 200 "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $1" "$TASKENGINE_URL/api/v1/stats" || true)" "client token"
}

# ---------------------------------------------------------------- build

build() {
  echo "==> Building taskengine"
  local gocmd=()
  if go version >/dev/null 2>&1; then gocmd=(go)
  elif command -v mise >/dev/null 2>&1; then gocmd=(mise x go@1.25 -- go)
  elif [ -x /opt/homebrew/bin/go ]; then gocmd=(/opt/homebrew/bin/go)
  else die "Go not found"; fi
  (cd "$REPO/src/taskengine" && GOTOOLCHAIN=auto "${gocmd[@]}" build -o bin/taskengine.new ./cmd/taskengine && mv bin/taskengine.new bin/taskengine)
}

# ---------------------------------------------------------------- macOS

xml_escape() { sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' <<<"$1"; }

# mac_install LABEL ENV_KEY ENV_VALUE -- ARGS...
mac_install() {
  local label="$1" env_key="$2" env_value="$3"; shift 4
  local domain="gui/$(id -u)" plist="$MAC_AGENTS/$label.plist" log="$HOME/Library/Logs/$label.log" args="" a
  local program=("$BIN")
  if [ -x "$MAC_LAUNCHER" ]; then
    program=("$MAC_LAUNCHER" "$BIN")
  else
    echo "WARN: $MAC_LAUNCHER not found; running taskengine directly (grant Full Disk Access to the binary)"
  fi
  for a in "${program[@]}" "$@"; do args+="		<string>$(xml_escape "$a")</string>"$'\n'; done

  echo "==> Installing $plist"
  launchctl bootout "$domain/$label" 2>/dev/null || true
  mkdir -p "$MAC_AGENTS"
  (umask 077; cat > "$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$label</string>
	<key>ProgramArguments</key>
	<array>
$args	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
		<key>$env_key</key>
		<string>$(xml_escape "$env_value")</string>
	</dict>
	<key>WorkingDirectory</key>
	<string>$REPO</string>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>StandardOutPath</key>
	<string>$log</string>
	<key>StandardErrorPath</key>
	<string>$log</string>
</dict>
</plist>
EOF
  )
  plutil -lint "$plist" >/dev/null
  launchctl bootstrap "$domain" "$plist"
  sleep 3
  launchctl print "$domain/$label" | grep -E "^\s*(state|pid) =" || true
  tail -n 5 "$log" 2>/dev/null || true
}

mac_retire_server() {
  if [ -f "$MAC_AGENTS/$MAC_OLD_SERVER.plist" ]; then
    echo "==> Retiring local server ($MAC_OLD_SERVER)"
    launchctl bootout "gui/$(id -u)/$MAC_OLD_SERVER" 2>/dev/null || true
    mkdir -p "$MAC_AGENTS/disabled"
    mv "$MAC_AGENTS/$MAC_OLD_SERVER.plist" "$MAC_AGENTS/disabled/"
  fi
}

mac_uninstall() {
  launchctl bootout "gui/$(id -u)/$1" 2>/dev/null || true
  rm -f "$MAC_AGENTS/$1.plist"
}

# ---------------------------------------------------------------- Linux

# linux_install UNIT DESCRIPTION -- [Service] lines...
linux_install() {
  local unit="$1" desc="$2"; shift 3
  local path="$LINUX_UNIT_DIR/$unit"
  mkdir -p "$LINUX_UNIT_DIR"
  if [ -f "$path" ] && ! grep -q "Managed by scripts/install-worker.sh" "$path"; then
    cp "$path" "$path.bak-$(date +%Y%m%d%H%M%S)"
    echo "==> Backed up existing $unit"
  fi
  echo "==> Installing $path"
  {
    echo "# Managed by scripts/install-worker.sh — rerun it instead of editing."
    echo "[Unit]"
    echo "Description=$desc"
    echo "After=network-online.target"
    echo "Wants=network-online.target"
    echo "StartLimitIntervalSec=0"
    echo
    echo "[Service]"
    printf '%s\n' "$@"
    echo "Restart=always"
    echo "RestartSec=30"
    echo
    echo "[Install]"
    echo "WantedBy=default.target"
  } > "$path"
  systemctl --user daemon-reload
  systemctl --user enable "$unit" >/dev/null
}

linux_start() {
  systemctl --user restart "$1"
  if [ "$(loginctl show-user "$USER" -p Linger --value 2>/dev/null)" != "yes" ]; then
    echo "WARN: lingering is off; services only start after login. Enable boot start with: loginctl enable-linger $USER"
  fi
  sleep 3
  systemctl --user --no-pager status "$1" | sed -n 1,5p || true
  journalctl --user -u "$1" -n 5 --no-pager 2>/dev/null || true
}

linux_uninstall() {
  systemctl --user disable --now "$1" 2>/dev/null || true
  rm -f "$LINUX_UNIT_DIR/$1"
  systemctl --user daemon-reload
}

# ---------------------------------------------------------------- actions

install_worker() {
  local id="$1"
  [[ "$id" =~ ^[A-Za-z0-9_-]+$ ]] || die "invalid worker id '$id'"
  [ -f "$REPO/tasks/workers/$id.yaml" ] || echo "WARN: no tasks/workers/$id.yaml; the worker will run default (non-opt-in) plugins"
  load_url
  local token; token="$(require_env TASKENGINE_TOKEN "Worker token" secret)"
  verify_worker_token "$token"
  build
  case "$OS" in
    Darwin)
      mac_retire_server
      mac_install com.home.taskengine-worker TASKENGINE_TOKEN "$token" -- worker --server-url "$TASKENGINE_URL" --worker-id "$id" ;;
    Linux)
      linux_install taskengine-worker.service "TaskEngine worker ($id)" -- \
        "WorkingDirectory=$REPO" "EnvironmentFile=$ENV_FILE" \
        "ExecStart=$BIN worker --server-url \${TASKENGINE_URL} --worker-id $id"
      linux_start taskengine-worker.service ;;
    *) die "unsupported OS $OS" ;;
  esac
}

install_producer() {
  load_url
  local token; token="$(require_env TASKENGINE_PRODUCER_TOKEN "Producer token (your API key or another client token)" secret)"
  verify_client_token "$token"
  build
  local args=(producer --server-url "$TASKENGINE_URL" --tasks-dir tasks --interval "$PRODUCER_INTERVAL")
  case "$OS" in
    Darwin)
      mac_install com.home.taskengine-producer TASKENGINE_PRODUCER_TOKEN "$token" -- "${args[@]}" ;;
    Linux)
      linux_install taskengine-producer.service "TaskEngine producer" -- \
        "WorkingDirectory=$REPO" "EnvironmentFile=$ENV_FILE" \
        "ExecStart=$BIN producer --server-url \${TASKENGINE_URL} --tasks-dir tasks --interval $PRODUCER_INTERVAL"
      linux_start taskengine-producer.service ;;
    *) die "unsupported OS $OS" ;;
  esac
}

install_comfyui() {
  local dir="${1%/}" listen="${COMFYUI_LISTEN:-127.0.0.1}" port="${COMFYUI_PORT:-8188}"
  [ "$OS" = Linux ] || die "comfyui service is only supported on Linux"
  [ -n "$dir" ] || die "COMFYUI_DIR is required"
  [ -f "$dir/main.py" ] || die "$dir/main.py not found"
  [ -x "$dir/venv/bin/python" ] || die "$dir/venv/bin/python not found"
  # The directory may sit on a drive mounted late (fstab nofail); wait for it.
  linux_install comfyui.service "ComfyUI ($listen:$port)" -- \
    "WorkingDirectory=$dir" \
    "ExecStartPre=/usr/bin/test -f \"$dir/main.py\"" \
    "ExecStart=\"$dir/venv/bin/python\" main.py --listen $listen --port $port" \
    "Environment=PYTHONUNBUFFERED=1"
  if ss -ltn "sport = :$port" 2>/dev/null | grep -q LISTEN; then
    echo "==> comfyui.service enabled for boot, but port $port is in use (ComfyUI already running by hand)."
    echo "    Stop that instance, then: systemctl --user start comfyui"
  else
    linux_start comfyui.service
  fi
}

uninstall() {
  local what="${1:-worker}"
  case "$OS:$what" in
    Darwin:worker)   mac_uninstall com.home.taskengine-worker ;;
    Darwin:producer) mac_uninstall com.home.taskengine-producer ;;
    Linux:worker)    linux_uninstall taskengine-worker.service ;;
    Linux:producer)  linux_uninstall taskengine-producer.service ;;
    Linux:comfyui)   linux_uninstall comfyui.service ;;
    *) die "cannot uninstall '$what' on $OS" ;;
  esac
  echo "==> $what service removed ($ENV_FILE kept)"
}

case "$ACTION" in
  install)          [ -n "$ARG" ] || usage; install_worker "$ARG" ;;
  install-producer) install_producer ;;
  install-comfyui)  [ -n "$ARG" ] || usage; install_comfyui "$ARG" ;;
  uninstall)        uninstall "$ARG" ;;
  *) usage ;;
esac
