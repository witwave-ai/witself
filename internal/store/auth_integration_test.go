package store

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCuratorTokenPostgresLifecycle(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := st.ProvisionAccount(ctx, "curator-token-test@witwave.ai", "curator token test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "curator")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("ordinary credentials authenticate as full", func(t *testing.T) {
		opRaw, opTokenID, _, err := st.CreateOperatorToken(
			ctx, provisioned.AccountID, provisioned.OperatorID, "operator test", nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		opPrincipal, ok, err := st.AuthenticatePrincipal(ctx, opRaw)
		if err != nil || !ok {
			t.Fatalf("authenticate operator = %#v / %t / %v", opPrincipal, ok, err)
		}
		if opPrincipal.Kind != PrincipalOperator || opPrincipal.TokenID != opTokenID ||
			opPrincipal.AccessProfile != AccessProfileFull || opPrincipal.TokenExpiresAt != nil {
			t.Fatalf("operator principal = %#v", opPrincipal)
		}

		agentRaw, agentTokenID, _, err := st.CreateAgentToken(
			ctx, provisioned.AccountID, provisioned.OperatorID, agent.ID,
		)
		if err != nil {
			t.Fatal(err)
		}
		agentPrincipal, ok, err := st.AuthenticatePrincipal(ctx, agentRaw)
		if err != nil || !ok {
			t.Fatalf("authenticate agent = %#v / %t / %v", agentPrincipal, ok, err)
		}
		if agentPrincipal.Kind != PrincipalAgent || agentPrincipal.TokenID != agentTokenID ||
			agentPrincipal.AccessProfile != AccessProfileFull || agentPrincipal.TokenExpiresAt != nil {
			t.Fatalf("agent principal = %#v", agentPrincipal)
		}
	})

	t.Run("preview profile carries expiry and expires", func(t *testing.T) {
		const displayName = "nightly preview curator"
		raw, tokenID, agentName, expiresAt, err := st.CreateCuratorToken(
			ctx, provisioned.AccountID, provisioned.OperatorID, agent.ID,
			AccessProfileCuratorPreview, "  "+displayName+"  ", time.Hour,
		)
		if err != nil {
			t.Fatal(err)
		}
		if raw == "" || tokenID == "" || agentName != agent.Name || expiresAt.IsZero() {
			t.Fatalf("mint result = raw:%t id:%q agent:%q expires:%v", raw != "", tokenID, agentName, expiresAt)
		}

		principal, ok, err := st.AuthenticatePrincipal(ctx, raw)
		if err != nil || !ok {
			t.Fatalf("authenticate preview = %#v / %t / %v", principal, ok, err)
		}
		if principal.TokenID != tokenID || principal.AccessProfile != AccessProfileCuratorPreview ||
			principal.TokenExpiresAt == nil || !principal.TokenExpiresAt.Equal(expiresAt) {
			t.Fatalf("preview principal = %#v, expires want %v", principal, expiresAt)
		}

		var storedProfile, storedDisplayName, storedHash string
		var storedExpiresAt time.Time
		if err := st.pool.QueryRow(ctx, `
			SELECT access_profile, display_name, token_hash, expires_at
			FROM tokens WHERE id=$1`, tokenID).
			Scan(&storedProfile, &storedDisplayName, &storedHash, &storedExpiresAt); err != nil {
			t.Fatal(err)
		}
		if storedProfile != AccessProfileCuratorPreview || storedDisplayName != displayName ||
			storedHash != hashToken(raw) || !storedExpiresAt.Equal(expiresAt) {
			t.Fatalf("stored token = profile:%q name:%q hash-match:%t expires:%v",
				storedProfile, storedDisplayName, storedHash == hashToken(raw), storedExpiresAt)
		}

		var metadataJSON []byte
		if err := st.pool.QueryRow(ctx, `
			SELECT metadata FROM account_events
			WHERE verb=$1 AND metadata->>'token_id'=$2`, VerbAgentTokenMinted, tokenID).
			Scan(&metadataJSON); err != nil {
			t.Fatal(err)
		}
		var metadata map[string]any
		if err := json.Unmarshal(metadataJSON, &metadata); err != nil {
			t.Fatal(err)
		}
		if metadata["access_profile"] != AccessProfileCuratorPreview ||
			metadata["display_name"] != displayName || metadata["expires_at"] == "" {
			t.Fatalf("audit metadata = %#v", metadata)
		}
		serialized := string(metadataJSON)
		if strings.Contains(serialized, raw) || strings.Contains(serialized, hashToken(raw)) ||
			strings.Contains(serialized, "token_hash") {
			t.Fatalf("audit metadata contains credential material: %s", serialized)
		}

		if _, err := st.pool.Exec(ctx,
			`UPDATE tokens SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, tokenID); err != nil {
			t.Fatal(err)
		}
		if expired, ok, err := st.AuthenticatePrincipal(ctx, raw); err != nil || ok {
			t.Fatalf("authenticate expired = %#v / %t / %v", expired, ok, err)
		}
	})

	t.Run("apply profile uses normal revocation", func(t *testing.T) {
		raw, tokenID, _, expiresAt, err := st.CreateCuratorToken(
			ctx, provisioned.AccountID, provisioned.OperatorID, agent.ID,
			AccessProfileCuratorApply, "bounded apply curator", 30*time.Minute,
		)
		if err != nil {
			t.Fatal(err)
		}
		principal, ok, err := st.AuthenticatePrincipal(ctx, raw)
		if err != nil || !ok || principal.AccessProfile != AccessProfileCuratorApply ||
			principal.TokenExpiresAt == nil || !principal.TokenExpiresAt.Equal(expiresAt) {
			t.Fatalf("authenticate apply = %#v / %t / %v", principal, ok, err)
		}
		if err := st.RevokeToken(ctx, provisioned.AccountID, provisioned.OperatorID, tokenID); err != nil {
			t.Fatal(err)
		}
		if revoked, ok, err := st.AuthenticatePrincipal(ctx, raw); err != nil || ok {
			t.Fatalf("authenticate revoked = %#v / %t / %v", revoked, ok, err)
		}
	})
}
