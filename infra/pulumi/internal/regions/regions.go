// Package regions exposes Witself's canonical placement region catalog to
// witself-infra. The root repo's regions/catalog.json is the source of truth;
// this package embeds a synced copy so released binaries do not need a checkout.
package regions

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed catalog.json
var catalogJSON []byte

// Catalog is the top-level region catalog document.
type Catalog struct {
	SchemaVersion string            `json:"schema_version"`
	Description   string            `json:"description"`
	Regions       map[string]Region `json:"regions"`
}

// Region is one canonical Witself placement region.
type Region struct {
	Name      string              `json:"name"`
	Geography string              `json:"geography"`
	Providers map[string]Provider `json:"providers"`
}

// Provider is one cloud provider's native region mapping.
type Provider struct {
	Region  string `json:"region"`
	Name    string `json:"name"`
	Match   string `json:"match"`
	AZCount int    `json:"az_count,omitempty"`
	OptIn   string `json:"opt_in,omitempty"`
}

var catalog = mustLoad(catalogJSON)

// Default returns the embedded placement-region catalog.
func Default() Catalog {
	return catalog
}

// LookupProviderRegion maps a provider-native region, such as aws/us-west-2,
// to a canonical Witself region code, such as usw2.
func LookupProviderRegion(cloud, providerRegion string) (code string, region Region, provider Provider, ok bool) {
	for c, r := range catalog.Regions {
		p, found := r.Providers[cloud]
		if found && p.Region == providerRegion {
			return c, r, p, true
		}
	}
	return "", Region{}, Provider{}, false
}

func mustLoad(raw []byte) Catalog {
	var c Catalog
	if err := json.Unmarshal(raw, &c); err != nil {
		panic(fmt.Sprintf("load region catalog: %v", err))
	}
	if c.SchemaVersion == "" {
		panic("load region catalog: missing schema_version")
	}
	for code, region := range c.Regions {
		if len(region.Providers) == 0 {
			panic(fmt.Sprintf("load region catalog: %s has no provider mappings", code))
		}
	}
	return c
}
