package trust_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell/trust"
)

func addIgnoredField(raw []byte, name string, value []byte) []byte {
	nameJSON, _ := json.Marshal(name)
	result := append([]byte{}, bytes.TrimSpace(raw)...)
	result = result[:len(result)-1]
	result = append(result, ',')
	result = append(result, nameJSON...)
	result = append(result, ':')
	result = append(result, value...)
	return append(result, '}')
}

func TestExactTopLevelFieldsComponentSet(t *testing.T) {
	base := vectorBytes(t, "positive-base.json")
	for _, c := range []struct{ name, value string }{
		{"RELEASE_SEQUENCE", "99"},
		{"ROOT_SEQUENCE", "99"},
		{"DELEGATION_SEQUENCE", "99"},
		{"FORMAT_VERSION", "99"},
		{"KIND", `"attacker-kind"`},
		{"\u212aind", `"unicode-casefold-kind"`},
		{"CRITICAL", `["unknown_critical_field"]`},
		{"PLATFORMS", `[{"unknown":true,"components":[{"required_extensions":null,"schema":{"target":1.5}}]}]`},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := trust.ParseComponentSet(addIgnoredField(base, c.name, []byte(c.value)))
			if err != nil {
				t.Fatalf("unknown noncritical field influenced parsing: %v", err)
			}
			want, err := trust.ParseComponentSet(base)
			if err != nil {
				t.Fatal("base fixture invalid")
			}
			gotJSON, gotErr := json.Marshal(got)
			wantJSON, wantErr := json.Marshal(want)
			if gotErr != nil || wantErr != nil || !bytes.Equal(gotJSON, wantJSON) {
				t.Fatal("unknown field changed understood component metadata")
			}
		})
	}
}

func TestExactTopLevelFieldsAuthorityDocuments(t *testing.T) {
	for _, c := range []struct{ kind, fixture, field, value string }{
		{"delegation", "positive-delegation.json", "DELEGATION_SEQUENCE", "99"},
		{"delegation", "positive-delegation.json", "ROOT_SEQUENCE", "99"},
		{"delegation", "positive-delegation.json", "KEYS", `[{"unknown":true}]`},
		{"delegation", "positive-delegation.json", "REVOKED_KEY_IDS", `["not-a-key"]`},
		{"delegation", "positive-delegation.json", "CRITICAL", `["unknown"]`},
		{"root_rotation", "positive-rotation.json", "ROOT_SEQUENCE", "99"},
		{"root_rotation", "positive-rotation.json", "NEW_SEQUENCE", "99"},
		{"root_rotation", "positive-rotation.json", "NEW_ROOT", `{"unknown":true}`},
		{"root_rotation", "positive-rotation.json", "CRITICAL", `["unknown"]`},
	} {
		t.Run(c.kind+"/"+c.field, func(t *testing.T) {
			base := vectorBytes(t, c.fixture)
			raw := addIgnoredField(base, c.field, []byte(c.value))
			var want, got any
			var err error
			if c.kind == "delegation" {
				want, err = trust.ParseDelegation(base)
				if err != nil {
					t.Fatal("base delegation invalid")
				}
				got, err = trust.ParseDelegation(raw)
			} else {
				want, err = trust.ParseRootRotation(base)
				if err != nil {
					t.Fatal("base rotation invalid")
				}
				got, err = trust.ParseRootRotation(raw)
			}
			if err != nil {
				t.Fatalf("unknown noncritical authority field influenced parsing: %v", err)
			}
			gotJSON, gotErr := json.Marshal(got)
			wantJSON, wantErr := json.Marshal(want)
			if gotErr != nil || wantErr != nil || !bytes.Equal(gotJSON, wantJSON) {
				t.Fatal("unknown field changed understood authority metadata")
			}
		})
	}
}

func TestExactTopLevelFieldsSignedVerifier(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-10-27T12:00:00Z")
	opts := trust.Options{Roots: []trust.PublicKey{fixturePublic(t, "root-a")}, Clock: trust.Clock{Now: now, Trusted: true}}
	t.Run("delegation-and-component", func(t *testing.T) {
		v := trust.NewVerifier(opts)
		doc := addIgnoredField(vectorBytes(t, "positive-delegation.json"), "DELEGATION_SEQUENCE", []byte("99"))
		doc = addIgnoredField(doc, "KEYS", []byte(`[{"unknown":true}]`))
		sig, err := trust.SignDocument(doc, "delegation", fixturePrivate(t, "root-a"))
		if err != nil {
			t.Fatal("fixture signing failed")
		}
		out := v.VerifyDelegation(doc, sig)
		if !out.Accepted || out.DelegationSequence != 1 {
			t.Fatalf("ignored authority field altered signed result: %s", out.Reason)
		}
		doc = addIgnoredField(vectorBytes(t, "positive-base.json"), "RELEASE_SEQUENCE", []byte("99"))
		doc = addIgnoredField(doc, "PLATFORMS", []byte(`[{"unknown":true}]`))
		doc = addIgnoredField(doc, "CRITICAL", []byte(`["unknown"]`))
		sig, err = trust.SignDocument(doc, "component_set", fixturePrivate(t, "release-1"))
		if err != nil {
			t.Fatal("fixture signing failed")
		}
		out = v.AcceptForAcquisition(doc, sig)
		if !out.Accepted || out.ReleaseSequence != 1 || out.DelegationSequence != 1 {
			t.Fatalf("ignored component field altered signed result: %s", out.Reason)
		}
	})
	t.Run("rotation", func(t *testing.T) {
		v := trust.NewVerifier(opts)
		doc := addIgnoredField(vectorBytes(t, "positive-rotation.json"), "NEW_SEQUENCE", []byte("99"))
		doc = addIgnoredField(doc, "NEW_ROOT", []byte(`{"unknown":true}`))
		sig, err := trust.SignDocument(doc, "root_rotation", fixturePrivate(t, "root-a"), fixturePrivate(t, "root-b"))
		if err != nil {
			t.Fatal("fixture signing failed")
		}
		out := v.VerifyRotation(doc, sig)
		if !out.Accepted || out.RootSequence != 2 {
			t.Fatalf("ignored rotation field altered signed result: %s", out.Reason)
		}
	})
}
