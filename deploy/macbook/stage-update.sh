#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────
# Mosaic Bridge — staging on-device installer
#
# Sibling of update.sh. Installs a candidate binary (already
# scp'd to /tmp/mosaic-bridge-stage by the developer) into
# /usr/local/mosaic-bridge-stage/ for parallel-run soak
# testing alongside the prod bridge.
#
# Differences from update.sh:
#   • Targets /usr/local/mosaic-bridge-stage/ + the
#     com.mosaic.bridge.stage launchd label (not prod's).
#   • No GitHub-release fetch + SHA256 dance — the source of
#     truth for staging is a CI-built workflow artifact the
#     developer downloaded with `gh run download`. The audit
#     trail is the artifact's commit SHA, recorded by CI.
#   • HARD FAIL if .env doesn't have BRIDGE_INSTANCE_NAME=stage
#     AND BRIDGE_SHADOW_MODE=true. The binary itself enforces
#     the same pair (config.validate() refuses to boot a
#     non-shadow stage), but checking here means a misconfig
#     fails before the binary swap rather than producing a
#     restart loop on the new binary. Belt to the binary's
#     suspenders.
#   • Health probe targets :3600 (stage's BRIDGE_PORT).
#
# Usage (run by `make deploy-stage` from the developer's laptop):
#   sudo /usr/local/mosaic-bridge-stage/stage-update.sh
#   sudo /usr/local/mosaic-bridge-stage/stage-update.sh rollback
#
# Source binary path:
#   /tmp/mosaic-bridge-stage   (scp'd in by the caller)
# ──────────────────────────────────────────────────────────
set -euo pipefail

INSTALL_DIR="${INSTALL_DIR:-/usr/local/mosaic-bridge-stage}"
SERVICE="com.mosaic.bridge.stage"
PLIST="/Library/LaunchDaemons/${SERVICE}.plist"
SOURCE_BIN="${SOURCE_BIN:-/tmp/mosaic-bridge-stage}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:3600/health}"

log()  { printf '[stage-update] %s\n' "$*"; }
fatal(){ printf '[stage-update] FATAL: %s\n' "$*" >&2; exit 1; }

require_root() {
    if [ "$(id -u)" -ne 0 ]; then
        fatal "must run as root (launchctl needs it). Try: sudo $0 $*"
    fi
}

# ──────────────────────────────────────────────────────────
# rollback — symmetric with update.sh's rollback path so the
# same `… rollback` muscle memory works on the stage instance.
# ──────────────────────────────────────────────────────────
if [ "${1:-}" = "rollback" ]; then
    require_root "$@"
    [ -f "$INSTALL_DIR/mosaic-bridge.prev" ] || fatal "no .prev binary to roll back to"
    log "rolling back to previous staging binary"
    launchctl unload "$PLIST" 2>/dev/null || true
    mv "$INSTALL_DIR/mosaic-bridge"      "$INSTALL_DIR/mosaic-bridge.failed.$(date +%s)"
    mv "$INSTALL_DIR/mosaic-bridge.prev" "$INSTALL_DIR/mosaic-bridge"
    launchctl load -w "$PLIST"
    log "rollback complete"
    exit 0
fi

require_root "$@"

# ──────────────────────────────────────────────────────────
# preflight: enforce the (instance=stage, shadow=true) invariant
# at the .env level. The binary enforces the same pair on boot,
# but failing here is louder and avoids a restart loop.
# ──────────────────────────────────────────────────────────
ENV_FILE="$INSTALL_DIR/.env"
[ -f "$ENV_FILE" ] || fatal ".env not found at $ENV_FILE — bootstrap not complete"

# grep for the literal lines, ignoring leading whitespace and trailing
# comments. Quoted values are intentionally rejected — the loadDotEnv
# parser strips quotes but accepting them here would let "true" pass
# as the string `"true"` and the operator would never know.
require_env_pair() {
    local key="$1" expected="$2"
    local got
    got=$(awk -F= -v k="$key" '
        /^[[:space:]]*#/ { next }
        $1 ~ ("^[[:space:]]*" k "[[:space:]]*$") {
            sub(/[[:space:]]*#.*$/, "", $2)
            gsub(/^[[:space:]]+|[[:space:]]+$/, "", $2)
            print $2
            exit
        }
    ' "$ENV_FILE")
    if [ -z "$got" ]; then
        fatal "$ENV_FILE missing required staging line: $key=$expected"
    fi
    if [ "$got" != "$expected" ]; then
        fatal "$ENV_FILE has $key=$got, expected $key=$expected (refusing to install — staging invariant violated)"
    fi
}
require_env_pair BRIDGE_INSTANCE_NAME stage
require_env_pair BRIDGE_SHADOW_MODE   true
log "preflight OK: $ENV_FILE has BRIDGE_INSTANCE_NAME=stage and BRIDGE_SHADOW_MODE=true"

# ──────────────────────────────────────────────────────────
# source binary check
# ──────────────────────────────────────────────────────────
[ -f "$SOURCE_BIN" ] || fatal "source binary not found at $SOURCE_BIN — did you run 'make deploy-stage' (which scps to that path)?"
[ -s "$SOURCE_BIN" ] || fatal "source binary at $SOURCE_BIN is empty"

# Optional version sanity-check. The installed binary supports
# `mosaic-bridge -version`; the source binary should too. We do
# not require the version string to differ from what's running
# (re-deploying the same SHA is legitimate during a soak).
NEW_VERSION=$("$SOURCE_BIN" -version 2>/dev/null || true)
if [ -x "$INSTALL_DIR/mosaic-bridge" ]; then
    CURRENT_VERSION=$("$INSTALL_DIR/mosaic-bridge" -version 2>/dev/null || true)
    log "current: ${CURRENT_VERSION:-unknown} → new: ${NEW_VERSION:-unknown}"
fi

# ──────────────────────────────────────────────────────────
# atomic swap + restart, mirroring update.sh
# ──────────────────────────────────────────────────────────
log "stopping stage bridge"
launchctl unload "$PLIST" 2>/dev/null || true

if [ -x "$INSTALL_DIR/mosaic-bridge" ]; then
    log "keeping current binary as .prev"
    mv -f "$INSTALL_DIR/mosaic-bridge" "$INSTALL_DIR/mosaic-bridge.prev"
fi

log "installing new staging binary"
install -m 0755 -o mosaic -g staff "$SOURCE_BIN" "$INSTALL_DIR/mosaic-bridge"
# Quarantine attribute removal — same reason as prod's update.sh:
# launchd would otherwise see a Gatekeeper prompt and the daemon
# never starts.
xattr -rd com.apple.quarantine "$INSTALL_DIR/mosaic-bridge" 2>/dev/null || true

# Drop the source so we don't accidentally re-install a stale
# binary on a subsequent run. Failing to delete is non-fatal —
# the install above is the load-bearing step.
rm -f "$SOURCE_BIN" 2>/dev/null || true

log "starting stage bridge"
launchctl load -w "$PLIST"

# Same 5s post-restart probe as prod. Stage's /health reports
# instance="stage" so an operator inspecting the response can
# tell at a glance which bridge they're talking to.
sleep 5
if curl -fsS --max-time 5 "$HEALTH_URL" > /dev/null; then
    log "health check OK — stage is running the new binary"
    exit 0
fi

# Failed — automatic rollback, same logic as prod's update.sh.
log "health check FAILED — rolling back staging"
if [ -f "$INSTALL_DIR/mosaic-bridge.prev" ]; then
    launchctl unload "$PLIST" 2>/dev/null || true
    mv -f "$INSTALL_DIR/mosaic-bridge"      "$INSTALL_DIR/mosaic-bridge.failed.$(date +%s)"
    mv -f "$INSTALL_DIR/mosaic-bridge.prev" "$INSTALL_DIR/mosaic-bridge"
    launchctl load -w "$PLIST"
    log "rolled back — investigate $INSTALL_DIR/bridge.err"
    exit 2
fi
fatal "new staging binary unhealthy and no .prev to fall back to"
