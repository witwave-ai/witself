package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
)

func TestSecretRetainedLimitBoundaryArchiveDeleteReplayAndOverLimitReads(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	p, _ := newSecretLimitPrincipal(ctx, t, st)
	setSecretLimitPlan(ctx, t, st, p.AccountID, 1, 2)

	status, err := st.GetSecretLimitStatus(ctx, p)
	if err != nil || status.Used != 0 || status.Max == nil || *status.Max != 2 ||
		status.Remaining == nil || *status.Remaining != 2 || status.Unlimited || status.OverLimit {
		t.Fatalf("initial status = %#v / %v", status, err)
	}

	firstInput := secretLimitCreateInput(t, "first", "create-first")
	first, err := st.CreateSecret(ctx, p, firstInput)
	if err != nil {
		t.Fatal(err)
	}
	archived, err := st.ArchiveSecret(ctx, p, first.Secret.ID, SecretLifecycleInput{
		ExpectedRowVersion: first.Secret.RowVersion, IdempotencyKey: "archive-first",
	})
	if err != nil {
		t.Fatal(err)
	}
	secondInput := secretLimitCreateInput(t, "second", "create-second")
	second, err := st.CreateSecret(ctx, p, secondInput)
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := st.CreateSecret(ctx, p, firstInput); err != nil ||
		!replay.Receipt.Replayed || replay.Secret.ID != first.Secret.ID {
		t.Fatalf("create replay at cap = %#v / %v", replay, err)
	}
	if _, err := st.CreateSecret(ctx, p, secretLimitCreateInput(t, "blocked", "create-blocked")); !errors.Is(err, ErrPlanLimitReached) {
		t.Fatalf("boundary create error = %v", err)
	} else {
		var limitErr *SecretLimitError
		if !errors.As(err, &limitErr) || limitErr.Status.Used != 2 {
			t.Fatalf("typed boundary error = %#v", err)
		}
	}

	deleted, err := st.DeleteSecret(ctx, p, archived.Secret.ID, SecretLifecycleInput{
		ExpectedRowVersion: archived.Secret.RowVersion, IdempotencyKey: "delete-first",
	})
	if err != nil || deleted.Secret.Lifecycle != SecretLifecycleDeleted ||
		deleted.Secret.DeletedAt == nil {
		t.Fatalf("delete = %#v / %v", deleted, err)
	}
	replayedDelete, err := st.DeleteSecret(ctx, p, archived.Secret.ID, SecretLifecycleInput{
		ExpectedRowVersion: archived.Secret.RowVersion, IdempotencyKey: "delete-first",
	})
	if err != nil || !replayedDelete.Receipt.Replayed ||
		replayedDelete.Secret.Lifecycle != SecretLifecycleDeleted {
		t.Fatalf("delete replay = %#v / %v", replayedDelete, err)
	}
	if _, err := st.GetSecret(ctx, p, archived.Secret.ID); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("ordinary get after delete = %v", err)
	}
	status, err = st.GetSecretLimitStatus(ctx, p)
	if err != nil || status.Used != 1 || status.Remaining == nil || *status.Remaining != 1 {
		t.Fatalf("status after delete = %#v / %v", status, err)
	}
	third, err := st.CreateSecret(ctx, p, secretLimitCreateInput(t, "third", "create-third"))
	if err != nil {
		t.Fatalf("create after delete = %v", err)
	}
	archivedSecond, err := st.ArchiveSecret(ctx, p, second.Secret.ID, SecretLifecycleInput{
		ExpectedRowVersion: second.Secret.RowVersion, IdempotencyKey: "archive-second",
	})
	if err != nil {
		t.Fatal(err)
	}

	setSecretLimitPlan(ctx, t, st, p.AccountID, 2, 1)
	status, err = st.GetSecretLimitStatus(ctx, p)
	if err != nil || status.Used != 2 || !status.OverLimit ||
		status.Remaining == nil || *status.Remaining != 0 {
		t.Fatalf("over-limit status = %#v / %v", status, err)
	}
	archivedPage, err := st.ListSecrets(ctx, p, SecretListOptions{
		Lifecycle: SecretLifecycleArchived, IncludeFields: true,
	})
	if err != nil || len(archivedPage.Secrets) != 1 ||
		archivedPage.Secrets[0].ID != second.Secret.ID {
		t.Fatalf("archived read while over limit = %#v / %v", archivedPage, err)
	}
	if _, err := st.GetSecret(ctx, p, third.Secret.ID); err != nil {
		t.Fatalf("second read while over limit = %v", err)
	}
	if restored, err := st.RestoreSecret(ctx, p, second.Secret.ID, SecretLifecycleInput{
		ExpectedRowVersion: archivedSecond.Secret.RowVersion, IdempotencyKey: "restore-over-limit",
	}); err != nil || restored.Secret.Lifecycle != SecretLifecycleActive {
		t.Fatalf("restore while over limit = %#v / %v", restored, err)
	}
}

func TestSecretRetainedLimitZeroAndMissingUnlimited(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	p, _ := newSecretLimitPrincipal(ctx, t, st)
	status, err := st.GetSecretLimitStatus(ctx, p)
	if err != nil || !status.Unlimited || status.Max != nil || status.Remaining != nil {
		t.Fatalf("unlimited status = %#v / %v", status, err)
	}
	setSecretLimitPlan(ctx, t, st, p.AccountID, 1, 0)
	if _, err := st.CreateSecret(ctx, p, secretLimitCreateInput(t, "zero", "create-zero")); !errors.Is(err, ErrPlanLimitReached) {
		t.Fatalf("zero-limit create error = %v", err)
	}
	status, err = st.GetSecretLimitStatus(ctx, p)
	if err != nil || status.Unlimited || status.Max == nil || *status.Max != 0 ||
		status.Remaining == nil || *status.Remaining != 0 || status.OverLimit {
		t.Fatalf("zero-limit status = %#v / %v", status, err)
	}
}

func TestSecretRetainedLimitConcurrentCreatesAcrossReplicasAndOwners(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	p, peer := newSecretLimitPrincipal(ctx, t, st)
	setSecretLimitPlan(ctx, t, st, p.AccountID, 1, 5)
	replica, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replica.Close)

	for index := 0; index < 4; index++ {
		if _, err := st.CreateSecret(ctx, p,
			secretLimitCreateInput(t, fmt.Sprintf("prefill-%02d", index), fmt.Sprintf("prefill-key-%02d", index))); err != nil {
			t.Fatal(err)
		}
	}
	inputs := make([]CreateSecretInput, 20)
	for index := range inputs {
		inputs[index] = secretLimitCreateInput(t,
			fmt.Sprintf("race-%02d", index), fmt.Sprintf("race-key-%02d", index))
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes, refusals := 0, 0
	for index := 0; index < 20; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			target := st
			if index%2 == 1 {
				target = replica
			}
			_, err := target.CreateSecret(ctx, p, inputs[index])
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrPlanLimitReached):
				refusals++
			default:
				t.Errorf("concurrent create %d: %v", index, err)
			}
		}(index)
	}
	wg.Wait()
	if successes != 1 || refusals != 19 {
		t.Fatalf("concurrent results successes=%d refusals=%d", successes, refusals)
	}

	// The cap is per owner rather than account-wide: another agent in the same
	// account has an independent five-secret allowance.
	if _, err := replica.CreateSecret(ctx, peer,
		secretLimitCreateInput(t, "peer-first", "peer-first-key")); err != nil {
		t.Fatalf("different owner create = %v", err)
	}
}

func TestSecretDeleteReceiptMigrationDowngradeWithoutDeleteReceipts(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	p, _ := newSecretLimitPrincipal(ctx, t, st)
	if _, err := st.CreateSecret(ctx, p,
		secretLimitCreateInput(t, "retained before down", "retained-before-down")); err != nil {
		t.Fatal(err)
	}
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 66)
	for _, name := range []string{
		"secret_mutation_receipts_operation_check",
		"secret_mutation_receipts_check",
	} {
		definition := secretReceiptConstraintDefinition(ctx, t, st, name)
		if strings.Contains(definition, "secret_delete") {
			t.Fatalf("legacy constraint %s still accepts secret_delete: %s", name, definition)
		}
	}
}

func TestSecretDeleteReceiptMigrationDowngradeRefusesCommittedDelete(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	p, _ := newSecretLimitPrincipal(ctx, t, st)
	created, err := st.CreateSecret(ctx, p,
		secretLimitCreateInput(t, "delete before down", "delete-before-down-create"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteSecret(ctx, p, created.Secret.ID, SecretLifecycleInput{
		ExpectedRowVersion: created.Secret.RowVersion,
		IdempotencyKey:     "delete-before-down",
	}); err != nil {
		t.Fatal(err)
	}
	downErr := migrationTestDown(t, dsn, true)
	if downErr == nil || !strings.Contains(downErr.Error(),
		"delete receipts exist") {
		t.Fatalf("downgrade error = %v", downErr)
	}
	assertMigrationTestVersion(t, dsn, int64(SchemaVersion()))
	for _, name := range []string{
		"secret_mutation_receipts_operation_check",
		"secret_mutation_receipts_check",
	} {
		definition := secretReceiptConstraintDefinition(ctx, t, st, name)
		if !strings.Contains(definition, "secret_delete") {
			t.Fatalf("refused downgrade changed constraint %s: %s", name, definition)
		}
	}
	var receipts int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM secret_mutation_receipts
		 WHERE operation='secret_delete'`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 {
		t.Fatalf("delete receipts after refused downgrade = %d, want 1", receipts)
	}
}

func newSecretLimitPrincipal(ctx context.Context, t *testing.T, st *Store) (Principal, Principal) {
	t.Helper()
	account, err := st.ProvisionAccount(ctx,
		fmt.Sprintf("secret-limit-%d@witwave.ai", time.Now().UnixNano()), "secret limits", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "peer")
	if err != nil {
		t.Fatal(err)
	}
	principal := func(agent Agent) Principal {
		return Principal{
			Kind: PrincipalAgent, ID: agent.ID, AccountID: account.AccountID,
			RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active",
			AccessProfile: AccessProfileFull,
		}
	}
	return principal(agent), principal(peer)
}

func setSecretLimitPlan(ctx context.Context, t *testing.T, st *Store, accountID string, revision, limit int64) {
	t.Helper()
	limits := map[string]int64{plans.StoredSecretLimit: limit}
	hash, err := plans.SnapshotHash("test", limits, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAccountPlan(ctx, accountID, revision, hash, "test",
		limits, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func secretLimitCreateInput(t *testing.T, name, retryKey string) CreateSecretInput {
	t.Helper()
	value := "public"
	return CreateSecretInput{
		ID: mustSecretTestID(t, "sec"), Name: name, Template: "generic",
		IdempotencyKey: retryKey,
		Fields: []CreateSecretFieldInput{{
			ID: mustSecretTestID(t, "fld"), Name: "value", Kind: SecretFieldText,
			Encoding: SecretEncodingUTF8, ValueVersion: 1, PublicValue: &value,
		}},
	}
}

func secretReceiptConstraintDefinition(ctx context.Context, t *testing.T, st *Store, name string) string {
	t.Helper()
	var definition string
	if err := st.pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid)
		  FROM pg_constraint
		 WHERE conrelid='secret_mutation_receipts'::regclass AND conname=$1`,
		name).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	return definition
}
