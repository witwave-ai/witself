// Package messagerunner implements client-owned autonomous handling of durable
// Witself messages without placing inference or provider credentials in the
// backend.
package messagerunner

import (
	"context"

	"github.com/witwave-ai/witself/internal/client"
)

// API is the trusted parent runner's narrow Witself surface. Provider children
// never receive this interface or the bearer token behind it.
type API interface {
	GetSelf(context.Context) (client.SelfDigest, error)
	ListenMessages(context.Context, client.MessageListenOptions) (client.MessageListenResult, error)
	ClaimMessage(context.Context, string, client.ClaimMessageInput) (client.MessageProcessing, error)
	RenewMessageClaim(context.Context, string, client.RenewMessageClaimInput) (client.MessageProcessing, error)
	ReleaseMessageClaim(context.Context, string, client.MessageClaimInput) (client.MessageProcessing, error)
	ReadMessage(context.Context, string) (client.Message, error)
	CompleteMessage(context.Context, string, client.CompleteMessageInput) (client.CompleteMessageResult, error)
	AckMessage(context.Context, string) (client.Message, error)
}

// HTTPAPI adapts the ordinary Witself client to the runner's token-retaining
// parent boundary.
type HTTPAPI struct {
	Endpoint string
	Token    string
}

// GetSelf resolves the bearer token's immutable agent identity.
func (a HTTPAPI) GetSelf(ctx context.Context) (client.SelfDigest, error) {
	return client.GetSelf(ctx, a.Endpoint, a.Token, client.SelfOptions{})
}

// ListenMessages performs one bounded metadata-only mailbox wait.
func (a HTTPAPI) ListenMessages(ctx context.Context, opts client.MessageListenOptions) (client.MessageListenResult, error) {
	return client.ListenMessages(ctx, a.Endpoint, a.Token, opts)
}

// ClaimMessage acquires or idempotently resumes one processing lease.
func (a HTTPAPI) ClaimMessage(ctx context.Context, messageID string, in client.ClaimMessageInput) (client.MessageProcessing, error) {
	return client.ClaimMessage(ctx, a.Endpoint, a.Token, messageID, in)
}

// RenewMessageClaim extends one exact processing generation.
func (a HTTPAPI) RenewMessageClaim(ctx context.Context, messageID string, in client.RenewMessageClaimInput) (client.MessageProcessing, error) {
	return client.RenewMessageClaim(ctx, a.Endpoint, a.Token, messageID, in)
}

// ReleaseMessageClaim gives up one exact processing generation.
func (a HTTPAPI) ReleaseMessageClaim(ctx context.Context, messageID string, in client.MessageClaimInput) (client.MessageProcessing, error) {
	return client.ReleaseMessageClaim(ctx, a.Endpoint, a.Token, messageID, in)
}

// ReadMessage crosses the explicit message-content boundary for a recipient.
func (a HTTPAPI) ReadMessage(ctx context.Context, messageID string) (client.Message, error) {
	return client.ReadMessage(ctx, a.Endpoint, a.Token, messageID)
}

// CompleteMessage atomically publishes a result and closes a live claim.
func (a HTTPAPI) CompleteMessage(ctx context.Context, messageID string, in client.CompleteMessageInput) (client.CompleteMessageResult, error) {
	return client.CompleteMessage(ctx, a.Endpoint, a.Token, messageID, in)
}

// AckMessage acknowledges a successfully handled or non-actionable delivery.
func (a HTTPAPI) AckMessage(ctx context.Context, messageID string) (client.Message, error) {
	return client.AckMessage(ctx, a.Endpoint, a.Token, messageID)
}
