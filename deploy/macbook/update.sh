#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────
# Mosaic Bridge — on-device updater
#
# Pulls the latest (or a specific) release from the GitHub
# repo, verifies SHA256, atomically swaps the binary,
# restarts launchd, and keeps a .prev for one-command
# rollback.
#
# Usage:
#   sudo /usr/local/mosaic-bridge/update.sh            # latest
#   sudo /usr/local/mosaic-bridge/update.sh v0.3.2     # specific tag
#   sudo /usr/local/mosaic-bridge/update.sh rollback   # revert to .prev
#
# Requires: curl, shasum, launchctl.
# Runs as root (it restarts a LaunchDaemon); the actual
# bridge process keeps running as the `mosaic` user.
# ──────────────────────────────────────────────────────────
set -euo pipefail

REPO="${REPO:-mosaic-climbing/checkin-bridge}"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/mosaic-bridge}"
SERVICE="com.mosaic.bridge"

# Detect arch / OS
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"          # darwin
ARCH_RAW="$(uname -m)"
case "$ARCH_RAW" in
    arm64|aarch64) ARCH="arm64" ;;
    x86_64|amd64)  ARCH="amd64" ;;
    *) echo "unsupported arch: $ARCH_RAW" >&2; exit 1 ;;
esac
ASSET="mosaic-bridge-${OS}-${ARCH}"

log()  { printf '[update] %s\n' "$*"; }
fatal(){ printf '[update] FATAL: %s\n' "$*" >&2; exit 1; }

require_root() {
    if [ "$(id -u)" -ne 0 ]; then
        fatal "must run as root (launchctl needs it). Try: sudo $0 $*"
    fi
}

# ──────────────────────────────────────────────────────────
# rollback
# ──────────────────────────────────────────────────────────
if [ "${1:-}" = "rollback" ]; then
    require_root "$@"
    [ -f "$INSTALL_DIR/mosaic-bridge.prev" ] || fatal "no .prev binary to roll back to"
    log "rolling back to previous binary"
    launchctl unload "/Library/LaunchDaemons/${SERVICE}.plist" 2>/dev/null || true
    mv "$INSTALL_DIR/mosaic-bridge"       "$INSTALL_DIR/mosaic-bridge.failed.$(date +%s)"
    mv "$INSTALL_DIR/mosaic-bridge.prev"  "$INSTALL_DIR/mosaic-bridge"
    launchctl load -w "/Library/LaunchDaemons/${SERVICE}.plist"
    log "rollback complete"
    exit 0
fi

require_root "$@"

# ──────────────────────────────────────────────────────────
# resolve tag: argument or "latest"
# ──────────────────────────────────────────────────────────
TAG="${1:-latest}"
if [ "$TAG" = "latest" ]; then
    log "resolving latest release tag"
    TAG=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
        | awk -F'"' '/"tag_name":/ {print $4; exit}')
    [ -n "$TAG" ] || fatal "could not resolve latest tag (is the repo public? is a release published?)"
fi
log "target tag: $TAG"

# Already running that version?
if [ -x "$INSTALL_DIR/mosaic-bridge" ]; then
    CURRENT=$("$INSTALL_DIR/mosaic-bridge" -version 2>/dev/null | awk '{print $2}' || true)
    if [ "$CURRENT" = "$TAG" ]; then
        log "already on $TAG; nothing to do"
        exit 0
    fi
    log "current version: ${CURRENT:-unknown} → $TAG"
fi

# ──────────────────────────────────────────────────────────
# fetch + verify
# ──────────────────────────────────────────────────────────
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

BASE="https://github.com/${REPO}/releases/download/${TAG}"
log "downloading $ASSET"
curl -fsSL --retry 3 -o "$TMP/$ASSET"         "$BASE/$ASSET"
curl -fsSL --retry 3 -o "$TMP/$ASSET.sha256"  "$BASE/$ASSET.sha256"

log "verifying SHA256"
( cd "$TMP" && shasum -a 256 -c "$ASSET.sha256" ) \
    || fatal "checksum mismatch — refusing to install"

chmod +x "$TMP/$ASSET"
# Quarantine removal so launchd can exec it without a Gatekeeper prompt
xattr -rd com.apple.quarantine "$TMP/$ASSET" 2>/dev/null || true

# ──────────────────────────────────────────────────────────
# atomic swap + restart
# ──────────────────────────────────────────────────────────
log "stopping bridge"
launchctl unload "/Library/LaunchDaemons/${SERVICE}.plist" 2>/dev/null || true

if [ -x "$INSTALL_DIR/mosaic-bridge" ]; then
    log "keeping old binary as .prev"
    mv -f "$INSTALL_DIR/mosaic-bridge" "$INSTALL_DIR/mosaic-bridge.prev"
fi

log "installing new binary"
install -m 0755 -o mosaic -g staff "$TMP/$ASSET" "$INSTALL_DIR/mosaic-bridge"

log "starting bridge"
launchctl load -w "/Library/LaunchDaemons/${SERVICE}.plist"

# Give it 5s to bind and respond
sleep 5
if curl -fsS --max-time 5 http://127.0.0.1:3500/health > /dev/null; then
    log "health check OK — $TAG is live"
    exit 0
fi

# Failed — automatic rollback
log "health check FAILED — rolling back"
if [ -f "$INSTALL_DIR/mosaic-bridge.prev" ]; then
    launchctl unload "/Library/LaunchDaemons/${SERVICE}.plist" 2>/dev/null || true
    mv -f "$INSTALL_DIR/mosaic-bridge" "$INSTALL_DIR/mosaic-bridge.failed.$(date +%s)"
    mv -f "$INSTALL_DIR/mosaic-bridge.prev" "$INSTALL_DIR/mosaic-bridge"
    launchctl load -w "/Library/LaunchDaemons/${SERVICE}.plist"
    log "rolled back — investigate /usr/local/mosaic-bridge/bridge.err"
    exit 2
fi
fatal "new binary unhealthy and no .prev to fall back to"
