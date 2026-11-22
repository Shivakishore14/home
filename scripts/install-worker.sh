#!/usr/bin/env bash
# Install / uninstall this machine's TaskEngine worker as a boot-time service.
#
#   scripts/install-worker.sh install   <worker-id>   (make worker-install WORKER_ID=...)
#   scripts/install-worker.sh uninstall               (make worker-uninstall)
#
#   macOS: LaunchAgent com.home.taskengine-worker, started through
#          ~/.local/bin/taskengine-launcher (keeps Full Disk Access across
#          rebuilds). Starts at boot when the user auto-logs in. Retires the
#          old local server agent (com.home.taskengine-server) if present.
#   Linux: systemd user unit taskengine-worker.service (needs lingering to
#          start at boot: loginctl enable-linger $USER).
#
# The server URL and worker token live in ~/.config/taskengine/worker.env
# (mode 600), created on first run; they are never written to the repo.
# Which plugins the worker runs comes from tasks/workers/<worker-id>.yaml on
# the server.
set -euo pipefail

ACTION="${1:-}"
WORKER_ID="${2:-}"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$REPO/src/taskengine/bin/taskengine"
ENV_DIR="$HOME/.config/taskengine"
ENV_FILE="$ENV_DIR/worker.env"
OS="$(uname -s)"

MAC_LABEL="com.home.taskengine-worker"
MAC_OLD_LABEL="com.home.taskengine-server"
MAC_AGENTS="$HOME/Library/LaunchAgents"
MAC_PLIST="$MAC_AGENTS/$MAC_LABEL.plist"
MAC_LAUNCHER="$HOME/.local/bin/taskengine-launcher"
LINUX_UNIT_DIR="$HOME/.config/systemd/user"
LINUX_UNIT="taskengine-worker.service"

die() { echo "ERROR: $*" >&2; exit 1; }

usage() {
  echo "usage: $0 install <worker-id> | uninstall" >&2
  exit 2
}

load_env() {
  if [ ! -f "$ENV_FILE" ]; then
    [ -t 0 ] || die "$ENV_FILE missing. Create it with TASKENGINE_URL=... and TASKENGINE_TOKEN=... (chmod 600), or rerun in a terminal to be prompted."
    echo "==> $ENV_FILE not found; creating it"
    local url token
    read -r -p "TaskEngine server URL (https://...): " url
    read -r -s -p "Worker token: " token; echo
    [ -n "$url" ] && [ -n "$token" ] || die "URL and token are required"
    mkdir -p "$ENV_DIR"
    (umask 077; printf 'TASKENGINE_URL=%s\nTASKENGINE_TOKEN=%s\n' "${url%/}" "$token" > "$ENV_FILE")
  fi
  chmod 600 "$ENV_FILE"
  # Parse KEY=VALUE lines literally (never execute the file), like systemd's
  # EnvironmentFile; optional surrounding quotes are stripped.
  local key value
  while IFS='=' read -r key value || [ -n "$key" ]; do
    case "$key" in ''|\#*) continue ;; esac
    value="${value%$'\r'}"
    if [[ "$value" =~ ^\"(.*)\"$ || "$value" =~ ^\'(.*)\'$ ]]; then value="${BASH_REMATCH[1]}"; fi
    case "$key" in
      TASKENGINE_URL) TASKENGINE_URL="${value%/}" ;;
      TASKENGINE_TOKEN) TASKENGINE_TOKEN="$value" ;;
    esac
  done < "$ENV_FILE"
  [ -n "${TASKENGINE_URL:-}" ] || die "TASKENGINE_URL not set in $ENV_FILE"
  [ -n "${TASKENGINE_TOKEN:-}" ] || die "TASKENGINE_TOKEN not set in $ENV_FILE"
}

verify_token() {
  # A claim for a plugin nobody has returns 204 with a valid token, 401 otherwise.
  local code
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $TASKENGINE_TOKEN" \
    -d "{\"worker_id\":\"$WORKER_ID\",\"plugins\":[\"__probe__\"]}" "$TASKENGINE_URL/api/v1/tasks/claim") || true
  case "$code" in
    204) echo "==> Token accepted by $TASKENGINE_URL" ;;
    401) die "token rejected by $TASKENGINE_URL (401)" ;;
    *)   die "unexpected response '$code' from $TASKENGINE_URL" ;;
  esac
}

build() {
  echo "==> Building taskengine"
  local gocmd=()
  if go version >/dev/null 2>&1; then
    gocmd=(go)
  elif command -v mise >/dev/null 2>&1; then
    gocmd=(mise x go@1.25 -- go)
  elif [ -x /opt/homebrew/bin/go ]; then
    gocmd=(/opt/homebrew/bin/go)
  else
    die "Go not found"
  fi
  (cd "$REPO/src/taskengine" && GOTOOLCHAIN=auto "${gocmd[@]}" build -o bin/taskengine.new ./cmd/taskengine && mv bin/taskengine.new bin/taskengine)
}

xml_escape() { sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' <<<"$1"; }

install_macos() {
  local domain="gui/$(id -u)" program=()
  if [ -x "$MAC_LAUNCHER" ]; then
    program=("$MAC_LAUNCHER" "$BIN")
  else
    echo "WARN: $MAC_LAUNCHER not found; running taskengine directly (Full Disk Access must be granted to the binary)"
    program=("$BIN")
  fi

  if [ -f "$MAC_AGENTS/$MAC_OLD_LABEL.plist" ]; then
    echo "==> Retiring local server ($MAC_OLD_LABEL)"
    launchctl bootout "$domain/$MAC_OLD_LABEL" 2>/dev/null || true
    mkdir -p "$MAC_AGENTS/disabled"
    mv "$MAC_AGENTS/$MAC_OLD_LABEL.plist" "$MAC_AGENTS/disabled/"
  fi

  echo "==> Installing $MAC_PLIST"
  launchctl bootout "$domain/$MAC_LABEL" 2>/dev/null || true
  mkdir -p "$MAC_AGENTS"
  local args="" a
  for a in "${program[@]}" worker --server-url "$TASKENGINE_URL" --worker-id "$WORKER_ID"; do
    args+="		<string>$(xml_escape "$a")</string>"$'\n'
  done
  (umask 077; cat > "$MAC_PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$MAC_LABEL</string>
	<key>ProgramArguments</key>
	<array>
$args	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
		<key>TASKENGINE_TOKEN</key>
		<string>$(xml_escape "$TASKENGINE_TOKEN")</string>
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
	<string>$HOME/Library/Logs/taskengine-worker.log</string>
	<key>StandardErrorPath</key>
	<string>$HOME/Library/Logs/taskengine-worker.log</string>
</dict>
</plist>
EOF
  )
  plutil -lint "$MAC_PLIST" >/dev/null
  launchctl bootstrap "$domain" "$MAC_PLIST"
  sleep 3
  launchctl print "$domain/$MAC_LABEL" | grep -E "^\s*(state|pid) =" || true
  tail -n 5 "$HOME/Library/Logs/taskengine-worker.log" || true
}

install_linux() {
  local unit_path="$LINUX_UNIT_DIR/$LINUX_UNIT"
  mkdir -p "$LINUX_UNIT_DIR"
  if [ -f "$unit_path" ] && ! grep -q "Managed by scripts/install-worker.sh" "$unit_path"; then
    cp "$unit_path" "$unit_path.bak-$(date +%Y%m%d%H%M%S)"
    echo "==> Backed up existing $LINUX_UNIT"
  fi
  echo "==> Installing $unit_path"
  cat > "$unit_path" <<EOF
# Managed by scripts/install-worker.sh — rerun it instead of editing.
[Unit]
Description=TaskEngine worker ($WORKER_ID)
After=network-online.target
Wants=network-online.target

[Service]
WorkingDirectory=$REPO
EnvironmentFile=$ENV_FILE
ExecStart=$BIN worker --server-url \${TASKENGINE_URL} --worker-id $WORKER_ID
Restart=always
RestartSec=30

[Install]
WantedBy=default.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable "$LINUX_UNIT" >/dev/null
  systemctl --user restart "$LINUX_UNIT"
  if [ "$(loginctl show-user "$USER" -p Linger --value 2>/dev/null)" != "yes" ]; then
    echo "WARN: lingering is off; the worker only starts after login. Enable boot start with: loginctl enable-linger $USER"
  fi
  sleep 3
  systemctl --user --no-pager status "$LINUX_UNIT" | sed -n 1,5p
  journalctl --user -u "$LINUX_UNIT" -n 5 --no-pager || true
}

uninstall() {
  case "$OS" in
    Darwin)
      launchctl bootout "gui/$(id -u)/$MAC_LABEL" 2>/dev/null || true
      rm -f "$MAC_PLIST"
      ;;
    Linux)
      systemctl --user disable --now "$LINUX_UNIT" 2>/dev/null || true
      rm -f "$LINUX_UNIT_DIR/$LINUX_UNIT"
      systemctl --user daemon-reload
      ;;
  esac
  echo "==> Worker service removed ($ENV_FILE kept)"
}

case "$ACTION" in
  install)
    [ -n "$WORKER_ID" ] || usage
    [[ "$WORKER_ID" =~ ^[A-Za-z0-9_-]+$ ]] || die "invalid worker id '$WORKER_ID'"
    [ -f "$REPO/tasks/workers/$WORKER_ID.yaml" ] || echo "WARN: no tasks/workers/$WORKER_ID.yaml; the worker will run default (non-opt-in) plugins"
    load_env
    verify_token
    build
    case "$OS" in
      Darwin) install_macos ;;
      Linux) install_linux ;;
      *) die "unsupported OS $OS" ;;
    esac
    ;;
  uninstall) uninstall ;;
  *) usage ;;
esac
