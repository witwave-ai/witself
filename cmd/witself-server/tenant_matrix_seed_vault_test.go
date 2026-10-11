package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/sealed"
	"github.com/witwave-ai/witself/internal/store"
)

// tenantSeedVault follows TestAgentOwnedSecretPostgresAndArchiveRoundTrip and
// TestVaultKeyRotationStagesAndFlipsAtomically. Store methods seed opaque client
// ciphertext without reading or writing any local vault. Each state-changing
// probe gets its own principal, so its control cannot invalidate a later row.
func tenantSeedVault(t *testing.T, rt *tenantRuntime, side tenantSide) {
	t.Helper()
	v := rt.f.bySide[side]
	p := tenantSeedPrincipal(rt, side, credAgent, "")
	tenantSeedVaultKey(t, rt, p, v)
	tenantSeedVaultSecret(t, rt, p, v, "secret")
	tenantSeedVaultEnrollment(t, rt, p, v, "enrollment", false)
	_, err := rt.st.CancelVaultKeyEnrollment(context.Background(), p, v["enrollment"], store.CancelVaultKeyEnrollmentInput{ExpectedRowVersion: 1, IdempotencyKey: "matrix-seed-cancel"})
	tenantSeedObjectError(t, "cancel inventory enrollment", err)
	tenantSeedVaultRotation(t, rt, p, v, "rotation", false)

	for _, row := range tenantRowsF2() {
		if !strings.Contains(row.Pattern, "/v1/vault/") && !strings.Contains(row.Pattern, "/v1/secrets") {
			continue
		}
		for _, probe := range row.Probes {
			if probe.Fresh == "" {
				continue
			}
			scope := tenantProbeID(row, probe)
			values := tenantSyntheticValues(side, "vault:"+scope)
			principal := tenantSeedVaultPrincipal(t, rt, side, probe.Fresh, values)
			rt.probeValues[side][scope] = values
			if probe.Fresh != "vault_register" {
				tenantSeedVaultKey(t, rt, principal, values)
			}
			switch {
			case probe.Fresh == "vault_register":
				// Registration itself is exercised by the own probe.
			case probe.Fresh == "enrollment_create":
				// Its own template keeps dedicated never-issued values, with a valid TTL.
				if side == sideA {
					rt.f.ownProbeValues[scope]["enrollment_expiry"] = time.Now().UTC().Add(45 * time.Minute).Truncate(time.Second).Format(time.RFC3339)
				}
			case strings.HasPrefix(probe.Fresh, "enrollment_"):
				key := "enrollment_" + strings.Split(probe.Fresh[len("enrollment_"):], "_")[0]
				approved := strings.Contains(probe.Fresh, "receive") || strings.Contains(probe.Fresh, "consume")
				tenantSeedVaultEnrollment(t, rt, principal, values, key, approved)
			case probe.Fresh == "rotation_start":
				// No rotation is open yet; B's source-key body control may start one.
			case strings.HasPrefix(probe.Fresh, "rotation_"):
				key := "rotation_" + strings.Split(probe.Fresh[len("rotation_"):], "_")[0]
				tenantSeedVaultSecret(t, rt, principal, values, "secret")
				tenantSeedVaultRotation(t, rt, principal, values, key, probe.Fresh == "rotation_commit")
			case probe.Fresh == "secret_create":
				// The wrapping-key body control creates the first secret under this key.
			case strings.HasPrefix(probe.Fresh, "secret_"):
				tenantSeedVaultSecret(t, rt, principal, values, probe.Fresh)
				if probe.Fresh == "secret_restore" {
					_, err := rt.st.ArchiveSecret(context.Background(), principal, values[probe.Fresh], store.SecretLifecycleInput{ExpectedRowVersion: 1, IdempotencyKey: "matrix-seed-archive"})
					tenantSeedObjectError(t, "archive restore secret", err)
				}
			default:
				t.Fatalf("vault seed has no lifecycle for %s", probe.Fresh)
			}
			// Keep every actual probe object and canary in the owner's leak inventory.
			// Sequence numbers and expiration timestamps are not owner values.
			for key, value := range values {
				if shape := tenantSyntheticShapes[key]; shape == "number" || shape == "version" || shape == "time" {
					continue
				}
				rt.f.bySide[side]["seed.vault."+scope+"."+key] = value
			}
		}
	}
	if side == sideA {
		for key, value := range v {
			if strings.HasPrefix(key, "vault_") || strings.HasPrefix(key, "secret") || strings.HasPrefix(key, "enrollment") || strings.HasPrefix(key, "rotation") || key == "dek" || key == "field" {
				rt.f.ownValues[key] = value
			}
		}
	}
}

// tenantSeedVaultPrincipal uses CreateAgent/CreateAgentToken just as the base
// identity seeder does. A Fresh token never shares mutable vault state with main.
func tenantSeedVaultPrincipal(t *testing.T, rt *tenantRuntime, side tenantSide, fresh string, values map[string]string) store.Principal {
	t.Helper()
	ctx := context.Background()
	base := rt.f.bySide[side]
	name := string(side) + "-vault-" + strings.ReplaceAll(fresh, "_", "-") + "-" + tenantSeedCanary(side, fresh)
	agent, err := rt.st.CreateAgent(ctx, base["account"], base["realm"], name)
	tenantSeedObjectError(t, "create vault probe agent", err)
	token, tokenID, _, err := rt.st.CreateAgentToken(ctx, base["account"], base["operator"], agent.ID)
	tenantSeedObjectError(t, "create vault probe token", err)
	rt.freshCreds[side][fresh] = token
	rt.secrets = append(rt.secrets, token)
	values["account"], values["realm"], values["agent"], values["agent-name"], values["realm-name"] = base["account"], base["realm"], agent.ID, name, base["realm-name"]
	values["seed.vault.token"], values["seed.vault.token-id"] = token, tokenID
	return store.Principal{Kind: store.PrincipalAgent, AccountID: base["account"], AccountStatus: "active", ID: agent.ID, RealmID: base["realm"], AgentName: name, RealmName: base["realm-name"], AccessProfile: store.AccessProfileFull}
}

// RegisterVaultKey validates public metadata only (secret_mutation.go:22).
func tenantSeedVaultKey(t *testing.T, rt *tenantRuntime, p store.Principal, v map[string]string) {
	t.Helper()
	// Fingerprints are account-unique (0055_add_agent_secrets.sql:27).
	// Keep the current fingerprint distinct from the target metadata carried by
	// the rotation-start probe, including its positive owner control.
	digest := sha256.Sum256([]byte("matrix-current-key:" + v["vault_key"]))
	v["seed.vault.key.fingerprint"] = hex.EncodeToString(digest[:])
	_, _, err := rt.st.RegisterVaultKey(context.Background(), p, store.RegisterVaultKeyInput{ID: v["vault_key"], KeyVersion: 1, Algorithm: store.SecretAEADAlgorithm, Fingerprint: v["seed.vault.key.fingerprint"], IdempotencyKey: "matrix-seed-key"})
	tenantSeedObjectError(t, "register vault key", err)
}

// CreateSecret accepts already-encrypted material; these test bytes have valid
// envelope lengths and stay opaque to the backend (secret.go:195-240).
func tenantSeedVaultSecret(t *testing.T, rt *tenantRuntime, p store.Principal, v map[string]string, key string) {
	t.Helper()
	_, err := rt.st.CreateSecret(context.Background(), p, store.CreateSecretInput{
		ID: v[key], Name: v["secret_name"], Description: v["secret_description"], Template: "custom", IdempotencyKey: "matrix-seed-secret",
		Fields: []store.CreateSecretFieldInput{{ID: v["field"], Name: "password", Kind: store.SecretFieldPassword, Sensitive: true, Encoding: store.SecretEncodingUTF8, ValueVersion: 1,
			Sealed: &store.SealedFieldInput{EnvelopeVersion: 1, Ciphertext: tenantSeedVaultBytes(t, v["vault_ciphertext"]), Algorithm: store.SecretAEADAlgorithm, AADVersion: 1,
				DEK: store.SealedDEKInput{ID: v["dek"], Generation: 1, WrappedDEK: tenantSeedVaultBytes(t, v["vault_wrapped_dek"]), WrapAlgorithm: store.SecretAEADAlgorithm, AADVersion: 1, WrapRevision: 1, WrappingKeyID: v["vault_key"], WrappingKeyVersion: 1}}}},
	})
	tenantSeedObjectError(t, "create sealed secret", err)
}

// Create/ApproveVaultKeyEnrollment use the store lifecycle from
// vault_key_rotation_integration_test.go:assertSuspendedVaultEnrollmentCancellation.
// Consume commitments must bind the exact request ID, not merely be a hash shape.
func tenantSeedVaultEnrollment(t *testing.T, rt *tenantRuntime, p store.Principal, v map[string]string, key string, approved bool) {
	t.Helper()
	ctx := context.Background()
	expiry := time.Now().UTC().Add(45 * time.Minute).Truncate(time.Second)
	v["enrollment_expiry"] = expiry.Format(time.RFC3339)
	_, err := rt.st.CreateVaultKeyEnrollment(ctx, p, store.CreateVaultKeyEnrollmentInput{ID: v[key], TargetLocationID: v["location_id"], TargetLocationName: v["location_name"], TargetPublicKey: v["vault_public_key"], TargetKeyAlgorithm: store.VaultEnrollmentTargetKeyAlgorithm, PairingCommitment: v["pairing_commitment"], ExpiresAt: expiry, IdempotencyKey: "matrix-seed-enrollment"})
	tenantSeedObjectError(t, "create enrollment", err)
	if !approved {
		return
	}
	commitment, err := sealed.AVKEnrollmentConsumeCommitment(v[key], tenantSeedVaultBytes(t, v["consume_proof"]))
	tenantSeedObjectError(t, "create enrollment commitment", err)
	v["consume_commitment"] = commitment
	_, err = rt.st.ApproveVaultKeyEnrollment(ctx, p, v[key], store.ApproveVaultKeyEnrollmentInput{ExpectedRowVersion: 1, SourceLocationID: v["source_location_id"], SourceEphemeralPublicKey: v["vault_public_key"], TransferCiphertext: tenantSeedVaultBytes(t, v["vault_transfer"]), TransferAlgorithm: store.VaultEnrollmentTransferAlgorithm, ConsumeCommitment: commitment, IdempotencyKey: "matrix-seed-approve"})
	tenantSeedObjectError(t, "approve enrollment", err)
}

// Start/StageVaultKeyRotation reproduce the one-DEK lifecycle in
// TestVaultKeyRotationStagesAndFlipsAtomically. Commit uses the returned plan
// hash and revision instead of a fabricated concurrency fence.
func tenantSeedVaultRotation(t *testing.T, rt *tenantRuntime, p store.Principal, v map[string]string, key string, staged bool) {
	t.Helper()
	ctx := context.Background()
	rotation, _, err := rt.st.StartVaultKeyRotation(ctx, p, store.StartVaultKeyRotationInput{ID: v[key], ExpectedSourceKeyID: v["vault_key"], ExpectedSourceKeyVersion: 1, ExpectedSourceKeyRowVersion: 1, TargetKeyID: v["vault_key_target"], TargetKeyVersion: 2, TargetAlgorithm: store.SecretAEADAlgorithm, TargetFingerprint: v["vault_fingerprint"], IdempotencyKey: "matrix-seed-rotation"})
	tenantSeedObjectError(t, "start rotation", err)
	if !staged {
		return
	}
	rotation, _, err = rt.st.StageVaultKeyRotation(ctx, p, rotation.ID, store.StageVaultKeyRotationInput{ExpectedRotationRowVersion: rotation.RowVersion, Items: []store.StageVaultKeyRotationItemInput{{DEKID: v["dek"], ExpectedSourceDEKRowVersion: 1, ExpectedSourceWrapRevision: 1, TargetWrappedDEK: tenantSeedVaultBytes(t, v["vault_wrapped_dek"]), TargetWrapRevision: 2}}, IdempotencyKey: "matrix-seed-stage"})
	tenantSeedObjectError(t, "stage rotation", err)
	v["plan_hash"] = rotation.StagedPlanHash
}

func tenantSeedVaultBytes(t *testing.T, encoded string) []byte {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	tenantSeedObjectError(t, "decode sealed bytes", err)
	return decoded
}
