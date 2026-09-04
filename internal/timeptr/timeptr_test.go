package timeptr

import (
	"testing"
	"time"
)

func TestOfPreservesValueAndLocation(t *testing.T) {
	location := time.FixedZone("test-offset", -7*60*60)
	value := time.Date(2026, 9, 4, 8, 30, 0, 123, location)

	got := Of(value)
	if got == nil || *got != value || got.Location() != location {
		t.Fatalf("Of(%v) = %#v, want unchanged non-nil copy", value, got)
	}
}

func TestNonZeroDistinguishesZeroAndPreservesLocation(t *testing.T) {
	if got := NonZero(time.Time{}); got != nil {
		t.Fatalf("NonZero(zero) = %#v, want nil", got)
	}

	location := time.FixedZone("test-offset", 5*60*60)
	value := time.Date(2026, 9, 4, 8, 30, 0, 0, location)
	got := NonZero(value)
	if got == nil || *got != value || got.Location() != location {
		t.Fatalf("NonZero(%v) = %#v, want unchanged non-nil copy", value, got)
	}
}

func TestUTCNormalizesLocationAndKeepsZeroNonNil(t *testing.T) {
	zero := UTC(time.Time{})
	if zero == nil || !zero.IsZero() || zero.Location() != time.UTC {
		t.Fatalf("UTC(zero) = %#v, want non-nil zero value in UTC", zero)
	}

	location := time.FixedZone("test-offset", 5*60*60)
	value := time.Date(2026, 9, 4, 8, 30, 0, 0, location)
	got := UTC(value)
	if got == nil || !got.Equal(value) || got.Location() != time.UTC {
		t.Fatalf("UTC(%v) = %#v, want same instant in UTC", value, got)
	}
}
