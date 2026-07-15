package store

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"
)

func TestAgentActivityPeerIsolationIdempotencyAndArchiveRoundTripPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := st.ProvisionAccount(ctx, "agent-activity@witwave.ai", "agent activity", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	otherRealm, err := st.CreateRealm(ctx, provisioned.AccountID, "other")
	if err != nil {
		t.Fatal(err)
	}
	createAgent := func(realmID, name string) Agent {
		t.Helper()
		agent, err := st.CreateAgent(ctx, provisioned.AccountID, realmID, name)
		if err != nil {
			t.Fatal(err)
		}
		return agent
	}
	principal := func(agent Agent, realmID string) Principal {
		return Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
			RealmID: realmID, AgentName: agent.Name, AccountStatus: "active"}
	}
	scott := createAgent(realm.ID, "scott")
	bob := createAgent(realm.ID, "bob")
	never := createAgent(realm.ID, "never-active")
	deleted := createAgent(realm.ID, "deleted")
	_ = createAgent(otherRealm.ID, "other-realm")
	if err := st.DeleteAgent(ctx, provisioned.AccountID, realm.ID, deleted.ID); err != nil {
		t.Fatal(err)
	}
	scottPrincipal := principal(scott, realm.ID)
	bobPrincipal := principal(bob, realm.ID)

	base := time.Now().UTC().Add(-10 * time.Minute)
	first, err := st.TouchAgentActivity(ctx, bobPrincipal, AgentActivityInput{
		Runtime: "codex", LocationID: "home-install", Location: "home",
		Event: "SessionStart", EventID: "evt_first", EventOccurredAt: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	retry, err := st.TouchAgentActivity(ctx, bobPrincipal, AgentActivityInput{
		Runtime: "codex", LocationID: "home-install", Location: "changed-on-retry",
		Event: "Stop", EventID: "evt_first", EventOccurredAt: base.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if retry.LastActivityAt != first.LastActivityAt || retry.EventID != "evt_first" ||
		retry.Event != "SessionStart" || retry.Location != "home" {
		t.Fatalf("duplicate touch moved projection: first=%#v retry=%#v", first, retry)
	}

	newer, err := st.TouchAgentActivity(ctx, bobPrincipal, AgentActivityInput{
		Runtime: "codex", LocationID: "home-install", Location: "home",
		Event: "AgentResponse", EventID: "evt_newer", EventOccurredAt: base.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := st.TouchAgentActivity(ctx, bobPrincipal, AgentActivityInput{
		Runtime: "codex", LocationID: "home-install", Location: "stale-location",
		Event: "UserPromptSubmit", EventID: "evt_stale", EventOccurredAt: base.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if stale.LastActivityAt != newer.LastActivityAt || stale.EventID != "evt_newer" ||
		stale.Event != "AgentResponse" || stale.Location != "home" {
		t.Fatalf("stale touch moved projection: newer=%#v stale=%#v", newer, stale)
	}
	var pinnedObservation time.Time
	if err := st.pool.QueryRow(ctx, `
		UPDATE agent_activity
		   SET last_activity_at = clock_timestamp() + interval '1 minute'
		 WHERE agent_id=$1 AND runtime='codex' AND location_id='home-install'
		 RETURNING last_activity_at`, bob.ID).Scan(&pinnedObservation); err != nil {
		t.Fatal(err)
	}
	monotonic, err := st.TouchAgentActivity(ctx, bobPrincipal, AgentActivityInput{
		Runtime: "codex", LocationID: "home-install", Location: "home",
		Event: "Stop", EventID: "evt_monotonic", EventOccurredAt: base.Add(4 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if monotonic.LastActivityAt.Before(pinnedObservation) {
		t.Fatalf("accepted touch moved server-observed time backward: pinned=%s got=%s",
			pinnedObservation, monotonic.LastActivityAt)
	}
	if _, err := st.TouchAgentActivity(ctx, bobPrincipal, AgentActivityInput{
		Runtime: "claude-code", LocationID: "laptop-install", Location: "laptop",
		Event: "Stop", EventID: "evt_claude", EventOccurredAt: base.Add(3 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	// Pin projection observation times so newest-runtime aggregation is fully
	// deterministic even on databases whose clock precision is coarse.
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_activity
		   SET last_activity_at = CASE runtime
	       WHEN 'codex' THEN now() - interval '2 minutes'
	       ELSE now() - interval '1 minute'
	   END
		 WHERE agent_id=$1`, bob.ID); err != nil {
		t.Fatal(err)
	}

	peers, err := st.ListAgentPeers(ctx, scottPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 2 {
		t.Fatalf("peers = %#v, want bob and never-active only", peers)
	}
	var bobPeer, neverPeer AgentPeer
	for _, peer := range peers {
		switch peer.ID {
		case bob.ID:
			bobPeer = peer
		case never.ID:
			neverPeer = peer
		default:
			t.Fatalf("cross-realm, deleted, or self agent leaked: %#v", peer)
		}
	}
	if bobPeer.LastActivityAt == nil || bobPeer.LastRuntime != "claude-code" ||
		bobPeer.LastLocation != "laptop" || bobPeer.LastEvent != "Stop" {
		t.Fatalf("bob newest activity = %#v", bobPeer)
	}
	if neverPeer.ID == "" || neverPeer.LastActivityAt != nil || neverPeer.LastRuntime != "" ||
		neverPeer.LastLocation != "" || neverPeer.LastEvent != "" {
		t.Fatalf("never-active peer = %#v", neverPeer)
	}
	forged, err := st.ListAgentPeers(ctx, Principal{
		Kind: PrincipalAgent, ID: scott.ID, AccountID: "acc_other", RealmID: realm.ID,
	})
	if err != nil || len(forged) != 0 {
		t.Fatalf("cross-account forged scope = %#v / %v", forged, err)
	}

	if err := st.SuspendAccountSystem(ctx, provisioned.AccountID, "evacuation", "activity archive round trip"); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := st.ExportAccount(ctx, provisioned.AccountID, "source-cell", "test", &archive); err != nil {
		t.Fatal(err)
	}
	if err := deleteAccountForIntegrationTest(ctx, st, provisioned.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ImportAccount(ctx, provisioned.AccountID, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}
	restored, err := st.ListAgentPeers(ctx, scottPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 2 {
		t.Fatalf("restored peers = %#v", restored)
	}
	restoredBob := false
	for _, peer := range restored {
		if peer.ID == bob.ID {
			restoredBob = true
			if peer.LastActivityAt == nil || peer.LastRuntime != "claude-code" ||
				peer.LastLocation != "laptop" || peer.LastEvent != "Stop" {
				t.Fatalf("restored bob activity = %#v", peer)
			}
		}
	}
	if !restoredBob {
		t.Fatalf("restored peer list omitted bob: %#v", restored)
	}
}
