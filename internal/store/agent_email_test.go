package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNormalizeAgentEmailProcessingInputs(t *testing.T) {
	if got, err := normalizeAgentEmailLease(0); err != nil || got != defaultAgentEmailLease {
		t.Fatalf("default lease = %v / %v", got, err)
	}
	for _, invalid := range []time.Duration{time.Second, 29 * time.Second, 16 * time.Minute} {
		if _, err := normalizeAgentEmailLease(invalid); !errors.Is(err, ErrAgentEmailInputInvalid) {
			t.Fatalf("lease %v error = %v", invalid, err)
		}
	}
	first, err := normalizeAgentEmailKey(" retry-key ", "claim")
	if err != nil || len(first) != 64 || strings.Contains(first, "retry-key") {
		t.Fatalf("processing key hash = %q / %v", first, err)
	}
	second, err := normalizeAgentEmailKey("retry-key", "completion")
	if err != nil || second != first {
		t.Fatalf("stable processing key hash = %q / %v, want %q", second, err, first)
	}
	if _, err := normalizeAgentEmailKey("", "claim"); !errors.Is(err, ErrAgentEmailInputInvalid) {
		t.Fatalf("empty key error = %v", err)
	}
	claimID, generation, err := normalizeAgentEmailFence(" ecl_aaaaaaaaaaaaaaaa ", 4)
	if err != nil || claimID != "ecl_aaaaaaaaaaaaaaaa" || generation != 4 {
		t.Fatalf("normalized fence = %q/%d / %v", claimID, generation, err)
	}
	for _, invalid := range []struct {
		claimID    string
		generation int64
	}{
		{"ecl_bad", 1}, {"mcl_aaaaaaaaaaaaaaaa", 1},
		{"ecl_aaaaaaaaaaaaaaaa", 0}, {"ecl_aaaaaaaaaaaaaaaa", maximumAgentEmailGeneration + 1},
	} {
		if _, _, err := normalizeAgentEmailFence(invalid.claimID, invalid.generation); !errors.Is(err, ErrAgentEmailInputInvalid) {
			t.Fatalf("fence %#v error = %v", invalid, err)
		}
	}
}

func TestNormalizeAgentEmailPilotScope(t *testing.T) {
	realmID := "realm_aaaaaaaaaaaaaaaa"
	agents := map[string]bool{
		"agent_aaaaaaaaaaaaaaaa": true,
		"agent_bbbbbbbbbbbbbbbb": true,
		"agent_cccccccccccccccc": true,
		"agent_dddddddddddddddd": true,
		"agent_eeeeeeeeeeeeeeee": true,
	}
	scope := AgentEmailPilotScope{
		Enabled: true, Domain: "Agent-Mail.Witwave.AI.", Audience: "cell-pilot-1",
		RealmIDs: map[string]bool{realmID: true}, AgentIDs: agents,
	}
	if got, err := normalizeAgentEmailPilotScope(scope); err != nil || got != "agent-mail.witwave.ai" {
		t.Fatalf("normalized scope = %q / %v", got, err)
	}
	tooFew := scope
	tooFew.AgentIDs = map[string]bool{"agent_aaaaaaaaaaaaaaaa": true}
	if _, err := normalizeAgentEmailPilotScope(tooFew); !errors.Is(err, ErrAgentEmailInputInvalid) {
		t.Fatalf("too-few scope error = %v", err)
	}
	twoRealms := scope
	twoRealms.RealmIDs = map[string]bool{
		realmID: true, "realm_bbbbbbbbbbbbbbbb": true,
	}
	if _, err := normalizeAgentEmailPilotScope(twoRealms); !errors.Is(err, ErrAgentEmailInputInvalid) {
		t.Fatalf("two-realm scope error = %v", err)
	}
	if _, err := requireAgentEmailPilotEnrollment(AgentEmailPilotScope{}, realmID, "agent_aaaaaaaaaaaaaaaa"); !errors.Is(err, ErrAgentEmailPilotDisabled) {
		t.Fatalf("disabled enrollment error = %v", err)
	}
	if _, err := requireAgentEmailPilotEnrollment(scope, realmID, "agent_zzzzzzzzzzzzzzzz"); !errors.Is(err, ErrAgentEmailPilotNotEnrolled) {
		t.Fatalf("unenrolled agent error = %v", err)
	}
	restricted := Principal{
		Kind: PrincipalAgent, ID: "agent_aaaaaaaaaaaaaaaa", RealmID: realmID,
		AccessProfile: AccessProfileCuratorPreview,
	}
	if err := requireAgentEmailPilotPrincipal(scope, restricted); !errors.Is(err, ErrAgentEmailForbidden) {
		t.Fatalf("restricted principal error = %v", err)
	}
}

func TestAgentEmailCursorRoundTrip(t *testing.T) {
	wantTime := time.Unix(0, 1721570400123456789).UTC()
	wantID := "emsg_aaaaaaaaaaaaaaaa"
	cursor := encodeAgentEmailCursor(wantTime, wantID)
	gotTime, gotID, err := decodeAgentEmailCursor(cursor)
	if err != nil || !gotTime.Equal(wantTime) || gotID != wantID {
		t.Fatalf("cursor round trip = %v/%q / %v", gotTime, gotID, err)
	}
	for _, invalid := range []string{"", "no-colon", "0:" + wantID, "1:emsg_bad"} {
		if _, _, err := decodeAgentEmailCursor(invalid); !errors.Is(err, ErrAgentEmailCursorInvalid) {
			t.Fatalf("cursor %q error = %v", invalid, err)
		}
	}
}
