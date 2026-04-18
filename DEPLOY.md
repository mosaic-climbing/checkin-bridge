# Mosaic Check-in Bridge — M1 MacBook Pro Deploy Runbook

This document walks through deploying the Mosaic Climbing check-in bridge on an M1 MacBook Pro at the gym in shadow mode, verifying the install, enrolling members, and flipping to live once you're confident.

**Shadow mode** means the bridge listens to real NFC taps, evaluates every decision, and logs what it would have done — but never unlocks a door, never writes to Redpoint, and never mutates UniFi user status. You can leave it running indefinitely alongside the UA-Hub's native access control with zero side effects.

---

> ⚠ **Before you do anything else: rotate the secrets.** The `.env.shadow` committed to this repo historically contained real `UNIFI_API_TOKEN` and `REDPOINT_API_KEY` values. Assume both are exposed and must be rotated:
> - **UniFi:** UniFi Access → Settings → Developer API → revoke the old token, issue a new one.
> - **Redpoint:** generate a new API key in the Redpoint admin console, then revoke the old one.
> - **Update** `.env` on the MacBook with the new values and `sudo launchctl kickstart -k system/com.mosaic.bridge`.
>
> The repo now ships `.env.shadow.example` with placeholders only. Your working `.env` should live only on the MacBook (mode 0600, owned by the `mosaic` user) and never be committed.

## 0. Prerequisites

Before starting, confirm you have:

- An M1 MacBook Pro on the gym LAN (Ethernet recommended — see section 1), plugged in and logged in.
- The UDM Pro's LAN IP and a confirmed ability to reach its UA-Hub API port (`12445/TCP`) from the MacBook.
- The bridge binary `mosaic-bridge-darwin-arm64` and config `.env.shadow` from the project.
- Admin access on the UDM to open the UA-Hub port if needed.
- A UniFi API token (already present in `.env.shadow` as `UNIFI_API_TOKEN`).
- A Redpoint API key and Gate ID (already present in `.env.shadow`).

A generated strong secret each for:
- `ADMIN_API_KEY` — the secret protecting the bridge's own admin API.
- `STAFF_PASSWORD` — the password for the staff dashboard login.

Generate them ahead of time:

```
openssl rand -hex 32   # for ADMIN_API_KEY
openssl rand -hex 16   # for STAFF_PASSWORD (or pick something memorable)
```

Store both in a password manager — you'll paste them into the `.env` in step 3.

---

## 1. Harden the MacBook Pro for unattended operation

This box will live at the gym with the lid closed, plugged in, running the bridge 24/7. A MacBook has different sleep/power behavior than a desktop, so there are a few extra steps.

### 1a. Networking — Ethernet via USB-C dongle

Wi-Fi is not reliable enough for a service that unlocks doors — a dropped association means taps fail until it reconnects. Use a USB-C Ethernet adapter plugged into the same switch/VLAN as the UDM. This gives you a stable MAC address for the DHCP reservation and consistent sub-millisecond latency to the UA-Hub.

Leave Wi-Fi on as a fallback for remote management, but the bridge's `UNIFI_HOST` should resolve to the Ethernet adapter's IP path, not Wi-Fi.

### 1b. System Settings

Open **System Settings** and configure:

- **General → Software Update** — turn off automatic macOS major version upgrades. Keep security patches on. You don't want an uncontrolled Sonoma→Sequoia upgrade at 2am.
- **Battery → Options** — enable "Prevent automatic sleeping when the display is off." This is the MacBook equivalent of the Mac mini's "Prevent automatic sleeping on power adapter" and is critical — without it, the laptop sleeps after a few minutes with the lid closed and the bridge goes offline.
- **Battery → Options** — enable "Wake for network access" so SSH stays reachable even if the display sleeps.
- **Lock Screen** — set "Require password after screen saver begins or display is turned off" to a long interval or never. This prevents the login screen from blocking launchd services after a wake.
- **Users & Groups** → choose an "Automatic login" account — pick a dedicated non-admin user if possible, otherwise the install account.
- **General → Login Items & Extensions** — enable "Reopen windows when logging back in."
- **Privacy & Security** → ensure **FileVault is off**, OR record the recovery key somewhere you'll have it at 3am. FileVault + reboot = bridge never starts until someone unlocks the disk.
- **Network** — note the current IP (use the Ethernet adapter's IP, not Wi-Fi), then on the UDM create a **DHCP reservation** pinning that MAC address to that IP.

### 1c. Lid-closed (clamshell) operation

The MacBook must keep running with the lid closed. macOS supports clamshell mode automatically *if* an external display is connected, but you probably don't want a monitor attached to a gym server box. To run lid-closed without an external display, you need to prevent sleep via `pmset`:

```
# Prevent system sleep entirely when on AC power
sudo pmset -c sleep 0
sudo pmset -c disksleep 0
sudo pmset -c displaysleep 0

# Disable hibernation (writes RAM to disk on sleep — not useful for a server)
sudo pmset -c hibernatemode 0
sudo pmset -c standby 0
sudo pmset -c autopoweroff 0

# Keep network interfaces alive during display sleep
sudo pmset -c womp 1
sudo pmset -c tcpkeepalive 1
```

Verify with `pmset -g` — you want `sleep 0`, `disksleep 0`, `hibernatemode 0`.

**Important:** these settings apply to the `-c` (charger/AC) power profile. If someone unplugs the MacBook, the battery profile takes over and the laptop *will* eventually sleep. Keep it plugged in.

### 1d. Power failure behavior

Unlike a Mac mini, a MacBook doesn't have a "start up automatically after a power failure" setting — but it has something better: a **battery**. Short power outages (UPS-style) are handled transparently; the laptop stays on the whole time. After a prolonged outage that drains the battery to 0%, the MacBook will power back on automatically when AC power is restored *if it was running when power was lost* (this is default M1 behavior). It will boot to the login screen, and with automatic login enabled, launchd picks up the bridge daemon.

### 1e. SSH access

Enable SSH so you can manage from your laptop: **System Settings → General → Sharing → Remote Login**. Restrict it to a single admin user.

---

## 2. Install the bridge files

From your laptop (where the project repo lives), copy the binary, env, and create a working directory on the MacBook. We'll run the bridge as a dedicated non-root user (`mosaic`) — this limits blast radius if anything in the service is ever compromised, and stops a stray bug from writing to arbitrary paths.

```
GYM=mosaic-gym.local    # or the DHCP-reserved IP of the MacBook's Ethernet adapter

# One-time: create a dedicated service user on the MacBook
ssh $GYM '
  if ! id mosaic >/dev/null 2>&1; then
    sudo dscl . -create /Users/mosaic
    sudo dscl . -create /Users/mosaic UserShell /usr/bin/false
    sudo dscl . -create /Users/mosaic RealName "Mosaic Bridge"
    sudo dscl . -create /Users/mosaic UniqueID "701"
    sudo dscl . -create /Users/mosaic PrimaryGroupID 20
    sudo dscl . -create /Users/mosaic NFSHomeDirectory /var/empty
  fi
'

# Create install dir + copy files
ssh $GYM "sudo mkdir -p /usr/local/mosaic-bridge/data && sudo chown -R \$USER /usr/local/mosaic-bridge"
scp mosaic-bridge-darwin-arm64 $GYM:/usr/local/mosaic-bridge/mosaic-bridge
scp .env.shadow                $GYM:/usr/local/mosaic-bridge/.env

# Lock it down, strip Gatekeeper quarantine, hand ownership to mosaic:
ssh $GYM '
  sudo chmod +x /usr/local/mosaic-bridge/mosaic-bridge
  # Remove the "downloaded from the internet" quarantine flag, or launchd
  # will refuse to exec the binary without a GUI prompt.
  sudo xattr -rd com.apple.quarantine /usr/local/mosaic-bridge/mosaic-bridge || true
  # Secrets file — nobody but the service user reads this.
  sudo chmod 600 /usr/local/mosaic-bridge/.env
  # Data dir — SQLite store and any future runtime state.
  sudo chmod 700 /usr/local/mosaic-bridge/data
  # Hand everything to the service user.
  sudo chown -R mosaic:staff /usr/local/mosaic-bridge
'
```

Layout on the MacBook:

```
/usr/local/mosaic-bridge/          (owned by mosaic:staff)
├── mosaic-bridge                  # the Go binary (755)
├── .env                           # config, secrets live here (600, mosaic:staff)
├── data/                          # SQLite store + runtime state (700, mosaic:staff)
├── bridge.log                     # stdout (populated once launchd starts the daemon)
└── bridge.err                     # stderr (populated once launchd starts the daemon)
```

**Why these modes matter:** `.env` holds the UniFi API token, Redpoint API key, `ADMIN_API_KEY`, and `STAFF_PASSWORD` in plaintext. Mode `600` means only the `mosaic` user can read it — scp defaults to `644`, which is world-readable on a shared box. `data/` gets `700` for the same reason: the SQLite store holds every member's name, Redpoint ID, and NFC UID.

---

## 3. Configure the bridge

SSH to the MacBook and edit `/usr/local/mosaic-bridge/.env`. The three changes vs. the committed `.env.shadow`:

```
# Point at the UDM over LAN (not localhost — the UDM is a different box now)
UNIFI_HOST=192.168.1.1              # replace with your UDM's LAN IP

# Persist data under the mini's install directory
DATA_DIR=/usr/local/mosaic-bridge/data

# Paste the secrets you generated in section 0
ADMIN_API_KEY=<paste openssl output>
STAFF_PASSWORD=<paste openssl output>
```

Leave everything else as-is. In particular:

- `BRIDGE_SHADOW_MODE=true` — keep this on until section 8.
- `BIND_ADDR=127.0.0.1` — keeps the dashboard on loopback for now. We'll revisit in section 6.
- `REDPOINT_GATE_ID` — leave populated. Shadow mode skips the Redpoint write anyway; keeping the gate ID lets the log show "SHADOW: would record check-in (gateId=…)" so you can verify the data is flowing correctly.

---

## 4. Open the UA-Hub port from the mini (if needed)

The bridge speaks to the UA-Hub REST API at `https://$UNIFI_HOST:12445` and its WebSocket at `wss://$UNIFI_HOST:12445/...`. From the MacBook, test:

```
curl -kI https://192.168.1.1:12445/api/v1/developer/doors
```

Expect `HTTP/2 401` or `HTTP/2 200` — anything that completes the TLS handshake means the port is reachable. If it hangs or refuses, open port `12445/TCP` on the UDM from the MacBook's Ethernet IP and try again.

---

## 5. Create the launch daemon

Write `/Library/LaunchDaemons/com.mosaic.bridge.plist` on the MacBook:

```
sudo tee /Library/LaunchDaemons/com.mosaic.bridge.plist > /dev/null <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.mosaic.bridge</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/mosaic-bridge/mosaic-bridge</string>
  </array>
  <key>UserName</key><string>mosaic</string>
  <key>GroupName</key><string>staff</string>
  <key>WorkingDirectory</key><string>/usr/local/mosaic-bridge</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/usr/local/mosaic-bridge/bridge.log</string>
  <key>StandardErrorPath</key><string>/usr/local/mosaic-bridge/bridge.err</string>
  <key>ThrottleInterval</key><integer>5</integer>
  <key>ProcessType</key><string>Interactive</string>
  <key>SoftResourceLimits</key>
  <dict>
    <key>NumberOfFiles</key><integer>4096</integer>
  </dict>
</dict>
</plist>
EOF

sudo chown root:wheel /Library/LaunchDaemons/com.mosaic.bridge.plist
sudo chmod 644        /Library/LaunchDaemons/com.mosaic.bridge.plist
```

Notes:

- `UserName=mosaic` / `GroupName=staff` drops privileges before launch — the daemon never runs as root even though launchd loads it from `/Library/LaunchDaemons`. Combined with the `600`/`700` file modes from section 2, only this one user can read the secrets or the SQLite store.
- `KeepAlive=true` restarts the process if it crashes. `ThrottleInterval=5` prevents tight crash loops — 5s minimum between restart attempts.
- `ProcessType=Interactive` tells launchd this is a latency-sensitive service (bias scheduler toward it), which matters because tap-to-unlock has a real-time SLO.
- `SoftResourceLimits.NumberOfFiles=4096` — the bridge holds one long-lived WebSocket and a pool of sqlite file descriptors; default macOS ulimits are low and can bite you during ingest runs.
- `WorkingDirectory` is critical — the bridge loads `.env` from the CWD.
- `StandardOutPath` / `StandardErrorPath` tail your logs in plain text. Rotation is set up automatically in section 10.

Because the daemon now runs as `mosaic`, make sure the log files themselves are writable by that user before first launch:

```
sudo touch /usr/local/mosaic-bridge/bridge.log /usr/local/mosaic-bridge/bridge.err
sudo chown mosaic:staff /usr/local/mosaic-bridge/bridge.log /usr/local/mosaic-bridge/bridge.err
```

Load it:

```
sudo launchctl load -w /Library/LaunchDaemons/com.mosaic.bridge.plist
```

Verify it started:

```
sudo launchctl list | grep mosaic    # should show a PID, not "-"
tail -f /usr/local/mosaic-bridge/bridge.log
```

You want to see:

1. A multi-line `SHADOW MODE ENABLED` warning banner.
2. `http server listening on http://127.0.0.1:3500`.
3. `UniFi WebSocket connected`.
4. If not first-run: `running migration N` lines for any pending schema migrations.

If the bridge exits immediately, check `bridge.err`. The most common failure is a missing or incorrect `.env` variable — the log usually names the field.

---

## 6. Open the dashboard

Until you've tested the dashboard works, leave `BIND_ADDR=127.0.0.1` and tunnel to it from your laptop:

```
ssh -L 3500:127.0.0.1:3500 $GYM
# then in a browser: http://localhost:3500/ui
```

Log in with `STAFF_PASSWORD`. You should see:

- The **Dashboard** page with live stats, the Recent Check-ins feed, and the new **Shadow Decisions** panel.
- In the sidebar: Members, Check-ins, Sync & Jobs, Door Policies, Metrics.

Once the dashboard works over the tunnel, you can optionally move it to the LAN so staff can reach it from inside the gym without SSH:

```
# In /usr/local/mosaic-bridge/.env:
BIND_ADDR=0.0.0.0
ALLOWED_NETWORKS=192.168.1.0/24    # your staff subnet

# Reload:
sudo launchctl unload /Library/LaunchDaemons/com.mosaic.bridge.plist
sudo launchctl load -w /Library/LaunchDaemons/com.mosaic.bridge.plist
```

Then the dashboard lives at `http://<macbook-lan-ip>:3500/ui` from any gym staff laptop.

---

## 7. Enroll members

Three paths, in order of how you'll actually use them.

### 7a. Bulk ingest from UniFi (do this first)

If UniFi already has NFC cards enrolled for most of your members, let the bridge match them against your Redpoint directory automatically.

1. **Sync & Jobs** → **Directory Sync → Run Now.** Pulls the current Redpoint customer list into the local cache so matching has fresh data. Takes ~1–5 minutes depending on customer count.
2. **Sync & Jobs** → **UniFi Ingest → Dry Run.** Fetches every UniFi user with an NFC credential and tries to match each to a Redpoint customer by email first, then by name. Returns a JSON summary in the result panel showing `matched` / `unmatched` / `skipped` counts.
3. **Sync & Jobs** → **Unmatched UniFi Users → Scan UniFi.** Lists every UniFi account the ingest couldn't resolve, tagged **No match** (no Redpoint customer found) or **Ambiguous** (multiple Redpoint customers match, can't tell which is right). Each row has a **Search Redpoint →** button.
4. For each unmatched row: click **Search Redpoint →** to jump to the Members page with their name/email prefilled. Either:
   - fix the record in Redpoint (usually a missing email or a name spelling difference) and re-run ingest, or
   - use the **Add Member Manually** form to enroll the mapping directly — paste the Redpoint customer ID from the search hit and the NFC token from the unmatched row.
5. When the unmatched list is clean, run the ingest once more — this time with `dry_run=false`:
   ```
   curl -X POST -H "X-API-Key: $ADMIN_API_KEY" \
     "http://localhost:3500/ingest/unifi?dry_run=false"
   ```
   This commits all matched mappings into the local store.

### 7b. One-off enrollment via the dashboard

**Members** tab → **Search Directory** (type name or email) → click **Select** on the right customer → paste NFC tag UID into the **Add Member Manually** form → **Add Member**.

To find an NFC tag UID: have the person tap their card once on any reader while the bridge is running, then on the MacBook:

```
grep "NFC tap" /usr/local/mosaic-bridge/bridge.log | tail -1
```

The `credential` field in that log line is the tag UID.

### 7c. Scripted / batch enrollment via API

```
curl -X POST http://localhost:3500/members \
  -H "X-API-Key: $ADMIN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "redpointId": "cust_abc123",
    "nfcUid": "04AABB1122",
    "firstName": "Alex",
    "lastName": "Smith"
  }'
```

Only `redpointId` and `nfcUid` are required; names auto-populate from the directory if omitted. UID is upper-cased and trimmed server-side.

---

## 8. Shadow-mode burn-in checklist

Leave the bridge running in shadow mode for at least several days of real traffic before flipping to live. The goal is to drive disagreements between the bridge and UniFi's native decision to zero — any non-zero number is a bug waiting to lock out a member or admit a non-member once you flip.

### 8a. Dashboard counters

- **Shadow Decisions** panel → `Disagree` counter should be **0** (or every disagreement should have a legitimate, understood explanation).
- **Would miss** (UniFi said ACCESS, bridge said denied) = paying members the bridge would lock out. Must be zero before flipping.
- **Would admit** (UniFi said BLOCKED, bridge said allowed) = taps UniFi rejected that the bridge would let through. Must be zero — or if non-zero, you need to understand why UniFi blocked them.

Watch the counters across at least one full weekly cycle of traffic (weekday peak, weekend peak, early morning, late evening) so you see every traffic shape real members produce.

### 8b. Tail the shadow log

```
tail -f /usr/local/mosaic-bridge/bridge.log | \
  grep -E "NFC tap|SHADOW|DENIED|CHECK-IN"
```

Confirm you see `SHADOW: would unlock door` for every successful tap and `SHADOW: would record check-in in Redpoint` with the correct gate ID. Confirm the UA-Hub's own system log (in the UniFi Access UI) shows the real unlocks happening on the same taps.

### 8c. Spot-check denials

Have a known expired/frozen member tap. You should see `DENIED: member not active` (or similar reason) in the log and a corresponding row in the bridge's **Recent Check-ins** marked red. UniFi should also reject the same tap natively.

### 8d. Test the recheck flow

Have someone whose Redpoint membership was renewed since the last 24h sync tap their card. The bridge should deny on the first pass, then do a live recheck against Redpoint, find the renewal, update the cache, and log `DENIED→ALLOWED: member renewed, recheck passed!`. Shadow mode still logs "would unlock" — no actual door movement.

### 8e. Timing: set the nightly sync for a real quiet period

Directory Sync defaults to run every 24h. On the UDM Pro this was 3am; on a MacBook you want it to run when the gym is closed and nobody is tapping — running directory sync while the door has active traffic is fine (it's decoupled), but the live-recheck storm that happens after a fresh sync is cleaner if nothing else is happening. If the gym opens at 6am, running the sync at 4–4:30am is safer than 3am on the off-chance someone is in there cleaning.

Check: `grep "directory sync" /usr/local/mosaic-bridge/bridge.log | tail -3` should show entries within the last 24h, at the expected hour in **your** timezone (macOS runs in the system TZ by default — verify with `date`).

### 8f. Handle staff, contractors, guests

The auto-ingest matches UniFi users to Redpoint customers by email then name. Anybody in UniFi without a Redpoint record — employees, trainers, contractors, trial passes — will land on the **Unmatched UniFi Users** list. Decide up front how to treat them:

- **Option A** (simplest): create a corresponding customer record in Redpoint so they get matched by the next ingest.
- **Option B**: add them manually through the dashboard's **Add Member Manually** form with a synthetic Redpoint ID like `staff_jane_smith`, and author a door policy that treats the `staff_` prefix as always-allowed.

Do **not** flip to live until the Unmatched list is empty — whatever UniFi lets in today, the bridge will lock out tomorrow if the person isn't in the bridge's store.

### 8g. Redpoint unreachable — verify fail-closed

The bridge defaults to fail-closed (deny) if Redpoint can't be reached during a live recheck. To verify: while the bridge is in shadow mode, temporarily block outbound to `lefclimbing.rphq.com` on your dev machine (or on the MacBook itself, briefly) and have a known-denied member tap. The log should show:

```
denied-tap recheck failed  error="...context deadline exceeded..." ...
SHADOW: would deny door  reason="..."
```

What you must **not** see is any line that says "recheck passed" without a clean GraphQL response — that would mean the bridge is interpreting a network error as a permissive decision. Once confirmed, restore connectivity.

### 8h. WebSocket silent-failure check

A known pitfall: the UniFi WebSocket can stay "connected" but stop delivering tap events (e.g., after a UDM firmware update). The bridge logs `UniFi WebSocket connected` once at startup and doesn't re-emit unless it reconnects. During burn-in, compare tap counts:

```
# Taps seen by the bridge in the last hour
grep "NFC tap" /usr/local/mosaic-bridge/bridge.log \
  | awk -v t="$(date -v-1H +%Y-%m-%dT%H)" '$1 >= t' | wc -l
```

Compare against UA-Hub's own "recent events" page for the same hour. If the bridge count is 0 but UA-Hub has events, the WebSocket is stale — restart the bridge (`sudo launchctl kickstart -k system/com.mosaic.bridge`) and file it as a known risk to revisit.

---

## 9. Flip to live

When the Disagree counter has been zero for several days of real traffic and you've spot-checked everything in section 8:

1. **On the MacBook:**
   ```
   # Edit /usr/local/mosaic-bridge/.env
   BRIDGE_SHADOW_MODE=false
   ```
2. **Reload:**
   ```
   sudo launchctl unload /Library/LaunchDaemons/com.mosaic.bridge.plist
   sudo launchctl load -w /Library/LaunchDaemons/com.mosaic.bridge.plist
   ```
3. **Verify:**
   ```
   tail -f /usr/local/mosaic-bridge/bridge.log
   ```
   The `SHADOW MODE ENABLED` banner should be gone. Tap a card — you should see `CHECK-IN SUCCESS` followed by `Redpoint check-in recorded async` instead of the `SHADOW: would …` lines.
4. **Watch for one shift.** Keep eyes on the log (or sit on the dashboard) through at least one busy period, ready to flip back if anything looks wrong.

**Rollback** is trivial at any point:

```
# Revert .env to BRIDGE_SHADOW_MODE=true and reload
sudo launchctl unload /Library/LaunchDaemons/com.mosaic.bridge.plist
sudo launchctl load -w /Library/LaunchDaemons/com.mosaic.bridge.plist
```

Or stop the bridge entirely — the UA-Hub's native access control has been running the whole time and keeps working without the bridge.

---

## 10. Operations reference

### Service control

```
# Status
sudo launchctl list | grep mosaic

# Stop (will NOT auto-restart)
sudo launchctl unload /Library/LaunchDaemons/com.mosaic.bridge.plist

# Start / reload after .env change
sudo launchctl load -w /Library/LaunchDaemons/com.mosaic.bridge.plist

# One-shot restart
sudo launchctl kickstart -k system/com.mosaic.bridge
```

### Logs

```
# Live tail
tail -f /usr/local/mosaic-bridge/bridge.log

# Errors only
tail -f /usr/local/mosaic-bridge/bridge.err

# Filter for specific events
grep -E "NFC tap|CHECK-IN|DENIED|SHADOW" /usr/local/mosaic-bridge/bridge.log

# Last hour
tail -10000 /usr/local/mosaic-bridge/bridge.log | grep "$(date -v-1H +%Y-%m-%dT%H)"
```

### Automated log rotation

macOS has a built-in rotator (`newsyslog`) that ships with the OS — set it up once and forget. Add a snippet:

```
sudo tee /etc/newsyslog.d/com.mosaic.bridge.conf > /dev/null <<'EOF'
# logfilename                               [owner:group]    mode count size when  flags [/pid_file] [sig_num]
/usr/local/mosaic-bridge/bridge.log         mosaic:staff     644  14    10240 *    JN
/usr/local/mosaic-bridge/bridge.err         mosaic:staff     644  14    10240 *    JN
EOF
```

That keeps 14 rotated files, rotates at 10 MB, gzips the old ones (`J`), and does not send a signal (`N`) — the bridge's logger reopens on the next write. Test manually:

```
sudo newsyslog -nv    # dry run, shows what would happen
sudo newsyslog -v     # actually rotate
```

### Store access

The SQLite store at `/usr/local/mosaic-bridge/data/bridge.db` is queryable directly:

```
sqlite3 /usr/local/mosaic-bridge/data/bridge.db "
  SELECT timestamp, customer_name, result, unifi_result, deny_reason
  FROM checkins
  ORDER BY id DESC LIMIT 20;
"
```

### Upgrading the bridge

When there's a new binary to deploy:

```
scp mosaic-bridge-darwin-arm64 $GYM:/tmp/mosaic-bridge.new
ssh $GYM '
  sudo mv /tmp/mosaic-bridge.new /usr/local/mosaic-bridge/mosaic-bridge.new
  sudo chmod +x /usr/local/mosaic-bridge/mosaic-bridge.new
  sudo launchctl unload /Library/LaunchDaemons/com.mosaic.bridge.plist
  sudo mv /usr/local/mosaic-bridge/mosaic-bridge /usr/local/mosaic-bridge/mosaic-bridge.prev
  sudo mv /usr/local/mosaic-bridge/mosaic-bridge.new /usr/local/mosaic-bridge/mosaic-bridge
  sudo launchctl load -w /Library/LaunchDaemons/com.mosaic.bridge.plist
  tail -f /usr/local/mosaic-bridge/bridge.log
'
```

Keep `.prev` for fast rollback:

```
sudo launchctl unload /Library/LaunchDaemons/com.mosaic.bridge.plist
sudo mv /usr/local/mosaic-bridge/mosaic-bridge.prev /usr/local/mosaic-bridge/mosaic-bridge
sudo launchctl load -w /Library/LaunchDaemons/com.mosaic.bridge.plist
```

### Automated database backup

The store is small (tens of MB at most for a gym-sized deployment). SQLite's `.backup` is safe while the bridge is running — it's a hot backup that copies the full database without blocking writes. Automate it via launchd so you don't forget:

```
# 1. Write the backup script
sudo tee /usr/local/mosaic-bridge/backup.sh > /dev/null <<'EOF'
#!/bin/bash
set -euo pipefail
BACKUP_DIR=/usr/local/mosaic-bridge/backups
mkdir -p "$BACKUP_DIR"
TS=$(date +%Y%m%d-%H%M)
/usr/bin/sqlite3 /usr/local/mosaic-bridge/data/bridge.db ".backup '$BACKUP_DIR/bridge-$TS.db'"
# Keep 30 days, delete anything older
find "$BACKUP_DIR" -name 'bridge-*.db' -mtime +30 -delete
EOF
sudo chmod +x /usr/local/mosaic-bridge/backup.sh
sudo chown mosaic:staff /usr/local/mosaic-bridge/backup.sh

# 2. Schedule it nightly at 3:30am via launchd
sudo tee /Library/LaunchDaemons/com.mosaic.bridge-backup.plist > /dev/null <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.mosaic.bridge-backup</string>
  <key>ProgramArguments</key>
  <array><string>/usr/local/mosaic-bridge/backup.sh</string></array>
  <key>UserName</key><string>mosaic</string>
  <key>StartCalendarInterval</key>
  <dict>
    <key>Hour</key><integer>3</integer>
    <key>Minute</key><integer>30</integer>
  </dict>
  <key>StandardErrorPath</key><string>/usr/local/mosaic-bridge/backup.err</string>
</dict>
</plist>
EOF
sudo chown root:wheel /Library/LaunchDaemons/com.mosaic.bridge-backup.plist
sudo chmod 644        /Library/LaunchDaemons/com.mosaic.bridge-backup.plist
sudo launchctl load -w /Library/LaunchDaemons/com.mosaic.bridge-backup.plist
```

For offsite copy, add a follow-up step that `rsync`s `backups/` to a NAS, to iCloud Drive, or to an S3 bucket — whichever you already use for gym data.

---

### Minimal health monitoring

With only one box, the failure mode you care about most is "the service stopped and nobody noticed." A plain cron-like health check is enough for now — you can graduate to a real alerting tool later.

```
# Write an alerter that hits /health and nags if it's down
sudo tee /usr/local/mosaic-bridge/healthcheck.sh > /dev/null <<'EOF'
#!/bin/bash
# Fire a notification channel (email, Pushover, Slack webhook, etc.) if the bridge
# isn't answering /health. Runs from launchd every 5 minutes.
set -u
URL="http://127.0.0.1:3500/health"
if ! curl -fsS --max-time 5 "$URL" > /dev/null; then
  # Replace this with whatever channel you actually check:
  logger -t mosaic-bridge "HEALTH FAIL: bridge not responding on $URL"
  # Example Pushover:
  # curl -fsS --data-urlencode "token=$PUSHOVER_TOKEN" \
  #          --data-urlencode "user=$PUSHOVER_USER" \
  #          --data-urlencode "message=Mosaic bridge DOWN" \
  #          https://api.pushover.net/1/messages.json
fi
EOF
sudo chmod +x /usr/local/mosaic-bridge/healthcheck.sh
sudo chown mosaic:staff /usr/local/mosaic-bridge/healthcheck.sh

sudo tee /Library/LaunchDaemons/com.mosaic.bridge-health.plist > /dev/null <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.mosaic.bridge-health</string>
  <key>ProgramArguments</key>
  <array><string>/usr/local/mosaic-bridge/healthcheck.sh</string></array>
  <key>UserName</key><string>mosaic</string>
  <key>StartInterval</key><integer>300</integer>
  <key>StandardErrorPath</key><string>/usr/local/mosaic-bridge/health.err</string>
</dict>
</plist>
EOF
sudo chown root:wheel /Library/LaunchDaemons/com.mosaic.bridge-health.plist
sudo chmod 644        /Library/LaunchDaemons/com.mosaic.bridge-health.plist
sudo launchctl load -w /Library/LaunchDaemons/com.mosaic.bridge-health.plist
```

If you already scrape Prometheus metrics somewhere, the bridge exposes `/metrics` on the same port — point your existing alerting at `http://<macbook>:3500/metrics` and add a rule on `up == 0` or a sentinel metric like `bridge_websocket_connected`.

## 11. Troubleshooting

**Bridge won't start, nothing in `bridge.log`.** Check `bridge.err` — usually a missing `.env` variable or a bad SQLite path. Confirm `WorkingDirectory` in the plist matches where `.env` lives.

**Dashboard returns 401 / login loops.** `STAFF_PASSWORD` in the `.env` is what the login form expects. Make sure there are no trailing spaces or quotes in the env file.

**WebSocket disconnects constantly.** Check UDM firewall rules — the bridge needs persistent outbound `12445/TCP` to the UDM. Also check UA-Hub API token hasn't expired or been rotated.

**Every tap logs `DENIED: not in store`.** Run a **Directory Sync** followed by **UniFi Ingest** from the Sync page. Your local store is empty.

**Shadow Decisions shows many disagreements.** Don't flip to live yet — investigate. Common causes: stale Redpoint directory cache (sync it), UniFi has users with NFC tags who aren't Redpoint customers (staff, contractors — you may want to add them manually or configure a special policy), door policies on the bridge side don't match UniFi's door groups.

**MacBook reboots and the bridge doesn't come back.** Check that FileVault is off or unlocked, that auto-login is enabled, that `RunAtLoad=true` is in the plist, and that `/Library/LaunchDaemons/com.mosaic.bridge.plist` is owned by `root:wheel` with mode `644`.

**Bridge goes offline after a while with the lid closed.** The MacBook is sleeping. Verify `pmset -g` shows `sleep 0` and `hibernatemode 0`. Re-run the `pmset` commands from section 1c. Also confirm the MacBook is plugged in — the `-c` (charger) power profile only applies on AC power; on battery, the laptop will eventually sleep regardless.

**Bridge unreachable after an Ethernet adapter is unplugged/replugged.** macOS sometimes reassigns the network interface name when a USB-C adapter is reconnected. Check `ifconfig` and confirm the Ethernet adapter has the expected IP. If it picked up a new DHCP lease, update the reservation on the UDM.

---

## 12. CI + remote deploy

Everything below is set up so you can fix issues from your laptop without touching the MacBook directly. The flow is: push code → CI builds + tests → tag a release → `make deploy` (or run `update.sh` on the box) pulls the new binary and swaps it in.

### 12a. Before your first `git push`

The repo has contained real tokens in its history on your local disk. Before any push to GitHub:

1. **Run the preflight scanner:**
   ```
   ./scripts/check-secrets.sh
   ```
   It will fail loudly if any `.env` file is staged, any tracked file contains a known-leaked token, or any template contains a non-placeholder secret. Fix anything it finds before pushing.
2. **Install the pre-push hook** so you can't accidentally push secrets later:
   ```
   make install-hooks
   ```
3. **Rotate the UniFi and Redpoint tokens** (see the warning banner at the top of this doc) — assume the historical values are already compromised.
4. **Add a GitHub repo secret** for the release workflow if you want Pushover/Slack alerts on failed deploys — not required for CI itself (no secrets needed to build the binary).
5. **Confirm `.gitignore` covers your tree:**
   ```
   git status --ignored | grep -E '(\.env|\.db|mosaic-bridge)'
   ```
   Every `.env`, `*.db`, and `mosaic-bridge-*` should be listed as ignored, not tracked.

### 12b. CI — what runs on every push

`.github/workflows/ci.yml` runs three jobs in order:

- **secret-scan** — executes `scripts/check-secrets.sh --ci` and also runs the public `gitleaks` scanner as a second opinion. This job must pass before any build starts, and it's enforced on every commit to `main` and every PR.
- **test** — `go vet`, `staticcheck`, `go mod tidy` verification, and `go test -race ./...` with coverage upload.
- **build** — cross-compiles `darwin-arm64`, `linux-arm64`, and `linux-amd64` with `-trimpath` and embedded version info, then uploads each as a CI artifact with a SHA256 sidecar. Artifacts live 30 days — useful for "show me the binary from PR #34".

### 12c. Releases — cutting a new version

`.github/workflows/release.yml` triggers on any tag push matching `v*` (or manually from the Actions tab). It:

1. Re-runs the secret scan.
2. Re-runs the tests (gate).
3. Cross-compiles all three targets with `-X main.version=<tag>` so `mosaic-bridge -version` prints something meaningful.
4. Publishes a GitHub Release with the three binaries, their individual `.sha256` files, a consolidated `SHA256SUMS`, and auto-generated release notes.

To cut a release from your laptop:

```
make release-tag VERSION=v0.3.2    # runs check-secrets + tests, then tags + pushes
```

Or manually:

```
git tag -a v0.3.2 -m "Release v0.3.2"
git push origin v0.3.2
```

The release workflow does the rest.

### 12d. Remote deploy — `update.sh` on the MacBook

`deploy/macbook/update.sh` is the on-box updater. One-time install on the MacBook:

```
ssh $GYM '
  sudo mkdir -p /usr/local/mosaic-bridge
  sudo curl -fsSL -o /usr/local/mosaic-bridge/update.sh \
    https://raw.githubusercontent.com/mosaic-climbing/checkin-bridge/main/deploy/macbook/update.sh
  sudo chmod +x /usr/local/mosaic-bridge/update.sh
'
```

From then on, you can push a new release and roll it out with one command from your laptop:

```
make deploy GYM=mosaic-gym.local                    # installs latest release
make deploy GYM=mosaic-gym.local TAG=v0.3.2         # pins a specific release

# Or directly on the box:
ssh $GYM "sudo /usr/local/mosaic-bridge/update.sh"
ssh $GYM "sudo /usr/local/mosaic-bridge/update.sh v0.3.2"
```

What the script does:

1. Resolves the tag (argument or `latest` via GitHub API).
2. Short-circuits if already on that version.
3. Downloads the `mosaic-bridge-darwin-arm64` asset + `.sha256`.
4. Verifies the checksum — refuses to install on mismatch.
5. Stops launchd, moves the old binary to `.prev`, installs the new one as `mosaic:staff` with mode 0755, restarts launchd.
6. Waits 5 seconds, hits `/health`. If healthy, exits success.
7. If `/health` fails, **automatically rolls back** to `.prev` and exits code 2.

Rollback is also available on demand:

```
ssh $GYM "sudo /usr/local/mosaic-bridge/update.sh rollback"
```

### 12e. Ops playbook for remote fixes

The loop you'll use for any future change:

1. Make the code change on your laptop.
2. `make test` — run unit tests locally.
3. `git commit && git push` — CI runs, catches compile / test failures.
4. Review CI output. If green, `make release-tag VERSION=v0.3.x` cuts the release.
5. `make deploy GYM=…` rolls it out.
6. Watch `tail -f /usr/local/mosaic-bridge/bridge.log` for a burn-in period.
7. If anything looks wrong: `make deploy TAG=v0.3.(x-1)` or `ssh $GYM "sudo /usr/local/mosaic-bridge/update.sh rollback"`.

The bridge logs its build version and a hash of the non-secret config on startup (`"configHash": "a1b2c3..."`). If that hash changes across restarts without a deploy, someone edited `.env` — useful for postmortems.

### 12f. What's not covered here

- **Auto-polling for new releases.** The MacBook currently only pulls when you tell it to. If you want an on-schedule check (e.g. hourly), add a launchd `StartCalendarInterval` job that runs `update.sh` — but prefer manual because uncontrolled auto-deploys to a door-unlock service is a bad idea.
- **Multi-environment (staging).** You have one gym, one MacBook. If a second location shows up, copy `update.sh` to each box, parameterize `GYM=` in the Makefile, and you're done.
- **Signed binaries.** The checksum verification catches tampering of the release asset, but not a compromised GitHub account. If that ever becomes a concern, add sigstore / cosign signing to the release workflow — one more line, no extra infra.

---

## 13. Appendix: file locations at a glance

| Path                                                       | What                           |
|------------------------------------------------------------|--------------------------------|
| `/usr/local/mosaic-bridge/mosaic-bridge`                   | The Go binary                  |
| `/usr/local/mosaic-bridge/.env`                            | Config (secrets live here)     |
| `/usr/local/mosaic-bridge/data/bridge.db`                  | SQLite store (members, checkins, policies, jobs) |
| `/usr/local/mosaic-bridge/bridge.log`                      | stdout                         |
| `/usr/local/mosaic-bridge/bridge.err`                      | stderr                         |
| `/Library/LaunchDaemons/com.mosaic.bridge.plist`           | launchd service definition     |
| `/Library/LaunchDaemons/com.mosaic.bridge-backup.plist`    | nightly SQLite backup job      |
| `/Library/LaunchDaemons/com.mosaic.bridge-health.plist`    | 5-minute healthcheck job       |
| `/etc/newsyslog.d/com.mosaic.bridge.conf`                  | log rotation rule              |
| `/usr/local/mosaic-bridge/backups/`                        | daily SQLite snapshots (30d)   |
| `/usr/local/mosaic-bridge/backup.sh`                       | backup script                  |
| `/usr/local/mosaic-bridge/healthcheck.sh`                  | healthcheck + alert script     |
| `http://<macbook-ip>:3500/ui`                              | Staff dashboard                |
| `http://<macbook-ip>:3500/health`                          | Health check endpoint          |
| `http://<macbook-ip>:3500/metrics`                         | Prometheus metrics             |
