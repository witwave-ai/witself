package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/agentemail"
	"github.com/witwave-ai/witself/internal/plans"
	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

const (
	agentEmailPilotEnabledEnv       = "WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED"
	agentEmailPilotDomainEnv        = "WITSELF_AGENT_EMAIL_PILOT_DOMAIN"
	agentEmailLegacyDomainsEnv      = "WITSELF_AGENT_EMAIL_ACCEPTED_LEGACY_DOMAINS"
	agentEmailPilotAudienceEnv      = "WITSELF_AGENT_EMAIL_PILOT_AUDIENCE"
	agentEmailPilotRealmIDEnv       = "WITSELF_AGENT_EMAIL_PILOT_REALM_ID"
	agentEmailPilotAgentIDsEnv      = "WITSELF_AGENT_EMAIL_PILOT_AGENT_IDS"
	agentEmailRelayPublicKeysEnv    = "WITSELF_AGENT_EMAIL_RELAY_PUBLIC_KEYS_JSON"
	agentEmailRelayReplayWindowEnv  = "WITSELF_AGENT_EMAIL_RELAY_REPLAY_WINDOW"
	agentEmailRetryCanaryAgentIDEnv = "WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID"
	defaultAgentEmailReplayWindow   = 5 * time.Minute
)

// agentEmailPilotConfigFromEnv parses all pilot trust and enrollment material
// before listeners start. The zero-value result is intentionally disabled.
func agentEmailPilotConfigFromEnv() (server.AgentEmailPilotConfig, error) {
	rawEnabled, ok := os.LookupEnv(agentEmailPilotEnabledEnv)
	if !ok {
		return server.AgentEmailPilotConfig{}, nil
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(rawEnabled))
	if err != nil {
		return server.AgentEmailPilotConfig{}, fmt.Errorf("%s must be a boolean: %w", agentEmailPilotEnabledEnv, err)
	}
	if !enabled {
		return server.AgentEmailPilotConfig{}, nil
	}
	require := func(name string) (string, error) {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			return "", fmt.Errorf("%s is required when %s=true", name, agentEmailPilotEnabledEnv)
		}
		return value, nil
	}
	domain, err := require(agentEmailPilotDomainEnv)
	if err != nil {
		return server.AgentEmailPilotConfig{}, err
	}
	legacyDomains, err := parseAgentEmailLegacyDomains(os.Getenv(agentEmailLegacyDomainsEnv))
	if err != nil {
		return server.AgentEmailPilotConfig{}, fmt.Errorf("%s: %w", agentEmailLegacyDomainsEnv, err)
	}
	audience, err := require(agentEmailPilotAudienceEnv)
	if err != nil {
		return server.AgentEmailPilotConfig{}, err
	}
	realmID, err := require(agentEmailPilotRealmIDEnv)
	if err != nil {
		return server.AgentEmailPilotConfig{}, err
	}
	agentIDsText, err := require(agentEmailPilotAgentIDsEnv)
	if err != nil {
		return server.AgentEmailPilotConfig{}, err
	}
	agentIDs, err := parseAgentEmailIDSet(agentIDsText)
	if err != nil {
		return server.AgentEmailPilotConfig{}, fmt.Errorf("%s: %w", agentEmailPilotAgentIDsEnv, err)
	}
	encodedKeys, err := require(agentEmailRelayPublicKeysEnv)
	if err != nil {
		return server.AgentEmailPilotConfig{}, err
	}
	var keyValues map[string]string
	decoder := json.NewDecoder(strings.NewReader(encodedKeys))
	if err := decoder.Decode(&keyValues); err != nil {
		return server.AgentEmailPilotConfig{}, fmt.Errorf("%s must be a JSON object: %w", agentEmailRelayPublicKeysEnv, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return server.AgentEmailPilotConfig{}, fmt.Errorf("%s must contain one JSON value", agentEmailRelayPublicKeysEnv)
	}
	publicKeys := make(map[string]ed25519.PublicKey, len(keyValues))
	for keyID, encoded := range keyValues {
		key, err := agentemail.ParsePublicKey(encoded)
		if err != nil {
			return server.AgentEmailPilotConfig{}, fmt.Errorf("%s key %q is invalid: %w", agentEmailRelayPublicKeysEnv, keyID, err)
		}
		publicKeys[keyID] = key
	}
	replayWindow := defaultAgentEmailReplayWindow
	if value := strings.TrimSpace(os.Getenv(agentEmailRelayReplayWindowEnv)); value != "" {
		replayWindow, err = time.ParseDuration(value)
		if err != nil {
			return server.AgentEmailPilotConfig{}, fmt.Errorf("%s must be a duration: %w", agentEmailRelayReplayWindowEnv, err)
		}
	}
	pilot := server.AgentEmailPilotConfig{
		Enabled: true, Domain: domain, LegacyDomains: legacyDomains, Audience: audience,
		RealmIDs: map[string]bool{realmID: true}, AgentIDs: agentIDs,
		RetryCanaryAgentID: strings.TrimSpace(os.Getenv(agentEmailRetryCanaryAgentIDEnv)),
		RelayPublicKeys:    publicKeys, RelayReplayWindow: replayWindow,
	}
	if err := server.ValidateAgentEmailPilotConfig(pilot); err != nil {
		return server.AgentEmailPilotConfig{}, err
	}
	return pilot, nil
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
	result := make(map[string]bool)
	for _, raw := range strings.Split(value, ",") {
		id := strings.TrimSpace(raw)
		if id == "" {
			return nil, errors.New("agent ids must be a comma-separated non-empty set")
		}
		if result[id] {
			return nil, fmt.Errorf("agent id %q is duplicated", id)
		}
		result[id] = true
	}
	return result, nil
}

func configureAgentEmail(ctx context.Context, cfg *server.Config, st *store.Store, pilot server.AgentEmailPilotConfig) error {
	cfg.AgentEmailPilot = pilot
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
		if pilot.Enabled && !agentEmailRealmAliasProjectionDomainAllowed(pilot, in) {
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
	if !pilot.Enabled {
		return nil
	}
	scope := store.AgentEmailPilotScope{
		Enabled: true, Domain: pilot.Domain,
		LegacyDomains:      append([]string(nil), pilot.LegacyDomains...),
		Audience:           pilot.Audience,
		RealmIDs:           cloneAgentEmailBoolMap(pilot.RealmIDs),
		AgentIDs:           cloneAgentEmailBoolMap(pilot.AgentIDs),
		RetryCanaryAgentID: pilot.RetryCanaryAgentID,
	}
	if _, err := st.ReconcileAgentEmailPilot(ctx, scope); err != nil {
		return fmt.Errorf("agent-email pilot startup reconciliation: %w", err)
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
