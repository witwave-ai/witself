// Package regions exposes Witself's canonical placement region catalog.
package regions

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

//go:embed catalog.json
var catalogFS embed.FS

// Catalog is the canonical placement-region mapping for all supported clouds.
type Catalog struct {
	SchemaVersion string            `json:"schema_version"`
	Regions       map[string]Region `json:"regions"`
}

// Region describes one canonical Witself region and its provider mappings.
type Region struct {
	Name      string              `json:"name"`
	Geography string              `json:"geography"`
	Providers map[string]Provider `json:"providers"`
}

// Provider maps one canonical region to a cloud provider's native region.
type Provider struct {
	Region string `json:"region"`
	Name   string `json:"name"`
	Match  string `json:"match"`
}

var (
	loadOnce sync.Once
	loaded   Catalog
	loadErr  error
)

// Load returns the embedded canonical region catalog.
func Load() (Catalog, error) {
	loadOnce.Do(func() {
		raw, err := catalogFS.ReadFile("catalog.json")
		if err != nil {
			loadErr = err
			return
		}
		if err := json.Unmarshal(raw, &loaded); err != nil {
			loadErr = fmt.Errorf("decode region catalog: %w", err)
			return
		}
		if loaded.SchemaVersion == "" || len(loaded.Regions) == 0 {
			loadErr = fmt.Errorf("region catalog is incomplete")
		}
	})
	return loaded, loadErr
}

// Codes returns every canonical region code in sorted order.
func Codes() ([]string, error) {
	catalog, err := Load()
	if err != nil {
		return nil, err
	}
	codes := make([]string, 0, len(catalog.Regions))
	for code := range catalog.Regions {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes, nil
}

// ValidCode reports whether code exists in the canonical region catalog.
func ValidCode(code string) bool {
	catalog, err := Load()
	if err != nil {
		return false
	}
	_, ok := catalog.Regions[code]
	return ok
}
