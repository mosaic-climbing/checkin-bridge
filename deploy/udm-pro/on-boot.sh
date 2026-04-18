#!/bin/bash
# ──────────────────────────────────────────────────────────────
# On-boot script: ensures the mosaic-bridge container survives
# UDM Pro firmware updates.
#
# Place this in /data/on_boot.d/ (requires on-boot-script from
# https://github.com/unifi-utilities/unifios-utilities)
#
# What firmware updates break:
#   - apt packages in /usr are wiped
#   - symlinks in /var/lib/machines/ are removed
#   - systemd service files outside /data are lost
#
# What persists:
#   - Everything under /data/ (including the container filesystem)
#
# This script re-establishes the plumbing on each boot.
# ──────────────────────────────────────────────────────────────

set -euo pipefail

CONTAINER_NAME="mosaic-bridge"
CONTAINER_DIR="/data/custom/machines/${CONTAINER_NAME}"

# Skip if container doesn't exist
[ -d "$CONTAINER_DIR" ] || exit 0

echo "[mosaic-bridge] Re-establishing container after boot..."

# Reinstall nspawn tooling if missing (wiped by firmware update)
if ! command -v machinectl &>/dev/null; then
    echo "[mosaic-bridge] Reinstalling systemd-container..."
    apt-get update -qq && apt-get install -y -qq systemd-container
fi

# Re-create symlink
mkdir -p /var/lib/machines
ln -sf "$CONTAINER_DIR" "/var/lib/machines/${CONTAINER_NAME}"

# Re-create nspawn config
mkdir -p /etc/systemd/nspawn
cat > "/etc/systemd/nspawn/${CONTAINER_NAME}.nspawn" <<EOF
[Exec]
Boot=on
ResolvConf=off

[Network]
Private=off
VirtualEthernet=off
EOF

# Start the container
machinectl enable "$CONTAINER_NAME" 2>/dev/null || true
machinectl start "$CONTAINER_NAME" 2>/dev/null || true

echo "[mosaic-bridge] Container started"
