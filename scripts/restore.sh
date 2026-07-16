#!/usr/bin/env bash
# ==============================================================================
# Home Server Data Restore Script
# ==============================================================================
# Restores the docker application data directory (${DOCKER_DIR}) from a
# timestamped gzip archive located in ${BACKUP_DIR}.
#
# Safeguards:
# - Temporarily stops Docker Compose services.
# - Moves existing active configuration to ${DOCKER_DIR}.old instead of deleting it.
# - Prompts for confirmation before proceeding.
# ==============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "${SCRIPT_DIR}")"

# Load environment configuration
if [ ! -f "${REPO_ROOT}/.env" ]; then
    echo " [X] ERROR: .env file not found. Run 'make init' first."
    exit 1
fi
# shellcheck disable=SC1091
source "${REPO_ROOT}/.env"

# Configuration variables
DOCKER_DIR="${DOCKER_DIR:-/Volumes/drive001/docker}"
BACKUP_DIR="${BACKUP_DIR:-${REPO_ROOT}/backups}"

echo "======================================================================"
echo " Starting Application Data Restoration"
echo "======================================================================"

# 1. Scan and present available backups
if [ ! -d "${BACKUP_DIR}" ] || [ -z "$(ls -A "${BACKUP_DIR}"/*.tar.gz 2>/dev/null)" ]; then
    echo " [X] ERROR: No backup files (*.tar.gz) found in ${BACKUP_DIR}."
    exit 1
fi

echo "Available Backups:"
backups=()
# Use a counter-based array populate
i=0
for f in "${BACKUP_DIR}"/*.tar.gz; do
    [ -e "$f" ] || continue
    i=$((i + 1))
    backups+=("$f")
    echo "  [$i] $(basename "$f") ($(du -h "$f" | cut -f1))"
done

echo ""
read -r -p "Enter backup number to restore (1-$i): " selection

# Validate input is a number in range
if ! [[ "$selection" =~ ^[0-9]+$ ]] || [ "$selection" -lt 1 ] || [ "$selection" -gt "$i" ]; then
    echo " [X] ERROR: Invalid selection."
    exit 1
fi

SELECTED_BACKUP="${backups[$((selection - 1))]}"
echo " [✓] Selected backup: $(basename "${SELECTED_BACKUP}")"
echo ""
echo " [!] WARNING: This will temporarily stop services, move current configurations"
echo "     to '${DOCKER_DIR}.old' and restore the chosen archive."
read -r -p " Are you sure you want to proceed? (y/N): " confirm
if [[ ! "$confirm" =~ ^[yY](es)?$ ]]; then
    echo " [!] Operation cancelled."
    exit 0
fi

# 2. Stop running services
echo " [!] Stopping Docker Compose services..."
if command -v docker &>/dev/null && docker compose version &>/dev/null; then
    (cd "${REPO_ROOT}" && docker compose down)
else
    echo " [!] Docker Compose not found. Proceeding without stopping containers."
fi

# 3. Handle active directories safely (move instead of delete)
if [ -d "${DOCKER_DIR}" ]; then
    OLD_DIR="${DOCKER_DIR}.old_$(date +%Y%m%d_%H%M%S)"
    echo " [!] Moving current active directory to ${OLD_DIR}..."
    mv "${DOCKER_DIR}" "${OLD_DIR}"
fi

# 4. Extract backup
echo " [!] Restoring archive..."
PARENT_DOCKER="$(dirname "${DOCKER_DIR}")"
mkdir -p "${DOCKER_DIR}"

if tar -xzf "${SELECTED_BACKUP}" -C "${PARENT_DOCKER}"; then
    echo " [✓] Extraction completed successfully."
else
    echo " [X] ERROR: Extraction failed! Attempting to revert from backup folder..."
    if [ -d "${OLD_DIR:-}" ]; then
        rm -rf "${DOCKER_DIR}"
        mv "${OLD_DIR}" "${DOCKER_DIR}"
        echo " [✓] Restored former active directory configuration."
    fi
    exit 1
fi

# 5. Start containers
echo " [!] Restarting Docker Compose services..."
if command -v docker &>/dev/null && docker compose version &>/dev/null; then
    (cd "${REPO_ROOT}" && docker compose up -d)
fi

echo "======================================================================"
echo " [✓] SUCCESS: Restore complete!"
echo "     Old configuration directory has been preserved as:"
echo "     ${OLD_DIR:-None}"
echo "======================================================================"
