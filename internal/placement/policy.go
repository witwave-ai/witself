package placement

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/witwave-ai/witself/regions"
)

type Policy struct {
	PreferredClouds   []string `json:"preferred_clouds"`
	PreferredRegions  []string `json:"preferred_regions"`
	PreferredChannels []string `json:"preferred_channels"`
	AllowedClouds     []string `json:"allowed_clouds"`
	AllowedRegions    []string `json:"allowed_regions"`
	AllowedChannels   []string `json:"allowed_channels"`
	RebalanceOn       []string `json:"rebalance_on"`
}

var ErrInvalidPolicy = errors.New("invalid placement policy")

var (
	legalClouds        = []string{"aws", "gcp", "azure"}
	legalChannels      = []string{"stable", "edge", "experimental"}
	legalRebalanceAxes = []string{"cloud", "region", "channel"}
)

func DefaultPolicy() Policy {
	return Policy{
		PreferredClouds:   []string{},
		PreferredRegions:  []string{"usw2", "use1"},
		PreferredChannels: []string{"stable", "edge", "experimental"},
		AllowedClouds:     []string{},
		AllowedRegions:    []string{},
		AllowedChannels:   []string{},
		RebalanceOn:       []string{"cloud", "channel"},
	}
}

func Normalize(p Policy) (Policy, error) {
	var err error
	if p.PreferredClouds, err = normalizeList("preferred_clouds", p.PreferredClouds, validCloud); err != nil {
		return Policy{}, err
	}
	if p.PreferredRegions, err = normalizeList("preferred_regions", p.PreferredRegions, regions.ValidCode); err != nil {
		return Policy{}, err
	}
	if p.PreferredChannels, err = normalizeList("preferred_channels", p.PreferredChannels, validChannel); err != nil {
		return Policy{}, err
	}
	if p.AllowedClouds, err = normalizeList("allowed_clouds", p.AllowedClouds, validCloud); err != nil {
		return Policy{}, err
	}
	if p.AllowedRegions, err = normalizeList("allowed_regions", p.AllowedRegions, regions.ValidCode); err != nil {
		return Policy{}, err
	}
	if p.AllowedChannels, err = normalizeList("allowed_channels", p.AllowedChannels, validChannel); err != nil {
		return Policy{}, err
	}
	if p.RebalanceOn, err = normalizeList("rebalance_on", p.RebalanceOn, validRebalanceAxis); err != nil {
		return Policy{}, err
	}
	return p, nil
}

func FromJSON(raw []byte) (Policy, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return DefaultPolicy(), nil
	}
	var p Policy
	if err := json.Unmarshal(raw, &p); err != nil {
		return Policy{}, fmt.Errorf("%w: %v", ErrInvalidPolicy, err)
	}
	return Normalize(p)
}

func FromAny(v any) (Policy, error) {
	if _, ok := v.(map[string]any); !ok {
		return Policy{}, fmt.Errorf("%w: placement_policy must be an object", ErrInvalidPolicy)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return Policy{}, fmt.Errorf("%w: %v", ErrInvalidPolicy, err)
	}
	return FromJSON(raw)
}

func MustJSON(p Policy) []byte {
	normalized, err := Normalize(p)
	if err != nil {
		panic(err)
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		panic(err)
	}
	return raw
}

func normalizeList(field string, values []string, valid func(string) bool) ([]string, error) {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, raw := range values {
		value := strings.ToLower(strings.TrimSpace(raw))
		if value == "" {
			continue
		}
		if !valid(value) {
			return nil, fmt.Errorf("%w: %s contains unknown value %q", ErrInvalidPolicy, field, raw)
		}
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

func validCloud(value string) bool {
	return slices.Contains(legalClouds, value)
}

func validChannel(value string) bool {
	return slices.Contains(legalChannels, value)
}

func validRebalanceAxis(value string) bool {
	return slices.Contains(legalRebalanceAxes, value)
}
