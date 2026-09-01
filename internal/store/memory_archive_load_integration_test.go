package store

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	archiveexport "github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/loadquality"
)

const (
	memoryArchiveLoadEnabled     = "WITSELF_MEMORY_ARCHIVE_LOAD"
	memoryArchiveLoadOverall     = 12 * time.Minute
	memoryArchiveLoadRecallLimit = 20
)

type memoryArchiveLoadFixture struct {
	cardinality       int
	principal         Principal
	peer              Principal
	profile           MemoryVectorProfile
	vectorReceipts    []MemoryVectorReceipt
	memories          []Memory
	sensitive         Memory
	sensitiveQuery    string
	sensitiveBroadPre bool
	archive           []byte
	snapshot          memoryArchiveLoadSnapshot
	recallSpecs       []memoryArchiveLoadRecallSpec
	beforeRecall      []memoryArchiveLoadRecallResult
	sourceRows        map[string]int
	sourceDigests     map[string]string
	verifiedRows      map[string]int
	importedRows      map[string]int
	measurement       loadquality.ArchiveCardinalityMeasurement
	outcome           loadquality.ArchiveCardinalityOutcome
	vectorProfileSeen bool
	vectorsSeen       bool
}

type memoryArchiveLoadSnapshot struct {
	asOf               time.Time
	changeSeq          int64
	deletedMemoryCount int64
}

type memoryArchiveLoadRecallSpec struct {
	name    string
	mode    string
	options MemoryRecallOptions
}

type memoryArchiveLoadRecallHit struct {
	memory Memory
	id     string
	score  MemoryRecallScore
}

type memoryArchiveLoadRecallMetadata struct {
	nextCursor         string
	retrievalMode      string
	vectorProfileID    string
	vectorCoverage     float64
	vectorCandidates   int
	vectorMatches      int
	candidateTruncated bool
	candidateLimit     int
	degraded           bool
	degradedReason     string
}

type memoryArchiveLoadRecallResult struct {
	hits     []memoryArchiveLoadRecallHit
	metadata memoryArchiveLoadRecallMetadata
}

func TestMemoryArchiveLoadResultRegistryMatchesCanonicalArchiveTables(t *testing.T) {
	canonical := canonicalArchiveTableNamesForSchema(SchemaVersion())
	sort.Strings(canonical)
	if !reflect.DeepEqual(canonical, loadquality.ArchivePortableTableNames()) {
		t.Fatal("archive load result registry differs from the canonical portable table registry")
	}
}

// TestNarrativeMemoryArchiveLoadPostgres is the fourth opt-in executable
// production-readiness slice for narrative memory. It exercises complete
// self archives at several high-cardinality memory/version/evidence/relation/
// transcript shapes, removes each frozen account from the same store, imports
// it, and proves the generated lexical projection plus portable client vectors
// through exact pinned-snapshot recall equivalence.
//
// Every fixture and vector is deterministic from a signed 64-bit seed. The
// harness invokes no AI, model, embedding provider, runtime client, MCP,
// credential, secret, or sealed-plane surface.
func TestNarrativeMemoryArchiveLoadPostgres(t *testing.T) {
	if os.Getenv(memoryArchiveLoadEnabled) != "1" {
		t.Skip(memoryArchiveLoadEnabled + "=1 is required")
	}
	dsn := strings.TrimSpace(os.Getenv("WITSELF_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Fatal("WITSELF_TEST_DATABASE_URL is required when memory archive load testing is enabled")
	}
	opts, err := loadquality.ParseArchiveOptions(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), memoryArchiveLoadOverall)
	defer cancel()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	var postgresVersion string
	if err := st.pool.QueryRow(ctx, `SHOW server_version`).Scan(&postgresVersion); err != nil {
		t.Fatalf("read PostgreSQL version: %v", err)
	}

	neighbor, _, err := provisionMemoryArchiveLoadPrincipals(ctx, st, opts.Seed, "neighbor", false)
	if err != nil {
		t.Fatal(err)
	}
	workload := loadquality.ArchiveWorkload{
		Seed: opts.Seed, SyntheticAccounts: len(opts.Cardinalities) + 1,
		SyntheticAgents:   2*len(opts.Cardinalities) + 1,
		Cardinalities:     append([]int(nil), opts.Cardinalities...),
		VersionsPerMemory: opts.VersionsPerMemory, EvidencePerMemory: opts.EvidencePerMemory,
		RelationsPerMemory: opts.RelationsPerMemory, TagsPerVersion: opts.TagsPerVersion,
		TranscriptSharePercent:             opts.TranscriptSharePercent,
		TranscriptEntriesPerSelectedMemory: opts.TranscriptEntriesPerSelectedMemory,
		VectorDimensions:                   opts.VectorDimensions,
		LexicalQueries:                     loadquality.ArchiveLexicalQueryCount,
		HybridQueries:                      loadquality.ArchiveHybridQueryCount,
		SafetyCalls:                        loadquality.ArchiveSafetyCallCount,
	}

	fixtures := make([]*memoryArchiveLoadFixture, len(opts.Cardinalities))
	runWorkloadWithDeadline := func(name string, deadline time.Duration, fn func(context.Context) error) {
		t.Helper()
		workloadCtx, cancelWorkload := context.WithTimeout(ctx, deadline)
		defer cancelWorkload()
		if err := fn(workloadCtx); err != nil {
			t.Fatalf("%s workload: %v", name, err)
		}
	}
	// The ladder's cost is proportional to the number of cardinality rungs
	// (seeding and importing the largest tenant dominates), so one absolute
	// per-workload deadline over the whole ladder flakes on a loaded machine
	// (observed: first live run tripped the 2-minute bound at 123.9s, the
	// rerun passed at 122.5s). Budget one full deadline per rung instead;
	// the overall driver context still bounds the test.
	ladderDeadline := time.Duration(len(opts.Cardinalities)) * loadquality.ArchiveWorkloadDeadline
	runWorkloadWithDeadline("cardinality ladder", ladderDeadline, func(workloadCtx context.Context) error {
		for caseIndex, cardinality := range opts.Cardinalities {
			seedStarted := time.Now()
			fixture, seedErr := seedMemoryArchiveLoadFixture(
				workloadCtx, st, opts, workload, caseIndex, cardinality,
			)
			seedDuration := time.Since(seedStarted)
			if seedErr != nil {
				return fmt.Errorf("seed cardinality %d: %w", cardinality, seedErr)
			}
			fixture.measurement.MemoryCount = cardinality
			fixture.measurement.Seed, seedErr = memoryArchiveLoadSingleStats(seedDuration)
			if seedErr != nil {
				return seedErr
			}

			before, lexicalStats, hybridStats, recallErr := runMemoryArchiveLoadRecalls(
				workloadCtx, st, fixture.principal, fixture.recallSpecs,
			)
			if recallErr != nil {
				return fmt.Errorf("baseline recall cardinality %d: %w", cardinality, recallErr)
			}
			fixture.beforeRecall = before
			if len(before[0].hits) != 1 ||
				!memoryArchiveLoadBroadRedacted(before[0].hits[0].memory, fixture.sensitive) {
				return fmt.Errorf("baseline cardinality %d sensitive recall was not fully redacted", cardinality)
			}
			fixture.sensitiveBroadPre = true
			fixture.measurement.LexicalRecallBefore = lexicalStats
			fixture.measurement.HybridRecallBefore = hybridStats

			if err := st.SuspendAccountSystem(
				workloadCtx, fixture.principal.AccountID, "evacuation",
				"memory archive load fixture",
			); err != nil {
				return fmt.Errorf("suspend cardinality %d account: %w", cardinality, err)
			}
			fixture.sourceRows, err = memoryArchiveLoadPortableCounts(
				workloadCtx, st, fixture.principal.AccountID,
			)
			if err != nil {
				return fmt.Errorf("count source cardinality %d rows: %w", cardinality, err)
			}
			fixture.sourceDigests, err = memoryArchiveLoadContentDigests(
				workloadCtx, st, fixture.principal.AccountID,
			)
			if err != nil {
				return fmt.Errorf("digest source cardinality %d rows: %w", cardinality, err)
			}

			var archive bytes.Buffer
			exportStarted := time.Now()
			err = st.ExportAccountSelf(
				workloadCtx, fixture.principal.AccountID,
				"archive-load-source", "test", &archive,
			)
			exportDuration := time.Since(exportStarted)
			if err != nil {
				return fmt.Errorf("self export cardinality %d: %w", cardinality, err)
			}
			fixture.measurement.Export, err = memoryArchiveLoadSingleStats(exportDuration)
			if err != nil {
				return err
			}
			fixture.archive = append([]byte(nil), archive.Bytes()...)
			fixtures[caseIndex] = fixture
		}
		return nil
	})

	runWorkloadWithDeadline("archive verification", ladderDeadline, func(workloadCtx context.Context) error {
		for _, fixture := range fixtures {
			verificationStarted := time.Now()
			if err := verifyMemoryArchiveLoadArchive(workloadCtx, fixture, workload); err != nil {
				return fmt.Errorf("verify cardinality %d archive: %w", fixture.cardinality, err)
			}
			verificationDuration := time.Since(verificationStarted)
			fixture.measurement.Verify, err = memoryArchiveLoadSingleStats(verificationDuration)
			if err != nil {
				return err
			}
			fixture.measurement.ExportRowsPerSecond = loadquality.ArchiveThroughput(
				int64(fixture.outcome.ExportedRows), fixture.measurement.Export.WallDurationMS,
			)
			fixture.measurement.ExportBytesPerSecond = loadquality.ArchiveThroughput(
				fixture.outcome.ArchiveBytes, fixture.measurement.Export.WallDurationMS,
			)
		}
		return nil
	})

	runWorkloadWithDeadline("same-store import", ladderDeadline, func(workloadCtx context.Context) error {
		for _, fixture := range fixtures {
			removed, removeErr := removeMemoryArchiveLoadAccount(
				workloadCtx, st, fixture.principal.AccountID, fixture.sourceRows,
			)
			if removeErr != nil {
				return fmt.Errorf("remove cardinality %d account: %w", fixture.cardinality, removeErr)
			}
			fixture.outcome.AccountRemovedBeforeImport = removed
			if !removed {
				return fmt.Errorf("cardinality %d account still exists before import", fixture.cardinality)
			}

			importStarted := time.Now()
			manifest, importErr := st.ImportAccount(
				workloadCtx, fixture.principal.AccountID, bytes.NewReader(fixture.archive),
			)
			importDuration := time.Since(importStarted)
			if importErr != nil {
				return fmt.Errorf("import cardinality %d account: %w", fixture.cardinality, importErr)
			}
			fixture.measurement.Import, err = memoryArchiveLoadSingleStats(importDuration)
			if err != nil {
				return err
			}
			if manifest.FormatVersion != fixture.outcome.ManifestFormatVersion ||
				manifest.SchemaVersion != fixture.outcome.ManifestSchemaVersion ||
				manifest.Purpose != fixture.outcome.ManifestPurpose ||
				manifest.Status != fixture.outcome.ManifestStatus {
				return fmt.Errorf("cardinality %d import returned different manifest coordinates", fixture.cardinality)
			}

			fixture.importedRows, err = memoryArchiveLoadPortableCounts(
				workloadCtx, st, fixture.principal.AccountID,
			)
			if err != nil {
				return fmt.Errorf("count imported cardinality %d rows: %w", fixture.cardinality, err)
			}
			if err := finishMemoryArchiveLoadRowComparison(fixture); err != nil {
				return err
			}
			importedDigests, digestErr := memoryArchiveLoadContentDigests(
				workloadCtx, st, fixture.principal.AccountID,
			)
			if digestErr != nil {
				return fmt.Errorf("digest imported cardinality %d rows: %w", fixture.cardinality, digestErr)
			}
			for table, digest := range fixture.sourceDigests {
				if importedDigests[table] != digest {
					return fmt.Errorf(
						"cardinality %d table %s content digest changed across the round trip",
						fixture.cardinality, table,
					)
				}
			}
			if err := verifyMemoryArchiveLoadImportedVectors(
				workloadCtx, st, fixture,
			); err != nil {
				return err
			}
			// The imported row must arrive in its exported lifecycle state;
			// resume's idempotent no-op on an already-active row would
			// otherwise mask an import that dropped the suspension.
			status, suspendedFor, statusErr := memoryArchiveLoadAccountStatus(
				workloadCtx, st, fixture.principal.AccountID,
			)
			if statusErr != nil {
				return statusErr
			}
			if status != "suspended" || suspendedFor != "evacuation" {
				return fmt.Errorf(
					"cardinality %d imported account status/suspended_for = %q/%q, want suspended/evacuation",
					fixture.cardinality, status, suspendedFor,
				)
			}
			if err := st.ResumeAccountSystem(
				workloadCtx, fixture.principal.AccountID, "evacuation",
			); err != nil {
				return fmt.Errorf("resume cardinality %d account: %w", fixture.cardinality, err)
			}
			status, _, statusErr = memoryArchiveLoadAccountStatus(
				workloadCtx, st, fixture.principal.AccountID,
			)
			if statusErr != nil {
				return statusErr
			}
			if status != "active" {
				return fmt.Errorf(
					"cardinality %d resumed account status = %q, want active",
					fixture.cardinality, status,
				)
			}
			fixture.outcome.SameStoreRoundTrip = true
			fixture.measurement.ImportRowsPerSecond = loadquality.ArchiveThroughput(
				int64(fixture.outcome.ImportedRows), fixture.measurement.Import.WallDurationMS,
			)
			fixture.measurement.ImportBytesPerSecond = loadquality.ArchiveThroughput(
				fixture.outcome.ArchiveBytes, fixture.measurement.Import.WallDurationMS,
			)
		}
		return nil
	})

	runWorkloadWithDeadline("recall equivalence", ladderDeadline, func(workloadCtx context.Context) error {
		for _, fixture := range fixtures {
			after, lexicalStats, hybridStats, recallErr := runMemoryArchiveLoadRecalls(
				workloadCtx, st, fixture.principal, fixture.recallSpecs,
			)
			if recallErr != nil {
				return fmt.Errorf("post-import recall cardinality %d: %w", fixture.cardinality, recallErr)
			}
			fixture.measurement.LexicalRecallAfter = lexicalStats
			fixture.measurement.HybridRecallAfter = hybridStats
			outcome, compareErr := compareMemoryArchiveLoadRecalls(
				fixture.recallSpecs, fixture.beforeRecall, after,
			)
			if compareErr != nil {
				return fmt.Errorf("cardinality %d recall equivalence: %w", fixture.cardinality, compareErr)
			}
			fixture.outcome.RecallEquivalence = outcome
		}
		return nil
	})

	runWorkloadWithDeadline("sensitive and isolation preservation", ladderDeadline, func(workloadCtx context.Context) error {
		for _, fixture := range fixtures {
			stats, outcome, safetyErr := runMemoryArchiveLoadSafety(
				workloadCtx, st, fixture, neighbor,
			)
			if safetyErr != nil {
				return fmt.Errorf("cardinality %d safety: %w", fixture.cardinality, safetyErr)
			}
			fixture.measurement.PostImportSafety = stats
			fixture.outcome.Safety = outcome
		}
		return nil
	})

	measurements := make([]loadquality.ArchiveCardinalityMeasurement, 0, len(fixtures))
	outcomes := make([]loadquality.ArchiveCardinalityOutcome, 0, len(fixtures))
	profilesRoundTripped := true
	vectorsRoundTripped := true
	for _, fixture := range fixtures {
		measurements = append(measurements, fixture.measurement)
		outcomes = append(outcomes, fixture.outcome)
		profilesRoundTripped = profilesRoundTripped && fixture.vectorProfileSeen
		vectorsRoundTripped = vectorsRoundTripped && fixture.vectorsSeen
	}
	result := loadquality.ArchiveResult{
		Schema:            loadquality.ArchiveResultSchemaV1,
		HarnessVersion:    loadquality.ArchiveHarnessVersion,
		StartedAt:         startedAt,
		CompletedAt:       time.Now().UTC(),
		Outcome:           "pass",
		PostgreSQLVersion: strings.TrimSpace(postgresVersion),
		Environment:       loadquality.ArchiveEnvironment(opts),
		Workload:          workload,
		Measurements: loadquality.ArchiveMeasurements{
			CardinalityLadder: measurements,
		},
		Outcomes: loadquality.ArchiveOutcomes{
			CardinalityLadder: outcomes,
			VectorPortability: loadquality.ArchiveVectorPortabilityOutcome{
				VectorProfilesPortable:     true,
				MemoryVectorsPortable:      true,
				HybridEquivalenceExercised: true,
				VectorPortabilityLimited:   false,
				VectorProfilesRoundTripped: profilesRoundTripped,
				MemoryVectorsRoundTripped:  vectorsRoundTripped,
			},
			AllCardinalitiesComplete: true,
		},
	}
	raw, err := loadquality.WriteArchiveResult(opts.ResultsPath, result)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sanitized memory-archive load result written to %s", opts.ResultsPath)
	t.Logf("sanitized memory-archive load result:\n%s", raw)
}

func seedMemoryArchiveLoadFixture(
	ctx context.Context,
	st *Store,
	opts loadquality.ArchiveOptions,
	workload loadquality.ArchiveWorkload,
	caseIndex int,
	cardinality int,
) (*memoryArchiveLoadFixture, error) {
	principal, peer, err := provisionMemoryArchiveLoadPrincipals(
		ctx, st, opts.Seed, fmt.Sprintf("case-%d", caseIndex), true,
	)
	if err != nil {
		return nil, err
	}
	selected, err := loadquality.ArchiveTranscriptSelectedCount(
		cardinality, opts.TranscriptSharePercent,
	)
	if err != nil {
		return nil, err
	}
	transcript, err := st.CreateTranscript(
		ctx, principal.AccountID, principal.RealmID, principal.ID,
		CreateTranscriptInput{ExternalID: fmt.Sprintf(
			"archive-load-%s", memoryArchiveLoadToken(opts.Seed, "transcript", fmt.Sprint(caseIndex)),
		)},
	)
	if err != nil {
		return nil, fmt.Errorf("create transcript fixture: %w", err)
	}
	type transcriptRange struct{ first, last int64 }
	ranges := make([]transcriptRange, selected)
	for selectedIndex := 0; selectedIndex < selected; selectedIndex++ {
		for entryIndex := 0; entryIndex < opts.TranscriptEntriesPerSelectedMemory; entryIndex++ {
			entry, appendErr := st.AppendTranscriptEntry(
				ctx, principal.AccountID, principal.RealmID, principal.ID, transcript.ID,
				AppendTranscriptEntryInput{
					ExternalID: fmt.Sprintf("archive-load-%06d-%02d", selectedIndex, entryIndex),
					Role:       TranscriptRoleUser,
					Body: fmt.Sprintf(
						"Synthetic archive transcript %s selection %d entry %d.",
						memoryArchiveLoadToken(opts.Seed, "entry", fmt.Sprint(caseIndex), fmt.Sprint(selectedIndex), fmt.Sprint(entryIndex)),
						selectedIndex, entryIndex,
					),
				},
			)
			if appendErr != nil {
				return nil, fmt.Errorf("append transcript fixture: %w", appendErr)
			}
			if entryIndex == 0 {
				ranges[selectedIndex].first = entry.Sequence
			}
			ranges[selectedIndex].last = entry.Sequence
		}
	}

	fixture := &memoryArchiveLoadFixture{
		cardinality: cardinality, principal: principal, peer: peer,
		memories: make([]Memory, 0, cardinality),
	}
	groups := [...]string{
		"archivealpha beacon", "archivebeta compass",
		"archivegamma lantern", "archivedelta harbor",
	}
	for memoryIndex := 0; memoryIndex < cardinality; memoryIndex++ {
		tags := make([]string, opts.TagsPerVersion)
		for tagIndex := range tags {
			tags[tagIndex] = fmt.Sprintf("archive_load_%02d", tagIndex)
		}
		evidence := make([]MemoryEvidenceInput, opts.EvidencePerMemory)
		for evidenceIndex := range evidence {
			evidence[evidenceIndex] = MemoryEvidenceInput{
				Type:               "system",
				ResolutionState:    MemoryEvidenceUnavailable,
				TerminalReasonCode: "synthetic_fixture",
			}
		}
		if memoryIndex < selected {
			evidence[0] = MemoryEvidenceInput{
				Type: "conversation", ResolutionState: MemoryEvidenceResolved,
				ResolvedKind:        MemoryCurationSourceTranscript,
				SourceTranscriptID:  transcript.ID,
				SourceSequenceFrom:  ranges[memoryIndex].first,
				SourceSequenceUntil: ranges[memoryIndex].last,
			}
		}
		contentStem := fmt.Sprintf(
			"Synthetic %s archive memory %s ordinal %d",
			groups[memoryIndex%len(groups)],
			memoryArchiveLoadToken(opts.Seed, "memory", fmt.Sprint(caseIndex), fmt.Sprint(memoryIndex)),
			memoryIndex,
		)
		sensitive := memoryIndex == 0
		if sensitive {
			fixture.sensitiveQuery = "vaultmarker" + memoryArchiveLoadToken(opts.Seed, "sensitive", fmt.Sprint(caseIndex))
			contentStem += " " + fixture.sensitiveQuery
		}
		links := []string{}
		if sensitive {
			links = []string{fmt.Sprintf(
				"https://example.invalid/archive/%s",
				memoryArchiveLoadToken(opts.Seed, "link", fmt.Sprint(caseIndex)),
			)}
		}
		occurredFrom := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC).
			Add(time.Duration(opts.Seed%86_400) * time.Second).
			Add(time.Duration(caseIndex*loadquality.MaximumArchiveCardinality+memoryIndex) * time.Minute)
		occurredUntil := occurredFrom.Add(30 * time.Second)
		salience := 0.2 + 0.7*float64(memoryIndex%97)/96
		captured, captureErr := st.CaptureMemory(ctx, principal, CaptureMemoryInput{
			Content: contentStem + " revision 1.", Kind: "observation",
			Tags: tags, Links: links, Salience: &salience, Sensitive: sensitive,
			OccurredFrom: &occurredFrom, OccurredUntil: &occurredUntil,
			CaptureReason: "load_test", Evidence: evidence,
			Client: MemoryClientProvenance{
				Runtime: "archive-load", Recipe: loadquality.ArchiveResultSchemaV1,
				RecipeVersion: loadquality.ArchiveHarnessVersion,
			},
			IdempotencyKey: fmt.Sprintf("archive-load-capture-%d-%d-%d", opts.Seed, caseIndex, memoryIndex),
		})
		if captureErr != nil {
			return nil, fmt.Errorf("capture memory fixture %d: %w", memoryIndex, captureErr)
		}
		current := captured.Memory
		for version := 2; version <= opts.VersionsPerMemory; version++ {
			content := fmt.Sprintf("%s revision %d.", contentStem, version)
			adjusted, adjustErr := st.AdjustMemory(ctx, principal, current.ID, AdjustMemoryInput{
				ExpectedVersion: current.Version, Content: &content,
				Reason: "archive load version fixture",
				Client: MemoryClientProvenance{
					Runtime: "archive-load", Recipe: loadquality.ArchiveResultSchemaV1,
					RecipeVersion: loadquality.ArchiveHarnessVersion,
				},
				IdempotencyKey: fmt.Sprintf(
					"archive-load-adjust-%d-%d-%d-%d", opts.Seed, caseIndex, memoryIndex, version,
				),
			})
			if adjustErr != nil {
				return nil, fmt.Errorf("adjust memory fixture %d version %d: %w", memoryIndex, version, adjustErr)
			}
			current = adjusted.Memory
		}
		fixture.memories = append(fixture.memories, current)
		if sensitive {
			fixture.sensitive = current
		}
	}
	if err := seedMemoryArchiveLoadRelations(
		ctx, st, principal, opts, caseIndex, fixture.memories,
	); err != nil {
		return nil, err
	}
	profile, err := st.CreateMemoryVectorProfile(ctx, principal, CreateMemoryVectorProfileInput{
		Provider: "synthetic", Model: "sha256-expanded-unit",
		Recipe: "archive-load", RecipeVersion: fmt.Sprintf("case-%d", caseIndex),
		Dimensions: opts.VectorDimensions, DistanceMetric: MemoryVectorMetricCosine,
		Normalization: MemoryVectorNormalizationL2,
	})
	if err != nil {
		return nil, fmt.Errorf("create vector profile: %w", err)
	}
	fixture.profile = profile
	fixture.vectorReceipts = make([]MemoryVectorReceipt, 0, cardinality)
	for memoryIndex, memory := range fixture.memories {
		vector, vectorErr := loadquality.DeterministicVector(
			opts.Seed, int64(caseIndex+1)*1_000_000+int64(memoryIndex), opts.VectorDimensions,
		)
		if vectorErr != nil {
			return nil, vectorErr
		}
		receipt, putErr := st.PutMemoryVector(ctx, principal, PutMemoryVectorInput{
			ProfileID: profile.ID, MemoryID: memory.ID, MemoryVersion: memory.Version,
			ContentHash: memory.ContentHash, Vector: vector,
		})
		if putErr != nil {
			return nil, fmt.Errorf("attach memory vector %d: %w", memoryIndex, putErr)
		}
		fixture.vectorReceipts = append(fixture.vectorReceipts, receipt)
	}
	fixture.snapshot, err = memoryArchiveLoadSnapshotCoordinates(ctx, st, principal)
	if err != nil {
		return nil, err
	}
	fixture.recallSpecs, err = memoryArchiveLoadRecallSpecs(opts, caseIndex, fixture)
	if err != nil {
		return nil, err
	}

	counts, err := memoryArchiveLoadPortableCounts(ctx, st, principal.AccountID)
	if err != nil {
		return nil, err
	}
	tagAssignments, err := memoryArchiveLoadTagAssignments(ctx, st, principal.AccountID)
	if err != nil {
		return nil, err
	}
	focal := memoryArchiveLoadFocalCounts(counts, tagAssignments)
	wantFocal, err := loadquality.ArchiveFocalCountsFor(cardinality, workload)
	if err != nil {
		return nil, err
	}
	if focal != wantFocal {
		return nil, fmt.Errorf("seeded focal counts %+v, want %+v", focal, wantFocal)
	}
	fixture.outcome.MemoryCount = cardinality
	fixture.outcome.FocalCounts = focal
	fixture.outcome.ExactFixtureCounts = true
	return fixture, nil
}

func provisionMemoryArchiveLoadPrincipals(
	ctx context.Context,
	st *Store,
	seed int64,
	label string,
	withPeer bool,
) (Principal, Principal, error) {
	token := memoryArchiveLoadToken(seed, "principal", label)
	provisioned, err := st.ProvisionAccount(
		ctx, fmt.Sprintf("memory-archive-load-%s@example.invalid", token),
		"memory archive load "+token, time.Hour,
	)
	if err != nil {
		return Principal{}, Principal{}, fmt.Errorf("provision archive account: %w", err)
	}
	activated, err := st.ActivateAccount(ctx, provisioned.AccountID)
	if err != nil {
		return Principal{}, Principal{}, fmt.Errorf("activate archive account: %w", err)
	}
	if !activated {
		return Principal{}, Principal{}, errors.New("activate archive account returned activated=false")
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "archive-load")
	if err != nil {
		return Principal{}, Principal{}, fmt.Errorf("create archive realm: %w", err)
	}
	owner, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "archive-owner-"+token)
	if err != nil {
		return Principal{}, Principal{}, fmt.Errorf("create archive owner: %w", err)
	}
	principal := Principal{
		Kind: PrincipalAgent, ID: owner.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: owner.Name, RealmName: realm.Name,
		AccountStatus: "active",
	}
	if !withPeer {
		return principal, Principal{}, nil
	}
	peerAgent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "archive-peer-"+token)
	if err != nil {
		return Principal{}, Principal{}, fmt.Errorf("create archive peer: %w", err)
	}
	peer := principal
	peer.ID = peerAgent.ID
	peer.AgentName = peerAgent.Name
	return principal, peer, nil
}

func seedMemoryArchiveLoadRelations(
	ctx context.Context,
	st *Store,
	p Principal,
	opts loadquality.ArchiveOptions,
	caseIndex int,
	memories []Memory,
) error {
	if len(memories) < opts.RelationsPerMemory+1 {
		return errors.New("archive relation fixture has too few memories")
	}
	maximumGroup := MaxMemoryCurationPlanActions / opts.RelationsPerMemory
	groupCap := memoryArchiveLoadRelationGroupCap(
		len(memories), opts.RelationsPerMemory, maximumGroup,
	)
	scope := MemoryCurationScope{
		Sources:          []string{MemoryCurationSourceMemory},
		MemoryStates:     []string{MemoryStateActive},
		IncludeSensitive: true,
		MaxMemories:      groupCap,
	}
	requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope: scope, CoalescingKey: fmt.Sprintf("archive_relations_%d", caseIndex),
		TriggerReason: "load_test", MaxAttempts: 3,
		IdempotencyKey: fmt.Sprintf("archive-load-relations-request-%d-%d", opts.Seed, caseIndex),
	})
	if err != nil {
		return fmt.Errorf("request archive relation curation: %w", err)
	}
	request := requested.Request
	processedMemories := 0
	createdRelations := 0
	for batch := 0; ; batch++ {
		started, startErr := st.StartCuration(ctx, p, StartMemoryCurationInput{
			RequestID:     request.ID,
			Caps:          MemoryCurationInputCaps{MaxMemories: groupCap},
			LeaseDuration: minMemoryCurationLease,
			Client: MemoryClientProvenance{
				Runtime: "archive-load", Recipe: loadquality.ArchiveResultSchemaV1,
				RecipeVersion: loadquality.ArchiveHarnessVersion,
			},
			IdempotencyKey: fmt.Sprintf("archive-load-relations-start-%d-%d-%d", opts.Seed, caseIndex, batch),
		})
		if startErr != nil {
			return fmt.Errorf("start archive relation curation batch %d: %w", batch, startErr)
		}
		page, pageErr := st.GetCurationRunInputs(
			ctx, p, started.Run.ID, started.Run.FencingGeneration,
			started.FirstInputCursor, maxMemoryCurationPageSize,
		)
		if pageErr != nil {
			return fmt.Errorf("read archive relation inputs batch %d: %w", batch, pageErr)
		}
		if page.NextCursor != "" {
			return errors.New("archive relation input batch unexpectedly required a second page")
		}
		refs := make([]MemoryCurationVersionReference, 0, groupCap)
		for _, input := range page.Inputs {
			if input.Kind == MemoryCurationSourceMemory {
				refs = append(refs, MemoryCurationVersionReference{
					MemoryID: input.MemoryID, Version: input.MemoryVersion,
				})
			}
		}
		if len(refs) <= opts.RelationsPerMemory {
			return fmt.Errorf("archive relation batch has %d memory inputs", len(refs))
		}
		actions := make([]MemoryCurationPlanAction, 0, len(refs)*opts.RelationsPerMemory)
		for fromIndex, from := range refs {
			for relationIndex := 0; relationIndex < opts.RelationsPerMemory; relationIndex++ {
				relationType := MemoryCurationRelationDerivedFrom
				if relationIndex%2 == 1 {
					relationType = MemoryCurationRelationSummarizes
				}
				actions = append(actions, MemoryCurationPlanAction{
					Ordinal: int64(len(actions) + 1), Operation: MemoryCurationOperationRelate,
					Relate: &MemoryCurationRelateAction{
						RelationType: relationType, From: from,
						To: refs[(fromIndex+relationIndex+1)%len(refs)],
					},
				})
			}
		}
		if len(actions) > MaxMemoryCurationPlanActions {
			return errors.New("archive relation batch exceeded the plan action bound")
		}
		draft, marshalErr := json.Marshal(MemoryCurationPlanDraft{
			Schema: MemoryCurationPlanSchemaV1, DraftRevision: 1, Actions: actions,
		})
		if marshalErr != nil {
			return fmt.Errorf("marshal archive relation plan: %w", marshalErr)
		}
		if _, planErr := st.PlanCuration(ctx, p, started.Run.ID, PlanMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration, Draft: draft,
			IdempotencyKey: fmt.Sprintf("archive-load-relations-plan-%d-%d-%d", opts.Seed, caseIndex, batch),
		}); planErr != nil {
			return fmt.Errorf("plan archive relations batch %d: %w", batch, planErr)
		}
		stored, getErr := st.GetCurationPlan(ctx, p, started.Run.ID, started.Run.FencingGeneration)
		if getErr != nil {
			return fmt.Errorf("get archive relation plan batch %d: %w", batch, getErr)
		}
		applied, applyErr := st.ApplyCuration(ctx, p, started.Run.ID, ApplyMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration,
			PlanRevision:      stored.Run.PlanRevision, PlanHash: stored.Run.PlanHash,
			IdempotencyKey: fmt.Sprintf("archive-load-relations-apply-%d-%d-%d", opts.Seed, caseIndex, batch),
		})
		if applyErr != nil {
			return fmt.Errorf("apply archive relations batch %d: %w", batch, applyErr)
		}
		if len(applied.Receipt.ActionResults) != len(actions) {
			return errors.New("archive relation apply returned a partial action result set")
		}
		for _, result := range applied.Receipt.ActionResults {
			if len(result.RelationIDs) != 1 {
				return errors.New("archive relation action did not create exactly one relation")
			}
		}
		processedMemories += len(refs)
		createdRelations += len(actions)
		if applied.FollowUpRequest == nil {
			break
		}
		request = *applied.FollowUpRequest
	}
	if processedMemories != len(memories) ||
		createdRelations != len(memories)*opts.RelationsPerMemory {
		return fmt.Errorf(
			"archive relation fixture processed memories/relations %d/%d, want %d/%d",
			processedMemories, createdRelations, len(memories),
			len(memories)*opts.RelationsPerMemory,
		)
	}
	return nil
}

func memoryArchiveLoadRelationGroupCap(memoryCount, relationsPerMemory, maximum int) int {
	if memoryCount <= maximum {
		return memoryCount
	}
	for candidate := maximum; candidate > relationsPerMemory; candidate-- {
		remainder := memoryCount % candidate
		if remainder == 0 || remainder > relationsPerMemory {
			return candidate
		}
	}
	return relationsPerMemory + 1
}

func memoryArchiveLoadSnapshotCoordinates(
	ctx context.Context,
	st *Store,
	p Principal,
) (memoryArchiveLoadSnapshot, error) {
	var out memoryArchiveLoadSnapshot
	err := st.pool.QueryRow(ctx, `
		SELECT clock_timestamp(), COALESCE((
		  SELECT last_change_seq FROM memory_change_clocks
		  WHERE account_id=$1 AND realm_id=$2
		    AND owner_kind='agent' AND owner_id=$3
		),0), (SELECT count(*) FROM memories
		  WHERE account_id=$1 AND realm_id=$2
		    AND owner_kind='agent' AND owner_id=$3
		    AND current_version IS NULL)`, p.AccountID, p.RealmID, p.ID).Scan(
		&out.asOf, &out.changeSeq, &out.deletedMemoryCount,
	)
	if err != nil {
		return memoryArchiveLoadSnapshot{}, fmt.Errorf("read archive recall snapshot: %w", err)
	}
	out.asOf = out.asOf.UTC()
	return out, nil
}

func memoryArchiveLoadRecallSpecs(
	opts loadquality.ArchiveOptions,
	caseIndex int,
	fixture *memoryArchiveLoadFixture,
) ([]memoryArchiveLoadRecallSpec, error) {
	primaryVector, err := loadquality.DeterministicVector(
		opts.Seed, int64(caseIndex+1)*1_000_000, opts.VectorDimensions,
	)
	if err != nil {
		return nil, err
	}
	secondaryVector, err := loadquality.DeterministicVector(
		opts.Seed, int64(caseIndex+1)*2_000_000, opts.VectorDimensions,
	)
	if err != nil {
		return nil, err
	}
	base := func(query string) MemoryRecallOptions {
		asOf := fixture.snapshot.asOf
		changeSeq := fixture.snapshot.changeSeq
		deletedCount := fixture.snapshot.deletedMemoryCount
		return MemoryRecallOptions{
			Query: query, Limit: memoryArchiveLoadRecallLimit,
			AsOf: &asOf, SnapshotChangeSeq: &changeSeq,
			SnapshotDeletedMemoryCount: &deletedCount,
		}
	}
	lexicalPrimary := base(fixture.sensitiveQuery)
	lexicalSecondary := base("archivebeta compass")
	hybridPrimary := base("archivegamma lantern")
	hybridPrimary.VectorProfileID = fixture.profile.ID
	hybridPrimary.QueryVector = primaryVector
	hybridSecondary := base("")
	hybridSecondary.VectorProfileID = fixture.profile.ID
	hybridSecondary.QueryVector = secondaryVector
	return []memoryArchiveLoadRecallSpec{
		{name: loadquality.ArchiveRecallCaseLexicalPrimary, mode: "lexical", options: lexicalPrimary},
		{name: loadquality.ArchiveRecallCaseLexicalSecondary, mode: "lexical", options: lexicalSecondary},
		{name: loadquality.ArchiveRecallCaseHybridPrimary, mode: "hybrid", options: hybridPrimary},
		{name: loadquality.ArchiveRecallCaseHybridSecondary, mode: "hybrid", options: hybridSecondary},
	}, nil
}

func runMemoryArchiveLoadRecalls(
	ctx context.Context,
	st *Store,
	p Principal,
	specs []memoryArchiveLoadRecallSpec,
) ([]memoryArchiveLoadRecallResult, loadquality.OperationStats, loadquality.OperationStats, error) {
	results := make([]memoryArchiveLoadRecallResult, 0, len(specs))
	lexicalDurations := make([]time.Duration, 0, loadquality.ArchiveLexicalQueryCount)
	hybridDurations := make([]time.Duration, 0, loadquality.ArchiveHybridQueryCount)
	var lexicalWall, hybridWall time.Duration
	for _, spec := range specs {
		operationStarted := time.Now()
		page, err := st.RecallMemories(ctx, p, spec.options)
		duration := time.Since(operationStarted)
		if spec.mode == "lexical" {
			lexicalDurations = append(lexicalDurations, duration)
			lexicalWall += duration
		} else {
			hybridDurations = append(hybridDurations, duration)
			hybridWall += duration
		}
		if err != nil {
			return nil, loadquality.OperationStats{}, loadquality.OperationStats{}, err
		}
		if page.RetrievalMode != spec.mode || len(page.Hits) == 0 {
			return nil, loadquality.OperationStats{}, loadquality.OperationStats{}, fmt.Errorf(
				"recall case %s returned mode %q with %d hits", spec.name, page.RetrievalMode, len(page.Hits),
			)
		}
		if spec.mode == loadquality.ArchiveRecallModeHybrid &&
			(page.VectorProfileID != spec.options.VectorProfileID || page.VectorCoverage != 1 ||
				page.VectorCandidates < 1 || page.VectorMatches != page.VectorCandidates) {
			return nil, loadquality.OperationStats{}, loadquality.OperationStats{}, fmt.Errorf(
				"hybrid recall case %s did not use complete compatible vector coverage", spec.name,
			)
		}
		result := memoryArchiveLoadRecallResult{
			hits: make([]memoryArchiveLoadRecallHit, 0, len(page.Hits)),
			metadata: memoryArchiveLoadRecallMetadata{
				nextCursor: page.NextCursor, retrievalMode: page.RetrievalMode,
				vectorProfileID: page.VectorProfileID, vectorCoverage: page.VectorCoverage,
				vectorCandidates: page.VectorCandidates, vectorMatches: page.VectorMatches,
				candidateTruncated: page.CandidateTruncated, candidateLimit: page.CandidateLimit,
				degraded: page.Degraded, degradedReason: page.DegradedReason,
			},
		}
		for _, hit := range page.Hits {
			if spec.mode == loadquality.ArchiveRecallModeHybrid && !hit.Score.VectorUsed {
				return nil, loadquality.OperationStats{}, loadquality.OperationStats{}, fmt.Errorf(
					"hybrid recall case %s returned a hit without vector scoring", spec.name,
				)
			}
			result.hits = append(result.hits, memoryArchiveLoadRecallHit{
				memory: hit.Memory, id: hit.Memory.ID, score: hit.Score,
			})
		}
		results = append(results, result)
	}
	if len(lexicalDurations) != loadquality.ArchiveLexicalQueryCount ||
		len(hybridDurations) != loadquality.ArchiveHybridQueryCount {
		return nil, loadquality.OperationStats{}, loadquality.OperationStats{}, errors.New("archive recall set is incomplete")
	}
	lexicalStats, err := loadquality.Summarize(lexicalDurations, lexicalWall)
	if err != nil {
		return nil, loadquality.OperationStats{}, loadquality.OperationStats{}, err
	}
	hybridStats, err := loadquality.Summarize(hybridDurations, hybridWall)
	if err != nil {
		return nil, loadquality.OperationStats{}, loadquality.OperationStats{}, err
	}
	return results, lexicalStats, hybridStats, nil
}

func compareMemoryArchiveLoadRecalls(
	specs []memoryArchiveLoadRecallSpec,
	before []memoryArchiveLoadRecallResult,
	after []memoryArchiveLoadRecallResult,
) (loadquality.ArchiveRecallEquivalenceOutcome, error) {
	if len(specs) != len(before) || len(before) != len(after) {
		return loadquality.ArchiveRecallEquivalenceOutcome{}, errors.New("recall result set size changed")
	}
	outcome := loadquality.ArchiveRecallEquivalenceOutcome{
		Cases:       make([]loadquality.ArchiveRecallEquivalenceCase, 0, len(specs)),
		BeforeCalls: len(before), AfterCalls: len(after),
		ScoreComparison: "exact", ScoreTolerance: 0,
		AllRankingsIdentical: true, AllScoreComponentsExact: true,
		AllMetadataExact: true, RetrievalProjectionProved: true,
	}
	for index, spec := range specs {
		rankedIDsIdentical := len(before[index].hits) == len(after[index].hits)
		scoreComponentsExact := rankedIDsIdentical
		if rankedIDsIdentical {
			for hitIndex := range before[index].hits {
				if before[index].hits[hitIndex].id != after[index].hits[hitIndex].id {
					rankedIDsIdentical = false
				}
				if before[index].hits[hitIndex].score != after[index].hits[hitIndex].score {
					scoreComponentsExact = false
				}
			}
		}
		metadataExact := before[index].metadata == after[index].metadata
		caseOutcome := loadquality.ArchiveRecallEquivalenceCase{
			Name: spec.name, Mode: spec.mode,
			BeforeHits: len(before[index].hits), AfterHits: len(after[index].hits),
			RankedIDsIdentical:   rankedIDsIdentical,
			ScoreComponentsExact: scoreComponentsExact, MetadataExact: metadataExact,
		}
		if !rankedIDsIdentical || !scoreComponentsExact || !metadataExact {
			return loadquality.ArchiveRecallEquivalenceOutcome{}, fmt.Errorf("recall case %s changed across import", spec.name)
		}
		outcome.Cases = append(outcome.Cases, caseOutcome)
	}
	return outcome, nil
}

func verifyMemoryArchiveLoadArchive(
	ctx context.Context,
	fixture *memoryArchiveLoadFixture,
	workload loadquality.ArchiveWorkload,
) error {
	verifiedRows := make(map[string]int)
	manifest, err := archiveexport.Read(ctx, bytes.NewReader(fixture.archive), archiveexport.ImportOptions{
		CurrentSchema: SchemaVersion(),
		OnManifest: func(item archiveexport.Manifest) error {
			if item.FormatVersion != archiveexport.FormatVersion ||
				item.SchemaVersion != SchemaVersion() || item.Purpose != archiveexport.PurposeSelf ||
				item.Status != "suspended" || item.AccountID != fixture.principal.AccountID {
				return errors.New("archive manifest coordinates do not match the suspended self-export contract")
			}
			if err := validateArchiveManifestTables(item.SchemaVersion, item.Tables); err != nil {
				return err
			}
			if !reflect.DeepEqual(item.Tables, canonicalArchiveTableNamesForSchema(item.SchemaVersion)) {
				return errors.New("archive manifest table order differs from the canonical export registry")
			}
			for _, table := range item.Tables {
				verifiedRows[table] = 0
			}
			return nil
		},
		Row: func(table string, row []byte) error {
			verifiedRows[table]++
			if table != "memory_versions" {
				return nil
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(row, &object); err != nil {
				return err
			}
			if _, archived := object["search_document"]; archived {
				return errors.New("generated memory search projection was archived")
			}
			return nil
		},
	})
	if err != nil {
		if errors.Is(err, archiveexport.ErrCorrupt) {
			return errors.New("internal/export checksum verification rejected the archive")
		}
		return err
	}
	checksums, err := memoryArchiveLoadChecksums(fixture.archive)
	if err != nil {
		return err
	}
	for table, count := range verifiedRows {
		if checksums.TableRows[table] != count {
			return fmt.Errorf("verified table %s count differs from checksums", table)
		}
	}
	if !memoryArchiveLoadSameRowCounts(fixture.sourceRows, verifiedRows) {
		return errors.New("source and checksum-verified per-table row counts differ")
	}
	fixture.verifiedRows = verifiedRows
	names := loadquality.ArchivePortableTableNames()
	if len(verifiedRows) != len(names) {
		return errors.New("archive manifest table count differs from the result contract registry")
	}
	for _, table := range names {
		if _, present := verifiedRows[table]; !present {
			return fmt.Errorf("archive manifest is missing result-contract table %s", table)
		}
	}
	tableRows := make([]loadquality.ArchiveTableRows, 0, len(names))
	nonEmpty := 0
	exportedTotal := 0
	verifiedTotal := 0
	for _, table := range names {
		exported := fixture.sourceRows[table]
		verified := verifiedRows[table]
		if exported > 0 {
			nonEmpty++
		}
		exportedTotal += exported
		verifiedTotal += verified
		tableRows = append(tableRows, loadquality.ArchiveTableRows{
			Name: table, Exported: exported, Verified: verified,
		})
	}
	chunkBytes := int64(0)
	for _, chunk := range checksums.Chunks {
		chunkBytes += int64(chunk.Bytes)
	}
	wantFocal, err := loadquality.ArchiveFocalCountsFor(fixture.cardinality, workload)
	if err != nil {
		return err
	}
	if fixture.outcome.FocalCounts != wantFocal {
		return errors.New("verified archive focal counts differ from the declared workload")
	}
	fixture.outcome.ManifestFormatVersion = manifest.FormatVersion
	fixture.outcome.ManifestSchemaVersion = manifest.SchemaVersion
	fixture.outcome.ManifestPurpose = manifest.Purpose
	fixture.outcome.ManifestStatus = manifest.Status
	fixture.outcome.ArchiveBytes = int64(len(fixture.archive))
	fixture.outcome.ChunkBytes = chunkBytes
	fixture.outcome.ManifestTables = len(manifest.Tables)
	fixture.outcome.NonEmptyTables = nonEmpty
	fixture.outcome.ChunkCount = len(checksums.Chunks)
	fixture.outcome.ExportedRows = exportedTotal
	fixture.outcome.VerifiedRows = verifiedTotal
	fixture.outcome.TableRows = tableRows
	fixture.outcome.ChecksumsRead = true
	fixture.outcome.AllChunksVerified = true
	fixture.outcome.AllTablesVerified = true
	return nil
}

func memoryArchiveLoadChecksums(archive []byte) (archiveexport.Checksums, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return archiveexport.Checksums{}, errors.New("open generated archive for checksum summary")
	}
	defer func() { _ = gz.Close() }()
	tarReader := tar.NewReader(gz)
	for {
		header, nextErr := tarReader.Next()
		if errors.Is(nextErr, io.EOF) {
			return archiveexport.Checksums{}, errors.New("generated archive has no checksum trailer")
		}
		if nextErr != nil {
			return archiveexport.Checksums{}, errors.New("read generated archive checksum summary")
		}
		if header.Name != "checksums.json" {
			continue
		}
		raw, readErr := io.ReadAll(tarReader)
		if readErr != nil {
			return archiveexport.Checksums{}, errors.New("read generated archive checksum trailer")
		}
		var checksums archiveexport.Checksums
		if err := json.Unmarshal(raw, &checksums); err != nil {
			return archiveexport.Checksums{}, errors.New("decode generated archive checksum trailer")
		}
		return checksums, nil
	}
}

func removeMemoryArchiveLoadAccount(
	ctx context.Context,
	st *Store,
	accountID string,
	want map[string]int,
) (bool, error) {
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	deleted, err := purgePortableAccountRowsTx(ctx, tx, accountID, SchemaVersion())
	if err != nil {
		return false, err
	}
	for table, count := range want {
		if table == "accounts" {
			continue
		}
		if deleted[table] != int64(count) {
			return false, fmt.Errorf("portable deletion count for %s was %d, want %d", table, deleted[table], count)
		}
	}
	tag, err := tx.Exec(ctx, `DELETE FROM accounts WHERE id=$1`, accountID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, errors.New("portable deletion did not remove exactly one account row")
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	var exists bool
	if err := st.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id=$1)`, accountID).Scan(&exists); err != nil {
		return false, err
	}
	return !exists, nil
}

func finishMemoryArchiveLoadRowComparison(fixture *memoryArchiveLoadFixture) error {
	if !memoryArchiveLoadSameRowCounts(fixture.verifiedRows, fixture.importedRows) {
		return fmt.Errorf("cardinality %d imported per-table row counts differ", fixture.cardinality)
	}
	importedTotal := 0
	for index := range fixture.outcome.TableRows {
		name := fixture.outcome.TableRows[index].Name
		imported := fixture.importedRows[name]
		fixture.outcome.TableRows[index].Imported = imported
		importedTotal += imported
	}
	fixture.outcome.ImportedRows = importedTotal
	fixture.outcome.ExactTableRowCounts = true
	return nil
}

func verifyMemoryArchiveLoadImportedVectors(
	ctx context.Context,
	st *Store,
	fixture *memoryArchiveLoadFixture,
) error {
	profiles, err := st.ListMemoryVectorProfiles(ctx, fixture.principal)
	if err != nil {
		return fmt.Errorf("list imported vector profiles: %w", err)
	}
	if len(profiles) != 1 || !memoryArchiveLoadSameVectorProfile(profiles[0], fixture.profile) {
		return errors.New("imported vector profile differs from the exported profile")
	}
	fixture.vectorProfileSeen = true
	if fixture.importedRows["memory_vectors"] != fixture.cardinality ||
		len(fixture.vectorReceipts) != fixture.cardinality {
		return errors.New("imported vector row count differs from memory cardinality")
	}
	expected := make(map[string]MemoryVectorReceipt, len(fixture.vectorReceipts))
	for _, receipt := range fixture.vectorReceipts {
		expected[memoryArchiveLoadVectorKey(receipt)] = receipt
	}
	rows, err := st.pool.Query(ctx, `
		SELECT profile_id,memory_id,memory_version,content_hash,vector_hash,
		       jsonb_array_length(vector),created_at
		FROM memory_vectors WHERE account_id=$1
		ORDER BY profile_id,memory_id,memory_version`, fixture.principal.AccountID)
	if err != nil {
		return fmt.Errorf("read imported vector receipts: %w", err)
	}
	defer rows.Close()
	seen := make(map[string]struct{}, len(expected))
	for rows.Next() {
		var actual MemoryVectorReceipt
		if err := rows.Scan(
			&actual.ProfileID, &actual.MemoryID, &actual.MemoryVersion,
			&actual.ContentHash, &actual.VectorHash, &actual.Dimensions,
			&actual.CreatedAt,
		); err != nil {
			return fmt.Errorf("scan imported vector receipt: %w", err)
		}
		key := memoryArchiveLoadVectorKey(actual)
		want, ok := expected[key]
		if !ok || !memoryArchiveLoadSameVectorReceipt(actual, want) {
			return errors.New("imported vector receipt differs from the exported fixture")
		}
		if _, duplicate := seen[key]; duplicate {
			return errors.New("imported vector receipt was duplicated")
		}
		seen[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read imported vector receipts: %w", err)
	}
	if len(seen) != len(expected) {
		return errors.New("imported vector receipt set is incomplete")
	}
	fixture.vectorsSeen = true
	tagAssignments, err := memoryArchiveLoadTagAssignments(
		ctx, st, fixture.principal.AccountID,
	)
	if err != nil {
		return err
	}
	if tagAssignments != fixture.outcome.FocalCounts.TagAssignments {
		return errors.New("imported tag assignment count differs from the exported fixture")
	}
	return nil
}

func memoryArchiveLoadSameVectorProfile(left, right MemoryVectorProfile) bool {
	return left.ID == right.ID && left.Provider == right.Provider &&
		left.Model == right.Model && left.Recipe == right.Recipe &&
		left.RecipeVersion == right.RecipeVersion && left.Dimensions == right.Dimensions &&
		left.DistanceMetric == right.DistanceMetric && left.Normalization == right.Normalization &&
		left.ContractHash == right.ContractHash && left.CreatedAt.Equal(right.CreatedAt)
}

func memoryArchiveLoadVectorKey(receipt MemoryVectorReceipt) string {
	return fmt.Sprintf("%s\x00%s\x00%d", receipt.ProfileID, receipt.MemoryID, receipt.MemoryVersion)
}

func memoryArchiveLoadSameVectorReceipt(left, right MemoryVectorReceipt) bool {
	return left.ProfileID == right.ProfileID && left.MemoryID == right.MemoryID &&
		left.MemoryVersion == right.MemoryVersion && left.ContentHash == right.ContentHash &&
		left.VectorHash == right.VectorHash && left.Dimensions == right.Dimensions &&
		left.CreatedAt.Equal(right.CreatedAt)
}

func runMemoryArchiveLoadSafety(
	ctx context.Context,
	st *Store,
	fixture *memoryArchiveLoadFixture,
	neighbor Principal,
) (loadquality.OperationStats, loadquality.ArchiveSafetyOutcome, error) {
	type call struct {
		principal Principal
		include   bool
	}
	calls := []call{
		{principal: fixture.principal},
		{principal: fixture.principal, include: true},
		{principal: fixture.peer},
		{principal: neighbor},
	}
	pages := make([]MemoryRecallPage, 0, len(calls))
	durations := make([]time.Duration, 0, len(calls))
	started := time.Now()
	for _, item := range calls {
		operationStarted := time.Now()
		page, err := st.RecallMemories(ctx, item.principal, MemoryRecallOptions{
			Query: fixture.sensitiveQuery, IncludeSensitive: item.include, Limit: 10,
		})
		durations = append(durations, time.Since(operationStarted))
		if err != nil {
			return loadquality.OperationStats{}, loadquality.ArchiveSafetyOutcome{}, err
		}
		pages = append(pages, page)
	}
	stats, err := loadquality.Summarize(durations, time.Since(started))
	if err != nil {
		return loadquality.OperationStats{}, loadquality.ArchiveSafetyOutcome{}, err
	}
	if len(pages[0].Hits) != 1 || len(pages[1].Hits) != 1 {
		return loadquality.OperationStats{}, loadquality.ArchiveSafetyOutcome{}, errors.New("post-import sensitive recalls did not return exactly one fixture")
	}
	broad := pages[0].Hits[0].Memory
	exact := pages[1].Hits[0].Memory
	broadRedacted := fixture.sensitiveBroadPre &&
		memoryArchiveLoadBroadRedacted(broad, fixture.sensitive)
	exactVisible := memoryArchiveLoadExactSensitiveVisible(exact, fixture.sensitive)
	crossAgentIsolated := len(pages[2].Hits) == 0
	crossAccountIsolated := len(pages[3].Hits) == 0
	if !broadRedacted || !exactVisible || !crossAgentIsolated || !crossAccountIsolated {
		return loadquality.OperationStats{}, loadquality.ArchiveSafetyOutcome{}, errors.New("post-import redaction or isolation assertion failed")
	}
	return stats, loadquality.ArchiveSafetyOutcome{
		RecallCalls: len(durations), SensitiveBroadRedacted: broadRedacted,
		SensitiveExactOwnerVisible: exactVisible, CrossAgentIsolated: crossAgentIsolated,
		CrossAccountIsolated: crossAccountIsolated,
	}, nil
}

func memoryArchiveLoadBroadRedacted(memory, expected Memory) bool {
	return memory.ID == expected.ID && memory.Version == expected.Version &&
		memory.State == MemoryStateActive && memory.Sensitive && memory.Redacted &&
		memory.Content == "" && memory.ContentHash == "" && len(memory.Tags) == 0 &&
		len(memory.Links) == 0 && memory.CaptureReason == "" &&
		memory.LifecycleReason == "" && memory.OccurredFrom == nil &&
		memory.OccurredUntil == nil && memory.IdempotencyKey == "" &&
		memory.RequestHash == "" && memory.Client == (MemoryClientProvenance{}) &&
		len(memory.Evidence) == 0
}

func memoryArchiveLoadExactSensitiveVisible(memory, expected Memory) bool {
	return memory.ID == expected.ID && memory.Version == expected.Version &&
		memory.State == expected.State && memory.Sensitive && !memory.Redacted &&
		memory.Content == expected.Content && memory.ContentHash == expected.ContentHash &&
		memory.ContentEncoding == expected.ContentEncoding && memory.Kind == expected.Kind &&
		reflect.DeepEqual(memory.Tags, expected.Tags) && reflect.DeepEqual(memory.Links, expected.Links) &&
		memory.CaptureReason == expected.CaptureReason &&
		memory.LifecycleReason == expected.LifecycleReason &&
		memoryArchiveLoadSameTimePointer(memory.OccurredFrom, expected.OccurredFrom) &&
		memoryArchiveLoadSameTimePointer(memory.OccurredUntil, expected.OccurredUntil) &&
		memory.IdempotencyKey == expected.IdempotencyKey && memory.RequestHash == expected.RequestHash &&
		memory.Client == expected.Client
}

func memoryArchiveLoadSameTimePointer(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func memoryArchiveLoadPortableCounts(
	ctx context.Context,
	st *Store,
	accountID string,
) (map[string]int, error) {
	tx, err := st.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	counts64, err := countPortableAccountRowsTx(ctx, tx, accountID, SchemaVersion())
	if err != nil {
		return nil, err
	}
	var accountCount int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE id=$1`, accountID).Scan(&accountCount); err != nil {
		return nil, err
	}
	counts64["accounts"] = accountCount
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(counts64))
	for table, count := range counts64 {
		counts[table] = int(count)
	}
	return counts, nil
}

// memoryArchiveLoadContentTables are the fixture-seeded tenant-content tables
// whose rows import verbatim: an order-independent per-table digest before
// export must survive the round trip byte-for-byte. The accounts row is
// excluded deliberately - import legitimately rewrites lifecycle fields - and
// is covered by the explicit status assertions instead.
var memoryArchiveLoadContentTables = []string{
	"memories", "memory_versions", "memory_evidence", "memory_relations",
	"transcript_conversations", "transcript_entries",
	"memory_vector_profiles", "memory_vectors",
}

func memoryArchiveLoadContentDigests(
	ctx context.Context,
	st *Store,
	accountID string,
) (map[string]string, error) {
	digests := make(map[string]string, len(memoryArchiveLoadContentTables))
	for _, table := range memoryArchiveLoadContentTables {
		query := fmt.Sprintf(`
			SELECT COALESCE(string_agg(row_hash, '' ORDER BY row_hash), '')
			FROM (SELECT md5(t::text) AS row_hash FROM %s t WHERE account_id=$1) rows`,
			pgx.Identifier{table}.Sanitize(),
		)
		var digest string
		if err := st.pool.QueryRow(ctx, query, accountID).Scan(&digest); err != nil {
			return nil, fmt.Errorf("digest table %s: %w", table, err)
		}
		digests[table] = digest
	}
	return digests, nil
}

func memoryArchiveLoadAccountStatus(
	ctx context.Context,
	st *Store,
	accountID string,
) (string, string, error) {
	var status string
	var suspendedFor *string
	if err := st.pool.QueryRow(ctx, `
		SELECT status, suspended_for FROM accounts WHERE id=$1`, accountID,
	).Scan(&status, &suspendedFor); err != nil {
		return "", "", fmt.Errorf("read imported account status: %w", err)
	}
	if suspendedFor == nil {
		return status, "", nil
	}
	return status, *suspendedFor, nil
}

func memoryArchiveLoadTagAssignments(ctx context.Context, st *Store, accountID string) (int, error) {
	var count int
	if err := st.pool.QueryRow(ctx, `
		SELECT COALESCE(sum(jsonb_array_length(tags)),0)
		FROM memory_versions WHERE account_id=$1`, accountID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count archive tag assignments: %w", err)
	}
	return count, nil
}

func memoryArchiveLoadFocalCounts(counts map[string]int, tagAssignments int) loadquality.ArchiveFocalCounts {
	return loadquality.ArchiveFocalCounts{
		Memories: counts["memories"], MemoryVersions: counts["memory_versions"],
		MemoryEvidence: counts["memory_evidence"], MemoryRelations: counts["memory_relations"],
		TranscriptConversations: counts["transcript_conversations"],
		TranscriptEntries:       counts["transcript_entries"],
		MemoryVectorProfiles:    counts["memory_vector_profiles"],
		MemoryVectors:           counts["memory_vectors"], TagAssignments: tagAssignments,
	}
}

func memoryArchiveLoadSameRowCounts(left, right map[string]int) bool {
	if len(left) != len(right) {
		return false
	}
	for table, count := range left {
		if right[table] != count {
			return false
		}
	}
	return true
}

func memoryArchiveLoadSingleStats(duration time.Duration) (loadquality.OperationStats, error) {
	return loadquality.Summarize([]time.Duration{duration}, duration)
}

func memoryArchiveLoadToken(seed int64, parts ...string) string {
	value := fmt.Sprintf("%d", seed)
	for _, part := range parts {
		value += "\x00" + part
	}
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:6])
}
