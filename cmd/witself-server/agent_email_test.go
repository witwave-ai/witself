package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

func TestAgentEmailPilotConfigFromEnvDefaultOffAndValid(t *testing.T) {
	clearAgentEmailPilotEnv(t)
	pilot, err := agentEmailPilotConfigFromEnv()
	if err != nil || pilot.Enabled {
		t.Fatalf("unset pilot = %+v, %v", pilot, err)
	}

	t.Setenv(agentEmailPilotEnabledEnv, "false")
	t.Setenv(agentEmailPilotAgentIDsEnv, "invalid-but-ignored")
	pilot, err = agentEmailPilotConfigFromEnv()
	if err != nil || pilot.Enabled {
		t.Fatalf("disabled pilot = %+v, %v", pilot, err)
	}

	setValidAgentEmailPilotEnv(t)
	pilot, err = agentEmailPilotConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !pilot.Enabled || pilot.Domain != "agent-mail.witwave.ai" || pilot.Audience != "cell-one" ||
		!pilot.RealmIDs["realm_aaaaaaaaaaaaaaaa"] || len(pilot.AgentIDs) != 5 ||
		len(pilot.RelayPublicKeys["pilot-key"]) != ed25519.PublicKeySize ||
		pilot.RelayReplayWindow != defaultAgentEmailReplayWindow {
		t.Fatalf("valid pilot = %+v", pilot)
	}
	t.Setenv(agentEmailRetryCanaryAgentIDEnv, "agent_aaaaaaaaaaaaaaaa")
	pilot, err = agentEmailPilotConfigFromEnv()
	if err != nil || pilot.RetryCanaryAgentID != "agent_aaaaaaaaaaaaaaaa" {
		t.Fatalf("retry canary config = %+v / %v", pilot, err)
	}

	t.Setenv(agentEmailRelayReplayWindowEnv, "90s")
	pilot, err = agentEmailPilotConfigFromEnv()
	if err != nil || pilot.RelayReplayWindow != 90*time.Second {
		t.Fatalf("custom replay window = %s, %v", pilot.RelayReplayWindow, err)
	}
}

func TestAgentEmailPilotConfigFromEnvRejectsUnsafeShapes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T)
		want   string
	}{
		{
			name: "invalid feature flag", want: agentEmailPilotEnabledEnv,
			mutate: func(t *testing.T) { t.Setenv(agentEmailPilotEnabledEnv, "enabled") },
		},
		{
			name: "missing required domain", want: agentEmailPilotDomainEnv,
			mutate: func(t *testing.T) { t.Setenv(agentEmailPilotDomainEnv, "") },
		},
		{
			name: "too few agents", want: "5-10",
			mutate: func(t *testing.T) {
				t.Setenv(agentEmailPilotAgentIDsEnv, "agent_aaaaaaaaaaaaaaaa,agent_bbbbbbbbbbbbbbbb")
			},
		},
		{
			name: "duplicate agent", want: "duplicated",
			mutate: func(t *testing.T) {
				t.Setenv(agentEmailPilotAgentIDsEnv, strings.Repeat("agent_aaaaaaaaaaaaaaaa,", 4)+"agent_aaaaaaaaaaaaaaaa")
			},
		},
		{
			name: "wrong audience case", want: "audience",
			mutate: func(t *testing.T) { t.Setenv(agentEmailPilotAudienceEnv, "Cell-One") },
		},
		{
			name: "bad key JSON", want: agentEmailRelayPublicKeysEnv,
			mutate: func(t *testing.T) { t.Setenv(agentEmailRelayPublicKeysEnv, `{"pilot-key":"broken"} trailing`) },
		},
		{
			name: "oversized replay window", want: "replay window",
			mutate: func(t *testing.T) { t.Setenv(agentEmailRelayReplayWindowEnv, "16m") },
		},
		{
			name: "unenrolled retry canary", want: "retry canary",
			mutate: func(t *testing.T) { t.Setenv(agentEmailRetryCanaryAgentIDEnv, "agent_zzzzzzzzzzzzzzzz") },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearAgentEmailPilotEnv(t)
			setValidAgentEmailPilotEnv(t)
			tc.mutate(t)
			_, err := agentEmailPilotConfigFromEnv()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestAgentEmailErrorMapping(t *testing.T) {
	if !errors.Is(mapAgentEmailIngestError(store.ErrAgentEmailUnknownRecipient), server.ErrAgentEmailUnknownRecipient) ||
		!errors.Is(mapAgentEmailIngestError(store.ErrAgentEmailPilotNotEnrolled), server.ErrAgentEmailUnknownRecipient) ||
		!errors.Is(mapAgentEmailIngestError(store.ErrAgentEmailReceiveDisabled), server.ErrAgentEmailReceiveDisabled) ||
		!errors.Is(mapAgentEmailIngestError(store.ErrAgentEmailRetryCanaryTemporary), server.ErrAgentEmailRetryCanaryTemporary) ||
		!errors.Is(mapAgentEmailIngestError(store.ErrAgentEmailRetryCanaryPermanent), server.ErrAgentEmailRetryCanaryPermanent) ||
		!errors.Is(mapAgentEmailIngestError(store.ErrAgentEmailPilotDisabled), server.ErrAgentEmailPilotUnavailable) {
		t.Fatal("ingestion errors did not map to typed relay verdict errors")
	}
	if !errors.Is(mapAgentEmailError(store.ErrAgentEmailInputInvalid), server.ErrBadInput) ||
		!errors.Is(mapAgentEmailError(store.ErrAgentEmailNotFound), server.ErrNotFound) ||
		!errors.Is(mapAgentEmailError(store.ErrAgentEmailBusy), server.ErrBusy) ||
		!errors.Is(mapAgentEmailError(store.ErrAgentEmailClaimLost), server.ErrConflict) ||
		!errors.Is(mapAgentEmailError(store.ErrAgentEmailCodeConsumed), server.ErrAgentEmailCodeConsumed) ||
		!errors.Is(mapAgentEmailError(store.ErrAgentEmailForbidden), server.ErrForbidden) {
		t.Fatal("owner email errors did not preserve HTTP sentinel classes")
	}
}

func setValidAgentEmailPilotEnv(t *testing.T) {
	t.Helper()
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encodedKeys, err := json.Marshal(map[string]string{
		"pilot-key": base64.StdEncoding.EncodeToString(publicKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(agentEmailPilotEnabledEnv, "TRUE")
	t.Setenv(agentEmailPilotDomainEnv, "agent-mail.witwave.ai")
	t.Setenv(agentEmailPilotAudienceEnv, "cell-one")
	t.Setenv(agentEmailPilotRealmIDEnv, "realm_aaaaaaaaaaaaaaaa")
	t.Setenv(agentEmailPilotAgentIDsEnv, strings.Join([]string{
		"agent_aaaaaaaaaaaaaaaa", "agent_bbbbbbbbbbbbbbbb", "agent_cccccccccccccccc",
		"agent_dddddddddddddddd", "agent_eeeeeeeeeeeeeeee",
	}, ","))
	t.Setenv(agentEmailRelayPublicKeysEnv, string(encodedKeys))
	_ = os.Unsetenv(agentEmailRelayReplayWindowEnv)
}

func clearAgentEmailPilotEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		agentEmailPilotEnabledEnv, agentEmailPilotDomainEnv, agentEmailPilotAudienceEnv,
		agentEmailPilotRealmIDEnv, agentEmailPilotAgentIDsEnv,
		agentEmailRelayPublicKeysEnv, agentEmailRelayReplayWindowEnv,
		agentEmailRetryCanaryAgentIDEnv,
	} {
		original, present := os.LookupEnv(name)
		name, original, present := name, original, present
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(name, original)
				return
			}
			_ = os.Unsetenv(name)
		})
		_ = os.Unsetenv(name)
	}
}
