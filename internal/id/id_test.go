package id

import (
	"encoding/base32"
	"strings"
	"testing"
)

func TestNewPrefixed(t *testing.T) {
	got, err := New("acc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "acc_") {
		t.Errorf("%q missing acc_ prefix", got)
	}
	body := strings.TrimPrefix(got, "acc_")
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(body))
	if err != nil {
		t.Fatalf("body not base32: %v", err)
	}
	if len(raw) != entropyBytes {
		t.Errorf("entropy = %d bytes, want %d", len(raw), entropyBytes)
	}
}

func TestNewUnique(t *testing.T) {
	seen := make(map[string]bool, 100)
	for range 100 {
		got, err := New("acc")
		if err != nil {
			t.Fatal(err)
		}
		if seen[got] {
			t.Fatalf("duplicate id: %s", got)
		}
		seen[got] = true
	}
}
