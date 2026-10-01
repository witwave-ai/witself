package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

// TestMapMemoryCurationErrorCarriesConflictReason pins that the server adapter
// keeps a store conflict's reason and plan positions, and that an
// unclassified conflict still maps to the bare server conflict.
func TestMapMemoryCurationErrorCarriesConflictReason(t *testing.T) {
	index := 1
	mapped := mapMemoryCurationError(fmt.Errorf("plan: %w", &store.MemoryCurationConflictError{
		Reason: "transcript_range_not_covered", ActionOrdinal: 2, EvidenceIndex: &index,
	}))
	var conflict *server.MemoryCurationConflictError
	if !errors.As(mapped, &conflict) || !errors.Is(mapped, server.ErrConflict) {
		t.Fatalf("mapped conflict = %v, want a reasoned server conflict", mapped)
	}
	if conflict.Reason != "transcript_range_not_covered" || conflict.ActionOrdinal != 2 ||
		conflict.EvidenceIndex == nil || *conflict.EvidenceIndex != 1 {
		t.Fatalf("mapped conflict = reason %q ordinal %d evidence set %t",
			conflict.Reason, conflict.ActionOrdinal, conflict.EvidenceIndex != nil)
	}
	bare := mapMemoryCurationError(store.ErrMemoryCurationConflict)
	if !errors.Is(bare, server.ErrConflict) || errors.As(bare, new(*server.MemoryCurationConflictError)) {
		t.Fatalf("unclassified conflict = %v, want the bare server conflict", bare)
	}
}
