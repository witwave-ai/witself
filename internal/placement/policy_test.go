package placement

import (
	"errors"
	"testing"
)

func TestDefaultPolicy(t *testing.T) {
	p, err := Normalize(DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if got := p.PreferredRegions; len(got) != 2 || got[0] != "usw2" || got[1] != "use1" {
		t.Fatalf("preferred_regions = %#v", got)
	}
	if got := p.PreferredChannels; len(got) != 3 || got[0] != "stable" || got[1] != "edge" || got[2] != "experimental" {
		t.Fatalf("preferred_channels = %#v", got)
	}
	if got := p.RebalanceOn; len(got) != 2 || got[0] != "cloud" || got[1] != "channel" {
		t.Fatalf("rebalance_on = %#v", got)
	}
}

func TestNormalizePolicy(t *testing.T) {
	p, err := Normalize(Policy{
		PreferredClouds:  []string{" GCP ", "aws", "gcp"},
		PreferredRegions: []string{"USW2", "use1"},
		AllowedClouds:    []string{"gcp", "aws"},
		RebalanceOn:      []string{"channel", "cloud", "channel"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.PreferredClouds; len(got) != 2 || got[0] != "gcp" || got[1] != "aws" {
		t.Fatalf("preferred_clouds = %#v", got)
	}
	if got := p.RebalanceOn; len(got) != 2 || got[0] != "channel" || got[1] != "cloud" {
		t.Fatalf("rebalance_on = %#v", got)
	}
}

func TestNormalizeRejectsUnknownRegion(t *testing.T) {
	_, err := Normalize(Policy{PreferredRegions: []string{"use9"}})
	if !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("err = %v, want ErrInvalidPolicy", err)
	}
}
