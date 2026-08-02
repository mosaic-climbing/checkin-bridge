// Command spike runs the Milestone-0 read-only verification probes against
// the Redpoint HQ GraphQL API (see gymos docs/PLAN.md §3). It answers the
// "doc-claimed" rows of the capability matrix with observed behavior:
// facility topology, facility-header read semantics, CheckInFilter
// date-range behavior, check-in channel completeness, history-walk pacing,
// Customer field exposure, and the customReports inventory.
//
// Safety properties:
//   - Read-only: every request is a GraphQL query; no mutations exist in
//     this binary.
//   - Paced: one request at a time with a fixed inter-request delay
//     (default 2s, matching the mirror walker's safe pacing) — never
//     parallel.
//   - Bounded: a full run is ~15-20 requests. If two requests rate-limit
//     (429) despite pacing, the run aborts rather than contributing to a
//     429 storm.
//   - PII-clean output: the report contains no member names, emails,
//     phone numbers, or birth dates — only counts, presence booleans, and
//     opaque ids — so results can be committed to docs/spike-results/.
//
// Run it on the gym MacBook where the production .env lives:
//
//	make spike-push spike-run spike-pull
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
)

const introspectionQuery = `query IntrospectionQuery {
  __schema {
    queryType { name }
    mutationType { name }
    types {
      kind name description
      fields(includeDeprecated: true) {
        name description
        args { name type { ...TypeRef } }
        type { ...TypeRef }
      }
      inputFields { name type { ...TypeRef } }
      enumValues { name }
      possibleTypes { name }
    }
  }
}
fragment TypeRef on __Type { kind name ofType { kind name ofType { kind name ofType { kind name } } } }`

func main() {
	envFile := flag.String("env-file", "/usr/local/mosaic-bridge/.env", "path to the bridge .env holding REDPOINT_* settings")
	out := flag.String("out", "", "write the markdown report here (default stdout)")
	schemaOut := flag.String("schema-out", "", "write the introspection schema JSON here if introspection is enabled")
	delay := flag.Duration("delay", 2*time.Second, "inter-request delay (rate-limit safety; do not lower)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	env, err := parseEnvFile(*envFile)
	if err != nil {
		logger.Error("cannot read env file", "path", *envFile, "error", err)
		os.Exit(2)
	}
	apiURL := env["REDPOINT_API_URL"]
	if apiURL == "" {
		apiURL = "https://lefclimbing.rphq.com"
	}
	if env["REDPOINT_API_KEY"] == "" {
		logger.Error("REDPOINT_API_KEY missing from env file")
		os.Exit(2)
	}

	s := &spike{
		graphqlURL:   strings.TrimRight(apiURL, "/") + "/api/graphql",
		apiKey:       env["REDPOINT_API_KEY"],
		facilityCode: env["REDPOINT_FACILITY_CODE"],
		gateID:       env["REDPOINT_GATE_ID"],
		delay:        *delay,
		schemaOut:    *schemaOut,
		logger:       logger,
		now:          time.Now().UTC(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	report := s.run(ctx)

	if *out == "" {
		fmt.Println(report)
		return
	}
	if err := os.WriteFile(*out, []byte(report), 0o644); err != nil {
		logger.Error("cannot write report", "path", *out, "error", err)
		os.Exit(2)
	}
	logger.Info("report written", "path", *out, "requests", s.requests)
}

type spike struct {
	graphqlURL   string
	apiKey       string
	facilityCode string
	gateID       string
	delay        time.Duration
	schemaOut    string
	logger       *slog.Logger
	now          time.Time

	b          strings.Builder
	requests   int
	rateLimits int
	facilities []facilityRow
}

type facilityRow struct {
	ID        string
	ShortName string
	LongName  string
	Timezone  string
	Active    bool
}

// query executes one paced GraphQL query under the given facility header
// (empty string sends no header). It aborts the whole run if pacing has
// failed to prevent repeated 429s — the production bridge shares this key.
func (s *spike) query(ctx context.Context, facility, q string, vars map[string]any) (map[string]any, error) {
	if s.rateLimits >= 2 {
		return nil, fmt.Errorf("aborting: %d rate-limited requests despite pacing", s.rateLimits)
	}
	if s.requests > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.requests++
	client := redpoint.NewClient(s.graphqlURL, s.apiKey, facility, s.logger)
	raw, err := client.ExecQuery(ctx, q, vars)
	if err != nil {
		if strings.Contains(err.Error(), "429") {
			s.rateLimits++
		}
		return nil, err
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return data, nil
}

func (s *spike) run(ctx context.Context) string {
	fmt.Fprintf(&s.b, "# M0 spike results — %s\n\n", s.now.Format("2006-01-02 15:04 UTC"))
	fmt.Fprintf(&s.b, "Endpoint: `%s` · configured facility header: `%s` · pacing: %s between requests.\n\n",
		s.graphqlURL, s.facilityCode, s.delay)
	s.b.WriteString("Output is PII-clean by design: no member names, emails, phones, or birth dates — counts, booleans, and opaque ids only.\n")

	probes := []struct {
		name string
		fn   func(context.Context)
	}{
		{"Introspection", s.probeIntrospection},
		{"Facility topology (M0.0)", s.probeFacilities},
		{"Facility-header read semantics (M0.0)", s.probeHeaderMatrix},
		{"Gate inventory", s.probeGates},
		{"Check-in window + channel completeness (M0.1, M0.2)", s.probeCheckinsWindow},
		{"History-walk pacing (M0.3)", s.probeHistoryWalk},
		{"Customer field exposure (M0.6)", s.probeCustomerFields},
		{"customReports inventory (M0.7)", s.probeCustomReports},
	}
	for _, p := range probes {
		fmt.Fprintf(&s.b, "\n## %s\n\n", p.name)
		p.fn(ctx)
		if s.rateLimits >= 2 {
			s.b.WriteString("\n**RUN ABORTED: repeated 429s despite pacing. Remaining probes skipped.**\n")
			break
		}
	}
	fmt.Fprintf(&s.b, "\n---\nTotal requests: %d · rate-limited: %d\n", s.requests, s.rateLimits)
	return s.b.String()
}

func (s *spike) probeIntrospection(ctx context.Context) {
	data, err := s.query(ctx, s.facilityCode, introspectionQuery, nil)
	if err != nil {
		fmt.Fprintf(&s.b, "Introspection FAILED (likely disabled server-side): `%v`\n", err)
		s.b.WriteString("Falling back to docs-shaped queries for the remaining probes.\n")
		return
	}
	types, _ := dig(data, "__schema", "types").([]any)
	fmt.Fprintf(&s.b, "Introspection ENABLED: %d types returned.\n", len(types))
	if s.schemaOut != "" {
		blob, err := json.MarshalIndent(data, "", "  ")
		if err == nil {
			err = os.WriteFile(s.schemaOut, blob, 0o644)
		}
		if err != nil {
			fmt.Fprintf(&s.b, "(could not save schema to %s: %v)\n", s.schemaOut, err)
		} else {
			fmt.Fprintf(&s.b, "Full schema saved to `%s` — commit it to docs/spike-results/.\n", s.schemaOut)
		}
	}
}

func (s *spike) probeFacilities(ctx context.Context) {
	const q = `query { facilities(first: 50, filter: {active: ALL}) {
		total edges { node { id shortName longName timezone active } } } }`
	data, err := s.query(ctx, s.facilityCode, q, nil)
	if err != nil {
		fmt.Fprintf(&s.b, "FAILED: `%v`\n", err)
		return
	}
	total, _ := dig(data, "facilities", "total").(float64)
	fmt.Fprintf(&s.b, "Org has %.0f facilities:\n\n| id | shortName | longName | timezone | active |\n|---|---|---|---|---|\n", total)
	for _, e := range edges(data, "facilities") {
		f := facilityRow{
			ID:        str(dig(e, "id")),
			ShortName: str(dig(e, "shortName")),
			LongName:  str(dig(e, "longName")),
			Timezone:  str(dig(e, "timezone")),
		}
		f.Active, _ = dig(e, "active").(bool)
		s.facilities = append(s.facilities, f)
		fmt.Fprintf(&s.b, "| %s | %s | %s | %s | %v |\n", f.ID, f.ShortName, f.LongName, f.Timezone, f.Active)
	}
	s.b.WriteString("\nInterpretation: does a LEF facility exist yet? Its presence/absence here settles gymos PLAN.md §1.1.\n")
}

func (s *spike) probeHeaderMatrix(ctx context.Context) {
	const q = `query { customers(filter: {active: ACTIVE}, first: 1) { total } }`
	// Header variants: the configured facility, no header at all, then every
	// facility discovered by the topology probe (skipping the configured one).
	variants := []string{s.facilityCode, ""}
	for _, f := range s.facilities {
		if f.ShortName != s.facilityCode {
			variants = append(variants, f.ShortName)
		}
	}
	s.b.WriteString("`customers(active: ACTIVE) { total }` under each facility header:\n\n| header | total | error |\n|---|---|---|\n")
	totals := map[string]string{}
	for _, v := range variants {
		label := v
		if v == "" {
			label = "(none)"
		}
		data, err := s.query(ctx, v, q, nil)
		if err != nil {
			fmt.Fprintf(&s.b, "| %s | — | `%v` |\n", label, err)
			continue
		}
		t := fmt.Sprintf("%.0f", dig(data, "customers", "total").(float64))
		totals[label] = t
		fmt.Fprintf(&s.b, "| %s | %s | |\n", label, t)
	}
	distinct := map[string]bool{}
	for _, t := range totals {
		distinct[t] = true
	}
	if len(totals) > 1 && len(distinct) == 1 {
		s.b.WriteString("\nInterpretation: identical totals — the facility header does NOT filter customer reads; the production mirror is org-complete.\n")
	} else if len(distinct) > 1 {
		s.b.WriteString("\nInterpretation: totals DIFFER — the facility header filters reads. Directory walks must run per-facility (or drop the header); the production mirror may be facility-partial.\n")
	}
}

func (s *spike) probeGates(ctx context.Context) {
	const q = `query { gates(first: 50, filter: {active: ALL}) {
		edges { node { id name active facility { id shortName } } } } }`
	data, err := s.query(ctx, "", q, nil)
	if err != nil {
		fmt.Fprintf(&s.b, "FAILED (no header — retrying with configured header may be needed): `%v`\n", err)
		return
	}
	s.b.WriteString("| gate id | name | facility | active | notes |\n|---|---|---|---|---|\n")
	for _, e := range edges(data, "gates") {
		id := str(dig(e, "id"))
		note := ""
		if id == s.gateID {
			note = "**bridge's REDPOINT_GATE_ID**"
		}
		fmt.Fprintf(&s.b, "| %s | %s | %s | %v | %s |\n",
			id, str(dig(e, "name")), str(dig(e, "facility", "shortName")), dig(e, "active"), note)
	}
}

func (s *spike) probeCheckinsWindow(ctx context.Context) {
	after, before := window(s.now, 24*time.Hour, 0)
	q := `query($f: CheckInFilter) { checkIns(first: 50, filter: $f) {
		total edges { node { id checkInUtc status facility { shortName } gate { id name } staff { id } } } } }`
	vars := map[string]any{"f": map[string]any{
		"checkInDate": map[string]any{"after": after, "before": before},
	}}
	data, err := s.query(ctx, s.facilityCode, q, vars)
	if err != nil {
		fmt.Fprintf(&s.b, "FAILED: `%v`\n", err)
		return
	}
	total, _ := dig(data, "checkIns", "total").(float64)
	rows := edges(data, "checkIns")
	fmt.Fprintf(&s.b, "Window `%s` → `%s` (UTC): total %.0f check-ins; sampled %d.\n\n", after, before, total, len(rows))

	var minTS, maxTS string
	staffed := 0
	gateCounts := map[string]int{}
	facCounts := map[string]int{}
	for _, e := range rows {
		ts := str(dig(e, "checkInUtc"))
		if minTS == "" || ts < minTS {
			minTS = ts
		}
		if ts > maxTS {
			maxTS = ts
		}
		if dig(e, "staff", "id") != nil {
			staffed++
		}
		gate := fmt.Sprintf("%s (%s)", str(dig(e, "gate", "name")), str(dig(e, "gate", "id")))
		gateCounts[gate]++
		facCounts[str(dig(e, "facility", "shortName"))]++
	}
	fmt.Fprintf(&s.b, "- Sampled checkInUtc range: `%s` → `%s` (bounds respected: %v)\n", minTS, maxTS, minTS >= after && maxTS <= before)
	fmt.Fprintf(&s.b, "- Staff-attributed (desk-channel) check-ins in sample: %d/%d — nonzero proves the read covers RPHQ's own desk surface (M0.2)\n", staffed, len(rows))
	s.b.WriteString("- Gate distribution (bridge gate vs others reveals channels):\n")
	for _, k := range sortedKeys(gateCounts) {
		marker := ""
		if strings.Contains(k, "("+s.gateID+")") {
			marker = " ← bridge"
		}
		fmt.Fprintf(&s.b, "  - %s: %d%s\n", k, gateCounts[k], marker)
	}
	s.b.WriteString("- Facility distribution:\n")
	for _, k := range sortedKeys(facCounts) {
		fmt.Fprintf(&s.b, "  - %s: %d\n", k, facCounts[k])
	}

	// facilityId filter behavior, if the topology probe found facilities.
	if len(s.facilities) > 0 {
		vars := map[string]any{"f": map[string]any{
			"checkInDate": map[string]any{"after": after, "before": before},
			"facilityId":  []string{s.facilities[0].ID},
		}}
		data, err := s.query(ctx, s.facilityCode, q, vars)
		if err != nil {
			fmt.Fprintf(&s.b, "\nfacilityId-filter probe FAILED: `%v`\n", err)
			return
		}
		ft, _ := dig(data, "checkIns", "total").(float64)
		fmt.Fprintf(&s.b, "\nSame window filtered to facilityId=%s (%s): total %.0f (vs %.0f unfiltered).\n",
			s.facilities[0].ID, s.facilities[0].ShortName, ft, total)
	}
}

func (s *spike) probeHistoryWalk(ctx context.Context) {
	after, before := window(s.now, 35*24*time.Hour, 28*24*time.Hour)
	q := `query($f: CheckInFilter, $after: String) { checkIns(first: 100, after: $after, filter: $f) {
		total pageInfo { hasNextPage endCursor } edges { node { id } } } }`
	filter := map[string]any{"checkInDate": map[string]any{"after": after, "before": before}}

	start := time.Now()
	var cursor any
	pages, fetched := 0, 0
	var total float64
	for pages < 3 {
		vars := map[string]any{"f": filter}
		if cursor != nil {
			vars["after"] = cursor
		}
		data, err := s.query(ctx, s.facilityCode, q, vars)
		if err != nil {
			fmt.Fprintf(&s.b, "FAILED on page %d: `%v`\n", pages+1, err)
			return
		}
		pages++
		total, _ = dig(data, "checkIns", "total").(float64)
		fetched += len(edges(data, "checkIns"))
		more, _ := dig(data, "checkIns", "pageInfo", "hasNextPage").(bool)
		if !more {
			break
		}
		cursor = dig(data, "checkIns", "pageInfo", "endCursor")
	}
	elapsed := time.Since(start)
	fmt.Fprintf(&s.b, "Historical window `%s` → `%s`: total %.0f check-ins; walked %d pages (%d rows) in %s at %s pacing.\n",
		after, before, total, pages, fetched, elapsed.Round(time.Millisecond), s.delay)
	if pages > 0 {
		perPage := elapsed / time.Duration(pages)
		fmt.Fprintf(&s.b, "≈ %s/page of 100 → a 100k-check-in backfill ≈ %s. Extrapolate against real history depth for the M1 baseline import estimate.\n",
			perPage.Round(time.Millisecond), (perPage * 1000).Round(time.Minute))
	}
}

func (s *spike) probeCustomerFields(ctx context.Context) {
	// Requests only presence-checkable fields; the report never prints
	// values. Names/emails are deliberately not requested at all.
	const q = `query { customers(filter: {active: ACTIVE}, first: 3) {
		edges { node { id mobilePhone otherPhone dateOfBirth doNotMail
			mobilePhoneMarketingOptIn mobilePhoneTransactionOptIn updatedAt createdAt } } } }`
	data, err := s.query(ctx, s.facilityCode, q, nil)
	if err != nil {
		fmt.Fprintf(&s.b, "Validation FAILED — fields named in the error are not exposed: `%v`\n", err)
		return
	}
	rows := edges(data, "customers")
	fields := []string{"mobilePhone", "otherPhone", "dateOfBirth", "doNotMail",
		"mobilePhoneMarketingOptIn", "mobilePhoneTransactionOptIn", "updatedAt", "createdAt"}
	fmt.Fprintf(&s.b, "All doc-claimed fields validated. Non-null presence across %d sampled customers (values withheld — PII):\n\n| field | non-null |\n|---|---|\n", len(rows))
	for _, f := range fields {
		n := 0
		for _, e := range rows {
			if dig(e, f) != nil {
				n++
			}
		}
		fmt.Fprintf(&s.b, "| %s | %d/%d |\n", f, n, len(rows))
	}
}

func (s *spike) probeCustomReports(ctx context.Context) {
	const q = `query { customReports(first: 50, filter: {active: ALL}) {
		total edges { node { id group name description active } } } }`
	data, err := s.query(ctx, s.facilityCode, q, nil)
	if err != nil {
		fmt.Fprintf(&s.b, "FAILED: `%v`\n", err)
		return
	}
	total, _ := dig(data, "customReports", "total").(float64)
	fmt.Fprintf(&s.b, "%.0f custom reports defined:\n\n| id | group | name | description | active |\n|---|---|---|---|---|\n", total)
	for _, e := range edges(data, "customReports") {
		fmt.Fprintf(&s.b, "| %s | %s | %s | %s | %v |\n",
			str(dig(e, "id")), str(dig(e, "group")), str(dig(e, "name")),
			strings.ReplaceAll(str(dig(e, "description")), "\n", " "), dig(e, "active"))
	}
}

// ─── helpers ─────────────────────────────────────────────────

// parseEnvFile reads KEY=VALUE lines, ignoring comments and blanks, and
// strips single/double quotes around values. Deliberately minimal — the
// bridge's .env is machine-managed and uses no shell interpolation.
func parseEnvFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		env[strings.TrimSpace(k)] = v
	}
	return env, nil
}

// rphqDateTime is the DateTime input format the server actually accepts.
// Empirically verified 2026-08-02: RFC3339 ("2026-08-01T22:23:29Z") is
// REJECTED with `DateTime must be a UTC date formatted as such:
// 2000-01-01 00:00:00`, despite the public docs showing an RFC3339
// example. Space-separated, no zone suffix, parsed as UTC.
const rphqDateTime = "2006-01-02 15:04:05"

// window returns [now-back, now-until] as UTC strings in the server's
// required DateTimeFilter format.
func window(now time.Time, back, until time.Duration) (after, before string) {
	return now.Add(-back).Format(rphqDateTime), now.Add(-until).Format(rphqDateTime)
}

// dig walks nested map[string]any by key path, returning nil on any miss.
func dig(v any, path ...string) any {
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// edges returns the node objects of data.<root>.edges[].
func edges(data map[string]any, root string) []any {
	raw, _ := dig(data, root, "edges").([]any)
	nodes := make([]any, 0, len(raw))
	for _, e := range raw {
		if n := dig(e, "node"); n != nil {
			nodes = append(nodes, n)
		}
	}
	return nodes
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
