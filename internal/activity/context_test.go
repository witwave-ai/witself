package activity

import (
	"context"
	"strings"
	"testing"
)

func TestActivityContext(t *testing.T) {
	base := context.Background()
	key := NewRequestID()
	passive := WithObservation(WithRequestID(base, key))
	if !IsObservation(passive) || RequestID(passive) != key || IsObservation(WithDeliberate(passive)) || RequestID(WithDeliberate(passive)) != key || IsObservation(base) {
		t.Fatal("intent or request scope lost")
	}
	if key == NewRequestID() {
		t.Fatal("random keys repeated")
	}
	for _, s := range []string{"", strings.Repeat("a", 15), strings.Repeat("a", 97), "0123456789abcdef\n", "0123456789abc/def", "0123456789abcédef"} {
		if ValidRequestID(s) {
			t.Fatalf("accepted invalid key of length %d", len(s))
		}
	}
	for _, s := range []string{key, strings.Repeat("a", 16), strings.Repeat("_-A9", 24)} {
		if !ValidRequestID(s) {
			t.Fatal("rejected valid key")
		}
	}
	ctx := DefaultOperation(base, "facts.set")
	ctx = DefaultOperation(ctx, "facts.propose")
	if op, _ := Operation(ctx); op != "facts.set" {
		t.Fatal("nested operation replaced outer")
	}
	if op, _ := Operation(DefaultOperation(WithOperation(base, ""), "facts.set")); op != "" {
		t.Fatal("excluded scope lost")
	}
}
func TestActivityCatalog(t *testing.T) {
	reads, writes := 0, 0
	for op, d := range Catalog() {
		if op != d.Category+"."+d.Action {
			t.Fatal("invalid descriptor")
		}
		if d.Write {
			writes++
		} else {
			reads++
		}
	}
	if reads != 20 || writes != 23 {
		t.Fatalf("catalog entries: read=%d write=%d", reads, writes)
	}
	for _, op := range ExcludedCatalog() {
		if _, ok := Lookup(op); ok {
			t.Fatal("excluded operation allowed")
		}
	}
	snapshot := Catalog()
	delete(snapshot, "facts.set")
	if _, ok := Lookup("facts.set"); !ok {
		t.Fatal("catalog is mutable")
	}
	for _, d := range Dimensions() {
		if Unit(d) == "" || Unit(d) == "activation" {
			t.Fatal("invalid report dimension")
		}
	}
}
