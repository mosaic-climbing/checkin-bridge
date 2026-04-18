// Package cardmap resolves NFC tag UIDs to Redpoint customer IDs.
//
// In the cache-first architecture, this is a simple pass-through:
// the NFC tag UID is the lookup key into the local membership cache.
// The cache is populated by the sync job which reads NFC UIDs from
// either Redpoint customer notes or UniFi Access user records.
package cardmap

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// Mapper resolves NFC tag UIDs to Redpoint customer IDs.
//
// The primary lookup path goes through the membership cache (by externalId).
// This mapper provides an additional override layer for cases where the
// NFC tag UID doesn't match what's stored in Redpoint (e.g., replacement
// tags, temporary tags, etc.).
type Mapper struct {
	mu       sync.RWMutex
	overrides map[string]string // NFC tag UID → Redpoint customer ID
	mapFile  string
	logger   *slog.Logger
}

func New(dataDir string, logger *slog.Logger) (*Mapper, error) {
	m := &Mapper{
		overrides: make(map[string]string),
		mapFile:   filepath.Join(dataDir, "card_overrides.json"),
		logger:    logger,
	}

	if err := m.load(); err != nil {
		return nil, err
	}

	logger.Info("card mapper initialized", "overrides", len(m.overrides))
	return m, nil
}

// Resolve returns the Redpoint customer ID for an NFC tag UID.
// If there's a manual override, that takes priority.
// Otherwise returns the tag UID itself (used as the cache lookup key).
func (m *Mapper) Resolve(tagUID string) string {
	m.mu.RLock()
	if override, ok := m.overrides[tagUID]; ok {
		m.mu.RUnlock()
		return override
	}
	m.mu.RUnlock()
	return tagUID
}

// HasOverride returns true if the tag UID has a manual override.
func (m *Mapper) HasOverride(tagUID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.overrides[tagUID]
	return ok
}

// SetOverride maps an NFC tag UID to a specific customer ID.
func (m *Mapper) SetOverride(tagUID, customerID string) error {
	m.mu.Lock()
	m.overrides[tagUID] = customerID
	m.mu.Unlock()
	m.logger.Info("card override set", "tagUid", tagUID, "customerId", customerID)
	return m.save()
}

// DeleteOverride removes a manual override.
func (m *Mapper) DeleteOverride(tagUID string) error {
	m.mu.Lock()
	delete(m.overrides, tagUID)
	m.mu.Unlock()
	m.logger.Info("card override removed", "tagUid", tagUID)
	return m.save()
}

// AllOverrides returns a copy of all manual overrides.
func (m *Mapper) AllOverrides() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]string, len(m.overrides))
	for k, v := range m.overrides {
		out[k] = v
	}
	return out
}

func (m *Mapper) load() error {
	data, err := os.ReadFile(m.mapFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read card overrides: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := json.Unmarshal(data, &m.overrides); err != nil {
		return fmt.Errorf("parse card overrides: %w", err)
	}
	m.logger.Info("card overrides loaded", "entries", len(m.overrides))
	return nil
}

func (m *Mapper) save() error {
	m.mu.RLock()
	data, err := json.MarshalIndent(m.overrides, "", "  ")
	m.mu.RUnlock()
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.mapFile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	// Atomic write: temp file + rename to prevent corruption on power loss
	tmp, err := os.CreateTemp(dir, ".tmp-overrides-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		// Clean up on error
		if _, statErr := os.Stat(tmpName); statErr == nil {
			os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	return os.Rename(tmpName, m.mapFile)
}
