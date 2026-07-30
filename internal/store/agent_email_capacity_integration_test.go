package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemail"
	"github.com/witwave-ai/witself/internal/plans"
)

func TestAgentEmailStorageCapacityLifecyclePostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	fixture := newAgentEmailCapacityFixture(ctx, t, st, "lifecycle")
	fitRaw := agentEmailCapacityRaw(fixture.address.Address, "fit")
	overflowRaw := agentEmailCapacityRaw(fixture.address.Address, "overflow")
	rejectedRaw := agentEmailCapacityRaw(fixture.address.Address, "raw-rejected")
	maximumRaw := int64(len(fitRaw))
	for _, raw := range [][]byte{overflowRaw, rejectedRaw} {
		if int64(len(raw)) > maximumRaw {
			maximumRaw = int64(len(raw))
		}
	}
	fitCapacity := int64(len(fitRaw))

	setAgentEmailCapacityPlan(
		ctx, t, st, fixture.accountID, 1, &maximumRaw, &fitCapacity,
	)
	assertAgentEmailCapacityStatus(
		ctx, t, st, fixture.owner,
		maximumRaw, 0, &fitCapacity, false, false, false,
	)

	fit, err := ingestAgentEmailCapacity(
		ctx, st, fixture.scope, fixture.address.Address, fitRaw,
	)
	if err != nil {
		t.Fatal(err)
	}
	if fit.PayloadRetentionState != AgentEmailPayloadRetained ||
		fit.AttachmentCount != 1 ||
		fit.AttachmentStorageBytes != fitCapacity ||
		fit.RetainedAttachmentStorageBytes != fitCapacity {
		t.Fatalf("fitting attachment result = %#v", fit)
	}
	assertAgentEmailCapacityInvariant(ctx, t, st, fixture.accountID, fitCapacity)
	assertAgentEmailCapacityStatus(
		ctx, t, st, fixture.owner,
		maximumRaw, fitCapacity, &fitCapacity, false, true, false,
	)

	// A missing attachment-capacity key is the canonical unlimited shape,
	// including when the account already retains attachment-bearing mail.
	setAgentEmailCapacityPlan(
		ctx, t, st, fixture.accountID, 2, &maximumRaw, nil,
	)
	assertAgentEmailCapacityStatus(
		ctx, t, st, fixture.owner,
		maximumRaw, fitCapacity, nil, true, false, false,
	)

	// Lowering the account beneath current usage is allowed and reported
	// honestly. It blocks only future physical growth; it never evicts mail.
	overCapacity := fitCapacity - 1
	setAgentEmailCapacityPlan(
		ctx, t, st, fixture.accountID, 3, &maximumRaw, &overCapacity,
	)
	assertAgentEmailCapacityStatus(
		ctx, t, st, fixture.owner,
		maximumRaw, fitCapacity, &overCapacity, false, false, true,
	)

	overflow, err := ingestAgentEmailCapacity(
		ctx, st, fixture.scope, fixture.address.Address, overflowRaw,
	)
	if err != nil {
		t.Fatalf("capacity overflow should be accepted: %v", err)
	}
	if overflow.PayloadRetentionState != AgentEmailPayloadOmittedCapacity ||
		overflow.AttachmentCount != 1 ||
		overflow.AttachmentStorageBytes != int64(len(overflowRaw)) ||
		overflow.RetainedAttachmentStorageBytes != 0 {
		t.Fatalf("overflow attachment result = %#v", overflow)
	}
	var rawWasOmitted bool
	if err := st.pool.QueryRow(ctx, `
		SELECT raw_mime IS NULL
		  FROM agent_email_messages
		 WHERE id=$1`,
		overflow.ID,
	).Scan(&rawWasOmitted); err != nil {
		t.Fatal(err)
	}
	if !rawWasOmitted {
		t.Fatal("capacity-omitted message retained raw MIME")
	}
	read, err := st.ReadAgentEmail(
		ctx, fixture.scope, fixture.owner, overflow.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if read.PayloadRetentionState != AgentEmailPayloadOmittedCapacity ||
		read.TextKind != "text/plain" ||
		!strings.Contains(read.Text, "bounded body overflow") {
		t.Fatalf("capacity-omitted message was not readable: %#v", read)
	}
	assertAgentEmailCapacityInvariant(ctx, t, st, fixture.accountID, fitCapacity)

	rawLimit := int64(len(rejectedRaw) - 1)
	setAgentEmailCapacityPlan(
		ctx, t, st, fixture.accountID, 4, &rawLimit, &overCapacity,
	)
	var messagesBefore int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_messages WHERE account_id=$1`,
		fixture.accountID,
	).Scan(&messagesBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := ingestAgentEmailCapacity(
		ctx, st, fixture.scope, fixture.address.Address, rejectedRaw,
	); err == nil {
		t.Fatal("message above the account raw-MIME cap was accepted")
	} else {
		if !errors.Is(err, ErrPlanLimitReached) {
			t.Fatalf("raw-MIME cap error = %v, want ErrPlanLimitReached", err)
		}
		var detail *PlanLimitError
		if !errors.As(err, &detail) ||
			detail.Dimension != plans.AgentEmailMaxRawBytesLimit ||
			detail.Used != int64(len(rejectedRaw)) ||
			detail.Max != rawLimit {
			t.Fatalf("raw-MIME cap detail = %#v / %v", detail, err)
		}
	}
	var messagesAfter int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_messages WHERE account_id=$1`,
		fixture.accountID,
	).Scan(&messagesAfter); err != nil {
		t.Fatal(err)
	}
	if messagesAfter != messagesBefore {
		t.Fatalf("raw-MIME refusal stored a row: before=%d after=%d",
			messagesBefore, messagesAfter)
	}

	// The database trigger maintains the derived account projection on the
	// same deletion path used by retention and mailbox cascades.
	if command, err := st.pool.Exec(ctx, `
		DELETE FROM agent_email_messages WHERE account_id=$1 AND id=$2`,
		fixture.accountID, fit.ID,
	); err != nil {
		t.Fatal(err)
	} else if command.RowsAffected() != 1 {
		t.Fatalf("deleted fitting message rows = %d", command.RowsAffected())
	}
	assertAgentEmailCapacityInvariant(ctx, t, st, fixture.accountID, 0)
	assertAgentEmailCapacityStatus(
		ctx, t, st, fixture.owner,
		rawLimit, 0, &overCapacity, false, false, false,
	)
}

func TestAgentEmailStorageCapacityConcurrentIngestCannotOvershootPostgres(
	t *testing.T,
) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	replica, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replica.Close)

	fixture := newAgentEmailCapacityFixture(ctx, t, st, "concurrent")
	rawA := agentEmailCapacityRaw(fixture.address.Address, "race-a")
	rawB := agentEmailCapacityRaw(fixture.address.Address, "race-b")
	if len(rawA) != len(rawB) {
		t.Fatalf("concurrency fixtures have different sizes: %d != %d",
			len(rawA), len(rawB))
	}
	maximumRaw := int64(len(rawA))
	capacity := maximumRaw
	setAgentEmailCapacityPlan(
		ctx, t, st, fixture.accountID, 1, &maximumRaw, &capacity,
	)

	type ingestResult struct {
		message AgentEmailMessage
		err     error
	}
	start := make(chan struct{})
	results := make(chan ingestResult, 2)
	raceCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for _, input := range []struct {
		store *Store
		raw   []byte
	}{
		{store: st, raw: rawA},
		{store: replica, raw: rawB},
	} {
		go func(input struct {
			store *Store
			raw   []byte
		}) {
			<-start
			message, ingestErr := ingestAgentEmailCapacity(
				raceCtx,
				input.store,
				fixture.scope,
				fixture.address.Address,
				input.raw,
			)
			results <- ingestResult{message: message, err: ingestErr}
		}(input)
	}
	close(start)

	retained, omitted := 0, 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent ingest error = %v", result.err)
		}
		switch result.message.PayloadRetentionState {
		case AgentEmailPayloadRetained:
			retained++
			if result.message.RetainedAttachmentStorageBytes != capacity {
				t.Fatalf("retained concurrent message = %#v", result.message)
			}
		case AgentEmailPayloadOmittedCapacity:
			omitted++
			if result.message.RetainedAttachmentStorageBytes != 0 {
				t.Fatalf("omitted concurrent message = %#v", result.message)
			}
		default:
			t.Fatalf("unexpected concurrent payload state = %#v", result.message)
		}
	}
	if retained != 1 || omitted != 1 {
		t.Fatalf("concurrent results retained=%d omitted=%d", retained, omitted)
	}
	assertAgentEmailCapacityInvariant(ctx, t, st, fixture.accountID, capacity)
	assertAgentEmailCapacityStatus(
		ctx, t, st, fixture.owner,
		maximumRaw, capacity, &capacity, false, true, false,
	)
}

type agentEmailCapacityFixture struct {
	accountID string
	owner     Principal
	scope     AgentEmailPilotScope
	address   AgentEmailAddress
}

func newAgentEmailCapacityFixture(
	ctx context.Context,
	t *testing.T,
	st *Store,
	suffix string,
) agentEmailCapacityFixture {
	t.Helper()
	account, err := st.ProvisionAccount(
		ctx,
		fmt.Sprintf("agent-email-capacity-%s-%d@witwave.ai", suffix, time.Now().UnixNano()),
		"agent email capacity "+suffix,
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if active, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !active {
		t.Fatalf("activate = %t / %v", active, err)
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "capacity "+suffix)
	if err != nil {
		t.Fatal(err)
	}
	var owner Agent
	enrolled := make(map[string]bool, 5)
	for index := range 5 {
		agent, err := st.CreateAgent(
			ctx, account.AccountID, realm.ID,
			fmt.Sprintf("%s agent %d", suffix, index+1),
		)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			owner = agent
		}
		enrolled[agent.ID] = true
	}
	scope := AgentEmailPilotScope{
		Enabled:  true,
		Domain:   "agent-mail.witwave.ai",
		Audience: "capacity-" + suffix,
		RealmIDs: map[string]bool{realm.ID: true},
		AgentIDs: enrolled,
	}
	address, err := st.EnsureAgentEmailMailbox(
		ctx, scope, account.AccountID, realm.ID, owner.ID, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	return agentEmailCapacityFixture{
		accountID: account.AccountID,
		owner: Principal{
			Kind: PrincipalAgent, ID: owner.ID, AccountID: account.AccountID,
			RealmID: realm.ID, AgentName: owner.Name, AccountStatus: "active",
		},
		scope: scope, address: address,
	}
}

func setAgentEmailCapacityPlan(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
	revision int64,
	maximumRaw, attachmentCapacity *int64,
) {
	t.Helper()
	limits := make(map[string]int64, 2)
	if maximumRaw != nil {
		limits[plans.AgentEmailMaxRawBytesLimit] = *maximumRaw
	}
	if attachmentCapacity != nil {
		limits[plans.AgentEmailAttachmentStorageBytesLimit] = *attachmentCapacity
	}
	policies := map[string]int64{
		plans.AgentEmailEntitlementVersionPolicy: plans.AgentEmailEntitlementVersion,
		plans.AgentEmailRetentionDaysPolicy:      90,
	}
	features := []string{plans.AgentEmailReceiveFeature}
	hash, err := plans.SnapshotHash(
		"test", limits, policies, features,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAccountPlan(
		ctx, accountID, revision, hash, "test",
		limits, policies, features,
	); err != nil {
		t.Fatal(err)
	}
}

func ingestAgentEmailCapacity(
	ctx context.Context,
	st *Store,
	scope AgentEmailPilotScope,
	recipient string,
	raw []byte,
) (AgentEmailMessage, error) {
	digest := sha256.Sum256(raw)
	return st.IngestAgentEmailPilot(ctx, scope, AgentEmailIngestInput{
		Relay: agentemail.RelayMetadata{
			Timestamp:         time.Now().Unix(),
			KeyID:             "capacity-test",
			Audience:          scope.Audience,
			EnvelopeSender:    "sender@example.com",
			EnvelopeRecipient: recipient,
			RawSize:           int64(len(raw)),
			RawSHA256:         hex.EncodeToString(digest[:]),
		},
		Raw: raw,
	})
}

func agentEmailCapacityRaw(recipient, token string) []byte {
	boundary := "capacity-" + token
	return []byte(strings.Join([]string{
		"From: Sender <sender@example.com>",
		"To: " + recipient,
		"Subject: capacity " + token,
		"Message-ID: <capacity-" + token + "@example.com>",
		"MIME-Version: 1.0",
		"Content-Type: multipart/mixed; boundary=" + boundary,
		"",
		"--" + boundary,
		"Content-Type: text/plain; charset=utf-8",
		"",
		"bounded body " + token,
		"--" + boundary,
		"Content-Type: application/octet-stream; name=sample.bin",
		"Content-Disposition: attachment; filename=sample.bin",
		"Content-Transfer-Encoding: base64",
		"",
		"YXR0YWNobWVudC1ieXRlcw==",
		"--" + boundary + "--",
		"",
	}, "\r\n"))
}

func assertAgentEmailCapacityInvariant(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
	want int64,
) {
	t.Helper()
	var projected, derived int64
	if err := st.pool.QueryRow(ctx, `
		SELECT account.retained_agent_email_attachment_bytes,
		       COALESCE((
		         SELECT sum(message.retained_attachment_storage_bytes)
		           FROM agent_email_messages message
		          WHERE message.account_id=account.id
		       ),0)::BIGINT
		  FROM accounts account
		 WHERE account.id=$1`,
		accountID,
	).Scan(&projected, &derived); err != nil {
		t.Fatal(err)
	}
	if projected != want || derived != want {
		t.Fatalf("attachment capacity projection=%d derived=%d want=%d",
			projected, derived, want)
	}
}

func assertAgentEmailCapacityStatus(
	ctx context.Context,
	t *testing.T,
	st *Store,
	owner Principal,
	wantMaximumRaw, wantUsed int64,
	wantMax *int64,
	wantUnlimited, wantAtLimit, wantOverLimit bool,
) {
	t.Helper()
	status, err := st.GetAgentEmailStorageStatus(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if status.MaximumRawBytes != wantMaximumRaw {
		t.Fatalf("maximum raw bytes=%d want=%d: %#v",
			status.MaximumRawBytes, wantMaximumRaw, status)
	}
	capacity := status.AttachmentCapacity
	if capacity.Used != wantUsed ||
		capacity.Unlimited != wantUnlimited ||
		capacity.AtLimit != wantAtLimit ||
		capacity.OverLimit != wantOverLimit {
		t.Fatalf(
			"attachment status=%#v, want used=%d unlimited=%t at=%t over=%t",
			capacity, wantUsed, wantUnlimited, wantAtLimit, wantOverLimit,
		)
	}
	if wantMax == nil {
		if capacity.Max != nil || capacity.Remaining != nil {
			t.Fatalf("unlimited attachment status exposed cap fields: %#v", capacity)
		}
		return
	}
	if capacity.Max == nil || *capacity.Max != *wantMax ||
		capacity.Remaining == nil {
		t.Fatalf("attachment cap fields=%#v want max=%d", capacity, *wantMax)
	}
	wantRemaining := *wantMax - wantUsed
	if wantRemaining < 0 {
		wantRemaining = 0
	}
	if *capacity.Remaining != wantRemaining {
		t.Fatalf("attachment remaining=%d want=%d: %#v",
			*capacity.Remaining, wantRemaining, capacity)
	}
}
