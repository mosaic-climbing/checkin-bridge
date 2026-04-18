#!/bin/bash
# ──────────────────────────────────────────────────────────────
# Mosaic Climbing – Install check-in bridge on UDM Pro
#
# Prerequisites:
#   - SSH access to your UDM Pro
#   - nspawn container already set up (see setup-container.sh)
#   - mosaic-bridge binary cross-compiled for linux/arm64
#
# Usage:
#   scp mosaic-bridge root@<UDM-IP>:/data/custom/machines/mosaic-bridge/opt/mosaic-bridge/
#   scp .env root@<UDM-IP>:/data/custom/machines/mosaic-bridge/opt/mosaic-bridge/
#   ssh root@<UDM-IP> bash /data/custom/machines/mosaic-bridge/opt/mosaic-bridge/install.sh
# ──────────────────────────────────────────────────────────────

set -euo pipefail

BRIDGE_DIR="/opt/mosaic-bridge"
BINARY="${BRIDGE_DIR}/mosaic-bridge"
ENV_FILE="${BRIDGE_DIR}/.env"

echo "=== Installing Mosaic Check-in Bridge ==="

# Verify files exist
if [ ! -f "$BINARY" ]; then
    echo "ERROR: Binary not found at $BINARY"
    echo "Copy it first: scp mosaic-bridge root@<UDM-IP>:/data/custom/machines/mosaic-bridge${BRIDGE_DIR}/"
    exit 1
fi

if [ ! -f "$ENV_FILE" ]; then
    echo "ERROR: .env not found at $ENV_FILE"
    echo "Copy it first: scp .env root@<UDM-IP>:/data/custom/machines/mosaic-bridge${BRIDGE_DIR}/"
    exit 1
fi

chmod +x "$BINARY"

# Create data directory for card mappings
mkdir -p "${BRIDGE_DIR}/data"

# Create systemd service
cat > /etc/systemd/system/mosaic-bridge.service <<'EOF'
[Unit]
Description=Mosaic Climbing Check-in Bridge
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/mosaic-bridge/mosaic-bridge
WorkingDirectory=/opt/mosaic-bridge
Restart=always
RestartSec=5
StartLimitInterval=60
StartLimitBurst=10

# Logging
StandardOutput=journal
StandardError=journal
SyslogIdentifier=mosaic-bridge

[Install]
WantedBy=multi-user.target
EOF

# Enable and start
systemctl daemon-reload
systemctl enable mosaic-bridge.service
systemctl start mosaic-bridge.service

echo ""
echo "=== Installation complete ==="
echo ""
echo "Service status:"
systemctl status mosaic-bridge.service --no-pager
echo ""
echo "Useful commands:"
echo "  systemctl status mosaic-bridge    # check status"
echo "  journalctl -u mosaic-bridge -f    # follow logs"
echo "  systemctl restart mosaic-bridge   # restart after .env change"
echo ""
echo "Admin API: curl http://localhost:3500/health"
