package client

import (
	"context"
	"encoding/json"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"
)

// MessageRequest is the durable coordination state for one realm-wide job.
type MessageRequest struct {
	ID                  string       `json:"id"`
	AccountID           string       `json:"account_id"`
	RealmID             string       `json:"realm_id"`
	OpeningMessageID    string       `json:"opening_message_id"`
	Coordinator         MessageAgent `json:"coordinator"`
	SelectionPolicy     string       `json:"selection_policy"`
	State               string       `json:"state"`
	Phase               string       `json:"phase,omitempty"`
	MaxAssignees        int          `json:"max_assignees"`
	CandidateCount      int          `json:"candidate_count"`
	OfferCount          int          `json:"offer_count"`
	DeclineCount        int          `json:"decline_count"`
	SelectedAgentIDs    []string     `json:"selected_agent_ids"`
	SelectionGeneration int64        `json:"selection_generation"`
	OfferDeadline       time.Time    `json:"offer_deadline"`
	ExpiresAt           time.Time    `json:"expires_at"`
	CreatedAt           time.Time    `json:"created_at"`
	UpdatedAt           time.Time    `json:"updated_at"`
	CompletedAt         *time.Time   `json:"completed_at,omitempty"`
	CancelledAt         *time.Time   `json:"cancelled_at,omitempty"`
	ExpiredAt           *time.Time   `json:"expired_at,omitempty"`
}

// MessageRequestCandidate is one agent in a request's immutable candidate snapshot.
type MessageRequestCandidate struct {
	Agent          MessageAgent `json:"agent"`
	ResponseState  string       `json:"response_state"`
	OfferMessageID string       `json:"offer_message_id,omitempty"`
	RespondedAt    *time.Time   `json:"responded_at,omitempty"`
	CreatedAt      time.Time    `json:"created_at"`
}

// MessageRequestOffer is one candidate's durable offer message.
type MessageRequestOffer struct {
	Agent     MessageAgent `json:"agent"`
	Message   Message      `json:"message"`
	OfferedAt time.Time    `json:"offered_at"`
}

// MessageRequestSelection is one immutable coordinator selection decision.
type MessageRequestSelection struct {
	ID               string       `json:"id"`
	Generation       int64        `json:"generation"`
	Coordinator      MessageAgent `json:"coordinator"`
	SelectedAgentIDs []string     `json:"selected_agent_ids"`
	CreatedAt        time.Time    `json:"created_at"`
}

// MessageRequestClaim is one selected agent's fenced work slot.
type MessageRequestClaim struct {
	ClaimID         string       `json:"claim_id"`
	RequestID       string       `json:"request_id"`
	SelectionID     string       `json:"selection_id"`
	Agent           MessageAgent `json:"agent"`
	State           string       `json:"state"`
	Generation      int64        `json:"generation"`
	FailureCount    int64        `json:"failure_count"`
	LeaseExpiresAt  *time.Time   `json:"lease_expires_at,omitempty"`
	ResultMessageID string       `json:"result_message_id,omitempty"`
	SelectedAt      time.Time    `json:"selected_at"`
	ClaimedAt       *time.Time   `json:"claimed_at,omitempty"`
	ReleasedAt      *time.Time   `json:"released_at,omitempty"`
	CompletedAt     *time.Time   `json:"completed_at,omitempty"`
	CancelledAt     *time.Time   `json:"cancelled_at,omitempty"`
	UpdatedAt       time.Time    `json:"updated_at"`
}

// MessageRequestDetail is the authorized request graph visible to the caller.
type MessageRequestDetail struct {
	Request        MessageRequest            `json:"request"`
	OpeningMessage Message                   `json:"opening_message"`
	Candidates     []MessageRequestCandidate `json:"candidates"`
	Offers         []MessageRequestOffer     `json:"offers"`
	Selections     []MessageRequestSelection `json:"selections"`
	Claims         []MessageRequestClaim     `json:"claims"`
}

// CreateMessageRequestInput carries one realm-wide request and its timing bounds.
type CreateMessageRequestInput struct {
	Subject            string
	Body               string
	Payload            json.RawMessage
	SelectionPolicy    string
	MaxAssignees       int
	OfferWindowSeconds int
	ExpiresInSeconds   int
	IdempotencyKey     string
}

// CreateMessageRequestResult contains the request and its opening message.
type CreateMessageRequestResult struct {
	Request        MessageRequest `json:"request"`
	OpeningMessage Message        `json:"opening_message"`
}

// MessageRequestListOptions selects one page of visible requests.
type MessageRequestListOptions struct {
	State  string
	Phase  string
	Role   string
	Limit  int
	Cursor string
}

// MessageRequestPage is one page of visible request summaries.
type MessageRequestPage struct {
	Requests   []MessageRequest `json:"requests"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

// OfferMessageRequestInput carries one candidate-authored offer.
type OfferMessageRequestInput struct {
	Subject        string
	Body           string
	Payload        json.RawMessage
	IdempotencyKey string
}

// OfferMessageRequestResult contains the updated request and durable offer.
type OfferMessageRequestResult struct {
	Request MessageRequest      `json:"request"`
	Offer   MessageRequestOffer `json:"offer"`
}

// SelectMessageRequestInput carries one client-ranked coordinator decision.
type SelectMessageRequestInput struct {
	SelectedAgentIDs   []string
	ReservationSeconds int
	IdempotencyKey     string
}

// SelectMessageRequestResult contains the selection and reserved work claims.
type SelectMessageRequestResult struct {
	Request   MessageRequest          `json:"request"`
	Selection MessageRequestSelection `json:"selection"`
	Claims    []MessageRequestClaim   `json:"claims"`
}

// ClaimMessageRequestInput requests a bounded lease for selected work.
type ClaimMessageRequestInput struct {
	LeaseSeconds   int
	IdempotencyKey string
}

// RenewMessageRequestInput renews one exact request-claim fence.
type RenewMessageRequestInput struct {
	ClaimID      string
	Generation   int64
	LeaseSeconds int
}

// ReleaseMessageRequestInput releases one exact request-claim fence.
type ReleaseMessageRequestInput struct {
	ClaimID              string
	Generation           int64
	DeterministicFailure bool
}

// CompleteMessageRequestInput atomically completes claimed work with a result.
type CompleteMessageRequestInput struct {
	ClaimID        string
	Generation     int64
	Subject        string
	Body           string
	Payload        json.RawMessage
	IdempotencyKey string
}

// CompleteMessageRequestResult contains the settled claim and result message.
type CompleteMessageRequestResult struct {
	Request MessageRequest      `json:"request"`
	Claim   MessageRequestClaim `json:"claim"`
	Message Message             `json:"message"`
}

// CreateMessageRequest opens one realm-wide request.
func CreateMessageRequest(ctx context.Context, endpoint, token string, in CreateMessageRequestInput) (CreateMessageRequestResult, error) {
	request := struct {
		Subject            string          `json:"subject,omitempty"`
		Body               string          `json:"body"`
		Payload            json.RawMessage `json:"payload,omitempty"`
		SelectionPolicy    string          `json:"selection_policy,omitempty"`
		MaxAssignees       int             `json:"max_assignees,omitempty"`
		OfferWindowSeconds int             `json:"offer_window_seconds,omitempty"`
		ExpiresInSeconds   int             `json:"expires_in_seconds,omitempty"`
	}{in.Subject, in.Body, in.Payload, in.SelectionPolicy, in.MaxAssignees, in.OfferWindowSeconds, in.ExpiresInSeconds}
	var out CreateMessageRequestResult
	if err := messageRequestJSON(ctx, http.MethodPost, messageRequestsURL(endpoint), token, in.IdempotencyKey, request, &out); err != nil {
		return CreateMessageRequestResult{}, err
	}
	return out, nil
}

// ListMessageRequests returns one page of requests visible to the authenticated agent.
func ListMessageRequests(ctx context.Context, endpoint, token string, opts MessageRequestListOptions) (MessageRequestPage, error) {
	params := neturl.Values{}
	if opts.State != "" {
		params.Set("state", opts.State)
	}
	if opts.Phase != "" {
		params.Set("phase", opts.Phase)
	}
	if opts.Role != "" {
		params.Set("role", opts.Role)
	}
	if opts.Limit != 0 {
		params.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		params.Set("cursor", opts.Cursor)
	}
	url := messageRequestsURL(endpoint)
	if len(params) != 0 {
		url += "?" + params.Encode()
	}
	var out MessageRequestPage
	if err := doJSON(ctx, http.MethodGet, url, token, nil, &out); err != nil {
		return MessageRequestPage{}, err
	}
	if out.Requests == nil {
		out.Requests = []MessageRequest{}
	}
	return out, nil
}

// GetMessageRequest returns one authorized request graph.
func GetMessageRequest(ctx context.Context, endpoint, token, requestID string) (MessageRequestDetail, error) {
	var out MessageRequestDetail
	if err := doJSON(ctx, http.MethodGet, messageRequestURL(endpoint, requestID), token, nil, &out); err != nil {
		return MessageRequestDetail{}, err
	}
	return out, nil
}

// OfferMessageRequest records the authenticated candidate's offer.
func OfferMessageRequest(ctx context.Context, endpoint, token, requestID string, in OfferMessageRequestInput) (OfferMessageRequestResult, error) {
	request := struct {
		Subject string          `json:"subject,omitempty"`
		Body    string          `json:"body"`
		Payload json.RawMessage `json:"payload,omitempty"`
	}{in.Subject, in.Body, in.Payload}
	var out OfferMessageRequestResult
	if err := messageRequestJSON(ctx, http.MethodPost, messageRequestActionURL(endpoint, requestID, "offer"), token, in.IdempotencyKey, request, &out); err != nil {
		return OfferMessageRequestResult{}, err
	}
	return out, nil
}

// DeclineMessageRequest records the authenticated candidate's decline.
func DeclineMessageRequest(ctx context.Context, endpoint, token, requestID, idempotencyKey string) (MessageRequest, error) {
	var out struct {
		Request MessageRequest `json:"request"`
	}
	if err := messageRequestJSON(ctx, http.MethodPost, messageRequestActionURL(endpoint, requestID, "decline"), token, idempotencyKey, struct{}{}, &out); err != nil {
		return MessageRequest{}, err
	}
	return out.Request, nil
}

// SelectMessageRequest records the authenticated coordinator's client-ranked selection.
func SelectMessageRequest(ctx context.Context, endpoint, token, requestID string, in SelectMessageRequestInput) (SelectMessageRequestResult, error) {
	request := struct {
		SelectedAgentIDs   []string `json:"selected_agent_ids"`
		ReservationSeconds int      `json:"reservation_seconds,omitempty"`
	}{in.SelectedAgentIDs, in.ReservationSeconds}
	var out SelectMessageRequestResult
	if err := messageRequestJSON(ctx, http.MethodPost, messageRequestActionURL(endpoint, requestID, "select"), token, in.IdempotencyKey, request, &out); err != nil {
		return SelectMessageRequestResult{}, err
	}
	return out, nil
}

// CancelMessageRequest cancels one request as its authenticated coordinator.
func CancelMessageRequest(ctx context.Context, endpoint, token, requestID string) (MessageRequest, error) {
	var out struct {
		Request MessageRequest `json:"request"`
	}
	if err := messageRequestJSON(ctx, http.MethodPost, messageRequestActionURL(endpoint, requestID, "cancel"), token, "", struct{}{}, &out); err != nil {
		return MessageRequest{}, err
	}
	return out.Request, nil
}

// ClaimMessageRequest acquires the authenticated selected agent's reservation.
func ClaimMessageRequest(ctx context.Context, endpoint, token, requestID string, in ClaimMessageRequestInput) (MessageRequestClaim, error) {
	request := struct {
		LeaseSeconds int `json:"lease_seconds"`
	}{in.LeaseSeconds}
	var out struct {
		Claim MessageRequestClaim `json:"claim"`
	}
	if err := messageRequestJSON(ctx, http.MethodPost, messageRequestActionURL(endpoint, requestID, "claim"), token, in.IdempotencyKey, request, &out); err != nil {
		return MessageRequestClaim{}, err
	}
	return out.Claim, nil
}

// RenewMessageRequest renews one exact live request-claim fence.
func RenewMessageRequest(ctx context.Context, endpoint, token, requestID string, in RenewMessageRequestInput) (MessageRequestClaim, error) {
	request := struct {
		ClaimID      string `json:"claim_id"`
		Generation   int64  `json:"generation"`
		LeaseSeconds int    `json:"lease_seconds"`
	}{in.ClaimID, in.Generation, in.LeaseSeconds}
	var out struct {
		Claim MessageRequestClaim `json:"claim"`
	}
	if err := messageRequestJSON(ctx, http.MethodPost, messageRequestActionURL(endpoint, requestID, "renew"), token, "", request, &out); err != nil {
		return MessageRequestClaim{}, err
	}
	return out.Claim, nil
}

// ReleaseMessageRequest releases one exact live request-claim fence.
func ReleaseMessageRequest(ctx context.Context, endpoint, token, requestID string, in ReleaseMessageRequestInput) (MessageRequestClaim, error) {
	request := struct {
		ClaimID              string `json:"claim_id"`
		Generation           int64  `json:"generation"`
		DeterministicFailure bool   `json:"deterministic_failure,omitempty"`
	}{in.ClaimID, in.Generation, in.DeterministicFailure}
	var out struct {
		Claim MessageRequestClaim `json:"claim"`
	}
	if err := messageRequestJSON(ctx, http.MethodPost, messageRequestActionURL(endpoint, requestID, "release"), token, "", request, &out); err != nil {
		return MessageRequestClaim{}, err
	}
	return out.Claim, nil
}

// CompleteMessageRequest atomically completes one exact claim and creates its result.
func CompleteMessageRequest(ctx context.Context, endpoint, token, requestID string, in CompleteMessageRequestInput) (CompleteMessageRequestResult, error) {
	request := struct {
		ClaimID    string          `json:"claim_id"`
		Generation int64           `json:"generation"`
		Subject    string          `json:"subject,omitempty"`
		Body       string          `json:"body"`
		Payload    json.RawMessage `json:"payload,omitempty"`
	}{in.ClaimID, in.Generation, in.Subject, in.Body, in.Payload}
	var out CompleteMessageRequestResult
	if err := messageRequestJSON(ctx, http.MethodPost, messageRequestActionURL(endpoint, requestID, "complete"), token, in.IdempotencyKey, request, &out); err != nil {
		return CompleteMessageRequestResult{}, err
	}
	return out, nil
}

func messageRequestJSON(ctx context.Context, method, url, token, idempotencyKey string, request, out any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	headers := map[string]string{}
	if idempotencyKey != "" {
		headers["Idempotency-Key"] = idempotencyKey
	}
	return doJSONWithHeaders(ctx, method, url, token, headers, body, out)
}

func messageRequestsURL(endpoint string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/message-requests"
}

func messageRequestURL(endpoint, requestID string) string {
	return messageRequestsURL(endpoint) + "/" + neturl.PathEscape(requestID)
}

func messageRequestActionURL(endpoint, requestID, action string) string {
	return messageRequestURL(endpoint, requestID) + ":" + action
}
