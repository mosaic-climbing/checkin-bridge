#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────
# Secret preflight — refuse to push/commit if the tree or the index
# contains values that look like credentials or PII.
#
# Run locally ad-hoc:                scripts/check-secrets.sh
# Install as a pre-push hook:        scripts/check-secrets.sh --install
# Install as a pre-commit hook:      scripts/check-secrets.sh --install-precommit
# CI invokes it with --ci (fails on any finding without interactive prompts).
#
# Checks:
#   1. .env files are neither staged nor tracked
#   2. No known-leaked credential values (post-rotation tripwire)
#   3. No high-entropy / key-shaped strings (grep patterns)
#   4. .env template files contain only placeholders
#   5. TruffleHog filesystem scan for verifiable credentials
#
# Check 5 is optional: it warns-but-passes on hosts without trufflehog
# installed so fresh clones aren't hard-blocked. CI installs trufflehog
# explicitly; operators should too — see install hints below.
# ──────────────────────────────────────────────────────────
set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BOLD='\033[1m'
NC='\033[0m'

MODE="${1:-}"
REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$REPO_ROOT"

# Install mode: drop a pre-push hook that runs this script.
if [ "$MODE" = "--install" ]; then
    HOOK=".git/hooks/pre-push"
    mkdir -p "$(dirname "$HOOK")"
    cat > "$HOOK" <<'HOOK_EOF'
#!/usr/bin/env bash
exec scripts/check-secrets.sh --ci
HOOK_EOF
    chmod +x "$HOOK"
    echo -e "${GREEN}Installed pre-push hook at $HOOK${NC}"
    exit 0
fi

# Install pre-commit mode: runs the same checks but at commit-time so
# leaks get caught before they ever enter the local history. Lighter
# than --install (pre-push) in the common case because check 5 only
# scans staged files; heavier on every commit, so think of the two as
# complementary, not redundant.
if [ "$MODE" = "--install-precommit" ]; then
    HOOK=".git/hooks/pre-commit"
    mkdir -p "$(dirname "$HOOK")"
    cat > "$HOOK" <<'HOOK_EOF'
#!/usr/bin/env bash
# Auto-installed by scripts/check-secrets.sh --install-precommit
exec scripts/check-secrets.sh --ci
HOOK_EOF
    chmod +x "$HOOK"
    echo -e "${GREEN}Installed pre-commit hook at $HOOK${NC}"
    echo "    runs scripts/check-secrets.sh --ci on every 'git commit'"
    echo "    bypass with: git commit --no-verify  (please don't)"
    exit 0
fi

FAIL=0

fail() { echo -e "${RED}FAIL${NC} $*"; FAIL=1; }
pass() { echo -e "${GREEN}PASS${NC} $*"; }
warn() { echo -e "${YELLOW}WARN${NC} $*"; }

# ──────────────────────────────────────────────────────────
# 1. Refuse if any .env file (except example/template) is about to
#    be committed.
# ──────────────────────────────────────────────────────────
echo -e "${BOLD}═══ 1. No .env files staged ═══${NC}"
if git rev-parse --git-dir >/dev/null 2>&1; then
    STAGED_ENV=$(git diff --cached --name-only --diff-filter=AM 2>/dev/null \
        | grep -E '(^|/)\.env($|\.)' \
        | grep -vE '\.env\.(example|shadow|shadow\.example)$' || true)
    if [ -n "$STAGED_ENV" ]; then
        fail "these .env files are staged and will be committed:"
        echo "$STAGED_ENV" | sed 's/^/    /'
    else
        pass "no forbidden .env files staged"
    fi

    TRACKED_ENV=$(git ls-files \
        | grep -E '(^|/)\.env($|\.)' \
        | grep -vE '\.env\.(example|shadow|shadow\.example)$' || true)
    if [ -n "$TRACKED_ENV" ]; then
        fail "these .env files are ALREADY tracked in git history (remove and rotate keys):"
        echo "$TRACKED_ENV" | sed 's/^/    /'
    else
        pass "no forbidden .env files tracked"
    fi
else
    warn "not in a git repo — skipping git-index checks"
fi

# ──────────────────────────────────────────────────────────
# 2. Refuse if any source file contains a known-leaked or
#    known-live credential value. Add values here when you
#    rotate, to catch accidental re-introductions.
# ──────────────────────────────────────────────────────────
echo -e "\n${BOLD}═══ 2. No known-secret substrings ═══${NC}"

# Historically leaked — these were in scripts/test-connectivity.sh and .env.shadow
# before the scrub. Both have since been rotated on their respective providers;
# we keep the OLD values here as a regression tripwire so any reappearance
# (from a stale clone, a restored backup, a forgotten doc) fails the push.
#
# We deliberately do NOT track REDPOINT_GATE_ID here — it's an entity
# identifier, not a credential, and the same value legitimately lives in
# every deployed .env. Guarding it as "forbidden" would be a category error
# and would fight any legitimate reference (runbooks, deploy examples, etc).
KNOWN_LEAKED=(
    "J82J4hS/5vzMjamdVxlhsw"
    "SrgJepdUFeb5TEfmdnldcPWAPjy8amkFxkAXWryWjBle3Gob5dc71wQRZXOjmwQLCJTFvSiMmJsAc3BG6eDLvggJEmA521eb7MWuhjIVwixPXb56Ozxw4CBuJJmy3EzL"
)

for v in "${KNOWN_LEAKED[@]}"; do
    # Search every tracked file + every untracked-but-unignored file,
    # excluding THIS script itself — the KNOWN_LEAKED array above
    # legitimately contains these strings as regression tripwires, and
    # would otherwise match itself on every run.
    HITS=$(git ls-files -co --exclude-standard 2>/dev/null \
        | grep -vFx 'scripts/check-secrets.sh' \
        | xargs -I{} grep -l -F "$v" {} 2>/dev/null || true)
    if [ -z "$HITS" ] && ! command -v git >/dev/null; then
        HITS=$(grep -rlF "$v" . 2>/dev/null \
            | grep -v '^\./\.git/' \
            | grep -vFx './scripts/check-secrets.sh' || true)
    fi
    if [ -n "$HITS" ]; then
        fail "known-leaked value '${v:0:12}…' found in:"
        echo "$HITS" | sed 's/^/    /'
    fi
done
[ $FAIL -eq 0 ] && pass "no known-leaked substrings present"

# ──────────────────────────────────────────────────────────
# 3. Heuristic pattern scan: anything that LOOKS like a key
#    appearing in a committed file.
# ──────────────────────────────────────────────────────────
echo -e "\n${BOLD}═══ 3. High-entropy / key-shaped strings ═══${NC}"

# Scan tracked + unignored files. Ignore binaries, .git, vendor.
CANDIDATES=$(git ls-files -co --exclude-standard 2>/dev/null \
    | grep -vE '\.(png|jpg|jpeg|gif|ico|pdf|zip|gz|tar|bin|db|sqlite)$' \
    | grep -vE '^(vendor|data|backups)/' \
    || true)

# Patterns that usually mean a secret:
#   - private keys
#   - AWS/GCP/GitHub tokens
#   - Generic "Bearer <long-string>" in non-test code
PATTERN_HITS=""
if [ -n "$CANDIDATES" ]; then
    PATTERN_HITS=$(echo "$CANDIDATES" | xargs grep -EnH \
        -e 'BEGIN (RSA |EC |OPENSSH |DSA )?PRIVATE KEY' \
        -e 'AKIA[0-9A-Z]{16}' \
        -e 'ghp_[A-Za-z0-9]{36}' \
        -e 'gho_[A-Za-z0-9]{36}' \
        -e 'xox[baprs]-[A-Za-z0-9-]{10,}' \
        -e 'sk-[A-Za-z0-9]{32,}' \
        2>/dev/null || true)
fi

if [ -n "$PATTERN_HITS" ]; then
    fail "key-shaped strings detected:"
    echo "$PATTERN_HITS" | sed 's/^/    /'
else
    pass "no key-shaped strings detected"
fi

# ──────────────────────────────────────────────────────────
# 4. Check that the .env template(s) contain only placeholders.
# ──────────────────────────────────────────────────────────
echo -e "\n${BOLD}═══ 4. Templates only have placeholders ═══${NC}"
for f in .env.shadow.example .env.example; do
    [ -f "$f" ] || continue
    # Expect every *_KEY/*_TOKEN/*_PASSWORD to start with "REPLACE_" or be empty
    BAD=$(grep -E '^[A-Z_]*_(KEY|TOKEN|PASSWORD|SECRET)=' "$f" \
        | grep -vE '=(REPLACE_|$|\s*$)' || true)
    if [ -n "$BAD" ]; then
        fail "$f contains non-placeholder secrets:"
        echo "$BAD" | sed 's/^/    /'
    else
        pass "$f placeholders look clean"
    fi
done

# ──────────────────────────────────────────────────────────
# 5. TruffleHog — entropy + regex + live-credential detection.
#    Complements the earlier grep-based scans: TruffleHog covers
#    hundreds of credential types and can verify live keys against
#    their issuing services. Behavior:
#      - Not installed     → WARN only (so fresh clones aren't blocked).
#      - Staged files set  → scan just those (pre-commit fast-path).
#      - Staged files empty or --ci mode → scan the whole tree.
#    Only "verified" and "unknown" (high-confidence) findings fail the
#    run; known-false-positive classes are deliberately suppressed to
#    keep the pre-commit hook usable.
# ──────────────────────────────────────────────────────────
echo -e "\n${BOLD}═══ 5. TruffleHog scan ═══${NC}"
if command -v trufflehog >/dev/null 2>&1; then
    TH_VERSION=$(trufflehog --version 2>&1 | head -n1 || true)

    # Choose scope. --ci: full tree (catches historical regressions
    # the grep scans miss). Default (used by pre-commit hook): staged
    # files only, so `git commit` stays responsive.
    TH_SCOPE="staged"
    if [ "$MODE" = "--ci" ]; then
        TH_SCOPE="tree"
    fi

    TH_TMP=""
    # Return 0 unconditionally — the test `[ -n "$TH_TMP" ]` is false when
    # there were no staged files to scan, which would make the function's
    # last-command exit status propagate out through the EXIT trap and
    # override our `exit 0` summary. That silently fails the pre-push hook
    # with no visible error. Took a while to track down — don't remove.
    cleanup_th() { [ -n "$TH_TMP" ] && rm -rf "$TH_TMP"; return 0; }
    trap cleanup_th EXIT

    TH_OUT=""
    TH_RC=0
    if [ "$TH_SCOPE" = "staged" ]; then
        # Mirror the staged-additions set into a temp tree, then scan
        # that tree. This avoids scanning unstaged changes (noise) and
        # avoids asking trufflehog to walk node_modules/vendor.
        STAGED_FILES=$(git diff --cached --name-only --diff-filter=AM 2>/dev/null \
            | grep -vE '\.(png|jpg|jpeg|gif|ico|pdf|zip|gz|tar|bin|db|sqlite)$' \
            || true)
        if [ -z "$STAGED_FILES" ]; then
            pass "TruffleHog: no staged text files to scan ($TH_VERSION)"
        else
            TH_TMP=$(mktemp -d)
            echo "$STAGED_FILES" | while IFS= read -r f; do
                [ -z "$f" ] && continue
                # Preserve relative paths so TruffleHog output points
                # the operator at the real file on disk.
                mkdir -p "$TH_TMP/$(dirname "$f")"
                git show ":$f" > "$TH_TMP/$f" 2>/dev/null || true
            done
            TH_OUT=$(trufflehog filesystem --no-update --fail \
                        --results=verified,unknown "$TH_TMP" 2>&1 || TH_RC=$?)
            if [ "$TH_RC" -ne 0 ]; then
                fail "TruffleHog flagged staged changes:"
                # Strip the temp-dir prefix so paths read as the real
                # workspace paths in the finding output.
                echo "$TH_OUT" | sed "s|$TH_TMP/||g" | sed 's/^/    /'
            else
                pass "TruffleHog staged-files scan clean ($TH_VERSION)"
            fi
        fi
    else
        # Whole-tree scan. --no-update keeps CI hermetic; --exclude-paths
        # is via a small on-the-fly file so we don't walk binaries or
        # vendor dirs that are already excluded by .gitignore but still
        # present on disk (build outputs).
        TH_EX=$(mktemp)
        cat > "$TH_EX" <<'EX_EOF'
^\.git/
^data/
^backups/
^node_modules/
^vendor/
^dist/
\.db$
\.sqlite$
\.png$
\.jpg$
\.jpeg$
\.gif$
\.ico$
\.pdf$
\.zip$
\.gz$
\.tar$
\.bin$
^bridge$
^mosaic-bridge(-[a-z0-9-]+)?$
EX_EOF
        TH_OUT=$(trufflehog filesystem --no-update --fail \
                    --exclude-paths "$TH_EX" \
                    --results=verified,unknown . 2>&1 || TH_RC=$?)
        rm -f "$TH_EX"
        if [ "$TH_RC" -ne 0 ]; then
            fail "TruffleHog flagged the working tree:"
            echo "$TH_OUT" | sed 's/^/    /'
        else
            pass "TruffleHog full-tree scan clean ($TH_VERSION)"
        fi
    fi
else
    warn "trufflehog not installed — skipping section 5 (see install hints below)"
    echo "    macOS:   brew install trufflesecurity/trufflehog/trufflehog"
    echo "    Linux:   curl -sSfL https://raw.githubusercontent.com/trufflesecurity/trufflehog/main/scripts/install.sh \\"
    echo "             | sh -s -- -b /usr/local/bin"
    echo "    Go:      go install github.com/trufflesecurity/trufflehog/v3@latest"
fi

# ──────────────────────────────────────────────────────────
# Summary
# ──────────────────────────────────────────────────────────
echo ""
if [ "$FAIL" -eq 0 ]; then
    echo -e "${GREEN}${BOLD}All checks passed — safe to push.${NC}"
    exit 0
else
    echo -e "${RED}${BOLD}Secret check FAILED.${NC}"
    echo "Fix the findings above before pushing. If this fires after a rotation,"
    echo "remove the old value from KNOWN_LEAKED at the top of this script."
    exit 1
fi
