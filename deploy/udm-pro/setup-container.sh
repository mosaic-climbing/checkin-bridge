#!/bin/bash
# ──────────────────────────────────────────────────────────────
# Set up an nspawn container for the Mosaic check-in bridge on UDM Pro
#
# UniFi OS 3.x+ removed Docker/Podman, so we use systemd-nspawn.
# The /data partition persists across firmware updates.
#
# Run this via SSH on your UDM Pro:
#   ssh root@<UDM-IP>
#   bash setup-container.sh
#
# Reference: https://github.com/unifi-utilities/unifios-utilities
# ──────────────────────────────────────────────────────────────

set -euo pipefail

CONTAINER_NAME="mosaic-bridge"
CONTAINER_DIR="/data/custom/machines/${CONTAINER_NAME}"

echo "=== Setting up nspawn container for Mosaic Bridge ==="
echo ""

# Step 1: Install nspawn tooling
echo "[1/6] Installing systemd-container and debootstrap..."
apt-get update -qq
apt-get install -y -qq systemd-container debootstrap

# Step 2: Create the container with a minimal Debian Bookworm base
if [ -d "$CONTAINER_DIR" ]; then
    echo "[2/6] Container directory already exists, skipping debootstrap"
else
    echo "[2/6] Creating Debian Bookworm container (this takes ~10 minutes)..."
    mkdir -p /data/custom/machines
    debootstrap --include=systemd,dbus bookworm "$CONTAINER_DIR"
fi

# Step 3: Configure the container
echo "[3/6] Configuring container..."
systemd-nspawn -D "$CONTAINER_DIR" --pipe bash <<'INNER'
# Set root password (change this!)
echo "root:mosaic2026" | chpasswd
# Enable networking
systemctl enable systemd-networkd
# DNS
echo "nameserver 1.1.1.1" > /etc/resolv.conf
echo "nameserver 8.8.8.8" >> /etc/resolv.conf
# Hostname
echo "mosaic-bridge" > /etc/hostname
# Create the bridge directory
mkdir -p /opt/mosaic-bridge/data
INNER

# Step 4: Create nspawn config
echo "[4/6] Creating nspawn configuration..."
mkdir -p /etc/systemd/nspawn
cat > "/etc/systemd/nspawn/${CONTAINER_NAME}.nspawn" <<EOF
[Exec]
Boot=on
ResolvConf=off

[Network]
# Share the host network so the bridge can reach both
# the UA-Hub (local) and Redpoint HQ (internet)
Private=off
VirtualEthernet=off
EOF

# Step 5: Link for machinectl management
echo "[5/6] Linking container for machinectl..."
mkdir -p /var/lib/machines
ln -sf "$CONTAINER_DIR" "/var/lib/machines/${CONTAINER_NAME}"

# Step 6: Enable and start
echo "[6/6] Starting container..."
machinectl enable "$CONTAINER_NAME"
machinectl start "$CONTAINER_NAME"

echo ""
echo "=== Container is running ==="
echo ""
echo "Access it with:    machinectl shell ${CONTAINER_NAME}"
echo "Container root:    ${CONTAINER_DIR}"
echo "Bridge dir:        ${CONTAINER_DIR}/opt/mosaic-bridge/"
echo ""
echo "Next steps:"
echo "  1. Cross-compile the bridge:  GOOS=linux GOARCH=arm64 go build -ldflags='-s -w' -o mosaic-bridge ./cmd/bridge"
echo "  2. Copy binary + .env to UDM: scp mosaic-bridge .env root@<UDM-IP>:${CONTAINER_DIR}/opt/mosaic-bridge/"
echo "  3. Install inside container:  machinectl shell ${CONTAINER_NAME} /bin/bash /opt/mosaic-bridge/install.sh"
