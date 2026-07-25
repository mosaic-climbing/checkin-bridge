#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────
# Mosaic Bridge — external health probe with push alerting
#
# Runs every 5 minutes via launchd (com.mosaic.bridge-healthcheck.plist).
# Probes the bridge's /health endpoint from OUTSIDE the process; when the
# probe fails it pushes to the same ntfy topic the bridge itself uses.
#
# This catches the class of failure the in-process watchdog can't:
# a dead, wedged, or not-listening bridge process. (A dead *MacBook*
# is caught one layer further out by the healthchecks.io dead-man —
# this script obviously dies with the machine.)
#
# Alert state machine: one push when the probe starts failing, a
# re-push every RENOTIFY_SECS while it keeps failing, and a recovery
# push when it comes back. State lives in a scratch file so it
# survives across launchd invocations.
#
# Config comes from the bridge's own .env (NTFY_URL / NTFY_TOPIC /
# NTFY_TOKEN) so there is exactly one place alerting is configured.
# ──────────────────────────────────────────────────────────
set -uo pipefail

INSTALL_DIR="${INSTALL_DIR:-/usr/local/mosaic-bridge}"
ENV_FILE="${ENV_FILE:-$INSTALL_DIR/.env}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:3500/health}"
STATE_FILE="${STATE_FILE:-$INSTALL_DIR/.healthcheck-state}"
RENOTIFY_SECS="${RENOTIFY_SECS:-1800}"   # re-alert every 30 min while down

log() { printf '[healthcheck] %s\n' "$*"; logger -t mosaic-healthcheck "$*" 2>/dev/null || true; }

# env_val KEY — extract KEY=value from the .env without sourcing it
# (values may contain characters that would be shell-interpreted).
env_val() {
    [ -r "$ENV_FILE" ] || return 0
    awk -F= -v k="$1" '$1==k {sub(/^[^=]*=/,""); gsub(/^"|"$/,""); print; exit}' "$ENV_FILE"
}

NTFY_URL="$(env_val NTFY_URL)"; NTFY_URL="${NTFY_URL:-https://ntfy.sh}"
NTFY_TOPIC="$(env_val NTFY_TOPIC)"
NTFY_TOKEN="$(env_val NTFY_TOKEN)"

push() { # push <priority> <title> <body>
    [ -n "$NTFY_TOPIC" ] || { log "ntfy not configured; alert not pushed: $2"; return 0; }
    local auth=()
    [ -n "$NTFY_TOKEN" ] && auth=(-H "Authorization: Bearer $NTFY_TOKEN")
    curl -fsS --max-time 10 \
        -H "X-Title: $2" -H "X-Priority: $1" -H "X-Tags: hospital" \
        "${auth[@]}" \
        -d "$3" "${NTFY_URL%/}/$NTFY_TOPIC" > /dev/null \
        || log "ntfy push failed for: $2"
}

now=$(date +%s)
state="ok"; last_alert=0
if [ -r "$STATE_FILE" ]; then
    read -r state last_alert < "$STATE_FILE" 2>/dev/null || { state="ok"; last_alert=0; }
fi

if curl -fsS --max-time 10 "$HEALTH_URL" > /dev/null 2>&1; then
    if [ "$state" = "down" ]; then
        log "bridge recovered"
        push default "Bridge /health recovered" "Probe at $HEALTH_URL is passing again."
    fi
    printf 'ok %s\n' "$now" > "$STATE_FILE"
    exit 0
fi

log "health probe FAILED ($HEALTH_URL)"
if [ "$state" != "down" ] || [ $((now - last_alert)) -ge "$RENOTIFY_SECS" ]; then
    push high "Bridge /health FAILING" "Probe at $HEALTH_URL is not responding. The bridge process is down or wedged. Door access continues via UA-Hub; membership sync and check-in recording are stopped."
    printf 'down %s\n' "$now" > "$STATE_FILE"
else
    printf 'down %s\n' "$last_alert" > "$STATE_FILE"
fi
exit 1
