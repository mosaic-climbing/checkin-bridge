package redpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// customerServer answers customer(id:) lookups. failFirst[id] = how many
// requests for that id to fail with 503 before answering; always503
// fails everything.
type customerServer struct {
	mu        sync.Mutex
	failFirst map[string]int
	always503 bool
	requests  int
	*httptest.Server
}

func newCustomerServer(t *testing.T) *customerServer {
	t.Helper()
	cs := &customerServer{failFirst: map[string]int{}}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		id, _ := req.Variables["id"].(string)
		cs.mu.Lock()
		cs.requests++
		fail := cs.always503
		if n := cs.failFirst[id]; n > 0 {
			cs.failFirst[id] = n - 1
			fail = true
		}
		cs.mu.Unlock()
		if fail {
			http.Error(w, "upstream sad", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"customer":{"id":%q,"active":true,"firstName":"F","lastName":"L","badge":{"status":"ACTIVE","customerBadge":{"id":"b","name":"Member"}}}}}`, id)
	}))
	t.Cleanup(cs.Server.Close)
	return cs
}

func (cs *customerServer) count() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.requests
}

func TestRefreshCustomers_SecondPassRecoversTransientFailure(t *testing.T) {
	withFastBackoff(t)
	srv := newCustomerServer(t)
	srv.failFirst["flaky"] = maxAttempts // exhausts the first pass, then recovers

	c := newClientFor(t, srv.URL)
	c.SetRateLimit(0, 1)
	out, err := c.RefreshCustomers(context.Background(), []string{"a", "flaky", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.FailedIDs) != 0 {
		t.Fatalf("FailedIDs = %v, want none after the second pass", out.FailedIDs)
	}
	if len(out.Customers) != 3 {
		t.Fatalf("Customers = %d, want 3", len(out.Customers))
	}
}

func TestRefreshCustomers_PermanentFailureStaysFailed(t *testing.T) {
	withFastBackoff(t)
	srv := newCustomerServer(t)
	srv.failFirst["dead"] = 1000

	c := newClientFor(t, srv.URL)
	c.SetRateLimit(0, 1)
	out, err := c.RefreshCustomers(context.Background(), []string{"a", "dead", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.FailedIDs) != 1 || out.FailedIDs[0] != "dead" {
		t.Fatalf("FailedIDs = %v, want [dead]", out.FailedIDs)
	}
	if len(out.Customers) != 2 {
		t.Fatalf("Customers = %d, want 2", len(out.Customers))
	}
}

func TestRefreshCustomers_AbortsEarlyWhenUpstreamIsDown(t *testing.T) {
	withFastBackoff(t)
	prev := outageAbortAfter
	outageAbortAfter = 3
	t.Cleanup(func() { outageAbortAfter = prev })

	srv := newCustomerServer(t)
	srv.always503 = true

	ids := make([]string, 20)
	for i := range ids {
		ids[i] = fmt.Sprintf("c%d", i)
	}
	c := newClientFor(t, srv.URL)
	c.SetRateLimit(0, 1)
	out, err := c.RefreshCustomers(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.FailedIDs) != len(ids) {
		t.Fatalf("FailedIDs = %d, want all %d", len(out.FailedIDs), len(ids))
	}
	// 3 IDs × maxAttempts, then abort; no second pass on a total outage.
	if n := srv.count(); n > outageAbortAfter*maxAttempts {
		t.Fatalf("requests = %d, want <= %d (abort should stop the walk)", n, outageAbortAfter*maxAttempts)
	}
}

func TestRefreshCustomers_SingleIDHasNoSecondPass(t *testing.T) {
	withFastBackoff(t)
	srv := newCustomerServer(t)
	srv.always503 = true

	c := newClientFor(t, srv.URL)
	c.SetRateLimit(0, 1)
	out, err := c.RefreshCustomers(context.Background(), []string{"only"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.FailedIDs) != 1 {
		t.Fatalf("FailedIDs = %v, want [only]", out.FailedIDs)
	}
	if n := srv.count(); n != maxAttempts {
		t.Fatalf("requests = %d, want %d (the recheck path must not pay for a second pass)", n, maxAttempts)
	}
}
