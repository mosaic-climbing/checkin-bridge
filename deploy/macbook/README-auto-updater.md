# Mac auto-updater

A `launchd` daemon that polls GitHub Releases every 90 seconds and rolls out new tags to the gym MacBook automatically. No SSH step.

## What it does

- Every 90 seconds, [auto-update.sh](auto-update.sh) hits `api.github.com/repos/mosaic-climbing/checkin-bridge/releases/latest`. That's 40 calls/hour, well under the 60/hour unauthenticated GitHub API limit per source IP.
- If the latest tag differs from the running binary's `-version` output, it invokes [update.sh](update.sh) with the new tag.
- `update.sh` does the actual work: SHA256 verify, atomic swap with `.prev` backup, `launchctl` restart, `:3500/health` probe, and **auto-rollback to `.prev` on health-check failure**. None of that changes — this folder just adds the polling loop on top.
- No-op runs are silent (no log spam every tick).

The auto-updater complements `make deploy`, not replaces it. `make deploy` still works for forcing an immediate roll-out from your laptop without waiting for the next 90-second tick.

## One-time install (on the gym Mac)

Run from the worktree on the Mac (or `scp` the three files over first). All commands need root because the LaunchDaemon runs in the system domain and `update.sh` is invoked as root.

```bash
# 1. Install the polling script next to update.sh.
sudo install -m 0755 -o root -g wheel \
    deploy/macbook/auto-update.sh \
    /usr/local/mosaic-bridge/auto-update.sh

# 2. Install the LaunchDaemon plist.
sudo install -m 0644 -o root -g wheel \
    deploy/macbook/com.mosaic.bridge-updater.plist \
    /Library/LaunchDaemons/com.mosaic.bridge-updater.plist

# 3. Load it. RunAtLoad=true means it kicks off immediately, then every 90 sec.
sudo launchctl load -w /Library/LaunchDaemons/com.mosaic.bridge-updater.plist
```

## Verify

```bash
# Service is loaded?
sudo launchctl list | grep com.mosaic.bridge-updater
# Expected: "<pid-or-dash>  <last-exit>  com.mosaic.bridge-updater"
# LastExitStatus should be 0.

# Force an immediate run instead of waiting for the next tick.
sudo launchctl kickstart -k system/com.mosaic.bridge-updater

# Watch the log.
sudo tail -f /var/log/com.mosaic.bridge-updater.log
```

**Expected output on a no-op tick:** silence (the script exits 0 without logging when `current == latest`).

**Expected output on an actual upgrade:**

```
[auto-update] 2026-04-28T14:35:02Z update available: current=v0.5.9 latest=v0.6.0
[update] target tag: v0.6.0
[update] downloading mosaic-bridge-darwin-arm64
[update] verifying SHA256 (from release channel — set EXPECTED_SHA256 for out-of-band)
mosaic-bridge-darwin-arm64: OK
[update] stopping bridge
[update] keeping old binary as .prev
[update] installing new binary
[update] starting bridge
[update] health check OK — v0.6.0 is live
[auto-update] 2026-04-28T14:35:18Z update to v0.6.0 succeeded
```

## End-to-end smoke test

After install, confirm a real release rolls out without a `make deploy`:

```bash
# On your laptop:
make release-tag VERSION=v0.5.9.1   # or whatever the next bump is
# DO NOT run `make deploy`. Wait up to ~90 seconds (plus the release-workflow build time).

# Then from your laptop, check the gym Mac:
ssh -t $GYM '/usr/local/mosaic-bridge/mosaic-bridge -version'
ssh -t $GYM 'sudo tail -30 /var/log/com.mosaic.bridge-updater.log'
```

The first command should show the new tag; the second should show the upgrade lines from above.

## Rollback / disable

### Pause the auto-updater

If you want to hold a version (debugging a regression, freeze before an event, etc.):

```bash
sudo launchctl unload /Library/LaunchDaemons/com.mosaic.bridge-updater.plist
```

To resume:

```bash
sudo launchctl load -w /Library/LaunchDaemons/com.mosaic.bridge-updater.plist
```

### Roll the bridge back to the previous binary

```bash
sudo /usr/local/mosaic-bridge/update.sh rollback
```

This restores `mosaic-bridge.prev` and restarts the daemon. **Caveat:** the auto-updater will re-install the latest tag on the next 90-second tick. If you want to *stay* on the rollback version, also `unload` the auto-updater (above). To return to auto-updating later, either bump the GitHub release past the broken one or yank the broken release.

### Yank a broken release

If a release ships and the bridge auto-rolls-back on the gym Mac (you'll see a `health check FAILED` line in the log followed by `rolled back`), the auto-updater will keep retrying every 90 seconds — loud logs, no damage, but noisy. To stop the loop:

1. **Delete or unpublish** the broken release on github.com/mosaic-climbing/checkin-bridge/releases. The auto-updater's `releases/latest` query then returns the prior good release, `current == latest`, and runs go silent.
2. Cut a fixed release with a higher version number.

## Uninstall

```bash
sudo launchctl unload /Library/LaunchDaemons/com.mosaic.bridge-updater.plist
sudo rm /Library/LaunchDaemons/com.mosaic.bridge-updater.plist
sudo rm /usr/local/mosaic-bridge/auto-update.sh
sudo rm -f /var/log/com.mosaic.bridge-updater.log /tmp/mosaic-bridge-updater.lock
```

## Files

| Path on Mac | Source in repo | Purpose |
|---|---|---|
| `/usr/local/mosaic-bridge/auto-update.sh` | [auto-update.sh](auto-update.sh) | Polls GitHub, calls `update.sh` on mismatch |
| `/Library/LaunchDaemons/com.mosaic.bridge-updater.plist` | [com.mosaic.bridge-updater.plist](com.mosaic.bridge-updater.plist) | Schedules the poll every 300 s |
| `/var/log/com.mosaic.bridge-updater.log` | (created at runtime) | Logs upgrade attempts and errors |
| `/tmp/mosaic-bridge-updater.lock/` | (created at runtime) | Mutex against concurrent runs |
| `/usr/local/mosaic-bridge/update.sh` | [update.sh](update.sh) | Unchanged — does the actual install |
| `/Library/LaunchDaemons/com.mosaic.bridge.plist` | [com.mosaic.bridge.plist](com.mosaic.bridge.plist) | The bridge daemon itself (referenced from DEPLOY.md §5) |
