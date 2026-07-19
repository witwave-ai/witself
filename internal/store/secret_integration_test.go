package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/sealed"
)

func TestAgentOwnedSecretPostgresAndArchiveRoundTrip(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	source, _ := newMigrationTestStore(t, baseDSN)
	destination, _ := newMigrationTestStore(t, baseDSN)
	if err := source.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := destination.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := source.ProvisionAccount(ctx,
		"sealed-vault@witwave.ai", "sealed vault", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := source.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := source.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := source.CreateAgent(ctx, provisioned.AccountID, realm.ID, "scott")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := source.CreateAgent(ctx, provisioned.AccountID, realm.ID, "peer")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active", AccessProfile: AccessProfileFull}
	peerPrincipal := Principal{Kind: PrincipalAgent, ID: peer.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: peer.Name, AccountStatus: "active", AccessProfile: AccessProfileFull}
	testConcurrentSecretVaultKeyRegistration(ctx, t, source, provisioned.AccountID, realm.ID)

	avk, err := sealed.GenerateAgentVaultKey(sealed.InitialAgentVaultKeyVersion)
	if err != nil {
		t.Fatal(err)
	}
	metadata := avk.Metadata()
	registered, receipt, err := source.RegisterVaultKey(ctx, p, RegisterVaultKeyInput{
		ID: metadata.ID, KeyVersion: int64(metadata.Version), Algorithm: metadata.Algorithm,
		Fingerprint: metadata.Fingerprint, IdempotencyKey: "register-vault-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if registered.Fingerprint != metadata.Fingerprint || receipt.Replayed {
		t.Fatalf("registered key = %#v / receipt %#v", registered, receipt)
	}
	replayedKey, replayedReceipt, err := source.RegisterVaultKey(ctx, p, RegisterVaultKeyInput{
		ID: metadata.ID, KeyVersion: int64(metadata.Version), Algorithm: metadata.Algorithm,
		Fingerprint: metadata.Fingerprint, IdempotencyKey: "register-vault-key",
	})
	if err != nil || replayedKey.ID != registered.ID || !replayedReceipt.Replayed {
		t.Fatalf("replayed key = %#v / %#v / %v", replayedKey, replayedReceipt, err)
	}
	if got, err := source.GetCurrentVaultKey(ctx, p); err != nil || got == nil || got.ID != metadata.ID {
		t.Fatalf("current key = %#v / %v", got, err)
	}
	if got, err := source.GetCurrentVaultKey(ctx, peerPrincipal); err != nil || got != nil {
		t.Fatalf("peer current key = %#v / %v", got, err)
	}

	secretID := mustSecretTestID(t, "sec")
	usernameFieldID := mustSecretTestID(t, "fld")
	passwordFieldID := mustSecretTestID(t, "fld")
	scope := sealed.FieldScope{
		Domain: sealed.FieldValueDomain, AccountID: p.AccountID, RealmID: p.RealmID,
		OwnerAgentID: p.ID, SecretID: secretID, FieldID: passwordFieldID,
	}
	const password = "violet-orbit-secret-924"
	envelope, err := sealed.SealSensitiveField(avk, []byte(password), sealed.SensitiveFieldOptions{
		Scope: scope, ValueVersion: 1, DEKGeneration: 1,
		ValueEncoding: sealed.ValueEncodingUTF8, WrapRevision: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	username := "scottbot"
	create := CreateSecretInput{
		ID: secretID, Name: "GitHub account", Description: "Agent login",
		Template: "login", Tags: []string{"github", "developer"},
		IdempotencyKey: "create-github-secret",
		Fields: []CreateSecretFieldInput{
			{ID: usernameFieldID, Name: "username", Kind: SecretFieldUsername,
				Sensitive: false, Encoding: SecretEncodingUTF8, ValueVersion: 1, PublicValue: &username},
			{ID: passwordFieldID, Name: "password", Kind: SecretFieldPassword,
				Sensitive: true, Encoding: SecretEncodingUTF8, ValueVersion: int64(envelope.ValueVersion),
				Sealed: secretStoreEnvelope(envelope)},
		},
	}
	created, err := source.CreateSecret(ctx, p, create)
	if err != nil {
		t.Fatal(err)
	}
	passwordField := created.Secret.Fields[0]
	if passwordField.ID != passwordFieldID {
		passwordField = created.Secret.Fields[1]
	}
	if created.Secret.SensitiveCount != 1 || passwordField.PublicValue != nil ||
		!passwordField.Redacted || created.Receipt.Replayed {
		t.Fatalf("created redacted secret = %#v", created)
	}
	replayed, err := source.CreateSecret(ctx, p, create)
	if err != nil || !replayed.Receipt.Replayed || replayed.Secret.ID != secretID {
		t.Fatalf("replayed secret = %#v / %v", replayed, err)
	}
	conflicting := create
	conflicting.Description = "different request"
	if _, err := source.CreateSecret(ctx, p, conflicting); !errors.Is(err, ErrSecretIdempotencyConflict) {
		t.Fatalf("idempotency conflict error = %v", err)
	}
	duplicateName := CreateSecretInput{
		ID: mustSecretTestID(t, "sec"), Name: create.Name, Template: "login",
		IdempotencyKey: "duplicate-live-name",
		Fields: []CreateSecretFieldInput{{
			ID: mustSecretTestID(t, "fld"), Name: "username", Kind: SecretFieldUsername,
			Encoding: SecretEncodingUTF8, ValueVersion: 1, PublicValue: &username,
		}},
	}
	if _, err := source.CreateSecret(ctx, p, duplicateName); !errors.Is(err, ErrSecretConflict) {
		t.Fatalf("duplicate live name error = %v", err)
	}

	if _, err := source.GetSecret(ctx, peerPrincipal, secretID); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("peer get error = %v", err)
	}
	otherRealm, err := source.CreateRealm(ctx, p.AccountID, "other")
	if err != nil {
		t.Fatal(err)
	}
	otherRealmAgent, err := source.CreateAgent(ctx, p.AccountID, otherRealm.ID, "other-realm-agent")
	if err != nil {
		t.Fatal(err)
	}
	otherRealmPrincipal := Principal{Kind: PrincipalAgent, ID: otherRealmAgent.ID,
		AccountID: p.AccountID, RealmID: otherRealm.ID, AccountStatus: "active", AccessProfile: AccessProfileFull}
	if _, err := source.GetSecret(ctx, otherRealmPrincipal, secretID); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("other-realm get error = %v", err)
	}
	forgedAccountPrincipal := p
	forgedAccountPrincipal.AccountID = "acc_aaaaaaaaaaaaaaaa"
	if _, err := source.GetSecret(ctx, forgedAccountPrincipal, secretID); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("cross-account get error = %v", err)
	}
	publicSearch, err := source.ListSecrets(ctx, p, SecretListOptions{Query: username, IncludeFields: true})
	if err != nil || len(publicSearch.Secrets) != 1 || publicSearch.Secrets[0].ID != secretID {
		t.Fatalf("public search = %#v / %v", publicSearch, err)
	}
	sealedSearch, err := source.ListSecrets(ctx, p, SecretListOptions{Query: password})
	if err != nil || len(sealedSearch.Secrets) != 0 {
		t.Fatalf("ciphertext search leaked a match = %#v / %v", sealedSearch, err)
	}

	material, err := source.AccessSecretField(ctx, p, secretID, passwordFieldID,
		AccessSecretFieldInput{IdempotencyKey: "read-password"})
	if err != nil {
		t.Fatal(err)
	}
	cleartext, err := sealed.OpenSensitiveField(avk, scope, secretEnvelopeFromMaterial(material))
	if err != nil || string(cleartext) != password {
		t.Fatalf("opened material = %q / %v", cleartext, err)
	}
	clear(cleartext)
	if _, err := source.AccessSecretField(ctx, peerPrincipal, secretID, passwordFieldID,
		AccessSecretFieldInput{IdempotencyKey: "peer-read"}); !errors.Is(err, ErrSecretFieldNotFound) {
		t.Fatalf("peer access error = %v", err)
	}
	testPeerSecretUsageIdempotencyNamespace(ctx, t, source, p, peerPrincipal,
		secretID, passwordFieldID, create.IdempotencyKey, "read-password")
	if _, err := source.pool.Exec(ctx, `
		CREATE FUNCTION reject_secret_read_usage() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
		  IF NEW.dimension = 'secret_read' THEN
		    RAISE EXCEPTION 'forced usage projection failure';
		  END IF;
		  RETURN NEW;
		END $$;
		CREATE TRIGGER reject_secret_read_usage
		BEFORE INSERT ON usage_events FOR EACH ROW
		EXECUTE FUNCTION reject_secret_read_usage()`); err != nil {
		t.Fatal(err)
	}
	if got, err := source.AccessSecretField(ctx, p, secretID, passwordFieldID,
		AccessSecretFieldInput{IdempotencyKey: "read-despite-metering-failure"}); err != nil || !bytes.Equal(got.Ciphertext, material.Ciphertext) {
		t.Fatalf("access with metering failure = %#v / %v", got, err)
	}
	var accessReceiptCount, failedUsageCount int
	if err := source.pool.QueryRow(ctx, `
		SELECT count(*) FROM secret_mutation_receipts
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND operation='field_access' AND target_id=$4`,
		p.AccountID, p.RealmID, p.ID, passwordFieldID).Scan(&accessReceiptCount); err != nil {
		t.Fatal(err)
	}
	if err := source.pool.QueryRow(ctx, `
		SELECT count(*) FROM usage_events
		 WHERE account_id=$1 AND idempotency_key=$2`, p.AccountID,
		"secret-access:"+p.ID+":"+secretIdempotencyKeyHash("read-despite-metering-failure")).Scan(&failedUsageCount); err != nil {
		t.Fatal(err)
	}
	if accessReceiptCount != 2 || failedUsageCount != 0 {
		t.Fatalf("durable access receipts / failed usage = %d / %d", accessReceiptCount, failedUsageCount)
	}
	if _, err := source.pool.Exec(ctx, `
		DROP TRIGGER reject_secret_read_usage ON usage_events;
		DROP FUNCTION reject_secret_read_usage()`); err != nil {
		t.Fatal(err)
	}
	archiveInput := SecretLifecycleInput{
		ExpectedRowVersion: created.Secret.RowVersion,
		IdempotencyKey:     "archive-github-secret",
	}
	archived, err := source.ArchiveSecret(ctx, p, secretID, archiveInput)
	if err != nil || archived.Secret.Lifecycle != SecretLifecycleArchived ||
		archived.Secret.RowVersion != created.Secret.RowVersion+1 ||
		archived.Receipt.ResultRevision != archived.Secret.RowVersion || archived.Receipt.Replayed {
		t.Fatalf("archived secret = %#v / %v", archived, err)
	}
	archiveReplay, err := source.ArchiveSecret(ctx, p, secretID, archiveInput)
	if err != nil || !archiveReplay.Receipt.Replayed ||
		archiveReplay.Secret.RowVersion != archived.Secret.RowVersion ||
		archiveReplay.Secret.Lifecycle != SecretLifecycleArchived {
		t.Fatalf("archive replay = %#v / %v", archiveReplay, err)
	}
	if _, err := source.ArchiveSecret(ctx, p, secretID, SecretLifecycleInput{
		ExpectedRowVersion: created.Secret.RowVersion,
		IdempotencyKey:     "archive-github-stale-revision",
	}); !errors.Is(err, ErrSecretConflict) {
		t.Fatalf("stale archive revision error = %v", err)
	}
	if _, err := source.ArchiveSecret(ctx, peerPrincipal, secretID, SecretLifecycleInput{
		ExpectedRowVersion: archived.Secret.RowVersion,
		IdempotencyKey:     "peer-archive-github",
	}); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("peer archive error = %v", err)
	}
	if _, err := source.ArchiveSecret(ctx, otherRealmPrincipal, secretID, SecretLifecycleInput{
		ExpectedRowVersion: archived.Secret.RowVersion,
		IdempotencyKey:     "other-realm-archive-github",
	}); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("other-realm archive error = %v", err)
	}
	if page, err := source.ListSecrets(ctx, p, SecretListOptions{}); err != nil || len(page.Secrets) != 0 {
		t.Fatalf("ordinary list included archived secret = %#v / %v", page, err)
	}
	if page, err := source.ListSecrets(ctx, p, SecretListOptions{Lifecycle: SecretLifecycleArchived}); err != nil || len(page.Secrets) != 1 || page.Secrets[0].ID != secretID {
		t.Fatalf("archived list = %#v / %v", page, err)
	}
	if _, err := source.GetSecret(ctx, p, secretID); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("ordinary get archived error = %v", err)
	}
	if replayed, err := source.CreateSecret(ctx, p, create); err != nil ||
		!replayed.Receipt.Replayed || replayed.Secret.Lifecycle != SecretLifecycleArchived {
		t.Fatalf("delayed create replay after archive = %#v / %v", replayed, err)
	}
	if _, err := source.AccessSecretField(ctx, p, secretID, passwordFieldID,
		AccessSecretFieldInput{IdempotencyKey: "read-archived"}); !errors.Is(err, ErrSecretFieldNotFound) {
		t.Fatalf("ordinary access archived error = %v", err)
	}
	if _, err := source.RestoreSecret(ctx, p, secretID, SecretLifecycleInput{
		ExpectedRowVersion: archived.Secret.RowVersion + 1,
		IdempotencyKey:     "restore-github-wrong-revision",
	}); !errors.Is(err, ErrSecretConflict) {
		t.Fatalf("wrong restore revision error = %v", err)
	}
	if _, err := source.RestoreSecret(ctx, peerPrincipal, secretID, SecretLifecycleInput{
		ExpectedRowVersion: archived.Secret.RowVersion,
		IdempotencyKey:     "peer-restore-github",
	}); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("peer restore error = %v", err)
	}
	if _, err := source.RestoreSecret(ctx, otherRealmPrincipal, secretID, SecretLifecycleInput{
		ExpectedRowVersion: archived.Secret.RowVersion,
		IdempotencyKey:     "other-realm-restore-github",
	}); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("other-realm restore error = %v", err)
	}

	// Archiving frees the byte-exact live-name slot. Restoring fails closed
	// while a replacement occupies it, and the same failed retry key remains
	// usable after the conflicting secret is itself archived.
	replacement, err := source.CreateSecret(ctx, p, duplicateName)
	if err != nil || replacement.Secret.Name != create.Name {
		t.Fatalf("replacement with archived name = %#v / %v", replacement, err)
	}
	restoreInput := SecretLifecycleInput{
		ExpectedRowVersion: archived.Secret.RowVersion,
		IdempotencyKey:     "restore-github-secret",
	}
	if _, err := source.RestoreSecret(ctx, p, secretID, restoreInput); !errors.Is(err, ErrSecretConflict) {
		t.Fatalf("restore live-name conflict error = %v", err)
	}
	replacementArchived, err := source.ArchiveSecret(ctx, p, replacement.Secret.ID,
		SecretLifecycleInput{ExpectedRowVersion: replacement.Secret.RowVersion,
			IdempotencyKey: "archive-replacement-secret"})
	if err != nil || replacementArchived.Secret.Lifecycle != SecretLifecycleArchived {
		t.Fatalf("archive replacement = %#v / %v", replacementArchived, err)
	}
	restored, err := source.RestoreSecret(ctx, p, secretID, restoreInput)
	if err != nil || restored.Secret.Lifecycle != SecretLifecycleActive ||
		restored.Secret.RowVersion != archived.Secret.RowVersion+1 ||
		restored.Receipt.ResultRevision != restored.Secret.RowVersion || restored.Receipt.Replayed {
		t.Fatalf("restored secret = %#v / %v", restored, err)
	}
	restoreReplay, err := source.RestoreSecret(ctx, p, secretID, restoreInput)
	if err != nil || !restoreReplay.Receipt.Replayed ||
		restoreReplay.Secret.RowVersion != restored.Secret.RowVersion ||
		restoreReplay.Secret.Lifecycle != SecretLifecycleActive {
		t.Fatalf("restore replay = %#v / %v", restoreReplay, err)
	}
	if page, err := source.ListSecrets(ctx, p, SecretListOptions{}); err != nil ||
		len(page.Secrets) != 1 || page.Secrets[0].ID != secretID {
		t.Fatalf("ordinary list after restore = %#v / %v", page, err)
	}
	if page, err := source.ListSecrets(ctx, p,
		SecretListOptions{Lifecycle: SecretLifecycleArchived}); err != nil ||
		len(page.Secrets) != 1 || page.Secrets[0].ID != replacement.Secret.ID {
		t.Fatalf("archived list after restore = %#v / %v", page, err)
	}

	var unsafeAuditRows int
	if err := source.pool.QueryRow(ctx, `
		SELECT count(*) FROM account_events
		 WHERE account_id=$1 AND metadata::text LIKE '%' || $2 || '%'`,
		p.AccountID, password).Scan(&unsafeAuditRows); err != nil {
		t.Fatal(err)
	}
	if unsafeAuditRows != 0 {
		t.Fatalf("secret material appeared in %d audit rows", unsafeAuditRows)
	}

	// Prove archive encoding is independent of the role/session bytea_output
	// setting, then perform a real move into a separately migrated schema.
	setSecretTestPoolByteaOutput(ctx, t, source.pool, "escape")
	if err := source.SuspendAccountSystem(ctx, p.AccountID, "evacuation", "sealed vault archive round trip"); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := source.ExportAccount(ctx, p.AccountID, "source-cell", "test", &archive); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(archive.Bytes(), []byte(password)) {
		t.Fatal("archive contains sensitive plaintext")
	}
	if _, err := destination.ImportAccount(ctx, p.AccountID, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}
	if err := destination.ResumeAccountSystem(ctx, p.AccountID, "evacuation"); err != nil {
		t.Fatal(err)
	}
	imported, err := destination.GetSecret(ctx, p, secretID)
	if err != nil || imported.ID != secretID || imported.SensitiveCount != 1 {
		t.Fatalf("imported secret = %#v / %v", imported, err)
	}
	importedMaterial, err := destination.AccessSecretField(ctx, p, secretID, passwordFieldID,
		AccessSecretFieldInput{IdempotencyKey: "read-imported-password"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(importedMaterial.Ciphertext, material.Ciphertext) ||
		!bytes.Equal(importedMaterial.DEK.WrappedDEK, material.DEK.WrappedDEK) {
		t.Fatal("cell move changed ciphertext or wrapped DEK")
	}
	cleartext, err = sealed.OpenSensitiveField(avk, scope, secretEnvelopeFromMaterial(importedMaterial))
	if err != nil || string(cleartext) != password {
		t.Fatalf("opened imported material = %q / %v", cleartext, err)
	}
	clear(cleartext)
}

func testPeerSecretUsageIdempotencyNamespace(
	ctx context.Context,
	t *testing.T,
	st *Store,
	first, peer Principal,
	firstSecretID, firstFieldID, createRetryKey, accessRetryKey string,
) {
	t.Helper()
	peerAVK, err := sealed.GenerateAgentVaultKey(sealed.InitialAgentVaultKeyVersion)
	if err != nil {
		t.Fatal(err)
	}
	metadata := peerAVK.Metadata()
	if _, _, err := st.RegisterVaultKey(ctx, peer, RegisterVaultKeyInput{
		ID: metadata.ID, KeyVersion: int64(metadata.Version), Algorithm: metadata.Algorithm,
		Fingerprint: metadata.Fingerprint, IdempotencyKey: "register-peer-vault-key",
	}); err != nil {
		t.Fatal(err)
	}

	peerSecretID := mustSecretTestID(t, "sec")
	peerPasswordFieldID := mustSecretTestID(t, "fld")
	peerTokenFieldID := mustSecretTestID(t, "fld")
	seal := func(fieldID, value string) (*SealedFieldInput, sealed.FieldScope) {
		t.Helper()
		scope := sealed.FieldScope{
			Domain: sealed.FieldValueDomain, AccountID: peer.AccountID, RealmID: peer.RealmID,
			OwnerAgentID: peer.ID, SecretID: peerSecretID, FieldID: fieldID,
		}
		envelope, err := sealed.SealSensitiveField(peerAVK, []byte(value), sealed.SensitiveFieldOptions{
			Scope: scope, ValueVersion: 1, DEKGeneration: 1,
			ValueEncoding: sealed.ValueEncodingUTF8, WrapRevision: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		return secretStoreEnvelope(envelope), scope
	}
	peerPassword, peerPasswordScope := seal(peerPasswordFieldID, "peer-violet-orbit-password")
	peerToken, _ := seal(peerTokenFieldID, "peer-violet-orbit-token")
	created, err := st.CreateSecret(ctx, peer, CreateSecretInput{
		ID: peerSecretID, Name: "GitHub account", Description: "Peer agent login",
		Template: "login", Tags: []string{"github"}, IdempotencyKey: createRetryKey,
		Fields: []CreateSecretFieldInput{
			{ID: peerPasswordFieldID, Name: "password", Kind: SecretFieldPassword,
				Sensitive: true, Encoding: SecretEncodingUTF8, ValueVersion: 1, Sealed: peerPassword},
			{ID: peerTokenFieldID, Name: "token", Kind: SecretFieldToken,
				Sensitive: true, Encoding: SecretEncodingUTF8, ValueVersion: 1, Sealed: peerToken},
		},
	})
	if err != nil || created.Secret.ID != peerSecretID {
		t.Fatalf("peer create with shared retry key = %#v / %v", created, err)
	}
	material, err := st.AccessSecretField(ctx, peer, peerSecretID, peerPasswordFieldID,
		AccessSecretFieldInput{IdempotencyKey: accessRetryKey})
	if err != nil {
		t.Fatalf("peer access with shared retry key = %v", err)
	}
	cleartext, err := sealed.OpenSensitiveField(peerAVK, peerPasswordScope,
		secretEnvelopeFromMaterial(material))
	if err != nil || string(cleartext) != "peer-violet-orbit-password" {
		t.Fatalf("open peer material = %q / %v", cleartext, err)
	}
	clear(cleartext)
	if _, err := st.AccessSecretField(ctx, peer, peerSecretID, peerTokenFieldID,
		AccessSecretFieldInput{IdempotencyKey: accessRetryKey}); !errors.Is(err, ErrSecretIdempotencyConflict) {
		t.Fatalf("same-agent cross-field retry reuse error = %v", err)
	}

	checks := []struct {
		dimension     string
		firstSubject  string
		secondSubject string
	}{
		{UsageDimensionStoredSecret, firstSecretID, peerSecretID},
		{UsageDimensionEncryptedStorage, firstSecretID, peerSecretID},
		{UsageDimensionSecretRead, firstFieldID, peerPasswordFieldID},
	}
	for _, check := range checks {
		var eventCount, agentCount int
		if err := st.pool.QueryRow(ctx, `
			SELECT count(*), count(DISTINCT agent_id)
			  FROM usage_events
			 WHERE account_id=$1 AND dimension=$2
			   AND ((agent_id=$3 AND subject_id=$4) OR
			        (agent_id=$5 AND subject_id=$6))`, first.AccountID,
			check.dimension, first.ID, check.firstSubject, peer.ID,
			check.secondSubject).Scan(&eventCount, &agentCount); err != nil {
			t.Fatal(err)
		}
		if eventCount != 2 || agentCount != 2 {
			t.Fatalf("%s usage rows/agents = %d/%d, want 2/2",
				check.dimension, eventCount, agentCount)
		}
	}
}

func testConcurrentSecretVaultKeyRegistration(ctx context.Context, t *testing.T, st *Store, accountID, realmID string) {
	t.Helper()
	newPrincipal := func(name string) Principal {
		agent, err := st.CreateAgent(ctx, accountID, realmID, name)
		if err != nil {
			t.Fatal(err)
		}
		return Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: accountID,
			RealmID: realmID, AgentName: agent.Name, AccountStatus: "active", AccessProfile: AccessProfileFull}
	}
	registerConcurrently := func(p Principal, inputs []RegisterVaultKeyInput) []error {
		start := make(chan struct{})
		errs := make([]error, len(inputs))
		var wait sync.WaitGroup
		wait.Add(len(inputs))
		for index := range inputs {
			go func(index int) {
				defer wait.Done()
				<-start
				_, _, errs[index] = st.RegisterVaultKey(ctx, p, inputs[index])
			}(index)
		}
		close(start)
		wait.Wait()
		return errs
	}

	samePrincipal := newPrincipal("concurrent-same-key")
	sameKey, err := sealed.GenerateAgentVaultKey(1)
	if err != nil {
		t.Fatal(err)
	}
	sameMetadata := sameKey.Metadata()
	sameErrors := registerConcurrently(samePrincipal, []RegisterVaultKeyInput{
		{ID: sameMetadata.ID, KeyVersion: 1, Algorithm: sameMetadata.Algorithm,
			Fingerprint: sameMetadata.Fingerprint, IdempotencyKey: "concurrent-same-a"},
		{ID: sameMetadata.ID, KeyVersion: 1, Algorithm: sameMetadata.Algorithm,
			Fingerprint: sameMetadata.Fingerprint, IdempotencyKey: "concurrent-same-b"},
	})
	if sameErrors[0] != nil || sameErrors[1] != nil {
		t.Fatalf("identical concurrent registrations = %v", sameErrors)
	}

	conflictPrincipal := newPrincipal("concurrent-conflicting-key")
	firstKey, err := sealed.GenerateAgentVaultKey(1)
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := sealed.GenerateAgentVaultKey(1)
	if err != nil {
		t.Fatal(err)
	}
	firstMetadata, secondMetadata := firstKey.Metadata(), secondKey.Metadata()
	conflictErrors := registerConcurrently(conflictPrincipal, []RegisterVaultKeyInput{
		{ID: firstMetadata.ID, KeyVersion: 1, Algorithm: firstMetadata.Algorithm,
			Fingerprint: firstMetadata.Fingerprint, IdempotencyKey: "concurrent-conflict-a"},
		{ID: secondMetadata.ID, KeyVersion: 1, Algorithm: secondMetadata.Algorithm,
			Fingerprint: secondMetadata.Fingerprint, IdempotencyKey: "concurrent-conflict-b"},
	})
	successes, conflicts := 0, 0
	for _, err := range conflictErrors {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrVaultKeyConflict):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent conflict error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("conflicting concurrent registrations = %v", conflictErrors)
	}
}

func setSecretTestPoolByteaOutput(ctx context.Context, t *testing.T, pool *pgxpool.Pool, value string) {
	t.Helper()
	connections := make([]*pgxpool.Conn, 0, pool.Config().MaxConns)
	defer func() {
		for _, connection := range connections {
			connection.Release()
		}
	}()
	for range pool.Config().MaxConns {
		connection, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, connection)
		if _, err := connection.Exec(ctx, `SELECT set_config('bytea_output',$1,false)`, value); err != nil {
			t.Fatal(err)
		}
	}
}

func mustSecretTestID(t *testing.T, prefix string) string {
	t.Helper()
	value, err := id.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func secretStoreEnvelope(envelope sealed.SensitiveFieldEnvelope) *SealedFieldInput {
	return &SealedFieldInput{
		EnvelopeVersion: int64(envelope.EnvelopeVersion), Ciphertext: envelope.Ciphertext,
		Algorithm: envelope.AEADAlgorithm, AADVersion: int64(envelope.AADVersion),
		DEK: SealedDEKInput{
			ID: envelope.DEKID, Generation: int64(envelope.DEKGeneration),
			WrappedDEK: envelope.WrappedDEK, WrapAlgorithm: envelope.WrapAlgorithm,
			AADVersion: int64(envelope.AADVersion), WrapRevision: int64(envelope.WrapRevision),
			WrappingKeyID: envelope.WrappingKeyID, WrappingKeyVersion: int64(envelope.WrappingKeyVersion),
		},
	}
}

func secretEnvelopeFromMaterial(material SecretMaterial) sealed.SensitiveFieldEnvelope {
	return sealed.SensitiveFieldEnvelope{
		EnvelopeVersion: uint32(material.EnvelopeVersion), AADVersion: uint32(material.AADVersion),
		Ciphertext: material.Ciphertext, AEADAlgorithm: material.Algorithm,
		ValueEncoding: material.Encoding, ValueVersion: uint64(material.ValueVersion),
		DEKID: material.DEK.ID, DEKGeneration: uint64(material.DEK.Generation),
		WrappedDEK: material.DEK.WrappedDEK, WrapAlgorithm: material.DEK.WrapAlgorithm,
		WrapRevision: uint64(material.DEK.WrapRevision), WrappingKeyID: material.DEK.WrappingKeyID,
		WrappingKeyVersion: uint64(material.DEK.WrappingKeyVersion),
	}
}

func TestSecretCursorRejectsTrailingJSON(t *testing.T) {
	encoded, err := encodeSecretCursor(secretCursor{
		UpdatedAt: time.Date(2026, 7, 18, 22, 0, 0, 0, time.UTC),
		ID:        "sec_aaaaaaaaaaaaaaaa",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeSecretCursor(encoded); err != nil {
		t.Fatalf("valid cursor: %v", err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	withTrailing := base64.RawURLEncoding.EncodeToString(append(decoded, []byte(` {}`)...))
	if _, err := decodeSecretCursor(withTrailing); !errors.Is(err, ErrSecretInputInvalid) {
		t.Fatalf("trailing JSON cursor error = %v", err)
	}
}

func TestSecretMaterialFingerprintBindsVersionAndEnvelope(t *testing.T) {
	base := SecretMaterial{
		SecretID: "sec_aaaaaaaaaaaaaaaa", FieldID: "fld_bbbbbbbbbbbbbbbb",
		Encoding: SecretEncodingUTF8, ValueVersion: 1, EnvelopeVersion: 1,
		Ciphertext: []byte("ciphertext"), Algorithm: SecretAEADAlgorithm, AADVersion: 1,
		DEK: SealedDEKInput{ID: "dek_cccccccccccccccc", Generation: 1,
			WrappedDEK: []byte("wrapped"), WrapAlgorithm: SecretAEADAlgorithm,
			AADVersion: 1, WrapRevision: 1, WrappingKeyID: "avk_dddddddddddddddd",
			WrappingKeyVersion: 1},
		SecretRevision: 1, FieldRevision: 1,
	}
	want := secretMaterialFingerprint(base)
	if got := secretMaterialFingerprint(base); got != want {
		t.Fatalf("fingerprint is not deterministic: %q != %q", got, want)
	}
	changed := base
	changed.FieldRevision++
	if got := secretMaterialFingerprint(changed); got == want {
		t.Fatal("field revision was not bound into material fingerprint")
	}
	changed = base
	changed.Ciphertext = append([]byte(nil), base.Ciphertext...)
	changed.Ciphertext[0] ^= 1
	if got := secretMaterialFingerprint(changed); got == want {
		t.Fatal("ciphertext was not bound into material fingerprint")
	}
}
