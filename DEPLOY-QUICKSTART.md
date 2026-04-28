# Mosaic Bridge — M1 MacBook Pro Quick-Start Deploy

This is the distilled "just run these commands" version. Every section cross-references `DEPLOY.md` for the long-form explanation.

**Plan shape:** Day 1 get it running in shadow mode (~1 hour). Week 1 hands-off burn-in. Day 8 flip to live (~2 minutes). All binary installs + upgrades go through GitHub Releases via `update.sh` — no manual `scp` from your laptop.

---

## Before you start (5 min)

You need:
1. An M1 MacBook Pro on the gym LAN, plugged in, admin account logged in
2. USB-C Ethernet adapter connected to the same switch as the UDM Pro
3. The GitHub repo set up: CI passing, a published release (or the ability to cut one — see below)
4. A UniFi API token, Redpoint API key, Redpoint Gate ID (stash in a password manager)

**Cut a release if you don't have one yet** (from your laptop):

```bash
make release-tag VERSION=v0.3.0
```

That runs the secret scan, tests, tags, and pushes — GitHub Actions picks up the tag and publishes the `mosaic-bridge-darwin-arm64` binary + SHA256 sidecar to the release page. Check the [Actions tab](https://github.com/mosaic-climbing/checkin-bridge/actions) for green.

**Generate two secrets** on your laptop and save them in your password manager:

```bash
openssl rand -hex 32   # → ADMIN_API_KEY
openssl rand -hex 16   # → STAFF_PASSWORD
```

**Rotate UniFi + Redpoint tokens before starting** if you haven't — the old `.env.shadow` in git history had real values. See `DEPLOY.md` top banner.

**Repo visibility:** `update.sh` uses the public GitHub Releases API. If the repo is private, either make it public, or patch `update.sh` to send `-H "Authorization: token $GITHUB_TOKEN"` on its curl calls. See `DEPLOY.md` §12d.

---

## Step 1 — Prep the MacBook for 24/7 (10 min)

On the MacBook itself (one time only):

**Prevent sleep** (the single most important setting — without this, the bridge stops when you close the lid):

```bash
sudo pmset -c sleep 0 disksleep 0 displaysleep 0 \
                 hibernatemode 0 standby 0 autopoweroff 0 \
                 womp 1 tcpkeepalive 1
pmset -g | grep -E '^ (sleep|hibernatemode|disksleep)'   # verify: all 0
```

**System Settings → enable these**:
- Battery → Options → "Prevent automatic sleeping when the display is off" ✓
- Battery → Options → "Wake for network access" ✓
- **Users & Groups → "Automatically log in as" → pick your admin user** → enter that user's password to confirm. (Note: the `General → Login Items` pane is for *apps* that launch after login — it does NOT control auto-login. Auto-login lives in Users & Groups, and is only available when FileVault is OFF; if the menu is greyed out, disable FileVault first.)
- General → Sharing → Remote Login ✓ (so you can SSH in from your laptop)
- FileVault: **OFF** (or record recovery key — FileVault + reboot means bridge doesn't start until someone physically unlocks the disk)

**About sudo over SSH:** the heredoc form (`ssh $GYM 'bash -s' <<REMOTE`) is non-interactive and `sudo` inside it can't prompt for a password. Do **not** try to "fix" this with `ssh -tt` + `sudo -v` at the top of the heredoc — the tty and heredoc stdin are the same stream, so sudo reads the heredoc lines as failed password attempts and the whole thing hangs.

Two patterns that actually work:

1. **For the one-time install (Step 2):** don't use a heredoc. Open an interactive root shell instead, paste the commands, exit. Step 2 has been rewritten this way.
2. **For recurring `make deploy` runs later:** grant NOPASSWD *only* to the one script that runs non-interactively — `update.sh` — not the whole user. Do this after Step 4, once `update.sh` is installed and root-owned (instructions below in Step 4).

The pattern to avoid: `NOPASSWD:ALL` for a human admin account. It sounds convenient but means any compromise of that account is instant root. The scoped approach keeps root access pinned to a root-owned binary the admin can't edit.

**On the UDM Pro:** create a DHCP reservation pinning the MacBook's Ethernet MAC to a fixed IP. Note the IP — you'll use it below.

Why it matters: `DEPLOY.md` §1.

---

## Step 2 — Create the service user, directories, and `.env` (5 min)

From your laptop:

```bash
export GYM=<admin-username>@<macbook-fixed-ip>    # e.g. mosaicadmin@192.168.1.42
# Verify it works — whoami should print the admin username on the Mac:
ssh $GYM 'whoami'
```

Omitting the `<admin-username>@` part makes SSH fall back to your *local* username,
which typically isn't the account on the gym Mac and causes every subsequent
`ssh $GYM ...` command to fail with "Permission denied (publickey)".

Open an interactive root shell and paste the commands. The `-t` on ssh allocates a tty for the password prompt; `sudo -i` gives you a root login shell on the gym Mac so the pasted commands run as root without per-line `sudo`.

```bash
ssh -t $GYM 'sudo -i'
# type your Mac admin password at the prompt
```

Now paste this block into that root shell (it's the same commands but without `sudo` prefixes — you're already root):

```bash
set -euo pipefail

# 1. Dedicated non-root service user
if ! id mosaic >/dev/null 2>&1; then
  dscl . -create /Users/mosaic
  dscl . -create /Users/mosaic UserShell /usr/bin/false
  dscl . -create /Users/mosaic RealName "Mosaic Bridge"
  dscl . -create /Users/mosaic UniqueID "701"
  dscl . -create /Users/mosaic PrimaryGroupID 20
  dscl . -create /Users/mosaic NFSHomeDirectory /var/empty
fi

# 2. Install dir + data dir with tight perms
mkdir -p /usr/local/mosaic-bridge/data
chmod 700 /usr/local/mosaic-bridge/data
chown -R mosaic:staff /usr/local/mosaic-bridge

# 3. Pre-create log files so launchd can write to them
touch /usr/local/mosaic-bridge/bridge.log /usr/local/mosaic-bridge/bridge.err
chown mosaic:staff /usr/local/mosaic-bridge/bridge.log /usr/local/mosaic-bridge/bridge.err

# verify
id mosaic
ls -la /usr/local/mosaic-bridge
```

Expect `id mosaic` to print the new user with UID 701, and `ls -la` to show `data/` owned by `mosaic:staff` mode 700. Then `exit` to leave the root shell, `exit` again to close the ssh session.

Verify from your laptop (read-only, no sudo needed):

```bash
ssh $GYM 'id mosaic && ls -la /usr/local/mosaic-bridge'
```

For the later `scp .env.shadow.example ...` + `ssh '... sudo mv ...'` block, the sudo call is a single command (not a heredoc), so either run `ssh -t $GYM 'sudo mv ...'` and type your password once, or do the mv inside the same `sudo -i` session earlier.

**Populate `.env` on your laptop, then ship the completed file** — editing in a real editor is much less error-prone than vi over SSH.

```bash
# Copy the tracked template to a gitignored working copy
cp .env.shadow.example .env

# Sanity-check it's gitignored (no output = ignored)
git check-ignore -v .env && echo "GITIGNORED ✓"
```

Open `.env` in your editor and fill in:

```ini
UNIFI_HOST=<udm-lan-ip>
DATA_DIR=/usr/local/mosaic-bridge/data

ADMIN_API_KEY=<openssl rand -hex 32 output>
STAFF_PASSWORD=<openssl rand -hex 16 output>

UNIFI_API_TOKEN=<fresh rotated token>
REDPOINT_API_KEY=<fresh rotated key>
REDPOINT_GATE_ID=<your gate id>

# Leave these as-is for Day 1
BRIDGE_SHADOW_MODE=true
BIND_ADDR=127.0.0.1

# Optional: pin sync to quiet hours
SYNC_TIME_LOCAL=04:00
```

Ship it, preserving 600 mode through the `/tmp` hop, then atomically install into the final path:

```bash
chmod 600 .env
scp -p .env $GYM:/tmp/bridge.env
ssh -t $GYM 'sudo install -o mosaic -g staff -m 600 /tmp/bridge.env /usr/local/mosaic-bridge/.env && sudo rm /tmp/bridge.env'
```

`install` is deliberate — it sets owner, group, and mode in one atomic operation, so there's no brief window where the file is world-readable at the final path. The `rm` wipes the `/tmp` copy immediately.

**Verify:**

```bash
ssh $GYM 'ls -la /usr/local/mosaic-bridge/.env'
# expect: -rw-------  1 mosaic  staff  <size> ...
```

The local `.env` can stay in your repo dir for reference — it's gitignored, CI's secret scan scopes env-file checks to staged files only, and having a copy is useful if you ever need to diff or re-deploy. Just never `git add` it.

**Sanity-check UA-Hub is reachable:**

```bash
ssh $GYM 'curl -kI https://<udm-lan-ip>:12445/api/v1/developer/doors'
# HTTP/2 200 or 401 means the port is open — anything completing TLS is fine
```

Why it matters: `DEPLOY.md` §2 + §3 + §4.

---

## Step 3 — Install the launchd plist (2 min)

The plist doesn't need the binary to exist yet — `update.sh` will fetch and install the binary on its first run, then the plist will be ready to load.

The plist is checked into the repo (`deploy/macbook/com.mosaic.bridge.plist`). Pull it onto the Mac via `curl`:

```bash
ssh -t $GYM '
  sudo curl -fsSL -o /Library/LaunchDaemons/com.mosaic.bridge.plist \
    https://raw.githubusercontent.com/mosaic-climbing/checkin-bridge/main/deploy/macbook/com.mosaic.bridge.plist
  sudo chown root:wheel /Library/LaunchDaemons/com.mosaic.bridge.plist
  sudo chmod 644        /Library/LaunchDaemons/com.mosaic.bridge.plist
'
```

Why it matters: `DEPLOY.md` §5.

---

## Step 4 — Install `update.sh` and run the first install (3 min)

One-time bootstrap — pull `update.sh` straight from the repo:

```bash
ssh $GYM '
  sudo curl -fsSL -o /usr/local/mosaic-bridge/update.sh \
    https://raw.githubusercontent.com/mosaic-climbing/checkin-bridge/main/deploy/macbook/update.sh
  sudo chmod +x /usr/local/mosaic-bridge/update.sh
'
```

**First install:** `update.sh` fetches the latest release binary, verifies its SHA256, installs it as `mosaic:staff` mode 0755, loads the launchd plist, waits 5 seconds, and calls `/health`. If healthy → exit 0. If not → automatic rollback (but on first install there's no `.prev` so the script exits with code 2 and you investigate).

```bash
ssh $GYM 'sudo /usr/local/mosaic-bridge/update.sh'
```

You should see (in order):

```
[update] resolving latest release tag
[update] target tag: v0.3.0
[update] downloading mosaic-bridge-darwin-arm64
[update] verifying SHA256
[update] stopping bridge
[update] installing new binary
[update] starting bridge
[update] health check OK — v0.3.0 is live
```

**Verify the service is running:**

```bash
ssh $GYM 'sudo launchctl list | grep mosaic'                    # should show a PID, not "-"
ssh $GYM 'tail -20 /usr/local/mosaic-bridge/bridge.log'          # look for: SHADOW MODE ENABLED / UniFi WebSocket connected
ssh $GYM 'curl -s http://127.0.0.1:3500/health'                  # should return OK-ish
```

If `update.sh` says "health check FAILED — rolling back" on first install with no `.prev`, look at `bridge.err` — it's almost always a missing `.env` value.

Why it matters: `DEPLOY.md` §12d.

---

## Step 5 — Reach the dashboard over SSH tunnel (2 min)

From your laptop:

```bash
ssh -L 3500:127.0.0.1:3500 $GYM
# then in a browser: http://localhost:3500/ui
```

Log in with `STAFF_PASSWORD`. You should see Dashboard, Members, Check-ins, Sync & Jobs, Door Policies, Metrics in the sidebar.

Why it matters: `DEPLOY.md` §6.

---

## Step 6 — Seed the member store (10 min)

In the dashboard:

1. **Sync & Jobs → Directory Sync → Run Now.** Pulls Redpoint customers into the local cache. Takes 1–5 minutes.
2. **Sync & Jobs → UniFi Ingest → Dry Run.** Matches every UniFi NFC user against Redpoint by email then name. Returns matched / unmatched / skipped counts.
3. **Sync & Jobs → Unmatched UniFi Users → Scan UniFi.** For each unmatched row, click **Search Redpoint →** and either fix the Redpoint record (usually a missing email) or use **Add Member Manually**.
4. Once the unmatched list is clean, re-run ingest for real:
   ```bash
   ssh $GYM 'curl -X POST -H "X-API-Key: $ADMIN_API_KEY" \
     "http://localhost:3500/ingest/unifi?dry_run=false"'
   ```

Why it matters: `DEPLOY.md` §7.

---

## Step 7 — Let it burn in (several days, hands-off)

The single goal of shadow mode: drive **Shadow Decisions → Disagree** to zero across a full weekly cycle of real traffic.

Each morning check:

```bash
# Dashboard → Shadow Decisions panel. Target: Disagree = 0.

# Or from the command line:
ssh $GYM 'tail -200 /usr/local/mosaic-bridge/bridge.log | grep -E "SHADOW|DENIED"'
```

**Must be zero before flipping:**
- **Would miss** (UniFi allowed, bridge denied) — paying members you'd lock out
- **Would admit** (UniFi blocked, bridge allowed) — taps UniFi rejected that you'd pass

Non-zero? Almost always one of: stale Redpoint cache (run Directory Sync), UniFi has staff/contractors not in Redpoint (add manually or define a policy), mismatched door groups.

Full burn-in checklist including the fail-closed test and WebSocket staleness check: `DEPLOY.md` §8.

---

## Step 8 — Flip to live (2 min, when ready)

```bash
ssh $GYM '
  sudo -u mosaic sed -i "" "s/^BRIDGE_SHADOW_MODE=.*/BRIDGE_SHADOW_MODE=false/" /usr/local/mosaic-bridge/.env
  sudo launchctl kickstart -k system/com.mosaic.bridge
  tail -f /usr/local/mosaic-bridge/bridge.log
'
```

The `SHADOW MODE ENABLED` banner should be gone. Tap a card — you should now see `CHECK-IN SUCCESS` and `Redpoint check-in recorded async` instead of `SHADOW: would …`.

**Sit on the log for one full shift** in case anything surprises you.

**Rollback at any time:**

```bash
ssh $GYM '
  sudo -u mosaic sed -i "" "s/^BRIDGE_SHADOW_MODE=.*/BRIDGE_SHADOW_MODE=true/" /usr/local/mosaic-bridge/.env
  sudo launchctl kickstart -k system/com.mosaic.bridge
'
```

Or just stop the service entirely — UA-Hub keeps enforcing the whole time, independent of the bridge.

Why it matters: `DEPLOY.md` §9.

---

## Step 9 — Reliability extras (30 min, do once)

Three small launchd jobs that make the deploy production-shaped. All three can be done any time after step 4.

**Nightly SQLite backup** (keeps 30 days):

```bash
ssh $GYM 'bash -s' <<'REMOTE'
sudo tee /usr/local/mosaic-bridge/backup.sh > /dev/null <<'SH'
#!/bin/bash
set -euo pipefail
BACKUP_DIR=/usr/local/mosaic-bridge/backups
mkdir -p "$BACKUP_DIR"
TS=$(date +%Y%m%d-%H%M)
/usr/bin/sqlite3 /usr/local/mosaic-bridge/data/bridge.db ".backup '$BACKUP_DIR/bridge-$TS.db'"
find "$BACKUP_DIR" -name 'bridge-*.db' -mtime +30 -delete
SH
sudo chmod +x /usr/local/mosaic-bridge/backup.sh
sudo chown mosaic:staff /usr/local/mosaic-bridge/backup.sh

sudo tee /Library/LaunchDaemons/com.mosaic.bridge-backup.plist > /dev/null <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.mosaic.bridge-backup</string>
  <key>ProgramArguments</key><array><string>/usr/local/mosaic-bridge/backup.sh</string></array>
  <key>UserName</key><string>mosaic</string>
  <key>StartCalendarInterval</key><dict><key>Hour</key><integer>3</integer><key>Minute</key><integer>30</integer></dict>
  <key>StandardErrorPath</key><string>/usr/local/mosaic-bridge/backup.err</string>
</dict></plist>
PLIST
sudo chown root:wheel /Library/LaunchDaemons/com.mosaic.bridge-backup.plist
sudo chmod 644        /Library/LaunchDaemons/com.mosaic.bridge-backup.plist
sudo launchctl load -w /Library/LaunchDaemons/com.mosaic.bridge-backup.plist
REMOTE
```

**Log rotation via `newsyslog`** (14 files, rotates at 10 MB, gzips old):

```bash
ssh $GYM 'sudo tee /etc/newsyslog.d/com.mosaic.bridge.conf > /dev/null' <<'EOF'
/usr/local/mosaic-bridge/bridge.log   mosaic:staff   644   14   10240   *   JN
/usr/local/mosaic-bridge/bridge.err   mosaic:staff   644   14   10240   *   JN
EOF
```

**5-minute healthcheck** (logs via `logger` — swap in Pushover / ntfy / email in `healthcheck.sh`):

```bash
ssh $GYM 'bash -s' <<'REMOTE'
sudo tee /usr/local/mosaic-bridge/healthcheck.sh > /dev/null <<'SH'
#!/bin/bash
set -u
URL="http://127.0.0.1:3500/health"
if ! curl -fsS --max-time 5 "$URL" > /dev/null; then
  logger -t mosaic-bridge "HEALTH FAIL: bridge not responding on $URL"
  # Uncomment + fill in to get pushed:
  # curl -fsS \
  #   --data-urlencode "token=$PUSHOVER_TOKEN" \
  #   --data-urlencode "user=$PUSHOVER_USER" \
  #   --data-urlencode "message=Mosaic bridge DOWN" \
  #   --data-urlencode "priority=1" \
  #   https://api.pushover.net/1/messages.json
fi
SH
sudo chmod +x /usr/local/mosaic-bridge/healthcheck.sh
sudo chown mosaic:staff /usr/local/mosaic-bridge/healthcheck.sh

sudo tee /Library/LaunchDaemons/com.mosaic.bridge-health.plist > /dev/null <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.mosaic.bridge-health</string>
  <key>ProgramArguments</key><array><string>/usr/local/mosaic-bridge/healthcheck.sh</string></array>
  <key>UserName</key><string>mosaic</string>
  <key>StartInterval</key><integer>300</integer>
  <key>StandardErrorPath</key><string>/usr/local/mosaic-bridge/health.err</string>
</dict></plist>
PLIST
sudo chown root:wheel /Library/LaunchDaemons/com.mosaic.bridge-health.plist
sudo chmod 644        /Library/LaunchDaemons/com.mosaic.bridge-health.plist
sudo launchctl load -w /Library/LaunchDaemons/com.mosaic.bridge-health.plist
REMOTE
```

Why it matters: `DEPLOY.md` §10.

---

## The upgrade loop (every future release)

All routine code changes flow through GitHub Actions. You never again manually copy a binary to the MacBook.

```bash
# 1. Make the change, commit, push. CI runs tests + build.
git push

# 2. Cut a release (runs secret-scan + tests, tags, pushes)
make release-tag VERSION=v0.3.1

# 3. Wait for the release workflow to go green on GitHub Actions

# 4. Roll it out to the gym
make deploy GYM=$GYM

# Or pin a specific version
make deploy GYM=$GYM TAG=v0.3.1

# 5. Watch the log for a burn-in period
ssh $GYM 'tail -f /usr/local/mosaic-bridge/bridge.log'
```

What `make deploy` / `update.sh` does under the hood:
1. Resolves tag (argument or `latest` via GitHub API)
2. Short-circuits if the bridge is already on that version
3. Downloads asset + SHA256
4. Verifies checksum (refuses to install on mismatch)
5. Stops launchd, keeps the old binary as `.prev`, installs new one as `mosaic:staff` 0755, restarts
6. Waits 5s, hits `/health` — if OK, exits success
7. If `/health` fails, **automatic rollback** to `.prev` and exits code 2

**Rollback on demand:**

```bash
ssh $GYM 'sudo /usr/local/mosaic-bridge/update.sh rollback'
```

Why it matters: `DEPLOY.md` §12.

---

## Day-to-day cheat sheet

**Check it's alive:**
```bash
ssh $GYM 'sudo launchctl list | grep mosaic'     # PID means running
ssh $GYM 'curl -s http://127.0.0.1:3500/health'
```

**Tail the log:**
```bash
ssh $GYM 'tail -f /usr/local/mosaic-bridge/bridge.log'
```

**Restart after an `.env` change:**
```bash
ssh $GYM 'sudo launchctl kickstart -k system/com.mosaic.bridge'
```

**Deploy / rollback:**
```bash
make deploy GYM=$GYM                                             # latest
make deploy GYM=$GYM TAG=v0.3.2                                  # specific
ssh $GYM 'sudo /usr/local/mosaic-bridge/update.sh rollback'      # revert
```

---

## "Something broke" triage

| Symptom | First check |
|---|---|
| `sudo: a terminal is required to read the password` in heredoc output, **or the heredoc hangs** | Don't try to fix this with `ssh -tt` + `sudo -v` — the tty and heredoc stdin collide and sudo consumes heredoc lines as password attempts. Use an interactive root shell instead: `ssh -t $GYM 'sudo -i'` then paste the commands without `sudo` prefixes. See Step 1's sudo-over-SSH note. |
| `update.sh` says "checksum mismatch" | Re-run; if it persists, the release asset may be corrupted — re-publish from GitHub Actions |
| `update.sh` rolled back after install | `ssh $GYM 'cat /usr/local/mosaic-bridge/bridge.err'` — usually a missing `.env` var in the new build's config schema |
| Bridge won't start (no `.prev`) | `ssh $GYM 'cat /usr/local/mosaic-bridge/bridge.err'` — missing `.env` value or bad `DATA_DIR` |
| Dashboard 401 / login loop | Trailing whitespace or quotes in `STAFF_PASSWORD` in `.env` |
| Every tap says "not in store" | Run Directory Sync + UniFi Ingest from the dashboard |
| WebSocket drops constantly | UDM firewall for `12445/TCP`, or UniFi API token rotated/expired |
| Bridge offline after lid closed | `ssh $GYM 'pmset -g \| grep sleep'` — re-run step 1 `pmset` commands. Also confirm it's on AC power. |
| Lots of shadow disagreements | Do **not** flip to live. Investigate per `DEPLOY.md` §11. |
| MacBook rebooted, bridge didn't come back | FileVault on? Auto-login? `RunAtLoad=true` in plist? Plist owned `root:wheel` mode 644? |

Everything else is in `DEPLOY.md` §11.

---

## Full `DEPLOY.md` section map

| Topic | Section |
|---|---|
| Prerequisites + secret rotation | top banner + §0 |
| MacBook hardening (pmset, power, SSH) | §1 |
| File layout + permissions | §2 |
| `.env` walkthrough | §3 |
| UDM connectivity test | §4 |
| launchd plist | §5 |
| Dashboard | §6 |
| Member enrollment paths | §7 |
| Shadow-mode burn-in checklist | §8 |
| Flip to live + rollback | §9 |
| Service control + logs + backups + healthcheck | §10 |
| Troubleshooting | §11 |
| CI + release workflow + `update.sh` internals | §12 |
| File locations at a glance | §13 |
