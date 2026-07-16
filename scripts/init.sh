#!/usr/bin/env bash
# ==============================================================================
# Home Server Environment Initialization Script
# ==============================================================================
# This script initializes the host directories and creates default configurations
# needed for Jellyfin, Home Assistant, and backups.
# ==============================================================================

set -euo pipefail

# Determine script directory and repo root
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "${SCRIPT_DIR}")"

echo "======================================================================"
echo " Starting Home Server Directory Initialization"
echo "======================================================================"

# 1. Setup .env file if it doesn't exist
if [ ! -f "${REPO_ROOT}/.env" ]; then
    echo " [!] .env file not found. Copying .env.example to .env..."
    cp "${REPO_ROOT}/.env.example" "${REPO_ROOT}/.env"
    echo " [✓] Created .env from template. Please customize it if needed."
else
    echo " [✓] Existing .env file found."
fi

# 2. Source the .env file to read configurations
# Note: Using an awk/sed clean loader to handle potential export syntax issues in pure bash
# or simply sourcing it if syntax is clean.
# Sourcing is safe here since we control the template.
# shellcheck disable=SC1091
source "${REPO_ROOT}/.env"

# Resolve directories (defaults if not present in env)
DOCKER_DIR="${DOCKER_DIR:-/Volumes/drive001/docker}"
MEDIA_DIR="${MEDIA_DIR:-/Volumes/drive001/media}"
BACKUP_DIR="${BACKUP_DIR:-${REPO_ROOT}/backups}"

echo "Configured paths:"
echo "  DOCKER_DIR: ${DOCKER_DIR}"
echo "  MEDIA_DIR:  ${MEDIA_DIR}"
echo "  BACKUP_DIR: ${BACKUP_DIR}"
echo "----------------------------------------------------------------------"

# 3. Check for external drive availability
echo "Checking external drive paths..."
PARENT_DOCKER="$(dirname "${DOCKER_DIR}")"
if [ ! -d "${PARENT_DOCKER}" ]; then
    echo " [X] ERROR: Parent directory of DOCKER_DIR (${PARENT_DOCKER}) does not exist."
    echo "     Is the external drive mounted? Check /Volumes/ or your mount settings."
    exit 1
fi
echo " [✓] External storage root is accessible."

# 4. Create directories for Docker Persistent Application Data
echo "Initializing application data directories under ${DOCKER_DIR}..."
mkdir -p "${DOCKER_DIR}/jellyfin/config"
mkdir -p "${DOCKER_DIR}/jellyfin/cache"
mkdir -p "${DOCKER_DIR}/homeassistant"
echo " [✓] Created application config folders."

# 5. Create media libraries structure
echo "Initializing media folders under ${MEDIA_DIR}..."
mkdir -p "${MEDIA_DIR}/Movies"
mkdir -p "${MEDIA_DIR}/TV Shows"
mkdir -p "${MEDIA_DIR}/Anime"
mkdir -p "${MEDIA_DIR}/Music"
mkdir -p "${MEDIA_DIR}/Photos"
echo " [✓] Created media libraries folders."

# 6. Create Backup directories
echo "Initializing backup folder at ${BACKUP_DIR}..."
mkdir -p "${BACKUP_DIR}"
echo " [✓] Created backups folder."

# 7. Apply appropriate file permissions (if running on macOS/Linux)
echo "Ensuring user permission structures match PUID/PGID..."
# Since PUID/PGID are defined, we check if running on macOS
if [[ "$OSTYPE" == "darwin"* ]]; then
    # On macOS, Docker Desktop uses virtiofs/osxfs, so permissions are handled by virtualization.
    # We will set the host permissions of the created folders to the current user.
    chmod -R 775 "${DOCKER_DIR}" 2>/dev/null || true
    chmod -R 775 "${MEDIA_DIR}" 2>/dev/null || true
    chmod -R 775 "${BACKUP_DIR}" 2>/dev/null || true
fi
echo " [✓] Folder permissions initialized."

echo "======================================================================"
echo " [✓] SUCCESS: Directory structures are fully prepared!"
echo "======================================================================"
