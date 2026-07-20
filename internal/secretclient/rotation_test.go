package secretclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/sealed"
)

func TestRotateVaultKeyStagesEveryPageCommitsAndRetainsEpochs(t *testing.T) {
	service, remote, source := newVaultKeyRotationTestService(t)
	defer source.Clear()
	remote.maxPage = 2
	remote.items = newVaultKeyRotationSourceItems(t, source, remote.identity, 5)

	rotation, err := service.RotateVaultKey(context.Background(), riskAcceptedRotationOptions())
	if err != nil {
		t.Fatal(err)
	}
	if rotation.LifecycleState != client.VaultKeyRotationCommitted || rotation.ItemCount != 5 ||
		rotation.StagedCount != 5 || remote.startCalls != 1 || remote.stageCalls != 3 ||
		remote.commitCalls != 1 || remote.listCalls < 6 {
		t.Fatalf("unexpected completed rotation/calls: rotation=%+v start=%d stage=%d commit=%d list=%d",
			rotation, remote.startCalls, remote.stageCalls, remote.commitCalls, remote.listCalls)
	}
	if remote.targetMissingAtStart {
		t.Fatal("Start ran before the exact target AVK epoch was durable locally")
	}
	if remote.intentMissingAtStart {
		t.Fatal("Start ran before its exact crash-recovery intent was durable locally")
	}
	sourceOnDisk, err := local.ReadAgentVaultKeyEpoch(testAccountName, testRealmName, testAgentName,
		rotation.SourceKeyID, uint64(rotation.SourceKeyVersion))
	if err != nil {
		t.Fatalf("retained source epoch: %v", err)
	}
	sourceOnDisk.Clear()
	targetOnDisk, err := local.ReadAgentVaultKeyEpoch(testAccountName, testRealmName, testAgentName,
		rotation.TargetKeyID, uint64(rotation.TargetKeyVersion))
	if err != nil {
		t.Fatalf("current target epoch: %v", err)
	}
	defer targetOnDisk.Clear()
	if remote.current == nil || !vaultKeyMatches(targetOnDisk, remote.current, remote.identity) {
		t.Fatal("commit did not resolve the exact target as current")
	}
	for _, item := range remote.items {
		if item.StagedAt == nil || len(item.TargetWrappedDEK) != sealed.WrappedDEKBytes {
			t.Fatal("commit occurred without a fully staged item")
		}
	}
	intent, err := local.ReadAgentVaultKeyRotationIntent(testAccountName, testRealmName, testAgentName)
	if err != nil || intent.RotationID != rotation.ID {
		t.Fatalf("terminal intent = %#v, %v", intent, err)
	}
	freshService := &Service{
		remote: remote, accountID: testAccountID,
		accountName: testAccountName, realmName: testRealmName, agentName: testAgentName,
	}
	startCalls, commitCalls := remote.startCalls, remote.commitCalls
	replayed, err := freshService.RotateVaultKey(context.Background(), riskAcceptedRotationOptions())
	if err != nil || replayed.ID != rotation.ID || replayed.LifecycleState != client.VaultKeyRotationCommitted {
		t.Fatalf("cross-process terminal recovery = %+v, %v", replayed, err)
	}
	if remote.startCalls != startCalls || remote.commitCalls != commitCalls {
		t.Fatal("terminal intent recovery issued a second mutation")
	}
	if err := freshService.AcknowledgeVaultKeyRotation(context.Background(), rotation.ID); err != nil {
		t.Fatalf("acknowledge terminal rotation: %v", err)
	}
	if _, err := local.ReadAgentVaultKeyRotationIntent(testAccountName, testRealmName, testAgentName); !errors.Is(err, local.ErrAgentVaultKeyRotationIntentUnavailable) {
		t.Fatalf("acknowledged intent still present: %v", err)
	}
	if err := freshService.AcknowledgeVaultKeyRotation(context.Background(), rotation.ID); err != nil {
		t.Fatalf("idempotent acknowledge: %v", err)
	}
}

func TestRotateVaultKeyRequiresExactlyOneRecoveryDecisionBeforeRemoteWork(t *testing.T) {
	passphrase := []byte("rotation recovery passphrase for tests")
	defer clear(passphrase)
	tests := []struct {
		name    string
		options RotateVaultKeyOptions
	}{
		{name: "zero value"},
		{name: "sink without passphrase", options: RotateVaultKeyOptions{RecoverySink: &fakeVaultKeyRotationRecoverySink{}}},
		{name: "passphrase without sink", options: RotateVaultKeyOptions{RecoveryPassphrase: passphrase}},
		{name: "short passphrase", options: RotateVaultKeyOptions{
			RecoverySink: &fakeVaultKeyRotationRecoverySink{}, RecoveryPassphrase: []byte("too-short"),
		}},
		{name: "artifact and risk branches", options: RotateVaultKeyOptions{
			RecoverySink: &fakeVaultKeyRotationRecoverySink{}, RecoveryPassphrase: passphrase,
			AcceptUnrecoverableKeyLoss: true,
		}},
		{name: "risk branch with passphrase", options: RotateVaultKeyOptions{
			RecoveryPassphrase: passphrase, AcceptUnrecoverableKeyLoss: true,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, remote, source := newVaultKeyRotationTestService(t)
			defer source.Clear()
			remote.items = newVaultKeyRotationSourceItems(t, source, remote.identity, 1)
			rotation, err := service.RotateVaultKey(context.Background(), test.options)
			if rotation != nil || !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("result = %+v, %v, want invalid input", rotation, err)
			}
			if remote.selfCalls != 0 || remote.getOpenCalls != 0 || remote.startCalls != 0 ||
				remote.listCalls != 0 || remote.stageCalls != 0 || remote.commitCalls != 0 {
				t.Fatalf("invalid options reached remote: self/open/start/list/stage/commit = %d/%d/%d/%d/%d/%d",
					remote.selfCalls, remote.getOpenCalls, remote.startCalls, remote.listCalls,
					remote.stageCalls, remote.commitCalls)
			}
			if _, err := local.ReadAgentVaultKeyRotationIntent(
				testAccountName, testRealmName, testAgentName,
			); !errors.Is(err, local.ErrAgentVaultKeyRotationIntentUnavailable) {
				t.Fatalf("invalid options created rotation intent: %v", err)
			}
		})
	}
}

func TestRotateVaultKeyOptionsFormattingRedactsRecoveryPassphrase(t *testing.T) {
	const canary = "never-format-this-rotation-recovery-passphrase"
	options := RotateVaultKeyOptions{
		RecoverySink: &fakeVaultKeyRotationRecoverySink{}, RecoveryPassphrase: []byte(canary),
	}
	for _, rendered := range []string{
		fmt.Sprint(options), fmt.Sprintf("%+v", options), fmt.Sprintf("%#v", options),
	} {
		if strings.Contains(rendered, canary) || !strings.Contains(rendered, "<redacted>") {
			t.Fatalf("formatted options = %q", rendered)
		}
	}
}

func TestRotateVaultKeyDurablyPublishesAndVerifiesExactRecoveryBeforeCommit(t *testing.T) {
	service, remote, source := newVaultKeyRotationTestService(t)
	defer source.Clear()
	remote.items = newVaultKeyRotationSourceItems(t, source, remote.identity, 3)
	passphrase := []byte("rotation recovery passphrase for tests")
	defer clear(passphrase)
	sink := &fakeVaultKeyRotationRecoverySink{}
	defer func() { clear(sink.artifact) }()
	sink.beforePut = func(metadata sealed.AVKRecoveryMetadata, _ []byte) {
		if remote.rotation == nil || remote.rotation.LifecycleState != client.VaultKeyRotationOpen ||
			remote.rotation.StagedCount != remote.rotation.ItemCount ||
			!validVaultKeyRotationHash(remote.rotation.StagedPlanHash) {
			t.Fatal("recovery publication did not occur after complete independent plan verification")
		}
		if metadata.Scope != recoveryScope(remote.identity) || metadata.AVK.ID != remote.rotation.TargetKeyID ||
			metadata.AVK.Version != uint64(remote.rotation.TargetKeyVersion) {
			t.Fatalf("recovery metadata = %+v, want exact target", metadata)
		}
	}
	remote.beforeCommit = func(input client.CommitVaultKeyRotationInput) error {
		if len(sink.artifact) == 0 || sink.readCalls < 2 {
			return errors.New("commit preceded durable read-back")
		}
		key, err := sealed.OpenAgentVaultKeyRecovery(sink.artifact, passphrase, recoveryScope(remote.identity))
		if err != nil {
			return err
		}
		defer key.Clear()
		if key.ID() != remote.rotation.TargetKeyID || key.Version() != uint64(remote.rotation.TargetKeyVersion) {
			return errors.New("recovery artifact did not contain exact target")
		}
		if input.RecoveryDisposition.Mode != client.VaultKeyRotationRecoveryArtifact ||
			input.RecoveryDisposition.ArtifactSHA256 != vaultKeyRotationWrapperHash(sink.artifact) {
			return errors.New("commit disposition did not bind durable artifact")
		}
		return nil
	}

	rotation, err := service.RotateVaultKey(context.Background(), RotateVaultKeyOptions{
		RecoverySink: sink, RecoveryPassphrase: passphrase,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rotation.LifecycleState != client.VaultKeyRotationCommitted || sink.putCalls != 1 || sink.readCalls != 2 ||
		remote.commitCalls != 1 || rotation.RecoveryDispositionMode != client.VaultKeyRotationRecoveryArtifact ||
		rotation.RecoveryArtifactSHA256 != vaultKeyRotationWrapperHash(sink.artifact) {
		t.Fatalf("rotation/sink = %+v put=%d read=%d commit=%d", rotation, sink.putCalls, sink.readCalls, remote.commitCalls)
	}
}

func TestRotateVaultKeyAcceptsOnlyExactExistingRecoveryArtifact(t *testing.T) {
	const passphraseText = "rotation recovery passphrase for tests"
	tests := []struct {
		name       string
		artifact   func(*testing.T, *sealed.AgentVaultKey, client.SelfIdentity) []byte
		passphrase []byte
		want       error
	}{
		{
			name: "exact replay",
			artifact: func(t *testing.T, target *sealed.AgentVaultKey, identity client.SelfIdentity) []byte {
				return exportVaultKeyRotationRecoveryForTest(t, target, identity, []byte(passphraseText))
			},
			passphrase: []byte(passphraseText),
		},
		{
			name: "wrong target",
			artifact: func(t *testing.T, _ *sealed.AgentVaultKey, identity client.SelfIdentity) []byte {
				other := generateVaultKeyVersion(t, 2)
				defer other.Clear()
				return exportVaultKeyRotationRecoveryForTest(t, other, identity, []byte(passphraseText))
			},
			passphrase: []byte(passphraseText), want: ErrIntegrity,
		},
		{
			name: "wrong owner scope",
			artifact: func(t *testing.T, target *sealed.AgentVaultKey, identity client.SelfIdentity) []byte {
				identity.AgentID = mustRotationTestID(t, "agent")
				return exportVaultKeyRotationRecoveryForTest(t, target, identity, []byte(passphraseText))
			},
			passphrase: []byte(passphraseText), want: ErrIntegrity,
		},
		{
			name: "wrong passphrase",
			artifact: func(t *testing.T, target *sealed.AgentVaultKey, identity client.SelfIdentity) []byte {
				return exportVaultKeyRotationRecoveryForTest(t, target, identity, []byte(passphraseText))
			},
			passphrase: []byte("different recovery passphrase for tests"), want: ErrIntegrity,
		},
		{
			name: "malformed artifact",
			artifact: func(_ *testing.T, _ *sealed.AgentVaultKey, _ client.SelfIdentity) []byte {
				return []byte("not-a-recovery-artifact")
			},
			passphrase: []byte(passphraseText), want: ErrIntegrity,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, remote, source := newVaultKeyRotationTestService(t)
			defer source.Clear()
			target := generateVaultKeyVersion(t, 2)
			defer target.Clear()
			createVaultKeyEpoch(t, target)
			items := newVaultKeyRotationSourceItems(t, source, remote.identity, 1)
			remote.rotation = newOpenVaultKeyRotation(t, source, target, remote.identity, items)
			remote.items = items
			sink := &fakeVaultKeyRotationRecoverySink{artifact: test.artifact(t, target, remote.identity)}
			defer clear(sink.artifact)
			defer clear(test.passphrase)

			rotation, err := service.RotateVaultKey(context.Background(), RotateVaultKeyOptions{
				RecoverySink: sink, RecoveryPassphrase: test.passphrase,
			})
			if test.want == nil {
				if err != nil || rotation == nil || rotation.LifecycleState != client.VaultKeyRotationCommitted {
					t.Fatalf("exact replay result = %+v, %v", rotation, err)
				}
			} else if rotation != nil || !errors.Is(err, test.want) {
				t.Fatalf("result = %+v, %v, want %v", rotation, err, test.want)
			}
			if sink.putCalls != 0 {
				t.Fatalf("existing artifact was overwritten: put calls = %d", sink.putCalls)
			}
			if test.want != nil && remote.commitCalls != 0 {
				t.Fatal("invalid existing artifact reached commit")
			}
		})
	}
}

func TestRotateVaultKeyRecoveryPutIsCrashResumableAndFailClosed(t *testing.T) {
	t.Run("lost durable put acknowledgement is recovered by read-back", func(t *testing.T) {
		service, remote, source := newVaultKeyRotationTestService(t)
		defer source.Clear()
		remote.items = newVaultKeyRotationSourceItems(t, source, remote.identity, 1)
		passphrase := []byte("rotation recovery passphrase for tests")
		defer clear(passphrase)
		sink := &fakeVaultKeyRotationRecoverySink{putErr: errors.New("acknowledgement lost")}
		defer func() { clear(sink.artifact) }()
		rotation, err := service.RotateVaultKey(context.Background(), RotateVaultKeyOptions{
			RecoverySink: sink, RecoveryPassphrase: passphrase,
		})
		if err != nil || rotation == nil || rotation.LifecycleState != client.VaultKeyRotationCommitted ||
			sink.putCalls != 1 || sink.readCalls != 2 {
			t.Fatalf("lost put acknowledgement = %+v, %v put/read=%d/%d",
				rotation, err, sink.putCalls, sink.readCalls)
		}
	})

	t.Run("failed put without durable artifact leaves open rotation", func(t *testing.T) {
		service, remote, source := newVaultKeyRotationTestService(t)
		defer source.Clear()
		remote.items = newVaultKeyRotationSourceItems(t, source, remote.identity, 1)
		passphrase := []byte("rotation recovery passphrase for tests")
		defer clear(passphrase)
		failedSink := &fakeVaultKeyRotationRecoverySink{
			failBeforePut: true, putErr: errors.New("storage unavailable"),
		}
		rotation, err := service.RotateVaultKey(context.Background(), RotateVaultKeyOptions{
			RecoverySink: failedSink, RecoveryPassphrase: passphrase,
		})
		if rotation != nil || !errors.Is(err, ErrOperation) || remote.rotation == nil ||
			remote.rotation.LifecycleState != client.VaultKeyRotationOpen || remote.commitCalls != 0 {
			t.Fatalf("failed put result = %+v, %v remote=%+v commit=%d",
				rotation, err, remote.rotation, remote.commitCalls)
		}
		if remote.rotation.StagedCount != remote.rotation.ItemCount {
			t.Fatal("recovery gate did not run at the final pre-commit boundary")
		}

		// A fresh process resumes the exact open rotation and candidate. It does
		// not issue another Start, and commits only after a new sink verifies.
		fresh := &Service{
			remote: remote, accountID: testAccountID,
			accountName: testAccountName, realmName: testRealmName, agentName: testAgentName,
		}
		goodSink := &fakeVaultKeyRotationRecoverySink{}
		defer func() { clear(goodSink.artifact) }()
		completed, err := fresh.RotateVaultKey(context.Background(), RotateVaultKeyOptions{
			RecoverySink: goodSink, RecoveryPassphrase: passphrase,
		})
		if err != nil || completed == nil || completed.ID != remote.rotation.ID ||
			completed.LifecycleState != client.VaultKeyRotationCommitted || remote.startCalls != 1 ||
			remote.commitCalls != 1 || goodSink.putCalls != 1 {
			t.Fatalf("recovery resume = %+v, %v start/commit/put=%d/%d/%d",
				completed, err, remote.startCalls, remote.commitCalls, goodSink.putCalls)
		}
	})
}

func TestRotateVaultKeyRiskAcceptanceIsExplicitAndDurable(t *testing.T) {
	service, remote, source := newVaultKeyRotationTestService(t)
	defer source.Clear()
	remote.items = newVaultKeyRotationSourceItems(t, source, remote.identity, 1)
	rotation, err := service.RotateVaultKey(context.Background(), riskAcceptedRotationOptions())
	if err != nil || rotation == nil || rotation.LifecycleState != client.VaultKeyRotationCommitted {
		t.Fatalf("risk-accepted rotation = %+v, %v", rotation, err)
	}
	if len(remote.commitInputs) != 1 ||
		remote.commitInputs[0].RecoveryDisposition.Mode != client.VaultKeyRotationRiskAccepted ||
		remote.commitInputs[0].RecoveryDisposition.ArtifactSHA256 != "" ||
		rotation.RecoveryDispositionMode != client.VaultKeyRotationRiskAccepted ||
		rotation.RecoveryArtifactSHA256 != "" {
		t.Fatalf("risk disposition input/status = %+v / %+v", remote.commitInputs, rotation)
	}
}

func TestRotateVaultKeyRequiresExactRecoveryDispositionInEveryCommitOutcome(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*fakeVaultKeyRotationRemote)
	}{
		{
			name: "mutation response",
			configure: func(remote *fakeVaultKeyRotationRemote) {
				remote.mutateCommitResponse = replaceRotationRecoveryDispositionForTest
			},
		},
		{
			name: "first ambiguous status",
			configure: func(remote *fakeVaultKeyRotationRemote) {
				remote.loseCommitAfterApply = true
				remote.mutateGetResponse = func(rotation *client.VaultKeyRotation) {
					if rotation.LifecycleState == client.VaultKeyRotationCommitted {
						replaceRotationRecoveryDispositionForTest(rotation)
					}
				}
			},
		},
		{
			name: "final ambiguous status after exact retry",
			configure: func(remote *fakeVaultKeyRotationRemote) {
				remote.failCommitBeforeApply = 1
				remote.loseCommitAfterApply = true
				remote.mutateGetResponse = func(rotation *client.VaultKeyRotation) {
					if rotation.LifecycleState == client.VaultKeyRotationCommitted {
						replaceRotationRecoveryDispositionForTest(rotation)
					}
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, remote, source := newVaultKeyRotationTestService(t)
			defer source.Clear()
			remote.items = newVaultKeyRotationSourceItems(t, source, remote.identity, 1)
			test.configure(remote)
			rotation, err := service.RotateVaultKey(context.Background(), riskAcceptedRotationOptions())
			if rotation != nil || !errors.Is(err, ErrIntegrity) {
				t.Fatalf("mismatched disposition result = %+v, %v", rotation, err)
			}
		})
	}
}

func TestRotateVaultKeyTerminalReplayUsesCommittedDispositionWithoutTouchingNewSink(t *testing.T) {
	service, remote, source := newVaultKeyRotationTestService(t)
	defer source.Clear()
	remote.items = newVaultKeyRotationSourceItems(t, source, remote.identity, 1)
	committed, err := service.RotateVaultKey(context.Background(), riskAcceptedRotationOptions())
	if err != nil {
		t.Fatal(err)
	}

	passphrase := []byte("rotation recovery passphrase for tests")
	defer clear(passphrase)
	unrelatedSink := &fakeVaultKeyRotationRecoverySink{}
	fresh := &Service{
		remote: remote, accountID: testAccountID,
		accountName: testAccountName, realmName: testRealmName, agentName: testAgentName,
	}
	replayed, err := fresh.RotateVaultKey(context.Background(), RotateVaultKeyOptions{
		RecoverySink: unrelatedSink, RecoveryPassphrase: passphrase,
	})
	if err != nil || replayed == nil || replayed.ID != committed.ID ||
		replayed.RecoveryDispositionMode != client.VaultKeyRotationRiskAccepted {
		t.Fatalf("terminal replay = %+v, %v", replayed, err)
	}
	if unrelatedSink.putCalls != 0 || unrelatedSink.readCalls != 0 || remote.commitCalls != 1 {
		t.Fatalf("terminal replay used new recovery options: put/read/commit=%d/%d/%d",
			unrelatedSink.putCalls, unrelatedSink.readCalls, remote.commitCalls)
	}
}

func TestRotateVaultKeyArtifactTerminalReplayIgnoresRiskRetryOption(t *testing.T) {
	service, remote, source := newVaultKeyRotationTestService(t)
	defer source.Clear()
	remote.items = newVaultKeyRotationSourceItems(t, source, remote.identity, 1)
	passphrase := []byte("rotation recovery passphrase for tests")
	defer clear(passphrase)
	sink := &fakeVaultKeyRotationRecoverySink{}
	defer func() { clear(sink.artifact) }()
	committed, err := service.RotateVaultKey(context.Background(), RotateVaultKeyOptions{
		RecoverySink: sink, RecoveryPassphrase: passphrase,
	})
	if err != nil {
		t.Fatal(err)
	}
	reads, puts := sink.readCalls, sink.putCalls
	fresh := &Service{
		remote: remote, accountID: testAccountID,
		accountName: testAccountName, realmName: testRealmName, agentName: testAgentName,
	}
	replayed, err := fresh.RotateVaultKey(context.Background(), riskAcceptedRotationOptions())
	if err != nil || replayed == nil || replayed.ID != committed.ID ||
		replayed.RecoveryDispositionMode != client.VaultKeyRotationRecoveryArtifact ||
		replayed.RecoveryArtifactSHA256 != committed.RecoveryArtifactSHA256 {
		t.Fatalf("artifact terminal replay = %+v, %v", replayed, err)
	}
	if sink.readCalls != reads || sink.putCalls != puts || remote.commitCalls != 1 {
		t.Fatalf("artifact terminal replay repeated sink/commit work: read/put/commit=%d/%d/%d",
			sink.readCalls, sink.putCalls, remote.commitCalls)
	}
}

func TestRotateVaultKeyResumesOpenRunAndVerifiesAlreadyStagedItems(t *testing.T) {
	service, remote, source := newVaultKeyRotationTestService(t)
	defer source.Clear()
	target := generateVaultKeyVersion(t, 2)
	defer target.Clear()
	createVaultKeyEpoch(t, target)
	items := newVaultKeyRotationSourceItems(t, source, remote.identity, 3)
	rotation := newOpenVaultKeyRotation(t, source, target, remote.identity, items)
	stageVaultKeyRotationTestItem(t, source, target, remote.identity, rotation, &items[0])
	rotation.StagedCount = 1
	remote.rotation = rotation
	remote.items = items
	remote.maxPage = 1

	completed, err := service.RotateVaultKey(context.Background(), riskAcceptedRotationOptions())
	if err != nil {
		t.Fatal(err)
	}
	if completed.ID != rotation.ID || completed.LifecycleState != client.VaultKeyRotationCommitted ||
		remote.startCalls != 0 || remote.stageCalls != 2 || remote.commitCalls != 1 {
		t.Fatalf("resume did not finish existing run: %+v calls start=%d stage=%d commit=%d",
			completed, remote.startCalls, remote.stageCalls, remote.commitCalls)
	}
}

func TestRotateVaultKeyRecoversLostStartAndStageResponses(t *testing.T) {
	t.Run("start applied before response loss", func(t *testing.T) {
		service, remote, source := newVaultKeyRotationTestService(t)
		defer source.Clear()
		remote.items = newVaultKeyRotationSourceItems(t, source, remote.identity, 1)
		remote.loseStartAfterApply = true

		completed, err := service.RotateVaultKey(context.Background(), riskAcceptedRotationOptions())
		if err != nil {
			t.Fatal(err)
		}
		if completed.LifecycleState != client.VaultKeyRotationCommitted || remote.startCalls != 1 {
			t.Fatalf("lost Start response was not discovered: %+v calls=%d", completed, remote.startCalls)
		}
	})

	t.Run("start failed before mutation retries exact request", func(t *testing.T) {
		service, remote, source := newVaultKeyRotationTestService(t)
		defer source.Clear()
		remote.items = newVaultKeyRotationSourceItems(t, source, remote.identity, 1)
		remote.failStartBeforeApply = 1

		if _, err := service.RotateVaultKey(context.Background(), riskAcceptedRotationOptions()); err != nil {
			t.Fatal(err)
		}
		if remote.startCalls != 2 || len(remote.startInputs) != 2 ||
			remote.startInputs[0].ID != remote.startInputs[1].ID ||
			remote.startInputs[0].IdempotencyKey != remote.startInputs[1].IdempotencyKey {
			t.Fatalf("Start retry changed request fence: %#v", remote.startInputs)
		}
	})

	t.Run("stage applied before response loss", func(t *testing.T) {
		service, remote, source := newVaultKeyRotationTestService(t)
		defer source.Clear()
		remote.items = newVaultKeyRotationSourceItems(t, source, remote.identity, 3)
		remote.loseStageAfterApply = true

		completed, err := service.RotateVaultKey(context.Background(), riskAcceptedRotationOptions())
		if err != nil {
			t.Fatal(err)
		}
		if completed.LifecycleState != client.VaultKeyRotationCommitted || remote.stageCalls != 1 ||
			remote.commitCalls != 1 {
			t.Fatalf("lost Stage response was not reconciled: %+v stage=%d commit=%d",
				completed, remote.stageCalls, remote.commitCalls)
		}
	})

	t.Run("stage failed before mutation retries exact request", func(t *testing.T) {
		service, remote, source := newVaultKeyRotationTestService(t)
		defer source.Clear()
		remote.items = newVaultKeyRotationSourceItems(t, source, remote.identity, 2)
		remote.failStageBeforeApply = 1

		if _, err := service.RotateVaultKey(context.Background(), riskAcceptedRotationOptions()); err != nil {
			t.Fatal(err)
		}
		if remote.stageCalls != 2 || len(remote.stageInputs) != 2 ||
			remote.stageInputs[0].IdempotencyKey != remote.stageInputs[1].IdempotencyKey ||
			!equalVaultKeyRotationStageItems(remote.stageInputs[0].Items, remote.stageInputs[1].Items) {
			t.Fatalf("Stage retry changed request fence: %#v", remote.stageInputs)
		}
	})
}

func TestRotateVaultKeyRejectsTamperedAlreadyStagedWrapper(t *testing.T) {
	service, remote, source := newVaultKeyRotationTestService(t)
	defer source.Clear()
	target := generateVaultKeyVersion(t, 2)
	defer target.Clear()
	createVaultKeyEpoch(t, target)
	items := newVaultKeyRotationSourceItems(t, source, remote.identity, 1)
	rotation := newOpenVaultKeyRotation(t, source, target, remote.identity, items)
	stageVaultKeyRotationTestItem(t, source, target, remote.identity, rotation, &items[0])
	items[0].TargetWrappedDEK[0] ^= 0x80
	items[0].TargetWrapperSHA256 = vaultKeyRotationWrapperHash(items[0].TargetWrappedDEK)
	rotation.StagedCount = 1
	rotation.StagedPlanHash = vaultKeyRotationTestPlanHash(rotation, items)
	remote.rotation = rotation
	remote.items = items

	completed, err := service.RotateVaultKey(context.Background(), riskAcceptedRotationOptions())
	if completed != nil || !errors.Is(err, ErrIntegrity) {
		t.Fatalf("tampered staged wrapper result = %+v, %v, want integrity", completed, err)
	}
	if remote.stageCalls != 0 || remote.commitCalls != 0 {
		t.Fatal("tampered staged wrapper reached a mutation")
	}
	if stringsContainsAny(err.Error(), []string{items[0].DEKID, items[0].SecretID, items[0].TargetWrapperSHA256}) {
		t.Fatalf("integrity error disclosed wrapper metadata: %v", err)
	}
}

func TestRotateVaultKeyRejectsPlanHashNotMatchingVerifiedPages(t *testing.T) {
	service, remote, source := newVaultKeyRotationTestService(t)
	defer source.Clear()
	target := generateVaultKeyVersion(t, 2)
	defer target.Clear()
	createVaultKeyEpoch(t, target)
	items := newVaultKeyRotationSourceItems(t, source, remote.identity, 2)
	rotation := newOpenVaultKeyRotation(t, source, target, remote.identity, items)
	for index := range items {
		stageVaultKeyRotationTestItem(t, source, target, remote.identity, rotation, &items[index])
	}
	rotation.StagedCount = rotation.ItemCount
	rotation.StagedPlanHash = strings.Repeat("f", 64)
	if rotation.StagedPlanHash == vaultKeyRotationTestPlanHash(rotation, items) {
		rotation.StagedPlanHash = strings.Repeat("e", 64)
	}
	remote.rotation = rotation
	remote.items = items

	completed, err := service.RotateVaultKey(context.Background(), riskAcceptedRotationOptions())
	if completed != nil || !errors.Is(err, ErrIntegrity) || remote.commitCalls != 0 {
		t.Fatalf("wrong plan hash result = %+v, %v commit=%d", completed, err, remote.commitCalls)
	}
}

func TestRotateVaultKeyResolvesLostCommitThroughExactStatusAndCurrentBinding(t *testing.T) {
	service, remote, source := newVaultKeyRotationTestService(t)
	defer source.Clear()
	remote.items = newVaultKeyRotationSourceItems(t, source, remote.identity, 2)
	remote.loseCommitAfterApply = true

	completed, err := service.RotateVaultKey(context.Background(), riskAcceptedRotationOptions())
	if err != nil {
		t.Fatal(err)
	}
	if completed.LifecycleState != client.VaultKeyRotationCommitted || remote.commitCalls != 1 ||
		remote.getCalls < 2 || remote.current.ID != completed.TargetKeyID {
		t.Fatalf("ambiguous commit was not resolved: %+v commit=%d get=%d current=%+v",
			completed, remote.commitCalls, remote.getCalls, remote.current)
	}
}

func TestRotateVaultKeyFailsClosedOnOpenRotationBindingMismatch(t *testing.T) {
	service, remote, source := newVaultKeyRotationTestService(t)
	defer source.Clear()
	target := generateVaultKeyVersion(t, 2)
	defer target.Clear()
	createVaultKeyEpoch(t, target)
	items := newVaultKeyRotationSourceItems(t, source, remote.identity, 1)
	remote.rotation = newOpenVaultKeyRotation(t, source, target, remote.identity, items)
	remote.items = items
	other := generateKey(t)
	defer other.Clear()
	remote.current = bindingFor(other, remote.identity)
	remote.current.RowVersion = 9

	completed, err := service.RotateVaultKey(context.Background(), riskAcceptedRotationOptions())
	if completed != nil || !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("binding mismatch result = %+v, %v, want key mismatch", completed, err)
	}
	if remote.listCalls != 0 || remote.stageCalls != 0 || remote.commitCalls != 0 {
		t.Fatal("binding mismatch reached wrapper operations")
	}
}

func TestRotateVaultKeyConvergesLosingInstallationIntentOnCanonicalOpen(t *testing.T) {
	service, remote, source := newVaultKeyRotationTestService(t)
	defer source.Clear()
	losingTarget := generateVaultKeyVersion(t, 2)
	defer losingTarget.Clear()
	createVaultKeyEpoch(t, losingTarget)
	winnerTarget := generateVaultKeyVersion(t, 2)
	defer winnerTarget.Clear()
	items := newVaultKeyRotationSourceItems(t, source, remote.identity, 2)
	winner := newOpenVaultKeyRotation(t, source, winnerTarget, remote.identity, items)
	remote.rotation = winner
	remote.items = items
	losing := newOpenVaultKeyRotation(t, source, losingTarget, remote.identity, nil)
	losingIntent := vaultKeyRotationIntentFromRotation(
		losing, true, remote.current.RowVersion, mustRotationTestID(t, "op"),
	)
	if err := local.CreateAgentVaultKeyRotationIntent(
		testAccountName, testRealmName, testAgentName, *losingIntent,
	); err != nil {
		t.Fatal(err)
	}

	// This installation does not possess the winning target yet. It must stop
	// safely, but its durable intent must converge on the authenticated server
	// winner instead of remaining permanently pinned to its never-started ID.
	completed, err := service.RotateVaultKey(context.Background(), riskAcceptedRotationOptions())
	if completed != nil || !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("losing installation result = %+v, %v", completed, err)
	}
	intent, err := local.ReadAgentVaultKeyRotationIntent(testAccountName, testRealmName, testAgentName)
	if err != nil || !vaultKeyRotationMatchesIntent(winner, intent) ||
		intent.ExpectedSourceKeyRowVersion != 0 || intent.StartIdempotencyKey != "" {
		t.Fatalf("converged intent = %#v, %v", intent, err)
	}
	if remote.startCalls != 0 || remote.stageCalls != 0 || remote.commitCalls != 0 {
		t.Fatal("losing installation mutated the canonical open rotation without its target key")
	}

	// Simulate making the winner's target available to this installation. The
	// same journal now resumes the winner; no second Start is ever attempted.
	createVaultKeyEpoch(t, winnerTarget)
	completed, err = service.RotateVaultKey(context.Background(), riskAcceptedRotationOptions())
	if err != nil || completed == nil || completed.ID != winner.ID ||
		completed.LifecycleState != client.VaultKeyRotationCommitted {
		t.Fatalf("canonical winner resume = %+v, %v", completed, err)
	}
	if remote.startCalls != 0 || remote.commitCalls != 1 {
		t.Fatalf("winner resume calls start/commit = %d/%d", remote.startCalls, remote.commitCalls)
	}
}

func TestCancelVaultKeyRotationConvergesLosingIntentWithoutSelfOrKeys(t *testing.T) {
	service, remote, source := newVaultKeyRotationTestService(t)
	defer source.Clear()
	losingTarget := generateVaultKeyVersion(t, 2)
	defer losingTarget.Clear()
	winnerSource := generateVaultKeyVersion(t, 2)
	defer winnerSource.Clear()
	winnerTarget := generateVaultKeyVersion(t, 3)
	defer winnerTarget.Clear()
	winner := newOpenVaultKeyRotation(t, winnerSource, winnerTarget, remote.identity, nil)
	remote.rotation = winner
	remote.current = bindingFor(winnerSource, remote.identity)
	remote.current.RowVersion = 11
	losing := newOpenVaultKeyRotation(t, source, losingTarget, remote.identity, nil)
	losingIntent := vaultKeyRotationIntentFromRotation(
		losing, true, remote.current.RowVersion, mustRotationTestID(t, "op"),
	)
	if err := local.CreateAgentVaultKeyRotationIntent(
		testAccountName, testRealmName, testAgentName, *losingIntent,
	); err != nil {
		t.Fatal(err)
	}
	remote.selfErr = errors.New("self forbidden while suspended")
	remote.currentErr = errors.New("current key forbidden while suspended")

	cancelled, err := service.CancelVaultKeyRotation(context.Background(), winner.ID)
	if err != nil || cancelled == nil || cancelled.ID != winner.ID ||
		cancelled.LifecycleState != client.VaultKeyRotationCancelled {
		t.Fatalf("canonical cancellation = %+v, %v", cancelled, err)
	}
	if remote.selfCalls != 0 || remote.currentCalls != 0 || remote.startCalls != 0 {
		t.Fatalf("cancel used forbidden operations self/current/start = %d/%d/%d",
			remote.selfCalls, remote.currentCalls, remote.startCalls)
	}
	intent, err := local.ReadAgentVaultKeyRotationIntent(testAccountName, testRealmName, testAgentName)
	if err != nil || !vaultKeyRotationMatchesIntent(cancelled, intent) {
		t.Fatalf("cancelled canonical intent = %#v, %v", intent, err)
	}
}

func TestRotateVaultKeyRetiresExactPristineIntentAfterCanonicalAdvance(t *testing.T) {
	service, remote, source := newVaultKeyRotationTestService(t)
	defer source.Clear()
	losingTarget := generateVaultKeyVersion(t, 2)
	defer losingTarget.Clear()
	createVaultKeyEpoch(t, losingTarget)
	losing := newOpenVaultKeyRotation(t, source, losingTarget, remote.identity, nil)
	losingIntent := vaultKeyRotationIntentFromRotation(
		losing, true, remote.current.RowVersion, mustRotationTestID(t, "op"),
	)
	if err := local.CreateAgentVaultKeyRotationIntent(
		testAccountName, testRealmName, testAgentName, *losingIntent,
	); err != nil {
		t.Fatal(err)
	}
	winner := generateVaultKeyVersion(t, 2)
	defer winner.Clear()
	remote.current = bindingFor(winner, remote.identity)
	remote.current.RowVersion = 12
	remote.rotation = nil

	completed, err := service.RotateVaultKey(context.Background(), riskAcceptedRotationOptions())
	if completed != nil || !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("advanced canonical result = %+v, %v", completed, err)
	}
	if remote.startCalls != 0 || remote.stageCalls != 0 || remote.commitCalls != 0 {
		t.Fatal("stale pristine intent retirement mutated remote rotation state")
	}
	if _, err := local.ReadAgentVaultKeyRotationIntent(testAccountName, testRealmName, testAgentName); !errors.Is(err, local.ErrAgentVaultKeyRotationIntentUnavailable) {
		t.Fatalf("stale pristine intent was not retired: %v", err)
	}
}

func TestRotateVaultKeyNeverRetiresPristineIntentWithoutStrictCanonicalAdvance(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*testing.T, *fakeVaultKeyRotationRemote, *sealed.AgentVaultKey)
		want      error
	}{
		{
			name: "transient current failure",
			configure: func(_ *testing.T, remote *fakeVaultKeyRotationRemote, _ *sealed.AgentVaultKey) {
				remote.currentErr = errors.New("temporarily unavailable")
			},
			want: ErrOperation,
		},
		{
			name: "same version mismatch",
			configure: func(t *testing.T, remote *fakeVaultKeyRotationRemote, _ *sealed.AgentVaultKey) {
				other := generateVaultKeyVersion(t, 1)
				t.Cleanup(other.Clear)
				remote.current = bindingFor(other, remote.identity)
				remote.current.RowVersion = 9
			},
			want: ErrKeyMismatch,
		},
		{
			name: "newer open source is not current",
			configure: func(t *testing.T, remote *fakeVaultKeyRotationRemote, _ *sealed.AgentVaultKey) {
				current := generateVaultKeyVersion(t, 2)
				openSource := generateVaultKeyVersion(t, 2)
				openTarget := generateVaultKeyVersion(t, 3)
				t.Cleanup(current.Clear)
				t.Cleanup(openSource.Clear)
				t.Cleanup(openTarget.Clear)
				remote.current = bindingFor(current, remote.identity)
				remote.current.RowVersion = 10
				remote.rotation = newOpenVaultKeyRotation(t, openSource, openTarget, remote.identity, nil)
			},
			want: ErrIntegrity,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, remote, source := newVaultKeyRotationTestService(t)
			defer source.Clear()
			losingTarget := createPreparedLosingVaultKeyRotationIntent(t, remote, source)
			defer losingTarget.Clear()
			test.configure(t, remote, source)
			completed, err := service.RotateVaultKey(context.Background(), riskAcceptedRotationOptions())
			if completed != nil || !errors.Is(err, test.want) {
				t.Fatalf("result = %+v, %v, want %v", completed, err, test.want)
			}
			if _, err := local.ReadAgentVaultKeyRotationIntent(testAccountName, testRealmName, testAgentName); err != nil {
				t.Fatalf("insufficient evidence retired intent: %v", err)
			}
			if remote.startCalls != 0 || remote.stageCalls != 0 || remote.commitCalls != 0 {
				t.Fatal("insufficient canonical evidence reached rotation mutation")
			}
		})
	}
}

func TestVaultKeyRotationStatusAndCancelAreValueFreeAndIdempotent(t *testing.T) {
	service, remote, source := newVaultKeyRotationTestService(t)
	defer source.Clear()
	target := generateVaultKeyVersion(t, 2)
	defer target.Clear()
	items := newVaultKeyRotationSourceItems(t, source, remote.identity, 1)
	rotation := newOpenVaultKeyRotation(t, source, target, remote.identity, items)
	remote.rotation = rotation
	remote.items = items

	open, err := service.OpenVaultKeyRotation(context.Background())
	if err != nil || open == nil || open.ID != rotation.ID {
		t.Fatalf("open status = %+v, %v", open, err)
	}
	status, err := service.VaultKeyRotationStatus(context.Background(), rotation.ID)
	if err != nil || status.ID != rotation.ID || remote.stageCalls != 0 || remote.commitCalls != 0 {
		t.Fatalf("exact status = %+v, %v", status, err)
	}
	cancelled, err := service.CancelVaultKeyRotation(context.Background(), "")
	if err != nil || cancelled == nil || cancelled.LifecycleState != client.VaultKeyRotationCancelled ||
		remote.cancelCalls != 1 {
		t.Fatalf("cancel = %+v, %v calls=%d", cancelled, err, remote.cancelCalls)
	}
	// A crash after the canonical cancellation but before CLI acknowledgement
	// must be recoverable even when the original command omitted the ID. The
	// open endpoint is now empty, so this can only succeed through the journal.
	again, err := service.CancelVaultKeyRotation(context.Background(), "")
	if err != nil || again == nil || again.LifecycleState != client.VaultKeyRotationCancelled ||
		remote.cancelCalls != 1 {
		t.Fatalf("idempotent cancel = %+v, %v calls=%d", again, err, remote.cancelCalls)
	}
	if _, err := service.VaultKeyRotationStatus(context.Background(), "not-an-id"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid status id error = %v, want invalid input", err)
	}
}

func TestVaultKeyRotationLifecycleReadsAndCancelDoNotRequireActiveSelfOrKeys(t *testing.T) {
	service, remote, source := newVaultKeyRotationTestService(t)
	defer source.Clear()
	target := generateVaultKeyVersion(t, 2)
	defer target.Clear()
	items := newVaultKeyRotationSourceItems(t, source, remote.identity, 1)
	rotation := newOpenVaultKeyRotation(t, source, target, remote.identity, items)
	remote.rotation = rotation
	remote.items = items
	remote.selfErr = errors.New("self forbidden while suspended")
	remote.currentErr = errors.New("current key forbidden while suspended")

	open, err := service.OpenVaultKeyRotation(context.Background())
	if err != nil || open == nil || open.ID != rotation.ID {
		t.Fatalf("suspended open = %+v, %v", open, err)
	}
	status, err := service.VaultKeyRotationStatus(context.Background(), rotation.ID)
	if err != nil || status == nil || status.ID != rotation.ID {
		t.Fatalf("suspended status = %+v, %v", status, err)
	}
	cancelled, err := service.CancelVaultKeyRotation(context.Background(), rotation.ID)
	if err != nil || cancelled == nil || cancelled.LifecycleState != client.VaultKeyRotationCancelled {
		t.Fatalf("suspended cancel = %+v, %v", cancelled, err)
	}
	if remote.selfCalls != 0 || remote.currentCalls != 0 {
		t.Fatalf("value-free lifecycle called self/current = %d/%d", remote.selfCalls, remote.currentCalls)
	}
	intent, err := local.ReadAgentVaultKeyRotationIntent(testAccountName, testRealmName, testAgentName)
	if err != nil || intent.RotationID != rotation.ID {
		t.Fatalf("cancel intent = %#v, %v", intent, err)
	}
	if err := service.AcknowledgeVaultKeyRotation(context.Background(), rotation.ID); err != nil {
		t.Fatalf("suspended acknowledgement: %v", err)
	}
	if remote.selfCalls != 0 || remote.currentCalls != 0 {
		t.Fatal("suspended acknowledgement called self/current")
	}
}

func TestAcknowledgeVaultKeyRotationRequiresExactTerminalIntent(t *testing.T) {
	service, remote, source := newVaultKeyRotationTestService(t)
	defer source.Clear()
	target := generateVaultKeyVersion(t, 2)
	defer target.Clear()
	items := newVaultKeyRotationSourceItems(t, source, remote.identity, 1)
	rotation := newOpenVaultKeyRotation(t, source, target, remote.identity, items)
	remote.rotation = rotation
	remote.items = items
	if err := service.adoptVaultKeyRotationIntent(rotation); err != nil {
		t.Fatal(err)
	}
	if err := service.AcknowledgeVaultKeyRotation(context.Background(), rotation.ID); !errors.Is(err, ErrOperation) {
		t.Fatalf("open acknowledgement error = %v", err)
	}
	otherID := mustRotationTestID(t, "vkr")
	if err := service.AcknowledgeVaultKeyRotation(context.Background(), otherID); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("wrong-id acknowledgement error = %v", err)
	}
	if _, err := local.ReadAgentVaultKeyRotationIntent(testAccountName, testRealmName, testAgentName); err != nil {
		t.Fatalf("rejected acknowledgement deleted intent: %v", err)
	}
}

func TestCancelVaultKeyRotationReturnsCommittedRaceWinnerForAcknowledgement(t *testing.T) {
	service, remote, source := newVaultKeyRotationTestService(t)
	defer source.Clear()
	target := generateVaultKeyVersion(t, 2)
	defer target.Clear()
	rotation := newOpenVaultKeyRotation(t, source, target, remote.identity, nil)
	if err := service.adoptVaultKeyRotationIntent(rotation); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rotation.LifecycleState = client.VaultKeyRotationCommitted
	rotation.RecoveryDispositionMode = client.VaultKeyRotationRiskAccepted
	rotation.StagedPlanHash = ""
	rotation.RowVersion++
	rotation.UpdatedAt = now
	rotation.CommittedAt = &now
	remote.rotation = rotation
	remote.selfErr = errors.New("self forbidden while suspended")
	remote.currentErr = errors.New("current key forbidden while suspended")

	terminal, err := service.CancelVaultKeyRotation(context.Background(), rotation.ID)
	if err != nil || terminal == nil || terminal.ID != rotation.ID ||
		terminal.LifecycleState != client.VaultKeyRotationCommitted {
		t.Fatalf("committed cancellation race = %+v, %v", terminal, err)
	}
	if remote.cancelCalls != 0 || remote.selfCalls != 0 || remote.currentCalls != 0 {
		t.Fatal("terminal cancellation race invoked a mutation or active-only route")
	}
	if err := service.AcknowledgeVaultKeyRotation(context.Background(), rotation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := local.ReadAgentVaultKeyRotationIntent(testAccountName, testRealmName, testAgentName); !errors.Is(err, local.ErrAgentVaultKeyRotationIntentUnavailable) {
		t.Fatalf("committed terminal intent remained after acknowledgement: %v", err)
	}
}

type fakeVaultKeyRotationRemote struct {
	*fakeRemote

	rotation *client.VaultKeyRotation
	items    []client.VaultKeyRotationItem
	maxPage  int

	startCalls, getOpenCalls, getCalls, listCalls int
	stageCalls, commitCalls, cancelCalls          int
	startInputs                                   []client.StartVaultKeyRotationInput
	stageInputs                                   []client.StageVaultKeyRotationInput
	commitInputs                                  []client.CommitVaultKeyRotationInput

	failStartBeforeApply  int
	failStageBeforeApply  int
	failCommitBeforeApply int
	loseStartAfterApply   bool
	loseStageAfterApply   bool
	loseCommitAfterApply  bool
	targetMissingAtStart  bool
	intentMissingAtStart  bool
	beforeCommit          func(client.CommitVaultKeyRotationInput) error
	mutateCommitResponse  func(*client.VaultKeyRotation)
	mutateGetResponse     func(*client.VaultKeyRotation)
}

type fakeVaultKeyRotationRecoverySink struct {
	artifact      []byte
	putCalls      int
	readCalls     int
	failBeforePut bool
	putErr        error
	beforePut     func(sealed.AVKRecoveryMetadata, []byte)
}

func (sink *fakeVaultKeyRotationRecoverySink) PutIfAbsent(
	ctx context.Context,
	metadata sealed.AVKRecoveryMetadata,
	artifact []byte,
) error {
	sink.putCalls++
	if err := ctx.Err(); err != nil {
		return err
	}
	if sink.beforePut != nil {
		sink.beforePut(metadata, artifact)
	}
	if sink.artifact != nil {
		return ErrVaultKeyRotationRecoveryExists
	}
	if sink.failBeforePut {
		return sink.putErr
	}
	sink.artifact = append([]byte(nil), artifact...)
	return sink.putErr
}

func (sink *fakeVaultKeyRotationRecoverySink) ReadBack(ctx context.Context) ([]byte, error) {
	sink.readCalls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sink.artifact == nil {
		return nil, ErrVaultKeyRotationRecoveryUnavailable
	}
	return append([]byte(nil), sink.artifact...), nil
}

func newVaultKeyRotationTestService(t *testing.T) (*Service, *fakeVaultKeyRotationRemote, *sealed.AgentVaultKey) {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	base := newFakeRemote()
	source := generateKey(t)
	createLocalKey(t, source)
	base.current = bindingFor(source, base.identity)
	base.current.RowVersion = 7
	remote := &fakeVaultKeyRotationRemote{fakeRemote: base}
	service := &Service{
		remote: remote, accountID: testAccountID,
		accountName: testAccountName, realmName: testRealmName, agentName: testAgentName,
	}
	return service, remote, source
}

func (f *fakeVaultKeyRotationRemote) getOpenVaultKeyRotation(context.Context) (*client.VaultKeyRotation, error) {
	f.getOpenCalls++
	if f.rotation == nil || f.rotation.LifecycleState != client.VaultKeyRotationOpen {
		return nil, nil
	}
	return cloneVaultKeyRotation(f.rotation), nil
}

func (f *fakeVaultKeyRotationRemote) getVaultKeyRotation(_ context.Context, rotationID string) (*client.VaultKeyRotation, error) {
	f.getCalls++
	if f.rotation == nil || f.rotation.ID != rotationID {
		return nil, client.ErrNotFound
	}
	result := cloneVaultKeyRotation(f.rotation)
	if f.mutateGetResponse != nil {
		f.mutateGetResponse(result)
	}
	return result, nil
}

func (f *fakeVaultKeyRotationRemote) listVaultKeyRotationItems(_ context.Context, rotationID string, options client.VaultKeyRotationItemListOptions) (*client.VaultKeyRotationItemPage, error) {
	f.listCalls++
	if f.rotation == nil || f.rotation.ID != rotationID {
		return nil, errors.New("not found")
	}
	items := cloneVaultKeyRotationItems(f.items)
	sort.Slice(items, func(i, j int) bool { return items[i].DEKID < items[j].DEKID })
	start := 0
	if options.Cursor != "" {
		for start < len(items) && items[start].DEKID <= options.Cursor {
			start++
		}
	}
	limit := options.Limit
	if limit <= 0 {
		limit = vaultKeyRotationPageSize
	}
	if f.maxPage > 0 && f.maxPage < limit {
		limit = f.maxPage
	}
	end := start + limit
	if end > len(items) {
		end = len(items)
	}
	page := &client.VaultKeyRotationItemPage{Items: items[start:end]}
	if end < len(items) && end > start {
		page.NextCursor = items[end-1].DEKID
	}
	return page, nil
}

func (f *fakeVaultKeyRotationRemote) startVaultKeyRotation(_ context.Context, input client.StartVaultKeyRotationInput) (*client.VaultKeyRotationMutationResult, error) {
	f.startCalls++
	f.startInputs = append(f.startInputs, input)
	if f.failStartBeforeApply > 0 {
		f.failStartBeforeApply--
		return nil, errors.New("response unavailable")
	}
	if f.rotation == nil {
		key, err := local.ReadAgentVaultKeyEpoch(testAccountName, testRealmName, testAgentName,
			input.TargetKeyID, uint64(input.TargetKeyVersion))
		if err != nil {
			f.targetMissingAtStart = true
			return nil, errors.New("target not durable")
		}
		key.Clear()
		intent, intentErr := local.ReadAgentVaultKeyRotationIntent(testAccountName, testRealmName, testAgentName)
		if intentErr != nil || intent.RotationID != input.ID || intent.Source.ID != input.ExpectedSourceKeyID ||
			intent.Source.Version != uint64(input.ExpectedSourceKeyVersion) ||
			intent.ExpectedSourceKeyRowVersion != input.ExpectedSourceKeyRowVersion ||
			intent.Target.ID != input.TargetKeyID || intent.Target.Version != uint64(input.TargetKeyVersion) ||
			intent.Target.Algorithm != input.TargetAlgorithm || intent.Target.Fingerprint != input.TargetFingerprint ||
			intent.StartIdempotencyKey != input.IdempotencyKey {
			f.intentMissingAtStart = true
			return nil, errors.New("rotation intent not durable")
		}
		now := time.Now().UTC()
		f.rotation = &client.VaultKeyRotation{
			ID: input.ID, AccountID: f.identity.AccountID, RealmID: f.identity.RealmID,
			OwnerAgentID: f.identity.AgentID,
			SourceKeyID:  input.ExpectedSourceKeyID, SourceKeyVersion: input.ExpectedSourceKeyVersion,
			SourceKeyAlgorithm: f.current.Algorithm, SourceKeyFingerprint: f.current.Fingerprint,
			TargetKeyID: input.TargetKeyID, TargetKeyVersion: input.TargetKeyVersion,
			TargetKeyAlgorithm: input.TargetAlgorithm, TargetKeyFingerprint: input.TargetFingerprint,
			LifecycleState: client.VaultKeyRotationOpen, ItemCount: int64(len(f.items)), RowVersion: 1,
			CreatedAt: now, UpdatedAt: now,
		}
		for index := range f.items {
			f.items[index].RotationID = input.ID
			f.items[index].TargetWrappingKeyID = input.TargetKeyID
			f.items[index].TargetWrappingKeyVersion = input.TargetKeyVersion
		}
		if len(f.items) == 0 {
			f.rotation.StagedPlanHash = vaultKeyRotationTestPlanHash(f.rotation, f.items)
		}
	} else if f.rotation.ID != input.ID {
		return nil, errors.New("rotation in progress")
	}
	if f.loseStartAfterApply {
		f.loseStartAfterApply = false
		return nil, errors.New("response unavailable")
	}
	return &client.VaultKeyRotationMutationResult{Rotation: *cloneVaultKeyRotation(f.rotation)}, nil
}

func (f *fakeVaultKeyRotationRemote) stageVaultKeyRotation(_ context.Context, rotationID string, input client.StageVaultKeyRotationInput) (*client.VaultKeyRotationMutationResult, error) {
	f.stageCalls++
	f.stageInputs = append(f.stageInputs, cloneVaultKeyRotationStageInput(input))
	if f.failStageBeforeApply > 0 {
		f.failStageBeforeApply--
		return nil, errors.New("response unavailable")
	}
	if f.rotation == nil || f.rotation.ID != rotationID || f.rotation.LifecycleState != client.VaultKeyRotationOpen ||
		f.rotation.RowVersion != input.ExpectedRotationRowVersion {
		return nil, errors.New("stage conflict")
	}
	newlyStaged := int64(0)
	now := time.Now().UTC()
	for _, candidate := range input.Items {
		index := -1
		for itemIndex := range f.items {
			if f.items[itemIndex].DEKID == candidate.DEKID {
				index = itemIndex
				break
			}
		}
		if index < 0 || f.items[index].SourceDEKRowVersion != candidate.ExpectedSourceDEKRowVersion ||
			f.items[index].SourceWrapRevision != candidate.ExpectedSourceWrapRevision {
			return nil, errors.New("stage item conflict")
		}
		item := &f.items[index]
		if item.StagedAt != nil {
			if item.TargetWrapRevision != candidate.TargetWrapRevision ||
				!bytes.Equal(item.TargetWrappedDEK, candidate.TargetWrappedDEK) {
				return nil, errors.New("stage replay conflict")
			}
			continue
		}
		item.TargetWrappedDEK = append([]byte(nil), candidate.TargetWrappedDEK...)
		item.TargetWrapRevision = candidate.TargetWrapRevision
		item.TargetWrapperSHA256 = vaultKeyRotationWrapperHash(candidate.TargetWrappedDEK)
		item.StagedAt = &now
		newlyStaged++
	}
	if newlyStaged > 0 {
		f.rotation.StagedCount += newlyStaged
		f.rotation.RowVersion++
		f.rotation.UpdatedAt = now
	}
	if f.rotation.StagedCount == f.rotation.ItemCount {
		f.rotation.StagedPlanHash = vaultKeyRotationTestPlanHash(f.rotation, f.items)
	}
	if f.loseStageAfterApply {
		f.loseStageAfterApply = false
		return nil, errors.New("response unavailable")
	}
	return &client.VaultKeyRotationMutationResult{Rotation: *cloneVaultKeyRotation(f.rotation)}, nil
}

func (f *fakeVaultKeyRotationRemote) commitVaultKeyRotation(_ context.Context, rotationID string, input client.CommitVaultKeyRotationInput) (*client.VaultKeyRotationMutationResult, error) {
	f.commitCalls++
	f.commitInputs = append(f.commitInputs, input)
	if f.beforeCommit != nil {
		if err := f.beforeCommit(input); err != nil {
			return nil, err
		}
	}
	if f.rotation == nil || f.rotation.ID != rotationID || f.rotation.LifecycleState != client.VaultKeyRotationOpen ||
		f.rotation.RowVersion != input.ExpectedRotationRowVersion ||
		f.rotation.ItemCount != input.ExpectedItemCount || f.rotation.StagedCount != f.rotation.ItemCount ||
		f.rotation.StagedPlanHash != input.ExpectedPlanHash ||
		!validVaultKeyRotationRecoveryDisposition(
			input.RecoveryDisposition.Mode, input.RecoveryDisposition.ArtifactSHA256,
		) {
		return nil, errors.New("commit conflict")
	}
	if f.failCommitBeforeApply > 0 {
		f.failCommitBeforeApply--
		return nil, errors.New("response unavailable")
	}
	now := time.Now().UTC()
	f.rotation.LifecycleState = client.VaultKeyRotationCommitted
	f.rotation.RowVersion++
	f.rotation.UpdatedAt = now
	f.rotation.CommittedAt = &now
	f.rotation.StagedPlanHash = ""
	f.rotation.RecoveryDispositionMode = input.RecoveryDisposition.Mode
	f.rotation.RecoveryArtifactSHA256 = input.RecoveryDisposition.ArtifactSHA256
	f.current = &client.VaultKeyBinding{
		ID: f.rotation.TargetKeyID, AccountID: f.identity.AccountID, RealmID: f.identity.RealmID,
		OwnerAgentID: f.identity.AgentID, KeyVersion: f.rotation.TargetKeyVersion,
		Algorithm: f.rotation.TargetKeyAlgorithm, Fingerprint: f.rotation.TargetKeyFingerprint,
		LifecycleState: "current", RowVersion: 1,
	}
	if f.loseCommitAfterApply {
		f.loseCommitAfterApply = false
		return nil, errors.New("response unavailable")
	}
	response := cloneVaultKeyRotation(f.rotation)
	if f.mutateCommitResponse != nil {
		f.mutateCommitResponse(response)
	}
	return &client.VaultKeyRotationMutationResult{Rotation: *response}, nil
}

func (f *fakeVaultKeyRotationRemote) cancelVaultKeyRotation(_ context.Context, rotationID string, input client.CancelVaultKeyRotationInput) (*client.VaultKeyRotationMutationResult, error) {
	f.cancelCalls++
	if f.rotation == nil || f.rotation.ID != rotationID || f.rotation.LifecycleState != client.VaultKeyRotationOpen ||
		f.rotation.RowVersion != input.ExpectedRotationRowVersion {
		return nil, errors.New("cancel conflict")
	}
	now := time.Now().UTC()
	f.rotation.LifecycleState = client.VaultKeyRotationCancelled
	f.rotation.RowVersion++
	f.rotation.UpdatedAt = now
	f.rotation.CancelledAt = &now
	f.rotation.StagedPlanHash = ""
	return &client.VaultKeyRotationMutationResult{Rotation: *cloneVaultKeyRotation(f.rotation)}, nil
}

func newVaultKeyRotationSourceItems(t *testing.T, source *sealed.AgentVaultKey, identity client.SelfIdentity, count int) []client.VaultKeyRotationItem {
	t.Helper()
	items := make([]client.VaultKeyRotationItem, count)
	for index := range items {
		secretID := mustRotationTestID(t, "sec")
		fieldID := mustRotationTestID(t, "fld")
		kind := "password"
		if index%2 == 1 {
			kind = "totp"
		}
		domain, _ := fieldDomain(kind)
		envelope, err := sealed.SealSensitiveField(source, []byte(fmt.Sprintf("rotation-value-%d", index)), sealed.SensitiveFieldOptions{
			Scope: sealed.FieldScope{
				Domain: domain, AccountID: identity.AccountID, RealmID: identity.RealmID,
				OwnerAgentID: identity.AgentID, SecretID: secretID, FieldID: fieldID,
			},
			ValueVersion: 1, DEKGeneration: 1, ValueEncoding: sealed.ValueEncodingUTF8, WrapRevision: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		clear(envelope.Ciphertext)
		items[index] = client.VaultKeyRotationItem{
			SecretID: secretID, FieldID: fieldID, FieldKind: kind,
			DEKID: envelope.DEKID, DEKGeneration: int64(envelope.DEKGeneration),
			SourceDEKRowVersion: int64(index + 1), SourceWrapRevision: int64(envelope.WrapRevision),
			SourceWrappedDEK:    append([]byte(nil), envelope.WrappedDEK...),
			SourceWrapAlgorithm: envelope.WrapAlgorithm, SourceAADVersion: int64(envelope.AADVersion),
			SourceWrappingKeyID:      envelope.WrappingKeyID,
			SourceWrappingKeyVersion: int64(envelope.WrappingKeyVersion),
		}
		clear(envelope.WrappedDEK)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].DEKID < items[j].DEKID })
	return items
}

func newOpenVaultKeyRotation(t *testing.T, source, target *sealed.AgentVaultKey, identity client.SelfIdentity, items []client.VaultKeyRotationItem) *client.VaultKeyRotation {
	t.Helper()
	now := time.Now().UTC()
	rotation := &client.VaultKeyRotation{
		ID: mustRotationTestID(t, "vkr"), AccountID: identity.AccountID, RealmID: identity.RealmID,
		OwnerAgentID: identity.AgentID,
		SourceKeyID:  source.ID(), SourceKeyVersion: int64(source.Version()),
		SourceKeyAlgorithm: source.Algorithm(), SourceKeyFingerprint: source.Fingerprint(),
		TargetKeyID: target.ID(), TargetKeyVersion: int64(target.Version()),
		TargetKeyAlgorithm: target.Algorithm(), TargetKeyFingerprint: target.Fingerprint(),
		LifecycleState: client.VaultKeyRotationOpen, ItemCount: int64(len(items)), RowVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	for index := range items {
		items[index].RotationID = rotation.ID
		items[index].TargetWrappingKeyID = target.ID()
		items[index].TargetWrappingKeyVersion = int64(target.Version())
	}
	if len(items) == 0 {
		rotation.StagedPlanHash = vaultKeyRotationTestPlanHash(rotation, items)
	}
	return rotation
}

func stageVaultKeyRotationTestItem(t *testing.T, source, target *sealed.AgentVaultKey, identity client.SelfIdentity, rotation *client.VaultKeyRotation, item *client.VaultKeyRotationItem) {
	t.Helper()
	sourceWrapper, scope, err := vaultKeyRotationSourceWrapper(identity, rotation, item)
	if err != nil {
		t.Fatal(err)
	}
	targetWrapper, err := sealed.RewrapDEK(source, target, scope, sourceWrapper, uint64(item.SourceWrapRevision+1))
	if err != nil {
		t.Fatal(err)
	}
	item.TargetWrappedDEK = append([]byte(nil), targetWrapper.WrappedDEK...)
	item.TargetWrapRevision = int64(targetWrapper.WrapRevision)
	item.TargetWrapperSHA256 = vaultKeyRotationWrapperHash(item.TargetWrappedDEK)
	now := time.Now().UTC()
	item.StagedAt = &now
	clear(targetWrapper.WrappedDEK)
}

func vaultKeyRotationTestPlanHash(rotation *client.VaultKeyRotation, items []client.VaultKeyRotationItem) string {
	ordered := cloneVaultKeyRotationItems(items)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].DEKID < ordered[j].DEKID })
	hasher := newClientVaultKeyRotationPlanHasher(rotation)
	for _, item := range ordered {
		hasher.Add(item)
	}
	return hasher.Sum()
}

func generateVaultKeyVersion(t *testing.T, version uint64) *sealed.AgentVaultKey {
	t.Helper()
	key, err := sealed.GenerateAgentVaultKey(version)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func createVaultKeyEpoch(t *testing.T, key *sealed.AgentVaultKey) {
	t.Helper()
	if err := local.CreateAgentVaultKeyEpoch(testAccountName, testRealmName, testAgentName, key); err != nil {
		t.Fatal(err)
	}
}

func createPreparedLosingVaultKeyRotationIntent(t *testing.T, remote *fakeVaultKeyRotationRemote, source *sealed.AgentVaultKey) *sealed.AgentVaultKey {
	t.Helper()
	target := generateVaultKeyVersion(t, source.Version()+1)
	createVaultKeyEpoch(t, target)
	losing := newOpenVaultKeyRotation(t, source, target, remote.identity, nil)
	intent := vaultKeyRotationIntentFromRotation(
		losing, true, remote.current.RowVersion, mustRotationTestID(t, "op"),
	)
	if err := local.CreateAgentVaultKeyRotationIntent(
		testAccountName, testRealmName, testAgentName, *intent,
	); err != nil {
		t.Fatal(err)
	}
	return target
}

func mustRotationTestID(t *testing.T, prefix string) string {
	t.Helper()
	value, err := id.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func cloneVaultKeyRotation(value *client.VaultKeyRotation) *client.VaultKeyRotation {
	if value == nil {
		return nil
	}
	clone := *value
	if value.CommittedAt != nil {
		committed := *value.CommittedAt
		clone.CommittedAt = &committed
	}
	if value.CancelledAt != nil {
		cancelled := *value.CancelledAt
		clone.CancelledAt = &cancelled
	}
	return &clone
}

func cloneVaultKeyRotationItems(items []client.VaultKeyRotationItem) []client.VaultKeyRotationItem {
	clones := make([]client.VaultKeyRotationItem, len(items))
	for index := range items {
		clones[index] = items[index]
		clones[index].SourceWrappedDEK = append([]byte(nil), items[index].SourceWrappedDEK...)
		clones[index].TargetWrappedDEK = append([]byte(nil), items[index].TargetWrappedDEK...)
		if items[index].StagedAt != nil {
			staged := *items[index].StagedAt
			clones[index].StagedAt = &staged
		}
	}
	return clones
}

func cloneVaultKeyRotationStageInput(input client.StageVaultKeyRotationInput) client.StageVaultKeyRotationInput {
	clone := input
	clone.Items = make([]client.StageVaultKeyRotationItemInput, len(input.Items))
	copy(clone.Items, input.Items)
	for index := range clone.Items {
		clone.Items[index].TargetWrappedDEK = append([]byte(nil), input.Items[index].TargetWrappedDEK...)
	}
	return clone
}

func stringsContainsAny(value string, candidates []string) bool {
	for _, candidate := range candidates {
		if candidate != "" && bytes.Contains([]byte(value), []byte(candidate)) {
			return true
		}
	}
	return false
}

func equalVaultKeyRotationStageItems(first, second []client.StageVaultKeyRotationItemInput) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index].DEKID != second[index].DEKID ||
			first[index].ExpectedSourceDEKRowVersion != second[index].ExpectedSourceDEKRowVersion ||
			first[index].ExpectedSourceWrapRevision != second[index].ExpectedSourceWrapRevision ||
			first[index].TargetWrapRevision != second[index].TargetWrapRevision ||
			!bytes.Equal(first[index].TargetWrappedDEK, second[index].TargetWrappedDEK) {
			return false
		}
	}
	return true
}

func riskAcceptedRotationOptions() RotateVaultKeyOptions {
	return RotateVaultKeyOptions{AcceptUnrecoverableKeyLoss: true}
}

func exportVaultKeyRotationRecoveryForTest(
	t *testing.T,
	target *sealed.AgentVaultKey,
	identity client.SelfIdentity,
	passphrase []byte,
) []byte {
	t.Helper()
	artifact, err := sealed.ExportAgentVaultKeyRecovery(target, passphrase, recoveryScope(identity))
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func replaceRotationRecoveryDispositionForTest(rotation *client.VaultKeyRotation) {
	rotation.RecoveryDispositionMode = client.VaultKeyRotationRecoveryArtifact
	rotation.RecoveryArtifactSHA256 = strings.Repeat("a", 64)
}
