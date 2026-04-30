package testutil

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// FakeRedpoint simulates the Redpoint HQ GraphQL API.
type FakeRedpoint struct {
	Server    *httptest.Server
	mu        sync.Mutex
	Customers map[string]FakeCustomer
	CheckIns  []FakeCheckIn
}

type FakeCustomer struct {
	ID         string
	FirstName  string
	LastName   string
	Email      string
	ExternalID string
	Active     bool
	Badge      string // "ACTIVE", "FROZEN", "EXPIRED"
	BadgeName  string
}

type FakeCheckIn struct {
	GateID     string
	CustomerID string
}

func NewFakeRedpoint() *FakeRedpoint {
	f := &FakeRedpoint{
		Customers: make(map[string]FakeCustomer),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/graphql", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		w.Header().Set("Content-Type", "application/json")

		// `customer(id: $id)` — used by redpoint.Client.GetCustomer.
		// The fake keys customers by ExternalID; tests that exercise
		// this branch should set ID == ExternalID so the lookup works.
		// Order matters: this must come before customerByExternalId
		// because the externalId query also contains "customer".
		if strings.Contains(req.Query, "customer(id:") {
			id, _ := req.Variables["id"].(string)
			f.mu.Lock()
			var cust *FakeCustomer
			for _, c := range f.Customers {
				if c.ID == id {
					cc := c
					cust = &cc
					break
				}
			}
			f.mu.Unlock()
			if cust == nil {
				json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{"customer": nil},
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"customer": map[string]any{
						"id": cust.ID, "active": cust.Active,
						"firstName": cust.FirstName, "lastName": cust.LastName,
						"email": cust.Email, "externalId": cust.ExternalID,
						"badge": map[string]any{
							"status": cust.Badge,
							"customerBadge": map[string]any{
								"id": "badge-1", "name": cust.BadgeName,
							},
						},
					},
				},
			})
			return
		}

		if strings.Contains(req.Query, "customerByExternalId") {
			extID, _ := req.Variables["externalId"].(string)
			f.mu.Lock()
			cust, ok := f.Customers[extID]
			f.mu.Unlock()

			if !ok {
				json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{"customerByExternalId": nil},
				})
				return
			}

			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"customerByExternalId": map[string]any{
						"id": cust.ID, "active": cust.Active,
						"firstName": cust.FirstName, "lastName": cust.LastName,
						"email": cust.Email, "externalId": cust.ExternalID,
						"badge": map[string]any{
							"status": cust.Badge,
							"customerBadge": map[string]any{
								"id": "badge-1", "name": cust.BadgeName,
							},
						},
					},
				},
			})
			return
		}

		if strings.Contains(req.Query, "createCheckIn") {
			f.mu.Lock()
			f.CheckIns = append(f.CheckIns, FakeCheckIn{
				GateID:     "gate-1",
				CustomerID: "cust-1",
			})
			f.mu.Unlock()

			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"createCheckIn": map[string]any{
						"__typename": "CreateCheckInResult",
						"recordId":   "checkin-123",
						"record": map[string]any{
							"id":     "checkin-123",
							"status": "OK",
						},
					},
				},
			})
			return
		}

		// CustomersByEmail — returns the full set of customers whose Email
		// field matches the requested email (case-insensitive). Used by the
		// C2 email-based matching path; the list may legitimately have 2+
		// entries when a household shares an email.
		if strings.Contains(req.Query, "CustomersByEmail") {
			filter, _ := req.Variables["filter"].(map[string]any)
			wantEmail, _ := filter["email"].(string)
			wantEmail = strings.ToLower(strings.TrimSpace(wantEmail))

			f.mu.Lock()
			var edges []any
			for _, c := range f.Customers {
				if strings.ToLower(strings.TrimSpace(c.Email)) != wantEmail {
					continue
				}
				edges = append(edges, map[string]any{
					"node": map[string]any{
						"id": c.ID, "active": c.Active,
						"firstName": c.FirstName, "lastName": c.LastName,
						"email": c.Email, "externalId": c.ExternalID,
						"badge": map[string]any{
							"status": c.Badge,
							"customerBadge": map[string]any{
								"id": "badge-1", "name": c.BadgeName,
							},
						},
					},
				})
			}
			f.mu.Unlock()

			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"customers": map[string]any{"edges": edges},
				},
			})
			return
		}

		if strings.Contains(req.Query, "gates") {
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"gates": map[string]any{
						"edges": []any{
							map[string]any{
								"node": map[string]any{
									"id": "gate-1", "name": "Main Entrance", "active": true,
								},
							},
						},
					},
				},
			})
			return
		}

		// Default empty response
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	})

	f.Server = httptest.NewServer(mux)
	return f
}

func (f *FakeRedpoint) Close() { f.Server.Close() }
func (f *FakeRedpoint) GraphQLURL() string { return f.Server.URL + "/api/graphql" }
func (f *FakeRedpoint) CheckInCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.CheckIns)
}
func (f *FakeRedpoint) AddCustomer(c FakeCustomer) {
	f.mu.Lock()
	f.Customers[c.ExternalID] = c
	f.mu.Unlock()
}
