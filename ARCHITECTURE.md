# Mosaic Climbing – UniFi Access + Redpoint HQ Check-in Bridge

A single Go binary that connects your **G2 Pro reader + UA-Hub** to **Redpoint HQ**. Members tap their NFC card at the door and walk in — the bridge validates their membership and records the check-in automatically.

Compiles to a **5 MB binary** with zero runtime dependencies. Runs directly on your **UDM Pro** — no extra hardware needed.

---

## Architecture

```
┌──────────────────┐    WebSocket (wss)     ┌───────────────────┐    GraphQL (POST)    ┌──────────────┐
│                  │  ◄────────────────────  │                   │  ──────────────────►  │              │
│  G2 Pro Reader   │   NFC tap events       │   mosaic-bridge   │   customerByExtId     │  Redpoint HQ │
│       +          │                        │   (Go binary)     │   createCheckIn       │  GraphQL API │
│  UA-Hub          │  ────────────────────► │                   │                       │              │
│                  │   PUT /doors/:id/unlock│   :3500 admin API │  ◄──────────────────  │              │
└──────────────────┘    REST API            └───────────────────┘   Customer + Badge     └──────────────┘
```

### Check-in Flow

```
1. Member taps NFC card on G2 Pro reader
2. UA-Hub → WebSocket event → bridge (credential_id, door_id, auth_type: "NFC")
3. Bridge resolves card UID → Redpoint customer:
     external_id  →  customerByExternalId(externalId: cardUID)
     barcode      →  passes cardUID as barcode to createCheckIn
     lookup       →  local JSON map → customer(id: mappedId)
4. Validates badge status: ACTIVE → proceed, FROZEN/EXPIRED → deny
5. createCheckIn(gateId, customerId) → records the visit in Redpoint
6. PUT /doors/{id}/unlock → door opens for 5 seconds
7. Member walks in ✓
```

---

## Why Run on the UDM Pro

The bridge runs directly on your UDM Pro inside a lightweight container. This is the most reliable option because:

- The UDM Pro is **already always-on** and designed for 24/7 operation with a real SSD
- It's on the **same device** managing the UA-Hub, so the WebSocket connection is localhost — zero network hops, zero latency
- **No extra hardware** to buy, power, or maintain
- The `/data` partition **survives firmware updates**, so your bridge and its config persist
- If your internet goes down, NFC events still flow locally (Redpoint calls queue until connectivity returns)

UniFi OS 3.x+ removed Docker/Podman support, but supports **systemd-nspawn** containers — a lightweight alternative that's built into the kernel. The deploy scripts handle all of this for you.

---

## Hardware

| Component | Model | Role |
|-----------|-------|------|
| Reader | **G2 Pro** | NFC reader + intercom (camera, 2-way audio) |
| Controller | **UA-Hub** | Wired to electric strike/mag lock, exposes the API |
| Console | **UDM Pro** | Hosts UniFi Access + runs the bridge service |

---

## Redpoint HQ API

**Endpoint:** `https://{ORG}.rphq.com/api/graphql` (POST)
**Auth:** `Authorization: Bearer {token}` + `X-Redpoint-HQ-Facility: {3-letter-code}`
**Docs:** https://portal.redpointhq.com/docs/api/v1/

Key operations the bridge uses:

- **`customerByExternalId`** — look up member by NFC card UID (stored as externalId)
- **`createCheckIn`** — record the visit (gateId + customerId). Returns `CreateCheckInResult`, `DuplicateCheckInResult` (within 15s), or `CreateCheckInCustomerNotFound`
- **`gates`** — discover your gate ID during setup
- **Badge status:** `ACTIVE` / `FROZEN` / `EXPIRED`

---

## Project Structure

```
mosaic-checkin-bridge/
├── .env.example          ← copy to .env, fill in credentials
├── go.mod / go.sum       ← Go module deps (just gorilla/websocket)
├── Makefile              ← build / cross-compile targets
├── Dockerfile            ← multi-stage: builds to scratch (~8MB image)
├── ARCHITECTURE.md       ← this file
│
├── cmd/bridge/
│   └── main.go           ← entrypoint, wiring, graceful shutdown
│
├── internal/
│   ├── config/config.go       ← loads .env + env vars
│   ├── unifi/client.go        ← WebSocket listener + REST door control
│   ├── redpoint/client.go     ← GraphQL client (queries + mutations)
│   ├── cardmap/mapper.go      ← NFC card ↔ customer resolution
│   ├── checkin/handler.go     ← core orchestration logic
│   └── api/server.go          ← local admin HTTP API
│
├── deploy/udm-pro/
│   ├── setup-container.sh     ← creates the nspawn container on UDM Pro
│   ├── install.sh             ← installs the bridge inside the container
│   └── on-boot.sh             ← persists across firmware updates
│
└── data/
    └── card_map.json     ← auto-created if using "lookup" strategy
```

---

## Setup

### 1. Generate a UniFi Access API Token

UniFi console → **Access → Settings → Developer API → Create Token**

### 2. Configure

```bash
cp .env.example .env
# Edit with your values
```

### 3. Build & Run

```bash
# Build for your current platform
go build -o mosaic-bridge ./cmd/bridge

# Or cross-compile for a Raspberry Pi
GOOS=linux GOARCH=arm64 go build -o mosaic-bridge ./cmd/bridge

# Run
./mosaic-bridge
```

### 4. Find your Redpoint Gate ID

```bash
curl http://localhost:3500/gates
```

Copy the `id` for your entrance gate into `.env` as `REDPOINT_GATE_ID`.

### 5. Link NFC cards to customers

Store each member's NFC card UID as their `externalId` in Redpoint:

```graphql
mutation {
  updateCustomer(input: { id: "cust-id", externalId: "AB12CD34" }) {
    ... on UpdateCustomerResult { recordId }
  }
}
```

### 6. Verify

```bash
curl http://localhost:3500/health          # service status
curl http://localhost:3500/doors           # UniFi doors
curl http://localhost:3500/customer/AB12CD34   # test lookup
curl -X POST http://localhost:3500/test-checkin \
  -H "Content-Type: application/json" \
  -d '{"cardUid":"AB12CD34"}'             # simulate check-in (devhooks build only)
```

---

## NFC Card Strategies

| `CARD_MAPPING=` | How it works | Best for |
|-----------------|-------------|----------|
| `external_id` | Card UID = customer's externalId in Redpoint | New card rollouts (recommended) |
| `barcode` | Card UID = customer's Redpoint barcode | Barcodes already on NFC cards |
| `lookup` | Local JSON maps card UIDs → customer IDs | Pre-existing cards |

---

## Admin API

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | /health | Service health + UniFi connection |
| GET | /stats | Check-in counts, denials, errors |
| GET | /doors | UniFi doors |
| GET | /gates | Redpoint gates (find your gate ID) |
| GET | /checkins?limit=20 | Recent Redpoint check-ins |
| GET | /customer/{externalId} | Customer lookup + validation |
| GET | /cards | Card mappings (lookup strategy) |
| POST | /cards | Add: `{"cardUid":"...","customerId":"..."}` |
| DELETE | /cards/{cardUid} | Remove mapping |
| POST | /test-checkin | Simulate: `{"cardUid":"..."}` (devhooks build + BRIDGE_ENABLE_TEST_HOOKS=true) |
| POST | /unlock/{doorId} | Manual door unlock |

---

## Deployment on UDM Pro

UniFi OS 3.x+ removed Docker/Podman, but supports **systemd-nspawn** containers — a lightweight isolation mechanism built into the kernel. The bridge runs inside a minimal Debian container on your UDM Pro. All data lives under `/data`, which persists across firmware updates.

Three scripts in `deploy/udm-pro/` handle everything:

### Step 1: Create the container

SSH into your UDM Pro and run the setup script. This creates a minimal Debian Bookworm container (~390MB) under `/data/custom/machines/`. Takes about 10 minutes on first run.

```bash
# From your dev machine
scp deploy/udm-pro/setup-container.sh root@<UDM-IP>:~/
ssh root@<UDM-IP> bash ~/setup-container.sh
```

### Step 2: Build and deploy the bridge

Cross-compile the Go binary for ARM64 (the UDM Pro's architecture), then copy it and your `.env` into the container:

```bash
# On your dev machine
GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o mosaic-bridge ./cmd/bridge
# or: make build-pi

# Copy to the UDM Pro container filesystem
scp mosaic-bridge root@<UDM-IP>:/data/custom/machines/mosaic-bridge/opt/mosaic-bridge/
scp .env root@<UDM-IP>:/data/custom/machines/mosaic-bridge/opt/mosaic-bridge/
scp deploy/udm-pro/install.sh root@<UDM-IP>:/data/custom/machines/mosaic-bridge/opt/mosaic-bridge/
```

### Step 3: Install the systemd service

Run the install script inside the container. This creates a systemd service that starts automatically and restarts on failure:

```bash
ssh root@<UDM-IP> machinectl shell mosaic-bridge /bin/bash /opt/mosaic-bridge/install.sh
```

### Step 4: Survive firmware updates

Copy the on-boot script to `/data/on_boot.d/`. This re-establishes the container plumbing after any UDM Pro firmware update (packages in `/usr` and symlinks in `/var` get wiped, but everything in `/data` persists):

```bash
scp deploy/udm-pro/on-boot.sh root@<UDM-IP>:/data/on_boot.d/05-mosaic-bridge.sh
ssh root@<UDM-IP> chmod +x /data/on_boot.d/05-mosaic-bridge.sh
```

This requires the [unifios-utilities on-boot-script](https://github.com/unifi-utilities/unifios-utilities). If you don't have it installed yet, follow their README — it's a one-time setup.

### Managing the service

```bash
# SSH into the UDM Pro, then into the container
ssh root@<UDM-IP>
machinectl shell mosaic-bridge

# Inside the container:
systemctl status mosaic-bridge     # check status
journalctl -u mosaic-bridge -f     # follow logs
systemctl restart mosaic-bridge    # restart (e.g. after .env change)

# Or from the UDM Pro host directly:
curl http://localhost:3500/health   # admin API works from the host
```

### Updating the bridge

When you build a new version, just copy the binary and restart:

```bash
GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o mosaic-bridge ./cmd/bridge
scp mosaic-bridge root@<UDM-IP>:/data/custom/machines/mosaic-bridge/opt/mosaic-bridge/
ssh root@<UDM-IP> machinectl shell mosaic-bridge /bin/bash -c "systemctl restart mosaic-bridge"
```

---

## What Happens When Things Go Wrong

The bridge is designed to be self-healing:

- **Bridge crashes:** systemd restarts it within 5 seconds (max 10 retries per minute)
- **UniFi WebSocket drops:** auto-reconnects with exponential backoff (5s → 10s → 20s → ... → 60s max)
- **Redpoint API is down:** check-in recording fails, but the door still unlocks for validated members (fail-open for member experience)
- **UDM Pro reboots:** on-boot script re-establishes the container, systemd starts the bridge
- **Firmware update:** `/data` persists, on-boot script reinstalls nspawn tooling and restarts the container
- **Internet outage:** NFC events still flow locally (UniFi → bridge). Redpoint calls fail gracefully. Door still unlocks for previously-validated auth types
- **Duplicate NFC tap:** Redpoint deduplicates within 15 seconds, bridge still unlocks

---

## Troubleshooting

**WebSocket won't connect:** Check API token, verify UniFi Access is running. Since the bridge runs on the UDM Pro itself, `UNIFI_HOST` should be `127.0.0.1` or `localhost`. Auto-reconnects with backoff.

**Customer not found:** Verify the NFC card UID matches the customer's `externalId`. Use `GET /customer/{uid}` to test.

**Badge FROZEN/EXPIRED:** Member's plan needs attention in Redpoint. Bridge denies and logs the reason.

**Door won't unlock:** Check door ID (`GET /doors`), verify API token has door permissions, check `journalctl -u mosaic-bridge`.

**Gate ID not set:** Run `curl http://localhost:3500/gates`, copy the entrance gate ID to `.env`, restart the service.

**Container won't start after firmware update:** Make sure the on-boot script is in `/data/on_boot.d/` and executable. Run it manually: `bash /data/on_boot.d/05-mosaic-bridge.sh`.
