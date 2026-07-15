package store

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestMemorySourceCommitsMarkCurationDueExactlyOncePostgres(t *testing.T) {
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
		"memory-curation-due@witwave.ai", "memory curation due", time.Hour)
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
	p := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID, RealmID: realm.ID, AccountStatus: "active"}

	transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
		CreateTranscriptInput{ExternalID: "due-thread"})
	if err != nil {
		t.Fatal(err)
	}
	appendInput := AppendTranscriptEntryInput{
		ExternalID: "turn-1", Role: TranscriptRoleUser,
		Body: "Remember the architecture decision.",
	}
	entry, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
		transcript.ID, appendInput)
	if err != nil {
		t.Fatal(err)
	}
	assertCurationGeneration(ctx, t, st, p, 1)
	if _, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
		transcript.ID, appendInput); err != nil {
		t.Fatal(err)
	}
	assertCurationGeneration(ctx, t, st, p, 1)

	capture := CaptureMemoryInput{
		Content: "PostgreSQL is the sole canonical memory data store.", Kind: "decision",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState: MemoryEvidenceResolved, ResolvedKind: "transcript",
			SourceTranscriptID: transcript.ID, SourceSequenceFrom: entry.Sequence,
			SourceSequenceUntil: entry.Sequence,
		}},
		IdempotencyKey: "due-capture-1",
	}
	created, err := st.CaptureMemory(ctx, p, capture)
	if err != nil {
		t.Fatal(err)
	}
	assertCurationGeneration(ctx, t, st, p, 2)
	if replay, err := st.CaptureMemory(ctx, p, capture); err != nil || !replay.Receipt.Replayed {
		t.Fatalf("capture replay = %#v / %v", replay, err)
	}
	assertCurationGeneration(ctx, t, st, p, 2)

	adjustedContent := "PostgreSQL remains authoritative; retrieval indexes are derived."
	adjust := AdjustMemoryInput{
		ExpectedVersion: 1, Content: &adjustedContent,
		Reason: "clarify index boundary", IdempotencyKey: "due-adjust-1",
	}
	if _, err := st.AdjustMemory(ctx, p, created.Memory.ID, adjust); err != nil {
		t.Fatal(err)
	}
	assertCurationGeneration(ctx, t, st, p, 3)
	if replay, err := st.AdjustMemory(ctx, p, created.Memory.ID, adjust); err != nil || !replay.Receipt.Replayed {
		t.Fatalf("adjust replay = %#v / %v", replay, err)
	}
	assertCurationGeneration(ctx, t, st, p, 3)

	pendingCapture := CaptureMemoryInput{
		Content: "A pending source will be resolved later.", Kind: "observation",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState: MemoryEvidencePending,
			ExternalLocator: "codex://thread/due/turn/2",
		}},
		IdempotencyKey: "due-pending-capture-1",
	}
	pending, err := st.CaptureMemory(ctx, p, pendingCapture)
	if err != nil {
		t.Fatal(err)
	}
	assertCurationGeneration(ctx, t, st, p, 4)
	resolve := ResolveMemoryEvidenceInput{
		ResolvedKind: "transcript", SourceTranscriptID: transcript.ID,
		SourceSequenceFrom: entry.Sequence, SourceSequenceUntil: entry.Sequence,
		IdempotencyKey: "due-evidence-resolution-1",
	}
	if _, err := st.ResolveMemoryEvidence(ctx, p, pending.Memory.Evidence[0].ID, resolve); err != nil {
		t.Fatal(err)
	}
	assertCurationGeneration(ctx, t, st, p, 5)
	if _, err := st.ResolveMemoryEvidence(ctx, p, pending.Memory.Evidence[0].ID, resolve); err != nil {
		t.Fatal(err)
	}
	assertCurationGeneration(ctx, t, st, p, 5)

	var requestCount, receiptCount int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_curation_requests
		WHERE account_id=$1 AND realm_id=$2 AND owner_id=$3
		  AND state IN ('queued','claimed','retry_wait')`, p.AccountID, p.RealmID, p.ID).
		Scan(&requestCount); err != nil {
		t.Fatal(err)
	}
	if requestCount != 1 {
		t.Fatalf("open automatic requests = %d, want 1", requestCount)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_curation_mutations
		WHERE account_id=$1 AND realm_id=$2 AND owner_id=$3
		  AND operation='request' AND idempotency_key LIKE 'automatic:%'`,
		p.AccountID, p.RealmID, p.ID).Scan(&receiptCount); err != nil {
		t.Fatal(err)
	}
	if receiptCount != 5 {
		t.Fatalf("automatic trigger receipts = %d, want 5", receiptCount)
	}

	status, err := st.GetCurationStatus(ctx, p, "")
	if err != nil || status.Request == nil {
		t.Fatalf("queued status = %#v / %v", status, err)
	}
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: status.Request.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "due-start-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if started.Run.RequestGeneration != 5 {
		t.Fatalf("started generation = %d, want 5", started.Run.RequestGeneration)
	}
	lateCapture := CaptureMemoryInput{
		Content: "A source committed while an earlier generation is being curated.",
		Kind:    "observation",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState: MemoryEvidenceResolved, ResolvedKind: "transcript",
			SourceTranscriptID: transcript.ID, SourceSequenceFrom: entry.Sequence,
			SourceSequenceUntil: entry.Sequence,
		}},
		IdempotencyKey: "due-late-capture-1",
	}
	if _, err := st.CaptureMemory(ctx, p, lateCapture); err != nil {
		t.Fatal(err)
	}
	claimedStatus, err := st.GetCurationStatus(ctx, p, started.Run.ID)
	if err != nil || claimedStatus.Request == nil || claimedStatus.Run == nil {
		t.Fatalf("claimed status = %#v / %v", claimedStatus, err)
	}
	if claimedStatus.Request.State != MemoryCurationRequestClaimed ||
		claimedStatus.Request.RequestGeneration != 6 ||
		claimedStatus.Run.RequestGeneration != 5 ||
		claimedStatus.Lane.RequestGeneration != 6 {
		t.Fatalf("later trigger was lost into claimed run: %#v", claimedStatus)
	}
	var latestResultState string
	if err := st.pool.QueryRow(ctx, `
		SELECT result_state FROM memory_curation_mutations
		WHERE account_id=$1 AND realm_id=$2 AND owner_id=$3
		  AND operation='request' AND idempotency_key LIKE 'automatic:memory:%'
		ORDER BY created_at DESC,id DESC LIMIT 1`, p.AccountID, p.RealmID, p.ID).
		Scan(&latestResultState); err != nil {
		t.Fatal(err)
	}
	if latestResultState != MemoryCurationRequestClaimed {
		t.Fatalf("claimed automatic receipt state = %q", latestResultState)
	}
}

func assertCurationGeneration(ctx context.Context, t *testing.T, st *Store, p Principal, want int64) {
	t.Helper()
	status, err := st.GetCurationStatus(ctx, p, "")
	if err != nil {
		t.Fatal(err)
	}
	if status.Lane.RequestGeneration != want || status.Request == nil ||
		status.Request.RequestGeneration != want || status.Request.State != MemoryCurationRequestQueued {
		t.Fatalf("curation status generation = %#v, want %d queued", status, want)
	}
}
