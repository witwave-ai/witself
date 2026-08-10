package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
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
	t.Setenv(agentEmailLegacyDomainsEnv, "legacy-one.example")
	pilot, err = agentEmailPilotConfigFromEnv()
	if err != nil || len(pilot.LegacyDomains) != 1 ||
		pilot.LegacyDomains[0] != "legacy-one.example" {
		t.Fatalf("legacy domains = %+v / %v", pilot.LegacyDomains, err)
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

func TestAgentEmailProductionConfigFromEnvDefaultOffAndValid(t *testing.T) {
	clearAgentEmailPilotEnv(t)
	t.Setenv(agentEmailProductionEnabledEnv, "true")
	t.Setenv(agentEmailReceiveDomainEnv, "witmail.net")
	t.Setenv(agentEmailReceiveAudienceEnv, "civo-sandbox-usw2-dev")
	t.Setenv(agentEmailReceiveAccountIDsEnv,
		"acc_aaaaaaaaaaaaaaaa,acc_bbbbbbbbbbbbbbbb")
	t.Setenv(agentEmailRetryCanaryAgentIDEnv, "agent_aaaaaaaaaaaaaaaa")
	setAgentEmailRelayPublicKeyEnv(t)

	receive, err := agentEmailReceiveConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !receive.Enabled || receive.Mode != server.AgentEmailReceiveModeProduction ||
		receive.Domain != "witmail.net" ||
		receive.Audience != "civo-sandbox-usw2-dev" ||
		len(receive.AccountIDs) != 2 ||
		!receive.AccountIDs["acc_aaaaaaaaaaaaaaaa"] ||
		len(receive.RealmIDs) != 0 || len(receive.AgentIDs) != 0 ||
		receive.RetryCanaryAgentID != "agent_aaaaaaaaaaaaaaaa" ||
		len(receive.RelayPublicKeys["pilot-key"]) != ed25519.PublicKeySize {
		t.Fatalf("valid production receive = %+v", receive)
	}

	t.Setenv(agentEmailPilotEnabledEnv, "true")
	if _, err := agentEmailReceiveConfigFromEnv(); err == nil ||
		!strings.Contains(err.Error(), "cannot both be true") {
		t.Fatalf("dual receive mode error = %v", err)
	}
}

func TestAgentEmailProductionConfigFromEnvRejectsUnsafeShapes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T)
		want   string
	}{
		{
			name: "invalid feature flag", want: agentEmailProductionEnabledEnv,
			mutate: func(t *testing.T) {
				t.Setenv(agentEmailProductionEnabledEnv, "enabled")
			},
		},
		{
			name: "missing domain", want: agentEmailReceiveDomainEnv,
			mutate: func(t *testing.T) { t.Setenv(agentEmailReceiveDomainEnv, "") },
		},
		{
			name: "missing cohort", want: agentEmailReceiveAccountIDsEnv,
			mutate: func(t *testing.T) { t.Setenv(agentEmailReceiveAccountIDsEnv, "") },
		},
		{
			name: "duplicate account", want: "duplicated",
			mutate: func(t *testing.T) {
				t.Setenv(agentEmailReceiveAccountIDsEnv,
					"acc_aaaaaaaaaaaaaaaa,acc_aaaaaaaaaaaaaaaa")
			},
		},
		{
			name: "invalid account", want: "canonical",
			mutate: func(t *testing.T) {
				t.Setenv(agentEmailReceiveAccountIDsEnv, "acc_not-valid")
			},
		},
		{
			name: "whitespace", want: "canonical",
			mutate: func(t *testing.T) {
				t.Setenv(agentEmailReceiveAccountIDsEnv, " acc_aaaaaaaaaaaaaaaa")
			},
		},
		{
			name: "unsorted", want: "sorted order",
			mutate: func(t *testing.T) {
				t.Setenv(agentEmailReceiveAccountIDsEnv,
					"acc_bbbbbbbbbbbbbbbb,acc_aaaaaaaaaaaaaaaa")
			},
		},
		{
			name: "wildcard", want: "canonical",
			mutate: func(t *testing.T) {
				t.Setenv(agentEmailReceiveAccountIDsEnv, "*")
			},
		},
		{
			name: "over cohort cap", want: "1-100",
			mutate: func(t *testing.T) {
				t.Setenv(agentEmailReceiveAccountIDsEnv,
					strings.TrimSuffix(strings.Repeat("acc_aaaaaaaaaaaaaaaa,", 101), ","))
			},
		},
		{
			name: "invalid canary", want: "retry canary",
			mutate: func(t *testing.T) {
				t.Setenv(agentEmailRetryCanaryAgentIDEnv, "agent_not-valid")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearAgentEmailPilotEnv(t)
			t.Setenv(agentEmailProductionEnabledEnv, "true")
			t.Setenv(agentEmailReceiveDomainEnv, "witmail.net")
			t.Setenv(agentEmailReceiveAudienceEnv, "cell-one")
			t.Setenv(agentEmailReceiveAccountIDsEnv, "acc_aaaaaaaaaaaaaaaa")
			setAgentEmailRelayPublicKeyEnv(t)
			tc.mutate(t)
			_, err := agentEmailReceiveConfigFromEnv()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestAgentEmailIdentifierParseErrorsAreValueFree(t *testing.T) {
	privateAccountID := "acc_aaaaaaaaaaaaaaaa"
	privateAgentID := "agent_bbbbbbbbbbbbbbbb"
	for _, tc := range []struct {
		name       string
		privateID  string
		parse      func() error
		wantDetail string
	}{
		{
			name: "invalid account", privateID: "acc_customer-private",
			parse: func() error {
				_, err := parseAgentEmailProductionAccountIDs("acc_customer-private")
				return err
			},
			wantDetail: "position 1 is not canonical",
		},
		{
			name: "duplicate account", privateID: privateAccountID,
			parse: func() error {
				_, err := parseAgentEmailProductionAccountIDs(
					privateAccountID + "," + privateAccountID,
				)
				return err
			},
			wantDetail: "position 2 is duplicated",
		},
		{
			name: "duplicate agent", privateID: privateAgentID,
			parse: func() error {
				_, err := parseAgentEmailIDSet(privateAgentID + "," + privateAgentID)
				return err
			},
			wantDetail: "position 2 is duplicated",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.parse()
			if err == nil || !strings.Contains(err.Error(), tc.wantDetail) {
				t.Fatalf("error = %v, want bounded detail %q", err, tc.wantDetail)
			}
			if strings.Contains(err.Error(), tc.privateID) {
				t.Fatalf("identifier leaked in parser error: %v", err)
			}
		})
	}
}

func TestAgentEmailLogSafeErrorRedactsIdentifiersAndPreservesReason(t *testing.T) {
	identifiers := []string{
		"acc_aaaaaaaaaaaaaaaa",
		"realm_bbbbbbbbbbbbbbbb",
		"agent_cccccccccccccccc",
	}
	cause := fmt.Errorf(
		"account %s realm %s agent %s: %w",
		identifiers[0], identifiers[1], identifiers[2],
		store.ErrAgentEmailPilotNotEnrolled,
	)
	err := newAgentEmailLogSafeError(
		"agent-email production startup preflight", "preflight_failed", cause,
	)
	if got, want := err.Error(),
		"agent-email production startup preflight failed (reason=cohort_not_ready)"; got != want {
		t.Fatalf("safe error = %q, want %q", got, want)
	}
	if !errors.Is(err, store.ErrAgentEmailPilotNotEnrolled) {
		t.Fatalf("safe error lost its internal sentinel: %v", err)
	}
	for _, identifier := range identifiers {
		if strings.Contains(err.Error(), identifier) {
			t.Fatalf("identifier %q leaked in safe error: %v", identifier, err)
		}
	}

	unknown := newAgentEmailLogSafeError(
		"agent-email production backfill reconciliation", "reconciliation_failed",
		errors.New(strings.Join(identifiers, " ")),
	)
	if got, want := unknown.Error(),
		"agent-email production backfill reconciliation failed (reason=reconciliation_failed)"; got != want {
		t.Fatalf("safe fallback error = %q, want %q", got, want)
	}
	for _, identifier := range identifiers {
		if strings.Contains(unknown.Error(), identifier) {
			t.Fatalf("identifier %q leaked in fallback error: %v", identifier, unknown)
		}
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
			name: "too many legacy domains", want: "at most 1",
			mutate: func(t *testing.T) {
				t.Setenv(agentEmailLegacyDomainsEnv, "legacy-one.example,legacy-two.example")
			},
		},
		{
			name: "primary repeated as legacy", want: "legacy domain",
			mutate: func(t *testing.T) {
				t.Setenv(agentEmailLegacyDomainsEnv, "agent-mail.witwave.ai")
			},
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

func TestAgentEmailRealmAliasProjectionDomainAllowed(t *testing.T) {
	pilot := server.AgentEmailPilotConfig{
		Enabled: true, Domain: "witmail.net",
		LegacyDomains: []string{"agent-mail.witwave.ai"},
	}
	for _, tc := range []struct {
		name   string
		domain string
		state  string
		want   bool
	}{
		{name: "primary applied", domain: "witmail.net", state: store.AgentEmailRealmAliasApplied, want: true},
		{name: "primary suspended", domain: "witmail.net", state: store.AgentEmailRealmAliasSuspended, want: true},
		{name: "legacy applied refused", domain: "agent-mail.witwave.ai", state: store.AgentEmailRealmAliasApplied},
		{name: "legacy suspended refused", domain: "agent-mail.witwave.ai", state: store.AgentEmailRealmAliasSuspended},
		{name: "legacy retired cleanup", domain: "agent-mail.witwave.ai", state: store.AgentEmailRealmAliasRetired, want: true},
		{name: "unknown retired refused", domain: "other.example", state: store.AgentEmailRealmAliasRetired},
		{name: "noncanonical refused", domain: "WITMAIL.NET", state: store.AgentEmailRealmAliasRetired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := server.AgentEmailRealmAliasApplyRequest{Domain: tc.domain, State: tc.state}
			if got := agentEmailRealmAliasProjectionDomainAllowed(pilot, in); got != tc.want {
				t.Fatalf("allowed = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestAgentEmailErrorMapping(t *testing.T) {
	rateLimit := &store.AgentEmailRateLimitError{
		Dimension:  "email_received_bytes",
		Scope:      "recipient",
		Source:     "plan",
		RetryAfter: 3 * time.Second,
		Retryable:  true,
	}
	if !errors.Is(mapAgentEmailIngestError(store.ErrAgentEmailUnknownRecipient), server.ErrAgentEmailUnknownRecipient) ||
		!errors.Is(mapAgentEmailIngestError(store.ErrAgentEmailPilotNotEnrolled), server.ErrAgentEmailUnknownRecipient) ||
		!errors.Is(mapAgentEmailIngestError(store.ErrAgentEmailReceiveDisabled), server.ErrAgentEmailReceiveDisabled) ||
		!errors.Is(mapAgentEmailIngestError(
			&store.FeatureNotEnabledError{Feature: "agent_email_receive"},
		), server.ErrAgentEmailFeatureDisabled) ||
		!errors.Is(mapAgentEmailIngestError(store.ErrAgentEmailRetryCanaryTemporary), server.ErrAgentEmailRetryCanaryTemporary) ||
		!errors.Is(mapAgentEmailIngestError(store.ErrAgentEmailRetryCanaryPermanent), server.ErrAgentEmailRetryCanaryPermanent) ||
		!errors.Is(mapAgentEmailIngestError(store.ErrAgentEmailPilotDisabled), server.ErrAgentEmailPilotUnavailable) ||
		!errors.Is(mapAgentEmailIngestError(rateLimit), server.ErrAgentEmailRateLimited) {
		t.Fatal("ingestion errors did not map to typed relay verdict errors")
	}
	var mappedRate *server.AgentEmailRateLimitError
	if err := mapAgentEmailIngestError(rateLimit); !errors.As(err, &mappedRate) ||
		mappedRate.Dimension != rateLimit.Dimension ||
		mappedRate.Scope != rateLimit.Scope ||
		mappedRate.Source != rateLimit.Source ||
		mappedRate.RetryAfter != rateLimit.RetryAfter ||
		mappedRate.Retryable != rateLimit.Retryable {
		t.Fatalf("agent-email rate mapping = %#v / %v", mappedRate, err)
	}
	if !errors.Is(mapAgentEmailError(store.ErrAgentEmailInputInvalid), server.ErrBadInput) ||
		!errors.Is(mapAgentEmailError(store.ErrAgentEmailNotFound), server.ErrNotFound) ||
		!errors.Is(mapAgentEmailError(store.ErrAgentEmailBusy), server.ErrBusy) ||
		!errors.Is(mapAgentEmailError(store.ErrAgentEmailClaimLost), server.ErrConflict) ||
		!errors.Is(mapAgentEmailError(store.ErrAgentEmailCodeConsumed), server.ErrAgentEmailCodeConsumed) ||
		!errors.Is(mapAgentEmailError(store.ErrAgentEmailForbidden), server.ErrForbidden) {
		t.Fatal("owner email errors did not preserve HTTP sentinel classes")
	}
	var featureErr *server.FeatureNotEnabledError
	if err := mapAgentEmailError(&store.FeatureNotEnabledError{Feature: "agent_email_receive"}); !errors.Is(err, server.ErrFeatureNotEnabled) || !errors.As(err, &featureErr) ||
		featureErr.Feature != "agent_email_receive" {
		t.Fatalf("agent-email feature refusal mapping = %#v / %v", featureErr, err)
	}
}

func TestAgentEmailStorageLimitMappingAndConversions(t *testing.T) {
	rawLimit := &store.PlanLimitError{
		Dimension: plans.AgentEmailMaxRawBytesLimit,
		Used:      10*1024*1024 + 1,
		Max:       10 * 1024 * 1024,
		Plan:      "private_plan_name",
	}
	if mapped := mapAgentEmailIngestError(rawLimit); !errors.Is(
		mapped, server.ErrAgentEmailRawSizeExceeded,
	) {
		t.Fatalf("raw-size mapping = %v", mapped)
	}
	otherLimit := &store.PlanLimitError{
		Dimension: plans.StoredMemoryLimit,
		Used:      2,
		Max:       1,
		Plan:      "private_plan_name",
	}
	if mapped := mapAgentEmailIngestError(otherLimit); mapped != otherLimit ||
		errors.Is(mapped, server.ErrAgentEmailRawSizeExceeded) {
		t.Fatalf("unrelated limit mapping = %v", mapped)
	}

	maximum, remaining := int64(8192), int64(0)
	status := toServerAgentEmailStorageStatus(store.AgentEmailStorageStatus{
		MaximumRawBytes: 10 * 1024 * 1024,
		AttachmentCapacity: store.MemoryLimitStatus{
			Used: 9000, Max: &maximum, Remaining: &remaining,
			NearLimit: true, OverLimit: true,
		},
	})
	if status.MaximumRawBytes != 10*1024*1024 ||
		status.AttachmentCapacity.Used != 9000 ||
		status.AttachmentCapacity.Max == nil ||
		*status.AttachmentCapacity.Max != maximum ||
		status.AttachmentCapacity.Remaining == nil ||
		*status.AttachmentCapacity.Remaining != remaining ||
		!status.AttachmentCapacity.NearLimit ||
		!status.AttachmentCapacity.OverLimit ||
		status.AttachmentCapacity.Unlimited ||
		status.AttachmentCapacity.AtLimit ||
		status.AttachmentCapacity.Unavailable {
		t.Fatalf("storage status conversion = %+v", status)
	}

	message := toServerAgentEmailMessage(store.AgentEmailMessage{
		AttachmentStorageBytes:         4096,
		RetainedAttachmentStorageBytes: 0,
		PayloadRetentionState:          store.AgentEmailPayloadOmittedCapacity,
	})
	if message.AttachmentStorageBytes != 4096 ||
		message.RetainedAttachmentStorageBytes != 0 ||
		message.PayloadRetentionState != "omitted_capacity" {
		t.Fatalf("message storage conversion = %+v", message)
	}
}

func TestWriteNewPrivateAgentEmailJSONIsExclusiveAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "primary-canary.json")
	value := agentEmailPrimaryCanaryManifest{
		SchemaVersion: 2, Domain: "witmail.net",
		WorkerName: "witself-agent-email-pilot",
		AccountIDs: []string{"acc_aaaaaaaaaaaaaaaa"},
		Agents: []agentEmailPrimaryCanaryManifestAgent{{
			AgentID: "agent_aaaaaaaaaaaaaaaa",
			RealmID: "realm_bbbbbbbbbbbbbbbb",
			Address: "alpha.bbbbbbbbbbbbbbbb@witmail.net",
		}},
	}
	if err := writeNewPrivateAgentEmailJSON(path, value); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("private manifest permissions = %o, want 600", got)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 5 || decoded["schema_version"] != float64(2) ||
		decoded["domain"] != "witmail.net" ||
		decoded["worker_name"] != "witself-agent-email-pilot" {
		t.Fatalf("private manifest envelope = %#v", decoded)
	}
	decodedAccounts, ok := decoded["account_ids"].([]any)
	if !ok || len(decodedAccounts) != 1 ||
		decodedAccounts[0] != "acc_aaaaaaaaaaaaaaaa" {
		t.Fatalf("private manifest accounts = %#v", decoded["account_ids"])
	}
	decodedAgents, ok := decoded["agents"].([]any)
	if !ok || len(decodedAgents) != 1 {
		t.Fatalf("private manifest agents = %#v", decoded["agents"])
	}
	decodedAgent, ok := decodedAgents[0].(map[string]any)
	if !ok || len(decodedAgent) != 3 ||
		decodedAgent["agent_id"] != "agent_aaaaaaaaaaaaaaaa" ||
		decodedAgent["realm_id"] != "realm_bbbbbbbbbbbbbbbb" ||
		decodedAgent["address"] != "alpha.bbbbbbbbbbbbbbbb@witmail.net" {
		t.Fatalf("private manifest agent = %#v", decodedAgents[0])
	}
	if err := writeNewPrivateAgentEmailJSON(path, struct{}{}); err == nil {
		t.Fatal("exclusive manifest rewrite unexpectedly succeeded")
	}
	payloadAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(payloadAfter) != string(payload) {
		t.Fatal("failed exclusive manifest rewrite changed the original file")
	}
}

func TestReadAgentEmailBackfillOverridesRequiresCanonicalPrivateManifest(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "overrides.json")
	payload := []byte(`{"schema_version":1,"overrides":[` +
		`{"agent_id":"agent_aaaaaaaaaaaaaaaa","agent_segment":"support-agent"},` +
		`{"agent_id":"agent_bbbbbbbbbbbbbbbb","agent_segment":"mail-bot"}]}`)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	overrides, err := readAgentEmailBackfillOverrides(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(overrides) != 2 || overrides["agent_aaaaaaaaaaaaaaaa"] != "support-agent" ||
		overrides["agent_bbbbbbbbbbbbbbbb"] != "mail-bot" {
		t.Fatalf("overrides = %#v", overrides)
	}
	if _, err := readAgentEmailBackfillOverrides("relative.json"); err == nil {
		t.Fatal("relative override manifest unexpectedly succeeded")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readAgentEmailBackfillOverrides(path); err == nil {
		t.Fatal("non-private override manifest unexpectedly succeeded")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(
		`{"schema_version":1,"overrides":[`+
			`{"agent_id":"agent_bbbbbbbbbbbbbbbb","agent_segment":"mail-bot"},`+
			`{"agent_id":"agent_aaaaaaaaaaaaaaaa","agent_segment":"support-agent"}]}`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readAgentEmailBackfillOverrides(path); err == nil {
		t.Fatal("unsorted override manifest unexpectedly succeeded")
	}
}

func TestReadAgentEmailBackfillOverridesRejectsMalformedBoundaryFiles(t *testing.T) {
	directory := t.TempDir()
	missing := filepath.Join(directory, "private-customer-identity.json")
	if _, err := readAgentEmailBackfillOverrides(missing); err == nil ||
		strings.Contains(err.Error(), missing) {
		t.Fatalf("missing private override error leaked its path: %v", err)
	}
	tests := map[string]string{
		"wrong-schema": `{"schema_version":2,"overrides":[` +
			`{"agent_id":"agent_aaaaaaaaaaaaaaaa","agent_segment":"support"}]}`,
		"empty": `{"schema_version":1,"overrides":[]}`,
		"unknown-field": `{"schema_version":1,"unexpected":true,"overrides":[` +
			`{"agent_id":"agent_aaaaaaaaaaaaaaaa","agent_segment":"support"}]}`,
		"trailing-json": `{"schema_version":1,"overrides":[` +
			`{"agent_id":"agent_aaaaaaaaaaaaaaaa","agent_segment":"support"}]} {}`,
		"duplicate-agent": `{"schema_version":1,"overrides":[` +
			`{"agent_id":"agent_aaaaaaaaaaaaaaaa","agent_segment":"support"},` +
			`{"agent_id":"agent_aaaaaaaaaaaaaaaa","agent_segment":"support-two"}]}`,
		"noncanonical-segment": `{"schema_version":1,"overrides":[` +
			`{"agent_id":"agent_aaaaaaaaaaaaaaaa","agent_segment":"Support"}]}`,
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(directory, name+".json")
			if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readAgentEmailBackfillOverrides(path); err == nil {
				t.Fatal("malformed override manifest unexpectedly succeeded")
			}
		})
	}

	oversize := filepath.Join(directory, "oversize.json")
	if err := os.WriteFile(oversize, []byte(strings.Repeat("x", 64*1024+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readAgentEmailBackfillOverrides(oversize); err == nil {
		t.Fatal("oversize override manifest unexpectedly succeeded")
	}

	nonregular := filepath.Join(directory, "directory.json")
	if err := os.Mkdir(nonregular, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readAgentEmailBackfillOverrides(nonregular); err == nil {
		t.Fatal("directory override manifest unexpectedly succeeded")
	}

	target := filepath.Join(directory, "target.json")
	if err := os.WriteFile(target, []byte(
		`{"schema_version":1,"overrides":[`+
			`{"agent_id":"agent_aaaaaaaaaaaaaaaa","agent_segment":"support"}]}`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := readAgentEmailBackfillOverrides(link); err == nil {
		t.Fatal("symlink override manifest unexpectedly succeeded")
	}
}

func TestAgentEmailBackfillCommandRequiresOnePrivateExceptionOutput(t *testing.T) {
	overrides := "/private/overrides.json"
	report := "/private/backfill-exception.json"
	gotOverrides, gotReport, ok := parseAgentEmailBackfillCommandArgs([]string{
		"--exception-output", report, "--overrides", overrides,
	})
	if !ok || gotOverrides != overrides || gotReport != report {
		t.Fatalf("parsed backfill args = %q / %q / %v", gotOverrides, gotReport, ok)
	}
	gotOverrides, gotReport, ok = parseAgentEmailBackfillCommandArgs([]string{
		"--exception-output", report,
	})
	if !ok || gotOverrides != "" || gotReport != report {
		t.Fatalf("parsed no-override backfill args = %q / %q / %v",
			gotOverrides, gotReport, ok)
	}
	for _, args := range [][]string{
		{},
		{"--overrides", overrides},
		{"--exception-output", report, "--exception-output", report},
		{"--exception-output", ""},
		{"--unknown", report},
	} {
		if _, _, ok := parseAgentEmailBackfillCommandArgs(args); ok {
			t.Fatalf("invalid backfill args unexpectedly parsed: %#v", args)
		}
	}
}

func TestAgentEmailBackfillExceptionReportIsPrivateAndExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backfill-exception.json")
	if err := validateNewPrivateAgentEmailJSONPath(path); err != nil {
		t.Fatal(err)
	}
	privateErr := &store.AgentEmailProductionBackfillError{
		AgentID: "agent_aaaaaaaaaaaaaaaa", RealmID: "realm_bbbbbbbbbbbbbbbb",
		ReasonCode: "agent_segment_requires_override", Err: store.ErrAgentEmailInputInvalid,
	}
	if strings.Contains(privateErr.Error(), privateErr.AgentID) ||
		strings.Contains(privateErr.Error(), privateErr.RealmID) ||
		!errors.Is(privateErr, store.ErrAgentEmailInputInvalid) {
		t.Fatalf("backfill exception error exposed identity or lost its sentinel: %v", privateErr)
	}
	report := agentEmailBackfillExceptionReport{
		SchemaVersion: 1, State: "requires_operator_override",
		ProcessedAgentCount: 3,
		AgentID:             privateErr.AgentID,
		RealmID:             privateErr.RealmID,
		ReasonCode:          privateErr.ReasonCode,
	}
	if err := writeNewPrivateAgentEmailJSON(path, report); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("exception report permissions = %o", info.Mode().Perm())
	}
	var decoded agentEmailBackfillExceptionReport
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != report {
		t.Fatalf("exception report = %#v", decoded)
	}
	if err := validateNewPrivateAgentEmailJSONPath(path); err == nil {
		t.Fatal("existing exception report path unexpectedly validated")
	}
	if err := writeNewPrivateAgentEmailJSON(path, report); err == nil {
		t.Fatal("exception report overwrite unexpectedly succeeded")
	}
}

func setValidAgentEmailPilotEnv(t *testing.T) {
	t.Helper()
	setAgentEmailRelayPublicKeyEnv(t)
	t.Setenv(agentEmailPilotEnabledEnv, "TRUE")
	t.Setenv(agentEmailPilotDomainEnv, "agent-mail.witwave.ai")
	t.Setenv(agentEmailPilotAudienceEnv, "cell-one")
	t.Setenv(agentEmailPilotRealmIDEnv, "realm_aaaaaaaaaaaaaaaa")
	t.Setenv(agentEmailPilotAgentIDsEnv, strings.Join([]string{
		"agent_aaaaaaaaaaaaaaaa", "agent_bbbbbbbbbbbbbbbb", "agent_cccccccccccccccc",
		"agent_dddddddddddddddd", "agent_eeeeeeeeeeeeeeee",
	}, ","))
	_ = os.Unsetenv(agentEmailRelayReplayWindowEnv)
}

func setAgentEmailRelayPublicKeyEnv(t *testing.T) {
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
	t.Setenv(agentEmailRelayPublicKeysEnv, string(encodedKeys))
}

func clearAgentEmailPilotEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		agentEmailPilotEnabledEnv, agentEmailPilotDomainEnv, agentEmailPilotAudienceEnv,
		agentEmailProductionEnabledEnv, agentEmailReceiveDomainEnv,
		agentEmailReceiveAudienceEnv, agentEmailReceiveAccountIDsEnv,
		agentEmailLegacyDomainsEnv,
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
