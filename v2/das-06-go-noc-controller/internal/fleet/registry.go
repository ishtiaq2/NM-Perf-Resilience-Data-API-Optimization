package fleet

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// InventorySite is a site the NOC must know about even if it never connects:
// provisioning data (normally from the CRM or a provisioning database).
type InventorySite struct {
	ID     string `json:"id"`
	Tenant string `json:"tenant"`
	Name   string `json:"name,omitempty"`
	Venue  string `json:"venue,omitempty"`
	Region string `json:"region,omitempty"`
}

// LoadInventory adds the listed sites (status "offline" until they connect).
func (st *Store) LoadInventory(sites []InventorySite) {
	for _, in := range sites {
		if in.ID == "" || in.Tenant == "" {
			continue
		}
		s := st.getOrCreate(in.ID, in.Tenant)
		st.mutate(s, func(s *Site, m *mutation) {
			s.inventory = true
			if s.stats.Name == "" {
				s.stats.Name, s.stats.Venue, s.stats.Region = in.Name, in.Venue, in.Region
				s.stats.MaxTempC, s.stats.MinRxDbm, s.stats.MaxVswr = nan(), nan(), nan()
			}
			m.fleet = true
		})
	}
}

func nan() Num { var z float64; return Num(z / z) }

// RegistryEntry is what survives a NOC restart for one site. Live state does not:
// every device sends its summary and active alarms again when it reconnects.
type RegistryEntry struct {
	ID            string               `json:"id"`
	Tenant        string               `json:"tenant"`
	Name          string               `json:"name,omitempty"`
	Venue         string               `json:"venue,omitempty"`
	Region        string               `json:"region,omitempty"`
	EverConnected bool                 `json:"everConnected"`
	LastSeen      int64                `json:"lastSeen,omitempty"`
	Acks          map[string]ackRecord `json:"acks,omitempty"`
}

// Registry returns the durable part of the fleet, sorted by site id.
func (st *Store) Registry() []RegistryEntry {
	var out []RegistryEntry
	for _, s := range st.all() {
		s.mu.Lock()
		e := RegistryEntry{ID: s.ID, Tenant: s.tenant, Name: s.stats.Name, Venue: s.stats.Venue, Region: s.stats.Region, EverConnected: s.everConnected}
		if !s.lastSeen.IsZero() {
			e.LastSeen = s.lastSeen.UnixMilli()
		}
		if len(s.acks) > 0 {
			e.Acks = make(map[string]ackRecord, len(s.acks))
			for k, v := range s.acks {
				e.Acks[k] = v
			}
		}
		s.mu.Unlock()
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b RegistryEntry) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return out
}

// RestoreRegistry loads the registry at start-up. Sites that were connected before
// are "stale" for the grace period: they are expected to reconnect within seconds.
func (st *Store) RestoreRegistry(entries []RegistryEntry) {
	now := st.cfg.Now()
	for _, e := range entries {
		if e.ID == "" || e.Tenant == "" {
			continue
		}
		s := st.getOrCreate(e.ID, e.Tenant)
		st.mutate(s, func(s *Site, m *mutation) {
			if s.connected {
				return
			}
			s.stats.Name, s.stats.Venue, s.stats.Region = e.Name, e.Venue, e.Region
			s.stats.MaxTempC, s.stats.MinRxDbm, s.stats.MaxVswr = nan(), nan(), nan()
			s.everConnected = e.EverConnected
			if e.EverConnected {
				s.disconnectedAt = now // the NOC was down, not necessarily the site
			}
			if e.LastSeen > 0 {
				s.lastSeen = time.UnixMilli(e.LastSeen)
			}
			for k, v := range e.Acks {
				s.acks[k] = v
			}
			m.fleet = true
		})
	}
}

// SaveRegistry writes the registry atomically (temp file + rename).
func (st *Store) SaveRegistry(path string) error {
	b, err := json.Marshal(st.Registry())
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync() // on disk before the rename makes it the registry
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// LoadRegistry reads a registry file written by SaveRegistry (missing file: empty).
func LoadRegistry(path string) ([]RegistryEntry, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []RegistryEntry
	return out, json.Unmarshal(b, &out)
}

// LoadInventoryFile reads a JSON array of InventorySite.
func LoadInventoryFile(path string) ([]InventorySite, error) {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	var out []InventorySite
	return out, json.Unmarshal(b, &out)
}
