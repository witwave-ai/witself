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

type Catalog struct {
	SchemaVersion string            `json:"schema_version"`
	Regions       map[string]Region `json:"regions"`
}

type Region struct {
	Name      string              `json:"name"`
	Geography string              `json:"geography"`
	Providers map[string]Provider `json:"providers"`
}

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

func ValidCode(code string) bool {
	catalog, err := Load()
	if err != nil {
		return false
	}
	_, ok := catalog.Regions[code]
	return ok
}
