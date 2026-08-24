package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	archiveexport "github.com/witwave-ai/witself/internal/export"
)

func TestAccountEvacuationBeginIsExactIDIdempotentPostgres(t *testing.T) {
	ctx, st := openAccountEvacuationTestStore(t)
	provisioned := provisionActiveEvacuationTestAccount(ctx, t, st, "begin")
	const evacuationID = "evac_begin_exact_id"

	first, err := st.BeginAccountEvacuation(
		ctx, provisioned.AccountID, evacuationID, "exact retry test",
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.AccountID != provisioned.AccountID ||
		first.EvacuationID != evacuationID ||
		first.Role != "source" ||
		first.Status != "suspended" ||
		first.StartedAt == nil ||
		first.CompletedAt != nil ||
		first.Completed ||
		first.Aborted {
		t.Fatalf("first begin acknowledgement = %#v", first)
	}

	retry, err := st.BeginAccountEvacuation(
		ctx, provisioned.AccountID, evacuationID, "ignored on retry",
	)
	if err != nil {
		t.Fatal(err)
	}
	if retry.AccountID != first.AccountID ||
		retry.EvacuationID != first.EvacuationID ||
		retry.Role != "source" ||
		retry.Status != first.Status ||
		retry.StartedAt == nil ||
		!retry.StartedAt.Equal(*first.StartedAt) ||
		retry.CompletedAt != nil ||
		retry.Completed ||
		retry.Aborted {
		t.Fatalf("retry acknowledgement = %#v; first = %#v", retry, first)
	}

	if _, err := st.BeginAccountEvacuation(
		ctx, provisioned.AccountID, "evac_begin_competing_id", "must conflict",
	); !errors.Is(err, ErrAccountEvacuationInProgress) {
		t.Fatalf(
			"competing begin error = %v, want ErrAccountEvacuationInProgress",
			err,
		)
	}

	var suspendEvents int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM account_events
		 WHERE account_id = $1
		   AND verb = $2`,
		provisioned.AccountID, VerbAccountSuspendedBySystem,
	).Scan(&suspendEvents); err != nil {
		t.Fatal(err)
	}
	if suspendEvents != 1 {
		t.Fatalf("system suspension events = %d, want 1", suspendEvents)
	}

	if _, err := st.AbortAccountEvacuation(
		ctx, provisioned.AccountID, evacuationID,
	); err != nil {
		t.Fatal(err)
	}
}

func TestAccountEvacuationBeginRejectsUnmarkedLegacySystemSuspensionPostgres(
	t *testing.T,
) {
	ctx, st := openAccountEvacuationTestStore(t)
	provisioned := provisionActiveEvacuationTestAccount(
		ctx, t, st, "legacy-system-suspension",
	)
	if err := st.SuspendAccountSystem(
		ctx, provisioned.AccountID, "evacuation",
		"legacy suspension without an exact epoch",
	); err != nil {
		t.Fatal(err)
	}

	if _, err := st.BeginAccountEvacuation(
		ctx, provisioned.AccountID, "evac_must_not_adopt_legacy",
		"new exact operation",
	); !errors.Is(err, ErrAccountEvacuationMismatch) {
		t.Fatalf(
			"begin over unmarked legacy suspension error = %v, want %v",
			err, ErrAccountEvacuationMismatch,
		)
	}

	var status string
	var suspendedFor string
	var evacuationID *string
	if err := st.pool.QueryRow(ctx, `
		SELECT status, suspended_for, evacuation_id
		  FROM accounts
		 WHERE id = $1`,
		provisioned.AccountID,
	).Scan(&status, &suspendedFor, &evacuationID); err != nil {
		t.Fatal(err)
	}
	if status != "suspended" ||
		suspendedFor != "evacuation" ||
		evacuationID != nil {
		t.Fatalf(
			"legacy suspension changed = status %q for %q evacuation %#v",
			status, suspendedFor, evacuationID,
		)
	}
	if err := st.ResumeAccountSystem(
		ctx, provisioned.AccountID, "evacuation",
	); err != nil {
		t.Fatal(err)
	}
}

func TestAccountEvacuationFenceBlocksTenantMutationsPostgres(t *testing.T) {
	ctx, st := openAccountEvacuationTestStore(t)
	provisioned := provisionActiveEvacuationTestAccount(ctx, t, st, "fence")
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "fenced-realm")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(
		ctx, provisioned.AccountID, realm.ID, "fenced-agent",
	)
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{
		Kind:          PrincipalAgent,
		ID:            agent.ID,
		AccountID:     provisioned.AccountID,
		RealmID:       realm.ID,
		AgentName:     agent.Name,
		AccountStatus: "active",
	}
	if _, err := st.TouchAgentActivity(ctx, principal, AgentActivityInput{
		Runtime:         "codex",
		LocationID:      "evacuation-fence-test",
		Location:        "before",
		Event:           "SessionStart",
		EventID:         "evt_before_evacuation",
		EventOccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	const evacuationID = "evac_mutation_fence"
	if _, err := st.BeginAccountEvacuation(
		ctx, provisioned.AccountID, evacuationID, "mutation fence test",
	); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		sql  string
		args []any
	}{
		{
			name: "accounts",
			sql:  `UPDATE accounts SET display_name = display_name WHERE id = $1`,
			args: []any{provisioned.AccountID},
		},
		{
			name: "direct account_id table",
			sql:  `UPDATE realms SET name = name WHERE id = $1`,
			args: []any{realm.ID},
		},
		{
			name: "agents through realm",
			sql:  `UPDATE agents SET name = name WHERE id = $1`,
			args: []any{agent.ID},
		},
		{
			name: "agent_activity through agent and realm",
			sql: `
				UPDATE agent_activity
				   SET location = location
				 WHERE agent_id = $1
				   AND runtime = 'codex'
				   AND location_id = 'evacuation-fence-test'`,
			args: []any{agent.ID},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := st.pool.Exec(ctx, test.sql, test.args...); err == nil {
				t.Fatal("mutation succeeded while evacuation marker was active")
			} else {
				assertAccountEvacuationFenceError(t, err)
			}
		})
	}

	if _, err := st.AbortAccountEvacuation(
		ctx, provisioned.AccountID, evacuationID,
	); err != nil {
		t.Fatal(err)
	}
}

func TestAccountEvacuationIndirectFencesRejectInvisibleParentsPostgres(
	t *testing.T,
) {
	ctx, st := openAccountEvacuationTestStore(t)
	provisioned := provisionActiveEvacuationTestAccount(
		ctx, t, st, "invisible-parent",
	)

	t.Run("agent referencing uncommitted realm", func(t *testing.T) {
		parentTx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = parentTx.Rollback(ctx) }()
		realmID := fmt.Sprintf("rlm_uncommitted_%d", time.Now().UnixNano())
		if _, err := parentTx.Exec(ctx, `
			INSERT INTO realms(id, account_id, name)
			VALUES ($1, $2, $3)`,
			realmID, provisioned.AccountID, realmID,
		); err != nil {
			t.Fatal(err)
		}

		childTx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = childTx.Rollback(ctx) }()
		if _, err := childTx.Exec(ctx,
			`SET LOCAL lock_timeout = '500ms'`,
		); err != nil {
			t.Fatal(err)
		}
		_, err = childTx.Exec(ctx, `
			INSERT INTO agents(id, realm_id, name)
			VALUES ($1, $2, $3)`,
			fmt.Sprintf("agt_uncommitted_%d", time.Now().UnixNano()),
			realmID, "uncommitted-realm-agent",
		)
		if err == nil {
			t.Fatal("agent insert unexpectedly crossed an invisible realm")
		}
		assertAccountEvacuationFenceError(t, err)
	})

	t.Run("activity referencing uncommitted agent", func(t *testing.T) {
		realm, err := st.CreateRealm(
			ctx, provisioned.AccountID,
			fmt.Sprintf("activity-parent-%d", time.Now().UnixNano()),
		)
		if err != nil {
			t.Fatal(err)
		}
		parentTx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = parentTx.Rollback(ctx) }()
		agentID := fmt.Sprintf("agt_uncommitted_%d", time.Now().UnixNano())
		if _, err := parentTx.Exec(ctx, `
			INSERT INTO agents(id, realm_id, name)
			VALUES ($1, $2, $3)`,
			agentID, realm.ID, "uncommitted-activity-agent",
		); err != nil {
			t.Fatal(err)
		}

		childTx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = childTx.Rollback(ctx) }()
		if _, err := childTx.Exec(ctx,
			`SET LOCAL lock_timeout = '500ms'`,
		); err != nil {
			t.Fatal(err)
		}
		_, err = childTx.Exec(ctx, `
			INSERT INTO agent_activity(
				agent_id, runtime, location_id, location,
				last_event, last_event_id, last_event_occurred_at
			) VALUES ($1, 'codex', 'invisible-parent', '',
			          'SessionStart', 'evt_invisible_parent',
			          clock_timestamp())`,
			agentID,
		)
		if err == nil {
			t.Fatal("activity insert unexpectedly crossed an invisible agent")
		}
		assertAccountEvacuationFenceError(t, err)
	})
}

func TestAccountEvacuationIndirectFencesLockDependencyMappingsPostgres(
	t *testing.T,
) {
	ctx, st := openAccountEvacuationTestStore(t)
	source := provisionActiveEvacuationTestAccount(ctx, t, st, "mapping-source")
	target := provisionActiveEvacuationTestAccount(ctx, t, st, "mapping-target")
	sourceRealm, err := st.CreateRealm(
		ctx, source.AccountID,
		fmt.Sprintf("mapping-source-%d", time.Now().UnixNano()),
	)
	if err != nil {
		t.Fatal(err)
	}
	targetRealm, err := st.CreateRealm(
		ctx, target.AccountID,
		fmt.Sprintf("mapping-target-%d", time.Now().UnixNano()),
	)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(
		ctx, source.AccountID, sourceRealm.ID, "mapping-agent",
	)
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{
		Kind: PrincipalAgent, ID: agent.ID, AccountID: source.AccountID,
		RealmID: sourceRealm.ID, AgentName: agent.Name, AccountStatus: "active",
	}
	if _, err := st.TouchAgentActivity(ctx, principal, AgentActivityInput{
		Runtime: "codex", LocationID: "mapping-lock", Location: "source",
		Event: "SessionStart", EventID: "evt_mapping_lock",
		EventOccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("agent waits for realm mapping", func(t *testing.T) {
		mappingTx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = mappingTx.Rollback(ctx) }()
		if _, err := mappingTx.Exec(ctx,
			`UPDATE realms SET account_id = $2 WHERE id = $1`,
			sourceRealm.ID, target.AccountID,
		); err != nil {
			t.Fatal(err)
		}

		mutationTx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = mutationTx.Rollback(ctx) }()
		if _, err := mutationTx.Exec(ctx,
			`SET LOCAL lock_timeout = '200ms'`,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := mutationTx.Exec(ctx,
			`UPDATE agents SET name = name WHERE id = $1`,
			agent.ID,
		); err == nil {
			t.Fatal("agent mutation did not wait for realm mapping lock")
		} else {
			assertPostgresCode(t, err, "55P03")
		}
	})

	t.Run("activity waits for agent mapping", func(t *testing.T) {
		mappingTx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = mappingTx.Rollback(ctx) }()
		if _, err := mappingTx.Exec(ctx,
			`UPDATE agents SET realm_id = $2 WHERE id = $1`,
			agent.ID, targetRealm.ID,
		); err != nil {
			t.Fatal(err)
		}

		mutationTx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = mutationTx.Rollback(ctx) }()
		if _, err := mutationTx.Exec(ctx,
			`SET LOCAL lock_timeout = '200ms'`,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := mutationTx.Exec(ctx, `
			UPDATE agent_activity
			   SET location = location
			 WHERE agent_id = $1
			   AND runtime = 'codex'
			   AND location_id = 'mapping-lock'`,
			agent.ID,
		); err == nil {
			t.Fatal("activity mutation did not wait for agent mapping lock")
		} else {
			assertPostgresCode(t, err, "55P03")
		}
	})
}

func TestAbortAccountEvacuationRestoresOnlyEvacuationSuspensionPostgres(
	t *testing.T,
) {
	ctx, st := openAccountEvacuationTestStore(t)

	tests := []struct {
		name          string
		prepare       func(ProvisionedAccount) error
		wantStatus    string
		wantSuspended string
	}{
		{
			name:       "active account returns active",
			prepare:    func(ProvisionedAccount) error { return nil },
			wantStatus: "active",
		},
		{
			name: "owner suspension is preserved",
			prepare: func(p ProvisionedAccount) error {
				return st.SuspendAccountOwner(
					ctx, p.AccountID, p.OperatorID, "owner pause",
				)
			},
			wantStatus:    "suspended",
			wantSuspended: "owner_request",
		},
		{
			name: "closed tombstone is preserved",
			prepare: func(p ProvisionedAccount) error {
				return st.CloseAccount(
					ctx, p.AccountID, p.OperatorID, "closed before move",
				)
			},
			wantStatus: "closed",
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provisioned := provisionActiveEvacuationTestAccount(
				ctx, t, st, fmt.Sprintf("abort-%d", index),
			)
			if err := test.prepare(provisioned); err != nil {
				t.Fatal(err)
			}
			evacuationID := fmt.Sprintf("evac_abort_preserve_%d", index)
			if _, err := st.BeginAccountEvacuation(
				ctx, provisioned.AccountID, evacuationID, "abort semantics",
			); err != nil {
				t.Fatal(err)
			}

			aborted, err := st.AbortAccountEvacuation(
				ctx, provisioned.AccountID, evacuationID,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !aborted.Aborted ||
				aborted.Completed ||
				aborted.Role != "source" ||
				aborted.CompletedAt == nil ||
				aborted.Status != test.wantStatus {
				t.Fatalf("abort acknowledgement = %#v", aborted)
			}
			retry, err := st.AbortAccountEvacuation(
				ctx, provisioned.AccountID, evacuationID,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !retry.Aborted ||
				retry.Completed ||
				retry.Role != "source" ||
				retry.CompletedAt == nil ||
				!retry.CompletedAt.Equal(*aborted.CompletedAt) ||
				retry.Status != test.wantStatus {
				t.Fatalf("abort retry = %#v; first = %#v", retry, aborted)
			}

			account, err := st.GetAccount(ctx, provisioned.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			if account.Status != test.wantStatus ||
				account.SuspendedFor != test.wantSuspended {
				t.Fatalf(
					"account after abort = status %q suspended_for %q",
					account.Status, account.SuspendedFor,
				)
			}
			assertAccountEvacuationReceipt(
				ctx, t, st, provisioned.AccountID, evacuationID,
				"aborted", aborted.CompletedAt,
			)
		})
	}
}

func TestCompleteAccountEvacuationPersistsRetryReceiptPostgres(t *testing.T) {
	ctx, st := openAccountEvacuationTestStore(t)
	provisioned := provisionActiveEvacuationTestAccount(ctx, t, st, "complete")
	const evacuationID = "evac_complete_receipt"

	if _, err := st.BeginAccountEvacuation(
		ctx, provisioned.AccountID, evacuationID, "completion receipt test",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CompleteAccountEvacuation(
		ctx, provisioned.AccountID, evacuationID,
	); !errors.Is(err, ErrAccountEvacuationMismatch) {
		t.Fatalf("source completion error = %v, want %v",
			err, ErrAccountEvacuationMismatch)
	}
	var archive bytes.Buffer
	if err := st.ExportAccountEvacuation(
		ctx, provisioned.AccountID, evacuationID,
		"same-cell", "test", &archive,
	); err != nil {
		t.Fatal(err)
	}
	var emptyArchive bytes.Buffer
	emptySources := make(
		[]archiveexport.RowSource, 0, len(canonicalArchiveTables),
	)
	for _, table := range canonicalArchiveTables {
		emptySources = append(
			emptySources, emptyArchiveTableSource(table.name),
		)
	}
	if err := archiveexport.Write(ctx, &emptyArchive, archiveexport.Manifest{
		SchemaVersion: SchemaVersion(),
		AccountID:     provisioned.AccountID,
		Status:        "suspended",
		EvacuationID:  evacuationID,
	}, emptySources); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ImportAccountEvacuation(
		ctx, provisioned.AccountID, evacuationID,
		bytes.NewReader(emptyArchive.Bytes()),
	); !errors.Is(err, ErrArchiveContent) {
		t.Fatalf(
			"empty same-cell archive error = %v, want ErrArchiveContent",
			err,
		)
	}
	var roleAfterInvalidArchive string
	if err := st.pool.QueryRow(ctx,
		`SELECT evacuation_role FROM accounts WHERE id = $1`,
		provisioned.AccountID,
	).Scan(&roleAfterInvalidArchive); err != nil {
		t.Fatal(err)
	}
	if roleAfterInvalidArchive != "source" {
		t.Fatalf(
			"invalid archive promoted role to %q, want source",
			roleAfterInvalidArchive,
		)
	}
	_, disposition, err := st.ImportAccountEvacuation(
		ctx, provisioned.AccountID, evacuationID,
		bytes.NewReader(archive.Bytes()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !disposition.AlreadyImported ||
		disposition.EvacuationCompleted ||
		disposition.CurrentStatus != "suspended" ||
		disposition.EvacuationRole != "target" {
		t.Fatalf("same-cell import disposition = %#v", disposition)
	}
	if _, err := st.AbortAccountEvacuation(
		ctx, provisioned.AccountID, evacuationID,
	); !errors.Is(err, ErrAccountEvacuationMismatch) {
		t.Fatalf("target abort error = %v, want %v",
			err, ErrAccountEvacuationMismatch)
	}
	if err := st.ExportAccountEvacuation(
		ctx, provisioned.AccountID, evacuationID,
		"same-cell", "test", &bytes.Buffer{},
	); !errors.Is(err, ErrAccountEvacuationMismatch) {
		t.Fatalf("target export error = %v, want %v",
			err, ErrAccountEvacuationMismatch)
	}
	if _, err := st.FinalizeAccountEvacuationSource(
		ctx, provisioned.AccountID, evacuationID,
	); !errors.Is(err, ErrAccountEvacuationMismatch) {
		t.Fatalf("target finalization error = %v, want %v",
			err, ErrAccountEvacuationMismatch)
	}
	completed, err := st.CompleteAccountEvacuation(
		ctx, provisioned.AccountID, evacuationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !completed.Completed ||
		completed.Aborted ||
		completed.Role != "target" ||
		completed.CompletedAt == nil ||
		completed.Status != "active" {
		t.Fatalf("completion acknowledgement = %#v", completed)
	}
	assertAccountEvacuationReceipt(
		ctx, t, st, provisioned.AccountID, evacuationID,
		"completed", completed.CompletedAt,
	)

	retry, err := st.CompleteAccountEvacuation(
		ctx, provisioned.AccountID, evacuationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Completed ||
		retry.Aborted ||
		retry.Role != "target" ||
		retry.CompletedAt == nil ||
		!retry.CompletedAt.Equal(*completed.CompletedAt) ||
		retry.Status != "active" {
		t.Fatalf("completion retry = %#v; first = %#v", retry, completed)
	}
	if _, err := st.AbortAccountEvacuation(
		ctx, provisioned.AccountID, evacuationID,
	); !errors.Is(err, ErrAccountEvacuationMismatch) {
		t.Fatalf(
			"abort after completion error = %v, want ErrAccountEvacuationMismatch",
			err,
		)
	}
	if _, err := st.BeginAccountEvacuation(
		ctx, provisioned.AccountID, evacuationID, "cannot reuse receipt id",
	); !errors.Is(err, ErrAccountEvacuationMismatch) {
		t.Fatalf(
			"begin after completion error = %v, want ErrAccountEvacuationMismatch",
			err,
		)
	}
	var restoredEvents int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM account_events
		 WHERE account_id = $1
		   AND verb = $2`,
		provisioned.AccountID, VerbAccountRestored,
	).Scan(&restoredEvents); err != nil {
		t.Fatal(err)
	}
	if restoredEvents != 1 {
		t.Fatalf(
			"account.restored events after exact retry = %d, want 1",
			restoredEvents,
		)
	}
}

func TestImportAccountEvacuationExactIDRetryAndGenericExistingRejectionPostgres(
	t *testing.T,
) {
	ctx, st := openAccountEvacuationTestStore(t)
	provisioned := provisionActiveEvacuationTestAccount(ctx, t, st, "import")
	const evacuationID = "evac_import_exact_id"

	if _, err := st.BeginAccountEvacuation(
		ctx, provisioned.AccountID, evacuationID, "exact import test",
	); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := st.ExportAccountEvacuation(
		ctx,
		provisioned.AccountID,
		evacuationID,
		"source-cell",
		"test",
		&archive,
	); err != nil {
		t.Fatal(err)
	}
	archiveBytes := bytes.Clone(archive.Bytes())

	if _, err := st.AbortAccountEvacuation(
		ctx, provisioned.AccountID, evacuationID,
	); err != nil {
		t.Fatal(err)
	}
	if err := deleteAccountForIntegrationTest(
		ctx, st, provisioned.AccountID,
	); err != nil {
		t.Fatal(err)
	}

	manifest, disposition, err := st.ImportAccountEvacuation(
		ctx,
		provisioned.AccountID,
		evacuationID,
		bytes.NewReader(archiveBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	if disposition != (AccountImportDisposition{EvacuationRole: "target"}) {
		t.Fatalf("first exact-id import disposition = %#v", disposition)
	}
	if manifest.AccountID != provisioned.AccountID ||
		manifest.EvacuationID != evacuationID {
		t.Fatalf("imported manifest = %#v", manifest)
	}

	retryManifest, disposition, err := st.ImportAccountEvacuation(
		ctx,
		provisioned.AccountID,
		evacuationID,
		bytes.NewReader(archiveBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !disposition.AlreadyImported ||
		disposition.EvacuationCompleted ||
		disposition.CurrentStatus != "suspended" ||
		disposition.EvacuationRole != "target" ||
		retryManifest.AccountID != manifest.AccountID ||
		retryManifest.EvacuationID != manifest.EvacuationID {
		t.Fatalf(
			"exact-id retry = manifest %#v disposition %#v",
			retryManifest, disposition,
		)
	}

	if _, err := st.ImportAccount(
		ctx, provisioned.AccountID, bytes.NewReader(archiveBytes),
	); !errors.Is(err, ErrAccountExists) {
		t.Fatalf("generic retry error = %v, want ErrAccountExists", err)
	}
	if _, _, err := st.ImportAccountEvacuation(
		ctx,
		provisioned.AccountID,
		"evac_import_competing_id",
		bytes.NewReader(archiveBytes),
	); !errors.Is(err, ErrArchiveContent) {
		t.Fatalf(
			"competing exact-id import error = %v, want ErrArchiveContent",
			err,
		)
	}

	completed, err := st.CompleteAccountEvacuation(
		ctx, provisioned.AccountID, evacuationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !completed.Completed || completed.Status != "active" {
		t.Fatalf("import completion = %#v", completed)
	}
	if err := st.CloseAccount(
		ctx, provisioned.AccountID, provisioned.OperatorID,
		"closed after completed import",
	); err != nil {
		t.Fatal(err)
	}

	completedRetryManifest, disposition, err := st.ImportAccountEvacuation(
		ctx,
		provisioned.AccountID,
		evacuationID,
		bytes.NewReader(archiveBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !disposition.AlreadyImported ||
		!disposition.EvacuationCompleted ||
		disposition.CurrentStatus != "closed" ||
		disposition.EvacuationRole != "target" ||
		completedRetryManifest.AccountID != manifest.AccountID ||
		completedRetryManifest.EvacuationID != manifest.EvacuationID {
		t.Fatalf(
			"completed exact-id retry = manifest %#v disposition %#v",
			completedRetryManifest, disposition,
		)
	}
}

func TestFinalizeAccountEvacuationSourcePurgesExactlyOnceAndProtectsLaterRestorePostgres(
	t *testing.T,
) {
	ctx, st := openAccountEvacuationTestStore(t)
	source := provisionActiveEvacuationTestAccount(ctx, t, st, "finalize")
	other := provisionActiveEvacuationTestAccount(ctx, t, st, "finalize-other")
	realm, err := st.CreateRealm(ctx, source.AccountID, "finalize-realm")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(
		ctx, source.AccountID, realm.ID, "finalize-agent",
	)
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{
		Kind: PrincipalAgent, ID: agent.ID, AccountID: source.AccountID,
		RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active",
	}
	if _, err := st.TouchAgentActivity(ctx, principal, AgentActivityInput{
		Runtime: "codex", LocationID: "finalize", Location: "source",
		Event: "SessionStart", EventID: "evt_finalize",
		EventOccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	sourceTicket, _, err := st.OpenTicket(ctx, OpenTicketInput{
		AccountID: source.AccountID, OperatorID: source.OperatorID,
		Subject: "source ticket", Body: "source ticket body",
	})
	if err != nil {
		t.Fatal(err)
	}
	otherTicket, otherMessage, err := st.OpenTicket(ctx, OpenTicketInput{
		AccountID: other.AccountID, OperatorID: other.OperatorID,
		Subject: "other ticket", Body: "other ticket body",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate latent/manual corruption that supported store paths reject: a
	// message owned by the other account points at the source account's
	// ticket through a legacy unscoped cascading FK.
	if _, err := st.pool.Exec(ctx, `
		UPDATE support_ticket_messages
		   SET ticket_id = $2
		 WHERE id = $1`,
		otherMessage.ID, sourceTicket.ID,
	); err != nil {
		t.Fatal(err)
	}

	const evacuationID = "evac_finalize_exact_source"
	const otherEvacuationID = "evac_finalize_other_source"
	if _, err := st.BeginAccountEvacuation(
		ctx, source.AccountID, evacuationID, "source finalization",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginAccountEvacuation(
		ctx, other.AccountID, otherEvacuationID, "cross-account guard",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FinalizeAccountEvacuationSource(
		ctx, source.AccountID, "evac_finalize_wrong_epoch",
	); !errors.Is(err, ErrAccountEvacuationMismatch) {
		t.Fatalf("wrong-id finalization error = %v, want %v",
			err, ErrAccountEvacuationMismatch)
	}
	if _, err := st.FinalizeAccountEvacuationSource(
		ctx, other.AccountID, evacuationID,
	); !errors.Is(err, ErrAccountEvacuationMismatch) {
		t.Fatalf("cross-account finalization error = %v, want %v",
			err, ErrAccountEvacuationMismatch)
	}
	var otherExists bool
	if err := st.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM accounts WHERE id = $1)`,
		other.AccountID,
	).Scan(&otherExists); err != nil {
		t.Fatal(err)
	}
	if !otherExists {
		t.Fatal("cross-account finalization removed the other account")
	}
	if _, err := st.AbortAccountEvacuation(
		ctx, other.AccountID, otherEvacuationID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FinalizeAccountEvacuationSource(
		ctx, source.AccountID, evacuationID,
	); !errors.Is(err, ErrAccountEvacuationIntegrity) {
		t.Fatalf(
			"cross-account cascade finalization error = %v, want %v",
			err, ErrAccountEvacuationIntegrity,
		)
	}
	var corruptedMessageAccount string
	if err := st.pool.QueryRow(ctx, `
		SELECT account_id
		  FROM support_ticket_messages
		 WHERE id = $1`,
		otherMessage.ID,
	).Scan(&corruptedMessageAccount); err != nil {
		t.Fatalf("rejected finalization deleted cross-account child: %v", err)
	}
	if corruptedMessageAccount != other.AccountID {
		t.Fatalf(
			"cross-account child owner = %q, want %q",
			corruptedMessageAccount, other.AccountID,
		)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE support_ticket_messages
		   SET ticket_id = $2
		 WHERE id = $1`,
		otherMessage.ID, otherTicket.ID,
	); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := st.ExportAccountEvacuation(
		ctx, source.AccountID, evacuationID,
		"source-cell", "test", &archive,
	); err != nil {
		t.Fatal(err)
	}
	archiveBytes := bytes.Clone(archive.Bytes())
	finalized, err := st.FinalizeAccountEvacuationSource(
		ctx, source.AccountID, evacuationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if finalized.AccountID != source.AccountID ||
		finalized.EvacuationID != evacuationID ||
		finalized.SourceStatus != "suspended" ||
		finalized.FinalizedAt.IsZero() ||
		!finalized.Finalized ||
		finalized.AlreadyFinalized {
		t.Fatalf("source finalization acknowledgement = %#v", finalized)
	}

	var accountCount, realmCount, agentCount, activityCount int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM accounts WHERE id = $1`,
		source.AccountID,
	).Scan(&accountCount); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM realms WHERE account_id = $1`,
		source.AccountID,
	).Scan(&realmCount); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM agents WHERE id = $1`,
		agent.ID,
	).Scan(&agentCount); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_activity WHERE agent_id = $1`,
		agent.ID,
	).Scan(&activityCount); err != nil {
		t.Fatal(err)
	}
	if accountCount != 0 || realmCount != 0 ||
		agentCount != 0 || activityCount != 0 {
		t.Fatalf(
			"source residue = account %d realm %d agent %d activity %d",
			accountCount, realmCount, agentCount, activityCount,
		)
	}

	retry, err := st.FinalizeAccountEvacuationSource(
		ctx, source.AccountID, evacuationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Finalized || !retry.AlreadyFinalized ||
		retry.SourceStatus != finalized.SourceStatus ||
		!retry.FinalizedAt.Equal(finalized.FinalizedAt) {
		t.Fatalf("source finalization retry = %#v; first = %#v",
			retry, finalized)
	}

	_, disposition, err := st.ImportAccountEvacuation(
		ctx, source.AccountID, evacuationID,
		bytes.NewReader(archiveBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	if disposition != (AccountImportDisposition{EvacuationRole: "target"}) {
		t.Fatalf("restored target disposition = %#v", disposition)
	}
	var restoredRole string
	if err := st.pool.QueryRow(ctx, `
		SELECT evacuation_role
		  FROM accounts
		 WHERE id = $1`,
		source.AccountID,
	).Scan(&restoredRole); err != nil {
		t.Fatal(err)
	}
	if restoredRole != "target" {
		t.Fatalf("restored role = %q, want target", restoredRole)
	}

	staleRetry, err := st.FinalizeAccountEvacuationSource(
		ctx, source.AccountID, evacuationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !staleRetry.Finalized || !staleRetry.AlreadyFinalized ||
		!staleRetry.FinalizedAt.Equal(finalized.FinalizedAt) {
		t.Fatalf("stale source finalization retry = %#v", staleRetry)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT evacuation_role
		  FROM accounts
		 WHERE id = $1`,
		source.AccountID,
	).Scan(&restoredRole); err != nil {
		t.Fatalf("stale finalization deleted restored target: %v", err)
	}
	if restoredRole != "target" {
		t.Fatalf("role after stale finalization = %q, want target", restoredRole)
	}

	completed, err := st.CompleteAccountEvacuation(
		ctx, source.AccountID, evacuationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !completed.Completed || completed.Role != "target" ||
		completed.Status != "active" {
		t.Fatalf("restored target completion = %#v", completed)
	}
	const nextEvacuationID = "evac_finalize_later_epoch"
	if _, err := st.BeginAccountEvacuation(
		ctx, source.AccountID, nextEvacuationID, "later lifecycle",
	); err != nil {
		t.Fatalf("begin after old finalization receipt: %v", err)
	}
	if _, err := st.AbortAccountEvacuation(
		ctx, source.AccountID, nextEvacuationID,
	); err != nil {
		t.Fatalf("abort later lifecycle: %v", err)
	}
}

func TestFinalizeClosedSourceRestoresAuthoritativeTombstonePostgres(
	t *testing.T,
) {
	ctx, st := openAccountEvacuationTestStore(t)
	source := provisionActiveEvacuationTestAccount(ctx, t, st, "finalize-closed")
	if err := st.CloseAccount(
		ctx, source.AccountID, source.OperatorID, "closed before evacuation",
	); err != nil {
		t.Fatal(err)
	}
	const evacuationID = "evac_finalize_closed_source"
	began, err := st.BeginAccountEvacuation(
		ctx, source.AccountID, evacuationID, "closed tombstone move",
	)
	if err != nil {
		t.Fatal(err)
	}
	if began.Status != "closed" || began.Role != "source" {
		t.Fatalf("closed begin acknowledgement = %#v", began)
	}
	var archive bytes.Buffer
	if err := st.ExportAccountEvacuation(
		ctx, source.AccountID, evacuationID,
		"closed-source", "test", &archive,
	); err != nil {
		t.Fatal(err)
	}
	finalized, err := st.FinalizeAccountEvacuationSource(
		ctx, source.AccountID, evacuationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if finalized.SourceStatus != "closed" || !finalized.Finalized {
		t.Fatalf("closed source finalization = %#v", finalized)
	}
	_, disposition, err := st.ImportAccountEvacuation(
		ctx, source.AccountID, evacuationID,
		bytes.NewReader(archive.Bytes()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if disposition.EvacuationRole != "target" {
		t.Fatalf("closed target import disposition = %#v", disposition)
	}
	completed, err := st.CompleteAccountEvacuation(
		ctx, source.AccountID, evacuationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !completed.Completed || completed.Role != "target" ||
		completed.Status != "closed" {
		t.Fatalf("closed target completion = %#v", completed)
	}
	account, err := st.GetAccount(ctx, source.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != "closed" {
		t.Fatalf("restored account status = %q, want closed", account.Status)
	}
}

func TestFinalizePurgedClosedSourcePreservesSingleErasureEventPostgres(
	t *testing.T,
) {
	ctx, st := openAccountEvacuationTestStore(t)
	source := provisionActiveEvacuationTestAccount(ctx, t, st, "finalize-purged")
	if err := st.CloseAccount(
		ctx, source.AccountID, source.OperatorID, "purged before evacuation",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE accounts
		   SET closed_at=statement_timestamp() - interval '31 days'
		 WHERE id=$1`, source.AccountID); err != nil {
		t.Fatal(err)
	}
	purged, err := st.ProcessAccountPurgeBatch(
		ctx, 100, DefaultAccountPurgeWorkerConfig().Grace,
	)
	if err != nil {
		t.Fatal(err)
	}
	if purged.PurgedAccounts != 1 || purged.AttachmentInvariantFailures != 0 {
		t.Fatalf("account purge before evacuation = %#v", purged)
	}

	const evacuationID = "evac_finalize_purged_source"
	if _, err := st.BeginAccountEvacuation(
		ctx, source.AccountID, evacuationID, "purged tombstone move",
	); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := st.ExportAccountEvacuation(
		ctx, source.AccountID, evacuationID,
		"purged-source", "test", &archive,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FinalizeAccountEvacuationSource(
		ctx, source.AccountID, evacuationID,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ImportAccountEvacuation(
		ctx, source.AccountID, evacuationID,
		bytes.NewReader(archive.Bytes()),
	); err != nil {
		t.Fatal(err)
	}
	completed, err := st.CompleteAccountEvacuation(
		ctx, source.AccountID, evacuationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !completed.Completed || completed.Status != "closed" {
		t.Fatalf("purged target completion = %#v", completed)
	}
	assertPurgedEvacuationEventSet(ctx, t, st, source.AccountID)

	retry, err := st.CompleteAccountEvacuation(
		ctx, source.AccountID, evacuationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Completed || retry.Status != "closed" {
		t.Fatalf("purged target completion retry = %#v", retry)
	}
	assertPurgedEvacuationEventSet(ctx, t, st, source.AccountID)
}

func assertPurgedEvacuationEventSet(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
) {
	t.Helper()
	var total, canonical int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (
		         WHERE actor_kind=$2
		           AND actor_id IS NULL
		           AND verb=$3
		           AND metadata='{}'::jsonb
		       )
		  FROM account_events
		 WHERE account_id=$1`,
		accountID, ActorSystem, VerbAccountPurged,
	).Scan(&total, &canonical); err != nil {
		t.Fatal(err)
	}
	if total != 1 || canonical != 1 {
		t.Fatalf(
			"purged evacuation events = total %d canonical %d, want 1/1",
			total,
			canonical,
		)
	}
}

func TestFinalizeAccountEvacuationSourceConcurrentExactRetryPostgres(
	t *testing.T,
) {
	ctx, st := openAccountEvacuationTestStore(t)
	source := provisionActiveEvacuationTestAccount(
		ctx, t, st, "finalize-concurrent",
	)
	const evacuationID = "evac_finalize_concurrent_exact"
	if _, err := st.BeginAccountEvacuation(
		ctx, source.AccountID, evacuationID, "concurrent finalization",
	); err != nil {
		t.Fatal(err)
	}

	concurrentCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	type finalizationResult struct {
		record AccountEvacuationFinalization
		err    error
	}
	start := make(chan struct{})
	results := make(chan finalizationResult, 2)
	for range 2 {
		go func() {
			<-start
			record, err := st.FinalizeAccountEvacuationSource(
				concurrentCtx, source.AccountID, evacuationID,
			)
			results <- finalizationResult{record: record, err: err}
		}()
	}
	close(start)
	first := <-results
	second := <-results
	for index, result := range []finalizationResult{first, second} {
		if result.err != nil {
			t.Fatalf("finalizer %d: %v", index+1, result.err)
		}
		if !result.record.Finalized ||
			result.record.AccountID != source.AccountID ||
			result.record.EvacuationID != evacuationID ||
			result.record.SourceStatus != "suspended" ||
			result.record.FinalizedAt.IsZero() {
			t.Fatalf(
				"finalizer %d acknowledgement = %#v",
				index+1, result.record,
			)
		}
	}
	if first.record.AlreadyFinalized == second.record.AlreadyFinalized {
		t.Fatalf(
			"concurrent finalizer retry flags = %t and %t, want one first and one retry",
			first.record.AlreadyFinalized,
			second.record.AlreadyFinalized,
		)
	}
	if !first.record.FinalizedAt.Equal(second.record.FinalizedAt) {
		t.Fatalf(
			"concurrent finalizer receipt times = %s and %s",
			first.record.FinalizedAt,
			second.record.FinalizedAt,
		)
	}
}

func TestAccountEvacuationMigrationCoversCanonicalArchiveTablesPostgres(
	t *testing.T,
) {
	ctx, st := openAccountEvacuationTestStore(t)

	for _, archiveTable := range canonicalArchiveTables {
		archiveTable := archiveTable
		t.Run(archiveTable.name, func(t *testing.T) {
			wantFunction := "witself_tenant_evacuation_fence"
			switch archiveTable.name {
			case "accounts":
				wantFunction = "witself_account_evacuation_fence"
			case "agents":
				wantFunction = "witself_agents_evacuation_fence"
			case "agent_activity":
				wantFunction = "witself_agent_activity_evacuation_fence"
			}

			var count int
			var functionName string
			if err := st.pool.QueryRow(ctx, `
				SELECT count(*), coalesce(max(p.proname), '')
				  FROM pg_trigger trigger
				  JOIN pg_class relation
				    ON relation.oid = trigger.tgrelid
				  JOIN pg_namespace namespace
				    ON namespace.oid = relation.relnamespace
				  JOIN pg_proc p
				    ON p.oid = trigger.tgfoid
				 WHERE namespace.nspname = 'public'
				   AND relation.relname = $1
				   AND trigger.tgname = 'account_evacuation_fence'
				   AND NOT trigger.tgisinternal`,
				archiveTable.name,
			).Scan(&count, &functionName); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf(
					"account_evacuation_fence trigger count = %d, want 1",
					count,
				)
			}
			if functionName != wantFunction {
				t.Fatalf(
					"trigger function = %q, want %q",
					functionName, wantFunction,
				)
			}
		})
	}
}

func TestAccountEvacuationMigrationDowngradePostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}

	t.Run("schema 71 empty state can downgrade", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 71)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 70)
		assertMigrationTestTable(
			t, st, "account_evacuation_finalizations", false,
		)
		assertMigrationTestColumn(
			t, st, "accounts", "evacuation_role", false,
		)
	})

	t.Run("schema 71 active role refuses downgrade", func(t *testing.T) {
		ctx := context.Background()
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 71)
		account, err := st.ProvisionAccount(
			ctx, "down-active-role@witwave.ai", "Down Active Role",
			time.Hour,
		)
		if err != nil {
			t.Fatal(err)
		}
		if activated, err := st.ActivateAccount(
			ctx, account.AccountID,
		); err != nil || !activated {
			t.Fatalf("activate = %t / %v", activated, err)
		}
		const evacuationID = "evac_down_active_role"
		if _, err := st.BeginAccountEvacuation(
			ctx, account.AccountID, evacuationID, "downgrade guard",
		); err != nil {
			t.Fatal(err)
		}

		downErr := migrationTestDown(t, dsn, true)
		if downErr == nil || !strings.Contains(
			downErr.Error(), "evacuations are active",
		) {
			t.Fatalf("schema-71 active downgrade error = %v", downErr)
		}
		assertMigrationTestVersion(t, dsn, 71)
		var marker, role string
		if err := st.pool.QueryRow(ctx, `
			SELECT evacuation_id, evacuation_role
			  FROM accounts
			 WHERE id = $1`,
			account.AccountID,
		).Scan(&marker, &role); err != nil {
			t.Fatal(err)
		}
		if marker != evacuationID || role != "source" {
			t.Fatalf(
				"authority after refused downgrade = %q/%q",
				marker, role,
			)
		}
	})

	t.Run("schema 71 finalization receipt refuses downgrade", func(t *testing.T) {
		ctx := context.Background()
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 71)
		account, err := st.ProvisionAccount(
			ctx, "down-finalized@witwave.ai", "Down Finalized",
			time.Hour,
		)
		if err != nil {
			t.Fatal(err)
		}
		if activated, err := st.ActivateAccount(
			ctx, account.AccountID,
		); err != nil || !activated {
			t.Fatalf("activate = %t / %v", activated, err)
		}
		const evacuationID = "evac_down_finalized"
		if _, err := st.BeginAccountEvacuation(
			ctx, account.AccountID, evacuationID, "downgrade guard",
		); err != nil {
			t.Fatal(err)
		}
		if _, err := st.FinalizeAccountEvacuationSource(
			ctx, account.AccountID, evacuationID,
		); err != nil {
			t.Fatal(err)
		}

		downErr := migrationTestDown(t, dsn, true)
		if downErr == nil || !strings.Contains(
			downErr.Error(), "finalization receipts exist",
		) {
			t.Fatalf(
				"schema-71 receipt downgrade error = %v",
				downErr,
			)
		}
		assertMigrationTestVersion(t, dsn, 71)
		var receipts int
		if err := st.pool.QueryRow(ctx, `
			SELECT count(*)
			  FROM account_evacuation_finalizations
			 WHERE account_id = $1
			   AND evacuation_id = $2`,
			account.AccountID, evacuationID,
		).Scan(&receipts); err != nil {
			t.Fatal(err)
		}
		if receipts != 1 {
			t.Fatalf(
				"finalization receipts after refused downgrade = %d",
				receipts,
			)
		}
	})

	t.Run("schema 84 down history can finalize source", func(t *testing.T) {
		ctx := context.Background()
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 85)
		migrationTestDownTo(t, dsn, 84)
		assertMigrationTestVersion(t, dsn, 84)
		// Preserve the history shape used by Goose variants that retain an
		// explicit unapplied record for the migration that was rolled back.
		// Reading the newest row as the live schema would incorrectly treat the
		// now-absent schema-85 alias table as present.
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO goose_db_version(version_id, is_applied)
			VALUES (85, false)`); err != nil {
			t.Fatal(err)
		}

		var latestVersion int64
		var latestApplied bool
		if err := st.pool.QueryRow(ctx, `
			SELECT version_id, is_applied
			  FROM goose_db_version
			 ORDER BY id DESC
			 LIMIT 1`).Scan(&latestVersion, &latestApplied); err != nil {
			t.Fatal(err)
		}
		if latestVersion != 85 || latestApplied {
			t.Fatalf(
				"latest migration history = version %d applied=%t; want version 85 applied=false",
				latestVersion, latestApplied,
			)
		}

		account, err := st.ProvisionAccount(
			ctx, "down-history-finalize@witwave.ai",
			"Down History Finalize", time.Hour,
		)
		if err != nil {
			t.Fatal(err)
		}
		if activated, err := st.ActivateAccount(
			ctx, account.AccountID,
		); err != nil || !activated {
			t.Fatalf("activate = %t / %v", activated, err)
		}
		const evacuationID = "evac_down_history_finalize"
		if _, err := st.BeginAccountEvacuation(
			ctx, account.AccountID, evacuationID, "down history guard",
		); err != nil {
			t.Fatal(err)
		}
		finalized, err := st.FinalizeAccountEvacuationSource(
			ctx, account.AccountID, evacuationID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !finalized.Finalized || finalized.AlreadyFinalized {
			t.Fatalf("source finalization = %+v", finalized)
		}
	})

	t.Run("schema 70 empty state can downgrade", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 70)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 69)
		assertMigrationTestColumn(
			t, st, "accounts", "evacuation_id", false,
		)
		assertMigrationTestColumn(
			t, st, "accounts", "last_evacuation_id", false,
		)
	})

	for _, test := range []struct {
		name          string
		active        bool
		wantErrorText string
	}{
		{
			name:          "active marker refuses schema 70 downgrade",
			active:        true,
			wantErrorText: "evacuation state exists",
		},
		{
			name:          "completion receipt refuses schema 70 downgrade",
			active:        false,
			wantErrorText: "evacuation state exists",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			st, dsn := newMigrationTestStore(t, baseDSN)
			migrationTestUpTo(t, dsn, 70)
			account, err := st.ProvisionAccount(
				ctx, "schema70-"+strings.ReplaceAll(
					test.name, " ", "-",
				)+"@witwave.ai",
				"Schema 70 Downgrade",
				time.Hour,
			)
			if err != nil {
				t.Fatal(err)
			}
			const evacuationID = "evac_schema70_down_guard"
			if test.active {
				tx, err := st.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(ctx) }()
				if err := setEvacuationAuthorityTx(
					ctx, tx, evacuationID,
				); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, `
					UPDATE accounts
					   SET evacuation_id = $2,
					       evacuation_started_at = clock_timestamp()
					 WHERE id = $1`,
					account.AccountID, evacuationID,
				); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			} else if _, err := st.pool.Exec(ctx, `
				UPDATE accounts
				   SET last_evacuation_id = $2,
				       last_evacuation_completed_at = clock_timestamp(),
				       last_evacuation_outcome = 'completed'
				 WHERE id = $1`,
				account.AccountID, evacuationID,
			); err != nil {
				t.Fatal(err)
			}

			downErr := migrationTestDown(t, dsn, true)
			if downErr == nil || !strings.Contains(
				downErr.Error(), test.wantErrorText,
			) {
				t.Fatalf(
					"schema-70 downgrade error = %v",
					downErr,
				)
			}
			assertMigrationTestVersion(t, dsn, 70)
			var currentID, lastID *string
			if err := st.pool.QueryRow(ctx, `
				SELECT evacuation_id, last_evacuation_id
				  FROM accounts
				 WHERE id = $1`,
				account.AccountID,
			).Scan(&currentID, &lastID); err != nil {
				t.Fatal(err)
			}
			if test.active {
				if currentID == nil || *currentID != evacuationID {
					t.Fatalf(
						"active marker after refused downgrade = %v",
						currentID,
					)
				}
			} else if lastID == nil || *lastID != evacuationID {
				t.Fatalf(
					"completion receipt after refused downgrade = %v",
					lastID,
				)
			}
		})
	}
}

func openAccountEvacuationTestStore(t *testing.T) (context.Context, *Store) {
	t.Helper()
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	return ctx, st
}

func provisionActiveEvacuationTestAccount(
	ctx context.Context,
	t *testing.T,
	st *Store,
	label string,
) ProvisionedAccount {
	t.Helper()
	suffix := time.Now().UnixNano()
	provisioned, err := st.ProvisionAccount(
		ctx,
		fmt.Sprintf("evacuation-%s-%d@witwave.ai", label, suffix),
		fmt.Sprintf("evacuation %s %d", label, suffix),
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupAccountEvacuationTestAccount(
			context.Background(), t, st, provisioned.AccountID,
		)
	})
	if activated, err := st.ActivateAccount(
		ctx, provisioned.AccountID,
	); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	return provisioned
}

func cleanupAccountEvacuationTestAccount(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
) {
	t.Helper()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Errorf("begin evacuation test cleanup: %v", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var evacuationID *string
	if err := tx.QueryRow(
		ctx,
		`SELECT evacuation_id FROM accounts WHERE id = $1`,
		accountID,
	).Scan(&evacuationID); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("read evacuation test account during cleanup: %v", err)
			return
		}
		// A finalization test may deliberately leave only its cell-local
		// receipt. Remove that test artifact after the behavior is asserted.
		if _, err := tx.Exec(ctx,
			`DELETE FROM account_evacuation_finalizations WHERE account_id = $1`,
			accountID,
		); err != nil {
			t.Errorf("delete evacuation finalization receipt during cleanup: %v", err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			t.Errorf("commit evacuation receipt cleanup: %v", err)
		}
		return
	}
	if evacuationID != nil {
		if err := setEvacuationAuthorityTx(ctx, tx, *evacuationID); err != nil {
			t.Errorf("set evacuation cleanup authority: %v", err)
			return
		}
		if _, err := tx.Exec(ctx, `
			UPDATE accounts
			   SET evacuation_id = NULL,
			       evacuation_started_at = NULL,
			       evacuation_role = NULL,
			       last_evacuation_id = $2,
			       last_evacuation_completed_at = clock_timestamp(),
			       last_evacuation_outcome = 'aborted'
			 WHERE id = $1`,
			accountID, *evacuationID,
		); err != nil {
			t.Errorf("clear evacuation marker during cleanup: %v", err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Errorf("commit evacuation test cleanup: %v", err)
		return
	}
	if err := deleteAccountForIntegrationTest(ctx, st, accountID); err != nil {
		t.Errorf("delete evacuation test account: %v", err)
		return
	}
	if _, err := st.pool.Exec(ctx,
		`DELETE FROM account_evacuation_finalizations WHERE account_id = $1`,
		accountID,
	); err != nil {
		t.Errorf("delete evacuation finalization receipt during cleanup: %v", err)
	}
}

func assertAccountEvacuationFenceError(t *testing.T, err error) {
	t.Helper()
	assertPostgresCode(t, err, "55006")
}

func assertPostgresCode(t *testing.T, err error, want string) {
	t.Helper()
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) {
		t.Fatalf("mutation error = %T %v, want PostgreSQL error", err, err)
	}
	if postgresError.Code != want {
		t.Fatalf(
			"mutation SQLSTATE = %q (%v), want %s",
			postgresError.Code, err, want,
		)
	}
}

func assertAccountEvacuationReceipt(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID, evacuationID, outcome string,
	completedAt *time.Time,
) {
	t.Helper()
	var currentID, currentRole *string
	var lastID, lastOutcome string
	var lastCompletedAt time.Time
	if err := st.pool.QueryRow(ctx, `
		SELECT evacuation_id, evacuation_role, last_evacuation_id,
		       last_evacuation_completed_at, last_evacuation_outcome
		  FROM accounts
		 WHERE id = $1`,
		accountID,
	).Scan(
		&currentID, &currentRole, &lastID, &lastCompletedAt, &lastOutcome,
	); err != nil {
		t.Fatal(err)
	}
	if currentID != nil {
		t.Fatalf("active evacuation marker = %q, want nil", *currentID)
	}
	if currentRole != nil {
		t.Fatalf("active evacuation role = %q, want nil", *currentRole)
	}
	if lastID != evacuationID || lastOutcome != outcome {
		t.Fatalf(
			"last evacuation receipt = id %q outcome %q, want %q/%q",
			lastID, lastOutcome, evacuationID, outcome,
		)
	}
	if completedAt == nil || !lastCompletedAt.Equal(*completedAt) {
		t.Fatalf(
			"last evacuation completed_at = %s, acknowledgement = %v",
			lastCompletedAt, completedAt,
		)
	}
}
