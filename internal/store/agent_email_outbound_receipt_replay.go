package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// AgentEmailOutboundReceiptReplayTTL matches the managed edge receipt's
	// bounded idempotency lifetime. Operators cannot extend it at invocation.
	AgentEmailOutboundReceiptReplayTTL = 7 * 24 * time.Hour
	// AgentEmailOutboundCloudflareProvider is the normalized managed provider
	// recorded by both live dispatch settlement and receipt proof validation.
	AgentEmailOutboundCloudflareProvider = "cloudflare_email_sending"
)

var (
	// ErrAgentEmailOutboundReceiptReplayRefused reports that the immutable local
	// acceptance does not satisfy every operator-supplied replay assertion.
	ErrAgentEmailOutboundReceiptReplayRefused = errors.New(
		"outbound agent-email receipt replay refused",
	)
)

// AgentEmailOutboundReceiptReplayInput binds an operator proof request to one
// exact local acceptance. ExpectedAttemptCount is deliberately fixed at one;
// this command is proof for the initial production canary, not a general retry
// surface.
type AgentEmailOutboundReceiptReplayInput struct {
	AccountID            string
	SendID               string
	ExpectedAcceptedAt   time.Time
	ExpectedAttemptCount int64
}

// LoadAgentEmailOutboundReceiptReplay reconstructs the immutable dispatch only
// after its accepted local record satisfies every explicit operator assertion.
// The transaction is repeatable-read and database-enforced read-only. It never
// claims, retries, updates, migrates, or otherwise changes cell state.
func (s *Store) LoadAgentEmailOutboundReceiptReplay(
	ctx context.Context,
	in AgentEmailOutboundReceiptReplayInput,
) (AgentEmailOutboundDispatch, error) {
	if in.AccountID != strings.TrimSpace(in.AccountID) ||
		in.SendID != strings.TrimSpace(in.SendID) ||
		!validAgentEmailGeneratedID(in.AccountID, "acc") ||
		!validAgentEmailGeneratedID(in.SendID, "esnd") ||
		in.ExpectedAcceptedAt.IsZero() || in.ExpectedAttemptCount != 1 {
		return AgentEmailOutboundDispatch{}, fmt.Errorf(
			"%w: expected assertions are invalid",
			ErrAgentEmailOutboundReceiptReplayRefused,
		)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return AgentEmailOutboundDispatch{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var databaseNow time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseNow); err != nil {
		return AgentEmailOutboundDispatch{}, fmt.Errorf(
			"read outbound receipt replay clock: %w", err,
		)
	}
	msg, err := scanAgentEmailOutbound(tx.QueryRow(
		ctx,
		agentEmailOutboundSelect()+" WHERE id=$1 AND account_id=$2",
		in.SendID,
		in.AccountID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailOutboundDispatch{}, ErrAgentEmailOutboundReceiptReplayRefused
	}
	if err != nil {
		return AgentEmailOutboundDispatch{}, fmt.Errorf(
			"read outbound receipt replay source: %w", err,
		)
	}
	if err := validateAgentEmailOutboundReceiptReplay(
		msg, in, databaseNow.UTC(),
	); err != nil {
		return AgentEmailOutboundDispatch{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailOutboundDispatch{}, err
	}
	dispatch := dispatchFromAgentEmailOutbound(msg)
	// A receipt proof is read-only and needs no worker claim capability.
	dispatch.Claim = AgentEmailOutboundClaimFence{}
	return dispatch, nil
}

func validateAgentEmailOutboundReceiptReplay(
	msg AgentEmailOutboundMessage,
	in AgentEmailOutboundReceiptReplayInput,
	now time.Time,
) error {
	if msg.ID != in.SendID || msg.AccountID != in.AccountID ||
		msg.State != AgentEmailOutboundAccepted ||
		msg.ProviderState != AgentEmailOutboundAccepted ||
		msg.Provider != AgentEmailOutboundCloudflareProvider ||
		strings.TrimSpace(msg.ProviderMessageID) == "" ||
		msg.AcceptedAt == nil ||
		!msg.AcceptedAt.Equal(in.ExpectedAcceptedAt) ||
		msg.AttemptCount != in.ExpectedAttemptCount {
		return ErrAgentEmailOutboundReceiptReplayRefused
	}
	age := now.Sub(msg.AcceptedAt.UTC())
	if age < 0 || age > AgentEmailOutboundReceiptReplayTTL {
		return ErrAgentEmailOutboundReceiptReplayRefused
	}
	return nil
}
