#!/usr/bin/env bash
# Mount the Mac mini media share on rootpc (Linux worker) over Tailscale.
# Mounts the Mac's "media" SMB share (/Volumes/drive001/media) at /mnt/macmini_media/media,
# matching path_mappings in tasks/workers/rootpc.yaml.
#
# Usage: sudo ./scripts/setup-rootpc-mount.sh
set -euo pipefail

HOST="${HOST:-shivas-mac-mini}"
SHARE="${SHARE:-media}"
MNT="/mnt/macmini_media/media"
CREDS="/etc/samba/creds-macmini"

[ "$(id -u)" -eq 0 ] || { echo "Run with sudo"; exit 1; }
OWNER="${SUDO_USER:-shiva}"
OUID="$(id -u "$OWNER")"; OGID="$(id -g "$OWNER")"

read -rp "Mac username [server]: " SMBUSER; SMBUSER="${SMBUSER:-server}"
read -rsp "Mac password: " SMBPASS; echo

smbclient "//$HOST/$SHARE" -U "$SMBUSER%$SMBPASS" -c 'ls' >/dev/null \
  || { echo "Cannot log into //$HOST/$SHARE with those credentials"; exit 1; }

mkdir -p /etc/samba "$MNT"
umask 077
printf 'username=%s\npassword=%s\n' "$SMBUSER" "$SMBPASS" > "$CREDS"
chmod 600 "$CREDS"

LINE="//$HOST/$SHARE $MNT cifs credentials=$CREDS,uid=$OUID,gid=$OGID,file_mode=0664,dir_mode=0775,iocharset=utf8,vers=3.0,_netdev,nofail,x-systemd.automount,x-systemd.idle-timeout=0,x-systemd.requires=tailscaled.service,x-systemd.after=tailscaled.service 0 0"
sed -i "\|[[:space:]]$MNT[[:space:]]|d" /etc/fstab
echo "$LINE" >> /etc/fstab

systemctl daemon-reload
systemctl restart remote-fs.target
ls "$MNT" >/dev/null   # trigger automount

mountpoint -q "$MNT" && echo "OK: $MNT mounted:" && ls "$MNT" | head

sudo -u "$OWNER" XDG_RUNTIME_DIR="/run/user/$OUID" systemctl --user restart taskengine-worker.service || true
echo "Worker restarted. Check: systemctl --user status taskengine-worker"
