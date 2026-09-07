package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/loadquality"
)

const memoryRelevanceEnabled = "WITSELF_MEMORY_RELEVANCE"

// TestNarrativeMemoryRelevancePostgres measures the entire first lexical page
// against a fixed, independently adjudicated synthetic corpus. Completion means
// the observations are valid, not that the measured relevance meets a threshold.
func TestNarrativeMemoryRelevancePostgres(t *testing.T) {
	if os.Getenv(memoryRelevanceEnabled) != "1" {
		t.Skip(memoryRelevanceEnabled + "=1 is required")
	}
	dsn := strings.TrimSpace(os.Getenv("WITSELF_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Fatal("WITSELF_TEST_DATABASE_URL is required when relevance measurement is enabled")
	}
	opts, err := loadquality.ParseRelevanceOptions(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	corpus, digest, err := loadquality.DefaultRelevanceCorpus()
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	st, _ := newMemoryRelevanceTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal("migrate isolated relevance fixture")
	}
	var version int
	if err := st.pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::integer`).Scan(&version); err != nil || version < 100000 {
		t.Fatal("read supported PostgreSQL software version")
	}

	prepareCtx, prepareCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer prepareCancel()
	owner, labelsByID := prepareMemoryRelevanceFixture(prepareCtx, t, st, corpus)
	prepareCancel()
	var changeSeq, deletedCount int64
	var memoryCount, versionCount, evidenceCount, activeCount int
	if err := st.pool.QueryRow(ctx, `
		SELECT c.last_change_seq, c.active_memory_count,
		  (SELECT count(*) FROM memories m WHERE m.account_id=c.account_id
		    AND m.realm_id=c.realm_id AND m.owner_kind=c.owner_kind AND m.owner_id=c.owner_id
		    AND m.current_version IS NULL),
		  (SELECT count(*) FROM memories m WHERE m.account_id=c.account_id
		    AND m.realm_id=c.realm_id AND m.owner_kind=c.owner_kind AND m.owner_id=c.owner_id
		    AND m.current_version IS NOT NULL),
		  (SELECT count(*) FROM memory_versions v WHERE v.account_id=c.account_id
		    AND v.realm_id=c.realm_id AND v.owner_kind=c.owner_kind AND v.owner_id=c.owner_id),
		  (SELECT count(*) FROM memory_evidence e WHERE e.account_id=c.account_id
		    AND e.realm_id=c.realm_id AND e.owner_kind=c.owner_kind AND e.owner_id=c.owner_id)
		FROM memory_change_clocks c WHERE c.account_id=$1 AND c.realm_id=$2
		  AND c.owner_kind='agent' AND c.owner_id=$3`, owner.AccountID, owner.RealmID, owner.ID).
		Scan(&changeSeq, &activeCount, &deletedCount, &memoryCount, &versionCount, &evidenceCount); err != nil ||
		changeSeq != 2*int64(len(corpus.Memories)) || deletedCount != 0 || memoryCount != len(corpus.Memories) ||
		activeCount != memoryCount || versionCount != memoryCount || evidenceCount != memoryCount {
		t.Fatal("verify complete relevance fixture and committed snapshot")
	}
	contentByLabel := make(map[string]string, len(corpus.Memories))
	for _, memory := range corpus.Memories {
		contentByLabel[memory.Label] = memory.Content
	}
	queryCtx, queryCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer queryCancel()
	asOf := loadquality.RelevanceRecallAsOf()
	cases := make([]loadquality.RelevanceScore, 0, len(corpus.Queries))
	for _, query := range corpus.Queries {
		page, err := st.RecallMemories(queryCtx, owner, MemoryRecallOptions{
			Query: query.Query, Limit: query.TopK, ExcludeSensitive: true,
			AsOf: &asOf, SnapshotChangeSeq: &changeSeq, SnapshotDeletedMemoryCount: &deletedCount,
		})
		if err != nil {
			t.Fatalf("measure lexical relevance case %s", query.Name)
		}
		if page.RetrievalMode != "lexical" || page.Degraded || page.VectorCoverage != 0 ||
			page.VectorProfileID != "" || page.VectorCandidates != 0 || page.VectorMatches != 0 ||
			page.CandidateTruncated || page.CandidateLimit != 0 || page.DegradedReason != "" || len(page.Hits) > query.TopK {
			t.Fatalf("unexpected retrieval mode or page bound in case %s", query.Name)
		}
		labels := make([]string, 0, len(page.Hits))
		for _, hit := range page.Hits {
			label, known := labelsByID[hit.Memory.ID]
			if !known || hit.Memory.AccountID != owner.AccountID || hit.Memory.RealmID != owner.RealmID ||
				hit.Memory.OwnerKind != "agent" || hit.Memory.OwnerID != owner.ID || hit.Memory.Sensitive || hit.Memory.Redacted ||
				hit.Memory.Content != contentByLabel[label] || hit.Memory.State != MemoryStateActive || hit.Memory.Version != 1 ||
				hit.Memory.Salience != 0.5 || hit.Score.Salience != 0.5 || hit.Score.VectorUsed {
				t.Fatalf("unknown or changed synthetic hit in case %s", query.Name)
			}
			labels = append(labels, label)
		}
		score, err := loadquality.ScoreRelevance(corpus, query.Name, labels)
		if err != nil {
			t.Fatalf("score complete relevance case %s: %v", query.Name, err)
		}
		cases = append(cases, score)
	}
	result := loadquality.RelevanceResult{
		Schema: loadquality.RelevanceResultSchemaV1, HarnessVersion: "1",
		StartedAt: started, CompletedAt: time.Now().UTC(), Outcome: "measured",
		PostgreSQLVersion: fmt.Sprintf("%d.%d", version/10000, version%10000),
		Environment:       loadquality.RelevanceEnvironment(opts),
		CorpusSHA256:      digest, CorpusMemories: len(corpus.Memories), QueryCount: len(corpus.Queries),
		FixtureClock: loadquality.RelevanceFixtureClock, RecallAsOf: asOf, Cases: cases,
	}
	if _, err := loadquality.WriteRelevanceResult(opts.ResultsPath, result); err != nil {
		t.Fatal(err)
	}
	t.Logf("measured %d fixed relevance cases; no production quality threshold asserted", len(cases))
	t.Logf("sanitized relevance result written to %s", opts.ResultsPath)
}

func prepareMemoryRelevanceFixture(ctx context.Context, t *testing.T, st *Store, corpus loadquality.RelevanceCorpus) (Principal, map[string]string) {
	t.Helper()
	account, err := st.ProvisionAccount(ctx, "memory-relevance@example.invalid", "synthetic relevance", time.Hour)
	if err != nil {
		t.Fatal("provision synthetic relevance account")
	}
	if activated, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !activated {
		t.Fatal("activate synthetic relevance account")
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "relevance")
	if err != nil {
		t.Fatal("create synthetic relevance realm")
	}
	agent, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "owner")
	if err != nil {
		t.Fatal("create synthetic relevance agent")
	}
	owner := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: account.AccountID,
		RealmID: realm.ID, AgentName: agent.Name, RealmName: realm.Name, AccountStatus: "active"}
	labelsByID := make(map[string]string, len(corpus.Memories))
	epoch := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for index, fixture := range corpus.Memories {
		salience := 0.5
		captured, err := st.CaptureMemory(ctx, owner, CaptureMemoryInput{
			Content: fixture.Content, Kind: "note", Salience: &salience,
			CaptureReason: "relevance_fixture", IdempotencyKey: "relevance-" + fixture.Label,
			Client:   MemoryClientProvenance{Runtime: "load-quality", Recipe: "fixed-relevance-corpus", RecipeVersion: "1"},
			Evidence: []MemoryEvidenceInput{{ResolutionState: MemoryEvidenceUnavailable, TerminalReasonCode: "synthetic_fixture"}},
		})
		if err != nil {
			t.Fatalf("capture synthetic fixture %s", fixture.Label)
		}
		if _, duplicate := labelsByID[captured.Memory.ID]; duplicate {
			t.Fatal("capture reused a synthetic memory identity")
		}
		labelsByID[captured.Memory.ID] = fixture.Label
		// Fixture-only clock normalization preserves the production ranker while
		// making recency and timestamp tie-breaking reproducible across runs.
		at := epoch.Add(time.Duration(index) * time.Second)
		updated, err := st.pool.Exec(ctx, `UPDATE memories SET created_at=$1, updated_at=$1
			WHERE id=$2 AND account_id=$3 AND realm_id=$4 AND owner_kind='agent' AND owner_id=$5`,
			at, captured.Memory.ID, owner.AccountID, owner.RealmID, owner.ID)
		if err != nil || updated.RowsAffected() != 1 {
			t.Fatal("normalize synthetic memory clock")
		}
		updated, err = st.pool.Exec(ctx, `UPDATE memory_versions SET created_at=$1
			WHERE memory_id=$2 AND version=1 AND account_id=$3 AND realm_id=$4 AND owner_kind='agent' AND owner_id=$5`,
			at, captured.Memory.ID, owner.AccountID, owner.RealmID, owner.ID)
		if err != nil || updated.RowsAffected() != 1 {
			t.Fatal("normalize synthetic memory version clock")
		}
		persisted, err := st.GetMemory(ctx, owner, captured.Memory.ID)
		if err != nil || persisted.ID != captured.Memory.ID || persisted.AccountID != owner.AccountID ||
			persisted.RealmID != owner.RealmID || persisted.OwnerKind != "agent" || persisted.OwnerID != owner.ID ||
			persisted.Version != 1 || persisted.State != MemoryStateActive || persisted.Kind != "note" ||
			persisted.Content != fixture.Content || persisted.ContentHash != memoryContentHash(fixture.Content) || persisted.ContentEncoding != "plain" ||
			persisted.Salience != salience || persisted.Sensitive || persisted.Redacted ||
			len(persisted.Tags) != 0 || len(persisted.Links) != 0 ||
			!persisted.CreatedAt.Equal(at) || !persisted.UpdatedAt.Equal(at) ||
			persisted.ChangeSeq != 2*int64(index)+1 || len(persisted.Evidence) != 1 {
			t.Fatalf("verify captured relevance fixture %s before measurement", fixture.Label)
		}
		evidence := persisted.Evidence[0]
		if evidence.TargetVersion != 1 || evidence.EvidenceChangeSeq != 2*int64(index)+2 ||
			evidence.ResolutionState != MemoryEvidenceUnavailable || evidence.PendingEvidenceID != "" ||
			evidence.TerminalReasonCode != "synthetic_fixture" {
			t.Fatal("verify synthetic evidence cannot change fixture recency")
		}
	}
	return owner, labelsByID
}

const memoryRelevanceSetupFailure = "relevance database fixture failed; connection details suppressed"

// Keep native connection and cleanup diagnostics out of retained evidence logs.
// This boundary affects only the new relevance driver, not shared test behavior.
type memoryRelevanceReporter struct{ sink migrationTestReporter }

func (r memoryRelevanceReporter) Helper()                { r.sink.Helper() }
func (r memoryRelevanceReporter) Cleanup(cleanup func()) { r.sink.Cleanup(cleanup) }
func (r memoryRelevanceReporter) Fatal(_ ...any)         { r.sink.Fatal(memoryRelevanceSetupFailure) }
func (r memoryRelevanceReporter) Fatalf(_ string, _ ...any) {
	r.sink.Fatal(memoryRelevanceSetupFailure)
}
func (r memoryRelevanceReporter) Errorf(_ string, _ ...any) {
	r.sink.Errorf("%s", memoryRelevanceSetupFailure)
}

func newMemoryRelevanceTestStore(t migrationTestReporter, dsn string) (*Store, string) {
	t.Helper()
	return newMigrationTestStore(memoryRelevanceReporter{sink: t}, dsn)
}

func TestMemoryRelevanceSetupRedactsMalformedDSN(t *testing.T) {
	const childKey = "WITSELF_RELEVANCE_REDACTION_CHILD"
	const malformed = "postgres://relevance_fixture_user:relevance_fixture_password@%zz/relevance_fixture_database"
	if mode := os.Getenv(childKey); mode != "" {
		if mode == "raw" {
			newMigrationTestStore(t, malformed)
		} else {
			newMemoryRelevanceTestStore(t, malformed)
		}
		t.Fatal("malformed connection unexpectedly accepted")
	}
	// The raw control proves this exact malformed URL generates the diagnostic
	// being protected. URL parsing fails before any socket or schema is opened.
	for _, mode := range []string{"raw", "protected"} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMemoryRelevanceSetupRedactsMalformedDSN$", "-test.v")
		command.Env = append(os.Environ(), childKey+"="+mode)
		output, err := command.CombinedOutput()
		contextError := ctx.Err()
		cancel()
		var exitError *exec.ExitError
		if contextError != nil || !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
			t.Fatal("malformed connection control did not exit with the expected test failure")
		}
		text := string(output)
		containsPrivateField := strings.Contains(text, "relevance_fixture_user") ||
			strings.Contains(text, "relevance_fixture_password") || strings.Contains(text, "relevance_fixture_database")
		if mode == "raw" && !containsPrivateField {
			t.Fatal("malformed connection control did not exercise protected diagnostic fields")
		}
		if mode == "protected" && (containsPrivateField || !strings.Contains(text, memoryRelevanceSetupFailure)) {
			t.Fatal("connection details escaped the relevance setup boundary")
		}
	}
}
