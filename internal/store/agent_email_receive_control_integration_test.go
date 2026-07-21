package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemail"
)

func TestAgentEmailReceiveControlsRemainIndependentAndRealmDisableSurvivesZeroMailboxes(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx,
		"agent-email-receive-controls@witwave.ai", "agent email receive controls", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "receive controls")
	if err != nil {
		t.Fatal(err)
	}
	makeScope := func(prefix string) (AgentEmailPilotScope, []Agent) {
		t.Helper()
		agents := make([]Agent, 0, 5)
		enrolled := make(map[string]bool, 5)
		for _, suffix := range []string{"a", "b", "c", "d", "e"} {
			agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID,
				prefix+" agent "+suffix)
			if err != nil {
				t.Fatal(err)
			}
			agents = append(agents, agent)
			enrolled[agent.ID] = true
		}
		return AgentEmailPilotScope{
			Enabled: true, Domain: "agent-mail.witwave.ai", Audience: "cell-control-1",
			RealmIDs: map[string]bool{realm.ID: true}, AgentIDs: enrolled,
		}, agents
	}
	scope, agents := makeScope("first")
	addresses, err := st.ReconcileAgentEmailPilot(ctx, scope)
	if err != nil || len(addresses) != 5 {
		t.Fatalf("reconcile = %#v / %v", addresses, err)
	}
	operatorID := provisioned.OperatorID
	owner := Principal{
		Kind: PrincipalAgent, ID: agents[0].ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: agents[0].Name, AccountStatus: "active",
	}

	// Exercise the public mutations concurrently. Both serialize through the
	// account safety-write lock and update only their independent layer.
	type agentControlResult struct {
		control AgentEmailReceiveControl
		err     error
	}
	type realmControlResult struct {
		control AgentEmailRealmReceiveControl
		err     error
	}
	agentResults := make(chan agentControlResult, 1)
	realmResults := make(chan realmControlResult, 1)
	startControls := make(chan struct{})
	controlCtx, cancelControls := context.WithTimeout(ctx, 5*time.Second)
	defer cancelControls()
	var controlsReady sync.WaitGroup
	controlsReady.Add(2)
	go func() {
		controlsReady.Done()
		<-startControls
		control, setErr := st.SetAgentEmailReceiveControl(controlCtx, scope,
			provisioned.AccountID, operatorID, owner.ID, AgentEmailReceiveDisabled)
		agentResults <- agentControlResult{control: control, err: setErr}
	}()
	go func() {
		controlsReady.Done()
		<-startControls
		control, setErr := st.SetRealmAgentEmailReceiveControl(controlCtx, scope,
			provisioned.AccountID, operatorID, realm.ID, AgentEmailReceiveDisabled)
		realmResults <- realmControlResult{control: control, err: setErr}
	}()
	controlsReady.Wait()
	close(startControls)
	agentResult := <-agentResults
	realmResult := <-realmResults
	agentControl, err := agentResult.control, agentResult.err
	if err != nil || agentControl.ReceiveState != AgentEmailReceiveDisabled ||
		agentControl.AgentReceiveState != AgentEmailReceiveDisabled ||
		(agentControl.RealmReceiveState != AgentEmailReceiveEnabled &&
			agentControl.RealmReceiveState != AgentEmailReceiveDisabled) {
		t.Fatalf("agent disable = %#v / %v", agentControl, err)
	}
	realmControl, err := realmResult.control, realmResult.err
	if err != nil || realmControl.ReceiveState != AgentEmailReceiveDisabled ||
		realmControl.MailboxCount != 5 || realmControl.RowVersion != 2 {
		t.Fatalf("realm disable = %#v / %v", realmControl, err)
	}
	serializedAgent, err := st.GetAgentEmailReceiveControl(ctx, scope,
		provisioned.AccountID, operatorID, owner.ID)
	if err != nil || serializedAgent.ReceiveState != AgentEmailReceiveDisabled ||
		serializedAgent.AgentReceiveState != AgentEmailReceiveDisabled ||
		serializedAgent.RealmReceiveState != AgentEmailReceiveDisabled {
		t.Fatalf("serialized concurrent controls = %#v / %v", serializedAgent, err)
	}
	agentControl, err = st.SetAgentEmailReceiveControl(ctx, scope,
		provisioned.AccountID, operatorID, owner.ID, AgentEmailReceiveEnabled)
	if err != nil || agentControl.ReceiveState != AgentEmailReceiveDisabled ||
		agentControl.AgentReceiveState != AgentEmailReceiveEnabled ||
		agentControl.RealmReceiveState != AgentEmailReceiveDisabled ||
		agentControl.RealmDisabledAt == nil {
		t.Fatalf("independent agent enable under realm disable = %#v / %v", agentControl, err)
	}
	shown, err := st.GetAgentEmailAddress(ctx, scope, owner)
	if err != nil || shown.ReceiveState != AgentEmailReceiveDisabled ||
		shown.AgentReceiveState != AgentEmailReceiveEnabled ||
		shown.RealmReceiveState != AgentEmailReceiveDisabled {
		t.Fatalf("owner-visible disabled state = %#v / %v", shown, err)
	}
	raw := []byte("From: sender@example.com\r\nSubject: control\r\n\r\nbody")
	digest := sha256.Sum256(raw)
	ingest := func(address string, activeScope AgentEmailPilotScope) error {
		_, err := st.IngestAgentEmailPilot(ctx, activeScope, AgentEmailIngestInput{
			Relay: agentemail.RelayMetadata{
				Timestamp: time.Now().Unix(), KeyID: "pilot-key", Audience: activeScope.Audience,
				EnvelopeSender: "sender@example.com", EnvelopeRecipient: address,
				RawSize: int64(len(raw)), RawSHA256: hex.EncodeToString(digest[:]),
			},
			Raw: raw,
		})
		return err
	}
	if err := ingest(shown.Address, scope); !errors.Is(err, ErrAgentEmailReceiveDisabled) {
		t.Fatalf("realm-disabled ingest error = %v", err)
	}

	for _, agent := range agents {
		if err := st.DeleteAgent(ctx, provisioned.AccountID, realm.ID, agent.ID); err != nil {
			t.Fatal(err)
		}
	}
	realmControl, err = st.GetRealmAgentEmailReceiveControl(ctx, scope,
		provisioned.AccountID, operatorID, realm.ID)
	if err != nil || realmControl.ReceiveState != AgentEmailReceiveDisabled ||
		realmControl.MailboxCount != 0 || realmControl.RowVersion != 2 {
		t.Fatalf("zero-mailbox realm control = %#v / %v", realmControl, err)
	}

	secondScope, secondAgents := makeScope("second")
	secondAddresses, err := st.ReconcileAgentEmailPilot(ctx, secondScope)
	if err != nil || len(secondAddresses) != 5 {
		t.Fatalf("second reconcile = %#v / %v", secondAddresses, err)
	}
	secondOwner := Principal{
		Kind: PrincipalAgent, ID: secondAgents[0].ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: secondAgents[0].Name, AccountStatus: "active",
	}
	secondShown, err := st.GetAgentEmailAddress(ctx, secondScope, secondOwner)
	if err != nil || secondShown.ReceiveState != AgentEmailReceiveDisabled ||
		secondShown.AgentReceiveState != AgentEmailReceiveEnabled ||
		secondShown.RealmReceiveState != AgentEmailReceiveDisabled {
		t.Fatalf("new mailbox inherited durable realm disable = %#v / %v", secondShown, err)
	}
	if err := ingest(secondShown.Address, secondScope); !errors.Is(err, ErrAgentEmailReceiveDisabled) {
		t.Fatalf("new mailbox accepted under preserved realm disable: %v", err)
	}

	realmControl, err = st.SetRealmAgentEmailReceiveControl(ctx, secondScope,
		provisioned.AccountID, operatorID, realm.ID, AgentEmailReceiveEnabled)
	if err != nil || realmControl.ReceiveState != AgentEmailReceiveEnabled || realmControl.RowVersion != 3 {
		t.Fatalf("realm re-enable = %#v / %v", realmControl, err)
	}
	if err := ingest(secondShown.Address, secondScope); err != nil {
		t.Fatalf("ingest after realm re-enable = %v", err)
	}

	// The ingestion re-read must lock every row that contributes to the
	// effective receive decision. Prove that its shared lock covers the realm
	// control by showing that a concurrent realm disable cannot update that row
	// until the recipient transaction ends.
	raceCtx, cancelRace := context.WithTimeout(ctx, 5*time.Second)
	defer cancelRace()
	recipientTx, err := st.pool.Begin(raceCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = recipientTx.Rollback(raceCtx) }()
	lockedAddress, err := agentEmailAddressByRecipientTx(raceCtx, recipientTx,
		secondShown.Domain, secondShown.LocalPart, true)
	if err != nil || lockedAddress.RealmReceiveState != AgentEmailReceiveEnabled {
		t.Fatalf("locked recipient = %#v / %v", lockedAddress, err)
	}
	updateTx, err := st.pool.Begin(raceCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = updateTx.Rollback(raceCtx) }()
	var updatePID int32
	if err := updateTx.QueryRow(raceCtx, `SELECT pg_backend_pid()`).Scan(&updatePID); err != nil {
		t.Fatal(err)
	}
	updateDone := make(chan error, 1)
	go func() {
		_, updateErr := updateTx.Exec(raceCtx, `
			UPDATE agent_email_realm_receive_controls
			SET receive_state='disabled',disabled_at=clock_timestamp(),
			    updated_at=clock_timestamp(),row_version=row_version+1
			WHERE account_id=$1 AND realm_id=$2`, provisioned.AccountID, realm.ID)
		updateDone <- updateErr
	}()
	blocked := false
	for !blocked {
		select {
		case updateErr := <-updateDone:
			t.Fatalf("realm disable completed before recipient lock ended: %v", updateErr)
		case <-raceCtx.Done():
			t.Fatalf("realm disable did not reach recipient lock: %v", raceCtx.Err())
		default:
		}
		if err := st.pool.QueryRow(raceCtx,
			`SELECT cardinality(pg_blocking_pids($1)) > 0`, updatePID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if !blocked {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err := recipientTx.Commit(raceCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case updateErr := <-updateDone:
		if updateErr != nil {
			t.Fatal(updateErr)
		}
	case <-raceCtx.Done():
		t.Fatalf("realm disable stayed blocked after recipient commit: %v", raceCtx.Err())
	}
	if err := updateTx.Commit(raceCtx); err != nil {
		t.Fatal(err)
	}
	realmControl, err = st.GetRealmAgentEmailReceiveControl(ctx, secondScope,
		provisioned.AccountID, operatorID, realm.ID)
	if err != nil || realmControl.ReceiveState != AgentEmailReceiveDisabled || realmControl.RowVersion != 4 {
		t.Fatalf("serialized realm disable = %#v / %v", realmControl, err)
	}

	// Receive shutdown is a safety write: a suspended operator can inspect both
	// layers and disable them, but cannot re-enable either layer until resume.
	if _, err := st.SetRealmAgentEmailReceiveControl(ctx, secondScope,
		provisioned.AccountID, operatorID, realm.ID, AgentEmailReceiveEnabled); err != nil {
		t.Fatal(err)
	}
	if err := st.SuspendAccountSystem(ctx, provisioned.AccountID,
		"evacuation", "exercise suspended email safety controls"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAgentEmailReceiveControl(ctx, secondScope,
		provisioned.AccountID, operatorID, secondOwner.ID); err != nil {
		t.Fatalf("suspended agent control GET: %v", err)
	}
	if _, err := st.GetRealmAgentEmailReceiveControl(ctx, secondScope,
		provisioned.AccountID, operatorID, realm.ID); err != nil {
		t.Fatalf("suspended realm control GET: %v", err)
	}
	disabledAgent, err := st.SetAgentEmailReceiveControl(ctx, secondScope,
		provisioned.AccountID, operatorID, secondOwner.ID, AgentEmailReceiveDisabled)
	if err != nil {
		t.Fatalf("suspended agent disable: %v", err)
	}
	disabledRealm, err := st.SetRealmAgentEmailReceiveControl(ctx, secondScope,
		provisioned.AccountID, operatorID, realm.ID, AgentEmailReceiveDisabled)
	if err != nil {
		t.Fatalf("suspended realm disable: %v", err)
	}
	var safetyEventsBefore int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM account_events
		WHERE account_id=$1 AND verb IN ($2,$3)`, provisioned.AccountID,
		VerbAgentEmailAgentReceiveChanged, VerbAgentEmailRealmReceiveChanged).Scan(&safetyEventsBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAgentEmailReceiveControl(ctx, secondScope,
		provisioned.AccountID, operatorID, secondOwner.ID, AgentEmailReceiveEnabled); !errors.Is(err, ErrAccountNotActive) {
		t.Fatalf("suspended agent enable error = %v", err)
	}
	if _, err := st.SetRealmAgentEmailReceiveControl(ctx, secondScope,
		provisioned.AccountID, operatorID, realm.ID, AgentEmailReceiveEnabled); !errors.Is(err, ErrAccountNotActive) {
		t.Fatalf("suspended realm enable error = %v", err)
	}
	afterRejectedAgent, err := st.GetAgentEmailReceiveControl(ctx, secondScope,
		provisioned.AccountID, operatorID, secondOwner.ID)
	if err != nil || afterRejectedAgent.RowVersion != disabledAgent.RowVersion ||
		!afterRejectedAgent.UpdatedAt.Equal(disabledAgent.UpdatedAt) ||
		afterRejectedAgent.AgentReceiveState != AgentEmailReceiveDisabled {
		t.Fatalf("rejected suspended agent enable mutated control = %#v / before %#v / %v",
			afterRejectedAgent, disabledAgent, err)
	}
	afterRejectedRealm, err := st.GetRealmAgentEmailReceiveControl(ctx, secondScope,
		provisioned.AccountID, operatorID, realm.ID)
	if err != nil || afterRejectedRealm.RowVersion != disabledRealm.RowVersion ||
		!afterRejectedRealm.UpdatedAt.Equal(disabledRealm.UpdatedAt) ||
		afterRejectedRealm.ReceiveState != AgentEmailReceiveDisabled {
		t.Fatalf("rejected suspended realm enable mutated control = %#v / before %#v / %v",
			afterRejectedRealm, disabledRealm, err)
	}
	var safetyEventsAfter int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM account_events
		WHERE account_id=$1 AND verb IN ($2,$3)`, provisioned.AccountID,
		VerbAgentEmailAgentReceiveChanged, VerbAgentEmailRealmReceiveChanged).Scan(&safetyEventsAfter); err != nil {
		t.Fatal(err)
	}
	if safetyEventsAfter != safetyEventsBefore {
		t.Fatalf("rejected suspended enable appended audit events: before=%d after=%d",
			safetyEventsBefore, safetyEventsAfter)
	}
	if err := st.ResumeAccountSystem(ctx, provisioned.AccountID, "evacuation"); err != nil {
		t.Fatal(err)
	}
	resumedAgent, err := st.GetAgentEmailReceiveControl(ctx, secondScope,
		provisioned.AccountID, operatorID, secondOwner.ID)
	if err != nil || resumedAgent.AgentReceiveState != AgentEmailReceiveDisabled ||
		resumedAgent.RealmReceiveState != AgentEmailReceiveDisabled {
		t.Fatalf("resumed agent control = %#v / %v", resumedAgent, err)
	}
	resumedRealm, err := st.GetRealmAgentEmailReceiveControl(ctx, secondScope,
		provisioned.AccountID, operatorID, realm.ID)
	if err != nil || resumedRealm.ReceiveState != AgentEmailReceiveDisabled {
		t.Fatalf("resumed realm control = %#v / %v", resumedRealm, err)
	}

	// A GET must not heal or create lifecycle state. Missing rows are an
	// invariant failure/not-found until provisioning or PATCH establishes one.
	if _, err := st.pool.Exec(ctx, `
		DELETE FROM agent_email_realm_receive_controls
		WHERE account_id=$1 AND realm_id=$2`, provisioned.AccountID, realm.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetRealmAgentEmailReceiveControl(ctx, secondScope,
		provisioned.AccountID, operatorID, realm.ID); !errors.Is(err, ErrAgentEmailNotFound) {
		t.Fatalf("missing control GET error = %v", err)
	}
	var controlCount int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_realm_receive_controls
		WHERE account_id=$1 AND realm_id=$2`, provisioned.AccountID, realm.ID).Scan(&controlCount); err != nil {
		t.Fatal(err)
	}
	if controlCount != 0 {
		t.Fatalf("read-only realm GET recreated %d control rows", controlCount)
	}
}
