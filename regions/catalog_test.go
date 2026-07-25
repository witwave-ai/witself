package regions

import (
	"slices"
	"testing"
)

func TestCatalogLoads(t *testing.T) {
	codes, err := Codes()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"aps1", "euc1", "euw1", "euw2", "use1", "usw1", "usw2"}
	if !slices.Equal(codes, want) {
		t.Fatalf("codes = %#v, want %#v", codes, want)
	}
	for _, code := range want {
		if !ValidCode(code) {
			t.Fatalf("ValidCode(%q) = false", code)
		}
	}
}
