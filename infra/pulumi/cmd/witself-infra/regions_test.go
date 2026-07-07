package main

import "testing"

func TestResolveRegionCode(t *testing.T) {
	tests := []struct {
		name          string
		cloud         string
		region        string
		wantNameCode  string
		wantPlaceCode string
		wantOK        bool
	}{
		{
			name:          "catalog-backed region",
			cloud:         "aws",
			region:        "us-west-2",
			wantNameCode:  "usw2",
			wantPlaceCode: "usw2",
			wantOK:        true,
		},
		{
			name:          "legacy-only region remains provisionable",
			cloud:         "azure",
			region:        "eastus2",
			wantNameCode:  "use2",
			wantPlaceCode: "",
			wantOK:        true,
		},
		{
			name:   "unknown region",
			cloud:  "aws",
			region: "antarctica1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nameCode, placeCode, ok := resolveRegionCode(tt.cloud, tt.region)
			if nameCode != tt.wantNameCode || placeCode != tt.wantPlaceCode || ok != tt.wantOK {
				t.Fatalf("resolveRegionCode() = %q, %q, %v; want %q, %q, %v",
					nameCode, placeCode, ok, tt.wantNameCode, tt.wantPlaceCode, tt.wantOK)
			}
		})
	}
}
