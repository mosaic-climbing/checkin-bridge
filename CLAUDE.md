# CLAUDE.md

Context and conventions for AI assistants working on this repo. Read this first.

---

## SSH + sudo — read this before writing any ssh command

**The gym MacBook does NOT have passwordless sudo.** Every `sudo` invocation on the remote needs a password prompt. That constrains how you write remote commands from your laptop. I keep getting this wrong; this section exists to stop that.

### The two modes that actually work

**Mode A — one-shot sudo command** (the common case). Always use `-t`:

```bash
ssh -t $GYM 'sudo <command>'
```

`-t` allocates a TTY so `sudo` can prompt you for the Mac admin password. You type it once, the command runs, done. Without `-t`, `sudo` errors with `"sudo: a terminal is required to read the password"` and exits immediately. This applies to **every** diagnostic, not just `update.sh`:

- `ssh -t $GYM 'sudo -u mosaic sqlite3 /usr/local/mosaic-bridge/data/cache.db "SELECT ..."'`
- `ssh -t $GYM 'sudo awk -F= "/^ADMIN_API_KEY=/ {...}" /usr/local/mosaic-bridge/.env'`
- `ssh -t $GYM 'sudo launchctl kickstart -k system/com.mosaic.bridge'`
- `ssh -t $GYM 'sudo tail -50 /usr/local/mosaic-bridge/bridge.err'`

**Mode B — several sudo commands in a row.** Use an interactive root shell so you type the password once, not once per command:

```bash
ssh -t $GYM 'sudo -i'
# you're now root on the Mac; paste commands WITHOUT sudo prefixes
```

Use this for multi-step setup (installing files, editing sudoers, etc.).

### The mode that DOES NOT work — `-t` + heredoc

**NEVER write `ssh -t $GYM '...' <<'EOF'`**. The `-t` forces TTY mode and the heredoc goes to the same stdin. `sudo` reads the heredoc lines as password attempts, the command hangs or errors, and you lose time. This bites me repeatedly — hence this warning.

If you need to write a file content to a remote path, do one of:

1. **`scp` + `ssh -t sudo mv`** — separate the "what content" from the "where it lands":
   ```bash
   cat > /tmp/local-file <<'EOF'
   <content>
   EOF
   scp /tmp/local-file $GYM:/tmp/remote-file
   ssh -t $GYM 'sudo install -o root -g wheel -m 0644 /tmp/remote-file /etc/target/file && rm /tmp/remote-file'
   ```
2. **Interactive root shell** (Mode B), then `cat > /path <<EOF` inside the shell.

The pattern to avoid: `ssh -tt $GYM 'sudo -v' <<EOF`, `ssh $GYM 'sudo -S ...' <<EOF` with the password piped in — both fragile, both have bitten us.

### Why `make deploy` works without thinking

The Makefile's `deploy` target uses `-t`:

```make
deploy:
	ssh -t $(GYM) "sudo /usr/local/mosaic-bridge/update.sh $(TAG)"
```

If you write a new make target or script that runs sudo remotely, copy that pattern.

### Quick self-check before running a remote sudo command

Ask yourself:

1. Does the command need sudo on the remote? → Use `ssh -t`, not plain `ssh`.
2. Am I piping a heredoc into the ssh command? → Rewrite as `scp` + `ssh -t sudo install`.
3. Is it multi-step sudo? → Use `ssh -t $GYM 'sudo -i'` and paste inside.

---

## What this repo is

A single Go binary (`cmd/bridge`) that bridges NFC taps on a UniFi Access reader to Redpoint HQ membership validation, unlocks the door on a match, and records the check-in. One deployment target (the MacBook at Mosaic), one environment, one developer.

See `ARCHITECTURE.md` for the full picture and `DEPLOY.md` / `DEPLOY-QUICKSTART.md` for operational details.

---

## Git workflow

Trunk-based with short-lived feature branches. `main` is always deployable. Tags are releases.

**For non-trivial work (anything bigger than a typo or doc tweak):**

```bash
git checkout -b <type>/<short-description>
# ... commits ...
git push -u origin <branch>
gh pr create --fill                # CI runs on the PR
# after CI passes:
gh pr merge --squash --delete-branch
```

Branch name prefixes (loose, not enforced):
- `fix/` — bug fixes
- `feat/` — new functionality
- `refactor/` — code movement / cleanup with no behavior change
- `chore/` — deps, tooling, CI, non-code
- `docs/` — docs only

**Small/urgent fixes** (one-line typo, trivial doc fix) can go straight to `main`. Use judgment.

**Squash-merge on PR merge.** Keeps the `main` log linear and revert-friendly — one commit per logical change, so `git revert <sha>` rolls back cleanly without untangling a chain of WIP commits.

**Never force-push `main`.** Once branch protection is enabled this is enforced, but treat it as a rule from now either way.

---

## Releases

Releases come from git tags, not branches. Cut a release with:

```bash
make release-tag VERSION=vX.Y.Z
```

This tags the current commit on `main` (make sure CI is green first), pushes the tag, and triggers `.github/workflows/release.yml` which produces the three-target binary matrix and attaches them to the GitHub release. `scripts/update.sh` on the deployed bridge pulls from these releases.

Versioning is semver-ish:
- Patch bumps (`v0.3.1`) — bug fixes, doc tweaks, no behavior change
- Minor bumps (`v0.4.0`) — new features, behavior additions
- Major bumps (`v1.0.0`) — breaking config/CLI changes or architectural shifts

We're pre-1.0, so minor bumps can still introduce breaking changes — document them in the release notes.

---

## CI + pre-push

Every push and every PR runs `.github/workflows/ci.yml`:

1. **Secret scan** — `scripts/check-secrets.sh --ci` (5 sections: staged env files, known-leaked substrings, key-shape regex, template placeholder check, full-tree TruffleHog scan). No gitleaks — it went paid for orgs; TruffleHog covers the ground.
2. **Test & vet** — `go mod tidy` verification, `go vet`, `staticcheck`, `go test -race -coverprofile`.
3. **Build matrix** — `darwin/arm64`, `linux/arm64`, `linux/amd64`, with SHA-256 checksums.

Locally, a pre-push hook runs `scripts/check-secrets.sh` (staged-files scope) so secrets are caught before they leave the machine. Install it with:

```bash
./scripts/check-secrets.sh --install
```

If the pre-push hook ever "passes" but the push still fails with only `error: failed to push some refs` — check for an EXIT-trap bug. We hit one where the trap's last command used `[ -n "$VAR" ] && ...` which returned 1 when the var was empty, propagating a non-zero exit code out and making git reject the push. The fix was appending `return 0` to the trap function. If a similar pattern shows up elsewhere, apply the same fix.

---

## Code conventions

Go defaults except where noted below.

- **Lint gate:** `go vet` + `staticcheck` must pass. CI enforces. Fix findings; don't add `//lint:ignore` directives without a one-line comment explaining why.
- **Error messages:** lowercase first word unless starting with a proper noun that *can't* be rephrased. `fmt.Errorf("redpoint live query failed: %w", err)` — not `"Redpoint live query failed..."`. Staticcheck ST1005 will flag it.
- **Error wrapping:** always `%w` when wrapping; never `%v` on errors that callers might want to `errors.Is` / `errors.As`.
- **Context:** every function that might block on IO takes `ctx context.Context` as its first argument. No `context.Background()` in library code.
- **Logging:** `slog` everywhere. Structured fields, no `fmt.Sprintf` in log messages.
- **Metrics naming:** Prometheus convention (`_total`, `_seconds`, `_bytes` suffixes). See `internal/metrics/metrics.go` for the registry.
- **Testing:** table-driven where the test is a matrix. `testing.T.TempDir()` over `os.TempDir()`. Race detector on in CI; don't skip it.
- **No `init()`** unless registering with a std-lib registry (e.g. `database/sql` drivers). Wire dependencies explicitly from `main`.

---

## Secrets

Real secrets never enter the repo. Mechanism:

- `.env` is gitignored (`.gitignore` has it under "Secrets").
- `.env.shadow.example` is the committed template; copy it to `.env` on the target machine and fill in real values.
- `scripts/check-secrets.sh` has a `KNOWN_LEAKED` array of rotated-credential substrings that acts as a regression tripwire — never remove values from it without explicitly noting the rotation in the PR description.
- Don't add `REDPOINT_GATE_ID` or other entity identifiers to `KNOWN_LEAKED`; they're not credentials and legitimately live in every deployed `.env`.

When rotating a credential:
1. Issue the new value on the provider.
2. Append the **old** value to `KNOWN_LEAKED` in `check-secrets.sh` as a future tripwire.
3. Update `.env` on the deployed machine.
4. Restart the bridge.

---

## Deployment safety

The bridge has a **shadow mode** (`BRIDGE_SHADOW_MODE=true` in `.env`). In shadow mode it:
- Listens to UniFi tap events
- Resolves members and logs every decision
- **Never** unlocks the door, updates user status, or records check-ins in Redpoint

This is the deployment safety valve. Every new deploy starts in shadow mode for several days. Watch the logs and the Shadow Decisions dashboard panel. Only flip to live mode after multiple days of correct decisions.

Don't remove shadow mode without replacing it with an equivalent safety. It's the one mechanism that makes a config typo recoverable rather than destructive.

---

## Local development

```bash
go test ./...              # full test suite
go test -race ./...        # catch data races; CI runs this
go vet ./...
staticcheck ./...          # install: go install honnef.co/go/tools/cmd/staticcheck@latest
go build -o bin/bridge ./cmd/bridge
```

The bridge needs a `.env` to run. Copy `.env.shadow.example`, fill in test values, and point it at a non-production Redpoint facility if you have one.
