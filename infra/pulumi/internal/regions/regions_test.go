package regions

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedCatalogIsSyncedWithRootCatalog(t *testing.T) {
	rootPath := filepath.Join("..", "..", "..", "..", "regions", "catalog.json")
	root, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatalf("read root catalog: %v", err)
	}
	if !bytes.Equal(bytes.TrimSpace(root), bytes.TrimSpace(catalogJSON)) {
		t.Fatalf("embedded infra catalog is out of sync with %s", rootPath)
	}
}

func TestLookupProviderRegion(t *testing.T) {
	tests := []struct {
		cloud          string
		providerRegion string
		wantCode       string
		wantOK         bool
	}{
		{cloud: "aws", providerRegion: "us-west-2", wantCode: "usw2", wantOK: true},
		{cloud: "gcp", providerRegion: "us-west2", wantCode: "usw2", wantOK: true},
		{cloud: "azure", providerRegion: "westus2", wantCode: "usw2", wantOK: true},
		{cloud: "azure", providerRegion: "eastus2", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.cloud+"/"+tt.providerRegion, func(t *testing.T) {
			got, _, _, ok := LookupProviderRegion(tt.cloud, tt.providerRegion)
			if ok != tt.wantOK || got != tt.wantCode {
				t.Fatalf("LookupProviderRegion() = %q, %v; want %q, %v", got, ok, tt.wantCode, tt.wantOK)
			}
		})
	}
}
