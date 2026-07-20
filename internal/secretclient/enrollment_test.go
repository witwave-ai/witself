package secretclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/sealed"
)

func TestVaultKeyEnrollmentCredentialFormattingIsRedacted(t *testing.T) {
	const secret = "witself-avk-enrollment-pairing-v1:credential-canary"
	begin := BeginVaultKeyEnrollmentResult{PairingSecret: secret, SAS: "123456"}
	approve := ApproveVaultKeyEnrollmentInput{
		EnrollmentID: "enr_abcdefghijklmnop", PairingSecret: secret, SourceLocationName: "home",
	}
	for _, value := range []any{begin, approve} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), secret) || !strings.Contains(string(raw), "[redacted]") {
			t.Fatalf("credential formatting was not redacted: %s", raw)
		}
	}
	if strings.Contains(fmt.Sprint(begin), secret) || strings.Contains(fmt.Sprint(approve), secret) {
		t.Fatal("ordinary formatting exposed enrollment credential")
	}
}

func TestVaultKeyEnrollmentEndToEndSurvivesLostResponses(t *testing.T) {
	ctx := context.Background()
	sourceHome, targetHome := t.TempDir(), t.TempDir()
	remote := newEnrollmentRemote(t)
	service := enrollmentTestService(remote)

	setEnrollmentHome(t, sourceHome)
	sourceKey := generateKey(t)
	defer sourceKey.Clear()
	if err := local.CreateAgentVaultKeyEpoch(testAccountName, testRealmName, testAgentName, sourceKey); err != nil {
		t.Fatal(err)
	}
	remote.current = bindingFor(sourceKey, testIdentity)
	remote.loseCreateResponse = true
	remote.loseApproveResponse = true
	remote.loseConsumeResponse = true
	remote.createCheck = func(in client.CreateVaultKeyEnrollmentInput) error {
		state, err := local.ReadAgentVaultKeyEnrollmentState(
			testAccountName, testRealmName, testAgentName, in.ID,
		)
		if err != nil {
			return err
		}
		defer state.Clear()
		if state.Finalized() {
			return errors.New("request finalized before backend create")
		}
		return nil
	}
	remote.consumeCheck = func(_ client.ConsumeVaultKeyEnrollmentInput) error {
		key, err := local.ReadAgentVaultKeyEpoch(
			testAccountName, testRealmName, testAgentName, remote.current.ID, uint64(remote.current.KeyVersion),
		)
		if err != nil {
			return err
		}
		defer key.Clear()
		if key.Metadata() != sourceKey.Metadata() {
			return errors.New("consume preceded exact epoch publication")
		}
		return nil
	}

	setEnrollmentHome(t, targetHome)
	begin, err := service.BeginVaultKeyEnrollment(ctx, BeginVaultKeyEnrollmentInput{
		LocationName: "target", ExpiresAt: time.Now().Add(20*time.Minute + 731*time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	if begin.Enrollment.LifecycleState != client.VaultEnrollmentStatePending ||
		begin.Enrollment.ExpiresAt.Nanosecond() != 0 || len(begin.SAS) != sealed.AVKEnrollmentSASDigits {
		t.Fatalf("begin result = %#v", begin)
	}
	if rendered := fmt.Sprintf("%#v", *begin); strings.Contains(rendered, begin.PairingSecret) || !strings.Contains(rendered, "redacted") {
		t.Fatalf("begin formatting disclosed pairing credential: %q", rendered)
	}
	state, err := local.ReadAgentVaultKeyEnrollmentState(
		testAccountName, testRealmName, testAgentName, begin.Enrollment.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Finalized() || state.Request.EnrollmentRequestID != begin.Enrollment.ID {
		state.Clear()
		t.Fatal("begin did not durably finalize the exact public request")
	}
	state.Clear()

	setEnrollmentHome(t, sourceHome)
	approved, err := service.ApproveVaultKeyEnrollment(ctx, ApproveVaultKeyEnrollmentInput{
		EnrollmentID: begin.Enrollment.ID, PairingSecret: begin.PairingSecret, SourceLocationName: "source",
	})
	if err != nil {
		t.Fatal(err)
	}
	if approved.LifecycleState != client.VaultEnrollmentStateApproved || approved.SourceLocationID == approved.TargetLocationID {
		t.Fatalf("approved enrollment = %+v", approved)
	}
	encodedAVK, err := sealed.EncodeAgentVaultKey(sourceKey)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(encodedAVK)
	if bytes.Contains(remote.transfer.Ciphertext, encodedAVK) ||
		strings.Contains(fmt.Sprint(remote.lastApprove), string(encodedAVK)) ||
		strings.Contains(fmt.Sprint(remote.lastApprove), begin.PairingSecret) {
		t.Fatal("approve transport exposed AVK or pairing material")
	}

	setEnrollmentHome(t, targetHome)
	consumed, err := service.CompleteVaultKeyEnrollment(ctx, CompleteVaultKeyEnrollmentInput{
		EnrollmentID: begin.Enrollment.ID, TargetLocationName: "target",
	})
	if err != nil {
		t.Fatal(err)
	}
	if consumed.LifecycleState != client.VaultEnrollmentStateConsumed {
		t.Fatalf("complete state = %q", consumed.LifecycleState)
	}
	installed, err := local.ReadAgentVaultKeyEpoch(
		testAccountName, testRealmName, testAgentName, sourceKey.ID(), sourceKey.Version(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if installed.Metadata() != sourceKey.Metadata() {
		installed.Clear()
		t.Fatal("target exact epoch differs from source")
	}
	installed.Clear()
	if _, err := local.ReadAgentVaultKeyEnrollmentState(
		testAccountName, testRealmName, testAgentName, begin.Enrollment.ID,
	); !errors.Is(err, local.ErrAgentVaultKeyEnrollmentUnavailable) {
		t.Fatalf("pending state after consume = %v, want unavailable", err)
	}
	// A retry after a lost final response and after local cleanup is still
	// successful from canonical consumed state plus the exact installed epoch.
	again, err := service.CompleteVaultKeyEnrollment(ctx, CompleteVaultKeyEnrollmentInput{
		EnrollmentID: begin.Enrollment.ID, TargetLocationName: "target",
	})
	if err != nil || again.LifecycleState != client.VaultEnrollmentStateConsumed {
		t.Fatalf("complete retry = %#v, %v", again, err)
	}
	if remote.createCalls != 1 || remote.approveCalls != 1 || remote.consumeCalls != 1 || remote.getCalls < 3 {
		t.Fatalf("remote calls create=%d approve=%d consume=%d get=%d",
			remote.createCalls, remote.approveCalls, remote.consumeCalls, remote.getCalls)
	}
	for _, value := range []string{remote.lastCreate.IdempotencyKey, remote.lastApprove.IdempotencyKey, remote.lastConsume.IdempotencyKey} {
		if value == "" || strings.Contains(value, begin.PairingSecret) || strings.Contains(value, string(encodedAVK)) {
			t.Fatalf("unsafe enrollment idempotency key %q", value)
		}
	}
}

func TestBeginVaultKeyEnrollmentDiscoversCrashWindowsWithoutRequestID(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	setEnrollmentHome(t, home)
	remote := newEnrollmentRemote(t)
	key := generateKey(t)
	defer key.Clear()
	remote.current = bindingFor(key, testIdentity)
	service := enrollmentTestService(remote)

	// Model a crash after private preflight publication but before the POST.
	requestID := "enr_dddddddddddddddd"
	recipient, err := sealed.GenerateAVKEnrollmentRecipientKey()
	if err != nil {
		t.Fatal(err)
	}
	pairing, err := sealed.GenerateAVKEnrollmentPairingSecret()
	if err != nil {
		t.Fatal(err)
	}
	if err := local.CreateAgentVaultKeyEnrollmentPreflight(
		testAccountName, testRealmName, testAgentName, requestID, recipient, pairing,
	); err != nil {
		t.Fatal(err)
	}
	recipient.Clear()
	pairing.Clear()
	begin, err := service.BeginVaultKeyEnrollment(ctx, BeginVaultKeyEnrollmentInput{
		LocationName: "target", ExpiresAt: time.Now().Add(15 * time.Minute),
	})
	if err != nil || begin.Enrollment.ID != requestID {
		t.Fatalf("preflight recovery = %#v, %v", begin, err)
	}
	if remote.createCalls != 1 {
		t.Fatalf("create calls = %d, want one recovered POST", remote.createCalls)
	}

	// Model death after the backend commit and local finalize but before the
	// generated id or pairing credential reached the human. Begin has no id
	// input and must return the same durable request without another POST.
	again, err := service.BeginVaultKeyEnrollment(ctx, BeginVaultKeyEnrollmentInput{
		LocationName: "target", ExpiresAt: time.Now().Add(30 * time.Minute),
	})
	if err != nil || again.Enrollment.ID != requestID || again.PairingSecret != begin.PairingSecret {
		t.Fatalf("post-create recovery = %#v, %v", again, err)
	}
	if remote.createCalls != 1 {
		t.Fatalf("recovered active request was posted again: %d", remote.createCalls)
	}
}

func TestCompleteRetainsPendingStateUntilConsumeIsCanonical(t *testing.T) {
	ctx := context.Background()
	sourceHome, targetHome := t.TempDir(), t.TempDir()
	remote := newEnrollmentRemote(t)
	service := enrollmentTestService(remote)
	setEnrollmentHome(t, sourceHome)
	key := generateKey(t)
	defer key.Clear()
	if err := local.CreateAgentVaultKeyEpoch(testAccountName, testRealmName, testAgentName, key); err != nil {
		t.Fatal(err)
	}
	remote.current = bindingFor(key, testIdentity)
	setEnrollmentHome(t, targetHome)
	begin, err := service.BeginVaultKeyEnrollment(ctx, BeginVaultKeyEnrollmentInput{
		LocationName: "target", ExpiresAt: time.Now().Add(15 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	setEnrollmentHome(t, sourceHome)
	if _, err := service.ApproveVaultKeyEnrollment(ctx, ApproveVaultKeyEnrollmentInput{
		EnrollmentID: begin.Enrollment.ID, PairingSecret: begin.PairingSecret, SourceLocationName: "source",
	}); err != nil {
		t.Fatal(err)
	}
	remote.failConsumeBeforeCommit = true
	setEnrollmentHome(t, targetHome)
	_, err = service.CompleteVaultKeyEnrollment(ctx, CompleteVaultKeyEnrollmentInput{
		EnrollmentID: begin.Enrollment.ID, TargetLocationName: "target",
	})
	if !errors.Is(err, ErrOperation) {
		t.Fatalf("failed consume error = %v, want operation", err)
	}
	state, err := local.ReadAgentVaultKeyEnrollmentState(
		testAccountName, testRealmName, testAgentName, begin.Enrollment.ID,
	)
	if err != nil || !state.Finalized() {
		t.Fatalf("pending state was removed before consume: %#v, %v", state, err)
	}
	state.Clear()
	remote.failConsumeBeforeCommit = false
	consumed, err := service.CompleteVaultKeyEnrollment(ctx, CompleteVaultKeyEnrollmentInput{
		EnrollmentID: begin.Enrollment.ID, TargetLocationName: "target",
	})
	if err != nil || consumed.LifecycleState != client.VaultEnrollmentStateConsumed {
		t.Fatalf("consume retry = %#v, %v", consumed, err)
	}
}

func TestApproveAuthenticatesPairingBeforeLocalAVKReadAndErrorsStayValueFree(t *testing.T) {
	ctx := context.Background()
	targetHome, emptySourceHome := t.TempDir(), t.TempDir()
	remote := newEnrollmentRemote(t)
	key := generateKey(t)
	defer key.Clear()
	remote.current = bindingFor(key, testIdentity)
	service := enrollmentTestService(remote)
	setEnrollmentHome(t, targetHome)
	begin, err := service.BeginVaultKeyEnrollment(ctx, BeginVaultKeyEnrollmentInput{
		LocationName: "target", ExpiresAt: time.Now().Add(15 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := sealed.GenerateAVKEnrollmentPairingSecret()
	if err != nil {
		t.Fatal(err)
	}
	wrongEncoded, err := sealed.EncodeAVKEnrollmentPairingSecret(wrong)
	wrong.Clear()
	if err != nil {
		t.Fatal(err)
	}
	setEnrollmentHome(t, emptySourceHome)
	_, err = service.ApproveVaultKeyEnrollment(ctx, ApproveVaultKeyEnrollmentInput{
		EnrollmentID: begin.Enrollment.ID, PairingSecret: wrongEncoded, SourceLocationName: "source",
	})
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("wrong pairing error = %v, want integrity before key unavailable", err)
	}
	if strings.Contains(err.Error(), wrongEncoded) || strings.Contains(err.Error(), begin.PairingSecret) {
		t.Fatal("pairing credential appeared in error")
	}
	if remote.approveCalls != 0 {
		t.Fatal("wrong pairing reached approval mutation")
	}
}

func TestVaultKeyEnrollmentGetListAndCancelAreValidatedAndCleanupTerminalState(t *testing.T) {
	ctx := context.Background()
	setEnrollmentHome(t, t.TempDir())
	remote := newEnrollmentRemote(t)
	key := generateKey(t)
	defer key.Clear()
	remote.current = bindingFor(key, testIdentity)
	service := enrollmentTestService(remote)
	begin, err := service.BeginVaultKeyEnrollment(ctx, BeginVaultKeyEnrollmentInput{
		LocationName: "target", ExpiresAt: time.Now().Add(15 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.GetVaultKeyEnrollment(ctx, begin.Enrollment.ID)
	if err != nil || got.ID != begin.Enrollment.ID {
		t.Fatalf("get = %#v, %v", got, err)
	}
	items, err := service.ListVaultKeyEnrollments(ctx, client.VaultKeyEnrollmentListOptions{State: "pending", Limit: 5})
	if err != nil || len(items) != 1 || items[0].ID != begin.Enrollment.ID {
		t.Fatalf("list = %#v, %v", items, err)
	}
	cancelled, err := service.CancelVaultKeyEnrollment(ctx, begin.Enrollment.ID)
	if err != nil || cancelled.LifecycleState != client.VaultEnrollmentStateCancelled {
		t.Fatalf("cancel = %#v, %v", cancelled, err)
	}
	if _, err := local.ReadAgentVaultKeyEnrollmentState(
		testAccountName, testRealmName, testAgentName, begin.Enrollment.ID,
	); !errors.Is(err, local.ErrAgentVaultKeyEnrollmentUnavailable) {
		t.Fatalf("cancelled local state = %v, want unavailable", err)
	}
	// Canonical terminal retry succeeds without requiring deleted private state.
	again, err := service.CancelVaultKeyEnrollment(ctx, begin.Enrollment.ID)
	if err != nil || again.LifecycleState != client.VaultEnrollmentStateCancelled || remote.cancelCalls != 1 {
		t.Fatalf("cancel retry = %#v, %v, calls=%d", again, err, remote.cancelCalls)
	}
	newCurrent := generateKey(t)
	defer newCurrent.Clear()
	remote.current = bindingFor(newCurrent, testIdentity)
	if got, err := service.GetVaultKeyEnrollment(ctx, begin.Enrollment.ID); err != nil ||
		got.LifecycleState != client.VaultEnrollmentStateCancelled {
		t.Fatalf("terminal get after key rotation = %#v, %v", got, err)
	}
	if items, err := service.ListVaultKeyEnrollments(ctx, client.VaultKeyEnrollmentListOptions{}); err != nil || len(items) != 1 {
		t.Fatalf("terminal list after key rotation = %#v, %v", items, err)
	}
}

func TestCancelVaultKeyEnrollmentWhileSuspendedNeedsNoSelfCurrentKeyOrLocalAVK(t *testing.T) {
	ctx := context.Background()
	setEnrollmentHome(t, t.TempDir())
	remote := newEnrollmentRemote(t)
	key := generateKey(t)
	defer key.Clear()
	remote.current = bindingFor(key, testIdentity)
	service := enrollmentTestService(remote)

	begin, err := service.BeginVaultKeyEnrollment(ctx, BeginVaultKeyEnrollmentInput{
		LocationName: "target", ExpiresAt: time.Now().Add(15 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	selfCalls, currentCalls := remote.selfCalls, remote.currentCalls
	remote.selfErr = errors.New("self is gated while suspended")
	remote.currentErr = errors.New("current key is gated while suspended")
	remote.loseCancelResponse = true
	got, err := service.GetVaultKeyEnrollment(ctx, begin.Enrollment.ID)
	if err != nil || got.ID != begin.Enrollment.ID {
		t.Fatalf("suspended value-free get = %#v, %v", got, err)
	}
	items, err := service.ListVaultKeyEnrollments(ctx, client.VaultKeyEnrollmentListOptions{State: "pending", Limit: 5})
	if err != nil || len(items) != 1 || items[0].ID != begin.Enrollment.ID {
		t.Fatalf("suspended value-free list = %#v, %v", items, err)
	}

	cancelled, err := service.CancelVaultKeyEnrollment(ctx, begin.Enrollment.ID)
	if err != nil || cancelled.LifecycleState != client.VaultEnrollmentStateCancelled {
		t.Fatalf("suspended cancellation = %#v, %v", cancelled, err)
	}
	if remote.selfCalls != selfCalls || remote.currentCalls != currentCalls {
		t.Fatalf("suspended cancellation consulted self/current: self %d->%d current %d->%d",
			selfCalls, remote.selfCalls, currentCalls, remote.currentCalls)
	}
	if remote.cancelCalls != 1 || remote.getCalls < 2 {
		t.Fatalf("lost cancel response was not reconciled: cancel=%d get=%d", remote.cancelCalls, remote.getCalls)
	}
	if _, err := local.ReadAgentVaultKeyEnrollmentState(
		testAccountName, testRealmName, testAgentName, begin.Enrollment.ID,
	); !errors.Is(err, local.ErrAgentVaultKeyEnrollmentUnavailable) {
		t.Fatalf("cancelled local target state = %v, want unavailable", err)
	}
	if _, err := local.ReadAgentVaultKeyEpoch(
		testAccountName, testRealmName, testAgentName, key.ID(), key.Version(),
	); !errors.Is(err, local.ErrAgentVaultKeyUnavailable) {
		t.Fatalf("test unexpectedly had a local AVK: %v", err)
	}

	// A terminal retry stays available while suspended and after private target
	// state has already been removed.
	again, err := service.CancelVaultKeyEnrollment(ctx, begin.Enrollment.ID)
	if err != nil || again.LifecycleState != client.VaultEnrollmentStateCancelled || remote.cancelCalls != 1 {
		t.Fatalf("suspended terminal retry = %#v, %v, cancel calls=%d", again, err, remote.cancelCalls)
	}
}

func TestCancelVaultKeyEnrollmentRejectsConfiguredAccountMismatchBeforeMutation(t *testing.T) {
	ctx := context.Background()
	setEnrollmentHome(t, t.TempDir())
	remote := newEnrollmentRemote(t)
	key := generateKey(t)
	defer key.Clear()
	remote.current = bindingFor(key, testIdentity)
	service := enrollmentTestService(remote)
	begin, err := service.BeginVaultKeyEnrollment(ctx, BeginVaultKeyEnrollmentInput{
		LocationName: "target", ExpiresAt: time.Now().Add(15 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	remote.enrollment.AccountID = "acc_zzzzzzzzzzzzzzzz"
	remote.selfErr = errors.New("self must not be consulted")
	remote.currentErr = errors.New("current key must not be consulted")
	_, err = service.CancelVaultKeyEnrollment(ctx, begin.Enrollment.ID)
	if !errors.Is(err, ErrIdentityMismatch) || remote.cancelCalls != 0 {
		t.Fatalf("account mismatch cancellation = %v, cancel calls=%d", err, remote.cancelCalls)
	}
	state, stateErr := local.ReadAgentVaultKeyEnrollmentState(
		testAccountName, testRealmName, testAgentName, begin.Enrollment.ID,
	)
	if stateErr != nil {
		t.Fatalf("mismatched cancellation removed local state: %v", stateErr)
	}
	state.Clear()
}

func TestObservePrefersExactEpochAndBootstrapRemainsCrashDiscoverable(t *testing.T) {
	t.Run("exact epoch wins over unrelated legacy", func(t *testing.T) {
		remote := newFakeRemote()
		service := newTestService(t, remote)
		legacy, exact := generateKey(t), generateKey(t)
		defer legacy.Clear()
		defer exact.Clear()
		createLocalKey(t, legacy)
		if err := local.CreateAgentVaultKeyEpoch(testAccountName, testRealmName, testAgentName, exact); err != nil {
			t.Fatal(err)
		}
		remote.current = bindingFor(exact, remote.identity)
		got, err := service.ReconcileVaultKey(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer got.Clear()
		if got.Metadata() != exact.Metadata() {
			t.Fatal("reconcile selected unrelated legacy over exact backend epoch")
		}
	})

	t.Run("lost registration response reuses legacy staging then publishes epoch", func(t *testing.T) {
		remote := newFakeRemote()
		service := newTestService(t, remote)
		remote.registerErr = errors.New("response lost before fake canonicalization")
		_, err := service.ReconcileVaultKey(context.Background())
		if !errors.Is(err, ErrOperation) {
			t.Fatalf("first bootstrap error = %v", err)
		}
		staged := readLocalKey(t)
		metadata := staged.Metadata()
		staged.Clear()
		remote.registerErr = nil
		got, err := service.ReconcileVaultKey(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer got.Clear()
		if got.Metadata() != metadata || remote.registerCalls != 2 {
			t.Fatal("bootstrap retry generated a second key")
		}
		epoch := readLocalKeyEpoch(t, metadata.ID, metadata.Version)
		epoch.Clear()
	})
}

type enrollmentRemote struct {
	t                       *testing.T
	identity                client.SelfIdentity
	current                 *client.VaultKeyBinding
	enrollment              *client.VaultKeyEnrollment
	transfer                *client.VaultKeyEnrollmentTransfer
	lastCreate              client.CreateVaultKeyEnrollmentInput
	lastApprove             client.ApproveVaultKeyEnrollmentInput
	lastConsume             client.ConsumeVaultKeyEnrollmentInput
	lastCancel              client.CancelVaultKeyEnrollmentInput
	createCalls             int
	getCalls                int
	listCalls               int
	approveCalls            int
	receiveCalls            int
	consumeCalls            int
	cancelCalls             int
	createCheck             func(client.CreateVaultKeyEnrollmentInput) error
	consumeCheck            func(client.ConsumeVaultKeyEnrollmentInput) error
	loseCreateResponse      bool
	loseApproveResponse     bool
	loseConsumeResponse     bool
	failConsumeBeforeCommit bool
	loseCancelResponse      bool
	selfErr                 error
	currentErr              error
	selfCalls               int
	currentCalls            int
}

func newEnrollmentRemote(t *testing.T) *enrollmentRemote {
	return &enrollmentRemote{t: t, identity: testIdentity}
}

func (f *enrollmentRemote) self(context.Context) (client.SelfDigest, error) {
	f.selfCalls++
	if f.selfErr != nil {
		return client.SelfDigest{}, f.selfErr
	}
	return client.SelfDigest{Identity: f.identity}, nil
}

func (f *enrollmentRemote) currentVaultKey(context.Context) (*client.VaultKeyBinding, error) {
	f.currentCalls++
	if f.currentErr != nil {
		return nil, f.currentErr
	}
	if f.current == nil {
		return nil, nil
	}
	value := *f.current
	return &value, nil
}

func (f *enrollmentRemote) registerVaultKey(context.Context, client.RegisterVaultKeyInput) (*client.VaultKeyMutationResult, error) {
	return nil, errors.New("unexpected register")
}

func (f *enrollmentRemote) createVaultKeyEnrollment(_ context.Context, in client.CreateVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error) {
	f.createCalls++
	f.lastCreate = in
	if f.createCheck != nil {
		if err := f.createCheck(in); err != nil {
			return nil, err
		}
	}
	if f.enrollment == nil {
		now := time.Now().UTC().Truncate(time.Second)
		f.enrollment = &client.VaultKeyEnrollment{
			ID: in.ID, AccountID: f.identity.AccountID, RealmID: f.identity.RealmID,
			OwnerAgentID: f.identity.AgentID, VaultKeyID: f.current.ID,
			VaultKeyVersion: f.current.KeyVersion, VaultKeyAlgorithm: f.current.Algorithm,
			VaultKeyFingerprint: f.current.Fingerprint, TargetLocationID: in.TargetLocationID,
			TargetLocationName: in.TargetLocationName, TargetPublicKey: in.TargetPublicKey,
			TargetKeyAlgorithm: in.TargetKeyAlgorithm, PairingCommitment: in.PairingCommitment,
			LifecycleState: client.VaultEnrollmentStatePending, RowVersion: 1,
			CreatedAt: now, ExpiresAt: in.ExpiresAt,
		}
	}
	value := cloneClientEnrollment(f.enrollment)
	if f.loseCreateResponse {
		f.loseCreateResponse = false
		return nil, errors.New("create response lost")
	}
	return value, nil
}

func (f *enrollmentRemote) listVaultKeyEnrollments(context.Context, client.VaultKeyEnrollmentListOptions) ([]client.VaultKeyEnrollment, error) {
	f.listCalls++
	if f.enrollment == nil {
		return []client.VaultKeyEnrollment{}, nil
	}
	return []client.VaultKeyEnrollment{*cloneClientEnrollment(f.enrollment)}, nil
}

func (f *enrollmentRemote) getVaultKeyEnrollment(_ context.Context, enrollmentID string) (*client.VaultKeyEnrollment, error) {
	f.getCalls++
	if f.enrollment == nil || f.enrollment.ID != enrollmentID {
		return nil, client.ErrNotFound
	}
	return cloneClientEnrollment(f.enrollment), nil
}

func (f *enrollmentRemote) approveVaultKeyEnrollment(_ context.Context, enrollmentID string, in client.ApproveVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error) {
	f.approveCalls++
	f.lastApprove = in
	if f.enrollment == nil || f.enrollment.ID != enrollmentID ||
		f.enrollment.LifecycleState != client.VaultEnrollmentStatePending ||
		f.enrollment.RowVersion != in.ExpectedRowVersion {
		return nil, errors.New("approve conflict")
	}
	now := time.Now().UTC()
	f.enrollment.LifecycleState = client.VaultEnrollmentStateApproved
	f.enrollment.SourceLocationID = in.SourceLocationID
	f.enrollment.TransferAlgorithm = in.TransferAlgorithm
	f.enrollment.RowVersion++
	f.enrollment.ApprovedAt = &now
	f.transfer = &client.VaultKeyEnrollmentTransfer{
		Enrollment:               *cloneClientEnrollment(f.enrollment),
		SourceEphemeralPublicKey: in.SourceEphemeralPublicKey,
		Ciphertext:               append([]byte(nil), in.TransferCiphertext...), ConsumeCommitment: in.ConsumeCommitment,
	}
	if f.loseApproveResponse {
		f.loseApproveResponse = false
		return nil, errors.New("approve response lost")
	}
	return cloneClientEnrollment(f.enrollment), nil
}

func (f *enrollmentRemote) receiveVaultKeyEnrollment(_ context.Context, enrollmentID string, in client.ReceiveVaultKeyEnrollmentInput) (*client.VaultKeyEnrollmentTransfer, error) {
	f.receiveCalls++
	if f.transfer == nil || f.enrollment.ID != enrollmentID || f.enrollment.TargetLocationID != in.TargetLocationID ||
		f.enrollment.LifecycleState != client.VaultEnrollmentStateApproved {
		return nil, errors.New("receive conflict")
	}
	value := *f.transfer
	value.Enrollment = *cloneClientEnrollment(&f.transfer.Enrollment)
	value.Ciphertext = append([]byte(nil), f.transfer.Ciphertext...)
	return &value, nil
}

func (f *enrollmentRemote) consumeVaultKeyEnrollment(_ context.Context, enrollmentID string, in client.ConsumeVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error) {
	f.consumeCalls++
	f.lastConsume = in
	if f.failConsumeBeforeCommit {
		return nil, errors.New("consume unavailable")
	}
	if f.consumeCheck != nil {
		if err := f.consumeCheck(in); err != nil {
			return nil, err
		}
	}
	if f.transfer == nil || f.enrollment.ID != enrollmentID || f.enrollment.RowVersion != in.ExpectedRowVersion ||
		f.enrollment.TargetLocationID != in.TargetLocationID ||
		!sealed.VerifyAVKEnrollmentConsumeProof(enrollmentID, f.transfer.ConsumeCommitment, in.ConsumeProof) {
		return nil, errors.New("consume conflict")
	}
	now := time.Now().UTC()
	f.enrollment.LifecycleState = client.VaultEnrollmentStateConsumed
	f.enrollment.TransferAlgorithm = ""
	f.enrollment.RowVersion++
	f.enrollment.ConsumedAt = &now
	clear(f.transfer.Ciphertext)
	f.transfer = nil
	if f.loseConsumeResponse {
		f.loseConsumeResponse = false
		return nil, errors.New("consume response lost")
	}
	return cloneClientEnrollment(f.enrollment), nil
}

func (f *enrollmentRemote) cancelVaultKeyEnrollment(_ context.Context, enrollmentID string, in client.CancelVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error) {
	f.cancelCalls++
	f.lastCancel = in
	if f.enrollment == nil || f.enrollment.ID != enrollmentID || f.enrollment.RowVersion != in.ExpectedRowVersion {
		return nil, errors.New("cancel conflict")
	}
	now := time.Now().UTC()
	f.enrollment.LifecycleState = client.VaultEnrollmentStateCancelled
	f.enrollment.TransferAlgorithm = ""
	f.enrollment.RowVersion++
	f.enrollment.CancelledAt = &now
	if f.transfer != nil {
		clear(f.transfer.Ciphertext)
		f.transfer = nil
	}
	if f.loseCancelResponse {
		f.loseCancelResponse = false
		return nil, errors.New("cancel response lost")
	}
	return cloneClientEnrollment(f.enrollment), nil
}

func (f *enrollmentRemote) createSecret(context.Context, client.CreateSecretInput) (*client.SecretMutationResult, error) {
	return nil, errors.New("unexpected secret create")
}
func (f *enrollmentRemote) listSecrets(context.Context, client.SecretListOptions) (*client.SecretPage, error) {
	return nil, errors.New("unexpected secret list")
}
func (f *enrollmentRemote) getSecret(context.Context, string) (*client.Secret, error) {
	return nil, errors.New("unexpected secret get")
}
func (f *enrollmentRemote) accessSecretField(context.Context, string, string, string) (*client.SecretMaterial, error) {
	return nil, errors.New("unexpected secret access")
}

func enrollmentTestService(remote remote) *Service {
	return &Service{remote: remote, accountID: testAccountID, accountName: testAccountName,
		realmName: testRealmName, agentName: testAgentName}
}

func setEnrollmentHome(t *testing.T, home string) {
	t.Helper()
	if err := os.Setenv("WITSELF_HOME", home); err != nil {
		t.Fatal(err)
	}
}

func cloneClientEnrollment(value *client.VaultKeyEnrollment) *client.VaultKeyEnrollment {
	if value == nil {
		return nil
	}
	result := *value
	result.ApprovedAt = cloneEnrollmentTime(value.ApprovedAt)
	result.ConsumedAt = cloneEnrollmentTime(value.ConsumedAt)
	result.CancelledAt = cloneEnrollmentTime(value.CancelledAt)
	result.ExpiredAt = cloneEnrollmentTime(value.ExpiredAt)
	return &result
}

func cloneEnrollmentTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	result := *value
	return &result
}
