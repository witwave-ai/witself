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
	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

const (
	agentEmailPilotEnabledEnv      = "WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED"
	agentEmailPilotDomainEnv       = "WITSELF_AGENT_EMAIL_PILOT_DOMAIN"
	agentEmailPilotAudienceEnv     = "WITSELF_AGENT_EMAIL_PILOT_AUDIENCE"
	agentEmailPilotRealmIDEnv      = "WITSELF_AGENT_EMAIL_PILOT_REALM_ID"
	agentEmailPilotAgentIDsEnv     = "WITSELF_AGENT_EMAIL_PILOT_AGENT_IDS"
	agentEmailRelayPublicKeysEnv   = "WITSELF_AGENT_EMAIL_RELAY_PUBLIC_KEYS_JSON"
	agentEmailRelayReplayWindowEnv = "WITSELF_AGENT_EMAIL_RELAY_REPLAY_WINDOW"
	defaultAgentEmailReplayWindow  = 5 * time.Minute
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
		Enabled: true, Domain: domain, Audience: audience,
		RealmIDs: map[string]bool{realmID: true}, AgentIDs: agentIDs,
		RelayPublicKeys: publicKeys, RelayReplayWindow: replayWindow,
	}
	if err := server.ValidateAgentEmailPilotConfig(pilot); err != nil {
		return server.AgentEmailPilotConfig{}, err
	}
	return pilot, nil
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
	if !pilot.Enabled {
		return nil
	}
	scope := store.AgentEmailPilotScope{
		Enabled: true, Domain: pilot.Domain, Audience: pilot.Audience,
		RealmIDs: cloneAgentEmailBoolMap(pilot.RealmIDs),
		AgentIDs: cloneAgentEmailBoolMap(pilot.AgentIDs),
	}
	if _, err := st.ReconcileAgentEmailPilot(ctx, scope); err != nil {
		return fmt.Errorf("agent-email pilot startup reconciliation: %w", err)
	}
	cfg.IngestAgentEmailPilot = func(ctx context.Context, relay agentemail.RelayMetadata, raw []byte) error {
		_, err := st.IngestAgentEmailPilot(ctx, scope, store.AgentEmailIngestInput{Relay: relay, Raw: raw})
		return mapAgentEmailIngestError(err)
	}
	cfg.GetAgentEmailAddress = func(ctx context.Context, p server.DomainPrincipal) (server.AgentEmailAddress, error) {
		address, err := st.GetAgentEmailAddress(ctx, scope, toStorePrincipal(p))
		if err != nil {
			return server.AgentEmailAddress{}, mapAgentEmailError(err)
		}
		return toServerAgentEmailAddress(address), nil
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
		return server.AgentEmailCheckpoint{
			Pending: checkpoint.Pending, MailboxPending: checkpoint.MailboxPending,
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

func cloneAgentEmailBoolMap(source map[string]bool) map[string]bool {
	result := make(map[string]bool, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func mapAgentEmailIngestError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrAgentEmailUnknownRecipient),
		errors.Is(err, store.ErrAgentEmailPilotNotEnrolled),
		errors.Is(err, store.ErrAgentEmailAddressMissing):
		return server.ErrAgentEmailUnknownRecipient
	case errors.Is(err, store.ErrAgentEmailReceiveDisabled):
		return server.ErrAgentEmailReceiveDisabled
	case errors.Is(err, store.ErrAgentEmailPilotDisabled):
		return server.ErrAgentEmailPilotUnavailable
	case errors.Is(err, store.ErrAgentEmailInputInvalid):
		return wrapAsSentinel(server.ErrBadInput, store.ErrAgentEmailInputInvalid, err)
	default:
		return err
	}
}

func mapAgentEmailError(err error) error {
	switch {
	case err == nil:
		return nil
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

func toServerAgentEmailAddress(address store.AgentEmailAddress) server.AgentEmailAddress {
	return server.AgentEmailAddress{
		ID: address.ID, MailboxID: address.MailboxID, AccountID: address.AccountID,
		RealmID: address.RealmID, OwnerAgentID: address.OwnerAgentID,
		Address: address.Address, Domain: address.Domain, LocalPart: address.LocalPart,
		AgentSegment: address.AgentSegment, RealmLabel: address.RealmLabel,
		ProvisioningKind: address.ProvisioningKind, ReceiveState: address.ReceiveState,
		CreatedAt: address.CreatedAt, UpdatedAt: address.UpdatedAt,
		DisabledAt: address.DisabledAt, RetiredAt: address.RetiredAt,
	}
}

func toServerAgentEmailMessage(message store.AgentEmailMessage) server.AgentEmailMessage {
	return server.AgentEmailMessage{
		ID: message.ID, AccountID: message.AccountID, RealmID: message.RealmID,
		MailboxID: message.MailboxID, OwnerAgentID: message.OwnerAgentID, AddressID: message.AddressID,
		Provider: message.Provider, EnvelopeSender: message.EnvelopeSender,
		EnvelopeRecipient: message.EnvelopeRecipient, AgentSegment: message.AgentSegment,
		RealmLabel: message.RealmLabel, SubaddressTag: message.SubaddressTag,
		RawSizeBytes: message.RawSizeBytes, ParseState: message.ParseState,
		ParseErrorCode: message.ParseErrorCode, HeaderFrom: message.HeaderFrom,
		HeaderTo: message.HeaderTo, Subject: message.Subject, MIMEMessageID: message.MIMEMessageID,
		MessageDate: message.MessageDate, AttachmentCount: message.AttachmentCount,
		SPFResult: message.SPFResult, DKIMResult: message.DKIMResult,
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

func toServerAgentEmailProcessing(processing store.AgentEmailProcessing) server.AgentEmailProcessing {
	return server.AgentEmailProcessing{
		State: processing.State, Generation: processing.Generation,
		FailureCount: processing.FailureCount, ClaimID: processing.ClaimID,
		LeaseExpiresAt: processing.LeaseExpiresAt, CompletedAt: processing.CompletedAt,
	}
}
