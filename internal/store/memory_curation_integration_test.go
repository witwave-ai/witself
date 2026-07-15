package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestMemoryCurationLanePostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx,
		"memory-curation@witwave.ai", "memory curation", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "primary")
	if err != nil {
		t.Fatal(err)
	}
	otherAgent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "other")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID, RealmID: realm.ID, AccountStatus: "active"}

	transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
		CreateTranscriptInput{ExternalID: "curation-thread"})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
		transcript.ID, AppendTranscriptEntryInput{ExternalID: "turn-1", Role: TranscriptRoleUser, Body: "Remember our database decision."})
	if err != nil {
		t.Fatal(err)
	}
	created, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
		Content: "PostgreSQL remains the sole canonical memory store.", Kind: "decision",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState: MemoryEvidenceResolved, ResolvedKind: "transcript",
			SourceTranscriptID: transcript.ID, SourceSequenceFrom: entry.Sequence,
			SourceSequenceUntil: entry.Sequence,
		}},
		IdempotencyKey: "curation-memory-capture",
	})
	if err != nil {
		t.Fatal(err)
	}
	initialStatus, err := st.GetCurationStatus(ctx, p, "")
	if err != nil {
		t.Fatal(err)
	}
	// Transcript and memory source commits automatically mark curation due.
	// Explicit requests advance the same server-owned generation rather than
	// resetting it to a test-local starting value.
	baseGeneration := initialStatus.Lane.RequestGeneration

	requestInput := RequestMemoryCurationInput{
		TriggerReason: "session_end", IdempotencyKey: "curation-request-1",
	}
	requested, err := st.RequestCuration(ctx, p, requestInput)
	if err != nil {
		t.Fatal(err)
	}
	if requested.Request.State != MemoryCurationRequestQueued || requested.Receipt.Replayed || requested.Request.RequestGeneration != baseGeneration+1 {
		t.Fatalf("request = %#v", requested)
	}
	replayedRequest, err := st.RequestCuration(ctx, p, requestInput)
	if err != nil || !replayedRequest.Receipt.Replayed || replayedRequest.Request.ID != requested.Request.ID {
		t.Fatalf("request replay = %#v / %v", replayedRequest, err)
	}
	changedRequest := requestInput
	changedRequest.Priority = 10
	if _, err := st.RequestCuration(ctx, p, changedRequest); !errors.Is(err, ErrMemoryCurationIdempotencyConflict) {
		t.Fatalf("changed request replay error = %v", err)
	}
	coalescedInput := requestInput
	coalescedInput.IdempotencyKey = "curation-request-2"
	coalesced, err := st.RequestCuration(ctx, p, coalescedInput)
	if err != nil {
		t.Fatal(err)
	}
	if coalesced.Request.ID != requested.Request.ID || coalesced.Request.RequestGeneration != baseGeneration+2 || coalesced.Receipt.RequestGeneration != baseGeneration+2 {
		t.Fatalf("coalesced request = %#v", coalesced)
	}

	startInput := StartMemoryCurationInput{
		RequestID: requested.Request.ID, LeaseDuration: time.Minute,
		Client:         MemoryClientProvenance{Runtime: "codex", Model: "gpt-5"},
		IdempotencyKey: "curation-start-1",
	}
	started, err := st.StartCuration(ctx, p, startInput)
	if err != nil {
		t.Fatal(err)
	}
	if started.Run.State != MemoryCurationRunOpen || started.Run.FencingGeneration != 1 ||
		started.Run.MemoryInputCount != 1 || started.Run.EvidenceInputCount != 1 ||
		started.Run.TranscriptInputCount != 1 || started.Run.CursorInputCount != 3 ||
		started.FirstInputCursor == "" {
		t.Fatalf("started run = %#v", started)
	}
	replayedStart, err := st.StartCuration(ctx, p, startInput)
	if err != nil || !replayedStart.Receipt.Replayed || replayedStart.Run.ID != started.Run.ID {
		t.Fatalf("start replay = %#v / %v", replayedStart, err)
	}

	var gotMemory, gotEvidence, gotTranscript, gotCursor bool
	cursor := started.FirstInputCursor
	for {
		page, err := st.GetCurationRunInputs(ctx, p, started.Run.ID,
			started.Run.FencingGeneration, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, input := range page.Inputs {
			switch input.Kind {
			case MemoryCurationInputMemory:
				gotMemory = input.Memory != nil && input.Memory.ID == created.Memory.ID && input.Memory.Content == created.Memory.Content
			case MemoryCurationInputEvidence:
				gotEvidence = input.Evidence != nil && input.Evidence.MemoryID == created.Memory.ID
			case MemoryCurationInputTranscript:
				gotTranscript = len(input.TranscriptEntries) == 1 && input.TranscriptEntries[0].ID == entry.ID
			case MemoryCurationInputCursor:
				gotCursor = input.CursorUpper > input.CursorExpectedPrior
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if !gotMemory || !gotEvidence || !gotTranscript || !gotCursor {
		t.Fatalf("hydrated inputs memory=%v evidence=%v transcript=%v cursor=%v", gotMemory, gotEvidence, gotTranscript, gotCursor)
	}
	if _, err := st.GetCurationRunInputs(ctx, p, started.Run.ID,
		started.Run.FencingGeneration+1, "", 10); !errors.Is(err, ErrMemoryCurationFenceMismatch) {
		t.Fatalf("wrong fence read error = %v", err)
	}
	other := p
	other.ID = otherAgent.ID
	if _, err := st.GetCurationRun(ctx, other, started.Run.ID); !errors.Is(err, ErrMemoryCurationNotFound) {
		t.Fatalf("cross-owner run read error = %v", err)
	}

	renewInput := RenewMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration,
		Extension:         time.Minute, IdempotencyKey: "curation-renew-1",
	}
	renewed, err := st.RenewCuration(ctx, p, started.Run.ID, renewInput)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Run.LeaseExpiresAt == nil || renewed.Receipt.LeaseExpiresAt == nil {
		t.Fatalf("renewed = %#v", renewed)
	}
	renewReplay, err := st.RenewCuration(ctx, p, started.Run.ID, renewInput)
	if err != nil || !renewReplay.Receipt.Replayed {
		t.Fatalf("renew replay = %#v / %v", renewReplay, err)
	}
	changedRenew := renewInput
	changedRenew.Extension = 2 * time.Minute
	if _, err := st.RenewCuration(ctx, p, started.Run.ID, changedRenew); !errors.Is(err, ErrMemoryCurationIdempotencyConflict) {
		t.Fatalf("changed renew replay error = %v", err)
	}

	followUp, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		CoalescingKey: "follow_up", TriggerReason: "new_evidence",
		IdempotencyKey: "curation-follow-up",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: followUp.Request.ID, IdempotencyKey: "busy-start",
	}); !errors.Is(err, ErrMemoryCurationBusy) {
		t.Fatalf("parallel owner start error = %v", err)
	}
	cancelInput := FinishMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration,
		Reason:            "user_cancelled", IdempotencyKey: "curation-cancel-1",
	}
	cancelled, err := st.CancelCuration(ctx, p, started.Run.ID, cancelInput)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Run.State != MemoryCurationRunAbandoned {
		t.Fatalf("cancelled run = %#v", cancelled.Run)
	}
	cancelReplay, err := st.CancelCuration(ctx, p, started.Run.ID, cancelInput)
	if err != nil || !cancelReplay.Receipt.Replayed {
		t.Fatalf("cancel replay = %#v / %v", cancelReplay, err)
	}
	status, err := st.GetCurationStatus(ctx, p, started.Run.ID)
	if err != nil || status.Run == nil || status.Request == nil ||
		status.Request.State != MemoryCurationRequestCancelled || status.Lane.ActiveRunID != "" {
		t.Fatalf("cancel status = %#v / %v", status, err)
	}

	second, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: followUp.Request.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "curation-start-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Run.FencingGeneration <= started.Run.FencingGeneration {
		t.Fatalf("successor fence = %d after %d", second.Run.FencingGeneration, started.Run.FencingGeneration)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE memory_curation_runs SET lease_expires_at=clock_timestamp()-interval '1 second'
		WHERE id=$1`, second.Run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetCurationRunInputs(ctx, p, second.Run.ID,
		second.Run.FencingGeneration, second.FirstInputCursor, 10); !errors.Is(err, ErrMemoryCurationLeaseExpired) {
		t.Fatalf("expired input read error = %v", err)
	}
	expiredStatus, err := st.GetCurationStatus(ctx, p, second.Run.ID)
	if err != nil || expiredStatus.Run == nil || expiredStatus.Request == nil ||
		expiredStatus.Run.State != MemoryCurationRunInterrupted ||
		expiredStatus.Request.State != MemoryCurationRequestRetryWait ||
		expiredStatus.Lane.FencingGeneration <= second.Run.FencingGeneration {
		t.Fatalf("expired status = %#v / %v", expiredStatus, err)
	}
	if _, err := st.RenewCuration(ctx, p, second.Run.ID, RenewMemoryCurationInput{
		FencingGeneration: second.Run.FencingGeneration, Extension: time.Minute,
		IdempotencyKey: "stale-renew",
	}); !errors.Is(err, ErrMemoryCurationFenceMismatch) {
		t.Fatalf("stale renew error = %v", err)
	}

	abandonRequest, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		CoalescingKey: "abandon_lane", TriggerReason: "manual_refine",
		IdempotencyKey: "curation-abandon-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	abandonRun, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: abandonRequest.Request.ID, IdempotencyKey: "curation-start-3",
	})
	if err != nil {
		t.Fatal(err)
	}
	abandoned, err := st.AbandonCuration(ctx, p, abandonRun.Run.ID,
		FinishMemoryCurationInput{FencingGeneration: abandonRun.Run.FencingGeneration,
			Reason: "client_shutdown", IdempotencyKey: "curation-abandon-1"})
	if err != nil {
		t.Fatal(err)
	}
	if abandoned.Run.State != MemoryCurationRunAbandoned {
		t.Fatalf("abandoned run = %#v", abandoned.Run)
	}
	abandonedRequest, err := st.GetCurationRequest(ctx, p, abandonRequest.Request.ID)
	if err != nil || abandonedRequest.State != MemoryCurationRequestRetryWait || abandonedRequest.AttemptCount != 1 {
		t.Fatalf("abandoned request = %#v / %v", abandonedRequest, err)
	}
}
