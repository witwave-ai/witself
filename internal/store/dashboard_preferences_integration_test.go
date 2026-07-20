package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

func TestDashboardPreferencesRoundTripIsolationAndArchivePostgres(t *testing.T) {
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

	provisioned, err := st.ProvisionAccount(ctx, "dashboard-prefs@witwave.ai", "dashboard prefs", time.Hour)
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
	scott, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "scott")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "bob")
	if err != nil {
		t.Fatal(err)
	}
	principal := func(agent Agent) Principal {
		return Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
			RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active"}
	}
	scottPrincipal, bobPrincipal := principal(scott), principal(bob)

	// Absent row is the valid empty default, never an error.
	if prefs, err := st.GetDashboardPreferences(ctx, scottPrincipal); err != nil || prefs != nil {
		t.Fatalf("empty default = %#v / %v", prefs, err)
	}

	prefsDoc := func(theme string) json.RawMessage {
		return json.RawMessage(`{"schema":"witself.dashboard-prefs.v1","theme":"` + theme + `"}`)
	}
	// jsonb re-serializes documents (key order, spacing), so every prefs
	// assertion is semantic rather than byte-for-byte.
	assertTheme := func(prefs json.RawMessage, theme, context string) {
		t.Helper()
		var doc dashboardPreferencesDocument
		if err := json.Unmarshal(prefs, &doc); err != nil ||
			doc.Schema != DashboardPreferencesSchema || doc.Theme != theme {
			t.Fatalf("%s prefs = %s (%v), want theme %q", context, prefs, err, theme)
		}
	}
	first, err := st.PutDashboardPreferences(ctx, scottPrincipal, prefsDoc("amber"))
	if err != nil {
		t.Fatal(err)
	}
	if first.AgentID != scott.ID {
		t.Fatalf("first put = %#v", first)
	}
	assertTheme(first.Prefs, "amber", "first put")
	// Last-write-wins upsert: a second put simply replaces the document.
	second, err := st.PutDashboardPreferences(ctx, scottPrincipal, prefsDoc("high-contrast"))
	if err != nil {
		t.Fatal(err)
	}
	if second.AgentID != scott.ID {
		t.Fatalf("second put = %#v", second)
	}
	assertTheme(second.Prefs, "high-contrast", "second put")
	if second.UpdatedAt.Before(first.UpdatedAt) {
		t.Fatalf("updated_at moved backward: %s -> %s", first.UpdatedAt, second.UpdatedAt)
	}
	got, err := st.GetDashboardPreferences(ctx, scottPrincipal)
	if err != nil || got == nil {
		t.Fatalf("round trip get = %#v / %v", got, err)
	}
	assertTheme(got.Prefs, "high-contrast", "round trip")

	// The row is scoped to the exact bearer principal: another agent sees only
	// its own (absent) row, and a forged cross-account scope resolves nothing.
	if prefs, err := st.GetDashboardPreferences(ctx, bobPrincipal); err != nil || prefs != nil {
		t.Fatalf("foreign agent leak = %#v / %v", prefs, err)
	}
	forged := scottPrincipal
	forged.AccountID = "acc_other"
	if prefs, err := st.GetDashboardPreferences(ctx, forged); err != nil || prefs != nil {
		t.Fatalf("forged account scope = %#v / %v", prefs, err)
	}
	if _, err := st.PutDashboardPreferences(ctx, forged, prefsDoc("console")); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("forged put = %v, want ErrAgentNotFound", err)
	}
	if _, err := st.PutDashboardPreferences(ctx, scottPrincipal, json.RawMessage(`{"theme":"console"}`)); !errors.Is(err, ErrDashboardPreferencesInvalid) {
		t.Fatalf("invalid prefs = %v, want ErrDashboardPreferencesInvalid", err)
	}

	// A deleted agent has no preferences surface.
	if err := st.DeleteAgent(ctx, provisioned.AccountID, realm.ID, bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutDashboardPreferences(ctx, bobPrincipal, prefsDoc("console")); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("deleted agent put = %v, want ErrAgentNotFound", err)
	}

	// The preference row rides account export/import.
	if err := st.SuspendAccountSystem(ctx, provisioned.AccountID, "evacuation", "prefs archive round trip"); err != nil {
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
	restored, err := st.GetDashboardPreferences(ctx, scottPrincipal)
	if err != nil || restored == nil {
		t.Fatalf("restored get = %#v / %v", restored, err)
	}
	assertTheme(restored.Prefs, "high-contrast", "restored")
}
