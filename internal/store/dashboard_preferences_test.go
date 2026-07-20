package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestValidateDashboardPreferencesCanonicalizes(t *testing.T) {
	raw := json.RawMessage(`{
		"theme": "midnight",
		"schema": "witself.dashboard-prefs.v1"
	}`)
	canonical, err := validateDashboardPreferences(raw)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	want := `{"schema":"witself.dashboard-prefs.v1","theme":"midnight"}`
	if string(canonical) != want {
		t.Fatalf("canonical = %s, want %s", canonical, want)
	}
}

func TestValidateDashboardPreferencesRejections(t *testing.T) {
	cases := map[string]string{
		"empty":              ``,
		"not JSON":           `nonsense`,
		"array":              `["witself.dashboard-prefs.v1"]`,
		"string":             `"console"`,
		"null":               `null`,
		"missing schema":     `{"theme":"console"}`,
		"missing theme":      `{"schema":"witself.dashboard-prefs.v1"}`,
		"wrong schema":       `{"schema":"witself.dashboard-prefs.v2","theme":"console"}`,
		"non-string schema":  `{"schema":1,"theme":"console"}`,
		"non-string theme":   `{"schema":"witself.dashboard-prefs.v1","theme":7}`,
		"empty theme":        `{"schema":"witself.dashboard-prefs.v1","theme":""}`,
		"unknown key":        `{"schema":"witself.dashboard-prefs.v1","theme":"console","font":"mono"}`,
		"nested unknown":     `{"schema":"witself.dashboard-prefs.v1","theme":"console","extra":{"a":1}}`,
		"path-shaped theme":  `{"schema":"witself.dashboard-prefs.v1","theme":"../evil"}`,
		"slash theme":        `{"schema":"witself.dashboard-prefs.v1","theme":"a/b"}`,
		"space theme":        `{"schema":"witself.dashboard-prefs.v1","theme":"a b"}`,
		"control-char theme": `{"schema":"witself.dashboard-prefs.v1","theme":"a` + "\u0007" + `b"}`,
		"oversized theme": `{"schema":"witself.dashboard-prefs.v1","theme":"` +
			strings.Repeat("a", maxDashboardThemeBytes+1) + `"}`,
		"oversized document": `{"schema":"witself.dashboard-prefs.v1","theme":"console"` +
			strings.Repeat(" ", maxDashboardPreferencesBytes) + `}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := validateDashboardPreferences(json.RawMessage(raw)); !errors.Is(err, ErrDashboardPreferencesInvalid) {
				t.Fatalf("validate(%s) = %v, want ErrDashboardPreferencesInvalid", raw, err)
			}
		})
	}
}

func TestDashboardPreferencesRequireFullAgentPrincipal(t *testing.T) {
	st := &Store{}
	prefs := json.RawMessage(`{"schema":"witself.dashboard-prefs.v1","theme":"console"}`)
	cases := map[string]Principal{
		"operator": {Kind: PrincipalOperator, ID: "op_1", AccountID: "acc_1"},
		"curator profile": {Kind: PrincipalAgent, ID: "agt_1", AccountID: "acc_1",
			RealmID: "rlm_1", AccessProfile: AccessProfileCuratorApply},
		"missing realm": {Kind: PrincipalAgent, ID: "agt_1", AccountID: "acc_1"},
		"missing id":    {Kind: PrincipalAgent, AccountID: "acc_1", RealmID: "rlm_1"},
	}
	for name, principal := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := st.GetDashboardPreferences(context.Background(), principal); !errors.Is(err, ErrDashboardPreferencesForbidden) {
				t.Fatalf("get: %v, want ErrDashboardPreferencesForbidden", err)
			}
			if _, err := st.PutDashboardPreferences(context.Background(), principal, prefs); !errors.Is(err, ErrDashboardPreferencesForbidden) {
				t.Fatalf("put: %v, want ErrDashboardPreferencesForbidden", err)
			}
		})
	}
}

func TestPutDashboardPreferencesRejectsInvalidBeforeSQL(t *testing.T) {
	// A nil pool proves the validator runs before any query is attempted.
	st := &Store{}
	p := Principal{Kind: PrincipalAgent, ID: "agt_1", AccountID: "acc_1", RealmID: "rlm_1"}
	if _, err := st.PutDashboardPreferences(context.Background(), p, json.RawMessage(`{"theme":"console"}`)); !errors.Is(err, ErrDashboardPreferencesInvalid) {
		t.Fatalf("put invalid prefs = %v, want ErrDashboardPreferencesInvalid", err)
	}
}
