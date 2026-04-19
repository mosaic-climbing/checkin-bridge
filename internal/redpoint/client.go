// Package redpoint implements the Redpoint HQ GraphQL API client.
//
// API docs: https://portal.redpointhq.com/docs/api/v1/
// Endpoint: https://{ORG}.rphq.com/api/graphql
// Auth:     Authorization: Bearer {token}
// Facility: X-Redpoint-HQ-Facility: {3-letter code}
package redpoint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/metrics"
)

// FlexFloat handles JSON values that may be a number or a string containing a number.
type FlexFloat float64

func (f *FlexFloat) UnmarshalJSON(data []byte) error {
	// Try number first
	var n float64
	if err := json.Unmarshal(data, &n); err == nil {
		*f = FlexFloat(n)
		return nil
	}
	// Try string
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		if s == "" {
			*f = 0
			return nil
		}
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		*f = FlexFloat(n)
		return nil
	}
	return fmt.Errorf("cannot unmarshal %s into float64", string(data))
}

// Client talks to the Redpoint HQ GraphQL API.
type Client struct {
	graphqlURL   string
	apiKey       string
	facilityCode string
	httpClient   *http.Client
	logger       *slog.Logger

	// metrics is optional. When set, exec() records per-request duration
	// and per-outcome (success|error) counts. Wired from cmd/bridge via
	// SetMetrics after client construction so the redpoint package has no
	// construction-time dependency on internal/metrics — tests and ad-hoc
	// uses can leave it nil and the instrumentation is a no-op.
	metrics *metrics.Registry
}

func NewClient(graphqlURL, apiKey, facilityCode string, logger *slog.Logger) *Client {
	return &Client{
		graphqlURL:   graphqlURL,
		apiKey:       apiKey,
		facilityCode: facilityCode,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		logger:       logger,
	}
}

// SetMetrics attaches a metrics registry. Safe to call once during wiring
// (cmd/bridge/main.go). Nil is tolerated and disables instrumentation.
//
// A5 (architecture review): we want a histogram of Redpoint request
// latency so operators can see "Redpoint is slow" before it becomes
// "Redpoint timed out and the breaker tripped". The GraphQL client
// funnels every call through exec(), so a single timing point covers
// LookupByExternalID, CreateCheckIn, RefreshCustomers, SearchCustomersByName,
// CustomersByEmail, ListAllActiveCustomers, ListGates, ListRecentCheckIns,
// and any ad-hoc ExecQuery use — no per-method wiring needed.
func (c *Client) SetMetrics(m *metrics.Registry) { c.metrics = m }

// redpointLatencyBuckets is the bucket set requested by the A5
// observability plan: [50ms, 100ms, 250ms, 500ms, 1s, 5s]. These cover
// the two regimes the gym operator cares about:
//
//   - healthy (≤250ms): sub-second round-trip, door feels instant.
//   - degraded (250ms–5s): Redpoint slowing down but still answering.
//     Above 5s the request has almost certainly been abandoned by the
//     caller's 10s context before the result lands, so a coarser +Inf
//     bucket is sufficient past that point.
var redpointLatencyBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 5}

// observeDuration records the elapsed time since start into the
// histogram and bumps the matching outcome counter. Safe to call with
// c.metrics == nil (no-op). Named so exec() reads clean at the top.
func (c *Client) observeDuration(start time.Time, err error) {
	if c.metrics == nil {
		return
	}
	elapsed := time.Since(start).Seconds()
	c.metrics.Histogram("redpoint_request_duration_seconds", redpointLatencyBuckets).Observe(elapsed)
	if err == nil {
		c.metrics.Counter("redpoint_requests_total").Inc()
	} else {
		c.metrics.Counter("redpoint_request_errors_total").Inc()
	}
}

// ─── Domain Types ────────────────────────────────────────────

type Customer struct {
	ID             string       `json:"id"`
	Active         bool         `json:"active"`
	FirstName      string       `json:"firstName"`
	LastName       string       `json:"lastName"`
	Email          string       `json:"email"`
	Barcode        string       `json:"barcode"`
	ExternalID     string       `json:"externalId"`
	LastVisitDate  *string      `json:"lastVisitDate"`
	AccountBalance FlexFloat    `json:"accountBalance"`
	DueBalance     FlexFloat    `json:"dueBalance"`
	PastDueBalance FlexFloat    `json:"pastDueBalance"`
	CreditBalance  FlexFloat    `json:"creditBalance"`
	HomeFacility   *Facility    `json:"homeFacility"`
	Badge          *BadgeStatus `json:"badge"`
}

func (c *Customer) FullName() string {
	return c.FirstName + " " + c.LastName
}

type BadgeStatus struct {
	Status        string         `json:"status"` // ACTIVE, FROZEN, EXPIRED
	CustomerBadge *CustomerBadge `json:"customerBadge"`
}

type CustomerBadge struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	BackgroundColor string `json:"backgroundColor"`
	TextColor       string `json:"textColor"`
}

type Facility struct {
	ID        string `json:"id"`
	ShortName string `json:"shortName"`
	LongName  string `json:"longName"`
}

type Gate struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Active   bool      `json:"active"`
	Facility *Facility `json:"facility"`
}

type CheckIn struct {
	ID            string         `json:"id"`
	Status        string         `json:"status"` // OK, WARN, ALERT
	CheckInUTC    string         `json:"checkInUtc"`
	CheckOutUTC   *string        `json:"checkOutUtc"`
	Customer      *Customer      `json:"customer"`
	Gate          *Gate          `json:"gate"`
	CustomerBadge *CustomerBadge `json:"customerBadge"`
	Facility      *Facility      `json:"facility"`
}

// Validation result for a check-in attempt.
type ValidationResult struct {
	Valid     bool
	Reason    string
	BadgeName string
}

// CheckInResult from the createCheckIn mutation.
type CheckInResult struct {
	Success   bool
	RecordID  string
	Status    string
	CheckInAt string
	Duplicate bool
	Error     string
}

// ─── GraphQL Transport ───────────────────────────────────────

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (c *Client) exec(ctx context.Context, query string, vars map[string]any) (retData json.RawMessage, retErr error) {
	// Instrument every GraphQL call — exec is the sole transport path so
	// timing here captures LookupByExternalID / CreateCheckIn / everything.
	// observeDuration is nil-safe so tests + ad-hoc uses without metrics
	// wired up cost only the time.Now() call.
	start := time.Now()
	defer func() { c.observeDuration(start, retErr) }()

	body, err := json.Marshal(gqlRequest{Query: query, Variables: vars})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.graphqlURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if c.facilityCode != "" {
		req.Header.Set("X-Redpoint-HQ-Facility", c.facilityCode)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Wrap so retryable() can identify a true transport failure
		// (DNS / TCP / TLS / read / write) regardless of the underlying
		// concrete type. ctx.Err() preserves Is(ctx.Canceled / Deadline)
		// on the chain so we can refuse to retry on cancellation.
		return nil, &transportError{Err: fmt.Errorf("http request: %w", err)}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10MB limit
	if err != nil {
		// Body read mid-flight is also transport — server may be flapping.
		return nil, &transportError{Err: fmt.Errorf("read response: %w", err)}
	}

	if resp.StatusCode != http.StatusOK {
		// Typed so retryable() can decide: 429 + 5xx retry, other 4xx don't.
		// RetryAfter captures the server's explicit hint (RFC 7231 §7.1.3)
		// so the retry loop can honour it as a wait-floor — critical for
		// the undocumented Redpoint rate limiter, which is the only channel
		// telling us "don't come back for N seconds". We parse regardless
		// of status; the loop only reads it on retryable errors.
		return nil, &httpError{
			Status:     resp.StatusCode,
			Body:       string(respBody),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	var gqlResp gqlResponse
	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if len(gqlResp.Errors) > 0 {
		msgs := make([]string, len(gqlResp.Errors))
		for i, e := range gqlResp.Errors {
			msgs[i] = e.Message
		}
		return nil, fmt.Errorf("graphql errors: %v", msgs)
	}

	return gqlResp.Data, nil
}

// ─── Queries ─────────────────────────────────────────────────

const customerByExternalIDQuery = `
query CustomerByExternalId($externalId: String!) {
  customerByExternalId(externalId: $externalId) {
    id active firstName lastName email barcode externalId
    lastVisitDate accountBalance dueBalance pastDueBalance creditBalance
    homeFacility { id shortName longName }
    badge {
      status
      customerBadge { id name backgroundColor textColor }
    }
  }
}`

// LookupByExternalID finds a customer by their external ID (NFC card UID).
func (c *Client) LookupByExternalID(ctx context.Context, externalID string) (*Customer, error) {
	c.logger.Debug("looking up customer by externalId", "externalId", externalID)

	data, err := c.execWithRetry(ctx, customerByExternalIDQuery, map[string]any{
		"externalId": externalID,
	})
	if err != nil {
		return nil, fmt.Errorf("lookup by external id: %w", err)
	}

	var result struct {
		CustomerByExternalID *Customer `json:"customerByExternalId"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("unmarshal customer: %w", err)
	}

	if result.CustomerByExternalID == nil {
		c.logger.Info("no customer found", "externalId", externalID)
		return nil, nil
	}

	cust := result.CustomerByExternalID
	c.logger.Info("customer found",
		"id", cust.ID,
		"name", cust.FullName(),
		"badgeStatus", cust.Badge.Status,
	)
	return cust, nil
}

const customerByIDQuery = `
query Customer($id: ID!) {
  customer(id: $id) {
    id active firstName lastName email barcode externalId
    lastVisitDate accountBalance dueBalance pastDueBalance
    badge {
      status
      customerBadge { id name }
    }
  }
}`

// GetCustomer fetches a customer by their Redpoint ID.
func (c *Client) GetCustomer(ctx context.Context, id string) (*Customer, error) {
	data, err := c.execWithRetry(ctx, customerByIDQuery, map[string]any{"id": id})
	if err != nil {
		return nil, err
	}

	var result struct {
		Customer *Customer `json:"customer"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result.Customer, nil
}

// ValidateCheckIn checks whether a customer's membership allows entry.
func (c *Client) ValidateCheckIn(cust *Customer) ValidationResult {
	if cust == nil {
		return ValidationResult{Valid: false, Reason: "Customer not found"}
	}
	if !cust.Active {
		return ValidationResult{Valid: false, Reason: "Customer account is inactive"}
	}

	if cust.Badge == nil {
		return ValidationResult{Valid: false, Reason: "No membership badge"}
	}

	switch cust.Badge.Status {
	case "EXPIRED":
		return ValidationResult{Valid: false, Reason: "Membership expired"}
	case "FROZEN":
		return ValidationResult{Valid: false, Reason: "Membership is frozen"}
	case "ACTIVE":
		// ok
	default:
		return ValidationResult{Valid: false, Reason: fmt.Sprintf("Badge status: %s", cust.Badge.Status)}
	}

	if float64(cust.PastDueBalance) > 0 {
		return ValidationResult{
			Valid:  false,
			Reason: fmt.Sprintf("Past-due balance: $%.2f", float64(cust.PastDueBalance)),
		}
	}

	badgeName := "Unknown"
	if cust.Badge.CustomerBadge != nil {
		badgeName = cust.Badge.CustomerBadge.Name
	}

	return ValidationResult{Valid: true, Reason: "Active membership", BadgeName: badgeName}
}

// ─── Mutations ───────────────────────────────────────────────

const createCheckInMutation = `
mutation CreateCheckIn($input: CreateCheckInInput!) {
  createCheckIn(input: $input) {
    ... on CreateCheckInResult {
      __typename
      recordId
      record { id status checkInUtc facility { id shortName } gate { id name } customer { id firstName lastName } customerBadge { id name } }
    }
    ... on DuplicateCheckInResult {
      __typename
      recordId
      record { id status checkInUtc }
    }
    ... on CreateCheckInCustomerNotFound {
      __typename
    }
  }
}`

// CreateCheckIn records a check-in in Redpoint. Provide customerId OR barcode (not both).
func (c *Client) CreateCheckIn(ctx context.Context, gateID, customerID, barcode string) (*CheckInResult, error) {
	c.logger.Info("creating check-in", "gateId", gateID, "customerId", customerID)

	input := map[string]any{"gateId": gateID}
	if customerID != "" {
		input["customerId"] = customerID
	} else if barcode != "" {
		input["barcode"] = barcode
	}

	data, err := c.execWithRetry(ctx, createCheckInMutation, map[string]any{"input": input})
	if err != nil {
		return nil, fmt.Errorf("create check-in: %w", err)
	}

	// Parse the union type response
	var raw struct {
		CreateCheckIn json.RawMessage `json:"createCheckIn"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal check-in response: %w", err)
	}

	var typename struct {
		Typename string `json:"__typename"`
	}
	if err := json.Unmarshal(raw.CreateCheckIn, &typename); err != nil {
		return nil, fmt.Errorf("unmarshal typename: %w", err)
	}

	switch typename.Typename {
	case "CreateCheckInCustomerNotFound":
		return &CheckInResult{Success: false, Error: "Customer not found"}, nil

	case "DuplicateCheckInResult":
		var dup struct {
			RecordID string   `json:"recordId"`
			Record   *CheckIn `json:"record"`
		}
		json.Unmarshal(raw.CreateCheckIn, &dup)
		c.logger.Info("duplicate check-in (within 15s)", "recordId", dup.RecordID)
		return &CheckInResult{
			Success:   true,
			RecordID:  dup.RecordID,
			Status:    dup.Record.Status,
			CheckInAt: dup.Record.CheckInUTC,
			Duplicate: true,
		}, nil

	default: // CreateCheckInResult
		var res struct {
			RecordID string   `json:"recordId"`
			Record   *CheckIn `json:"record"`
		}
		json.Unmarshal(raw.CreateCheckIn, &res)
		status := ""
		checkInAt := ""
		if res.Record != nil {
			status = res.Record.Status
			checkInAt = res.Record.CheckInUTC
		}
		c.logger.Info("check-in recorded", "recordId", res.RecordID, "status", status)
		return &CheckInResult{
			Success:   true,
			RecordID:  res.RecordID,
			Status:    status,
			CheckInAt: checkInAt,
		}, nil
	}
}

// ─── Public Query Executor ───────────────────────────────────

// ExecQuery runs a GraphQL query with variables and returns the raw JSON data.
// This is the public wrapper around exec with retry logic, used by the
// server's directory sync adapter to page through Redpoint customers.
func (c *Client) ExecQuery(ctx context.Context, query string, vars map[string]any) (json.RawMessage, error) {
	return c.execWithRetry(ctx, query, vars)
}

// ─── Shared Retry Logic ──────────────────────────────────────

// httpError is returned by exec() when the Redpoint server replies with a
// non-2xx HTTP status. The retry wrapper inspects Status via errors.As to
// classify retryable (429 / 5xx) vs permanent (other 4xx — auth, validation,
// not found). Body is captured for logging only.
//
// RetryAfter holds the parsed value of the response's Retry-After header
// (RFC 7231 §7.1.3) — either "delta-seconds" or an HTTP-date converted to
// a duration. Zero means no hint was present or the header was malformed.
// The retry loop reads this field to use max(backoff, RetryAfter) as the
// wait before the next attempt, so a polite server that says "come back
// in 10s" gets 10s instead of our default 200ms × 2^n.
type httpError struct {
	Status     int
	Body       string
	RetryAfter time.Duration
}

func (e *httpError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// transportError flags failures at the HTTP transport layer (DNS, TCP,
// TLS handshake, idle connection, mid-stream read). These are always
// transient by nature, so the retry wrapper retries them unconditionally —
// except when the underlying error is a context cancellation/deadline,
// which retryable() detects via errors.Is on the unwrapped chain and
// declines to retry (no point waiting if the caller has already given up).
type transportError struct {
	Err error
}

func (e *transportError) Error() string { return e.Err.Error() }
func (e *transportError) Unwrap() error { return e.Err }

// Retry policy. Tests override backoffBase to keep total runtime small.
//
// Three attempts is the sweet spot for an interactive check-in path: at
// 200ms / 400ms / 800ms base wait, a worst-case retry burst still finishes
// inside the 10-second context most callers carry, while two retries past
// the first attempt is enough to ride out a single transient blip without
// piling on during a sustained outage (the breaker covers that).
var (
	maxAttempts = 3
	backoffBase = 200 * time.Millisecond
)

// backoffFor returns the wait before the (1-indexed) attempt N with ±25%
// jitter applied to an exponentially-doubling base. Jitter avoids the
// thundering-herd shape you get when many concurrent denied taps all retry
// in lockstep after the same upstream blip.
//
//	attempt 1 → ~200ms
//	attempt 2 → ~400ms
//	attempt 3 → ~800ms
func backoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := backoffBase << (attempt - 1) // 1×, 2×, 4×, ...
	// Symmetric jitter in [-25%, +25%]. rand/v2's global generator is
	// fine here — we don't need cryptographic unpredictability, just
	// per-process diversity to spread out concurrent retries.
	jitter := time.Duration(rand.Int64N(int64(base/2))) - base/4
	return base + jitter
}

// retryable classifies an error from exec() as transient (worth a retry)
// vs permanent (caller should give up immediately).
//
// Retry on:
//   - *transportError — network-layer failure (not ctx.Cancel/Deadline).
//   - *httpError with Status == 429 (rate-limited).
//   - *httpError with Status >= 500 (server-side failure).
//
// Don't retry on:
//   - Anything else — 4xx (auth, validation, not-found), GraphQL
//     application errors, JSON marshal/unmarshal failures, programmer bugs.
//   - context.Canceled / context.DeadlineExceeded anywhere on the chain —
//     the caller is already done waiting, sleeping further is wasted time.
func retryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var herr *httpError
	if errors.As(err, &herr) {
		return herr.Status == http.StatusTooManyRequests || herr.Status >= 500
	}
	var terr *transportError
	return errors.As(err, &terr)
}

// execWithRetry wraps exec with classified retry: exponential backoff +
// jitter, up to maxAttempts. The retry layer is the fast inner loop;
// callers that want a slow outer guard against sustained outages should
// stack a circuit breaker on top (see internal/statusync.breaker).
//
// CreateCheckIn safety note: retrying mutations is acceptable here
// because Redpoint's createCheckIn returns a typed DuplicateCheckInResult
// for any duplicate within 15s, so an accidental double-write produced by
// "request landed but response lost" is observable and harmless.
func (c *Client) execWithRetry(ctx context.Context, query string, vars map[string]any) (json.RawMessage, error) {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Honour cancellation before each new attempt so a caller whose
		// context expired during the previous wait gets a clean ctx.Err()
		// rather than a stale upstream error.
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}

		data, err := c.exec(ctx, query, vars)
		if err == nil {
			return data, nil
		}
		if !retryable(err) {
			return nil, err
		}
		lastErr = err

		if attempt == maxAttempts {
			break // last attempt — don't wait, return below
		}
		wait := backoffFor(attempt)
		// Honour a server-supplied Retry-After as a wait-floor. The
		// Redpoint rate limiter is undocumented, so a 429 carrying a
		// hint is the only authoritative signal we get; ignoring it in
		// favour of our ~200ms–800ms exponential would guarantee another
		// 429. We take max(backoff, Retry-After) rather than replacing
		// outright so a server that sends "Retry-After: 0" still gets a
		// minimum jittered backoff (prevents a tight hot-loop reconnect).
		var herr *httpError
		if errors.As(err, &herr) && herr.RetryAfter > wait {
			c.logger.Info("honouring Retry-After hint",
				"status", herr.Status, "hint", herr.RetryAfter, "baseWait", wait)
			wait = herr.RetryAfter
		}
		c.logger.Warn("retryable redpoint error, backing off",
			"attempt", attempt, "of", maxAttempts, "wait", wait, "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, lastErr
}

// ─── Targeted Customer Refresh (for daily sync) ──────────────

// RefreshCustomers fetches fresh data for a list of known customer IDs.
// This replaces the bulk ListAllActiveCustomers for daily syncs —
// instead of paginating through 54k+ records, we refresh only the
// ~100 members already in our cache.
func (c *Client) RefreshCustomers(ctx context.Context, customerIDs []string) ([]*Customer, error) {
	c.logger.Info("refreshing cached customers", "count", len(customerIDs))

	var refreshed []*Customer
	var errors []string

	for i, id := range customerIDs {
		// Rate-limit: small delay between requests
		if i > 0 && i%10 == 0 {
			select {
			case <-ctx.Done():
				return refreshed, ctx.Err()
			case <-time.After(1 * time.Second):
			}
		}

		data, err := c.execWithRetry(ctx, customerByIDQuery, map[string]any{"id": id})
		if err != nil {
			c.logger.Warn("failed to refresh customer", "id", id, "error", err)
			errors = append(errors, id)
			continue
		}

		var result struct {
			Customer *Customer `json:"customer"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			c.logger.Warn("failed to unmarshal customer", "id", id, "error", err)
			errors = append(errors, id)
			continue
		}

		if result.Customer != nil {
			refreshed = append(refreshed, result.Customer)
		} else {
			c.logger.Info("customer no longer exists in Redpoint", "id", id)
		}
	}

	c.logger.Info("customer refresh complete",
		"requested", len(customerIDs),
		"refreshed", len(refreshed),
		"errors", len(errors),
	)
	return refreshed, nil
}

// ─── Name-Based Customer Search (for UniFi-first ingest) ─────

const searchCustomersByNameQuery = `
query SearchCustomers($filter: CustomerFilter!, $first: Int) {
  customers(filter: $filter, first: $first) {
    edges {
      node {
        id active firstName lastName email barcode externalId
        badge {
          status
          customerBadge { id name }
        }
      }
    }
  }
}`

// SearchCustomersByName tries to find customers matching a name.
// It attempts the "search" filter field first — if the Redpoint API
// doesn't support it, it returns an error (callers should fall back
// to the bulk approach or manual matching).
func (c *Client) SearchCustomersByName(ctx context.Context, firstName, lastName string) ([]*Customer, error) {
	// Redpoint search uses "Last, First" format
	searchTerm := strings.TrimSpace(lastName + ", " + firstName)
	if lastName == "" {
		searchTerm = strings.TrimSpace(firstName)
	}
	c.logger.Debug("searching customers by name", "search", searchTerm)

	vars := map[string]any{
		"filter": map[string]any{
			"active": "ACTIVE",
			"search": searchTerm,
		},
		"first": 10,
	}

	data, err := c.execWithRetry(ctx, searchCustomersByNameQuery, vars)
	if err != nil {
		return nil, fmt.Errorf("search customers: %w", err)
	}

	var result struct {
		Customers struct {
			Edges []struct {
				Node Customer `json:"node"`
			} `json:"edges"`
		} `json:"customers"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("unmarshal search results: %w", err)
	}

	customers := make([]*Customer, len(result.Customers.Edges))
	for i := range result.Customers.Edges {
		customers[i] = &result.Customers.Edges[i].Node
	}
	return customers, nil
}

const customersByEmailQuery = `
query CustomersByEmail($filter: CustomerFilter!, $first: Int) {
  customers(filter: $filter, first: $first) {
    edges {
      node {
        id active firstName lastName email barcode externalId
        badge {
          status
          customerBadge { id name }
        }
      }
    }
  }
}`

// CustomersByEmail returns Redpoint customers whose email field matches the
// given address. Returns up to `first` matches (default 10) — the caller
// must be prepared to receive multiple rows because households commonly
// share an email between a parent's and a child's account, and the C2
// matching algorithm relies on seeing the full collision set so it can
// disambiguate by name locally.
//
// Empty email returns (nil, nil) without making a request — the caller's
// "email present" branch should not have routed here at all, but guarding
// here keeps the error mode obvious if it ever does.
//
// `active: ALL` is passed so an inactive Redpoint customer that's still
// holding a UA-Hub card surfaces as a match. The bridge will then
// deactivate them in UA-Hub on the writeback step. Filtering to ACTIVE
// here would silently let lapsed members keep tapping in.
func (c *Client) CustomersByEmail(ctx context.Context, email string, first int) ([]*Customer, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return nil, nil
	}
	if first <= 0 {
		first = 10
	}
	c.logger.Debug("looking up customers by email", "email", email, "first", first)

	vars := map[string]any{
		"filter": map[string]any{
			"active": "ALL",
			"email":  email,
		},
		"first": first,
	}

	data, err := c.execWithRetry(ctx, customersByEmailQuery, vars)
	if err != nil {
		return nil, fmt.Errorf("customers by email: %w", err)
	}

	var result struct {
		Customers struct {
			Edges []struct {
				Node Customer `json:"node"`
			} `json:"edges"`
		} `json:"customers"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("unmarshal email-match results: %w", err)
	}

	customers := make([]*Customer, len(result.Customers.Edges))
	for i := range result.Customers.Edges {
		customers[i] = &result.Customers.Edges[i].Node
	}
	return customers, nil
}

// ─── Bulk Customer Fetch (legacy, for initial bootstrap) ─────

const listActiveCustomersQuery = `
query Customers($filter: CustomerFilter!, $first: Int, $after: String) {
  customers(filter: $filter, first: $first, after: $after) {
    pageInfo { hasNextPage endCursor }
    edges {
      node {
        id active firstName lastName email barcode externalId
        badge {
          status
          customerBadge { id name }
        }
      }
    }
  }
}`

// ListAllActiveCustomers pages through all active customers in Redpoint.
// WARNING: This can be very slow (54k+ records) and may hit rate limits.
// Prefer RefreshCustomers for daily syncs and SearchCustomersByName for ingest.
// This is kept as a fallback for initial bootstrap only.
func (c *Client) ListAllActiveCustomers(ctx context.Context, pageSize int) ([]*Customer, error) {
	if pageSize <= 0 {
		pageSize = 100
	}

	var all []*Customer
	var cursor *string
	page := 0

	for {
		page++

		// Rate-limit: pause between pages to avoid 429s from Redpoint
		if page > 1 {
			select {
			case <-ctx.Done():
				return all, ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}

		vars := map[string]any{
			"filter": map[string]any{"active": "ACTIVE"},
			"first":  pageSize,
		}
		if cursor != nil {
			vars["after"] = *cursor
		}

		c.logger.Info("fetching customer page", "page", page, "pageSize", pageSize)

		data, err := c.execWithRetry(ctx, listActiveCustomersQuery, vars)
		if err != nil {
			return all, fmt.Errorf("list customers page %d: %w", page, err)
		}

		var result struct {
			Customers struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Edges []struct {
					Node Customer `json:"node"`
				} `json:"edges"`
			} `json:"customers"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			return all, fmt.Errorf("unmarshal customers page %d: %w", page, err)
		}

		for i := range result.Customers.Edges {
			all = append(all, &result.Customers.Edges[i].Node)
		}

		c.logger.Info("fetched customer page", "page", page, "count", len(result.Customers.Edges), "total", len(all))

		if !result.Customers.PageInfo.HasNextPage {
			break
		}
		cursor = &result.Customers.PageInfo.EndCursor
	}

	c.logger.Info("fetched all active customers", "total", len(all), "pages", page)
	return all, nil
}

// ─── Gate Discovery ──────────────────────────────────────────

const listGatesQuery = `
query Gates($filter: GateFilter!) {
  gates(first: 50, filter: $filter) {
    edges {
      node { id name active facility { id shortName longName } }
    }
  }
}`

// ListGates returns all active gates for the facility.
func (c *Client) ListGates(ctx context.Context) ([]Gate, error) {
	filter := map[string]any{"active": "ACTIVE"}

	data, err := c.execWithRetry(ctx, listGatesQuery, map[string]any{"filter": filter})
	if err != nil {
		return nil, err
	}

	var result struct {
		Gates struct {
			Edges []struct {
				Node Gate `json:"node"`
			} `json:"edges"`
		} `json:"gates"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}

	gates := make([]Gate, len(result.Gates.Edges))
	for i, e := range result.Gates.Edges {
		gates[i] = e.Node
	}
	return gates, nil
}

// ─── Recent Check-ins ────────────────────────────────────────

const listCheckInsQuery = `
query RecentCheckIns($first: Int) {
  checkIns(first: $first) {
    edges {
      node {
        id status checkInUtc checkOutUtc
        customer { id firstName lastName }
        gate { id name }
        customerBadge { name }
      }
    }
    total
  }
}`

type CheckInList struct {
	CheckIns []CheckIn `json:"checkIns"`
	Total    int       `json:"total"`
}

// ListRecentCheckIns returns the most recent check-ins.
func (c *Client) ListRecentCheckIns(ctx context.Context, limit int) (*CheckInList, error) {
	data, err := c.execWithRetry(ctx, listCheckInsQuery, map[string]any{"first": limit})
	if err != nil {
		return nil, err
	}

	var result struct {
		CheckIns struct {
			Edges []struct {
				Node CheckIn `json:"node"`
			} `json:"edges"`
			Total int `json:"total"`
		} `json:"checkIns"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}

	list := &CheckInList{Total: result.CheckIns.Total}
	for _, e := range result.CheckIns.Edges {
		list.CheckIns = append(list.CheckIns, e.Node)
	}
	return list, nil
}
