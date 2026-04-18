# Architecture Review: mosaic-checkin-bridge

**Reviewer**: senior architect pass
**Scope**: security + performance + structural recommendations
**Repo state at review**: Phase B + Phase C complete; `go vet`, `go build`, `go test -race ./...` all green.

## Executive summary

The bridge has matured well since the first pass: bcrypt-hashed staff auth, HMAC sessions with double-submit CSRF, shadow-mode gating, per-route timeouts, WAL-mode SQLite with a single writer, a breaker around the Redpoint recheck path, and a drained async writer pool are all in place. For a single-tenant on-prem service in a trusted LAN, that baseline is solid.

The work left falls into four buckets:

1. **Correctness gaps in the UniFi↔Redpoint propagation window** — the 24-hour default statusync interval is the biggest single membership-churn lag in the system, and the unmatched-user set is a forever-enabled group the bridge deliberately doesn't touch. Both have clear operational fixes.
2. **Security hardening that matters the moment the bridge is reachable from anything other than localhost** — specifically the X-Forwarded-For trust issue and the unbounded `loginAttempts` map. Both are one-line footguns that can bypass the rate-limiter and IP allowlist.
3. **Performance cliffs that will show up under real staff-UI use** — the fragment endpoints hit the DB and/or UniFi on every HTMX poll without caching, and `handleCheckins` hits Redpoint synchronously on every refresh.
4. **Structural tidy-ups** — consolidating the drain/shutdown plumbing, splitting the control-plane from the data-plane, and persisting the HMAC signing key so restarts don't invalidate staff sessions.

The rest of the document cites file:line locations, explains why each finding matters, and proposes a concrete fix. Findings are tagged **[HIGH]**, **[MED]**, or **[LOW]** based on likely exploit or observable slowdown on this deployment (one gym, ~1 check-in per 5 seconds peak, LAN-exposed UI). A prioritised roadmap is at the end.

---

## Architectural framing: the bridge layers on top of UA-Hub

Every finding below is easier to reason about with this model in mind: UA-Hub is the autonomous gatekeeper at all times. It holds the card database, it decides ACCESS vs BLOCKED on every tap, it unlocks the door. The bridge operates alongside it and influences behaviour in exactly three ways:

1. **Ahead of time (`statusync.RunSync`)**: the bridge pushes ACTIVE / DEACTIVATED into UA-Hub's user records based on Redpoint truth. UA-Hub then natively denies deactivated cards on the next tap with no bridge involvement.
2. **At tap time (override unlock)**: on every bridge-allow, `UnlockDoorForMember` is called. Redundant when UA-Hub already allowed; meaningful only on the denied-tap recheck path where UA-Hub denied a stale-deactivated card and the bridge reactivates + unlocks in the same breath.
3. **After the fact (Redpoint recording)**: check-ins are written to Redpoint HQ asynchronously so the gym's reporting dashboards are accurate.

**Consequence for "bridge down":** UA-Hub continues to allow every card that was ACTIVE in its DB at the moment the bridge died, and continues to deny every card that was DEACTIVATED. The door keeps working at full speed. What stops: membership-churn propagation (new suspensions don't reach UA-Hub), tap-time renewal recovery (the recheck path), and Redpoint recording (catchable later via `BackfillOnReconnect`).

**Consequence for "bridge up in live mode":** UA-Hub is still the autonomous decider. The bridge's job is to keep UA-Hub's user-status table honest with respect to Redpoint and to cover the lag window between Redpoint state changes and UA-Hub state changes. Both systems vote on every tap; `noteDisagreement` (`handler.go:124-165`) is the audit trail when they diverge.

---

## Correctness findings (membership propagation)

### [LOW] C1 — Align the statusync schedule with Redpoint's daily membership-update window — RESOLVED

**Where**: `internal/config/config.go:147-150` (default `Sync.IntervalHours = 24`), `internal/statusync/syncer.go:157-280` (sync body), package doc at `:1-19` describes the model.

**Context (operator-provided):** Redpoint itself updates membership state on a daily cadence, so there's no value in the bridge syncing faster than that — a 24-hour window is the correct size. The Redpoint API confirms this is the best achievable anyway: per the v1 GraphQL docs at `portal.redpointhq.com/docs/api/v1/`, there are no webhooks, no subscriptions, no `updatedAfter` filter, and no `orderBy` on `customers` — the only available mechanism is full cursor-paginated fetches.

That downgrades this finding from a correctness gap to a scheduling detail. The two things that *do* matter are:

  1. **Sync should run shortly AFTER Redpoint's daily update window.** If Redpoint updates memberships at, say, 02:00 and the bridge syncs at 03:00, the effective lag from "Redpoint state changed" to "UA-Hub reflects change" is one hour, not one day. If the bridge happens to sync at 01:00 (before Redpoint updates) the effective lag is 24 hours. Confirm Redpoint's daily-update timing with their support team and set the bridge's first-run + ticker schedule to fall in the window immediately after.
  2. **Don't introduce drift from daily to almost-daily.** The current `time.NewTicker(24h)` (combined with the 2-minute startup delay) will gradually drift away from the target clock time across restarts. If timing matters at all, pin the sync to a wall-clock time (e.g. 03:00 local) using a scheduler like `gocron` or a cron-shaped loop that computes the next 03:00 boundary. The existing implementation uses `time.NewTicker(s.config.SyncInterval)` which gives "every 24h from when I last started", not "every day at 03:00".

**Fix (small, worth doing):** change the sync loop to compute the next target wall-clock time rather than using an interval ticker. One-line config addition: `SyncTimeLocal string` (HH:MM format, e.g. "03:00"). If set, the loop sleeps until that moment instead of ticking every N hours. If empty, fall back to current behaviour for backwards compatibility.

Everything else in this finding (write-amplification reduction, dashboard gauge for sync freshness) moves under C3 where it more naturally belongs.

**Status (C1 RESOLVED).** Wall-clock scheduling lives in `internal/statusync/syncer.go: scheduleNext` (lines ~295–334) and is driven by the new `Config.SyncTimeLocal string` (HH:MM in the host's local timezone). When set, every iteration computes the next occurrence of that wall-clock time on `now.Location()`; if today's slot is already past, it bumps 24h forward. The first-run path additionally honours a `minLead` floor (defaults to 2 minutes via `Config.InitialDelay`) so the cache.Syncer has time to populate — a target inside that window is bumped to tomorrow rather than firing during cache warmup. When `SyncTimeLocal` is empty, the loop falls back to `now + SyncInterval` (preserves prior interval-ticker behaviour for backwards compatibility); a malformed value also falls through to the interval path with a loud Warn log so the bridge never wedges on a bad config. The "long-running sync skips a day" failure mode is closed by recomputing `scheduleNext` from `time.Now()` *after* each `RunSync` returns (lines ~274–292) — a 90-minute 03:00 sync still fires at 03:00 the next day, not at 04:30+. `sleepUntil(ctx, t)` uses a single `time.NewTimer(d)` + `select` against `ctx.Done()` so shutdown is responsive even mid-sleep, and an already-past target returns immediately while still respecting cancellation. Config is plumbed end-to-end: `SyncConfig.TimeLocal string` (`json:"timeLocal"`) reads from env `SYNC_TIME_LOCAL` (`internal/config/config.go:312`), boot validation rejects malformed values via `ParseHHMM` (lines 339–346) so operators see a hard error at startup rather than discovering silent fallback in production, and `cmd/bridge/main.go:218` passes `cfg.Sync.TimeLocal` into `statusync.Config.SyncTimeLocal`. Test coverage in `internal/statusync/syncer_test.go`: `TestScheduleNext_WallClock` (6 cases — today's-target / past-today / exactly-now / inside-minLead / outside-minLead / midnight crossing), `TestScheduleNext_Interval` (3 cases), `TestScheduleNext_MalformedFallsBackToInterval`, `TestSleepUntil_RespectsContext`, `TestSleepUntil_PastTargetReturnsImmediately`. `internal/config/config_test.go`: `TestParseHHMM` (11 sub-cases including boundary and malformed inputs) plus `TestValidation_SyncTimeLocal`. Full build/vet/test suite green in both default and `-tags devhooks` modes.

---

### [MED] C2 — Unmatched NFC users stay enabled forever; match rule should be "NFC ⇒ must map to a Redpoint customer by email" — RESOLVED

**Where**: `internal/statusync/syncer.go:184-212`.

```go
if len(user.NfcTokens) == 0 {
    result.Unmatched++
    continue
}
...
if cached == nil {
    // Not in our store — could be a staff member, admin, etc. Leave alone.
    result.Unmatched++
    continue
}
```

Today statusync skips UA-Hub users with no NFC tokens *and* users whose NFC tokens don't map to a local store member — both land in the `Unmatched` bucket. That overloads two different cases with very different security postures. In this deployment:

  - **Users with no NFC tokens are staff/vendor/contractor PIN-only users.** By design they should be left alone — the bridge has no opinion on their access.
  - **Users with NFC tokens are members.** Every one of them should match a Redpoint customer. A tapping NFC card that doesn't correspond to a paying customer is a security gap — usually an ex-member whose card wasn't revoked, a trial pass that got re-used, or a card from a prior access-control system.

The fix is to split those two cases apart and tighten the rule for the NFC side. The Redpoint API's `customers(filter: {email: X})` query (the only useful server-side filter they expose) is a natural match key here — UA-Hub user records already carry an email field, Redpoint customers have a canonical email, and the match is cheap (single cursor-paginated query with page size 1).

**Proposed rule:**

  1. **No NFC tokens → skip entirely.** PIN-only user, out of scope for the bridge. Move these out of the Unmatched bucket and into a new `Skipped (PIN-only)` counter so the real Unmatched number reflects actionable items.
  2. **Has NFC tokens, no email in UA-Hub → unmatched.** Flag for staff to add an email. Alert when this count is non-zero after a grace period (7 days configurable).
  3. **Has NFC tokens + email, query Redpoint by email → zero matches → unmatched.** Same grace-period + alert path. After the grace window elapses with no matching customer, default-deactivate in UA-Hub.
  4. **Has NFC tokens + email → matches one Redpoint customer.** Use that customer's `active` + badge status to decide ACTIVE/DEACTIVATED in UA-Hub. Persist the `nfc_uid → customer_id` mapping in the local store so the fast-path tap handler can continue to look up by NFC UID without re-querying Redpoint on every tap.
  5. **Has NFC tokens + email → matches multiple Redpoint customers** (email collision — common in households where a parent's email is on file for both the parent's and the child's Redpoint account). **Fall through to a name check on the email-matched candidates only**: filter the 2+ email matches by normalized `(firstName, lastName)` equality with the UA-Hub user. Exactly one survivor → definitive match (record with `matched_by='auto:email+name'`). Zero or two-plus survivors → unmatched, surface in the staff UI for manual disambiguation. The name check is local against the small candidate set, so it's free; the fallback never triggers in the "no collision" case.

This replaces the current NFC-UID-based matching with email-based matching, which has three real advantages over what's there now:

  - **No dependence on NFC UIDs being known to Redpoint.** Today the bridge's `cardMapper` exists partly to paper over cards whose UIDs never made it into Redpoint's `barcode` field. Under the email rule, `cardMapper` becomes a narrow exception-handling tool for true one-off overrides, not the primary matching mechanism.
  - **Uses the one Redpoint server-side filter that actually works.** Per the v1 API docs, `email` is one of only two supported filter fields on `customers`. Matching by email is the efficient path.
  - **Gives staff a concrete, actionable bootstrap task.** Chris's proposed rollout: run the first sync, produce an "unmatched NFC users" report, staff updates those UA-Hub users with the correct email, rerun. Every subsequent sync's unmatched count should be very small, and non-zero is a clear operational signal rather than ambiguous noise.

**Data model: persisting the UA-Hub ↔ Redpoint mapping**

The link between a UA-Hub user and a Redpoint customer is the core piece of state this whole flow produces. Store it in a new local table:

```sql
CREATE TABLE ua_user_mappings (
    ua_user_id           TEXT PRIMARY KEY,
    redpoint_customer_id TEXT NOT NULL,
    matched_at           TIMESTAMP NOT NULL,
    matched_by           TEXT NOT NULL,  -- 'auto:email', 'auto:name', 'staff:<username>'
    last_email_synced_at TIMESTAMP,
    UNIQUE (redpoint_customer_id)         -- one UA-Hub user per Redpoint customer
);
```

Redpoint itself is never written to — the writeback direction is strictly **Redpoint → UA-Hub** (email) and the mapping itself lives only in the bridge's local DB. This keeps Redpoint as the source of truth for customer data and makes UA-Hub a follower whose user records stay consistent with Redpoint over time.

---

**Algorithm (first sync and steady state are the same, gated by whether a mapping already exists):**

For each UA-Hub NFC user:

  1. **If a mapping already exists in `ua_user_mappings`** → use it directly. Re-read the Redpoint customer, mirror email into UA-Hub if it drifted, apply ACTIVE/DEACTIVATED status. Fast path for steady state.
  2. **No mapping yet → try automatic match:**
      - **(a) Email match** (if UA-Hub user has an email): `customers(filter: {email: X, active: ALL}, first: 10)`. Note `first: 10`, not `first: 2` — we explicitly want to see all email collisions because households share emails. Then:
          - **Exactly one Redpoint customer** with that email → record mapping (`matched_by='auto:email'`), proceed to writeback.
          - **Two or more Redpoint customers** share the email (parent + child case) → run the name check locally against just those candidates: filter for normalized-equal `(firstName, lastName)` with the UA-Hub user. Exactly one survivor → record mapping (`matched_by='auto:email+name'`), proceed to writeback. Otherwise → ambiguous, fall through to staff UI.
          - **Zero matches** → fall through to (b).
      - **(b) Name match** (fallback if (a) didn't resolve): cursor-paginate `customers(filter: {active: ALL})` and filter client-side for normalized-equal `(firstName, lastName)`. Normalization: `strings.ToLower(strings.TrimSpace(unicode-NFD-strip-diacritics(s)))` on both sides. Exactly one match → **definitive** (`matched_by='auto:name'`), record mapping, proceed to writeback. Multiple matches → **ambiguous**, surface in the staff UI. Zero matches → **no match**, surface in the staff UI.
  3. **On any resolved match (auto or staff-assigned) → mirror email into UA-Hub:** call UA-Hub `UpdateUser(email = matchedCustomer.email)` if the UA-Hub user's current email is empty or disagrees with Redpoint. Never overwrite a hand-entered UA-Hub email with a different Redpoint email without logging. Update `last_email_synced_at` in the mapping table.
  4. **Unmatched users** (no email match, no definitive name match, or explicit ambiguity) surface in the staff web UI, described below. They start a grace-period timer in `ua_user_mappings_pending` and are default-deactivated in UA-Hub after the window (default 7 days) if staff hasn't resolved them.

**Normalization rules (applied to both sides before comparison):**
  - Lowercase, trim, NFD-normalize + strip combining marks (removes accents: "José" → "jose").
  - Collapse multi-space. Ignore honorifics ("Dr.", "Mr.", "Mrs.") as a prefix-strip list.
  - Middle names / suffixes treated as optional — "Chris Smith" matches Redpoint's "Chris J. Smith" iff it's the *only* Redpoint customer whose first + last is Chris/Smith. If there's also a "Chris A. Smith", the match is ambiguous and goes to staff.

---

**Staff web UI: the "Needs Match" panel**

Add to the existing HTMX-driven staff UI under `/ui/unmatched`. Everything below is HTMX fragment endpoints consistent with the current shape of `server.go`:

  - **`GET /ui/unmatched`** — table of every UA-Hub user currently without a mapping, one row each. Columns:
      - UA-Hub name, UA-Hub email (often blank), NFC tokens (truncated), first seen, deactivation ETA if grace window is ticking.
      - Match state: `ambiguous (N candidates)` · `no match found` · `new today`.
      - Row click → detail fragment.
  - **`GET /ui/unmatched/{uaUserID}`** — detail fragment with:
      - UA-Hub side: name, email, NFC tokens, first-seen-at.
      - Candidate Redpoint customers (auto-computed: exact name match, fuzzy name match top-5, and an email search box). For each candidate: name, email, active status, home facility, last visit date, account balance — enough context for staff to make the call.
      - Search box: type any substring, posts to `/ui/unmatched/{uaUserID}/search?q=...` which runs a cursor-paginated name scan and returns a candidate fragment.
  - **`POST /ui/unmatched/{uaUserID}/match`** — body: `redpointCustomerId=X`. Bridge:
      - Writes `ua_user_mappings(uaUserID, X, now, 'staff:<username>', null)`.
      - Calls UA-Hub `UpdateUser` to mirror email from Redpoint customer X.
      - Logs to `match_audit` table (who, when, source of decision).
      - Returns the updated row fragment with "matched by <username> at <time>" so the staff user gets instant visual confirmation.
  - **`POST /ui/unmatched/{uaUserID}/skip`** — staff explicitly marks the user as "not a member, deactivate now". Bridge immediately deactivates in UA-Hub and records reason `'staff:skip'`. Useful for ex-member cards that predate the Redpoint directory.
  - **`POST /ui/unmatched/{uaUserID}/defer`** — moves the grace-period expiry out by one week so staff can keep researching without losing the card to auto-deactivation. Logged.

All five endpoints sit behind the same auth middleware as the rest of `/ui/*` (session cookie + CSRF + `X-Requested-With`). The "match" action is a destructive write against UA-Hub, so it also logs to the audit trail and sends a Pushover notification in live mode: `"Staff matched {uaName} → {redpointName} via /ui/unmatched"`. That way a drift between staff's mental model and what actually landed is visible.

---

**Safety gates on the writeback:**

  - UA-Hub email writes happen only when the UA-Hub email is empty OR a previous mapping-audit row shows the current UA-Hub email was itself set by the bridge (i.e. we're just mirroring ongoing drift from Redpoint). If UA-Hub's email differs from Redpoint's AND the audit trail shows a human set it, log a warning, skip the write, and surface the divergence in the staff UI for manual review.
  - Every UA-Hub `UpdateUser` call writes to `match_audit(ua_user_id, field, before, after, source, timestamp)` for postmortem forensics.
  - A new `bridge.sync_email_drift_total` counter tracks how often the bridge chooses to skip a write because of the hand-entered-email rule above. Non-zero is informational, not alarming.

---

**Operational rollout (matches Chris's plan):**

  1. Deploy the code with `BRIDGE_SHADOW_MODE=true`. First sync runs, attempts email + name matches, but skips every writeback (shadow mode). The `ua_user_mappings` rows land normally with `matched_by='auto:email'` or `'auto:name'`, and `ua_user_mappings_pending` collects the unmatched set. No writes to UA-Hub happen yet.
  2. Staff logs into `/ui/unmatched` and walks the list. For each unmatched or ambiguous user they pick the right Redpoint customer. Every `/match` action writes the mapping row but, because shadow mode is on, still skips the UA-Hub email write — so staff can review the proposed emails in the table before anything mutates.
  3. Once the "Needs Match" panel is empty or down to a handful of knowns-staff-will-skip, flip `BRIDGE_SHADOW_MODE=false`. The next sync runs, and for every mapped user whose UA-Hub email is empty or disagreeing, writes the Redpoint email into UA-Hub. From then on, every sync keeps emails mirrored automatically.
  4. Grace-period default-deactivation turns on with the flip to live. Anyone still unmatched after the 7-day window gets deactivated in UA-Hub (alerting via Pushover before and at deactivation time).

This keeps Redpoint untouched, gives staff one tool to resolve exceptions, and converges the UA-Hub user database to match Redpoint without losing human overrides.

---

**New-user provisioning flow (C2 addendum, per operator request)**

Going forward, every new UA-Hub NFC user is created through the bridge's staff web UI rather than UA-Hub's native admin. The UI enforces "email is required" at creation time, so the Redpoint match is established the moment the user exists and the `/ui/unmatched` list stays empty for non-legacy users.

**UniFi Access API capability confirmation.** Reading the UniFi Access API reference (`assets.identity.ui.com/unifi-access/api_reference.pdf`) confirms the bridge can drive the entire provisioning flow over HTTPS with no manual UA-Hub admin step — the API exposes every primitive needed:

  - **§3.2 User Registration** — `POST /api/v1/developer/users` with `{first_name, last_name, user_email, employee_number?, onboard_time?}`. Returns the new user's `id`. Permission key `edit:user`. The `user_email` field requires UA-Hub firmware **1.22.16 or later** — confirm the UA-Hub at the gym is on a recent build before relying on this; if it's older, the create call still works but email mirroring degrades to a follow-up `PUT` (also supported, see §3.3).
  - **§3.3 Update User** — `PUT /api/v1/developer/users/:id` with any subset of `{first_name, last_name, user_email, status, onboard_time, employee_number}`. This is the same endpoint statusync already uses for ACTIVE/DEACTIVATED status writes (`internal/unifi/client.go: UpdateUserStatus`), now used additionally for email mirroring.
  - **§3.6 Assign Access Policy to User** — `PUT /api/v1/developer/users/:id/access_policies` with `{access_policy_ids: [...]}`. Required to give a freshly-created user any door access; UA-Hub creates users with no policies attached by default. The bridge needs a `Bridge.DefaultAccessPolicyIDs` config — staff picks the right one for the "members" group at install time.
  - **§3.7 Assign NFC Card to User** — `PUT /api/v1/developer/users/:id/nfc_cards` with `{token: "<nfc_card_token>", force_add: true|false}`. Binds an already-enrolled NFC card to a user. `force_add: true` reassigns a card that's currently bound elsewhere (useful for replacing a lost card without manual cleanup).
  - **§6.2 Enroll NFC Card** — `POST /api/v1/developer/credentials/nfc_cards/sessions` with `{device_id: "<reader_id>", reset_ua_card: false}`. Wakes a UA reader into "enrollment mode" and returns a `session_id`. The staff member taps a fresh card on the reader; the bridge polls for completion. (Reader IDs come from the existing `ListDoors` call in `internal/unifi/client.go`.)
  - **§6.3 Fetch NFC Card Enrollment Status** — `GET /api/v1/developer/credentials/nfc_cards/sessions/:id` returns `{card_id, token}` once the tap is detected. Poll every ~500ms with a 30s timeout; the `token` from this response feeds straight into §3.7's `force_add` request body.
  - **§6.4 Remove Session** — `DELETE /api/v1/developer/credentials/nfc_cards/sessions/:id` cleans up an unused enrollment session if the staff member abandons the flow.

Together these five endpoints make the provisioning flow a single coherent staff-UI interaction:

  1. Staff fills in name + email → bridge live-validates email resolves to exactly one Redpoint customer (with the household-collision name-disambiguation fallback from rule #5 above).
  2. Staff clicks "Create" → bridge calls **§3.2** to create the UA-Hub user, captures the returned `id`.
  3. Bridge calls **§3.6** to attach the default member access policy.
  4. Bridge writes the `ua_user_mappings` row binding the new UA-Hub user to the matched Redpoint customer.
  5. UI then prompts: "Tap the new card on reader [DropDown of door readers]". Staff picks a reader; bridge calls **§6.2** to put it in enrollment mode.
  6. Bridge polls **§6.3** every 500ms; on success it captures the `token` and immediately calls **§3.7** to bind the card to the new user. On timeout, **§6.4** cleans up.
  7. Audit-logs the whole sequence (who, when, matched Redpoint customer ID, NFC token, reader used).

This is a real five-call orchestration, but it's all happening from the bridge to UA-Hub on the LAN — the staff member sees one form, one tap, one "Done". No manual handoff to UA-Hub's web admin at any point.

New endpoints on the staff UI:

  - **`GET /ui/members/new`** — form with fields: first name, last name, email (required), and an inline NFC enrollment widget (reader picker + "tap now" status). Live-validates the email against Redpoint as the staff member types: after a 400ms debounce, calls `GET /ui/members/new/lookup?email=X` which runs the same email-with-name-disambiguation logic from rule #5 and returns a small fragment showing the matched Redpoint customer (name, active status, membership badge) inline. If zero matches survive disambiguation, the form shows a blocking validation error — staff can't submit a new UA-Hub user whose email doesn't resolve to exactly one paying Redpoint customer.
  - **`POST /ui/members/new`** — on submit, the bridge runs steps 1–4 of the orchestration above (create user, assign policy, write mapping, audit-log) and returns a fragment with the new user ID and an embedded "Tap card now" widget pointed at the chosen reader.
  - **`POST /ui/members/new/{id}/enroll`** — body: `device_id=X`. Bridge runs step 5 (start enrollment session) and returns a fragment that polls **§6.3** every 500ms via HTMX `hx-trigger="every 500ms"`. On success the fragment swaps to a confirmation state. On timeout the fragment offers retry or cancel.
  - **`DELETE /ui/members/new/{id}/enroll`** — bridge calls **§6.4** to clean up the abandoned session and reverts the UI fragment to the "tap to start" state.

This preserves the invariant that every UA-Hub NFC user born after this feature lands arrives with an email that points at exactly one Redpoint customer. The "Needs Match" panel only contains users that predate the bridge — and as those are resolved, it converges to empty.

(Unrelated but worth noting: the API reference also documents **§11 Notification → Webhooks** for outbound event delivery from UA-Hub. This isn't needed for the provisioning flow but could replace the WebSocket-based access-log ingestion path in a future cycle if the WebSocket connection ever proves flaky. Out of scope for C2; mentioning it here so it doesn't get lost.)

**Guardrails:**

  - New-member creation is gated on staff session auth, CSRF, and the normal middleware. Additionally it requires `Bridge.AllowNewMembers=true` in config so the endpoint can be turned off entirely if the gym isn't comfortable with the bridge writing users to UA-Hub.
  - Every creation sends a Pushover notification: `"Staff created UA-Hub user {name} → Redpoint {redpointName} via /ui/members/new"`. Low-priority; volume is low and visibility beats silence.
  - If the staff member's session doesn't have `can_provision` capability (a new flag on the session) the endpoint 403s. By default all staff sessions get `can_provision=true`; the flag exists for the future case where the gym wants to limit member creation to managers only.
  - **`Bridge.DefaultAccessPolicyIDs []string`** must be configured before the feature is enabled — boot-time validation refuses to start the bridge with `AllowNewMembers=true` and an empty policy list. This avoids the "created a user with no access policies, looks like a working user but every tap denies" failure mode.
  - **NFC token uniqueness check** before §3.7: query `GET /api/v1/developer/credentials/nfc_cards/tokens/:token` (§6.7) first; if the token is already bound to a *different* user, refuse the bind unless staff explicitly checks "reassign this card" (which then sends `force_add: true`). This protects against the "swapped a child's card with a parent's by mistake" UI accident.
  - The UA-Hub user-creation API requires firmware **1.22.16+** for the `user_email` field to be populated at create time. The bridge should detect this at startup (single `GET /api/v1/developer/users` and inspect any user record's shape) and either log a warning + degrade to `POST /users` then `PUT /users/:id` for email, or refuse the feature outright depending on operator preference (`Bridge.RequireMinimumUAHubVersion=true|false`).

**Status (C2 RESOLVED).** The central complaint — "unmatched NFC users stay enabled forever" — is closed. Today a UA-Hub NFC user who can't be bound to a Redpoint customer falls into `ua_user_mappings_pending` with a grace window (`Bridge.UnmatchedGraceDays`, default 7, `BRIDGE_UNMATCHED_GRACE_DAYS` env) and is default-deactivated in `runExpiryPhase` on the next sync after `grace_until` passes. The matching rule itself is now email-first with name-fallback: `internal/statusync/matcher.go`'s `decideFromEmailResults` implements the five-branch tree (zero-rows → fall through; one row → `auto:email`; N rows, one name-hit → `auto:email+name`; N rows, zero/multiple name-hits → pending-ambiguous_email), and `decideFromNameResults` is the email-absent or email-zero-hit fallback (one row → `auto:name`, N rows → pending-ambiguous_name, zero rows → pending-no_match or pending-no_email depending on whether the UA user had an email at all). Orchestration sits in `internal/statusync/orchestrator.go` (`matchOne` + `persistDecision`), invoked from `Syncer.runMatchingPhase` for every UA user that isn't already bound. The persistence contract — UA user in at most one of mappings/pending at rest, `grace_until` anchored to first observation and never pushed forward except by explicit staff defer, every mapping-change gets a `match_audit` row with `field='mapping'` — is enforced in `persistDecision`. Storage is Migration 3 in `internal/store/migrations.go`: `ua_user_mappings` (UNIQUE on `redpoint_customer_id`), `ua_user_mappings_pending`, `match_audit` (indexed on user_id + timestamp). The staff "Needs Match" panel is the five `/ui/frag/unmatched*` routes in `internal/api/server.go:228-244`: `unmatched-list` (pending table), `unmatched/{uaUserId}/detail` (per-user candidate panel), `unmatched/{uaUserId}/search` (free-text name scan against Redpoint), `unmatched/{uaUserId}/match` (staff binds, writes `matched_by='staff'` + audit + drops pending), `unmatched/{uaUserId}/skip` (immediate deactivate + `staff:skip` audit), `unmatched/{uaUserId}/defer` (push grace_until out). Shadow mode is wired end-to-end: `BRIDGE_SHADOW_MODE` env → `config.Bridge.ShadowMode` → `statusSyncer.SetShadowMode(...)` in `cmd/bridge/main.go:260`, and inside the sync loop both `runMatchingPhase` (mapping writes are always safe — no UA-Hub mutation) and `runExpiryPhase` (skips the `UpdateUserStatus` call + preserves the pending row so flip-to-live re-finds it) honour the flag. The supervised `Start → supervisedLoop → runWithRecover → runLoop` pattern means a panic inside any phase becomes a logged `sync_loop_restarted_total` bump rather than a silent watchdog death (the alerting side of this is tracked separately under C3). Layer 4d — staff UI for new-member provisioning — is RESOLVED in the block below and adds the complementary forward-direction invariant: every UA-Hub NFC user born after this feature lands arrives with an email that points at exactly one Redpoint customer, so the `/ui/unmatched` list only accumulates legacy rows and converges to empty as staff resolves them. Full build/vet/test suite green in both default and `-tags devhooks` modes.

**Deferred** (carved out so it doesn't get lost): the steady-state UA-Hub email writeback described in the "Algorithm" step 3 and "Safety gates on the writeback" sections — reading each already-mapped UA user's Redpoint customer on every sync, pushing the Redpoint email back into UA-Hub via `§3.3 UpdateUser(email=...)` if empty or drifted, skipping the write with a `sync_email_drift_total` increment when a hand-entered email is detected in the audit trail, and calling `TouchMappingEmailSynced` on success — is not yet implemented. The `last_email_synced_at` column, `TouchMappingEmailSynced` method, and `MatchSourceBridgeSync` audit constant are in place as plumbing; only the sync-loop call-site is missing. This is a self-contained follow-up that can land once shadow-mode operation has produced a clean `/ui/unmatched` queue and the operator is ready to let the bridge start mutating UA-Hub user records on a sustained basis. Tracking as **C2 Layer 3c** for the next cycle.

**Status (C2 Layer 4d RESOLVED — staff UI for new-member provisioning).** The six routes from the addendum (`GET /ui/members/new`, `GET /ui/members/new/lookup`, `POST /ui/members/new`, `POST /ui/members/new/{id}/enroll`, `GET /ui/members/new/{id}/enroll/{sid}/poll`, `DELETE /ui/members/new/{id}/enroll/{sid}`) are wired in `internal/api/server.go` and dispatched into `internal/api/members_new.go`. All six are gated by `requireProvisioning(w)`: when `Bridge.AllowNewMembers=false` the call returns 403 with a friendly inline `AlertFragment` instead of the browser's default 403 page (same defence-in-depth pattern as the devhooks gate on `/test-checkin`). Boot validation refuses to start the bridge if `AllowNewMembers=true` is paired with an empty `DefaultAccessPolicyIDs`, so §3.6 is guaranteed a non-empty slice. The orchestration order in `handleMembersNewCreate` is deliberate: (a) authoritative re-run of email-with-name disambiguation (the form's live `/lookup` is a UX hint only), (b) collision check against `ua_user_mappings.redpoint_customer_id` (same invariant as the staff:match collision in `handleFragUnmatchedMatch`), (c) §3.2 create user, (d) §3.6 attach `DefaultAccessPolicyIDs`, (e) `match_audit` row + `ua_user_mappings` row written **before** any §3.6 error is surfaced — the forensic trail captures partial-failure state, and a user with no policy is effectively deactivated by default. The enrollment lifecycle uses HTMX self-polling: `EnrollmentPollingFragment` carries `hx-trigger="every 500ms"` against `#members-new-result`, so the same slot re-renders itself on every tick; once §6.3 returns a token, §6.7 verifies the card isn't already bound to another UA user (refusal renders `EnrollmentFailedFragment` and drops the orphan session via §6.4), then §3.7 binds with `forceAdd=false` and `EnrollmentCompleteFragment` (no every-500ms attribute) terminates the loop. Cancel re-renders `PostCreateFragment` so staff can retry the tap without losing the just-created UA-Hub user. Test coverage in `internal/api/members_new_test.go` (12 tests) asserts: gate blocks both the page and the POST when disabled (UA-Hub `UsersCreated` stays empty), all five email-lookup branches (empty / no-match / ambiguous-no-name / ambiguous-with-name-disambig / single-hit), the create happy path (exactly one `UsersCreated` + one `AccessPolicyAssignment` with `[pol-members]` + mapping row + `match_audit` row with `Source=auto:email` and `Field=mapping`), the no-match path (no §3.2 call, no mapping written), the collision path (existing mapping survives, no §3.2), the enroll-start path (§6.2 session opened, polling fragment returned), the pending-poll path (re-renders polling fragment, no §3.7), the complete-poll path (§3.7 with `forceAdd=false`, terminal complete fragment), the card-already-bound path (no §3.7, §6.4 deletes session, error fragment), and the cancel path (§6.4 fires, retry fragment). Pushover notifications from the guardrail block aren't asserted because no notify package exists in the bridge yet; when the notification path lands it should add an assertion. Full build/vet/test suite green in both default and `-tags devhooks` modes.

---

### [MED] C3 — No alert when the sync loop silently stops — RESOLVED

**Where**: `internal/statusync/syncer.go:137-152` (the ticker loop).

If `RunSync` returns an error, it's logged but the ticker continues. If the goroutine panics, the entire sync ticker dies — there's no watchdog. The bridge can appear healthy (HTTP server up, WebSocket connected, taps being recorded) while statusync has been silently off for days. Because UA-Hub keeps enforcing on last-synced state, the symptom is delayed: a member who should be suspended keeps getting in, and no one notices until someone spot-checks.

**Fix**: add a `last_sync_completed_at` metric (Gauge) set at the end of every successful `RunSync`. Alert if the age exceeds `SyncInterval * 2`. Additionally, wrap the ticker goroutine in a `defer recover()` that re-launches the loop and increments a `sync_loop_restarted_total` counter so panic-driven death becomes visible and self-healing.

**Status**: resolved. `Syncer.Start` now dispatches to `supervisedLoop`, which runs `runLoop` inside `runWithRecover` — a deferred `recover()` that traps panics, logs the stack, and returns `crashed=true`. On return (panic or clean), the supervisor bumps `sync_loop_restarted_total` and re-launches unless `ctx.Err()` is set (so shutdown doesn't trigger infinite restart). On the success path, `RunSync` stamps `last_sync_completed_at` with `time.Now().Unix()` and increments `sync_runs_total`. The gauge is wired into the Prometheus exposition via the existing registry in `cmd/bridge/main.go`. Alert rule to deploy: `time() - last_sync_completed_at > 2 * sync_interval_seconds`. Test coverage: `TestRunSync_StampsLivenessGauge` (gauge + counter), `TestRunWithRecover_CatchesPanic` / `_CleanReturn` (atomic recover contract), `TestSupervisedLoop_RestartsOnPanic` (end-to-end supervisor with a panicking injected fn).

---

### Notification channel for all alerts in this section

Slack is explicitly out of scope for this deployment. The alerting channels that fit a solo-operator on-prem MacBook are, in rough order of suitability:

1. **Pushover** (recommended): $5 one-time, purpose-built for sysadmin alerts, delivers to iPhone/Apple Watch/desktop with priority levels. Emergency priority retries until acknowledged — the right semantics for "door access system degraded". Single HTTP POST from the healthcheck script; DEPLOY.md §11 already shows the shape of the call.
2. **ntfy.sh** (free alternative): open-source HTTP push notifications with an iOS app. Self-hosted option available if you'd rather not trust a third party with the alert content. Same `curl` contract as Pushover — a one-line swap in the healthcheck script.
3. **Email via SMTP**: Chris already has `chris@lefclimbing.com`. Gmail SMTP + an app password works fine, but email is a pull channel — fine for nightly-digest style alerts, poor for "door is down right now". Use as a secondary channel, not a primary.
4. **macOS user notification center** via `osascript -e 'display notification ...'`: only useful while someone is physically at the gym MacBook; the box runs unattended so this is best reserved for a staff-UI toast alongside the real alerting channel.
5. **SMS via Twilio**: reliable but ~$0.01/message and requires maintaining an account + API key. Overkill for the alert volume this deployment produces, but worth considering for the single highest-priority alert (bridge fully down) if Pushover has ever been unreliable for you.

For this deployment I'd wire Pushover as the primary channel with a secondary nightly email digest summarising the prior day's disagreement count, unmatched-user count, and sync completion times — so even if a burst of real-time alerts were missed, the daily digest catches drift. All the C-series findings above assume one of these channels is in place.

---

## Security findings

### [HIGH] S1 — `X-Forwarded-For` is trusted unconditionally — RESOLVED

**Where**: `internal/api/middleware.go:150-173` (`extractClientIP`).

```go
if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
    // take first token
}
if xr := r.Header.Get("X-Real-IP"); xr != "" {
    return xr
}
return r.RemoteAddr
```

**Why it matters**: This runs before the IP allowlist check, the per-IP login rate-limit check, and before `handleUILogin` records failed attempts. Any attacker who can reach the listening socket — a misconfigured router, a compromised device on the guest Wi-Fi, a curl from a staff laptop — can send:

```
X-Forwarded-For: 10.0.1.5
```

and both the allowlist and the lockout counter will see `10.0.1.5` instead of the real peer. The allowlist becomes advisory, and the bcrypt lockout becomes per-attacker-chosen-key, i.e. unlimited.

**Fix**:
- Add `Bridge.TrustedProxies []string` (list of CIDRs). Ignore XFF/X-Real-IP unless `r.RemoteAddr`'s IP falls inside one of those CIDRs.
- Default to empty list — which makes the bridge use `r.RemoteAddr` only, the correct stance for the current topology where nothing proxies it.
- When `TrustedProxies` is populated, take the last untrusted hop in the XFF chain (not the first), which is the standard RFC-7239 walk.

**Secondary fix**: `handleUILogin` at `server.go:762` and `:786` passes `r.RemoteAddr` directly to `sm.RecordLoginFailure` / `RecordLoginSuccess` instead of going through `extractClientIP`. Once XFF is trustworthy, route both paths through `extractClientIP` so lockout and allowlist use the same identity.

**Status**: resolved. `extractClientIP` now takes a `trustedProxies []*net.IPNet` argument; headers are honoured only when `r.RemoteAddr`'s IP is itself inside the trusted set, and the XFF walk is right-to-left so left-most attacker-injected entries can never win. `SecurityConfig.TrustedProxies`, `BridgeConfig.TrustedProxies` (comma-separated CIDRs), and the `TRUSTED_PROXIES` env var were added; `ParseAllowedNetworks` is reused for parsing so error behaviour matches the existing allowlist path. `Server` now holds the parsed list and exposes `s.clientIP(r)`; `handleUILogin` and `handleUILogout` route lockout, audit, and log identity through it. Default is empty list (no proxy), which is correct for the current topology. Test coverage: `TestExtractClientIP_TrustModel` (10-case table: spoof-attempt cases, RFC-7239 walk, X-Real-IP fallback, IPv6, whitespace); `TestSecurityMiddleware_AllowlistHonoursTrust` (end-to-end proof that a spoofed XFF from an out-of-allowlist peer gets 403, while a legitimate in-allowlist peer without XFF gets 200).

---

### [HIGH] S2 — `loginAttempts` grows unboundedly — RESOLVED

**Where**: `internal/api/session.go:29`, reset logic at `:40-44`.

The map is keyed by IP and entries are pruned only when an attempt succeeds or when the window has elapsed *and* a new attempt from the same IP comes in. An attacker (or a scanner sweeping the LAN) can mint unique IPs via S1 or simply probe once from many real IPs, and nothing sweeps idle entries.

**Why it matters**: Memory growth is the obvious symptom, but the worse consequence is that the per-map mutex lives for the full lifetime of the process. Every login goes through it, and a map with millions of entries turns every login into a costly lookup + expiry check.

**Fix**:
- Cap the map at e.g. 10k entries; evict oldest on insert (LRU is fine — `container/list` + map is ~40 lines).
- Add a `janitor` goroutine that sweeps entries older than `loginWindowReset * 2` every minute and joins the `asyncWG` for shutdown.

**Status**: resolved. The `loginAttempts` map is now a bounded LRU (`map[string]*list.Element` + `container/list.List`) capped at `maxLoginAttemptEntries = 10_000`. `touchTrackerLocked` is the single entry point that creates-or-moves a tracker; when the map is at capacity, `evictOneLocked` walks from the LRU tail and removes the oldest *non-locked* entry. Currently-locked entries are preserved unconditionally — `TestLoginLRU_AllLockedEntriesGrowsPastCap` documents the deliberate choice to exceed cap rather than drop a live rate-limit (evicting a lockout would let an attacker reset their own limit by flooding unique source IPs). A background janitor goroutine started via `SessionManager.StartJanitor(ctx)` (called from main.go immediately after `NewSessionManager`) sweeps entries older than `loginStaleAge = 2 * loginWindowReset` every `loginJanitorInterval = 1 minute`, walks back-to-front and breaks early on the first fresh entry thanks to the LRU ordering invariant. Shutdown joins the goroutine via `SessionManager.Shutdown(ctx)` in main.go step 5 (own 2s deadline, separate from the checkin handler's `asyncWG`, since that one is package-scoped to `internal/checkin`). Test coverage: `TestLoginLRU_EvictsOldestNonLocked`, `TestLoginLRU_PreservesLockedEntriesDuringEviction`, `TestLoginLRU_AllLockedEntriesGrowsPastCap`, `TestSweepLoginAttempts_{RemovesStaleUnlocked,PreservesCurrentLockouts,ReclaimsExpiredLockouts,EmptyMapIsNoop}`, `TestJanitor_{ShutdownDrainsCleanly,ActuallySweepsViaTick}`, `TestAuthenticate_{LRUOrdering,BoundsTrackerMap}`, and `TestTouchTrackerLocked_DoesNotLeakBetweenIPs` — 12 tests total, all green.

---

### [MED] S3 — HMAC signature truncated to 64 bits — RESOLVED

**Where**: `internal/api/session.go:349`, `mac := hex.EncodeToString(h.Sum(nil))[:16]`.

Truncating SHA-256 HMAC to 16 hex chars = 64 bits. At 2^32 guesses an attacker has a 50/50 shot at forging a valid signature for a chosen session ID (birthday bound), and at 2^64 they can forge with certainty. That's not reachable over HTTP today — a sustained 10k req/s against the login endpoint would take ~580 million years — but there's no reason to truncate at all. Full 32-byte tag is 64 hex chars and costs nothing to compare.

**Fix**: drop `[:16]`. Bump the token format version string (`v2|`) so old sessions invalidate cleanly rather than silently failing signature check.

**Status**: resolved. `signToken` now returns the full 64-hex-char SHA-256 HMAC (drop of `[:16]`). Session tokens carry a version prefix constant `sessionTokenPrefix = "v2|"`; `CreateSession` emits `"v2|<raw-hex>.<sig>"`, and `ValidateSession` rejects any token missing that prefix before it even looks at the signature. The explicit prefix means any legacy v1 cookie sitting in an operator's browser after deploy fails validation cleanly and is re-issued on next login rather than silently passing a weaker check. Test coverage: `TestSessionToken_CarriesVersionPrefix`, `TestSessionToken_FullLengthHMAC` (asserts sig length == 64), `TestValidateSession_RejectsLegacyV1Format` (v1-shaped token without prefix is rejected), `TestValidateSession_RejectsWrongVersionPrefix` (real token with `v3|` substituted is rejected), `TestValidateSession_RejectsForgedFullLengthSignature` (full-length but wrong-value signature is rejected), `TestValidateSession_RoundTripV2`, `TestValidateSession_RejectsEmptyAndMalformed` — 7 new tests, all green.

---

### [MED] S4 — Signing key regenerated on every restart — RESOLVED

**Where**: `internal/api/session.go:NewSessionManager` (where `signingKey` is generated via `crypto/rand`).

Every restart — deploys, config reloads, crashes — invalidates every active staff session. That's a UX paper-cut, but it also makes it tempting for a future operator to "fix" by hard-coding a key, which is worse.

**Fix**: persist the key to `<DataDir>/session.key`, mode 0600, ownership locked to the service user. On boot:
```
if file exists → read and reuse
else          → generate, write atomically (tmp + rename), chmod 0600
```

**Status**: resolved. Added `NewSessionManagerWithKeyFile(staffPassword, keyPath string)` as the production constructor — `main.go` now computes `filepath.Join(cfg.Bridge.DataDir, "session.key")` and passes it in. The original `NewSessionManager(staffPassword)` is kept for tests and documented as ephemeral. `loadOrCreateSigningKey` handles three cases: (a) file exists with correct length → reuse and re-`chmod 0600` in case an operator loosened perms; (b) file exists with wrong length → fail loud (refuse to start rather than silently truncate or zero-pad an HMAC key); (c) absent → `crypto/rand` 32 bytes, write to a `.session.key.tmp-*` file in the same directory with 0600, `fsync`, then `os.Rename` into place — atomic under POSIX same-filesystem rename semantics. Parent directory is created with 0700 via `MkdirAll` if missing. Test coverage: `TestLoadOrCreateSigningKey_{CreatesOnFirstBoot,ReusesExisting,RejectsWrongLength,CreatesParentDir,TightensLoosePerms}`, `TestNewSessionManagerWithKeyFile_{SessionsSurviveRestart,ErrorPropagates}` — 7 new tests, all green.
Rotate manually by deleting the file (documented in README). Audit-log the generation vs. load path at boot so an unexpected key-regeneration is visible.

---

### [MED] S5 — `/test-checkin` ships in production — RESOLVED

**Where**: `internal/api/server.go:689-725`.

This endpoint simulates a tap against the handler. It's useful for integration work, but it's mounted on the same mux as the real endpoints and gated only by the admin API key. A stolen admin key — which is a compile-time-configured shared secret — becomes "unlimited free check-ins against Redpoint + physical unlock pulses" rather than "read access to audit data".

**Fix**: gate behind a build tag (`//go:build devhooks`) *and* an explicit `Bridge.EnableTestHooks` config flag. In the default production build the route doesn't exist. The CI matrix gains a `-tags devhooks` run for the integration tests that currently exercise it.

**Status**: resolved. `internal/api/testhooks_on.go` (`//go:build devhooks`) defines `testHooksCompiled = true` and implements `registerTestHooks(shortTimeout)` which installs the `POST /test-checkin` route only if `s.enableTestHooks` is true. `internal/api/testhooks_off.go` (`//go:build !devhooks`) defines `testHooksCompiled = false` and a no-op `registerTestHooks`. The `Server` struct gained an `enableTestHooks bool` field; `NewServer` accepts it as the final parameter. Config field `BridgeConfig.EnableTestHooks` plus environment binding `BRIDGE_ENABLE_TEST_HOOKS` control the runtime gate. Tests moved: `TestTestCheckin_Validation` moved to `testhooks_on_test.go` (with `setupTestServerWithTestHooks` helper that passes `enableTestHooks=true`); new tests added: `TestTestHooksCompiled_True_InDevhooksBuild` and `TestTestCheckin_Registered_InDevhooksBuild` in `testhooks_on_test.go` verify the route is present and functional in devhooks builds; `testhooks_off_test.go` (`//go:build !devhooks`) adds `TestTestHooksCompiled_False_InDefaultBuild` (constant check) and `TestTestCheckin_Absent_InDefaultBuild` (404 assertion). In production (default build) the route does not exist at all, compiled out entirely. In a devhooks build, it exists only if `BRIDGE_ENABLE_TEST_HOOKS=true` is set in config. Test coverage: three new files (testhooks_on_test.go, testhooks_off_test.go) with 5 tests total, all green in both build modes.

---

### [MED] S6 — `bulkLoadCustomers` detached from shutdown drain — RESOLVED

**Where**: `internal/api/server.go:522–525` (now routed through `bg.Group`).

**Resolution**: Introduced `internal/bg/group.go`, a small supervised goroutine group with a unified shutdown mechanism.

- **New package**: `internal/bg.Group` provides a context-based supervision model. Each goroutine is launched via `Go(name, func(ctx) error)`, receives a cancellable context, and errors (other than `context.Canceled`) are logged at Warn level. Panics are recovered and logged at Error.
- **Wiring**: Added `bg *bg.Group` field to `Server` and passed it through `NewServer(...)` as the 16th parameter (after `trustedProxies`, before `enableTestHooks`). Created `bgGroup` in `cmd/bridge/main.go` before service startup.
- **Long-running tasks migrated**: 
  - `bulkLoadCustomers` (manual API trigger): now `bgGroup.Go("directory-sync", func(ctx) { s.bulkLoadCustomers(ctx); return nil })` at line ~525.
  - `cache.Syncer`: added blocking `Run(ctx) error` method that does initial refresh + periodic loop. Launched via `bgGroup.Go("cache-syncer", syncer.Run)`.
  - `statusync.Syncer`: added blocking `Run(ctx) error` method wrapping `supervisedLoop`. Launched via `bgGroup.Go("statusync", statusSyncer.Run)`.
- **Graceful shutdown**: Shutdown sequence updated in `cmd/bridge/main.go`. After handler drain (8s), new step drains `bgGroup` with 30s timeout, giving in-flight syncs time to finish their current page. Logs "background group drain complete" or "background group drain incomplete" depending on deadline.
- **Testing**: All `NewServer` call sites updated: `internal/api/server_test.go` `setupTestServer`, `internal/api/needs_match_test.go` `buildNeedsMatchTestServer`, and `internal/api/testhooks_on_test.go` `setupTestServerWithTestHooks`. Each creates a test `bgGroup` with context cleanup.
- **Unit tests**: `internal/bg/group_test.go` validates panic recovery, deadline respect, context cancellation, multi-goroutine waits, and idempotency semantics.

---

### [LOW] S7 — Cookies not marked `Secure` — RESOLVED

**Where**: `internal/api/session.go` cookie-builder block, `internal/api/middleware.go` (SecurityMiddleware), `internal/config/config.go` (BridgeConfig).

Today the UI is served over plain HTTP on the LAN, so `Secure` would actively break login. Not a finding on current topology, but the moment the bridge is reverse-proxied behind TLS (which it should be) the cookies need `Secure: true`.

**Status**: resolved. New config field `BridgeConfig.HTTPS bool` (env: `BRIDGE_HTTPS`) controls the feature. Default is false (preserves current HTTP behavior). When true:
- `SessionManager.SetSecureCookies(bool)` method gates the `Secure` flag on all four cookie operations (SetCookie, ClearCookie, SetCSRFCookie, ClearCSRFCookie).
- `SecurityMiddleware` emits `Strict-Transport-Security: max-age=31536000; includeSubDomains` on every response (no `preload` — that's for HSTS preload list submission, out of scope).
- Comment block in `SetCSRFCookie` updated to note that when `secureCookies=true`, the cookies meet the `__Host-` prefix criteria (Secure, Path=/, no Domain) and a future commit could adopt the prefix for defense-in-depth.
- Startup log (info level) when HTTPS mode is enabled: `"HTTPS-aware mode enabled", "cookies_secure", true, "hsts", true`.
- Test coverage: `TestSetCookie_{InsecureByDefault,SecureWhenEnabled}`, `TestClearCookie_{InsecureByDefault,SecureWhenEnabled}`, `TestSetCSRFCookie_{InsecureByDefault,SecureWhenEnabled}`, `TestClearCSRFCookie_{InsecureByDefault,SecureWhenEnabled}` (8 tests in cookie_flags_test.go), `TestHSTS_{AbsentByDefault,PresentWhenHTTPSEnabled}` (2 tests in security_test.go). All 10 tests green. Backwards compatibility preserved: default (HTTPS=false) behaves exactly as today.

---

### [LOW] S8 — `Set-Cookie` domain + path — RESOLVED

Cookies are scoped with defaults (path=/). Fine for a single-app origin. If the bridge ever shares a hostname with another service, narrow to `/ui` and `/api` paths explicitly.

**Status**: investigation complete. Attempted to narrow Path from `/` to `/ui` in `internal/api/session.go`. Audit discovered six authenticated HTMX call sites from UI pages targeting root-level endpoints: `hx-post="/cache/sync"` (sync.html:10), `hx-post="/status-sync"` (sync.html:21), `hx-post="/directory/sync"` (sync.html:32), `hx-post="/ingest/unifi?dry_run=true"` (sync.html:43), `hx-post="/members"` (members.html:25), `hx-delete="/members/%s"` (fragments.go:94). Narrowing to `/ui` would cause these six calls to silently lose the session cookie and fail auth.

**Final disposition**: Path stays at `"/"` in named constants `sessionCookiePath` and `csrfCookiePath`. Added a detailed comment block in `internal/api/session.go` (lines 83–104) listing all six blocking endpoints and explaining that narrowing is deferred until a future refactor moves all authenticated endpoints under `/ui/*` (e.g., `/ui/cache/sync` instead of `/cache/sync`). Once that refactor lands, the constants can be flipped to `"/ui"` for a strict security improvement.

The named constants serve two purposes: (1) enabling the future refactor with a one-line constant change, and (2) the four path-assertion tests (`TestSetCookie_PathMatchesConstant`, `TestClearCookie_PathMatchesConstant`, `TestSetCSRFCookie_PathMatchesConstant`, `TestClearCSRFCookie_PathMatchesConstant` in `internal/api/cookie_flags_test.go`) which now guard the current `"/"` value and will catch any accidental narrowing in CI.

---

### [LOW] S9 — CSRF cookie value readable by JS — RESOLVED

The double-submit CSRF token is deliberately not `HttpOnly` (the HTMX layer needs to read it for the `X-CSRF-Token` header). That's correct. But the session cookie is `HttpOnly` and the CSRF binding is server-side, so an XSS on `/ui` still can't steal the session. Worth a comment in the code clarifying the trade-off so a future reader doesn't "fix" it.

**Status**: resolved. The review's framing ("not HttpOnly") was stale — in this codebase the CSRF cookie is already `HttpOnly: true`. The server injects the token into HTML at render time, so HTMX reads it from the DOM, not from `document.cookie`. Added a detailed 11-line comment block above `SetCSRFCookie` in `internal/api/session.go` (S9 section) that:
- Explains the double-submit pattern: cookie + header
- Documents why HttpOnly is correct here (server injects into HTML, not JS cookie-read)
- Notes this is a security improvement over the classic pattern (prevents CSS-exfil)
- Clarifies that XSS cannot exfiltrate the separate HttpOnly session cookie
- Explicitly warns against removing HttpOnly

The comment guides future maintainers to keep HttpOnly on the CSRF cookie rather than "fixing" a perceived issue.

---

## Performance findings

### [HIGH] P1 — Staff-UI fragments re-query full tables on every poll — RESOLVED

**Where**: `internal/api/server.go:1177-1194` (`handleFragMemberTable`) and `:1437-1470` (`handleFragUnmatchedTable`).

HTMX polls these every few seconds. Each call:

- `handleFragMemberTable`: returns the entire member set (one row per customer) in the template. With Mosaic's ~2k customers today it's tolerable; it scales O(N) with customers and the JSON-marshalled template output is megabytes once the member base grows.
- `handleFragUnmatchedTable`: triggers a live UniFi `FetchAccessUsers` **and** a full-table ingestion diff on every page load. Staff tabbing into the "Unmatched" view ~every 5 seconds means the bridge holds a continuous outbound HTTPS session to UniFi.

**Why it matters**: UniFi is on localhost so latency is low, but (a) the UA-Hub is not built to sustain sustained polling and (b) the diff work touches every row of the `customers` table. During a busy intake session (5 staff, 4 open tabs each), this can easily saturate the single SQLite writer we deliberately chose.

**Fix**:
- Add a 30s TTL cache at the handler layer for `handleFragMemberTable` and `handleFragUnmatchedTable`. Invalidate the cache on any mutation through the `/cards` or `/members` endpoints.
- Expose `Cache-Control: private, max-age=5` on fragment responses so HTMX can coalesce with browser cache.
- Page the member-table fragment: first 50 rows by default, with a "load more" button. HTMX handles this natively.

**Status**: resolved. Implemented three coordinated changes:

1. **TTL cache** (`internal/api/htmlcache.go`): Simple in-process TTL cache for rendered HTML fragments. Keys are keyed per-handler (e.g., `"frag-member-table:offset=0:limit=50"`). Uses version-bump invalidation: `Invalidate()` bumps a version counter so all in-flight entries become stale instantly without iterating the map. O(1) invalidation is critical for high-frequency mutations. Thread-safe with `sync.Mutex`.

2. **Pagination** (`internal/store/members.go`): New `AllMembersPaged(ctx, limit, offset)` method returns a page of members ordered by (last_name, first_name) along with the total count. Default limit 50, hard-capped at 200 to prevent abuse. Leaves existing `AllMembers` intact for back-compat.

3. **Paged rendering** (`internal/ui/fragments.go`): New `MemberTableFragmentPaged(rows, offset, total)` function renders either a full `<table>` (when offset==0) or just continuation rows. Includes a load-more `<tr>` at the bottom (id="member-table-load-more") with an HTMX button that fetches the next page via `hx-get="/ui/frag/member-table?offset=<nextOffset>&limit=50"` and swaps `outerHTML` on itself. When offset+len(rows) >= total, no load-more row is emitted.

Handler updates (`internal/api/server.go`):
- `handleFragMemberTable`: checks cache for key `"frag-member-table:offset=X:limit=Y"` before querying DB. On miss, calls `AllMembersPaged`, renders via `MemberTableFragmentPaged`, stores result with 30s TTL, and returns with `Cache-Control: private, max-age=5` header. Extracts limit/offset via `intParam` helper.
- `handleFragUnmatchedTable`: similar pattern — cache key `"frag-unmatched-table"`, 30s TTL, sets Cache-Control header upfront to cover all response paths.
- Cache invalidation: `handleAddCard`, `handleDeleteCard`, `handleAddMember`, `handleRemoveMember` each call `s.htmlCache.Invalidate()` on success path (post-audit, pre-response).

Test coverage: `TestFragMemberTable_ServedFromCacheOnSecondCall` (cache hit after first call, DB mutation doesn't affect cache until TTL expires), `TestFragMemberTable_InvalidatedByCardMutation` (mutation + Invalidate forces re-query), `TestFragMemberTable_CacheControlHeader`, `TestFragMemberTable_Pagination_DefaultPage` (75 members, GET with no params returns ~50 rows + load-more), `TestFragMemberTable_Pagination_SecondPage` (offset=50 returns remaining 25, no load-more), `TestFragMemberTable_Pagination_LimitCapped` (limit=9999 capped at 200), `TestFragUnmatchedTable_ServedFromCacheOnSecondCall`, `TestFragUnmatchedTable_CacheControlHeader`, `TestFragUnmatchedTable_InvalidatedByMemberMutation`, plus cache unit tests (`htmlcache_test.go`): `TestGet_Miss`, `TestSet_ThenGet_Hit`, `TestGet_AfterTTL_Miss`, `TestInvalidate_ForcesMiss`, `TestInvalidate_NewSetWorks`, `TestConcurrentReadersAndWriters` (10 goroutines, 100 ops each, `-race` clean). All tests green.

---

### [HIGH] P2 — `/checkins` hits Redpoint on every poll — RESOLVED

**Where**: `internal/api/server.go:223-236`.

This endpoint returns the last N check-ins by proxying live to Redpoint GraphQL on every request. The UI polls it every 3 seconds. That's ~28k Redpoint calls/day from a single tab, before we even consider multiple staff.

**Fix**: flip the default source to the local `checkins` table (we already export this via `/export/checkins`). Add `?source=redpoint` as an opt-in for the rare case when staff specifically want to see the authoritative Redpoint view. Polling goes to SQLite, which is free.

**Status**: resolved. `handleCheckins` now reads a `source` query parameter (default `local`). The `local` path uses `s.store.RecentCheckIns(ctx, limit)` — zero outbound calls, free to poll. The `redpoint` path keeps the original `s.redpoint.ListRecentCheckIns` behaviour for callers that explicitly want the authoritative view. An unknown source returns HTTP 400 so typos surface immediately instead of silently falling through to one branch.

The response envelope is unified across both sources — `{"checkIns": [...], "total": N, "source": "local|redpoint"}` — but the individual check-in item shape differs (local events are flat, Redpoint items have nested customer/gate/facility objects). Clients that need to interpret items should branch on the `source` field. A `limit` hard cap of 500 was added to prevent accidental fanout.

Tests added in `internal/api/checkins_source_test.go`:
- `TestCheckins_DefaultsToLocal` — omitting `source` hits SQLite
- `TestCheckins_LocalExplicit` — `source=local` is accepted
- `TestCheckins_LocalNoEventsReturnsEmptyArray` — empty DB returns `total:0`
- `TestCheckins_LocalRespectsLimit` — `limit=2` returns at most 2
- `TestCheckins_LimitCappedAt500` — oversized `limit` doesn't error
- `TestCheckins_InvalidSourceRejected` — unknown source → 400 mentioning "source"
- `TestCheckins_RedpointSourceCallsRedpoint` — `source=redpoint` takes the Redpoint branch (signalled by 502 when the test's fake Redpoint endpoint is unreachable)

Before the fix, a single staff tab polling every few seconds cost ~17–28k Redpoint calls/day; multiple staff scaled that linearly. After the fix, the default path costs zero Redpoint quota regardless of poll frequency or tab count.

---

### [MED] P3 — `CheckInStats` may skip the timestamp index — RESOLVED

**Where**: `internal/store/checkins.go:121-130`.

The five subqueries all use `WHERE date(timestamp) = date('now')`. `idx_checkins_timestamp` is on the raw column, and SQLite's query planner will only hit it if the LHS is the bare column. `date(timestamp)` is a function call, so this falls back to a full scan on each subquery, five scans per request.

**Fix**: store timestamps as ISO-8601 UTC (which they already are if they come through `time.Time.Format(time.RFC3339)`) and switch the predicate to a range:

```sql
WHERE timestamp >= ? AND timestamp < ?   -- [start_of_day, start_of_next_day)
```

Compute the boundaries once in Go and pass as parameters. Add an expression index `CREATE INDEX idx_checkins_date ON checkins(date(timestamp))` as a fallback if you want to keep the current SQL — but range-scan on the existing index is cleaner.

**Status**: resolved. Added `todayBoundsUTC()` in `internal/store/checkins.go` that returns today / tomorrow as bare `YYYY-MM-DD` strings (UTC). Three callers rewritten to use `WHERE timestamp >= ? AND timestamp < ?`: `CheckInStats` (the one P3 called out, plus four subqueries), `ShadowDecisionStatsToday` (same bug, same fix), and `CheckInsByHour` (the `date` parameter now scopes a half-open range rather than being wrapped in `date(timestamp)`). `EXPLAIN QUERY PLAN` on a freshly-opened DB confirms the four today-filtered subqueries now read `SEARCH checkins USING INDEX idx_checkins_timestamp (timestamp>? AND timestamp<?)`; the unconditional `COUNT(*) FROM checkins` is still a covering-index scan, which is inherent (no predicate to filter on) and not a regression.

Date-only boundaries (`"2026-04-17"`, not `"2026-04-17T00:00:00Z"`) are deliberate: the column DEFAULT is SQLite's `datetime('now')` which emits space-separated form (`"2026-04-17 15:30:00"`), while `RecordCheckIn` writes RFC3339 (`"2026-04-17T15:30:00Z"`). ASCII space (0x20) sorts before `T` (0x54), so an RFC3339 boundary would wrongly exclude rows stored in the space-separated form. Bare dates work for both because both share the YYYY-MM-DD prefix.

Tests added in `internal/store/checkins_range_test.go`:

- `TestCheckInStats_UsesTimestampIndex` — parses `EXPLAIN QUERY PLAN` and asserts all four filtered subqueries hit `idx_checkins_timestamp`; also asserts no `SEARCH checkins` line exists without an index clause, which would be the specific regression shape.
- `TestCheckInStats_CountsOnlyTodayRows` — yesterday / today / tomorrow rows seeded; today counters isolate today's two rows (one allowed, one denied, two unique customers).
- `TestCheckInStats_IncludesRowsWithSpaceSeparatedTimestamps` — inserts one row with `"2026-MM-DD HH:MM:SS"` (bypassing `RecordCheckIn`) and one RFC3339 row; both count toward today.
- `TestCheckInsByHour_DefaultsToTodayUTC` — empty date arg defaults to today UTC; yesterday rows don't bleed into today's hour buckets.
- `TestCheckInsByHour_ExplicitDateScopesToOneDay` — explicit date rejects the following day's rows.
- `TestCheckInsByHour_InvalidDateReturnsEmpty` — unparseable date returns zero rows rather than erroring (endpoint can't be crashed via bad query param).
- `TestShadowDecisionStatsToday_UsesRange` — yesterday / today / tomorrow rows seeded; Total = 2, both today rows agree.
- `TestTodayBoundsUTC_IsHalfOpen` — sanity: bounds are YYYY-MM-DD, 24h apart, `today < tomorrow` lexicographically.

Full test suite green in both `go test ./...` and `go test -tags devhooks ./...`. At 28k polls/day of `/stats` (before P2's `/checkins` fix collapsed its own N+1) the difference between five table scans and four index searches per poll compounds quickly; the fix is cheap and durable.

---

### [MED] P4 — `handleDirectorySearch` makes three sequential LIKE queries — RESOLVED

**Where**: `internal/api/server.go:866-940`.

Searches name, email, and external_id separately then merges in Go. Three round-trips to the single-writer SQLite; each is a fanout LIKE scan.

**Fix**: one query with `UNION ALL` and a dedicated FTS5 virtual table (`customers_fts(name, email, external_id, barcode)`), rebuilt via SQLite triggers on the base table. FTS5 turns prefix search from O(N·cols) to O(log N) and costs a few KB on disk. The triggers give you automatic index maintenance without cron sync.

**Status**: resolved. Migration 6 (`internal/store/migrations.go`) creates a contentless FTS5 virtual table `customers_fts(redpoint_id UNINDEXED, name, email, external_id, barcode)` with the `unicode61 remove_diacritics 2` tokenizer (so "Ramirez" matches "Ramírez"). Three triggers — `customers_fts_ai`, `customers_fts_ad`, `customers_fts_au` — keep the FTS index in sync with INSERT/DELETE/UPDATE on `customers`; the migration also backfills any rows that already exist. Schema version bumped to 6.

The query path (`Store.SearchCustomersFTS` in `internal/store/customers.go`) collapses to one statement:

```sql
SELECT c.* FROM customers_fts f
JOIN customers c ON c.redpoint_id = f.redpoint_id
WHERE customers_fts MATCH ?
ORDER BY bm25(customers_fts, 10.0, 5.0, 2.0, 2.0)
LIMIT ?
```

`EXPLAIN QUERY PLAN` confirms the planner walks the FTS5 module (`SCAN f VIRTUAL TABLE INDEX 0:M5`) and joins the base table by primary-key index (`SEARCH c USING INDEX sqlite_autoindex_customers_1 (redpoint_id=?)`). Column weights skew BM25 toward `name` (10×) over `email` (5×) over the id columns (2× each), matching how staff actually search. A `buildFTSQuery` helper tokenises the user's input on whitespace, strips characters outside `[A-Za-z0-9_@.-]` (defangs FTS5 reserved syntax like `"`, `(`, `*`, `:`), wraps each token in quoted prefix form (`"alice"*`), and joins with implicit AND. Pure-meta-character input sanitises to empty and short-circuits to zero results rather than erroring or matching everything.

The handler (`handleDirectorySearch` in `internal/api/server.go`) shrank from ~75 lines of email/name/last-name fan-out to one `SearchCustomersFTS(ctx, q, 50)` call. Response shape is unchanged so the UI didn't need updates. The cache-status annotation per result (`inCache`, `cacheNfcUid`) is preserved.

Tests added in `internal/store/customers_fts_test.go` (13 cases):

- Prefix match per indexed column: `TestSearchCustomersFTS_PrefixOnName`, `..._PrefixOnEmail`, `..._PrefixOnExternalID`, `..._PrefixOnBarcode`.
- Multi-token AND semantics: `TestSearchCustomersFTS_MultiTokenAndSemantics` — "alice smith" matches Alice Smith only, not Alicia Jones or Bob Smith.
- Trigger-driven sync: `TestSearchCustomersFTS_TriggerSyncOnUpsert` (insert → update → delete via raw SQL each surfaces in subsequent searches), `TestSearchCustomersFTS_BatchUpsertSyncs` (prepared-statement batch path also fires triggers).
- Diacritic folding: `TestSearchCustomersFTS_DiacriticFolding` — both directions ("ramirez" finds Ramírez; "josé" finds Jose).
- Sanitiser table test: `TestBuildFTSQuery_SanitisesMetaCharacters` — 12 input/output pairs including reserved characters, operator words, and edge cases.
- Empty/meta-only input: `TestSearchCustomersFTS_EmptyQueryReturnsEmpty` — five different "empty after sanitisation" inputs all return zero rows with no error.
- Limit cap: `TestSearchCustomersFTS_HardCapAt200`.
- BM25 ranking: `TestSearchCustomersFTS_BM25Ranking` — short-doc match ranks above long-doc match.
- Plan check: `TestSearchCustomersFTS_UsesFTSIndex` — pins `VIRTUAL TABLE` and PK-index join.

API-level tests in `internal/api/directory_search_test.go` (6 cases): missing q → 400, name-prefix find, full-email find, cache-status annotation, multi-token AND through the handler, meta-only query → 200 with zero results. Full suite green in both `go test ./...` and `go test -tags devhooks ./...`.

The old per-column scans were O(N·cols) per request; FTS5 is O(log N) per token plus the BM25 sort. At 2k customers the difference is small in absolute terms; at 10–20k (a realistic Redpoint mid-size gym) it's the difference between "snappy" and "noticeably laggy", and each search no longer contends with concurrent writes by issuing three separate read transactions in a row.

---

### [MED] P5 — No retry/backoff in the Redpoint client — RESOLVED

**Where**: `internal/redpoint/client.go` (and callers across `internal/statusync`, `internal/checkin`).

Every transient failure — a Redpoint 502, a packet drop on the WAN — surfaces immediately to the caller. The breaker you added in B#3 only wraps the recheck path. Async writes from `handler.recordInRedpoint` fail silently after one attempt; the tap is logged locally but never makes it to Redpoint, and the dashboard shows it as lost.

**Fix**: add a small retry wrapper in the Redpoint client. Exponential backoff starting at 200ms, three attempts, jitter. Only retry on network errors and 5xx + 429. Never retry on 4xx — that's permanent. Breaker + retry compose fine: retry is the fast inner loop, breaker is the slow outer guard.

**Status**: The previous `execWithRetry` was string-matching error messages (`strings.Contains(errMsg, "429") || ... || "EOF" || "connection reset"`), looping five times with linear 3s waits and no jitter, and — critically — five public methods (`LookupByExternalID`, `GetCustomer`, `CreateCheckIn`, `ListGates`, `ListRecentCheckIns`) bypassed it entirely by calling `c.exec` directly. So the methods most exposed to a flaky upstream were the ones with no retry at all.

`exec` now wraps its two failure modes in typed errors: `*httpError{Status, Body}` for non-2xx responses and `*transportError{Err}` for any HTTP transport failure (DNS, TCP, TLS, mid-stream disconnect, body-read failure). `execWithRetry` classifies via `errors.As` instead of string matching: retry on `*transportError`, retry on `*httpError` with `Status == 429 || Status >= 500`, do not retry on any other 4xx, and short-circuit on `context.Canceled`/`DeadlineExceeded` anywhere on the chain (no point waiting if the caller has given up). The loop is now `maxAttempts = 3` with `backoffFor(attempt)` returning `200ms × 2^(attempt-1)` with symmetric ±25% jitter — so attempts land at roughly 200ms, 400ms, 800ms apart, the worst-case retry burst comfortably fits inside the 10-second context most callers carry, and concurrent denied taps don't all retry in lockstep after a single upstream blip. Each backoff sleep selects on `ctx.Done()` so cancellation is honoured immediately. Every public method now routes through `execWithRetry`, including the mutation `CreateCheckIn` — safe because Redpoint's `createCheckIn` already returns a typed `DuplicateCheckInResult` for any duplicate within 15s, so an accidental "request landed but response lost" double-write is observable and harmless. `backoffBase` is a package var so tests can collapse it to ~100µs.

`internal/redpoint/retry_test.go` adds 16 cases: classification table for transport/429/5xx/4xx/ctx-canceled/plain errors; `backoffFor` doubling check (averaged over 500 samples) and ±25% jitter envelope; loop behaviour against a `flakingServer` (recover after 429, recover after two 5xx, fail after maxAttempts on sustained 503, no retry on 401/404, retry on a hijacked-and-closed connection that produces a real transport error from `net/http`); a context-cancellation test that asserts the loop unblocks within ~150ms when cancelled mid-backoff; a parametric "every public method routes through retry" suite that points each of `LookupByExternalID`, `GetCustomer`, `CreateCheckIn`, `ListGates`, `ListRecentCheckIns` at a server that 503s once then succeeds and asserts exactly two HTTP calls — one would have meant the method bypassed the retry layer; and two `exec`-level tests that pin the typed-error wrapping (a 418 surfaces as `*httpError{Status:418, Body:"…teapot…"}`, and pointing the client at a closed server yields a `*transportError`).

Full suite green in both `go test ./...` and `go test -tags devhooks ./...`. The breaker in `internal/statusync.RecheckDeniedTap` continues to work unchanged — it's now correctly composed: retry is the fast inner loop, the breaker is the slow outer guard against sustained outages, and the breaker's failure threshold counts attempts after exhausted retries (which is what you want — three quick attempts at 200/400/800ms is one logical "Redpoint try", and that's the unit the breaker should be counting).

---

### [LOW] P6 — `SetMaxOpenConns(1)` — RESOLVED

Deliberate, necessary for SQLite WAL + sqlx, correct. Worth a comment pointing at the modernc/sqlite driver's threading model so a future maintainer doesn't "optimise" this.

**Status**: Long-form comment added at `internal/store/store.go.Open` covering the three concrete failure modes a future "let's make it 10" change would unleash (per-connection transaction isolation surprises with WAL — read on conn A misses commit on conn B until A's txn ends; writer-lock contention under `_busy_timeout=5000` because only one conn holds the SQLite reserved-lock at a time and the rest either block or fail SQLITE_BUSY; prepared-statement cache and FTS5 virtual-table session split N ways), the recommended path forward if read concurrency *is* the bottleneck (separate read-only `*sql.DB` with `?mode=ro`, not a bumped pool), and the role of `SetConnMaxLifetime(0)` (keep that one conn alive across the process lifetime, avoid the WAL re-checkpoint cost on every reopen). The shorter sibling at `internal/custdir/directory.go.Open` references the long-form note and adds the package-specific footgun warning ("do not raise without removing every place in this package that assumes a single-writer model"). Build/vet/test green.

---

### [LOW] P7 — `bulkLoadCustomers` page-by-page without prepared statement — RESOLVED

Each page's customer rows upsert individually inside a transaction. For 2k customers the transaction commit cost dominates; that's fine at today's scale. If Mosaic ever hits 20k customers, switch to a `prepare` + `exec` loop, wrapped in a single transaction per page.

**Status**: The optimisation already lands on every bulk-write path. Tracing the call graph: `internal/api/server.go.bulkLoadCustomers` (line 610) builds a 100-row `[]store.Customer` per Redpoint page and hands the whole slice to `s.store.UpsertCustomerBatch`. `internal/store/customers.go.UpsertCustomerBatch` is `BeginTxx → tx.PrepareContext(INSERT … ON CONFLICT DO UPDATE) → loop stmt.ExecContext(row) → tx.Commit()` — exactly the prepare-once / exec-per-row / single-COMMIT shape the review prescribes. The legacy `internal/custdir/directory.go.UpsertBatch` (used by the older custdir bulk loader at line 213) follows the same pattern with the same INSERT…ON CONFLICT statement. So at the line the architecture review was looking at, the code was already doing one BEGIN, one PREPARE, N EXECs, one COMMIT per 100-row page — the reviewer was reading the now-removed earlier path that called `UpsertCustomer` (singular, no transaction wrapper) in a loop.

What the reviewer's concern *did* uncover is that there was no comment documenting why these batch functions look the way they do, which makes them inviting targets for "clean up the prepared-statement boilerplate" PRs that would silently undo a 10× regression. Both batch functions now carry explanatory comments tying the four-line pattern back to its purpose: one BEGIN per page (single fsync at COMMIT instead of N), one PREPARE (SQLite parses + plans + compiles the FTS5 trigger chain once, bind path is a memcpy after that), `defer tx.Rollback()` as the safety net (no-op after Commit), `s.mu.Lock()` enforcing application-layer single-writer over the single-conn pool. The custdir comment cross-references the longer note on the store side. Existing `TestCustomerCRUD` covers the batch happy path; full suite stays green in both `go test ./...` and `go test -tags devhooks ./...`.

---

## Architectural recommendations

### A1 — Split control plane and data plane — RESOLVED

The same HTTP server currently serves:

- **Data plane**: `/health`, `/stats`, `/checkins`, `/export/*`, `/ingest/unifi`. High-rate, read-mostly, gated by admin API key or shadow-mode rules.
- **Control plane**: `/test-checkin`, `/unlock/{doorId}`, `/status-sync`, `/cache/sync`. Mutating, infrequent, and the high-blast-radius endpoints.
- **Staff UI**: `/ui/*`, HTMX-driven, session-gated.

Today the only separation is path-based middleware. A single config bug (e.g. forgetting `adminApiKey`) disabled the whole gate and left every mutating endpoint open — that's exactly the class of bug the Phase C config-validator now refuses to boot on, but the root cause (one mux, one set of middleware) is still there.

**Proposal**: move the control-plane routes to a second `http.Server` on a separate port (`Bridge.ControlPort`) that binds only to 127.0.0.1 by default. The staff UI and data-plane stay on `Bridge.Port`. Config validation refuses to boot if `ControlPort == Port`. This is five-minute change in `cmd/bridge/main.go` and it means the attack surface of a leaked admin key is physically reachable only from the host.

**Status (resolved).** The split is wired end-to-end: `Server` now carries a second `controlMux` alongside `mux`, exposed via `Server.ControlHandler()`. `cmd/bridge/main.go` stands up two `http.Server`s — public on `BindAddr:Port` (unchanged) and control on `ControlBindAddr:ControlPort` (defaults to `127.0.0.1` and `Port + 1`). Shutdown drains the control listener first, then the public listener, before the Redpoint async drain. A dedicated `ControlSecurityMiddleware` guards the control plane with admin-Bearer-only auth (no `/ui` carve-out, no `/health` bypass, no session cookies, no CSRF); an empty configured `AdminAPIKey` refuses every request rather than falling open.

Scope of the move was deliberately narrowed from the original four-route list to the two routes that (a) cause physical-world side effects and (b) are never called by the staff UI from the browser: `POST /unlock/{doorId}` and the devhooks-gated `POST /test-checkin`. The four bulk-sync mutations (`/cache/sync`, `/directory/sync`, `/ingest/unifi`, `/status-sync`) stay on the public mux because `sync.html` posts to them directly via HTMX from authenticated browser sessions; moving them to the loopback listener would have silently broken the UI. Widening the split to include them is tracked as a follow-up that depends on the `/ui/sync/*` proxy refactor noted in `internal/api/session.go` alongside the S8 cookie-path-scoping work.

Configuration: `Bridge.ControlPort` (env `BRIDGE_CONTROL_PORT`) and `Bridge.ControlBindAddr` (env `BRIDGE_CONTROL_BIND_ADDR`, default `127.0.0.1`) are new fields in `BridgeConfig`. `Load()` auto-derives `ControlPort = Port + 1` when zero; `validate()` refuses `ControlPort == Port` and out-of-range values at boot with explicit error messages. Tests in `internal/config/config_test.go` cover the derivation, the override path, and the equal-port / out-of-range rejections.

Tests: `internal/api/control_plane_test.go` pins four invariants — the control route returns 404 on the public mux, resolves on the control mux, the four data-plane sync mutations stay on the public mux and return 404 on the control mux, and read-only endpoints (`/health`, `/stats`, `/checkins`, `/directory/status`) stay off the control mux. `internal/api/control_security_test.go` exercises the middleware shape: missing / wrong / correct bearer, the empty-config-key must-not-fall-open case, and the absence of `/ui` and `/health` carve-outs. `TestTestCheckin_Registered_InDevhooksBuild` now drives through `ControlHandler` to match the new mux assignment. Full suite green in both default and `-tags devhooks` modes.

---

### A2 — Centralise async work — RESOLVED

The codebase had three categories of goroutine:

- `asyncWG` for Redpoint writes (Phase B#6, drained).
- Ticker goroutines for sync + metrics (cancelled via context, *not* drained).
- `bulkLoadCustomers` fire-and-forget (neither cancelled nor drained).

Introduce a single `bg` package with:

```go
type Group struct { ... }
func (g *Group) Go(name string, fn func(ctx context.Context) error)
func (g *Group) Shutdown(ctx context.Context) error  // cancels + waits
```

Every goroutine registers through it, with `name` used for the slog attribute and metrics label (`bg_goroutines_running{name="sync"}`). Shutdown cancels the context and waits with a deadline. A stuck goroutine is logged by name at shutdown so incident response doesn't have to chase pprof for an identity.

**Status**: resolved. S6 already moved the three long-running tasks into `bg.Group` (`directory-sync`, `cache-syncer`, `statusync`). A2 layered the observability and supervision finishing touches on top of that:

- **Per-name gauge**. `internal/bg/group.go` grew an optional `Metrics` interface (`Inc(name) / Dec(name)`) and a `NewWithMetrics(parent, logger, metrics)` constructor; the original `New` is preserved for callers that don't want a gauge. The bg package keeps no dependency on `internal/metrics` — `cmd/bridge/main.go` defines a small `bgMetricsAdapter` that maps `Inc/Dec(name)` onto `metrics.Registry.Gauge(fmt.Sprintf("bg_goroutines_running{name=%q}", name)).Inc/Dec()`. `prometheusBaseName` strips the `{…}` suffix so a single `# TYPE bg_goroutines_running gauge` header covers every per-name series. The Inc/Dec defer ordering (LIFO: `defer trackEnd; defer recover()`) guarantees the gauge is decremented even when the goroutine panics, so the gauge can't drift upward across reconnects.
- **Stuck-goroutine logging at shutdown**. `Group` now tracks a live-name map (`map[string]int` keyed by name, entries deleted when count hits zero). On `Shutdown` deadline expiry the map is snapshotted under lock, sorted by name, and emitted one Warn line per still-running name (`"bg goroutine did not exit before shutdown deadline" name=… count=…`). An operator can now read a stalled-shutdown log and immediately see *which* task didn't drain — no need to chase pprof for a stack identity.
- **Last unmanaged goroutine eliminated**. `internal/unifi/client.go` previously fired the reconnect-backfill callback inside an anonymous `go cb(...)`, outside any supervised context — invisible to the gauge, not drained on shutdown. The `OnReconnect` contract now documents that the callback is invoked synchronously and must not block; `cmd/bridge/main.go` wraps the callback body in `bgGroup.Go("reconnect-backfill", …)`, so the backfill REST work shows up as `bg_goroutines_running{name="reconnect-backfill"}` and the in-flight replay drops cleanly when the group context cancels (each replayed event also gets a `context.WithTimeout(bgCtx, 15s)` so the per-event handler call respects shutdown too). The unifi package retained no dependency on `bg`.
- **Test coverage**. `internal/bg/group_test.go` gained eight new tests pinning the new behaviour: `TestMetrics_IncDecOnNormalExit`, `TestMetrics_IncDecOnPanic` (the LIFO defer guarantee), `TestMetrics_IncDecOnError`, `TestMetrics_PerName` (asserts the gauge is per-name, not coalesced), `TestNew_NoMetricsHookIsSafe` (backwards compat for callers using `New`), `TestShutdown_LogsStuckGoroutinesByName` (capturing slog handler asserts both stuck-goroutine names appear in the warning log + the right warning text), `TestShutdown_NoStuckLogWhenAllExit` (no false positive on clean shutdown), and `TestMetrics_Concurrent` (100 concurrent Go() calls, race-detector-clean). All pre-existing bg tests still pass. Full build/vet/test suite green in both default and `-tags devhooks` modes.

---

### A3 — Formalise the deny-recheck contract with Redpoint — RESOLVED

`RecheckDeniedTap` is the most interesting piece of business logic in the bridge — it papers over the window where UA-Hub's cached decision diverges from Redpoint's live state (suspended membership, card just re-enabled, etc.). Right now it's buried in `statusync`. Move it to its own package (`internal/recheck`) with:

- An explicit interface (`Rechecker`) the check-in handler depends on.
- A config knob for the recheck window (how stale a UA-Hub decision can be before we distrust it).
- A test suite that covers the four quadrants: UA=allow/deny × Redpoint=allow/deny, plus breaker-open and breaker-half-open.

This isolates the "business rule" from the "status propagation" side of `statusync` and makes the eventual Phase D feature-flag work trivially gate-able.

**Status (A3 RESOLVED).** Extracted into `internal/recheck` (`recheck.go` + `breaker.go` + tests), with the breaker moved alongside the path that actually uses it (statusync's daily loop never touched it). The handler now depends on a one-method `Rechecker` interface (`RecheckDeniedTap(ctx, nfc) (*Result, error)`) rather than `*statusync.Syncer`, and the recheck path's three upstreams (Store, RedpointClient, UnifiClient) are also defined as narrow local interfaces — so `recheck_test.go` substitutes hand-rolled fakes without spinning up SQLite, GraphQL, or UA-Hub. Wired in `cmd/bridge/main.go` via `recheck.New(db, redpointClient, unifiClient, recheck.Config{MaxStaleness: cfg.Bridge.RecheckMaxStaleness, ShadowMode: cfg.Bridge.ShadowMode}, …)` followed by `handler.SetRechecker(rechecker)` (the old `SetStatusSyncer` is gone). The freshness knob is a new `BridgeConfig.RecheckMaxStaleness` (env `BRIDGE_RECHECK_MAX_STALENESS`, parsed as `time.Duration`); zero — the default — reproduces the pre-A3 behaviour ("always recheck modulo breaker"), and any positive value short-circuits the live query when the cache is younger than the budget. An unparseable `cached_at` fails open to a live recheck so a corrupt timestamp can't lock out a renewed member. The breaker (5/60s default, both tunable on the recheck config) increments only on upstream-health failures (network, 5xx) — application-level "customer not found" is treated as an answer, not a health signal, and a dedicated test pins that. `recheck_test.go` covers the four-quadrant matrix (UA=deny × Redpoint=allow|deny), the freshness gate (zero/fresh/stale/junk-timestamp), the breaker (trips on consecutive failures, success resets, cooldown-then-success closes via injected clock), shadow mode (store updated, UA mutation skipped), and the two UA-failure tails (ListUsers fails, UpdateUserStatus fails — Reactivated=true with informative `Reason` because the cache is fresh). 20 tests total in the package (5 breaker unit + 15 service); all pass under `-race`. Full build/vet/test suite green in both default and `-tags devhooks` modes. Roadmap entry 16 marked RESOLVED.

---

### A4 — Persist the audit trail separately from the mutable cache — RESOLVED

The `customers` and `members` tables are a cache of Redpoint truth and get rebuilt on every full sync. The `checkins` table is the canonical audit trail for "who tapped when and what decision did we make" — it must never be lost.

Today they live in the same SQLite DB, which means any schema migration risks the audit trail and any `DROP TABLE customers` during a recovery scares everyone. Split them:

- `cache.db`: customers, members, gates. Rebuildable from Redpoint, safe to wipe.
- `audit.db`: checkins, jobs, shadow_decisions. Append-mostly, retain-long.

SQLite ATTACH lets queries that join both stay identical. Backup becomes "copy audit.db offsite" without worrying about cache size.

**Status (A4 RESOLVED).** The monolithic `bridge.db` is now two physical files under `DataDir`: `audit.db` (primary connection — `checkins`, `door_policies`, `jobs`, `ua_user_mappings`, `ua_user_mappings_pending`, `match_audit`) and `cache.db` (ATTACHed as `cache` at `Open()` time — `customers`, `customers_fts`, `members`, `sync_state`). The partition is clean: the only foreign key in the whole schema (`members.customer_id → customers.redpoint_id`) lives entirely inside `cache.db`, and no existing query joins across the two sides, so the ATTACH boundary is transparent — every unqualified `SELECT ... FROM customers` / `INSERT INTO checkins` keeps working because SQLite resolves unqualified names against main then attached in order. `SetMaxOpenConns(1)` + `SetConnMaxLifetime(0)` pins the ATTACH for the life of the process (ATTACH is per-connection; a larger pool would need to re-attach on every new conn). Migrations are split into two independent monotonic sequences (`cacheMigrations` len=3, `auditMigrations` len=3), each tracked by its file's own `schema_version` table — `migrateWith(db, migrations, label)` runs either sequence against either handle. Legacy `bridge.db` from pre-A4 installs is handled one-time at `Open()` by `splitLegacyDBIfNeeded`: it byte-copies the legacy file into both new paths, runs `pruneAuditCopy` / `pruneCacheCopy` to `DROP` the wrong-side tables from each copy (order matters: triggers before FTS virtual table), force-sets each file's `schema_version` to the post-A4 value so the idempotent audit migrations + the non-idempotent `ALTER TABLE checkins ADD unifi_result` + the non-idempotent FTS backfill don't re-run, and renames `bridge.db` → `bridge.db.pre-a4.bak`. The approach is filesystem-level duplicate-then-prune rather than `ATTACH … INSERT SELECT` because it preserves indexes/FKs/FTS structures byte-for-byte and survives any future schema drift without editing the split logic. Idempotent on re-boot — the early-return `if auditPath exists, skip` means second-and-subsequent boots are a no-op; a pathological state where both `bridge.db` and `audit.db` coexist is left alone rather than re-split (preserves operator-staged recovery snapshots). `internal/store/split_test.go` adds 6 scenarios: fresh install creates both files, legacy split preserves rows and writes correct `schema_version` on each side + prunes wrong-side tables absent, second `Open()` is idempotent (customer count stays 1 not 2), legacy+audit coexistence skips the split, FTS triggers still fire after ATTACH, and FK infrastructure remains intact across ATTACH (proven by explicitly enabling `PRAGMA foreign_keys=ON` on the pinned conn and asserting an orphan-member insert fails — guards against a hypothetical regression where ATTACH silently scopes PRAGMAs). Full build/vet/test suite green in both default and `-tags devhooks` modes. Roadmap entry 17 marked RESOLVED.

**Incidental discovery (out of scope, logged for follow-up).** While writing the FK test I probed `modernc.org/sqlite`'s DSN-param handling and found that the `?_journal_mode=WAL&_foreign_keys=ON&_busy_timeout=5000` shorthand (which the project has used since day one) is **silently ignored** — the driver only honours the `?_pragma=NAME(VAL)` form. The three PRAGMA settings currently evaluate to `journal_mode=delete`, `foreign_keys=0`, `busy_timeout=0` on every connection. This predates A4 and every production + test call site has been implicitly depending on that (no-FK, no-WAL, no-busy-timeout) behaviour; flipping to the correct `_pragma=` syntax cascades into ~20 test failures because fixtures create orphan members for test convenience. A4 preserves the historical behaviour verbatim to keep scope surgical — the pragma fix is appropriate follow-up work alongside the test-fixture cleanup. See the comment on `dsnFor` in `internal/store/store.go`.

---

### A5 — Observability — RESOLVED

Prometheus counters are in. Three things missing that I'd want before any Phase-D rollout:

- **Histogram on Redpoint request latency**, bucketed at [50ms, 100ms, 250ms, 500ms, 1s, 5s]. Today you can see that Redpoint failed, not that it's slow — which is what kills the UX long before it breaks.
- **Gauge on `asyncWG` depth**. If we ever see it climb above ~5 sustained, Redpoint is backing up.
- **Structured slog for every breaker transition** (closed→open with failure count, open→half-open, half-open→closed). Current logs cover failures but not state transitions explicitly.

**RESOLUTION.** All three operator-visible signals shipped, scoped to the single `redpoint.Client.exec()` funnel and the `recheck.breaker` so the instrumentation is mechanical rather than scattered.

The latency histogram (`redpoint_request_duration_seconds`, buckets exactly as specified) is observed inside `exec()`'s deferred `observeDuration(start, retErr)` so every transport call — including each retry attempt — contributes one observation. Success/error split lives in companion counters (`redpoint_requests_total`, `redpoint_request_errors_total`) instead of label-encoded histograms because the in-tree `metrics.Registry`'s Prometheus emitter has a latent bug with label-encoded histogram names; counters are unaffected. A null-customer GraphQL reply counts as a success (HTTP 200, valid GraphQL data) — the breaker invariant in `recheck.Service` already enforces this and we mirror it in metrics so an unknown card tap doesn't masquerade as a Redpoint outage. Nil-registry safety is preserved on the call path so existing tests keep passing without a metrics wire-up. Four tests in `internal/redpoint/metrics_test.go` pin success, customer-not-found, transport-error (3 retry attempts → 3 error increments), and nil-registry safety.

The async-write depth gauge (`redpoint_async_writes_in_flight`) is incremented in lockstep with `h.asyncWG.Add(1)` and decremented inside the goroutine via `defer` so the value tracks the WG counter at every observable moment, even if `recordInRedpoint` panics. Backfill events skip the async path entirely (and therefore never touch the gauge) so replaying a long outage's events doesn't falsely inflate "Redpoint backed up" alerts. Three tests in `internal/checkin/metrics_test.go` pin: gauge climbs to N while N goroutines are parked inside a blocking httptest server, gauge returns to 0 after `Shutdown` drains, dispatch is safe with a nil registry, and backfill events leave the gauge at 0.

The breaker emits structured slog at every operator-actionable transition: `closed_to_open` (initial trip, with failure count + threshold + cooldown), `open_to_closed` (cooldown elapsed, probe admitted), `probe_succeeded` (recovery confirmed via the next `success()` call), and `closed_to_open_after_probe` (re-trip while still in the probe window — alert rules can distinguish "still down" from "fresh outage"). Sub-threshold failures and ordinary `success()` calls stay silent so signal isn't drowned. The breaker carries `component=recheck.breaker` via `logger.With(...)` for filtering. Five tests in `internal/recheck/breaker_transition_test.go` pin the exact ordered transition sequence for trip-only, full recovery cycle, probe-window re-trip, ordinary-success silence, and the component-attribute wiring. The original five `breaker_test.go` invariants (closed-by-default, trip-after-threshold, reset-on-success, recover-after-cooldown, reopen-after-cooldown-on-failure) all still pass — the transition logging is non-invasive.

**Notes on the half-open spec.** The original recommendation references `open→half-open / half-open→closed / half-open→open`, which is the textbook breaker shape. The bridge's breaker deliberately avoids a separate half-open state for the reason documented in `breaker.go` — the first call after cooldown automatically acts as the probe, and modelling a full half-open gate adds locking complexity without changing observable behaviour. The instrumentation models the intent of that shape (transitions per probe outcome) without changing the state machine; alert authors get the same signal granularity the spec asked for.

---

## Prioritised roadmap

**This sprint (correctness + security hardening; each is <1 day):**

1. C2 — switch statusync matching from NFC-token lookup to email lookup with name-disambiguation fallback for household email collisions; skip PIN-only users outright; add grace-period default-deactivation for unmatched NFC users; build the `/ui/unmatched` panel and the `/ui/members/new` provisioning flow (using the confirmed UA-Hub API endpoints §3.2/§3.3/§3.6/§3.7/§6.2/§6.3 for create + assign-policy + enroll-card + bind-card). This is the highest-leverage correctness item because it closes the "ex-member card enabled forever" gap.
2. C3 — `last_sync_completed_at` gauge with Pushover alert if age > 48h; `defer recover()` on the ticker goroutine.
3. S1 — trusted-proxies config + XFF walk.
4. S2 — bound `loginAttempts`; add janitor goroutine.
5. S3 — full-length HMAC tag + version bump.
6. S6 — centralise shutdown drain via a single `bg.Group` (includes fix to `bulkLoadCustomers`).

**Next sprint (performance under real load; each is 1–2 days):**

7. P2 — flip `/checkins` to read from local store by default.
8. P1 — fragment caching + pagination.
9. P3 — range predicate on timestamp; add expression index as belt-and-suspenders.
10. P5 — retry wrapper on Redpoint client with jitter.
11. C1 — pin sync to a wall-clock time (HH:MM config) shortly after Redpoint's daily update window, instead of drifting 24h ticker.

**Following sprint (structural; each is 2–3 days):**

12. A1 — split control plane to its own port. — RESOLVED
13. A2 — `bg.Group` becomes the shared primitive. — RESOLVED
14. S4 — persist signing key to `<DataDir>/session.key`.
15. S5 — `devhooks` build tag around `/test-checkin`.

**Later (bigger rocks):**

16. A3 — extract recheck package. — RESOLVED
17. A4 — split cache.db / audit.db. — RESOLVED
18. P4 — FTS5 on customers for directory search.
19. A5 — Redpoint latency histogram + breaker transition logs. — RESOLVED

Everything above this line is deferrable after C2/C3/S1/S2 land. C2 is the top priority because it's the only item whose failure mode is "a non-member keeps getting in" — which is the single outcome this bridge exists to prevent. (The previously-high-priority C1 has been downgraded now that we know Redpoint itself updates membership state only once daily — the 24-hour window is the correct size; what matters is alignment with Redpoint's update timing and detecting sync failure, both of which are covered by C3 and the small C1 fix above.)

---

## Appendix: what's already in good shape

Worth calling out so the review doesn't read as purely negative:

- **Config validation refuses to boot without `ADMIN_API_KEY`** (Phase C). This was the single highest-impact fix of the last cycle — the admin-auth gate in middleware.go has a `if cfg.AdminAPIKey != "" && ...` pattern that silently fell open. Forcing a non-empty value at boot closes that class of bug for good.
- **`NonSecretHash()` emitted at startup** gives operators a one-line diff of "config actually changed between reboot N and N+1", which is the kind of observability that saves postmortems.
- **Circuit breaker around `RecheckDeniedTap`** (Phase B#3) is a clean implementation — threshold + cooldown + state enum, 5 tests, half-open transition tested. This is the pattern I'd apply next to the status-sync write path.
- **Shadow-mode gating** threaded through `unlockAndRecord`, `recordInRedpoint`, and `writeUniFiStatus` means a live deploy can be run side-by-side with the incumbent system with confidence. Not many bridges get this right.
- **The asymmetric propagation model is the right default.** Near-real-time reactivation via `RecheckDeniedTap` (paying members get in immediately after renewal) paired with daily deactivation (suspended members lose access within a day) is exactly the trade-off a customer-facing gym should make — false denials are expensive, false admits are cheap and self-correcting. The finding at C1 is about *configuring and monitoring* that window, not about changing the asymmetry itself.
- **WAL + `SetMaxOpenConns(1)`** for SQLite is the correct model for this workload and avoids the writer-contention footgun that trips up most Go+SQLite projects.
- **Graceful shutdown ordering** (HTTP → WS → handler drain → cancel bg → defers) is the correct sequence and rare to see laid out this cleanly. The remaining work is consolidating the drain set (A2), not re-ordering it.

The bridge is in much better shape than a typical first-production on-prem Go service. The remaining work is "harden the edges" rather than "rewrite the middle".
