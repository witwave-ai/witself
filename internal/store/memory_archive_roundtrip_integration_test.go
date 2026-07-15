package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	archiveexport "github.com/witwave-ai/witself/internal/export"
)

// TestNarrativeMemoryArchiveCellMovePostgres proves that an account archive
// carries the canonical narrative-memory rows needed for an actual cell move.
// The destination is a separately migrated schema, not a delete-and-reinsert
// in the source schema. In particular, memory_versions.search_document is a
// PostgreSQL-generated retrieval projection and is deliberately absent from
// the archive; successful destination recall proves it was rebuilt from the
// imported canonical content.
func TestNarrativeMemoryArchiveCellMovePostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	runNarrativeMemoryArchiveCellMovePostgres(
		t, dsn, dsn, "local-postgres", "local-postgres",
	)
}

// TestNarrativeMemoryManagedCloudConformance runs the same destructive,
// schema-isolated account-move contract against every managed PostgreSQL
// target and every directed provider pair. It is deliberately opt-in: each
// configured database user must be allowed to create and drop schemas, and a
// passing run is certification evidence for the exact endpoints supplied by
// the operator, not proof of a provider inferred from a hostname.
func TestNarrativeMemoryManagedCloudConformance(t *testing.T) {
	if os.Getenv("WITSELF_MEMORY_CLOUD_CONFORMANCE") != "1" {
		t.Skip("WITSELF_MEMORY_CLOUD_CONFORMANCE=1 is required")
	}
	certificationMode := false
	switch value := strings.TrimSpace(os.Getenv("WITSELF_MEMORY_CLOUD_CERTIFY")); value {
	case "", "0":
		t.Log("cloud conformance is running in rehearsal mode; no provider certification will be produced")
	case "1":
		certificationMode = true
	default:
		t.Fatalf("WITSELF_MEMORY_CLOUD_CERTIFY must be 0 or 1")
	}
	targets := []struct {
		name        string
		env         string
		resourceEnv string
	}{
		{name: "aws", env: "WITSELF_MEMORY_AWS_DATABASE_URL", resourceEnv: "WITSELF_MEMORY_AWS_RESOURCE_ID"},
		{name: "gcp", env: "WITSELF_MEMORY_GCP_DATABASE_URL", resourceEnv: "WITSELF_MEMORY_GCP_RESOURCE_ID"},
		{name: "azure", env: "WITSELF_MEMORY_AZURE_DATABASE_URL", resourceEnv: "WITSELF_MEMORY_AZURE_RESOURCE_ID"},
	}
	endpoints := make([]managedMemoryCloudEndpoint, 0, len(targets))
	for i := range targets {
		if strings.TrimSpace(os.Getenv(targets[i].env)) == "" {
			t.Fatalf("%s is required when cloud conformance is enabled", targets[i].env)
		}
		if certificationMode && strings.TrimSpace(os.Getenv(targets[i].resourceEnv)) == "" {
			t.Fatalf("%s is required when cloud certification is enabled", targets[i].resourceEnv)
		}
		if certificationMode {
			endpoints = append(endpoints, managedMemoryCloudEndpoint{
				Provider: targets[i].name, DSN: os.Getenv(targets[i].env),
				ResourceID: os.Getenv(targets[i].resourceEnv),
			})
		}
	}
	if certificationMode {
		assertManagedMemoryCloudEndpointsDistinct(t, endpoints)
	}

	for _, source := range targets {
		for _, destination := range targets {
			source := source
			destination := destination
			t.Run(source.name+"_to_"+destination.name, func(t *testing.T) {
				reporter := memoryArchiveTestReporter(t)
				if certificationMode {
					reporter = newManagedMemoryCloudArchiveReporter(t, source.name, destination.name)
				}
				runNarrativeMemoryArchiveCellMoveWithReporter(
					reporter, os.Getenv(source.env), os.Getenv(destination.env),
					source.name+"-managed-postgres", destination.name+"-managed-postgres",
				)
			})
		}
	}
}

func runNarrativeMemoryArchiveCellMovePostgres(
	t *testing.T,
	sourceDSN string,
	destinationDSN string,
	sourceCell string,
	destinationCell string,
) {
	t.Helper()
	runNarrativeMemoryArchiveCellMoveWithReporter(
		t, sourceDSN, destinationDSN, sourceCell, destinationCell,
	)
}

type memoryArchiveTestReporter interface {
	migrationTestReporter
	Logf(format string, args ...any)
}

func runNarrativeMemoryArchiveCellMoveWithReporter(
	t memoryArchiveTestReporter,
	sourceDSN string,
	destinationDSN string,
	sourceCell string,
	destinationCell string,
) {
	t.Helper()
	ctx := context.Background()
	source, _ := newMigrationTestStore(t, sourceDSN)
	destination, _ := newMigrationTestStore(t, destinationDSN)
	if err := source.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := destination.Migrate(); err != nil {
		t.Fatal(err)
	}
	assertMemoryArchiveRetrievalIndex(t, source)
	assertMemoryArchiveRetrievalIndex(t, destination)
	t.Logf("source %s PostgreSQL %s; destination %s PostgreSQL %s",
		sourceCell, memoryArchivePostgresVersion(t, source),
		destinationCell, memoryArchivePostgresVersion(t, destination))

	p, otherOwner := provisionMemoryArchiveCellMoveAccount(
		ctx, t, source, "memory-cell-move-source@witwave.ai", "memory cell move source",
	)

	transcript, err := source.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
		CreateTranscriptInput{ExternalID: "cell-move-thread", Title: "Cell move evidence"})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := source.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
		transcript.ID, AppendTranscriptEntryInput{
			ExternalID: "cell-move-turn", Role: TranscriptRoleUser,
			Body: "Remember the cellmove quasar portability decision.",
		})
	if err != nil {
		t.Fatal(err)
	}

	activeSalience := 0.95
	activeCapture := CaptureMemoryInput{
		Content: "The cellmove quasar decision starts in the source cell.",
		Kind:    "decision", Tags: []string{"portability", "cell-move"},
		Salience: &activeSalience, CaptureReason: "explicit_remember",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState: MemoryEvidencePending,
			ExternalLocator: "codex://cell-move-thread/turn/1",
		}},
		Client:         MemoryClientProvenance{Runtime: "codex", Model: "gpt-5"},
		IdempotencyKey: "cell-move-active-capture",
	}
	active, err := source.CaptureMemory(ctx, p, activeCapture)
	if err != nil {
		t.Fatal(err)
	}
	pendingEvidenceID := active.Memory.Evidence[0].ID
	resolveInput := ResolveMemoryEvidenceInput{
		ResolvedKind: "transcript", SourceTranscriptID: transcript.ID,
		SourceSequenceFrom: entry.Sequence, SourceSequenceUntil: entry.Sequence,
		IdempotencyKey: "cell-move-evidence-resolution",
	}
	resolvedEvidence, err := source.ResolveMemoryEvidence(ctx, p, pendingEvidenceID, resolveInput)
	if err != nil {
		t.Fatal(err)
	}
	activeContent := "The cellmove quasar decision uses PostgreSQL rows as authority; search indexes are derived."
	activeAdjusted, err := source.AdjustMemory(ctx, p, active.Memory.ID, AdjustMemoryInput{
		ExpectedVersion: active.Memory.Version, Content: &activeContent,
		AddTags: []string{"postgresql"}, Reason: "state the canonical-data boundary",
		IdempotencyKey: "cell-move-active-adjust",
	})
	if err != nil {
		t.Fatalf("request applied curation fixture: %v", err)
	}
	sensitiveContent := "The cellmove quasar vaultmarker detail remains sensitive across cells."
	sensitive, err := source.CaptureMemory(ctx, p, CaptureMemoryInput{
		Content: sensitiveContent, Kind: "decision",
		Tags: []string{"portability", "sensitive"}, Sensitive: true,
		CaptureReason: "explicit_remember",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState:    MemoryEvidenceUnavailable,
			TerminalReasonCode: "runtime_did_not_record",
		}},
		IdempotencyKey: "cell-move-sensitive-capture",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertMemoryArchiveCellMoveSensitiveRecall(ctx, t, source, p, sensitive.Memory)

	forgottenSalience := 0.85
	forgottenCapture := CaptureMemoryInput{
		Content: "A cellmove quasar note is intentionally forgotten during the move.",
		Kind:    "note", Tags: []string{"portability", "forgotten"},
		Salience: &forgottenSalience,
		Evidence: []MemoryEvidenceInput{{
			ResolutionState:    MemoryEvidenceUnavailable,
			TerminalReasonCode: "runtime_did_not_record",
		}},
		IdempotencyKey: "cell-move-forgotten-capture",
	}
	forgotten, err := source.CaptureMemory(ctx, p, forgottenCapture)
	if err != nil {
		t.Fatal(err)
	}
	forgetInput := MemoryLifecycleInput{
		ExpectedVersion: forgotten.Memory.Version, Reason: "keep out of ordinary recall",
		IdempotencyKey: "cell-move-forget",
	}
	forgottenHead, err := source.ForgetMemory(ctx, p, forgotten.Memory.ID, forgetInput)
	if err != nil {
		t.Fatal(err)
	}

	sourceToSplit, err := source.CaptureMemory(ctx, p, CaptureMemoryInput{
		Content: "The original cellmove quasar narrative combines two independent decisions.",
		Kind:    "decision", Tags: []string{"portability", "combined"},
		Evidence: []MemoryEvidenceInput{{
			ResolutionState:    MemoryEvidenceUnavailable,
			TerminalReasonCode: "runtime_did_not_record",
		}},
		IdempotencyKey: "cell-move-supersession-source",
	})
	if err != nil {
		t.Fatalf("start applied curation fixture: %v", err)
	}
	firstReplacementSalience := 0.75
	secondReplacementSalience := 0.55
	supersedeInput := SupersedeMemoryInput{
		MemoryID: sourceToSplit.Memory.ID, ExpectedVersion: sourceToSplit.Memory.Version,
		Reason: "split independent decisions",
		Replacements: []CaptureMemoryInput{
			{
				Content: "The first cellmove quasar decision keeps canonical history in PostgreSQL.",
				Kind:    "decision", Tags: []string{"portability", "first"},
				Salience: &firstReplacementSalience,
				Evidence: []MemoryEvidenceInput{{
					ResolutionState: MemoryEvidenceResolved, ResolvedKind: "memory",
					SourceMemoryID:      sourceToSplit.Memory.ID,
					SourceMemoryVersion: sourceToSplit.Memory.Version,
				}},
				IdempotencyKey: "cell-move-replacement-first",
			},
			{
				Content: "The second cellmove quasar decision rebuilds lexical retrieval in each cell.",
				Kind:    "decision", Tags: []string{"portability", "second"},
				Salience: &secondReplacementSalience,
				Evidence: []MemoryEvidenceInput{{
					ResolutionState:    MemoryEvidenceUnavailable,
					TerminalReasonCode: "runtime_did_not_record",
				}},
				IdempotencyKey: "cell-move-replacement-second",
			},
		},
		IdempotencyKey: "cell-move-supersede",
	}
	superseded, err := source.SupersedeMemory(ctx, p, supersedeInput)
	if err != nil {
		t.Fatal(err)
	}

	deletedCapture := CaptureMemoryInput{
		Content: "A deleted cellmove quasar narrative must never return after a cell move.",
		Kind:    "note", Tags: []string{"portability", "deleted"},
		Evidence: []MemoryEvidenceInput{{
			ResolutionState:    MemoryEvidenceUnavailable,
			TerminalReasonCode: "runtime_did_not_record",
		}},
		IdempotencyKey: "cell-move-deleted-capture",
	}
	deleted, err := source.CaptureMemory(ctx, p, deletedCapture)
	if err != nil {
		t.Fatal(err)
	}
	deletePreview, err := source.DeleteMemory(ctx, p, DeleteMemoryInput{MemoryID: deleted.Memory.ID})
	if err != nil {
		t.Fatal(err)
	}
	if deletePreview.Blocked {
		t.Fatalf("independent memory deletion unexpectedly blocked: %#v", deletePreview)
	}
	deleteReceipt, err := applyMemoryDeletePreview(
		ctx, source, p, deletePreview, "cell-move-permanent-delete",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !deleteReceipt.Applied || deleteReceipt.RetryShieldCount == 0 {
		t.Fatalf("permanent deletion did not create retry shields: %#v", deleteReceipt)
	}

	historyIDs := []string{
		active.Memory.ID,
		sensitive.Memory.ID,
		forgotten.Memory.ID,
		sourceToSplit.Memory.ID,
		superseded.Replacements[0].ID,
		superseded.Replacements[1].ID,
	}
	sourceHistories := memoryArchiveCellMoveHistories(ctx, t, source, p, historyIDs)
	if got := len(sourceHistories[active.Memory.ID]); got != 2 {
		t.Fatalf("active history has %d versions, want 2", got)
	}
	if got := len(sourceHistories[forgotten.Memory.ID]); got != 2 {
		t.Fatalf("forgotten history has %d versions, want 2", got)
	}
	if got := len(sourceHistories[sourceToSplit.Memory.ID]); got != 2 {
		t.Fatalf("superseded history has %d versions, want 2", got)
	}
	if activeAdjusted.Memory.State != MemoryStateActive ||
		forgottenHead.Memory.State != MemoryStateForgotten ||
		superseded.Source.State != MemoryStateSuperseded {
		t.Fatalf("source fixture states: active=%q forgotten=%q superseded=%q",
			activeAdjusted.Memory.State, forgottenHead.Memory.State, superseded.Source.State)
	}
	factInput := SetFactInput{
		Predicate:      "decisions/cell-move-authority",
		Value:          json.RawMessage(`"postgresql"`),
		SourceKind:     FactSourceAgent,
		IdempotencyKey: "cell-move-fact-set",
	}
	sourceFact, err := source.SetFact(ctx, p, factInput)
	if err != nil {
		t.Fatal(err)
	}
	sourceFactHistory, err := source.FactHistory(ctx, p, sourceFact.ID)
	if err != nil {
		t.Fatal(err)
	}
	insertMemoryArchiveCurationFixture(
		ctx, t, source, p, activeAdjusted.Memory.ID, activeAdjusted.Memory.Version,
	)
	vectorProfile, err := source.CreateMemoryVectorProfile(ctx, p, CreateMemoryVectorProfileInput{
		Provider: "archive-fixture", Model: "portable-two-axis",
		Recipe: "exact-memory-content", RecipeVersion: "1", Dimensions: 2,
		DistanceMetric: MemoryVectorMetricCosine,
		Normalization:  MemoryVectorNormalizationL2,
	})
	if err != nil {
		t.Fatal(err)
	}
	activeForVectors, err := source.ListMemories(ctx, p, MemoryListOptions{
		State: MemoryStateActive, IncludeSensitive: true, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(activeForVectors.Memories, func(i, j int) bool {
		return activeForVectors.Memories[i].ID < activeForVectors.Memories[j].ID
	})
	for i, memory := range activeForVectors.Memories {
		angle := float64(i+1) * 0.05
		if _, err := source.PutMemoryVector(ctx, p, PutMemoryVectorInput{
			ProfileID: vectorProfile.ID, MemoryID: memory.ID,
			MemoryVersion: memory.Version, ContentHash: memory.ContentHash,
			Vector: []float64{math.Cos(angle), math.Sin(angle)},
		}); err != nil {
			t.Fatalf("store archive vector for %s: %v", memory.ID, err)
		}
	}
	sourceHybrid := memoryArchiveCellMoveHybridRecall(ctx, t, source, p, vectorProfile.ID)
	if sourceHybrid.Degraded || sourceHybrid.VectorCoverage != 1 ||
		sourceHybrid.VectorMatches != len(activeForVectors.Memories) {
		t.Fatalf("source hybrid fixture is incomplete: %#v", sourceHybrid)
	}

	sourceRecall := memoryArchiveCellMoveRecall(ctx, t, source, p)
	wantRecallIDs := []string{
		active.Memory.ID,
		sensitive.Memory.ID,
		superseded.Replacements[0].ID,
		superseded.Replacements[1].ID,
	}
	sort.Strings(wantRecallIDs)
	if got := memoryArchiveCellMoveRecallIDs(sourceRecall); !reflect.DeepEqual(got, wantRecallIDs) {
		t.Fatalf("source recall IDs = %v, want %v", got, wantRecallIDs)
	}

	sourceCounts := memoryArchiveCellMoveCountsForAccount(ctx, t, source, p.AccountID)
	if sourceCounts.Relations != 2 || sourceCounts.DeletedHeads != 1 ||
		sourceCounts.RetryShields == 0 || sourceCounts.Evidence < 6 {
		t.Fatalf("source portability fixture is incomplete: %#v", sourceCounts)
	}

	if err := source.ExportAccount(ctx, p.AccountID, sourceCell, "test", io.Discard); !errors.Is(err, ErrAccountNotExportable) {
		t.Fatalf("active source export = %v, want ErrAccountNotExportable", err)
	}
	if err := source.SuspendAccountSystem(ctx, p.AccountID, "evacuation", "move narrative memory to another cell"); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := source.ExportAccount(ctx, p.AccountID, sourceCell, "test", &archive); err != nil {
		t.Fatal(err)
	}
	archiveBytes := archive.Bytes()
	manifest, versionRows := inspectMemoryArchiveCellMove(ctx, t, archiveBytes)
	if manifest.Cell != sourceCell || manifest.AccountID != p.AccountID {
		t.Fatalf("archive manifest = %#v", manifest)
	}
	if versionRows != sourceCounts.Versions {
		t.Fatalf("archive memory_versions rows = %d, want %d", versionRows, sourceCounts.Versions)
	}

	// The destination may already serve unrelated tenants, but the account
	// being moved must not exist there. Its neighboring tenant also gives the
	// post-import recall assertions a concrete isolation boundary.
	neighbor, _ := provisionMemoryArchiveCellMoveAccount(
		ctx, t, destination, "memory-cell-move-neighbor@witwave.ai", "memory cell move neighbor",
	)
	neighborMemory, err := destination.CaptureMemory(ctx, neighbor, CaptureMemoryInput{
		Content: "A neighboring tenant also uses the cellmove quasar phrase.",
		Kind:    "note",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState:    MemoryEvidenceUnavailable,
			TerminalReasonCode: "runtime_did_not_record",
		}},
		IdempotencyKey: "neighbor-cell-move-memory",
	})
	if err != nil {
		t.Fatalf("plan applied curation fixture: %v", err)
	}
	var targetExists bool
	if err := destination.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM accounts WHERE id=$1)`, p.AccountID,
	).Scan(&targetExists); err != nil {
		t.Fatal(err)
	}
	if targetExists {
		t.Fatal("destination target account was not clean before import")
	}

	importedManifest, err := destination.ImportAccount(ctx, p.AccountID, bytes.NewReader(archiveBytes))
	if err != nil {
		t.Fatal(err)
	}
	if importedManifest.AccountID != p.AccountID || importedManifest.Cell != sourceCell {
		t.Fatalf("imported manifest = %#v", importedManifest)
	}
	assertImportedMemoryArchiveCurationFixture(ctx, t, destination, p)
	if err := destination.ResumeAccountSystem(ctx, p.AccountID, "evacuation"); err != nil {
		t.Fatal(err)
	}

	destinationFact, err := destination.GetFact(ctx, p, "self", factInput.Predicate)
	if err != nil {
		t.Fatal(err)
	}
	if destinationFact.ID != sourceFact.ID ||
		destinationFact.ResolvedAssertionID != sourceFact.ResolvedAssertionID ||
		string(destinationFact.Value) != string(sourceFact.Value) {
		t.Fatalf("semantic fact changed across cells: source=%#v destination=%#v",
			sourceFact, destinationFact)
	}
	destinationFactHistory, err := destination.FactHistory(ctx, p, destinationFact.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(destinationFactHistory, sourceFactHistory) {
		t.Fatalf("semantic fact history changed across cells: source=%#v destination=%#v",
			sourceFactHistory, destinationFactHistory)
	}
	replayedFact, err := destination.SetFact(ctx, p, factInput)
	if err != nil {
		t.Fatal(err)
	}
	if replayedFact.ID != sourceFact.ID || replayedFact.ResolvedAssertionID != sourceFact.ResolvedAssertionID {
		t.Fatalf("semantic fact idempotency changed across cells: source=%#v replay=%#v",
			sourceFact, replayedFact)
	}
	if _, err := destination.GetFact(ctx, otherOwner, "self", factInput.Predicate); !errors.Is(err, ErrFactNotFound) {
		t.Fatalf("other owner semantic fact read = %v, want ErrFactNotFound", err)
	}

	destinationHistories := memoryArchiveCellMoveHistories(ctx, t, destination, p, historyIDs)
	if !reflect.DeepEqual(destinationHistories, sourceHistories) {
		sourceJSON, _ := json.MarshalIndent(sourceHistories, "", "  ")
		destinationJSON, _ := json.MarshalIndent(destinationHistories, "", "  ")
		t.Fatalf("memory histories changed across cells\nsource: %s\ndestination: %s",
			sourceJSON, destinationJSON)
	}

	destinationRecall := memoryArchiveCellMoveRecall(ctx, t, destination, p)
	if !reflect.DeepEqual(destinationRecall, sourceRecall) {
		t.Fatalf("lexical recall changed across cells\nsource: %#v\ndestination: %#v",
			sourceRecall, destinationRecall)
	}
	if got := memoryArchiveCellMoveRecallIDs(destinationRecall); !reflect.DeepEqual(got, wantRecallIDs) {
		t.Fatalf("destination recall IDs = %v, want %v", got, wantRecallIDs)
	}
	assertMemoryArchiveCellMoveSensitiveRecall(ctx, t, destination, p, sensitive.Memory)
	destinationProfiles, err := destination.ListMemoryVectorProfiles(ctx, p)
	if err != nil || len(destinationProfiles) != 1 ||
		destinationProfiles[0].ID != vectorProfile.ID ||
		destinationProfiles[0].ContractHash != vectorProfile.ContractHash {
		t.Fatalf("vector profile changed across cells: %#v / %v", destinationProfiles, err)
	}
	destinationHybrid := memoryArchiveCellMoveHybridRecall(ctx, t, destination, p, vectorProfile.ID)
	if !reflect.DeepEqual(destinationHybrid, sourceHybrid) {
		t.Fatalf("hybrid vector recall changed across cells\nsource: %#v\ndestination: %#v",
			sourceHybrid, destinationHybrid)
	}
	if containsString(memoryArchiveCellMoveRecallIDs(destinationRecall), neighborMemory.Memory.ID) {
		t.Fatal("neighbor memory leaked into imported account recall")
	}

	neighborRecall := memoryArchiveCellMoveRecall(ctx, t, destination, neighbor)
	if got := memoryArchiveCellMoveRecallIDs(neighborRecall); !reflect.DeepEqual(got, []string{neighborMemory.Memory.ID}) {
		t.Fatalf("neighbor recall crossed account boundary: %v", got)
	}
	if _, err := destination.GetMemory(ctx, otherOwner, active.Memory.ID); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("other owner exact read = %v, want ErrMemoryNotFound", err)
	}
	if _, err := destination.GetMemory(ctx, p, neighborMemory.Memory.ID); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("cross-account exact read = %v, want ErrMemoryNotFound", err)
	}

	destinationCounts := memoryArchiveCellMoveCountsForAccount(ctx, t, destination, p.AccountID)
	if !reflect.DeepEqual(destinationCounts, sourceCounts) {
		t.Fatalf("canonical memory row counts changed across cells: source=%#v destination=%#v",
			sourceCounts, destinationCounts)
	}
	var rebuiltSearchDocuments int64
	if err := destination.pool.QueryRow(ctx, `
		SELECT count(*)
		FROM memory_versions v
		WHERE v.account_id=$1 AND v.state='active'
		  AND v.search_document @@ websearch_to_tsquery('simple'::regconfig,'cellmove quasar')`,
		p.AccountID).Scan(&rebuiltSearchDocuments); err != nil {
		t.Fatal(err)
	}
	if rebuiltSearchDocuments < int64(len(wantRecallIDs)) {
		t.Fatalf("destination rebuilt %d matching search documents, want at least %d",
			rebuiltSearchDocuments, len(wantRecallIDs))
	}

	resolvedReplay, err := destination.ResolveMemoryEvidence(ctx, p, pendingEvidenceID, resolveInput)
	if err != nil || resolvedReplay.ID != resolvedEvidence.ID {
		t.Fatalf("evidence retry after cell move = %#v / %v", resolvedReplay, err)
	}
	forgottenReplay, err := destination.ForgetMemory(ctx, p, forgotten.Memory.ID, forgetInput)
	if err != nil || !forgottenReplay.Receipt.Replayed ||
		forgottenReplay.Memory.Version != forgottenHead.Memory.Version {
		t.Fatalf("forget retry after cell move = %#v / %v", forgottenReplay, err)
	}
	supersedeReplay, err := destination.SupersedeMemory(ctx, p, supersedeInput)
	if err != nil || !supersedeReplay.Receipt.Replayed ||
		supersedeReplay.Receipt.SupersessionSetID != superseded.Receipt.SupersessionSetID {
		t.Fatalf("supersession retry after cell move = %#v / %v", supersedeReplay, err)
	}
	if _, err := destination.CaptureMemory(ctx, p, deletedCapture); !errors.Is(err, ErrMemoryDeleted) {
		t.Fatalf("permanently deleted capture retry after cell move = %v, want ErrMemoryDeleted", err)
	}
}

func insertMemoryArchiveCurationFixture(
	ctx context.Context,
	t memoryArchiveTestReporter,
	st *Store,
	p Principal,
	memoryID string,
	memoryVersion int64,
) {
	t.Helper()
	// Source writes create/coalesce automatic curation work. Replace that
	// incidental queue with a deterministic graph created through the same
	// public state machine whose archive representation is under test.
	if _, err := st.pool.Exec(ctx, `DELETE FROM memory_curation_lanes
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3`,
		p.AccountID, p.RealmID, p.ID); err != nil {
		t.Fatal(err)
	}
	scope := MemoryCurationScope{
		Sources: []string{MemoryCurationSourceMemory}, MemoryStates: []string{MemoryStateActive},
		MaxMemories: 500,
	}
	createDraft := func(localRef, content, sourceMemoryID string, sourceMemoryVersion int64) []byte {
		t.Helper()
		draft := MemoryCurationPlanDraft{
			Schema: MemoryCurationPlanSchemaV1, DraftRevision: 1,
			Actions: []MemoryCurationPlanAction{{
				Ordinal: 1, Operation: MemoryCurationOperationCreate,
				Create: &MemoryCurationCreateAction{
					LocalRef: localRef,
					Snapshot: MemoryCurationMemorySnapshot{
						Content: content, Kind: "note",
						Evidence: []MemoryCurationEvidence{{
							Type: "memory", ResolutionState: MemoryEvidenceResolved,
							ResolvedKind: MemoryCurationSourceMemory,
							SourceMemory: &MemoryCurationVersionReference{
								MemoryID: sourceMemoryID, Version: sourceMemoryVersion,
							},
						}},
					},
				},
			}},
		}
		raw, err := json.Marshal(draft)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	appliedRequest, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope: scope, CoalescingKey: "archive-applied", TriggerReason: "manual",
		IdempotencyKey: "archive-request-applied",
	})
	if err != nil {
		t.Fatal(err)
	}
	appliedStart, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: appliedRequest.Request.ID, LeaseDuration: 5 * time.Minute,
		Client:         MemoryClientProvenance{Runtime: "archive-test"},
		IdempotencyKey: "archive-start-applied",
	})
	if err != nil {
		t.Fatal(err)
	}
	appliedPlan, err := st.PlanCuration(ctx, p, appliedStart.Run.ID, PlanMemoryCurationInput{
		FencingGeneration: appliedStart.Run.FencingGeneration,
		Draft:             createDraft("portable", "Portable curation archive fixture.", memoryID, memoryVersion),
		IdempotencyKey:    "archive-plan-applied",
	})
	if err != nil {
		t.Fatal(err)
	}
	applied, err := st.ApplyCuration(ctx, p, appliedStart.Run.ID, ApplyMemoryCurationInput{
		FencingGeneration: appliedStart.Run.FencingGeneration,
		PlanRevision:      appliedPlan.Run.PlanRevision, PlanHash: appliedPlan.Run.PlanHash,
		IdempotencyKey: "archive-apply",
	})
	if err != nil {
		t.Fatalf("apply curation fixture: %v", err)
	}
	if len(applied.Receipt.ActionResults) != 1 || len(applied.Receipt.ActionResults[0].AfterHeads) != 1 {
		t.Fatalf("unexpected apply receipt: %+v", applied.Receipt)
	}
	produced := applied.Receipt.ActionResults[0].AfterHeads[0]

	activeRequest, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope: scope, CoalescingKey: "archive-active", TriggerReason: "manual",
		IdempotencyKey: "archive-request-active",
	})
	if err != nil {
		t.Fatalf("request active curation fixture: %v", err)
	}
	activeStart, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: activeRequest.Request.ID, LeaseDuration: 5 * time.Minute,
		Client:         MemoryClientProvenance{Runtime: "archive-test"},
		IdempotencyKey: "archive-start-active",
	})
	if err != nil {
		t.Fatalf("start active curation fixture: %v", err)
	}
	activeInputs, err := st.GetCurationRunInputs(ctx, p, activeStart.Run.ID,
		activeStart.Run.FencingGeneration, "", 200)
	if err != nil {
		t.Fatalf("read active curation fixture inputs: %v", err)
	}
	producedWasFrozen := false
	for _, input := range activeInputs.Inputs {
		if input.Kind == MemoryCurationInputMemory && input.MemoryID == produced.MemoryID &&
			input.MemoryVersion == produced.Version {
			producedWasFrozen = true
			break
		}
	}
	if !producedWasFrozen {
		t.Fatalf("first apply output %s/%d was not frozen into the second run",
			produced.MemoryID, produced.Version)
	}
	if _, err := st.PlanCuration(ctx, p, activeStart.Run.ID, PlanMemoryCurationInput{
		FencingGeneration: activeStart.Run.FencingGeneration,
		Draft:             createDraft("active", "Active curation archive fixture.", produced.MemoryID, produced.Version),
		IdempotencyKey:    "archive-plan-active",
	}); err != nil {
		t.Fatalf("plan active curation fixture: %v", err)
	}
}

func assertImportedMemoryArchiveCurationFixture(
	ctx context.Context,
	t memoryArchiveTestReporter,
	st *Store,
	p Principal,
) {
	t.Helper()
	var activeRun *string
	var fence int64
	if err := st.pool.QueryRow(ctx, `
		SELECT active_run_id,fencing_generation FROM memory_curation_lanes
		WHERE account_id=$1 AND realm_id=$2 AND owner_id=$3`,
		p.AccountID, p.RealmID, p.ID).Scan(&activeRun, &fence); err != nil {
		t.Fatal(err)
	}
	if activeRun != nil || fence != 3 {
		t.Fatalf("imported curation lane active_run=%v fence=%d, want nil/3", activeRun, fence)
	}
	var requestState, runState, terminalReason string
	var claimedRun *string
	var lease *time.Time
	if err := st.pool.QueryRow(ctx, `
		SELECT q.state,q.claimed_run_id,r.state,r.lease_expires_at,r.terminal_reason_code
		FROM memory_curation_requests q
		JOIN memory_curation_runs r ON r.request_id=q.id
		WHERE q.account_id=$1 AND q.realm_id=$2 AND q.owner_id=$3
		  AND r.account_id=$1 AND r.realm_id=$2 AND r.owner_id=$3
		  AND r.state='interrupted'
		ORDER BY r.created_at DESC LIMIT 1`, p.AccountID, p.RealmID, p.ID).Scan(
		&requestState, &claimedRun, &runState, &lease, &terminalReason,
	); err != nil {
		t.Fatal(err)
	}
	if requestState != "queued" || claimedRun != nil || runState != "interrupted" ||
		lease != nil || terminalReason != "cell_import" {
		t.Fatalf("active curation lease crossed cells: request=%q claimed=%v run=%q lease=%v reason=%q",
			requestState, claimedRun, runState, lease, terminalReason)
	}
	var appliedRuns, actions, inputs, mutations int64
	if err := st.pool.QueryRow(ctx, `
		SELECT
		 (SELECT count(*) FROM memory_curation_runs WHERE account_id=$1 AND state='applied'),
		 (SELECT count(*) FROM memory_curation_actions WHERE account_id=$1),
		 (SELECT count(*) FROM memory_curation_run_inputs WHERE account_id=$1),
		 (SELECT count(*) FROM memory_curation_mutations WHERE account_id=$1)`,
		p.AccountID).Scan(&appliedRuns, &actions, &inputs, &mutations); err != nil {
		t.Fatal(err)
	}
	if appliedRuns != 1 || actions != 2 || inputs < 2 || mutations < 7 {
		t.Fatalf("curation graph changed across cells: applied=%d actions=%d inputs=%d mutations=%d",
			appliedRuns, actions, inputs, mutations)
	}
}

type memoryArchiveCellMoveRecallHit struct {
	ID       string
	Content  string
	Version  int64
	State    string
	Lexical  float64
	Salience float64
}

type memoryArchiveCellMoveHybridHit struct {
	ID         string
	Similarity float64
	VectorUsed bool
}

type memoryArchiveCellMoveHybridResult struct {
	Hits               []memoryArchiveCellMoveHybridHit
	RetrievalMode      string
	VectorCoverage     float64
	VectorCandidates   int
	VectorMatches      int
	CandidateTruncated bool
	Degraded           bool
	DegradedReason     string
}

type memoryArchiveCellMoveCounts struct {
	Memories     int64
	Versions     int64
	Evidence     int64
	Relations    int64
	DeletedRefs  int64
	DeletedHeads int64
	RetryShields int64
}

func provisionMemoryArchiveCellMoveAccount(
	ctx context.Context,
	t memoryArchiveTestReporter,
	st *Store,
	email string,
	displayName string,
) (Principal, Principal) {
	t.Helper()
	provisioned, err := st.ProvisionAccount(ctx, email, displayName, time.Hour)
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
	owner, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "primary")
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "other")
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{
		Kind: PrincipalAgent, ID: owner.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AccountStatus: "active",
	}
	otherPrincipal := principal
	otherPrincipal.ID = other.ID
	return principal, otherPrincipal
}

func memoryArchiveCellMoveHistories(
	ctx context.Context,
	t memoryArchiveTestReporter,
	st *Store,
	p Principal,
	memoryIDs []string,
) map[string][]Memory {
	t.Helper()
	histories := make(map[string][]Memory, len(memoryIDs))
	for _, memoryID := range memoryIDs {
		var versions []Memory
		cursor := ""
		for {
			page, err := st.GetMemoryHistoryPage(ctx, p, memoryID, MemoryHistoryOptions{
				Limit: 2, Cursor: cursor,
			})
			if err != nil {
				t.Fatal(err)
			}
			versions = append(versions, page.Versions...)
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		histories[memoryID] = versions
	}
	return histories
}

func memoryArchiveCellMoveRecall(
	ctx context.Context,
	t memoryArchiveTestReporter,
	st *Store,
	p Principal,
) []memoryArchiveCellMoveRecallHit {
	t.Helper()
	page, err := st.RecallMemories(ctx, p, MemoryRecallOptions{
		Query: "cellmove quasar", IncludeSensitive: true, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor != "" || page.RetrievalMode != "lexical" || page.Degraded {
		t.Fatalf("unexpected recall page metadata: %#v", page)
	}
	hits := make([]memoryArchiveCellMoveRecallHit, 0, len(page.Hits))
	for _, hit := range page.Hits {
		hits = append(hits, memoryArchiveCellMoveRecallHit{
			ID: hit.Memory.ID, Content: hit.Memory.Content,
			Version: hit.Memory.Version, State: hit.Memory.State,
			Lexical: hit.Score.Lexical, Salience: hit.Score.Salience,
		})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].ID < hits[j].ID })
	return hits
}

func assertMemoryArchiveCellMoveSensitiveRecall(
	ctx context.Context,
	t memoryArchiveTestReporter,
	st *Store,
	p Principal,
	expected Memory,
) {
	t.Helper()
	redacted, err := st.RecallMemories(ctx, p, MemoryRecallOptions{
		Query: "vaultmarker", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(redacted.Hits) != 1 {
		t.Fatalf("default sensitive recall returned %d hits: %#v", len(redacted.Hits), redacted)
	}
	redactedMemory := redacted.Hits[0].Memory
	if redactedMemory.ID != expected.ID || !redactedMemory.Sensitive || !redactedMemory.Redacted ||
		redactedMemory.Content != "" || redactedMemory.ContentHash != "" ||
		len(redactedMemory.Tags) != 0 || len(redactedMemory.Links) != 0 ||
		redactedMemory.CaptureReason != "" || redactedMemory.IdempotencyKey != "" {
		t.Fatalf("default recall did not fully redact sensitive memory: %#v", redactedMemory)
	}
	revealed, err := st.RecallMemories(ctx, p, MemoryRecallOptions{
		Query: "vaultmarker", IncludeSensitive: true, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(revealed.Hits) != 1 {
		t.Fatalf("authorized sensitive recall returned %d hits: %#v", len(revealed.Hits), revealed)
	}
	revealedMemory := revealed.Hits[0].Memory
	if revealedMemory.ID != expected.ID || revealedMemory.Version != expected.Version ||
		revealedMemory.State != MemoryStateActive || !revealedMemory.Sensitive || revealedMemory.Redacted ||
		revealedMemory.Content != expected.Content || revealedMemory.ContentHash != expected.ContentHash ||
		!reflect.DeepEqual(revealedMemory.Tags, expected.Tags) ||
		revealedMemory.CaptureReason != expected.CaptureReason {
		t.Fatalf("authorized recall changed sensitive memory: got=%#v want=%#v", revealedMemory, expected)
	}
}

func memoryArchiveCellMoveHybridRecall(ctx context.Context, t memoryArchiveTestReporter, st *Store, p Principal, profileID string) memoryArchiveCellMoveHybridResult {
	t.Helper()
	page, err := st.RecallMemories(ctx, p, MemoryRecallOptions{
		VectorProfileID: profileID, QueryVector: []float64{1, 0}, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := memoryArchiveCellMoveHybridResult{
		Hits: []memoryArchiveCellMoveHybridHit{}, RetrievalMode: page.RetrievalMode,
		VectorCoverage: page.VectorCoverage, VectorCandidates: page.VectorCandidates,
		VectorMatches: page.VectorMatches, CandidateTruncated: page.CandidateTruncated,
		Degraded: page.Degraded, DegradedReason: page.DegradedReason,
	}
	for _, hit := range page.Hits {
		result.Hits = append(result.Hits, memoryArchiveCellMoveHybridHit{
			ID: hit.Memory.ID, Similarity: hit.Score.Similarity,
			VectorUsed: hit.Score.VectorUsed,
		})
	}
	return result
}

func memoryArchiveCellMoveRecallIDs(hits []memoryArchiveCellMoveRecallHit) []string {
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.ID)
	}
	sort.Strings(ids)
	return ids
}

func memoryArchiveCellMoveCountsForAccount(
	ctx context.Context,
	t memoryArchiveTestReporter,
	st *Store,
	accountID string,
) memoryArchiveCellMoveCounts {
	t.Helper()
	var counts memoryArchiveCellMoveCounts
	if err := st.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM memories WHERE account_id=$1),
		  (SELECT count(*) FROM memory_versions WHERE account_id=$1),
		  (SELECT count(*) FROM memory_evidence WHERE account_id=$1),
		  (SELECT count(*) FROM memory_relations WHERE account_id=$1),
		  (SELECT count(*) FROM memory_deleted_references WHERE account_id=$1),
		  (SELECT count(*) FROM memories WHERE account_id=$1 AND current_version IS NULL),
		  (SELECT count(*) FROM memory_deleted_references
		     WHERE account_id=$1 AND former_reference_kind LIKE 'idempotency.%')`,
		accountID).Scan(
		&counts.Memories, &counts.Versions, &counts.Evidence,
		&counts.Relations, &counts.DeletedRefs, &counts.DeletedHeads,
		&counts.RetryShields,
	); err != nil {
		t.Fatal(err)
	}
	return counts
}

func inspectMemoryArchiveCellMove(
	ctx context.Context,
	t memoryArchiveTestReporter,
	archive []byte,
) (archiveexport.Manifest, int64) {
	t.Helper()
	var versionRows int64
	var profileRows, vectorRows int64
	manifest, err := archiveexport.Read(ctx, bytes.NewReader(archive), archiveexport.ImportOptions{
		CurrentSchema: SchemaVersion(),
		OnManifest: func(manifest archiveexport.Manifest) error {
			required := map[string]bool{
				"memory_change_clocks":      false,
				"memories":                  false,
				"memory_versions":           false,
				"memory_vector_profiles":    false,
				"memory_vectors":            false,
				"memory_evidence":           false,
				"memory_relations":          false,
				"memory_deleted_references": false,
			}
			for _, table := range manifest.Tables {
				lower := strings.ToLower(table)
				if strings.Contains(lower, "index") || strings.Contains(lower, "search") ||
					strings.Contains(lower, "embedding") {
					return errors.New("archive unexpectedly contains a derived retrieval table: " + table)
				}
				if _, ok := required[table]; ok {
					required[table] = true
				}
			}
			for table, seen := range required {
				if !seen {
					return errors.New("archive omitted canonical memory table: " + table)
				}
			}
			return nil
		},
		Row: func(table string, row []byte) error {
			if table == "memory_vector_profiles" {
				profileRows++
				return nil
			}
			if table == "memory_vectors" {
				vectorRows++
				return nil
			}
			if table != "memory_versions" {
				return nil
			}
			versionRows++
			var object map[string]json.RawMessage
			if err := json.Unmarshal(row, &object); err != nil {
				return err
			}
			if _, archived := object["search_document"]; archived {
				return errors.New("generated memory_versions.search_document was archived")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if profileRows != 1 || vectorRows == 0 {
		t.Fatalf("archive vector rows profiles=%d vectors=%d", profileRows, vectorRows)
	}
	return manifest, versionRows
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func memoryArchivePostgresVersion(t memoryArchiveTestReporter, st *Store) string {
	t.Helper()
	var version string
	if err := st.pool.QueryRow(context.Background(), `SHOW server_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func assertMemoryArchiveRetrievalIndex(t memoryArchiveTestReporter, st *Store) {
	t.Helper()
	var accessMethod string
	var valid, ready bool
	if err := st.pool.QueryRow(context.Background(), `
		SELECT am.amname,i.indisvalid,i.indisready
		FROM pg_class idx
		JOIN pg_namespace ns ON ns.oid=idx.relnamespace
		JOIN pg_index i ON i.indexrelid=idx.oid
		JOIN pg_am am ON am.oid=idx.relam
		WHERE ns.nspname=current_schema()
		  AND idx.relname='memory_versions_search'`).Scan(
		&accessMethod, &valid, &ready,
	); err != nil {
		t.Fatalf("read destination lexical index: %v", err)
	}
	if accessMethod != "gin" || !valid || !ready {
		t.Fatalf("memory_versions_search access=%q valid=%t ready=%t, want gin/true/true",
			accessMethod, valid, ready)
	}
}
