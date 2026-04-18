#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────
# Mosaic Climbing – API Connectivity Test
#
# Run this from any machine on your network to verify
# UniFi Access and Redpoint HQ API connectivity.
#
# Usage: ./scripts/test-connectivity.sh
# ──────────────────────────────────────────────────────────
set -euo pipefail

# ── Config — loaded from env only, never hardcoded ───────
# This script reads from .env automatically if present (same file the
# bridge uses), or from env vars. Never commit secrets into this file.
if [ -f "$(dirname "$0")/../.env" ]; then
    # shellcheck disable=SC1091
    set -a
    . "$(dirname "$0")/../.env"
    set +a
fi

UNIFI_HOST="${UNIFI_HOST:?UNIFI_HOST not set (put it in .env or export it)}"
UNIFI_PORT="${UNIFI_PORT:-12445}"
UNIFI_TOKEN="${UNIFI_API_TOKEN:?UNIFI_API_TOKEN not set}"

REDPOINT_URL="${REDPOINT_API_URL:?REDPOINT_API_URL not set}"
REDPOINT_KEY="${REDPOINT_API_KEY:?REDPOINT_API_KEY not set}"
REDPOINT_FACILITY="${REDPOINT_FACILITY_CODE:-Mosaic}"

# ── Colors ────────────────────────────────────────────────
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

pass() { echo -e "  ${GREEN}PASS${NC} $1"; }
fail() { echo -e "  ${RED}FAIL${NC} $1"; }
info() { echo -e "  ${CYAN}INFO${NC} $1"; }
header() { echo -e "\n${BOLD}═══ $1 ═══${NC}"; }

UNIFI_BASE="https://${UNIFI_HOST}:${UNIFI_PORT}/api/v1/developer"

# ── Helper: pretty-print JSON ────────────────────────────
pp() {
    python3 -m json.tool 2>/dev/null || cat
}

# ──────────────────────────────────────────────────────────
header "1. UniFi Access API (${UNIFI_HOST}:${UNIFI_PORT})"
# ──────────────────────────────────────────────────────────

echo -e "\n${YELLOW}Testing: GET /doors${NC}"
DOORS=$(curl -sk --connect-timeout 5 "${UNIFI_BASE}/doors" \
    -H "Authorization: Bearer ${UNIFI_TOKEN}" 2>&1) || true

if echo "$DOORS" | python3 -c "import sys,json; json.load(sys.stdin)" 2>/dev/null; then
    pass "UniFi Access API is reachable"
    echo "$DOORS" | pp
    DOOR_COUNT=$(echo "$DOORS" | python3 -c "
import sys, json
d = json.load(sys.stdin)
items = d.get('data', d) if isinstance(d, dict) else d
print(len(items) if isinstance(items, list) else '?')
" 2>/dev/null || echo "?")
    info "Found ${DOOR_COUNT} door(s)"
else
    fail "UniFi Access API unreachable or auth failed"
    echo "  Response: ${DOORS:0:200}"
    info "Check: Is ${UNIFI_HOST}:${UNIFI_PORT} reachable? Is the API token correct?"
    info "Try: curl -sk ${UNIFI_BASE}/doors -H 'Authorization: Bearer YOUR_TOKEN'"
fi

echo -e "\n${YELLOW}Testing: GET /users (NFC credentials, all pages)${NC}"
ALL_USERS="[]"
PAGE=1
PAGE_SIZE=100
TOTAL_FETCHED=0

while true; do
    USERS=$(curl -sk --connect-timeout 10 --max-time 30 "${UNIFI_BASE}/users?page_num=${PAGE}&page_size=${PAGE_SIZE}" \
        -H "Authorization: Bearer ${UNIFI_TOKEN}" 2>&1) || true

    if ! echo "$USERS" | python3 -c "import sys,json; json.load(sys.stdin)" 2>/dev/null; then
        if [ "$PAGE" -eq 1 ]; then
            fail "UniFi users endpoint unreachable"
            echo "  Response: ${USERS:0:200}"
        fi
        break
    fi

    COUNT=$(echo "$USERS" | python3 -c "
import sys, json
d = json.load(sys.stdin)
users = d.get('data', d) if isinstance(d, dict) else d
print(len(users) if isinstance(users, list) else 0)
" 2>/dev/null)

    if [ "$PAGE" -eq 1 ]; then
        pass "UniFi users endpoint reachable"
    fi

    # Merge this page into ALL_USERS
    ALL_USERS=$(python3 -c "
import sys, json
existing = json.loads(sys.argv[1])
data = json.load(sys.stdin)
users = data.get('data', data) if isinstance(data, dict) else data
if isinstance(users, list):
    existing.extend(users)
print(json.dumps(existing))
" "$ALL_USERS" <<< "$USERS" 2>/dev/null)

    TOTAL_FETCHED=$((TOTAL_FETCHED + COUNT))
    info "Page ${PAGE}: ${COUNT} users (total so far: ${TOTAL_FETCHED})"

    if [ "${COUNT:-0}" -lt "$PAGE_SIZE" ]; then
        break
    fi
    PAGE=$((PAGE + 1))
done

if [ "$TOTAL_FETCHED" -gt 0 ]; then
    python3 -c "
import sys, json

users = json.loads(sys.argv[1])
print(f'  Total users: {len(users)}')

nfc_users = []
for u in users:
    name = u.get('name', '') or f\"{u.get('first_name', '')} {u.get('last_name', '')}\"
    email = u.get('user_email', '') or u.get('email', '')

    tokens = []
    for card in u.get('nfc_cards', []):
        if isinstance(card, dict):
            t = card.get('token') or card.get('card_id') or card.get('uid', '')
            if t: tokens.append(t)
        elif isinstance(card, str):
            tokens.append(card)

    if tokens:
        nfc_users.append({'name': name.strip(), 'email': email, 'tokens': tokens})

print(f'  Users with NFC cards: {len(nfc_users)}')
print()
for u in nfc_users[:20]:
    tag = ', '.join(u['tokens'])
    email = f\" ({u['email']})\" if u['email'] else ''
    print(f\"    {u['name']}{email}  ->  NFC: {tag}\")
if len(nfc_users) > 20:
    print(f'    ... and {len(nfc_users)-20} more')
" "$ALL_USERS" 2>/dev/null || echo "  (could not parse users response)"
fi

# ──────────────────────────────────────────────────────────
header "2. Redpoint HQ API (${REDPOINT_URL})"
# ──────────────────────────────────────────────────────────

GQL="${REDPOINT_URL}/api/graphql"

echo -e "\n${YELLOW}Testing: GraphQL gates query${NC}"
GATES=$(curl -s --connect-timeout 10 -X POST "$GQL" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer ${REDPOINT_KEY}" \
    -H "X-Redpoint-HQ-Facility: ${REDPOINT_FACILITY}" \
    -d '{"query":"{ gates(first: 50, filter: {active: ACTIVE}) { edges { node { id name active facility { shortName longName } } } } }"}' 2>&1) || true

if echo "$GATES" | python3 -c "import sys,json; d=json.load(sys.stdin); assert 'data' in d" 2>/dev/null; then
    pass "Redpoint GraphQL API is reachable and authenticated"
    echo "$GATES" | pp
    python3 -c "
import sys, json
d = json.load(sys.stdin)
edges = d.get('data', {}).get('gates', {}).get('edges', [])
print(f'  Found {len(edges)} gate(s):')
for e in edges:
    n = e['node']
    fac = n.get('facility', {}).get('shortName', '?')
    print(f\"    ID: {n['id']}  Name: {n['name']}  Facility: {fac}\")
" <<< "$GATES" 2>/dev/null
    echo ""
    info "Copy a gate ID above and set it as REDPOINT_GATE_ID in your .env"
elif echo "$GATES" | python3 -c "import sys,json; d=json.load(sys.stdin); assert 'errors' in d" 2>/dev/null; then
    fail "Redpoint API returned errors:"
    echo "$GATES" | pp
else
    fail "Redpoint API unreachable or auth failed"
    echo "  Response: ${GATES:0:300}"
fi

echo -e "\n${YELLOW}Testing: GraphQL customers query (first page)${NC}"
CUSTS=$(curl -s --connect-timeout 10 -X POST "$GQL" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer ${REDPOINT_KEY}" \
    -H "X-Redpoint-HQ-Facility: ${REDPOINT_FACILITY}" \
    -d '{"query":"{ customers(filter: {active: ACTIVE}, first: 5) { pageInfo { hasNextPage endCursor } edges { node { id firstName lastName email externalId badge { status customerBadge { name } } } } } }"}' 2>&1) || true

if echo "$CUSTS" | python3 -c "import sys,json; d=json.load(sys.stdin); assert 'data' in d" 2>/dev/null; then
    pass "Customer query works"
    python3 -c "
import sys, json
d = json.load(sys.stdin)
edges = d.get('data', {}).get('customers', {}).get('edges', [])
pi = d.get('data', {}).get('customers', {}).get('pageInfo', {})
print(f'  Showing first {len(edges)} active customers (hasNextPage: {pi.get(\"hasNextPage\", \"?\")}):')
print()
for e in edges:
    c = e['node']
    badge = c.get('badge', {}) or {}
    status = badge.get('status', 'N/A')
    plan = (badge.get('customerBadge') or {}).get('name', 'N/A')
    ext = c.get('externalId', '')
    email = c.get('email', '')
    print(f\"    {c['firstName']} {c['lastName']}  ({email})\")
    print(f\"      Badge: {status} ({plan})  ExternalID: '{ext}'\")
" <<< "$CUSTS" 2>/dev/null
else
    fail "Customer query failed"
    echo "  Response: ${CUSTS:0:300}"
fi

# ──────────────────────────────────────────────────────────
header "3. Summary"
# ──────────────────────────────────────────────────────────

echo ""
echo "  Next steps:"
echo "    1. Verify UniFi users with NFC cards appear above"
echo "    2. Note your gate ID from the Redpoint gates list"
echo "    3. Check if customer notes contain NFC tag UIDs"
echo "    4. Run the bridge with: go run ./cmd/bridge"
echo "    5. Hit: curl http://localhost:3500/ingest/unifi  (dry run)"
echo ""
echo "  If APIs failed, check:"
echo "    - UniFi: is Developer API enabled in Access settings?"
echo "    - Redpoint: is the API key valid? is the facility code right?"
echo ""
