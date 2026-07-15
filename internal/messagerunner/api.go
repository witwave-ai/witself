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

// MessageRequestAPI is an optional autonomous open-request surface. Runner
// implementations discover it with a type assertion so existing mailbox-only
// API adapters remain source compatible.
type MessageRequestAPI interface {
	ListMessageRequests(context.Context, client.MessageRequestListOptions) (client.MessageRequestPage, error)
	GetMessageRequest(context.Context, string) (client.MessageRequestDetail, error)
	OfferMessageRequest(context.Context, string, client.OfferMessageRequestInput) (client.OfferMessageRequestResult, error)
	DeclineMessageRequest(context.Context, string, string) (client.MessageRequest, error)
	SelectMessageRequest(context.Context, string, client.SelectMessageRequestInput) (client.SelectMessageRequestResult, error)
	ClaimMessageRequest(context.Context, string, client.ClaimMessageRequestInput) (client.MessageRequestClaim, error)
	RenewMessageRequest(context.Context, string, client.RenewMessageRequestInput) (client.MessageRequestClaim, error)
	ReleaseMessageRequest(context.Context, string, client.ReleaseMessageRequestInput) (client.MessageRequestClaim, error)
	CompleteMessageRequest(context.Context, string, client.CompleteMessageRequestInput) (client.CompleteMessageRequestResult, error)
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

// ListMessageRequests returns one metadata-only page of request work visible
// to the pinned agent.
func (a HTTPAPI) ListMessageRequests(ctx context.Context, opts client.MessageRequestListOptions) (client.MessageRequestPage, error) {
	return client.ListMessageRequests(ctx, a.Endpoint, a.Token, opts)
}

// GetMessageRequest returns the request detail authorized for the pinned
// agent.
func (a HTTPAPI) GetMessageRequest(ctx context.Context, requestID string) (client.MessageRequestDetail, error) {
	return client.GetMessageRequest(ctx, a.Endpoint, a.Token, requestID)
}

// OfferMessageRequest records one inference-authored candidate offer.
func (a HTTPAPI) OfferMessageRequest(ctx context.Context, requestID string, in client.OfferMessageRequestInput) (client.OfferMessageRequestResult, error) {
	return client.OfferMessageRequest(ctx, a.Endpoint, a.Token, requestID, in)
}

// DeclineMessageRequest records a candidate's terminal non-participation.
func (a HTTPAPI) DeclineMessageRequest(ctx context.Context, requestID, idempotencyKey string) (client.MessageRequest, error) {
	return client.DeclineMessageRequest(ctx, a.Endpoint, a.Token, requestID, idempotencyKey)
}

// SelectMessageRequest persists the coordinator provider's bounded choice.
func (a HTTPAPI) SelectMessageRequest(ctx context.Context, requestID string, in client.SelectMessageRequestInput) (client.SelectMessageRequestResult, error) {
	return client.SelectMessageRequest(ctx, a.Endpoint, a.Token, requestID, in)
}

// ClaimMessageRequest acquires one selected request reservation.
func (a HTTPAPI) ClaimMessageRequest(ctx context.Context, requestID string, in client.ClaimMessageRequestInput) (client.MessageRequestClaim, error) {
	return client.ClaimMessageRequest(ctx, a.Endpoint, a.Token, requestID, in)
}

// RenewMessageRequest extends one exact selected-request claim.
func (a HTTPAPI) RenewMessageRequest(ctx context.Context, requestID string, in client.RenewMessageRequestInput) (client.MessageRequestClaim, error) {
	return client.RenewMessageRequest(ctx, a.Endpoint, a.Token, requestID, in)
}

// ReleaseMessageRequest gives up one exact selected-request claim.
func (a HTTPAPI) ReleaseMessageRequest(ctx context.Context, requestID string, in client.ReleaseMessageRequestInput) (client.MessageRequestClaim, error) {
	return client.ReleaseMessageRequest(ctx, a.Endpoint, a.Token, requestID, in)
}

// CompleteMessageRequest publishes a result and closes one selected request
// claim atomically.
func (a HTTPAPI) CompleteMessageRequest(ctx context.Context, requestID string, in client.CompleteMessageRequestInput) (client.CompleteMessageRequestResult, error) {
	return client.CompleteMessageRequest(ctx, a.Endpoint, a.Token, requestID, in)
}
