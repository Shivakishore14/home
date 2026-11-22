#!/usr/bin/env bash
# Install the TaskEngine worker on the Mac mini as a LaunchAgent that starts
# at boot (the "server" user auto-logs in), and retire the old local server.
#
#   TASKENGINE_URL=https://<host> scripts/install-macmini-worker.sh
#
# The worker token is read from $TASKENGINE_TOKEN or prompted for. URL and
# token are written only into the plist (mode 600), never into the repo.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
LABEL="com.home.taskengine-worker"
OLD_LABEL="com.home.taskengine-server"
AGENTS="$HOME/Library/LaunchAgents"
PLIST="$AGENTS/$LABEL.plist"
LAUNCHER="$HOME/.local/bin/taskengine-launcher"
BIN="$REPO/src/taskengine/bin/taskengine"
LOG="$HOME/Library/Logs/taskengine-worker.log"
WORKER_ID="${WORKER_ID:-macmini}"
PLUGINS="${PLUGINS:-video-transcoder}"
DOMAIN="gui/$(id -u)"

: "${TASKENGINE_URL:?set TASKENGINE_URL (e.g. https://taskengine.example.com)}"
if [ -z "${TASKENGINE_TOKEN:-}" ]; then
  read -r -s -p "Worker token: " TASKENGINE_TOKEN; echo
fi
[ -n "$TASKENGINE_TOKEN" ] || { echo "token is required" >&2; exit 1; }

# Fail early if the token is wrong rather than crash-looping under launchd.
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $TASKENGINE_TOKEN" \
  -d "{\"worker_id\":\"$WORKER_ID\",\"plugins\":[\"__probe__\"]}" "$TASKENGINE_URL/api/v1/tasks/claim")
case "$code" in
  204) echo "==> Token accepted by $TASKENGINE_URL" ;;
  401) echo "ERROR: token rejected (401)" >&2; exit 1 ;;
  *)   echo "ERROR: unexpected response $code from $TASKENGINE_URL" >&2; exit 1 ;;
esac

[ -x "$LAUNCHER" ] || { echo "ERROR: $LAUNCHER missing (keeps Full Disk Access across rebuilds; see src/taskengine/cmd/fda-launcher)" >&2; exit 1; }

echo "==> Building taskengine"
GO="$(command -v go || echo /opt/homebrew/bin/go)"
(cd "$REPO/src/taskengine" && "$GO" build -o bin/taskengine.new ./cmd/taskengine && mv bin/taskengine.new bin/taskengine)

if [ -f "$AGENTS/$OLD_LABEL.plist" ]; then
  echo "==> Retiring local server ($OLD_LABEL)"
  launchctl bootout "$DOMAIN/$OLD_LABEL" 2>/dev/null || true
  mkdir -p "$AGENTS/disabled"
  mv "$AGENTS/$OLD_LABEL.plist" "$AGENTS/disabled/"
fi

echo "==> Installing $PLIST"
launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
umask 077
cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$LABEL</string>
	<key>ProgramArguments</key>
	<array>
		<string>$LAUNCHER</string>
		<string>$BIN</string>
		<string>worker</string>
		<string>--server-url</string>
		<string>$TASKENGINE_URL</string>
		<string>--worker-id</string>
		<string>$WORKER_ID</string>
		<string>--plugins</string>
		<string>$PLUGINS</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
		<key>TASKENGINE_TOKEN</key>
		<string>$TASKENGINE_TOKEN</string>
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
	<string>$LOG</string>
	<key>StandardErrorPath</key>
	<string>$LOG</string>
</dict>
</plist>
EOF
chmod 600 "$PLIST"

launchctl bootstrap "$DOMAIN" "$PLIST"
sleep 3
launchctl print "$DOMAIN/$LABEL" | grep -E "state =|pid =" || true
tail -5 "$LOG"
