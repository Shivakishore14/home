#!/usr/bin/env bash
# ==============================================================================
# Home Server Startup Automation Script
# ==============================================================================
# This script is triggered on login/boot via a macOS Launch Agent. It ensures
# Docker Desktop is running, waits for the daemon to become active, and then
# starts the Home Server services.
# ==============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "${SCRIPT_DIR}")"

echo "======================================================================"
echo " Starting Home Server Boot Automation..."
echo "======================================================================"
echo "Timestamp: $(date)"

# 1. Start Docker Desktop if it is not already running
if ! docker info >/dev/null 2>&1; then
    echo "==> Docker daemon not reachable. Launching Docker Desktop..."
    open -a Docker
    
    # Wait for the Docker daemon to become responsive
    echo "==> Waiting for Docker daemon to initialize..."
    ATTEMPTS=0
    MAX_ATTEMPTS=30 # 60 seconds total wait time
    
    while ! docker info >/dev/null 2>&1; do
        ATTEMPTS=$((ATTEMPTS + 1))
        if [ "$ATTEMPTS" -ge "$MAX_ATTEMPTS" ]; then
            echo " [X] ERROR: Docker daemon failed to start after 60 seconds."
            exit 1
        fi
        sleep 2
    done
fi

echo " [✓] Docker daemon is running and active."

# 2. Change directory to repo root and start services
cd "${REPO_ROOT}"
echo "==> Starting home server services via 'make up'..."
make up

echo "======================================================================"
echo " [✓] SUCCESS: Home Server startup complete!"
echo "======================================================================"
