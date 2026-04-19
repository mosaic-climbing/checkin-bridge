package mirror

import (
	"context"

	"github.com/mosaic-climbing/checkin-bridge/internal/store"
)

// StoreAdapter wraps *store.Store so it satisfies the mirror.Store
// interface without polluting the base store with mirror's private
// type shapes. Every method is a thin field-by-field copy — a design
// choice, not a cost centre. The alternative (making mirror.Store a
// type alias for *store.Store) would couple the walker to every
// future change in store's API.
type StoreAdapter struct {
	Inner *store.Store
}

// NewStoreAdapter wraps a *store.Store for mirror.Walker.
func NewStoreAdapter(s *store.Store) *StoreAdapter {
	return &StoreAdapter{Inner: s}
}

func (a *StoreAdapter) GetSyncState(ctx context.Context) (*SyncState, error) {
	s, err := a.Inner.GetSyncState(ctx)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, nil
	}
	return &SyncState{
		Status:       s.Status,
		TotalFetched: s.TotalFetched,
		LastCursor:   s.LastCursor,
		LastError:    s.LastError,
		StartedAt:    s.StartedAt,
		CompletedAt:  s.CompletedAt,
	}, nil
}

func (a *StoreAdapter) StartSync(ctx context.Context) error {
	return a.Inner.StartSync(ctx)
}

func (a *StoreAdapter) UpdateSyncState(ctx context.Context, state *SyncState) error {
	return a.Inner.UpdateSyncState(ctx, &store.SyncState{
		Status:       state.Status,
		TotalFetched: state.TotalFetched,
		LastCursor:   state.LastCursor,
		LastError:    state.LastError,
		StartedAt:    state.StartedAt,
		CompletedAt:  state.CompletedAt,
	})
}

func (a *StoreAdapter) UpsertCustomerWithBadgeBatch(ctx context.Context, customers []Customer) error {
	batch := make([]store.Customer, len(customers))
	for i, c := range customers {
		batch[i] = store.Customer{
			RedpointID:            c.RedpointID,
			FirstName:             c.FirstName,
			LastName:              c.LastName,
			Email:                 c.Email,
			Barcode:               c.Barcode,
			ExternalID:            c.ExternalID,
			Active:                c.Active,
			UpdatedAt:             c.UpdatedAt,
			BadgeStatus:           c.BadgeStatus,
			BadgeName:             c.BadgeName,
			PastDueBalance:        c.PastDueBalance,
			HomeFacilityShortName: c.HomeFacilityShortName,
			LastSyncedAt:          c.LastSyncedAt,
		}
	}
	return a.Inner.UpsertCustomerWithBadgeBatch(ctx, batch)
}

func (a *StoreAdapter) MarkSyncComplete(ctx context.Context, status, lastError string) error {
	return a.Inner.MarkSyncComplete(ctx, status, lastError)
}
