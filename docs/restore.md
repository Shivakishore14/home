# Backup & Restore Procedures

This document outlines the backup and restore procedures for the home server application data.

---

## 1. Automated Backups

To perform a backup, run:
```bash
make backup
```
This target invokes `scripts/backup.sh` which executes the following steps:
1. **Reads Configuration**: Sources host paths (`DOCKER_DIR`, `BACKUP_DIR`) from your `.env` file.
2. **Graceful Shutdown**: Stops the Docker Compose stack using `docker compose down` to release active file locks on SQLite/internal databases (Jellyfin/Home Assistant config locks).
3. **Archiving**: Compresses the active `${DOCKER_DIR}` directory into a `.tar.gz` archive inside `${BACKUP_DIR}` using timestamped naming:
   `home_backup_YYYYMMDD_HHMMSS.tar.gz`
4. **Resumes Stack**: Restarts the services using `docker compose up -d`.

---

## 2. Restoration Guide

In the event of database corruption, system reinstallation, or drive replacement, follow these steps to restore your configurations.

### Step 1: Initialize Environment
Ensure the environment directories and repository are cloned and initialized:
```bash
make init
```

### Step 2: Run Restore command
Initiate the interactive restoration prompt:
```bash
make restore
```

### Step 3: Select Backup Archive
The script scans the `backups/` directory (or the path defined by `${BACKUP_DIR}`) and lists available options:
```text
Available Backups:
  [1] home_backup_20260716_140000.tar.gz (45M)
  [2] home_backup_20260716_153000.tar.gz (48M)

Enter backup number to restore (1-2):
```
Enter the number corresponding to your desired restore point.

### Step 4: Safeguard confirmation
The restore script will prompt you:
```text
[!] WARNING: This will temporarily stop services, move current configurations
    to '/Volumes/drive001/docker.old_<timestamp>' and restore the chosen archive.
Are you sure you want to proceed? (y/N):
```
Type `y` and press **Enter**.

### Step 5: Verification & Cleanup
1. The script stops all running containers.
2. It renames the current `/Volumes/drive001/docker` folder to `/Volumes/drive001/docker.old_<timestamp>` to prevent data loss.
3. It extracts the archive to create a fresh `/Volumes/drive001/docker` folder.
4. It restarts the containers.
5. If the restoration succeeds, verify service availability by visiting:
   - Jellyfin: `http://localhost:8096`
   - Home Assistant: `http://localhost:8123`
6. Once validated, you can safely delete the old backup directory:
   ```bash
   rm -rf /Volumes/drive001/docker.old_*
   ```

---

## 3. Disasters Recovery (Fresh System Setup)

If your Mac mini or internal SSD crashes and you need to deploy the stack onto a clean machine:
1. Re-mount the external drive (`/Volumes/drive001`).
2. Clone the repository onto the new machine.
3. Create your `.env` file pointing to `/Volumes/drive001/docker` and your backups.
4. Run `make restore` to list backups on your drive or in the repository, and select the latest backup to unpack the Docker state.
5. All configurations will be restored, and containers will boot up exactly as they were.
