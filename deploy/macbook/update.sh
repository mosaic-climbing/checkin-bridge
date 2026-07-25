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
#
# SHA256 verification:
#   By default the SHA256 file is fetched from the same GitHub release as
#   the binary. A GitHub-side compromise could swap both in lockstep, so
#   for belt-and-suspenders assurance supply the hash out of band:
#
#     sudo EXPECTED_SHA256=<64-hex> /usr/local/mosaic-bridge/update.sh v0.3.2
#
#   When EXPECTED_SHA256 is set, update.sh does NOT fetch the .sha256 file
#   from the release; it verifies the binary against that value only. The
#   expected hash should be published through a separate channel (release
#   notes commit, staff Slack, email) so a GitHub takeover can't forge it.
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
fatal(){ printf '[update] FATAL: %s\n' "$*" >&2; notify high "Bridge update FAILED" "$*"; exit 1; }

# ── push notifications ────────────────────────────────────
# Reads NTFY_* from the deployed .env (single source of alerting truth,
# shared with the bridge process and healthcheck.sh). Never fails the
# update — alerting is best-effort here.
env_val() {
    [ -r "$INSTALL_DIR/.env" ] || return 0
    awk -F= -v k="$1" '$1==k {sub(/^[^=]*=/,""); gsub(/^"|"$/,""); print; exit}' "$INSTALL_DIR/.env"
}
notify() { # notify <priority> <title> <body>
    local topic; topic="$(env_val NTFY_TOPIC)"
    [ -n "$topic" ] || return 0
    local url token
    url="$(env_val NTFY_URL)"; url="${url:-https://ntfy.sh}"
    token="$(env_val NTFY_TOKEN)"
    local auth=()
    [ -n "$token" ] && auth=(-H "Authorization: Bearer $token")
    curl -fsS --max-time 10 \
        -H "X-Title: $2" -H "X-Priority: $1" -H "X-Tags: package" \
        "${auth[@]}" -d "$3" "${url%/}/$topic" > /dev/null 2>&1 || true
}

# ── .env preflight ────────────────────────────────────────
# Refuse to restart the service on top of a missing or truncated .env.
# The config loader would refuse to boot anyway (validate() requires the
# API keys), but failing preflight keeps the CURRENT binary running
# instead of bouncing the service into a crash loop. A truncated .env is
# also the one path that could silently flip shadow mode — BRIDGE_SHADOW_
# MODE defaults to false (live) in code when the line is absent.
preflight_env() {
    [ -s "$INSTALL_DIR/.env" ] || fatal ".env missing or empty at $INSTALL_DIR/.env — refusing to restart the service"
    local k
    for k in UNIFI_API_TOKEN REDPOINT_API_KEY ADMIN_API_KEY STAFF_PASSWORD; do
        [ -n "$(env_val "$k")" ] || fatal ".env is missing $k — looks truncated; refusing to restart the service"
    done
    # Timestamped backup, keep the 10 most recent. The deployed .env is
    # the only copy of this machine's config; every update snapshots it
    # so an accidental edit is one `cp` away from undone.
    local backup_dir="$INSTALL_DIR/env-backups"
    mkdir -p "$backup_dir"
    chmod 0700 "$backup_dir"
    local backup="$backup_dir/.env.$(date +%Y%m%d-%H%M%S)"
    cp -p "$INSTALL_DIR/.env" "$backup"
    chmod 0600 "$backup"
    ls -t "$backup_dir"/.env.* 2>/dev/null | tail -n +11 | xargs rm -f 2>/dev/null || true
    log ".env preflight OK (backup: $backup)"
}

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
    notify high "Bridge rolled back (manual)" "Operator ran update.sh rollback; previous binary restored."
    exit 0
fi

require_root "$@"
preflight_env

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
curl -fsSL --retry 3 -o "$TMP/$ASSET" "$BASE/$ASSET"

if [ -n "${EXPECTED_SHA256:-}" ]; then
    # Out-of-band hash supplied by the operator. Don't fetch the .sha256
    # from the release — that's the channel we're trying to verify
    # independently. Validate format first so a typo'd env var fails
    # noisily instead of silently skipping verification.
    if ! [[ "$EXPECTED_SHA256" =~ ^[0-9a-fA-F]{64}$ ]]; then
        fatal "EXPECTED_SHA256 must be exactly 64 hex characters"
    fi
    log "verifying SHA256 against EXPECTED_SHA256 (out-of-band)"
    ACTUAL=$(shasum -a 256 "$TMP/$ASSET" | awk '{print $1}')
    EXPECTED_LC=$(printf '%s' "$EXPECTED_SHA256" | tr 'A-F' 'a-f')
    if [ "$ACTUAL" != "$EXPECTED_LC" ]; then
        fatal "checksum mismatch — expected $EXPECTED_LC, got $ACTUAL"
    fi
else
    curl -fsSL --retry 3 -o "$TMP/$ASSET.sha256" "$BASE/$ASSET.sha256"
    log "verifying SHA256 (from release channel — set EXPECTED_SHA256 for out-of-band)"
    ( cd "$TMP" && shasum -a 256 -c "$ASSET.sha256" ) \
        || fatal "checksum mismatch — refusing to install"
fi

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
    notify default "Bridge updated to $TAG" "Deploy verified: /health OK after restart."
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
    notify high "Bridge update to $TAG auto-rolled back" "New binary failed /health after restart; previous binary restored. Check bridge.err."
    exit 2
fi
fatal "new binary unhealthy and no .prev to fall back to"
