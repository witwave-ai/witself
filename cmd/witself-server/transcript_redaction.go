package main

import (
	"context"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

func configureTranscriptRedaction(cfg *server.Config, st *store.Store) {
	cfg.RedactTranscriptEntries = func(ctx context.Context, p server.DomainPrincipal, in server.RedactTranscriptEntriesRequest) (server.TranscriptRedactionReceipt, error) {
		result, err := st.RedactTranscriptEntries(ctx, toStorePrincipal(p), toStoreRedactTranscriptEntriesInput(in))
		if err != nil {
			return server.TranscriptRedactionReceipt{}, mapTranscriptError(err)
		}
		return toServerTranscriptRedactionReceipt(result), nil
	}
}

func toStoreRedactTranscriptEntriesInput(in server.RedactTranscriptEntriesRequest) store.RedactTranscriptEntriesInput {
	return store.RedactTranscriptEntriesInput{
		TranscriptID: in.TranscriptID, FromSequence: in.FromSequence, UntilSequence: in.UntilSequence,
		ExpectedRedactionSetRevision: in.ExpectedRedactionSetRevision, IdempotencyKey: in.IdempotencyKey, Apply: in.Apply,
	}
}

func toServerTranscriptRedactionReceipt(result store.TranscriptRedactionResult) server.TranscriptRedactionReceipt {
	entries := make([]server.TranscriptRedactionEntry, len(result.Entries))
	for i, entry := range result.Entries {
		entries[i] = server.TranscriptRedactionEntry{
			EntryID: entry.EntryID, Sequence: entry.Sequence, Role: entry.Role,
			Kind: entry.Kind, Redacted: entry.Redacted, CreatedAt: entry.CreatedAt,
		}
	}
	return server.TranscriptRedactionReceipt{
		TranscriptID: result.TranscriptID, FromSequence: result.FromSequence, UntilSequence: result.UntilSequence,
		RedactionSetRevision: result.RedactionSetRevision, EntryCount: result.EntryCount,
		RedactableCount: result.RedactableCount, AlreadyRedactedCount: result.AlreadyRedactedCount,
		PartialCaptureEvent: result.PartialCaptureEvent, MemoryEvidenceCount: result.MemoryEvidenceCount,
		EvidenceMemoryIDs: result.EvidenceMemoryIDs, CurationRunsToCancel: result.CurationRunsToCancel,
		CancelledCurationRunCount: result.CancelledCurationRunCount, Entries: entries,
		ReceiptID: result.ReceiptID, RedactedCount: result.RedactedCount, RedactedAt: result.RedactedAt,
		Applied: result.Applied, Replayed: result.Replayed,
	}
}
