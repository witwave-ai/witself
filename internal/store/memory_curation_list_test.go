package store

import (
	"errors"
	"testing"
	"time"
)

func TestMemoryCurationDueCursorCarriesQueuePosition(t *testing.T) {
	asOf := time.Date(2026, 7, 14, 18, 0, 0, 0, time.UTC)
	dueAt := asOf.Add(-time.Hour)
	createdAt := asOf.Add(-30 * time.Minute)
	want := memoryCurationRequestCursor{
		Version: 1, Restricted: true, ExcludeSensitive: true, AsOf: asOf,
		Priority: 20, DueAt: dueAt, CreatedAt: createdAt,
		RequestID: "mcrq_abcdefghijklmnop",
	}
	raw, err := encodeMemoryCurationRequestCursor(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeMemoryCurationRequestCursor(raw, "", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Priority != want.Priority || !got.DueAt.Equal(want.DueAt) ||
		!got.CreatedAt.Equal(want.CreatedAt) || got.RequestID != want.RequestID {
		t.Fatalf("cursor = %#v, want %#v", got, want)
	}
	if _, err := decodeMemoryCurationRequestCursor(raw, "", false, true); !errors.Is(err, ErrMemoryCurationInputInvalid) {
		t.Fatalf("cross-profile cursor error = %v", err)
	}
	if _, err := decodeMemoryCurationRequestCursor(raw, "", true, false); !errors.Is(err, ErrMemoryCurationInputInvalid) {
		t.Fatalf("cross-filter cursor error = %v", err)
	}
	want.DueAt = asOf.Add(time.Second)
	if _, err := encodeMemoryCurationRequestCursor(want); !errors.Is(err, ErrMemoryCurationInputInvalid) {
		t.Fatalf("future due cursor error = %v", err)
	}
}
