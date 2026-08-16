#!/usr/bin/env bash
set -e

DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
REPO_ROOT="$(dirname "$(dirname "$DIR")")"
BIN="$DIR/bin/taskengine"
PORT=8099
DB_PATH="/tmp/taskengine_e2e.db"
TEST_MEDIA_DIR="/tmp/taskengine_e2e_media"

echo "=========================================================="
echo "⚡ Starting TaskEngine Full End-to-End Test (E2E)"
echo "=========================================================="

rm -f "$DB_PATH"*
rm -rf "$TEST_MEDIA_DIR"
mkdir -p "$TEST_MEDIA_DIR"

# 1. Build binary
echo "[1/7] Building TaskEngine binary..."
make -C "$DIR" build

# 2. Generate a 1-second sample video using ffmpeg
echo "[2/7] Generating synthetic video test clip..."
SAMPLE_VIDEO="$TEST_MEDIA_DIR/input_video.mkv"
ffmpeg -y -f lavfi -i testsrc=duration=1:size=320x240:rate=10 -c:v libx264 "$SAMPLE_VIDEO" >/dev/null 2>&1

# 3. Start Server in background
echo "[3/7] Starting TaskEngine Server on port $PORT..."
"$BIN" server --port "$PORT" --tasks-dir "$REPO_ROOT/tasks" --db-path "$DB_PATH" &
SERVER_PID=$!

cleanup() {
    echo "Cleaning up processes and files..."
    kill $WORKER_PID 2>/dev/null || true
    kill $SERVER_PID 2>/dev/null || true
    rm -f "$DB_PATH"*
    rm -rf "$TEST_MEDIA_DIR"
}
trap cleanup EXIT

# Wait for server ready
sleep 1
curl -s "http://127.0.0.1:$PORT/api/v1/stats" >/dev/null || (sleep 1 && curl -s "http://127.0.0.1:$PORT/api/v1/stats" >/dev/null)

# 4. Start Worker in background
echo "[4/7] Starting TaskEngine Worker..."
"$BIN" worker --server-url "http://127.0.0.1:$PORT" --worker-id "e2e-worker" --concurrency 2 --poll-interval 200ms &
WORKER_PID=$!
sleep 1

# 5. Enqueue Command-Runner Task
echo "[5/7] Enqueueing Command Runner Task..."
TASK1_RESP=$(curl -s -X POST "http://127.0.0.1:$PORT/api/v1/tasks" \
  -H "Content-Type: application/json" \
  -d '{"plugin_name": "command-runner", "priority": 10, "params": {"command": "echo E2E_COMMAND_OK; echo PROGRESS: 100%"}}')
TASK1_ID=$(echo "$TASK1_RESP" | grep -o '"id":"[^"]*' | cut -d'"' -f4)
echo "Enqueued Command Task ID: $TASK1_ID"

# 6. Enqueue Video-Transcode Task
echo "[6/7] Enqueueing Video Transcode Task on $SAMPLE_VIDEO..."
TASK2_RESP=$(curl -s -X POST "http://127.0.0.1:$PORT/api/v1/tasks" \
  -H "Content-Type: application/json" \
  -d "{\"plugin_name\": \"video-transcoder\", \"target_file\": \"$SAMPLE_VIDEO\", \"priority\": 5, \"params\": {\"target_codec\": \"libx264\", \"crf\": 28, \"preset\": \"ultrafast\", \"container\": \"mp4\"}}")
TASK2_ID=$(echo "$TASK2_RESP" | grep -o '"id":"[^"]*' | cut -d'"' -f4)
echo "Enqueued Transcode Task ID: $TASK2_ID"

# Wait for both tasks to complete
echo "Waiting for worker to process tasks..."
for i in {1..30}; do
    TASK1_STATUS=$(curl -s "http://127.0.0.1:$PORT/api/v1/tasks/$TASK1_ID" | grep -o '"status":"[^"]*' | cut -d'"' -f4)
    TASK2_STATUS=$(curl -s "http://127.0.0.1:$PORT/api/v1/tasks/$TASK2_ID" | grep -o '"status":"[^"]*' | cut -d'"' -f4)
    
    if [ "$TASK1_STATUS" = "COMPLETED" ] && [ "$TASK2_STATUS" = "COMPLETED" ]; then
        echo "Both tasks completed successfully!"
        break
    fi
    sleep 0.5
done

if [ "$TASK1_STATUS" != "COMPLETED" ] || [ "$TASK2_STATUS" != "COMPLETED" ]; then
    echo "ERROR: Tasks did not complete in time! Task1: $TASK1_STATUS, Task2: $TASK2_STATUS"
    exit 1
fi

# Verify transcoded output file exists
EXPECTED_OUTPUT="$TEST_MEDIA_DIR/input_video.transcoded.mp4"
if [ ! -f "$EXPECTED_OUTPUT" ]; then
    echo "ERROR: Transcoded output file $EXPECTED_OUTPUT not found!"
    exit 1
fi
echo "Verified transcoded media file: $EXPECTED_OUTPUT ($(wc -c < "$EXPECTED_OUTPUT") bytes)"

# 7. Enqueue Task with Prerequisites & Synced Assets
echo "[7/8] Enqueueing Task with Prerequisites & Synced Python Assets..."
TASK3_RESP=$(curl -s -X POST "http://127.0.0.1:$PORT/api/v1/tasks" \
  -H "Content-Type: application/json" \
  -d '{"plugin_name": "command-runner", "priority": 8, "params": {"task_name": "system-metrics-reporter", "prerequisites": {"check_command": "which python3"}, "command": "python3 $TASK_ASSETS_DIR/gather_metrics.py"}}')
TASK3_ID=$(echo "$TASK3_RESP" | grep -o '"id":"[^"]*' | cut -d'"' -f4)
echo "Enqueued Synced Asset Task ID: $TASK3_ID"

for i in {1..30}; do
    TASK3_STATUS=$(curl -s "http://127.0.0.1:$PORT/api/v1/tasks/$TASK3_ID" | grep -o '"status":"[^"]*' | cut -d'"' -f4)
    if [ "$TASK3_STATUS" = "COMPLETED" ]; then
        echo "Asset sync task completed successfully!"
        break
    fi
    sleep 0.5
done

if [ "$TASK3_STATUS" != "COMPLETED" ]; then
    echo "ERROR: Task3 did not complete! Status: $TASK3_STATUS"
    exit 1
fi

# 8. Test Config Reload Endpoint & CLI
echo "[8/8] Testing config reload CLI..."
"$BIN" reload --server-url "http://127.0.0.1:$PORT"
"$BIN" status --server-url "http://127.0.0.1:$PORT"

echo "=========================================================="
echo " [✓] TaskEngine E2E Test Passed Successfully!"
echo "=========================================================="
