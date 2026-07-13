package store

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeFactListOptions(t *testing.T) {
	opts, err := normalizeFactListOptions(FactListOptions{
		Subject: " My   Spouse ", PredicatePrefix: "preferences/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Subject != "my spouse" || opts.PredicatePrefix != "preferences/" || opts.Limit != 100 {
		t.Fatalf("normalized options = %#v", opts)
	}
	if opts.RetrievalMode != FactRetrievalModeSearch {
		t.Fatalf("default retrieval mode = %q", opts.RetrievalMode)
	}
}

func TestNormalizeFactListOptionsRejectsInvalidInput(t *testing.T) {
	for _, opts := range []FactListOptions{
		{Limit: -1},
		{Limit: 501},
		{Subject: "bad\x00subject"},
		{PredicatePrefix: "Bad Predicate"},
		{RetrievalMode: "background_magic"},
	} {
		if _, err := normalizeFactListOptions(opts); !errors.Is(err, ErrFactInputInvalid) {
			t.Errorf("normalizeFactListOptions(%#v) error = %v, want ErrFactInputInvalid", opts, err)
		}
	}
}

func TestFactRetrievalModeRankingEligibility(t *testing.T) {
	for _, mode := range []FactRetrievalMode{
		FactRetrievalModeExact,
		FactRetrievalModeSearch,
		FactRetrievalModeTemporal,
	} {
		if !validFactRetrievalMode(mode) || !factRetrievalRanks(mode) {
			t.Errorf("mode %q should be valid and ranking eligible", mode)
		}
	}
	if !validFactRetrievalMode(FactRetrievalModeSelfHydration) || factRetrievalRanks(FactRetrievalModeSelfHydration) {
		t.Fatal("self hydration should be valid but excluded from ranking")
	}
	if validFactRetrievalMode("unknown") || factRetrievalRanks("unknown") {
		t.Fatal("unknown retrieval mode was accepted")
	}
}

func TestFactUsageProjectionFiltersRetrievalModes(t *testing.T) {
	for _, want := range []string{
		"COALESCE(metadata->>'retrieval_mode', 'exact')",
		"('exact', 'search', 'temporal')",
	} {
		if !strings.Contains(factUsageSelect, want) {
			t.Errorf("usage projection does not contain %q", want)
		}
	}
	if strings.Contains(factUsageSelect, "self_hydration") {
		t.Fatal("self hydration leaked into the ranking projection")
	}
}

func TestFactListOrderClause(t *testing.T) {
	standard := factListOrderClause(false)
	if standard != " ORDER BY f.predicate, s.canonical_key, f.id" {
		t.Fatalf("standard order = %q", standard)
	}
	ranked := factListOrderClause(true)
	for _, want := range []string{
		"usage_count DESC",
		"u.last_used_at DESC NULLS LAST",
		"f.predicate, s.canonical_key, f.id",
	} {
		if !strings.Contains(ranked, want) {
			t.Errorf("ranked order %q does not contain %q", ranked, want)
		}
	}
}
