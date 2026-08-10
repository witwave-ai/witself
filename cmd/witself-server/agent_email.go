package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/witwave-ai/witself/internal/agentemail"
	"github.com/witwave-ai/witself/internal/plans"
	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

const (
	agentEmailPilotEnabledEnv        = "WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED"
	agentEmailPilotDomainEnv         = "WITSELF_AGENT_EMAIL_PILOT_DOMAIN"
	agentEmailProductionEnabledEnv   = "WITSELF_AGENT_EMAIL_RECEIVE_PRODUCTION_ENABLED"
	agentEmailReceiveDomainEnv       = "WITSELF_AGENT_EMAIL_RECEIVE_DOMAIN"
	agentEmailReceiveAudienceEnv     = "WITSELF_AGENT_EMAIL_RECEIVE_AUDIENCE"
	agentEmailReceiveAccountIDsEnv   = "WITSELF_AGENT_EMAIL_RECEIVE_ACCOUNT_IDS"
	agentEmailLegacyDomainsEnv       = "WITSELF_AGENT_EMAIL_ACCEPTED_LEGACY_DOMAINS"
	agentEmailPilotAudienceEnv       = "WITSELF_AGENT_EMAIL_PILOT_AUDIENCE"
	agentEmailPilotRealmIDEnv        = "WITSELF_AGENT_EMAIL_PILOT_REALM_ID"
	agentEmailPilotAgentIDsEnv       = "WITSELF_AGENT_EMAIL_PILOT_AGENT_IDS"
	agentEmailRelayPublicKeysEnv     = "WITSELF_AGENT_EMAIL_RELAY_PUBLIC_KEYS_JSON"
	agentEmailRelayReplayWindowEnv   = "WITSELF_AGENT_EMAIL_RELAY_REPLAY_WINDOW"
	agentEmailRetryCanaryAgentIDEnv  = "WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID"
	defaultAgentEmailReplayWindow    = 5 * time.Minute
	maximumAgentEmailReceiveAccounts = 100
	agentEmailPrimaryCanaryDomain    = "witmail.net"
	agentEmailPrimaryCanaryWorker    = "witself-agent-email-pilot"
)

type agentEmailPrimaryCanaryManifestAgent struct {
	AgentID string `json:"agent_id"`
	RealmID string `json:"realm_id"`
	Address string `json:"address"`
}

type agentEmailPrimaryCanaryManifest struct {
	SchemaVersion int                                    `json:"schema_version"`
	Domain        string                                 `json:"domain"`
	WorkerName    string                                 `json:"worker_name"`
	AccountIDs    []string                               `json:"account_ids"`
	Agents        []agentEmailPrimaryCanaryManifestAgent `json:"agents"`
}

type agentEmailBackfillOverride struct {
	AgentID      string `json:"agent_id"`
	AgentSegment string `json:"agent_segment"`
}

type agentEmailBackfillOverrideManifest struct {
	SchemaVersion int                          `json:"schema_version"`
	Overrides     []agentEmailBackfillOverride `json:"overrides"`
}

type agentEmailBackfillExceptionReport struct {
	SchemaVersion       int    `json:"schema_version"`
	State               string `json:"state"`
	ProcessedAgentCount int64  `json:"processed_agent_count"`
	AgentID             string `json:"agent_id"`
	RealmID             string `json:"realm_id"`
	ReasonCode          string `json:"reason_code"`
}

// agentEmailLogSafeError keeps an internal cause available to errors.Is/As
// without allowing its potentially identifying text to cross a pod-log
// boundary. Operation and reason are fixed, bounded server vocabulary.
type agentEmailLogSafeError struct {
	operation string
	reason    string
	cause     error
}

func (e *agentEmailLogSafeError) Error() string {
	return fmt.Sprintf("%s failed (reason=%s)", e.operation, e.reason)
}

func (e *agentEmailLogSafeError) Unwrap() error {
	return e.cause
}

func newAgentEmailLogSafeError(operation, fallbackReason string, cause error) error {
	reason := fallbackReason
	switch {
	case errors.Is(cause, context.Canceled):
		reason = "canceled"
	case errors.Is(cause, context.DeadlineExceeded):
		reason = "deadline_exceeded"
	case errors.Is(cause, store.ErrAgentEmailPilotDisabled),
		errors.Is(cause, store.ErrAgentEmailReceiveDisabled):
		reason = "receive_disabled"
	case errors.Is(cause, store.ErrAgentEmailInputInvalid):
		reason = "invalid_configuration"
	case errors.Is(cause, store.ErrAgentEmailPilotNotEnrolled):
		reason = "cohort_not_ready"
	case errors.Is(cause, store.ErrAccountNotFound):
		reason = "account_not_found"
	case errors.Is(cause, store.ErrAccountNotActive):
		reason = "account_not_active"
	case errors.Is(cause, store.ErrAgentEmailAddressMissing):
		reason = "mailbox_missing"
	case errors.Is(cause, store.ErrAgentEmailAddressConflict),
		errors.Is(cause, store.ErrAgentEmailConflict):
		reason = "conflict"
	}
	return &agentEmailLogSafeError{
		operation: operation,
		reason:    reason,
		cause:     cause,
	}
}

// agentEmailReceiveConfigFromEnv parses all relay trust and exactly one receive
// mode before listeners start. Both legacy pilot and production receive are
// independently default-off and mutually exclusive.
func agentEmailReceiveConfigFromEnv() (server.AgentEmailReceiveConfig, error) {
	pilotEnabled, err := parseAgentEmailEnabledEnv(agentEmailPilotEnabledEnv)
	if err != nil {
		return server.AgentEmailReceiveConfig{}, err
	}
	productionEnabled, err := parseAgentEmailEnabledEnv(agentEmailProductionEnabledEnv)
	if err != nil {
		return server.AgentEmailReceiveConfig{}, err
	}
	if pilotEnabled && productionEnabled {
		return server.AgentEmailReceiveConfig{}, fmt.Errorf(
			"%s and %s cannot both be true",
			agentEmailPilotEnabledEnv, agentEmailProductionEnabledEnv,
		)
	}
	if !pilotEnabled && !productionEnabled {
		return server.AgentEmailReceiveConfig{}, nil
	}
	enabledEnv := agentEmailPilotEnabledEnv
	domainEnv := agentEmailPilotDomainEnv
	audienceEnv := agentEmailPilotAudienceEnv
	mode := server.AgentEmailReceiveModeLegacyPilot
	if productionEnabled {
		enabledEnv = agentEmailProductionEnabledEnv
		domainEnv = agentEmailReceiveDomainEnv
		audienceEnv = agentEmailReceiveAudienceEnv
		mode = server.AgentEmailReceiveModeProduction
	}
	require := func(name string) (string, error) {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			return "", fmt.Errorf("%s is required when %s=true", name, enabledEnv)
		}
		return value, nil
	}
	domain, err := require(domainEnv)
	if err != nil {
		return server.AgentEmailReceiveConfig{}, err
	}
	legacyDomains, err := parseAgentEmailLegacyDomains(os.Getenv(agentEmailLegacyDomainsEnv))
	if err != nil {
		return server.AgentEmailReceiveConfig{}, fmt.Errorf("%s: %w", agentEmailLegacyDomainsEnv, err)
	}
	audience, err := require(audienceEnv)
	if err != nil {
		return server.AgentEmailReceiveConfig{}, err
	}
	var accountIDs, realmIDs, agentIDs map[string]bool
	if productionEnabled {
		accountIDsText, present := os.LookupEnv(agentEmailReceiveAccountIDsEnv)
		if !present || strings.TrimSpace(accountIDsText) == "" {
			return server.AgentEmailReceiveConfig{}, fmt.Errorf(
				"%s is required when %s=true",
				agentEmailReceiveAccountIDsEnv, enabledEnv,
			)
		}
		accountIDs, err = parseAgentEmailProductionAccountIDs(accountIDsText)
		if err != nil {
			return server.AgentEmailReceiveConfig{}, fmt.Errorf("%s: %w", agentEmailReceiveAccountIDsEnv, err)
		}
	} else {
		realmID, err := require(agentEmailPilotRealmIDEnv)
		if err != nil {
			return server.AgentEmailReceiveConfig{}, err
		}
		agentIDsText, err := require(agentEmailPilotAgentIDsEnv)
		if err != nil {
			return server.AgentEmailReceiveConfig{}, err
		}
		agentIDs, err = parseAgentEmailIDSet(agentIDsText)
		if err != nil {
			return server.AgentEmailReceiveConfig{}, fmt.Errorf("%s: %w", agentEmailPilotAgentIDsEnv, err)
		}
		realmIDs = map[string]bool{realmID: true}
	}
	encodedKeys, err := require(agentEmailRelayPublicKeysEnv)
	if err != nil {
		return server.AgentEmailReceiveConfig{}, err
	}
	var keyValues map[string]string
	decoder := json.NewDecoder(strings.NewReader(encodedKeys))
	if err := decoder.Decode(&keyValues); err != nil {
		return server.AgentEmailReceiveConfig{}, fmt.Errorf("%s must be a JSON object: %w", agentEmailRelayPublicKeysEnv, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return server.AgentEmailReceiveConfig{}, fmt.Errorf("%s must contain one JSON value", agentEmailRelayPublicKeysEnv)
	}
	publicKeys := make(map[string]ed25519.PublicKey, len(keyValues))
	for keyID, encoded := range keyValues {
		key, err := agentemail.ParsePublicKey(encoded)
		if err != nil {
			return server.AgentEmailReceiveConfig{}, fmt.Errorf("%s key %q is invalid: %w", agentEmailRelayPublicKeysEnv, keyID, err)
		}
		publicKeys[keyID] = key
	}
	replayWindow := defaultAgentEmailReplayWindow
	if value := strings.TrimSpace(os.Getenv(agentEmailRelayReplayWindowEnv)); value != "" {
		replayWindow, err = time.ParseDuration(value)
		if err != nil {
			return server.AgentEmailReceiveConfig{}, fmt.Errorf("%s must be a duration: %w", agentEmailRelayReplayWindowEnv, err)
		}
	}
	receive := server.AgentEmailReceiveConfig{
		Enabled: true, Mode: mode, Domain: domain,
		LegacyDomains: legacyDomains, Audience: audience,
		AccountIDs: accountIDs, RealmIDs: realmIDs, AgentIDs: agentIDs,
		RetryCanaryAgentID: strings.TrimSpace(os.Getenv(agentEmailRetryCanaryAgentIDEnv)),
		RelayPublicKeys:    publicKeys, RelayReplayWindow: replayWindow,
	}
	if err := server.ValidateAgentEmailReceiveConfig(receive); err != nil {
		return server.AgentEmailReceiveConfig{}, err
	}
	return receive, nil
}

// agentEmailPilotConfigFromEnv is retained for source compatibility with the
// focused legacy parser tests and downstream embedders.
func agentEmailPilotConfigFromEnv() (server.AgentEmailPilotConfig, error) {
	return agentEmailReceiveConfigFromEnv()
}

// runAgentEmailProductionBackfill is an explicit one-shot operator action.
// Serving replicas never call it: they perform only read-only production
// preflight, while this command owns the bounded, idempotent mailbox backfill.
func runAgentEmailProductionBackfill(overridesPath, exceptionOutputPath string) int {
	receive, err := agentEmailReceiveConfigFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-server: %v\n", newAgentEmailLogSafeError(
			"agent-email production backfill configuration", "invalid_configuration", err,
		))
		return 1
	}
	if !receive.Enabled || receive.Mode != server.AgentEmailReceiveModeProduction {
		fmt.Fprintf(os.Stderr,
			"witself-server: agent-email backfill requires production receive to be enabled\n")
		return 1
	}
	overrides, err := readAgentEmailBackfillOverrides(overridesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-server: %v\n", newAgentEmailLogSafeError(
			"agent-email production backfill override validation", "invalid_override_manifest", err,
		))
		return 1
	}
	if err := validateNewPrivateAgentEmailJSONPath(exceptionOutputPath); err != nil {
		fmt.Fprintf(os.Stderr, "witself-server: %v\n", newAgentEmailLogSafeError(
			"agent-email production backfill exception output validation",
			"invalid_exception_output", err,
		))
		return 1
	}
	dsn := dbDSN()
	if dsn == "" {
		fmt.Fprintf(os.Stderr,
			"witself-server: agent-email backfill requires WITSELF_DATABASE_URL or DATABASE_URL\n")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-server: %v\n", newAgentEmailLogSafeError(
			"agent-email production backfill database open", "database_unavailable", err,
		))
		return 1
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		fmt.Fprintf(os.Stderr, "witself-server: %v\n", newAgentEmailLogSafeError(
			"agent-email production backfill migration", "migration_failed", err,
		))
		return 1
	}
	scope := toStoreAgentEmailReceiveScope(receive)
	before, err := st.PreflightAgentEmailProductionCohort(ctx, scope)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-server: %v\n", newAgentEmailLogSafeError(
			"agent-email production backfill preflight", "preflight_failed", err,
		))
		return 1
	}
	processed, err := st.ReconcileAgentEmailProductionCohortWithOverrides(
		ctx, scope, overrides,
	)
	if err != nil {
		var exception *store.AgentEmailProductionBackfillError
		if errors.As(err, &exception) {
			report := agentEmailBackfillExceptionReport{
				SchemaVersion: 1, State: "requires_operator_override",
				ProcessedAgentCount: processed,
				AgentID:             exception.AgentID, RealmID: exception.RealmID,
				ReasonCode: exception.ReasonCode,
			}
			if reportErr := writeNewPrivateAgentEmailJSON(
				exceptionOutputPath, report,
			); reportErr != nil {
				fmt.Fprintln(os.Stderr,
					"witself-server: agent-email backfill could not write its private exception report")
				return 1
			}
			fmt.Fprintln(os.Stderr,
				"witself-server: agent-email backfill requires review of its private exception report")
			return 1
		}
		fmt.Fprintf(os.Stderr, "witself-server: %v\n", newAgentEmailLogSafeError(
			"agent-email production backfill reconciliation", "reconciliation_failed", err,
		))
		return 1
	}
	after, err := st.PreflightAgentEmailProductionCohort(ctx, scope)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-server: %v\n", newAgentEmailLogSafeError(
			"agent-email production backfill verification", "verification_failed", err,
		))
		return 1
	}
	if after.MissingMailboxCount != 0 {
		fmt.Fprintf(os.Stderr,
			"witself-server: agent-email backfill verification found %d live agents without mailboxes; rerun the idempotent command\n",
			after.MissingMailboxCount)
		return 1
	}
	result := struct {
		AccountCount        int   `json:"account_count"`
		LiveAgentCount      int64 `json:"live_agent_count"`
		MissingBefore       int64 `json:"missing_mailbox_count_before"`
		ProcessedAgentCount int64 `json:"processed_agent_count"`
		ReadyMailboxCount   int64 `json:"ready_mailbox_count"`
		MissingAfter        int64 `json:"missing_mailbox_count_after"`
		RetryCanaryReady    bool  `json:"retry_canary_ready"`
		OverrideCount       int   `json:"override_count"`
	}{
		AccountCount: before.AccountCount, LiveAgentCount: after.LiveAgentCount,
		MissingBefore: before.MissingMailboxCount, ProcessedAgentCount: processed,
		ReadyMailboxCount: after.ReadyMailboxCount,
		MissingAfter:      after.MissingMailboxCount, RetryCanaryReady: after.RetryCanaryReady,
		OverrideCount: len(overrides),
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintf(os.Stderr, "witself-server: %v\n", newAgentEmailLogSafeError(
			"agent-email production backfill result encoding", "result_encoding_failed", err,
		))
		return 1
	}
	return 0
}

func validateNewPrivateAgentEmailJSONPath(path string) error {
	if path == "" || path != strings.TrimSpace(path) || !filepath.IsAbs(path) ||
		filepath.Clean(path) != path {
		return errors.New("path must be one canonical absolute path")
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("path could not be inspected")
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil || !parent.IsDir() {
		return errors.New("parent directory is missing or invalid")
	}
	return nil
}

func readAgentEmailBackfillOverrides(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	if path != strings.TrimSpace(path) || !filepath.IsAbs(path) ||
		filepath.Clean(path) != path {
		return nil, errors.New("override manifest path must be one canonical absolute path")
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("override manifest could not be inspected")
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Mode().Perm() != 0o600 ||
		pathInfo.Size() < 2 || pathInfo.Size() > 64*1024 {
		return nil, errors.New("override manifest must be a 2-65536 byte regular mode-0600 file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("override manifest could not be opened")
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, errors.New("override manifest could not be verified")
	}
	if !os.SameFile(pathInfo, openedInfo) {
		return nil, errors.New("override manifest changed while opening")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 64*1024+1))
	decoder.DisallowUnknownFields()
	var manifest agentEmailBackfillOverrideManifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, errors.New("override manifest was invalid JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("override manifest must contain one JSON value")
	}
	if manifest.SchemaVersion != 1 || len(manifest.Overrides) < 1 ||
		len(manifest.Overrides) > 1000 {
		return nil, errors.New("override manifest must contain 1-1000 schema-v1 overrides")
	}
	result := make(map[string]string, len(manifest.Overrides))
	previousAgentID := ""
	for index, override := range manifest.Overrides {
		if !validAgentEmailConfigGeneratedID(override.AgentID, "agent") ||
			override.AgentID <= previousAgentID {
			return nil, errors.New("override agent ids must be unique and in canonical sorted order")
		}
		segment, err := agentemail.ValidateAgentSegment(override.AgentSegment)
		if err != nil || segment != override.AgentSegment {
			return nil, fmt.Errorf(
				"override entry %d has an invalid canonical agent segment", index+1,
			)
		}
		result[override.AgentID] = segment
		previousAgentID = override.AgentID
	}
	return result, nil
}

// runAgentEmailProductionCanaryManifest emits the edge tool's exact private
// manifest from actual cell mailbox rows. It performs no database writes and
// refuses stdout so agent IDs and addresses cannot accidentally enter logs.
func runAgentEmailProductionCanaryManifest(outputPath string) int {
	receive, err := agentEmailReceiveConfigFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-server: %v\n", newAgentEmailLogSafeError(
			"agent-email production canary configuration", "invalid_configuration", err,
		))
		return 1
	}
	if !receive.Enabled || receive.Mode != server.AgentEmailReceiveModeProduction {
		fmt.Fprintln(os.Stderr,
			"witself-server: agent-email canary manifest requires production receive to be enabled")
		return 1
	}
	if receive.Domain != agentEmailPrimaryCanaryDomain {
		fmt.Fprintf(os.Stderr,
			"witself-server: agent-email canary manifest requires domain %s\n",
			agentEmailPrimaryCanaryDomain)
		return 1
	}
	if outputPath == "" || outputPath != strings.TrimSpace(outputPath) ||
		!filepath.IsAbs(outputPath) || filepath.Clean(outputPath) != outputPath {
		fmt.Fprintln(os.Stderr,
			"witself-server: agent-email canary manifest requires one canonical absolute --output path")
		return 1
	}
	dsn := dbDSN()
	if dsn == "" {
		fmt.Fprintln(os.Stderr,
			"witself-server: agent-email canary manifest requires WITSELF_DATABASE_URL or DATABASE_URL")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-server: %v\n", newAgentEmailLogSafeError(
			"agent-email production canary database open", "database_unavailable", err,
		))
		return 1
	}
	defer st.Close()
	if err := st.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "witself-server: %v\n", newAgentEmailLogSafeError(
			"agent-email production canary database ping", "database_unavailable", err,
		))
		return 1
	}
	agents, err := st.ListAgentEmailProductionCanaryAgents(
		ctx, toStoreAgentEmailReceiveScope(receive),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-server: %v\n", newAgentEmailLogSafeError(
			"agent-email production canary snapshot", "canary_snapshot_failed", err,
		))
		return 1
	}
	accountIDs := make([]string, 0, len(receive.AccountIDs))
	for accountID, enabled := range receive.AccountIDs {
		if enabled {
			accountIDs = append(accountIDs, accountID)
		}
	}
	sort.Strings(accountIDs)
	manifest := agentEmailPrimaryCanaryManifest{
		SchemaVersion: 2,
		Domain:        agentEmailPrimaryCanaryDomain,
		WorkerName:    agentEmailPrimaryCanaryWorker,
		AccountIDs:    accountIDs,
		Agents:        make([]agentEmailPrimaryCanaryManifestAgent, len(agents)),
	}
	for i, agent := range agents {
		manifest.Agents[i] = agentEmailPrimaryCanaryManifestAgent{
			AgentID: agent.AgentID, RealmID: agent.RealmID, Address: agent.Address,
		}
	}
	if err := writeNewPrivateAgentEmailJSON(outputPath, manifest); err != nil {
		fmt.Fprintln(os.Stderr,
			"witself-server: agent-email canary manifest could not create its private output")
		return 1
	}
	if _, err := fmt.Fprintf(os.Stdout,
		"witself-server: wrote private agent-email canary manifest with %d agents\n",
		len(agents)); err != nil {
		fmt.Fprintln(os.Stderr,
			"witself-server: agent-email canary manifest could not report completion")
		return 1
	}
	return 0
}

func writeNewPrivateAgentEmailJSON(path string, value any) error {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	fail := func(writeErr error) error {
		_ = file.Close()
		_ = os.Remove(path)
		return writeErr
	}
	if err := file.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := file.Write(payload); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func parseAgentEmailEnabledEnv(name string) (bool, error) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return false, nil
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return enabled, nil
}

func parseAgentEmailLegacyDomains(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	if len(parts) > 1 {
		return nil, errors.New("at most 1 legacy domain may be configured")
	}
	result := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, raw := range parts {
		domain := strings.TrimSpace(raw)
		if domain == "" {
			return nil, errors.New("legacy domains must be a comma-separated non-empty set")
		}
		if seen[domain] {
			return nil, fmt.Errorf("legacy domain %q is duplicated", domain)
		}
		seen[domain] = true
		result = append(result, domain)
	}
	return result, nil
}

func parseAgentEmailIDSet(value string) (map[string]bool, error) {
	return parseAgentEmailGeneratedIDSet(value, "agent ids", "agent id")
}

func parseAgentEmailGeneratedIDSet(value, plural, singular string) (map[string]bool, error) {
	result := make(map[string]bool)
	for index, raw := range strings.Split(value, ",") {
		id := strings.TrimSpace(raw)
		if id == "" {
			return nil, fmt.Errorf("%s must be a comma-separated non-empty set", plural)
		}
		if result[id] {
			return nil, fmt.Errorf("%s at position %d is duplicated", singular, index+1)
		}
		result[id] = true
	}
	return result, nil
}

func parseAgentEmailProductionAccountIDs(value string) (map[string]bool, error) {
	if value == "" || value != strings.TrimSpace(value) {
		return nil, errors.New("account ids must be canonical without surrounding whitespace")
	}
	parts := strings.Split(value, ",")
	if len(parts) < 1 || len(parts) > maximumAgentEmailReceiveAccounts {
		return nil, fmt.Errorf("account ids must contain 1-%d entries", maximumAgentEmailReceiveAccounts)
	}
	result := make(map[string]bool, len(parts))
	previous := ""
	for index, accountID := range parts {
		if accountID != strings.TrimSpace(accountID) ||
			!validAgentEmailConfigGeneratedID(accountID, "acc") {
			return nil, fmt.Errorf("account id at position %d is not canonical", index+1)
		}
		if result[accountID] {
			return nil, fmt.Errorf("account id at position %d is duplicated", index+1)
		}
		if previous != "" && accountID < previous {
			return nil, errors.New("account ids must be in canonical sorted order")
		}
		result[accountID] = true
		previous = accountID
	}
	return result, nil
}

func validAgentEmailConfigGeneratedID(value, prefix string) bool {
	body := strings.TrimPrefix(value, prefix+"_")
	if body == value || len(body) != 16 {
		return false
	}
	for _, char := range []byte(body) {
		if (char < 'a' || char > 'z') && (char < '2' || char > '7') {
			return false
		}
	}
	return true
}

func configureAgentEmail(ctx context.Context, cfg *server.Config, st *store.Store, receive server.AgentEmailReceiveConfig) error {
	cfg.AgentEmailReceive = receive
	// Alias projection is a control-plane lifecycle surface, not a process-local
	// pilot enrollment surface. Keep it wired even while receive is disabled so
	// a later policy/configuration change needs no client reinstall or schema
	// rewrite. When this process does have one receive domain, only the primary
	// domain may gain or retain reversible alias authority. A configured legacy
	// domain may converge only to a terminal tombstone, so cleanup cannot mint a
	// new suspended legacy claim.
	cfg.ApplyAgentEmailRealmAlias = func(
		ctx context.Context,
		accountID string,
		in server.AgentEmailRealmAliasApplyRequest,
	) (server.AgentEmailRealmAlias, error) {
		if receive.Enabled && !agentEmailRealmAliasProjectionDomainAllowed(receive, in) {
			return server.AgentEmailRealmAlias{}, server.ErrBadInput
		}
		alias, err := st.ApplyAgentEmailRealmAlias(ctx, accountID, store.ApplyAgentEmailRealmAliasInput{
			ClaimID: in.ClaimID, RealmID: in.RealmID, Domain: in.Domain,
			RealmLabel: in.RealmLabel, State: in.State,
			ControllerRevision: in.ControllerRevision,
		})
		if err != nil {
			return server.AgentEmailRealmAlias{}, mapAgentEmailRealmAliasError(err)
		}
		return toServerAgentEmailRealmAlias(alias), nil
	}
	cfg.GetAgentEmailRealmAlias = func(
		ctx context.Context,
		accountID, claimID string,
	) (server.AgentEmailRealmAlias, error) {
		alias, err := st.GetAgentEmailRealmAlias(ctx, accountID, claimID)
		if err != nil {
			return server.AgentEmailRealmAlias{}, mapAgentEmailRealmAliasError(err)
		}
		return toServerAgentEmailRealmAlias(alias), nil
	}
	cfg.GetAgentEmailRealmAliasTarget = func(
		ctx context.Context,
		accountID, realmID string,
	) (server.AgentEmailRealmAliasTarget, error) {
		target, err := st.GetAgentEmailRealmAliasTarget(ctx, accountID, realmID)
		if err != nil {
			return server.AgentEmailRealmAliasTarget{}, mapAgentEmailRealmAliasError(err)
		}
		return server.AgentEmailRealmAliasTarget{
			AccountID: target.AccountID,
			RealmID:   target.RealmID,
			Exists:    target.Exists,
		}, nil
	}
	cfg.ListAgentEmailRealmAliases = func(
		ctx context.Context,
		accountID string,
	) ([]server.AgentEmailRealmAlias, error) {
		aliases, err := st.ListAgentEmailRealmAliases(ctx, accountID)
		if err != nil {
			return nil, mapAgentEmailRealmAliasError(err)
		}
		result := make([]server.AgentEmailRealmAlias, len(aliases))
		for i, alias := range aliases {
			result[i] = toServerAgentEmailRealmAlias(alias)
		}
		return result, nil
	}
	cfg.ApplyAgentEmailCustomDomainRoute = func(
		ctx context.Context,
		accountID string,
		in server.AgentEmailCustomDomainRouteApplyRequest,
	) (server.AgentEmailCustomDomainRoute, error) {
		route, err := st.ApplyAgentEmailCustomDomainRoute(
			ctx, accountID, store.ApplyAgentEmailCustomDomainRouteInput{
				DomainRequestID:          in.DomainRequestID,
				DomainAllocationRevision: in.DomainAllocationRevision,
				DomainStateRevision:      in.DomainStateRevision,
				RealmAliasClaimID:        in.RealmAliasClaimID,
				RealmAliasRevision:       in.RealmAliasRevision,
				RealmID:                  in.RealmID, Domain: in.Domain,
				RealmLabel: in.RealmLabel, State: in.State,
				SuspensionDisposition: in.SuspensionDisposition,
				ControllerRevision:    in.ControllerRevision,
			},
		)
		if err != nil {
			return server.AgentEmailCustomDomainRoute{}, mapAgentEmailCustomDomainRouteError(err)
		}
		return toServerAgentEmailCustomDomainRoute(route), nil
	}
	cfg.GetAgentEmailCustomDomainRoute = func(
		ctx context.Context,
		accountID, domainRequestID, realmAliasClaimID string,
	) (server.AgentEmailCustomDomainRoute, error) {
		route, err := st.GetAgentEmailCustomDomainRoute(
			ctx, accountID, domainRequestID, realmAliasClaimID,
		)
		if err != nil {
			return server.AgentEmailCustomDomainRoute{}, mapAgentEmailCustomDomainRouteError(err)
		}
		return toServerAgentEmailCustomDomainRoute(route), nil
	}
	cfg.GetRealmEmailRouteLifecycle = func(
		ctx context.Context,
		accountID, realmID string,
	) (server.RealmEmailRouteLifecycle, error) {
		route, err := st.GetRealmEmailRouteLifecycle(ctx, accountID, realmID)
		if err != nil {
			return server.RealmEmailRouteLifecycle{}, mapRealmEmailRouteLifecycleError(err)
		}
		return toServerRealmEmailRouteLifecycle(route), nil
	}
	cfg.ListRealmEmailRouteLifecycles = func(
		ctx context.Context,
		accountID, cursor string,
		limit int,
	) (server.RealmEmailRouteLifecyclePage, error) {
		page, err := st.ListRealmEmailRouteLifecycles(ctx, accountID, cursor, limit)
		if err != nil {
			return server.RealmEmailRouteLifecyclePage{}, mapRealmEmailRouteLifecycleError(err)
		}
		routes := make([]server.RealmEmailRouteLifecycle, len(page.Routes))
		for i, route := range page.Routes {
			routes[i] = toServerRealmEmailRouteLifecycle(route)
		}
		return server.RealmEmailRouteLifecyclePage{
			Routes: routes, NextCursor: page.NextCursor,
		}, nil
	}
	cfg.PrepareRealmEmailRouteRetirement = func(
		ctx context.Context,
		accountID string,
		in server.RealmEmailRouteRetirementRequest,
	) (server.RealmEmailRouteLifecycle, error) {
		route, err := st.PrepareRealmEmailRouteRetirement(
			ctx, accountID, toStoreRealmEmailRouteRetirementInput(in),
		)
		if err != nil {
			return server.RealmEmailRouteLifecycle{}, mapRealmEmailRouteLifecycleError(err)
		}
		return toServerRealmEmailRouteLifecycle(route), nil
	}
	cfg.CommitRealmEmailRouteRetirement = func(
		ctx context.Context,
		accountID string,
		in server.RealmEmailRouteRetirementRequest,
	) (server.RealmEmailRouteLifecycle, error) {
		route, err := st.CommitRealmEmailRouteRetirement(
			ctx, accountID, toStoreRealmEmailRouteRetirementInput(in),
		)
		if err != nil {
			return server.RealmEmailRouteLifecycle{}, mapRealmEmailRouteLifecycleError(err)
		}
		return toServerRealmEmailRouteLifecycle(route), nil
	}
	if !receive.Enabled {
		return nil
	}
	scope := toStoreAgentEmailReceiveScope(receive)
	if receive.Mode == server.AgentEmailReceiveModeProduction {
		if err := st.ValidateAgentEmailProductionCohort(ctx, scope); err != nil {
			return newAgentEmailLogSafeError(
				"agent-email production startup preflight", "preflight_failed", err,
			)
		}
	} else if _, err := st.ReconcileAgentEmailPilot(ctx, scope); err != nil {
		return newAgentEmailLogSafeError(
			"agent-email pilot startup reconciliation", "reconciliation_failed", err,
		)
	}
	cfg.IngestAgentEmailPilot = func(ctx context.Context, relay agentemail.RelayMetadata, raw []byte) error {
		message, err := st.IngestAgentEmailPilot(
			ctx, scope, store.AgentEmailIngestInput{Relay: relay, Raw: raw},
		)
		if err != nil {
			return mapAgentEmailIngestError(err)
		}
		if message.PayloadRetentionState == store.AgentEmailPayloadOmittedCapacity {
			return server.ErrAgentEmailAttachmentOmitted
		}
		return nil
	}
	cfg.RequireAgentEmailEntitlement = func(ctx context.Context, p server.DomainPrincipal) error {
		return mapAgentEmailError(st.RequireAgentEmailReceiveEnabled(ctx, toStorePrincipal(p)))
	}
	cfg.GetAgentEmailAddress = func(ctx context.Context, p server.DomainPrincipal) (server.AgentEmailAddress, error) {
		address, err := st.GetAgentEmailAddress(ctx, scope, toStorePrincipal(p))
		if err != nil {
			return server.AgentEmailAddress{}, mapAgentEmailError(err)
		}
		return toServerAgentEmailAddress(address), nil
	}
	cfg.GetAgentEmailStorageStatus = func(ctx context.Context, p server.DomainPrincipal) (server.AgentEmailStorageStatus, error) {
		status, err := st.GetAgentEmailStorageStatus(ctx, toStorePrincipal(p))
		if err != nil {
			return server.AgentEmailStorageStatus{}, mapAgentEmailError(err)
		}
		return toServerAgentEmailStorageStatus(status), nil
	}
	cfg.ArmAgentEmailRetryCanary = func(ctx context.Context, p server.DomainPrincipal, challenge string) (server.AgentEmailRetryCanaryCheckpoint, error) {
		checkpoint, err := st.ArmAgentEmailRetryCanary(ctx, scope, toStorePrincipal(p), challenge)
		return toServerAgentEmailRetryCanaryCheckpoint(checkpoint), mapAgentEmailError(err)
	}
	cfg.GetAgentEmailRetryCanary = func(ctx context.Context, p server.DomainPrincipal, challenge string) (server.AgentEmailRetryCanaryCheckpoint, error) {
		checkpoint, err := st.GetAgentEmailRetryCanaryStatus(ctx, scope, toStorePrincipal(p), challenge)
		return toServerAgentEmailRetryCanaryCheckpoint(checkpoint), mapAgentEmailError(err)
	}
	cfg.GetAgentEmailReceiveControl = func(ctx context.Context, accountID, operatorID, agentID string) (server.AgentEmailReceiveControl, error) {
		control, err := st.GetAgentEmailReceiveControl(ctx, scope, accountID, operatorID, agentID)
		return toServerAgentEmailReceiveControl(control), mapAgentEmailError(err)
	}
	cfg.SetAgentEmailReceiveControl = func(ctx context.Context, accountID, operatorID, agentID, receiveState string) (server.AgentEmailReceiveControl, error) {
		control, err := st.SetAgentEmailReceiveControl(ctx, scope, accountID, operatorID, agentID, receiveState)
		return toServerAgentEmailReceiveControl(control), mapAgentEmailError(err)
	}
	cfg.GetRealmEmailReceiveControl = func(ctx context.Context, accountID, operatorID, realmID string) (server.AgentEmailRealmReceiveControl, error) {
		control, err := st.GetRealmAgentEmailReceiveControl(ctx, scope, accountID, operatorID, realmID)
		return toServerAgentEmailRealmReceiveControl(control), mapAgentEmailError(err)
	}
	cfg.SetRealmEmailReceiveControl = func(ctx context.Context, accountID, operatorID, realmID, receiveState string) (server.AgentEmailRealmReceiveControl, error) {
		control, err := st.SetRealmAgentEmailReceiveControl(ctx, scope, accountID, operatorID, realmID, receiveState)
		return toServerAgentEmailRealmReceiveControl(control), mapAgentEmailError(err)
	}
	cfg.ListAgentEmails = func(ctx context.Context, p server.DomainPrincipal, opts server.AgentEmailListOptions) (server.AgentEmailPage, error) {
		page, err := st.ListAgentEmails(ctx, scope, toStorePrincipal(p), store.AgentEmailFilter{
			Unread: opts.Unread, Unacked: opts.Unacked, OldestFirst: opts.OldestFirst,
			Limit: opts.Limit, Cursor: opts.Cursor,
		})
		if err != nil {
			return server.AgentEmailPage{}, mapAgentEmailError(err)
		}
		messages := make([]server.AgentEmailMessage, len(page.Messages))
		for i, message := range page.Messages {
			messages[i] = toServerAgentEmailMessage(message)
		}
		return server.AgentEmailPage{Messages: messages, NextCursor: page.NextCursor}, nil
	}
	cfg.ReadAgentEmail = func(ctx context.Context, p server.DomainPrincipal, messageID string) (server.AgentEmailMessage, error) {
		message, err := st.ReadAgentEmail(ctx, scope, toStorePrincipal(p), messageID)
		if err != nil {
			return server.AgentEmailMessage{}, mapAgentEmailError(err)
		}
		return toServerAgentEmailMessage(message), nil
	}
	cfg.AckAgentEmail = func(ctx context.Context, p server.DomainPrincipal, messageID string) (server.AgentEmailMessage, error) {
		message, err := st.AckAgentEmail(ctx, scope, toStorePrincipal(p), messageID)
		if err != nil {
			return server.AgentEmailMessage{}, mapAgentEmailError(err)
		}
		return toServerAgentEmailMessage(message), nil
	}
	cfg.MarkAgentEmailCodeConsumed = func(ctx context.Context, p server.DomainPrincipal, messageID string) (server.AgentEmailMessage, error) {
		message, err := st.MarkAgentEmailCodeConsumed(ctx, scope, toStorePrincipal(p), messageID)
		if err != nil {
			return server.AgentEmailMessage{}, mapAgentEmailError(err)
		}
		return toServerAgentEmailMessage(message), nil
	}
	cfg.GetSelfAgentEmailCheckpoint = func(ctx context.Context, p server.DomainPrincipal) (server.AgentEmailCheckpoint, error) {
		checkpoint, err := st.GetSelfAgentEmailCheckpoint(ctx, scope, toStorePrincipal(p))
		if err != nil {
			return server.AgentEmailCheckpoint{}, mapAgentEmailError(err)
		}
		enabled := checkpoint.Enabled
		return server.AgentEmailCheckpoint{
			Enabled: &enabled,
			Pending: checkpoint.Pending, MailboxPending: checkpoint.MailboxPending,
			ReceiveState:      checkpoint.ReceiveState,
			AgentReceiveState: checkpoint.AgentReceiveState,
			RealmReceiveState: checkpoint.RealmReceiveState,
		}, nil
	}
	cfg.ClaimAgentEmail = func(ctx context.Context, p server.DomainPrincipal, messageID string, in server.ClaimAgentEmailRequest) (server.AgentEmailProcessing, error) {
		processing, err := st.ClaimAgentEmail(ctx, scope, toStorePrincipal(p), messageID, store.ClaimAgentEmailInput{
			LeaseDuration: time.Duration(in.LeaseSeconds) * time.Second, IdempotencyKey: in.IdempotencyKey,
		})
		return toServerAgentEmailProcessing(processing), mapAgentEmailError(err)
	}
	cfg.RenewAgentEmailClaim = func(ctx context.Context, p server.DomainPrincipal, messageID string, in server.RenewAgentEmailClaimRequest) (server.AgentEmailProcessing, error) {
		processing, err := st.RenewAgentEmailClaim(ctx, scope, toStorePrincipal(p), messageID, store.RenewAgentEmailClaimInput{
			ClaimID: in.ClaimID, Generation: in.Generation,
			LeaseDuration: time.Duration(in.LeaseSeconds) * time.Second,
		})
		return toServerAgentEmailProcessing(processing), mapAgentEmailError(err)
	}
	cfg.ReleaseAgentEmailClaim = func(ctx context.Context, p server.DomainPrincipal, messageID string, in server.ReleaseAgentEmailClaimRequest) (server.AgentEmailProcessing, error) {
		processing, err := st.ReleaseAgentEmailClaim(ctx, scope, toStorePrincipal(p), messageID, store.ReleaseAgentEmailClaimInput{
			ClaimID: in.ClaimID, Generation: in.Generation, DeterministicFailure: in.DeterministicFailure,
		})
		return toServerAgentEmailProcessing(processing), mapAgentEmailError(err)
	}
	cfg.CompleteAgentEmail = func(ctx context.Context, p server.DomainPrincipal, messageID string, in server.CompleteAgentEmailRequest) (server.AgentEmailProcessing, error) {
		processing, err := st.CompleteAgentEmail(ctx, scope, toStorePrincipal(p), messageID, store.CompleteAgentEmailInput{
			ClaimID: in.ClaimID, Generation: in.Generation, IdempotencyKey: in.IdempotencyKey,
		})
		return toServerAgentEmailProcessing(processing), mapAgentEmailError(err)
	}
	return nil
}

func toStoreAgentEmailReceiveScope(receive server.AgentEmailReceiveConfig) store.AgentEmailReceiveScope {
	return store.AgentEmailReceiveScope{
		Enabled: receive.Enabled, Mode: receive.Mode, Domain: receive.Domain,
		LegacyDomains:      append([]string(nil), receive.LegacyDomains...),
		Audience:           receive.Audience,
		AccountIDs:         cloneAgentEmailBoolMap(receive.AccountIDs),
		RealmIDs:           cloneAgentEmailBoolMap(receive.RealmIDs),
		AgentIDs:           cloneAgentEmailBoolMap(receive.AgentIDs),
		RetryCanaryAgentID: receive.RetryCanaryAgentID,
	}
}

func agentEmailRealmAliasProjectionDomainAllowed(
	pilot server.AgentEmailPilotConfig,
	in server.AgentEmailRealmAliasApplyRequest,
) bool {
	primary, err := agentemail.ValidateDomain(pilot.Domain)
	if err != nil || primary != pilot.Domain {
		return false
	}
	domain, err := agentemail.ValidateDomain(in.Domain)
	if err != nil || domain != in.Domain {
		return false
	}
	if domain == primary {
		return true
	}
	if in.State != store.AgentEmailRealmAliasRetired {
		return false
	}
	for _, legacy := range pilot.LegacyDomains {
		if domain == legacy {
			return true
		}
	}
	return false
}

func toServerRealmEmailRouteLifecycle(
	route store.RealmEmailRouteLifecycle,
) server.RealmEmailRouteLifecycle {
	return server.RealmEmailRouteLifecycle{
		AccountID: route.AccountID, RealmID: route.RealmID,
		State: route.State, Generation: route.Generation,
		OperationID: route.OperationID,
	}
}

func toStoreRealmEmailRouteRetirementInput(
	in server.RealmEmailRouteRetirementRequest,
) store.RealmEmailRouteRetirementInput {
	return store.RealmEmailRouteRetirementInput{
		RealmID: in.RealmID, OperationID: in.OperationID,
		ExpectedGeneration: in.ExpectedGeneration,
	}
}

func mapRealmEmailRouteLifecycleError(err error) error {
	switch {
	case errors.Is(err, store.ErrRealmEmailRouteInputInvalid):
		return server.ErrBadInput
	case errors.Is(err, store.ErrAccountNotFound), errors.Is(err, store.ErrRealmNotFound):
		return server.ErrNotFound
	case errors.Is(err, store.ErrRealmEmailRouteConflict):
		return server.ErrConflict
	default:
		return err
	}
}

func cloneAgentEmailBoolMap(source map[string]bool) map[string]bool {
	result := make(map[string]bool, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func mapAgentEmailIngestError(err error) error {
	var limitErr *store.PlanLimitError
	var rateErr *store.AgentEmailRateLimitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &rateErr) && rateErr != nil:
		return &server.AgentEmailRateLimitError{
			Dimension:  rateErr.Dimension,
			Scope:      rateErr.Scope,
			Source:     rateErr.Source,
			RetryAfter: rateErr.RetryAfter,
			Retryable:  rateErr.Retryable,
		}
	case errors.As(err, &limitErr) &&
		limitErr.Dimension == plans.AgentEmailMaxRawBytesLimit:
		return server.ErrAgentEmailRawSizeExceeded
	case errors.Is(err, store.ErrAgentEmailUnknownRecipient),
		errors.Is(err, store.ErrAgentEmailPilotNotEnrolled),
		errors.Is(err, store.ErrAgentEmailAddressMissing):
		return server.ErrAgentEmailUnknownRecipient
	case errors.Is(err, store.ErrAgentEmailReceiveDisabled):
		return server.ErrAgentEmailReceiveDisabled
	case errors.Is(err, store.ErrFeatureNotEnabled):
		return server.ErrAgentEmailFeatureDisabled
	case errors.Is(err, store.ErrAgentEmailRetryCanaryTemporary):
		return server.ErrAgentEmailRetryCanaryTemporary
	case errors.Is(err, store.ErrAgentEmailRetryCanaryPermanent):
		return server.ErrAgentEmailRetryCanaryPermanent
	case errors.Is(err, store.ErrAgentEmailPilotDisabled):
		return server.ErrAgentEmailPilotUnavailable
	case errors.Is(err, store.ErrAgentEmailInputInvalid):
		return wrapAsSentinel(server.ErrBadInput, store.ErrAgentEmailInputInvalid, err)
	default:
		return err
	}
}

func mapAgentEmailError(err error) error {
	var featureErr *store.FeatureNotEnabledError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &featureErr):
		return &server.FeatureNotEnabledError{Feature: featureErr.Feature}
	case errors.Is(err, store.ErrAgentEmailInputInvalid), errors.Is(err, store.ErrAgentEmailCursorInvalid):
		return wrapAsSentinel(server.ErrBadInput, store.ErrAgentEmailInputInvalid, err)
	case errors.Is(err, store.ErrAgentEmailNotFound), errors.Is(err, store.ErrAgentEmailAddressMissing):
		return server.ErrNotFound
	case errors.Is(err, store.ErrAgentEmailBusy):
		return server.ErrBusy
	case errors.Is(err, store.ErrAgentEmailClaimLost), errors.Is(err, store.ErrAgentEmailConflict):
		return server.ErrConflict
	case errors.Is(err, store.ErrAgentEmailCodeConsumed):
		return server.ErrAgentEmailCodeConsumed
	case errors.Is(err, store.ErrAgentEmailForbidden), errors.Is(err, store.ErrAgentEmailPilotNotEnrolled),
		errors.Is(err, store.ErrAgentNotFound), errors.Is(err, store.ErrAccountNotActive):
		return server.ErrForbidden
	case errors.Is(err, store.ErrAccountNotFound):
		return server.ErrNotFound
	default:
		return err
	}
}

func mapAgentEmailRealmAliasError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrAgentEmailInputInvalid):
		return wrapAsSentinel(server.ErrBadInput, store.ErrAgentEmailInputInvalid, err)
	case errors.Is(err, store.ErrAccountNotFound), errors.Is(err, store.ErrRealmNotFound),
		errors.Is(err, store.ErrAgentEmailRealmAliasNotFound):
		return server.ErrNotFound
	case errors.Is(err, store.ErrAgentEmailRealmAliasConflict):
		return server.ErrConflict
	default:
		return err
	}
}

func mapAgentEmailCustomDomainRouteError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrAgentEmailInputInvalid):
		return wrapAsSentinel(server.ErrBadInput, store.ErrAgentEmailInputInvalid, err)
	case errors.Is(err, store.ErrAccountNotFound), errors.Is(err, store.ErrRealmNotFound),
		errors.Is(err, store.ErrAgentEmailCustomDomainRouteNotFound):
		return server.ErrNotFound
	case errors.Is(err, store.ErrAgentEmailCustomDomainRouteConflict):
		return server.ErrConflict
	default:
		return err
	}
}

func toServerAgentEmailAddress(address store.AgentEmailAddress) server.AgentEmailAddress {
	addresses := make([]server.AgentEmailCanonicalAddress, len(address.Addresses))
	for i, canonical := range address.Addresses {
		addresses[i] = server.AgentEmailCanonicalAddress{
			Address: canonical.Address, Domain: canonical.Domain, Role: canonical.Role,
		}
	}
	aliases := make([]server.AgentEmailRealmAliasAddress, len(address.Aliases))
	for i, alias := range address.Aliases {
		aliases[i] = server.AgentEmailRealmAliasAddress{
			ClaimID: alias.ClaimID, Address: alias.Address, LocalPart: alias.LocalPart,
			RealmLabel: alias.RealmLabel, State: alias.State,
			ControllerRevision: alias.ControllerRevision, UpdatedAt: alias.UpdatedAt,
			SuspendedAt: alias.SuspendedAt, RetiredAt: alias.RetiredAt,
		}
	}
	return server.AgentEmailAddress{
		ID: address.ID, MailboxID: address.MailboxID, AccountID: address.AccountID,
		RealmID: address.RealmID, OwnerAgentID: address.OwnerAgentID,
		Address: address.Address, Domain: address.Domain, LocalPart: address.LocalPart,
		AgentSegment: address.AgentSegment, RealmLabel: address.RealmLabel,
		ProvisioningKind: address.ProvisioningKind, ReceiveState: address.ReceiveState,
		AgentReceiveState: address.AgentReceiveState,
		RealmReceiveState: address.RealmReceiveState, RowVersion: address.RowVersion,
		CreatedAt: address.CreatedAt, UpdatedAt: address.UpdatedAt,
		DisabledAt: address.DisabledAt, RealmDisabledAt: address.RealmDisabledAt,
		RetiredAt: address.RetiredAt, Addresses: addresses, Aliases: aliases,
	}
}

func toServerAgentEmailRealmAlias(alias store.AgentEmailRealmAlias) server.AgentEmailRealmAlias {
	return server.AgentEmailRealmAlias{
		ClaimID: alias.ClaimID, AccountID: alias.AccountID, RealmID: alias.RealmID,
		Domain: alias.Domain, RealmLabel: alias.RealmLabel, State: alias.State,
		ControllerRevision: alias.ControllerRevision, CreatedAt: alias.CreatedAt,
		UpdatedAt: alias.UpdatedAt, SuspendedAt: alias.SuspendedAt,
		RetiredAt: alias.RetiredAt,
	}
}

func toServerAgentEmailCustomDomainRoute(
	route store.AgentEmailCustomDomainRoute,
) server.AgentEmailCustomDomainRoute {
	return server.AgentEmailCustomDomainRoute{
		SchemaVersion: "witself.v0", AccountID: route.AccountID,
		DomainRequestID:          route.DomainRequestID,
		DomainAllocationRevision: route.DomainAllocationRevision,
		DomainStateRevision:      route.DomainStateRevision,
		RealmAliasClaimID:        route.RealmAliasClaimID,
		RealmAliasRevision:       route.RealmAliasRevision,
		RealmID:                  route.RealmID, Domain: route.Domain,
		RealmLabel: route.RealmLabel, State: route.State,
		SuspensionDisposition: route.SuspensionDisposition,
		ControllerRevision:    route.ControllerRevision,
	}
}

func toServerAgentEmailReceiveControl(control store.AgentEmailReceiveControl) server.AgentEmailReceiveControl {
	return server.AgentEmailReceiveControl{
		AccountID: control.AccountID, RealmID: control.RealmID, AgentID: control.AgentID,
		ReceiveState: control.ReceiveState, AgentReceiveState: control.AgentReceiveState,
		RealmReceiveState: control.RealmReceiveState, RowVersion: control.RowVersion,
		UpdatedAt: control.UpdatedAt, DisabledAt: control.DisabledAt,
		RealmDisabledAt: control.RealmDisabledAt,
	}
}

func toServerAgentEmailRealmReceiveControl(control store.AgentEmailRealmReceiveControl) server.AgentEmailRealmReceiveControl {
	return server.AgentEmailRealmReceiveControl{
		AccountID: control.AccountID, RealmID: control.RealmID,
		ReceiveState: control.ReceiveState, MailboxCount: control.MailboxCount,
		RowVersion: control.RowVersion, UpdatedAt: control.UpdatedAt,
		DisabledAt: control.DisabledAt,
	}
}

func toServerAgentEmailMessage(message store.AgentEmailMessage) server.AgentEmailMessage {
	return server.AgentEmailMessage{
		ID: message.ID, AccountID: message.AccountID, RealmID: message.RealmID,
		MailboxID: message.MailboxID, OwnerAgentID: message.OwnerAgentID, AddressID: message.AddressID,
		Provider: message.Provider, EnvelopeSender: message.EnvelopeSender,
		EnvelopeRecipient: message.EnvelopeRecipient, AgentSegment: message.AgentSegment,
		RealmLabel: message.RealmLabel, RecipientRouteKind: message.RecipientRouteKind,
		RecipientRealmAliasClaimID:     message.RecipientRealmAliasClaimID,
		RecipientCustomDomainRequestID: message.RecipientCustomDomainRequestID,
		SubaddressTag:                  message.SubaddressTag,
		RawSizeBytes:                   message.RawSizeBytes, ParseState: message.ParseState,
		ParseErrorCode: message.ParseErrorCode, HeaderFrom: message.HeaderFrom,
		HeaderTo: message.HeaderTo, Subject: message.Subject, MIMEMessageID: message.MIMEMessageID,
		MessageDate: message.MessageDate, AttachmentCount: message.AttachmentCount,
		AttachmentStorageBytes:         message.AttachmentStorageBytes,
		RetainedAttachmentStorageBytes: message.RetainedAttachmentStorageBytes,
		PayloadRetentionState:          message.PayloadRetentionState,
		SPFResult:                      message.SPFResult, DKIMResult: message.DKIMResult,
		DMARCResult: message.DMARCResult, SpamVerdict: message.SpamVerdict,
		SenderVerificationState:    message.SenderVerificationState,
		PossibleDuplicate:          message.PossibleDuplicate,
		PossibleDuplicateOfMessage: message.PossibleDuplicateOfMessage,
		ReceivedAt:                 message.ReceivedAt, CreatedAt: message.CreatedAt,
		Folder: message.Folder, DeliveredAt: message.DeliveredAt,
		ReadState: server.AgentEmailReadState{
			State: message.ReadState.State, ReadAt: message.ReadState.ReadAt,
			AckedAt: message.ReadState.AckedAt, CodeConsumedAt: message.ReadState.CodeConsumedAt,
		},
		Processing: toServerAgentEmailProcessing(message.Processing),
		Text:       message.Text, TextKind: message.TextKind,
	}
}

func toServerAgentEmailStorageStatus(
	status store.AgentEmailStorageStatus,
) server.AgentEmailStorageStatus {
	return server.AgentEmailStorageStatus{
		MaximumRawBytes: status.MaximumRawBytes,
		AttachmentCapacity: server.MemoryLimitStatus{
			Used:      status.AttachmentCapacity.Used,
			Max:       status.AttachmentCapacity.Max,
			Remaining: status.AttachmentCapacity.Remaining,
			Unlimited: status.AttachmentCapacity.Unlimited,
			NearLimit: status.AttachmentCapacity.NearLimit,
			AtLimit:   status.AttachmentCapacity.AtLimit,
			OverLimit: status.AttachmentCapacity.OverLimit,
		},
	}
}

func toServerAgentEmailProcessing(processing store.AgentEmailProcessing) server.AgentEmailProcessing {
	return server.AgentEmailProcessing{
		State: processing.State, Generation: processing.Generation,
		FailureCount: processing.FailureCount, ClaimID: processing.ClaimID,
		LeaseExpiresAt: processing.LeaseExpiresAt, CompletedAt: processing.CompletedAt,
	}
}

func toServerAgentEmailRetryCanaryCheckpoint(checkpoint store.AgentEmailRetryCanaryCheckpoint) server.AgentEmailRetryCanaryCheckpoint {
	return server.AgentEmailRetryCanaryCheckpoint{
		State: checkpoint.State, Armed: checkpoint.Armed,
		Tempfailed: checkpoint.Tempfailed, Accepted: checkpoint.Accepted,
		TempfailCount: checkpoint.TempfailCount,
	}
}
