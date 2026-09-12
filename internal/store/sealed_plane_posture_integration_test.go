package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/sealed"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestReadSealedPlanePostureMetricsPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	st, p, current := newVaultLifecycleTestAgent(ctx, t, dsn, "sealed-posture@witwave.ai")

	read := func() SealedPlanePostureMetrics {
		t.Helper()
		m, err := st.ReadSealedPlanePostureMetrics(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if got := read(); got != (SealedPlanePostureMetrics{}) {
		t.Fatalf("empty posture = %+v, want all zeros", got)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := st.pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	startRotation := func(keyVersion uint64, idempotencyKey string) VaultKeyRotation {
		t.Helper()
		key, err := sealed.GenerateAgentVaultKey(keyVersion)
		if err != nil {
			t.Fatal(err)
		}
		defer key.Clear()
		rotation, _, err := st.StartVaultKeyRotation(ctx, p,
			vaultLifecycleRotationInput(t, current, key, idempotencyKey))
		if err != nil {
			t.Fatal(err)
		}
		return rotation
	}
	rotation := startRotation(2, "posture-cancelled")
	exec(`UPDATE agent_vault_key_rotations
		SET created_at = statement_timestamp() - interval '2 hours' WHERE id = $1`, rotation.ID)
	if got := read(); got.OpenRotations != 1 || got.OldestOpenRotationSeconds < 7200 || got.OldestOpenRotationSeconds > 7210 {
		t.Fatalf("open rotation posture = %+v; want one open, age 7200..7210 seconds", got)
	}
	if _, _, err := st.CancelVaultKeyRotation(ctx, p, rotation.ID, CancelVaultKeyRotationInput{
		ExpectedRotationRowVersion: rotation.RowVersion, IdempotencyKey: "posture-cancel",
	}); err != nil {
		t.Fatal(err)
	}
	rotation = startRotation(2, "posture-committed")
	if _, _, err := st.CommitVaultKeyRotation(ctx, p, rotation.ID, CommitVaultKeyRotationInput{
		ExpectedRotationRowVersion: rotation.RowVersion, ExpectedItemCount: 0,
		ExpectedPlanHash:    rotation.StagedPlanHash,
		RecoveryDisposition: VaultKeyRotationRecoveryDisposition{Mode: VaultKeyRotationRiskAccepted},
		IdempotencyKey:      "posture-commit",
	}); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.OpenRotations != 0 || got.OldestOpenRotationSeconds != 0 {
		t.Fatalf("completed rotation posture = %+v; committed and cancelled must be excluded", got)
	}
	currentKey, err := st.GetCurrentVaultKey(ctx, p)
	if err != nil || currentKey == nil {
		t.Fatalf("current key = %v / %v", currentKey, err)
	}
	current = *currentKey
	rotation = startRotation(3, "posture-open")
	exec(`UPDATE agent_vault_key_rotations
		SET created_at = statement_timestamp() - interval '2 hours' WHERE id = $1`, rotation.ID)

	// A second agent has pending and approved enrollments while the first
	// rotates. The cell-wide query must count both states, ignoring lazy expiry.
	agent, err := st.CreateAgent(ctx, p.AccountID, p.RealmID, "enrolling")
	if err != nil {
		t.Fatal(err)
	}
	enrollmentKey, err := sealed.GenerateAgentVaultKey(1)
	if err != nil {
		t.Fatal(err)
	}
	defer enrollmentKey.Clear()
	keyMeta := enrollmentKey.Metadata()
	p2 := p
	p2.ID, p2.AgentName = agent.ID, agent.Name
	if _, _, err := st.RegisterVaultKey(ctx, p2, RegisterVaultKeyInput{
		ID: keyMeta.ID, KeyVersion: 1, Algorithm: keyMeta.Algorithm,
		Fingerprint: keyMeta.Fingerprint, IdempotencyKey: "posture-enrollment-key",
	}); err != nil {
		t.Fatal(err)
	}
	for i, state := range []string{"pending", "approved", "pending", "approved", "cancelled", "consumed", "expired"} {
		// Ages are asserted against the server's statement_timestamp(), so the
		// fixture must be anchored to the server clock too: a client-side
		// time.Now() lets host/container clock skew floor a 600-second wait to
		// 599 and flake the assertion below.
		createdOffset, expiresOffset := "-10 minutes", "20 minutes"
		if i == 2 || i == 3 {
			createdOffset, expiresOffset = "-1 hour", "-1 minute"
		}
		// Fixture rows satisfy lifecycle CHECKs without needing client transfer
		// crypto; posture reads must never inspect that opaque transfer material.
		exec(`INSERT INTO agent_vault_key_enrollments
			(id, account_id, realm_id, owner_agent_id, vault_key_id, vault_key_version,
			 target_location_id, target_public_key, target_key_algorithm, pairing_commitment,
			 lifecycle_state, created_at, expires_at, source_location_id,
			 source_ephemeral_public_key, transfer_ciphertext, transfer_algorithm,
			 consume_commitment, approved_at, consumed_at, cancelled_at, expired_at)
		VALUES ($1,$2,$3,$4,$5,1,$6,$7,$8,$9,$10,
			statement_timestamp() + $11::interval, statement_timestamp() + $12::interval,
			CASE WHEN $10 IN ('approved','consumed') THEN $6 END,
			CASE WHEN $10 = 'approved' THEN $7 END,
			CASE WHEN $10 = 'approved' THEN decode(repeat('ab',64),'hex') END,
			CASE WHEN $10 = 'approved' THEN $13 END,
			CASE WHEN $10 = 'approved' THEN $9 END,
			CASE WHEN $10 IN ('approved','consumed') THEN statement_timestamp() + $11::interval END,
			CASE WHEN $10 = 'consumed' THEN statement_timestamp() + $11::interval END,
			CASE WHEN $10 = 'cancelled' THEN statement_timestamp() + $11::interval END,
			CASE WHEN $10 = 'expired' THEN statement_timestamp() + $11::interval END)`,
			mustSecretTestID(t, "enr"), p.AccountID, p.RealmID, agent.ID, keyMeta.ID,
			mustSecretTestID(t, "loc"), strings.Repeat("A", 43), VaultEnrollmentTargetKeyAlgorithm,
			strings.Repeat("a", 64), state, createdOffset, expiresOffset, VaultEnrollmentTransferAlgorithm)
	}
	if got := read(); got.PendingEnrollments != 2 || got.OldestPendingEnrollmentSeconds < 600 || got.OldestPendingEnrollmentSeconds > 610 {
		t.Fatalf("enrollment posture = %+v; want two unexpired waits, age 600..610 seconds", got)
	}

	// Count delivery events, rather than quantity or all dimensions, and keep
	// only the maximum agent count instead of returning any principal identity.
	for i, fixture := range []struct {
		agentID   string
		dimension string
		count     int
		age       time.Duration
	}{
		{p.ID, "secret_read", 3, time.Minute},
		{p2.ID, "secret_read", 7, time.Minute},
		{p.ID, "secret_read", 20, 16 * time.Minute},
		{p2.ID, "secret_write", 30, time.Minute},
	} {
		exec(`INSERT INTO usage_events
			(id, account_id, realm_id, agent_id, dimension, quantity, unit,
			 subject_type, subject_id, idempotency_key, occurred_at)
		SELECT 'usg_posture_' || $1 || '_' || n, $2, $3, $4, $5, 9, 'fields',
			'secret', 'fixture', 'posture-' || $1 || '-' || n, $6
		FROM generate_series(1, $7::integer) n`, fmt.Sprint(i), p.AccountID, p.RealmID,
			fixture.agentID, fixture.dimension, time.Now().Add(-fixture.age), fixture.count)
	}
	if got := read(); got.MaxAgentDeliveries15m != 7 || got.OpenRotations != 1 || got.PendingEnrollments != 2 {
		t.Fatalf("active posture = %+v; want 7 maximum deliveries, one rotation, two enrollments", got)
	}

	// Future creation times cannot produce negative gauges. Once the last
	// active records leave their windows, ages and counts reset to zero.
	exec(`UPDATE agent_vault_key_rotations SET
		created_at = statement_timestamp() + interval '1 minute',
		updated_at = statement_timestamp() + interval '1 minute' WHERE lifecycle_state = 'open'`)
	exec(`UPDATE agent_vault_key_enrollments SET created_at = statement_timestamp() + interval '1 minute'
		WHERE lifecycle_state IN ('pending','approved') AND expires_at > statement_timestamp()`)
	if got := read(); got.OldestOpenRotationSeconds != 0 || got.OldestPendingEnrollmentSeconds != 0 {
		t.Fatalf("future creation posture = %+v; want zero ages", got)
	}
	exec(`UPDATE agent_vault_key_rotations SET lifecycle_state = 'cancelled', cancelled_at = statement_timestamp()
		WHERE lifecycle_state = 'open'`)
	exec(`UPDATE agent_vault_key_enrollments SET created_at = statement_timestamp() - interval '2 hours',
		expires_at = statement_timestamp() - interval '1 minute'`)
	exec(`UPDATE usage_events SET occurred_at = statement_timestamp() - interval '16 minutes'`)
	if got := read(); got != (SealedPlanePostureMetrics{}) {
		t.Fatalf("settled posture = %+v, want all zeros", got)
	}
}
