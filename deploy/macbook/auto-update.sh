#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────
# Mosaic Bridge — auto-updater poller
#
# Polls the GitHub Releases API for the latest tag and, when
# it differs from the running binary's reported version,
# invokes update.sh to perform the actual install.
#
# Designed to be invoked by launchd every 90 seconds via
# /Library/LaunchDaemons/com.mosaic.bridge-updater.plist (40 polls/hour,
# under the 60/hour unauthenticated GitHub API limit).
#
# All the real work — download, SHA256 verify, atomic swap,
# launchctl restart, /health probe, auto-rollback to .prev
# on failure — lives in update.sh. This wrapper just decides
# whether to call it.
#
# Runs as root (update.sh requires root for launchctl).
# Requires: curl, awk.
# ──────────────────────────────────────────────────────────
set -euo pipefail

REPO="${REPO:-mosaic-climbing/checkin-bridge}"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/mosaic-bridge}"
BIN="$INSTALL_DIR/mosaic-bridge"
UPDATE_SH="$INSTALL_DIR/update.sh"
LOCK_DIR="/tmp/mosaic-bridge-updater.lock"

log() { printf '[auto-update] %s %s\n' "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" "$*"; }

# mkdir is atomic and POSIX-portable; macOS doesn't ship flock(1) by default.
# launchd also won't fire a new instance while one is running, so this is
# belt-and-suspenders against a stuck process.
if ! mkdir "$LOCK_DIR" 2>/dev/null; then
    log "another updater run in progress, skipping"
    exit 0
fi
trap 'rmdir "$LOCK_DIR" 2>/dev/null || true' EXIT

[ -x "$UPDATE_SH" ] || { log "ERROR: $UPDATE_SH missing or not executable"; exit 1; }

# Expected -version output: "mosaic-bridge vX.Y.Z" — second field is the tag.
# If the parsed string doesn't match a vX.Y.Z shape, treat current as "unknown"
# (forces an upgrade attempt) and log a warning so a future format change is loud.
version_re='^v[0-9]+\.[0-9]+\.[0-9]+'
current="unknown"
if [ -x "$BIN" ]; then
    raw=$("$BIN" -version 2>/dev/null | awk '{print $2}' || true)
    if [[ "$raw" =~ $version_re ]]; then
        current="$raw"
    elif [ -n "$raw" ]; then
        log "WARN: unexpected -version output ($raw); treating current as unknown"
    fi
fi

latest=$(curl -fsSL --max-time 30 \
    "https://api.github.com/repos/${REPO}/releases/latest" \
    | awk -F'"' '/"tag_name":/ {print $4; exit}' || true)

if [ -z "$latest" ]; then
    log "ERROR: failed to fetch latest release tag from GitHub API"
    exit 1
fi

if ! [[ "$latest" =~ $version_re ]]; then
    log "ERROR: GitHub returned unexpected tag_name ($latest); refusing to install"
    exit 1
fi

if [ "$current" = "$latest" ]; then
    # Quiet no-op — don't log every tick when there's nothing to do.
    exit 0
fi

log "update available: current=$current latest=$latest"

if "$UPDATE_SH" "$latest"; then
    log "update to $latest succeeded"
    exit 0
else
    rc=$?
    log "ERROR: update to $latest FAILED (exit $rc) — update.sh's auto-rollback should have restored .prev"
    exit "$rc"
fi
