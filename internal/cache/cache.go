// Package cache maintains a local membership cache for offline door validation.
//
// The cache serves two purposes:
//   1. On every successful check-in, the member is written to the cache
//      so that if Redpoint goes down, the bridge can still validate them locally.
//   2. A daily sync job queries Redpoint for all active members, refreshes
//      the cache, and prunes anyone whose badge is no longer ACTIVE.
//
// Cache is persisted to a JSON file under data/member_cache.json so it
// survives bridge restarts.
package cache

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CachedMember is the locally stored membership record.
type CachedMember struct {
	CustomerID  string `json:"customerId"`
	ExternalID  string `json:"externalId"` // NFC card UID
	Barcode     string `json:"barcode"`
	FirstName   string `json:"firstName"`
	LastName    string `json:"lastName"`
	BadgeStatus string `json:"badgeStatus"` // ACTIVE, FROZEN, EXPIRED
	BadgeName   string `json:"badgeName"`   // membership plan name
	Active      bool   `json:"active"`      // customer account active flag
	CachedAt    string `json:"cachedAt"`    // when this record was last refreshed
	LastCheckIn string `json:"lastCheckIn"` // last time they checked in via the bridge
}

func (m *CachedMember) FullName() string {
	return m.FirstName + " " + m.LastName
}

// IsAllowed returns true if the cached record indicates the member should be let in.
func (m *CachedMember) IsAllowed() bool {
	return m.Active && m.BadgeStatus == "ACTIVE"
}

// DenyReason returns a human-readable reason why the member was denied.
func (m *CachedMember) DenyReason() string {
	if !m.Active {
		return "customer account inactive"
	}
	switch m.BadgeStatus {
	case "FROZEN":
		return "membership frozen"
	case "EXPIRED":
		return "membership expired"
	case "":
		return "no membership badge"
	default:
		return "badge status: " + m.BadgeStatus
	}
}

// MemberCache provides thread-safe, persistent local membership storage.
type MemberCache struct {
	mu        sync.RWMutex
	members   map[string]*CachedMember // keyed by externalId (NFC card UID)
	byID      map[string]string        // customerId → externalId (reverse index)
	byBarcode map[string]string        // barcode → externalId (reverse index)
	filePath  string
	logger    *slog.Logger
}

func New(dataDir string, logger *slog.Logger) (*MemberCache, error) {
	c := &MemberCache{
		members:   make(map[string]*CachedMember),
		byID:      make(map[string]string),
		byBarcode: make(map[string]string),
		filePath:  filepath.Join(dataDir, "member_cache.json"),
		logger:    logger,
	}

	if err := c.load(); err != nil {
		return nil, err
	}

	logger.Info("member cache initialized", "members", len(c.members), "file", c.filePath)
	return c, nil
}

// ─── Lookups ─────────────────────────────────────────────────

// GetByExternalID looks up a member by their NFC card UID.
func (c *MemberCache) GetByExternalID(externalID string) *CachedMember {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.members[externalID]
}

// GetByCustomerID looks up a member by their Redpoint customer ID.
func (c *MemberCache) GetByCustomerID(customerID string) *CachedMember {
	c.mu.RLock()
	extID, ok := c.byID[customerID]
	if !ok {
		c.mu.RUnlock()
		return nil
	}
	m := c.members[extID]
	c.mu.RUnlock()
	return m
}

// GetByBarcode looks up a member by their Redpoint barcode.
func (c *MemberCache) GetByBarcode(barcode string) *CachedMember {
	c.mu.RLock()
	extID, ok := c.byBarcode[barcode]
	if !ok {
		c.mu.RUnlock()
		return nil
	}
	m := c.members[extID]
	c.mu.RUnlock()
	return m
}

// ─── Writes ──────────────────────────────────────────────────

// Put adds or updates a member in the cache.
func (c *MemberCache) Put(member *CachedMember) {
	c.mu.Lock()
	c.members[member.ExternalID] = member
	c.byID[member.CustomerID] = member.ExternalID
	if member.Barcode != "" {
		c.byBarcode[member.Barcode] = member.ExternalID
	}
	c.mu.Unlock()

	c.logger.Debug("cache updated", "externalId", member.ExternalID, "name", member.FullName())
}

// RecordCheckIn updates the LastCheckIn timestamp for a member.
func (c *MemberCache) RecordCheckIn(externalID string) {
	c.mu.Lock()
	if m, ok := c.members[externalID]; ok {
		m.LastCheckIn = time.Now().UTC().Format(time.RFC3339)
	}
	c.mu.Unlock()
}

// BulkReplace replaces the entire cache with a new set of members.
// Used by the daily sync to refresh from Redpoint.
func (c *MemberCache) BulkReplace(members []*CachedMember) {
	newMap := make(map[string]*CachedMember, len(members))
	newByID := make(map[string]string, len(members))
	newByBarcode := make(map[string]string, len(members))

	for _, m := range members {
		if m.ExternalID != "" {
			newMap[m.ExternalID] = m
			newByID[m.CustomerID] = m.ExternalID
			if m.Barcode != "" {
				newByBarcode[m.Barcode] = m.ExternalID
			}
		}
	}

	c.mu.Lock()
	oldCount := len(c.members)
	c.members = newMap
	c.byID = newByID
	c.byBarcode = newByBarcode
	c.mu.Unlock()

	c.logger.Info("cache bulk replaced",
		"previousCount", oldCount,
		"newCount", len(newMap),
	)
}

// Remove deletes a member from the cache by externalId.
func (c *MemberCache) Remove(externalID string) {
	c.mu.Lock()
	if m, ok := c.members[externalID]; ok {
		delete(c.byID, m.CustomerID)
		if m.Barcode != "" {
			delete(c.byBarcode, m.Barcode)
		}
		delete(c.members, externalID)
	}
	c.mu.Unlock()
}

// PruneInactive removes all members whose badge is not ACTIVE or whose
// account is inactive. Called by the daily sync job.
func (c *MemberCache) PruneInactive() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	pruned := 0
	for extID, m := range c.members {
		if !m.IsAllowed() {
			delete(c.byID, m.CustomerID)
			if m.Barcode != "" {
				delete(c.byBarcode, m.Barcode)
			}
			delete(c.members, extID)
			c.logger.Info("pruned from cache",
				"externalId", extID,
				"name", m.FullName(),
				"badgeStatus", m.BadgeStatus,
				"active", m.Active,
			)
			pruned++
		}
	}

	return pruned
}

// ─── Stats ───────────────────────────────────────────────────

type CacheStats struct {
	TotalMembers  int    `json:"totalMembers"`
	ActiveMembers int    `json:"activeMembers"`
	FilePath      string `json:"filePath"`
}

func (c *MemberCache) Stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	active := 0
	for _, m := range c.members {
		if m.IsAllowed() {
			active++
		}
	}

	return CacheStats{
		TotalMembers:  len(c.members),
		ActiveMembers: active,
		FilePath:      c.filePath,
	}
}

// AllCustomerIDs returns a deduplicated list of Redpoint customer IDs in the cache.
// Used by the targeted sync to refresh only known members.
func (c *MemberCache) AllCustomerIDs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	seen := make(map[string]bool, len(c.byID))
	ids := make([]string, 0, len(c.byID))
	for custID := range c.byID {
		if custID != "" && !seen[custID] {
			seen[custID] = true
			ids = append(ids, custID)
		}
	}
	return ids
}

// All returns a copy of all cached members. Used by the admin API.
func (c *MemberCache) All() []*CachedMember {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]*CachedMember, 0, len(c.members))
	for _, m := range c.members {
		cp := *m
		out = append(out, &cp)
	}
	return out
}

// ─── Persistence ─────────────────────────────────────────────

// Save writes the cache to disk atomically. Uses write-to-temp + rename
// to avoid corruption if the process is killed mid-write (e.g. power loss).
func (c *MemberCache) Save() error {
	c.mu.RLock()
	members := make([]*CachedMember, 0, len(c.members))
	for _, m := range c.members {
		members = append(members, m)
	}
	c.mu.RUnlock()

	data, err := json.MarshalIndent(members, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cache: %w", err)
	}

	dir := filepath.Dir(c.filePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}

	if err := atomicWrite(c.filePath, data, 0o600); err != nil {
		return fmt.Errorf("write cache: %w", err)
	}

	c.logger.Debug("cache saved to disk", "members", len(members))
	return nil
}

// atomicWrite writes data to a temp file then renames it to path.
// This prevents corruption from partial writes on power loss.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	// Clean up on error
	success := false
	defer func() {
		if !success {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	// Sync to ensure data hits disk before rename
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}

	success = true
	return nil
}

func (c *MemberCache) load() error {
	data, err := os.ReadFile(c.filePath)
	if os.IsNotExist(err) {
		c.logger.Info("no existing cache file, starting empty")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read cache: %w", err)
	}

	var members []*CachedMember
	if err := json.Unmarshal(data, &members); err != nil {
		// Try loading backup if main file is corrupted
		backupPath := c.filePath + ".bak"
		c.logger.Warn("cache file corrupted, trying backup", "error", err, "backup", backupPath)
		backupData, backupErr := os.ReadFile(backupPath)
		if backupErr != nil {
			return fmt.Errorf("parse cache: %w (no backup available)", err)
		}
		if err2 := json.Unmarshal(backupData, &members); err2 != nil {
			return fmt.Errorf("parse cache: %w (backup also corrupted: %v)", err, err2)
		}
		c.logger.Warn("loaded from backup cache file", "members", len(members))
	}

	for _, m := range members {
		if m.ExternalID != "" {
			c.members[m.ExternalID] = m
			c.byID[m.CustomerID] = m.ExternalID
			if m.Barcode != "" {
				c.byBarcode[m.Barcode] = m.ExternalID
			}
		}
	}

	c.logger.Info("cache loaded from disk", "members", len(c.members))
	return nil
}

// SaveBackup creates a backup of the current cache file.
// Called before bulk operations that replace the entire cache.
func (c *MemberCache) SaveBackup() error {
	data, err := os.ReadFile(c.filePath)
	if os.IsNotExist(err) {
		return nil // nothing to back up
	}
	if err != nil {
		return err
	}
	backupPath := c.filePath + ".bak"
	return os.WriteFile(backupPath, data, 0o600)
}
