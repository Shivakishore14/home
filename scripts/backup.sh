#!/usr/bin/env bash
# ==============================================================================
# Home Server Data Backup Script
# ==============================================================================
# Archives the docker application data directory (${DOCKER_DIR}) into a 
# timestamped gzip archive stored in ${BACKUP_DIR}.
#
# To prevent database corruption (SQLite/etc.), the script will stop the containers
# during the archiving process and bring them back up afterwards.
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
TIMESTAMP=$(date +"%Y%m%d_%H%M%S")
BACKUP_FILENAME="home_backup_${TIMESTAMP}.tar.gz"
BACKUP_PATH="${BACKUP_DIR}/${BACKUP_FILENAME}"

echo "======================================================================"
echo " Starting Application Data Backup"
echo " Timestamp: $(date)"
echo "======================================================================"

# Check if docker config directory exists and is not empty
if [ ! -d "${DOCKER_DIR}" ] || [ -z "$(ls -A "${DOCKER_DIR}")" ]; then
    echo " [X] ERROR: DOCKER_DIR (${DOCKER_DIR}) does not exist or is empty."
    echo "     Nothing to backup."
    exit 1
fi

# Ensure backups directory exists
mkdir -p "${BACKUP_DIR}"

# 1. Stop the running services to prevent data corruption during tar
echo " [!] Stopping Docker Compose services to secure database state..."
if command -v docker &>/dev/null && docker compose version &>/dev/null; then
    (cd "${REPO_ROOT}" && docker compose down)
else
    echo " [!] Docker Compose not found. Proceeding with backup anyway (databases might be active!)."
fi

# 2. Perform archiving
echo " [!] Archiving ${DOCKER_DIR} to ${BACKUP_PATH}..."
# Use tar -czf. Note: -C shifts the context to avoid absolute path warnings, 
# storing relative to the parent directory of 'docker' folder.
PARENT_DOCKER="$(dirname "${DOCKER_DIR}")"
DIR_NAME="$(basename "${DOCKER_DIR}")"

if tar -czf "${BACKUP_PATH}" -C "${PARENT_DOCKER}" "${DIR_NAME}"; then
    echo " [✓] Backup archive created successfully."
else
    echo " [X] ERROR: Backup archiving failed!"
    # Ensure we attempt to start services back up even on failure
    if command -v docker &>/dev/null && docker compose version &>/dev/null; then
        echo " [!] Restoring Docker Compose services..."
        (cd "${REPO_ROOT}" && docker compose up -d)
    fi
    exit 1
fi

# 3. Restart services
echo " [!] Restarting Docker Compose services..."
if command -v docker &>/dev/null && docker compose version &>/dev/null; then
    (cd "${REPO_ROOT}" && docker compose up -d)
fi

# 4. Display backup metrics
FILE_SIZE=$(du -h "${BACKUP_PATH}" | cut -f1)
echo "----------------------------------------------------------------------"
echo " Backup Summary:"
echo "   Archive Path: ${BACKUP_PATH}"
echo "   Archive Size: ${FILE_SIZE}"
echo "======================================================================"
echo " [✓] SUCCESS: Backup operation complete!"
echo "======================================================================"
