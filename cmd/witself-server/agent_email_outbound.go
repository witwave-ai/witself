package main

import (
	"context"
	"errors"

	"github.com/witwave-ai/witself/internal/plans"
	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

// configureAgentEmailOutbound is intentionally independent from the
// process-local receive rollout. Plan and operator policy are resolved by the
// store on every request, so an account transition needs no client or MCP
// reinstall and no server restart.
func configureAgentEmailOutbound(cfg *server.Config, st *store.Store) {
	cfg.ApplyAgentEmailOutboundProviderEvent = func(ctx context.Context, event server.AgentEmailOutboundProviderEvent) error {
		_, err := st.ApplyAgentEmailOutboundProviderEvent(ctx, store.AgentEmailOutboundProviderEventInput{
			Provider: store.AgentEmailOutboundCloudflareProvider, EventID: event.EventID,
			ProviderMessageID: event.ProviderMessageID,
			EventClass:        event.EventClass, OccurredAt: event.OccurredAt,
		})
		return mapAgentEmailOutboundProviderEventError(err)
	}
	cfg.RequireAgentEmailSendEntitlement = func(ctx context.Context, p server.DomainPrincipal) error {
		return mapAgentEmailOutboundError(st.RequireAgentEmailSendEnabled(ctx, toStorePrincipal(p)))
	}
	cfg.QueueAgentEmail = func(ctx context.Context, p server.DomainPrincipal, in server.SendAgentEmailRequest, idempotencyKey string) (server.AgentEmailOutboundMessage, error) {
		message, err := st.QueueAgentEmail(ctx, toStorePrincipal(p), store.SendAgentEmailInput{
			To: in.To, Subject: in.Subject, Text: in.Text, IdempotencyKey: idempotencyKey,
		})
		return toServerAgentEmailOutboundMessage(message), mapAgentEmailOutboundError(err)
	}
	cfg.ReplyAgentEmail = func(ctx context.Context, p server.DomainPrincipal, inboundMessageID string, in server.ReplyAgentEmailRequest, idempotencyKey string) (server.AgentEmailOutboundMessage, error) {
		message, err := st.ReplyAgentEmail(ctx, toStorePrincipal(p), inboundMessageID, store.ReplyAgentEmailInput{
			Text: in.Text, IdempotencyKey: idempotencyKey,
		})
		return toServerAgentEmailOutboundMessage(message), mapAgentEmailOutboundError(err)
	}
	cfg.ListAgentEmailOutbox = func(ctx context.Context, p server.DomainPrincipal, opts server.AgentEmailOutboundListOptions) (server.AgentEmailOutboundPage, error) {
		page, err := st.ListAgentEmailOutbox(ctx, toStorePrincipal(p), store.AgentEmailOutboundFilter{
			State: opts.State, Limit: opts.Limit, Cursor: opts.Cursor,
		})
		if err != nil {
			return server.AgentEmailOutboundPage{}, mapAgentEmailOutboundError(err)
		}
		messages := make([]server.AgentEmailOutboundMessage, len(page.Messages))
		for i, message := range page.Messages {
			messages[i] = toServerAgentEmailOutboundMessage(message)
		}
		return server.AgentEmailOutboundPage{Messages: messages, NextCursor: page.NextCursor}, nil
	}
	cfg.GetAgentEmailOutbound = func(ctx context.Context, p server.DomainPrincipal, messageID string) (server.AgentEmailOutboundMessage, error) {
		message, err := st.GetAgentEmailOutbound(ctx, toStorePrincipal(p), messageID)
		return toServerAgentEmailOutboundMessage(message), mapAgentEmailOutboundError(err)
	}
	cfg.GetAgentEmailSendControl = func(ctx context.Context, accountID, operatorID, agentID string) (server.AgentEmailSendControl, error) {
		control, err := st.GetAgentEmailSendControl(ctx, accountID, operatorID, agentID)
		return toServerAgentEmailSendControl(control), mapAgentEmailOutboundControlError(err)
	}
	cfg.SetAgentEmailSendControl = func(ctx context.Context, accountID, operatorID, agentID, state string, expectedRowVersion int64) (server.AgentEmailSendControl, error) {
		control, err := st.SetAgentEmailSendControl(ctx, accountID, operatorID, agentID, state, expectedRowVersion)
		return toServerAgentEmailSendControl(control), mapAgentEmailOutboundControlError(err)
	}
	cfg.GetRealmEmailSendControl = func(ctx context.Context, accountID, operatorID, realmID string) (server.AgentEmailRealmSendControl, error) {
		control, err := st.GetAgentEmailRealmSendControl(ctx, accountID, operatorID, realmID)
		return toServerAgentEmailRealmSendControl(control), mapAgentEmailOutboundControlError(err)
	}
	cfg.SetRealmEmailSendControl = func(ctx context.Context, accountID, operatorID, realmID, state string, expectedRowVersion int64) (server.AgentEmailRealmSendControl, error) {
		control, err := st.SetAgentEmailRealmSendControl(ctx, accountID, operatorID, realmID, state, expectedRowVersion)
		return toServerAgentEmailRealmSendControl(control), mapAgentEmailOutboundControlError(err)
	}
}

func mapAgentEmailOutboundProviderEventError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrAgentEmailOutboundInputInvalid):
		return server.ErrBadInput
	case errors.Is(err, store.ErrAgentEmailOutboundNotFound):
		return server.ErrNotFound
	case errors.Is(err, store.ErrAgentEmailOutboundConflict):
		return server.ErrConflict
	default:
		return err
	}
}

func mapAgentEmailOutboundControlError(err error) error {
	if errors.Is(err, store.ErrAgentEmailOutboundConflict) {
		return server.ErrConflict
	}
	return mapAgentEmailOutboundError(err)
}

func mapAgentEmailOutboundError(err error) error {
	var featureErr *store.FeatureNotEnabledError
	var rateErr *store.AgentEmailOutboundRateLimitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &featureErr):
		return &server.FeatureNotEnabledError{Feature: featureErr.Feature}
	case errors.Is(err, store.ErrAgentEmailSendDisabled):
		return &server.FeatureNotEnabledError{Feature: plans.AgentEmailSendFeature}
	case errors.As(err, &rateErr) && rateErr != nil:
		return &server.AgentEmailOutboundRateLimitError{
			Scope: rateErr.Scope, Limit: rateErr.Limit, Used: rateErr.Used,
			WindowSeconds: rateErr.WindowSeconds, RetryAfter: rateErr.RetryAfter,
			ResetAt: rateErr.ResetAt, Source: rateErr.Source, Retryable: rateErr.Retryable,
		}
	case errors.Is(err, store.ErrAgentEmailOutboundInputInvalid),
		errors.Is(err, store.ErrAgentEmailOutboundCursorInvalid):
		return server.ErrBadInput
	case errors.Is(err, store.ErrAgentEmailOutboundNotFound):
		return server.ErrNotFound
	case errors.Is(err, store.ErrAgentEmailOutboundForbidden),
		errors.Is(err, store.ErrAgentNotFound), errors.Is(err, store.ErrAccountNotActive):
		return server.ErrForbidden
	case errors.Is(err, store.ErrAgentEmailOutboundConflict):
		return server.ErrIdempotencyConflict
	case errors.Is(err, store.ErrAgentEmailSenderUnavailable),
		errors.Is(err, store.ErrAgentEmailReplyUnavailable),
		errors.Is(err, store.ErrAgentEmailRecipientSuppressed):
		return server.ErrConflict
	case errors.Is(err, store.ErrAccountNotFound):
		return server.ErrNotFound
	default:
		return err
	}
}

func toServerAgentEmailOutboundMessage(message store.AgentEmailOutboundMessage) server.AgentEmailOutboundMessage {
	return server.AgentEmailOutboundMessage{
		ID: message.ID, AccountID: message.AccountID, RealmID: message.RealmID,
		OwnerAgentID: message.OwnerAgentID, From: message.FromAddress,
		ReplyTo: message.ReplyToAddress, To: message.ToAddress, Subject: message.Subject,
		State: message.State, ProviderState: message.ProviderState, Provider: message.Provider,
		ErrorCode: message.LastErrorCode, RequestKind: message.RequestKind,
		ReplyToInboundMessageID: message.ReplyToInboundMessageID,
		ThreadKey:               message.ThreadKey, AttemptCount: message.AttemptCount,
		QueuedAt: message.QueuedAt, CreatedAt: message.CreatedAt, UpdatedAt: message.UpdatedAt,
		ProviderStartedAt: message.ProviderStartedAt, AcceptedAt: message.AcceptedAt,
		DeliveredAt: message.DeliveredAt, DeferredAt: message.DeferredAt,
		FailedAt: message.FailedAt, AmbiguousAt: message.AmbiguousAt, CanceledAt: message.CanceledAt,
	}
}

func toServerAgentEmailSendControl(control store.AgentEmailSendControl) server.AgentEmailSendControl {
	return server.AgentEmailSendControl{
		AccountID: control.AccountID, RealmID: control.RealmID, AgentID: control.AgentID,
		SendState: control.SendState, AgentSendState: control.AgentSendState,
		RealmSendState: control.RealmSendState, RowVersion: control.RowVersion,
		RealmRowVersion: control.RealmRowVersion, UpdatedAt: control.UpdatedAt,
		DisabledAt: control.DisabledAt, RealmDisabledAt: control.RealmDisabledAt,
	}
}

func toServerAgentEmailRealmSendControl(control store.AgentEmailRealmSendControl) server.AgentEmailRealmSendControl {
	return server.AgentEmailRealmSendControl{
		AccountID: control.AccountID, RealmID: control.RealmID,
		SendState: control.SendState, AgentCount: control.AgentCount,
		RowVersion: control.RowVersion, UpdatedAt: control.UpdatedAt,
		DisabledAt: control.DisabledAt,
	}
}
