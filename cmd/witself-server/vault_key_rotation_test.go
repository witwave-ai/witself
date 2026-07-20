package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

func TestConfigureSecretsWiresVaultKeyRotationVertical(t *testing.T) {
	var cfg server.Config
	configureSecrets(&cfg, &store.Store{})
	callbacks := map[string]bool{
		"start":    cfg.StartVaultKeyRotation != nil,
		"get open": cfg.GetOpenVaultKeyRotation != nil,
		"get":      cfg.GetVaultKeyRotation != nil,
		"list":     cfg.ListVaultKeyRotationItems != nil,
		"stage":    cfg.StageVaultKeyRotation != nil,
		"commit":   cfg.CommitVaultKeyRotation != nil,
		"cancel":   cfg.CancelVaultKeyRotation != nil,
	}
	for name, wired := range callbacks {
		if !wired {
			t.Errorf("%s rotation callback is nil", name)
		}
	}
}

func TestMapSecretErrorMapsVaultKeyRotationErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		in   error
		want error
	}{
		{name: "not found", in: store.ErrVaultKeyRotationNotFound, want: server.ErrNotFound},
		{name: "already open", in: store.ErrVaultKeyRotationInProgress, want: server.ErrConflict},
		{name: "cross lifecycle", in: store.ErrVaultLifecycleInProgress, want: server.ErrConflict},
		{name: "fence conflict", in: store.ErrVaultKeyRotationConflict, want: server.ErrConflict},
		{name: "incomplete", in: store.ErrVaultKeyRotationIncomplete, want: server.ErrConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := mapSecretError(test.in); !errors.Is(got, test.want) {
				t.Fatalf("mapSecretError(%v) = %v, want %v", test.in, got, test.want)
			}
		})
	}
}

func TestVaultKeyRotationStoreToServerProjectionPreservesOpaqueFences(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	rotation := toServerVaultKeyRotation(store.VaultKeyRotation{
		ID: "vkr_abcdefghijklmnop", AccountID: "acc_1", RealmID: "realm_1",
		OwnerAgentID: "agent_1", SourceKeyID: "avk_abcdefghijklmnop", SourceKeyVersion: 1,
		SourceKeyAlgorithm: store.SecretAEADAlgorithm, SourceKeyFingerprint: strings.Repeat("a", 64),
		TargetKeyID: "avk_bcdefghijklmnopq", TargetKeyVersion: 2,
		TargetKeyAlgorithm: store.SecretAEADAlgorithm, TargetKeyFingerprint: strings.Repeat("b", 64),
		LifecycleState: store.VaultKeyRotationOpen, ItemCount: 1, StagedCount: 1,
		RecoveryDispositionMode: store.VaultKeyRotationRecoveryArtifact,
		RecoveryArtifactSHA256:  strings.Repeat("c", 64),
		RowVersion:              2, StagedPlanHash: "plan", CreatedAt: now, UpdatedAt: now,
	})
	if rotation.ID != "vkr_abcdefghijklmnop" || rotation.SourceKeyVersion != 1 ||
		rotation.SourceKeyAlgorithm != store.SecretAEADAlgorithm ||
		rotation.SourceKeyFingerprint != strings.Repeat("a", 64) ||
		rotation.TargetKeyVersion != 2 || rotation.TargetKeyAlgorithm != store.SecretAEADAlgorithm ||
		rotation.TargetKeyFingerprint != strings.Repeat("b", 64) || rotation.StagedPlanHash != "plan" {
		t.Fatalf("rotation projection = %#v", rotation)
	}
	if rotation.RecoveryDispositionMode != server.VaultKeyRotationRecoveryArtifact ||
		rotation.RecoveryArtifactSHA256 != strings.Repeat("c", 64) {
		t.Fatalf("rotation recovery projection = %#v", rotation)
	}
	wrapper := bytes.Repeat([]byte{7}, 60)
	item := toServerVaultKeyRotationItem(store.VaultKeyRotationItem{
		RotationID: rotation.ID, SecretID: "sec_abcdefghijklmnop",
		FieldID: "fld_abcdefghijklmnop", FieldKind: store.SecretFieldPassword,
		DEKID: "dek_abcdefghijklmnop", DEKGeneration: 1,
		SourceDEKRowVersion: 3, SourceWrapRevision: 4, SourceWrappedDEK: wrapper,
		SourceWrapAlgorithm: store.SecretAEADAlgorithm, SourceAADVersion: store.SecretAADVersion,
		SourceWrappingKeyID: rotation.SourceKeyID, SourceWrappingKeyVersion: 1,
		TargetWrappingKeyID: rotation.TargetKeyID, TargetWrappingKeyVersion: 2,
		TargetWrappedDEK: wrapper, TargetWrapRevision: 5, TargetWrapperSHA256: "wrapper-hash",
	})
	if item.DEKID != "dek_abcdefghijklmnop" || item.SourceDEKRowVersion != 3 ||
		item.TargetWrapRevision != 5 || !bytes.Equal(item.SourceWrappedDEK, wrapper) ||
		!bytes.Equal(item.TargetWrappedDEK, wrapper) {
		t.Fatalf("rotation item projection = %#v", item)
	}
}
