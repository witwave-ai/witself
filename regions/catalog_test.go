package regions

import "testing"

func TestCatalogLoads(t *testing.T) {
	codes, err := Codes()
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 6 {
		t.Fatalf("codes len = %d, want 6", len(codes))
	}
	for _, code := range []string{"use1", "usw2", "euw2"} {
		if !ValidCode(code) {
			t.Fatalf("ValidCode(%q) = false", code)
		}
	}
}
