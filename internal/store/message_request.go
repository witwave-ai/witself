package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
)

// Message request protocol constants define stable selection, lifecycle,
// phase, candidate-response, and claim-state values.
const (
	MessageRequestSelectionClientRanked = "client_ranked"

	MessageRequestStateOpen      = "open"
	MessageRequestStateCompleted = "completed"
	MessageRequestStateCancelled = "cancelled"
	MessageRequestStateExpired   = "expired"

	MessageRequestPhaseCollectingOffers  = "collecting_offers"
	MessageRequestPhaseAwaitingSelection = "awaiting_selection"
	MessageRequestPhaseAssigned          = "assigned"

	MessageRequestCandidatePending  = "pending"
	MessageRequestCandidateOffered  = "offered"
	MessageRequestCandidateDeclined = "declined"

	MessageRequestClaimReserved  = "reserved"
	MessageRequestClaimClaimed   = "claimed"
	MessageRequestClaimReleased  = "released"
	MessageRequestClaimCompleted = "completed"
	MessageRequestClaimCancelled = "cancelled"

	defaultMessageRequestOfferWindow = 30 * time.Second
	minMessageRequestOfferWindow     = time.Second
	maxMessageRequestOfferWindow     = 15 * time.Minute
	defaultMessageRequestExpiry      = time.Hour
	maxMessageRequestExpiry          = 7 * 24 * time.Hour
	defaultMessageRequestPageSize    = 50
	maxMessageRequestPageSize        = 100
	maxMessageRequestAssignees       = 8
	// Selection history is protocol-bounded so a coordinator cannot make one
	// request's detail projection grow without limit. Each selection can mint
	// at most maxMessageRequestAssignees claims.
	maxMessageRequestSelectionHistory = 256
	maxMessageRequestClaimHistory     = maxMessageRequestSelectionHistory * maxMessageRequestAssignees
	maxMessageRequestReconcileBatch   = 32
)

// Message request errors classify validation, visibility, concurrency, claim,
// and cursor failures for callers.
var (
	ErrMessageRequestInputInvalid  = errors.New("invalid message request input")
	ErrMessageRequestNotFound      = errors.New("message request not found")
	ErrMessageRequestForbidden     = errors.New("message request access forbidden")
	ErrMessageRequestConflict      = errors.New("message request conflict")
	ErrMessageRequestBusy          = errors.New("message request claim is busy")
	ErrMessageRequestClaimLost     = errors.New("message request claim was lost")
	ErrMessageRequestCursorInvalid = errors.New("malformed message request cursor")
)

// MessageRequest is the recoverable coordination view. State is terminal/open
// storage state; Phase is derived from durable deadlines, responses, and live
// reservations, so no scheduler is required for correctness.
type MessageRequest struct {
	ID                  string
	AccountID           string
	RealmID             string
	OpeningMessageID    string
	Coordinator         MessageAgent
	SelectionPolicy     string
	State               string
	Phase               string
	MaxAssignees        int
	CandidateCount      int
	PendingCount        int
	OfferCount          int
	DeclineCount        int
	ActiveClaimCount    int
	CompletedClaimCount int
	SelectedAgentIDs    []string
	SelectionGeneration int64
	OfferDeadline       time.Time
	ExpiresAt           time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
	CompletedAt         *time.Time
	CancelledAt         *time.Time
	ExpiredAt           *time.Time

	// Opening-message content is included by create/get and omitted by list.
	Subject  string
	Body     string
	Payload  json.RawMessage
	ThreadID string

	offerWindowSeconds int
	expiresInSeconds   int
}

// MessageRequestCandidate is one immutable realm-snapshot participant and its response state.
type MessageRequestCandidate struct {
	Agent          MessageAgent
	ResponseState  string
	OfferMessageID string
	RespondedAt    *time.Time
	CreatedAt      time.Time
}

// MessageRequestOffer pairs a candidate with its durable offer message.
type MessageRequestOffer struct {
	Agent     MessageAgent
	Message   Message
	OfferedAt time.Time
}

// MessageRequestSelection records one immutable coordinator assignment decision.
type MessageRequestSelection struct {
	ID               string
	Generation       int64
	Coordinator      MessageAgent
	SelectedAgentIDs []string
	CreatedAt        time.Time
}

// MessageRequestClaim is one generation-fenced reservation or processing claim.
type MessageRequestClaim struct {
	ClaimID         string
	RequestID       string
	SelectionID     string
	Agent           MessageAgent
	State           string
	Generation      int64
	FailureCount    int64
	LeaseExpiresAt  *time.Time
	ResultMessageID string
	SelectedAt      time.Time
	ClaimedAt       *time.Time
	ReleasedAt      *time.Time
	CompletedAt     *time.Time
	CancelledAt     *time.Time
	UpdatedAt       time.Time
	claimKeyHash    string
	completeKeyHash string
}

// MessageRequestDetail is the authorized full projection of one request.
type MessageRequestDetail struct {
	Request        MessageRequest
	OpeningMessage Message
	Candidates     []MessageRequestCandidate
	Offers         []MessageRequestOffer
	Selections     []MessageRequestSelection
	Claims         []MessageRequestClaim
}

// OpenMessageRequestInput describes a new realm-wide delegation request.
type OpenMessageRequestInput struct {
	Subject         string
	Body            string
	Payload         json.RawMessage
	SelectionPolicy string
	MaxAssignees    int
	OfferWindow     time.Duration
	ExpiresIn       time.Duration
	IdempotencyKey  string
}

// OpenMessageRequestResult returns a request and its fan-out opening message.
type OpenMessageRequestResult struct {
	Request        MessageRequest
	OpeningMessage Message
}

// OfferMessageRequestInput describes a candidate's offer response.
type OfferMessageRequestInput struct {
	Subject        string
	Body           string
	Payload        json.RawMessage
	IdempotencyKey string
}

// OfferMessageRequestResult returns the updated request and durable offer.
type OfferMessageRequestResult struct {
	Request MessageRequest
	Offer   MessageRequestOffer
}

// DeclineMessageRequestInput carries candidate decline idempotency metadata.
type DeclineMessageRequestInput struct {
	IdempotencyKey string
}

// SelectMessageRequestInput describes one client-ranked assignment decision.
type SelectMessageRequestInput struct {
	SelectedAgentIDs []string
	Reservation      time.Duration
	IdempotencyKey   string
}

// SelectMessageRequestResult returns a selection and its initial reservations.
type SelectMessageRequestResult struct {
	Request   MessageRequest
	Selection MessageRequestSelection
	Claims    []MessageRequestClaim
}

// ClaimMessageRequestInput converts a selected reservation into a processing claim.
type ClaimMessageRequestInput struct {
	LeaseDuration  time.Duration
	IdempotencyKey string
}

// RenewMessageRequestInput extends one exact processing-claim generation.
type RenewMessageRequestInput struct {
	ClaimID       string
	Generation    int64
	LeaseDuration time.Duration
}

// ReleaseMessageRequestInput releases one exact processing-claim generation.
type ReleaseMessageRequestInput struct {
	ClaimID              string
	Generation           int64
	DeterministicFailure bool
}

// CompleteMessageRequestInput completes one exact claim and supplies its result.
type CompleteMessageRequestInput struct {
	ClaimID        string
	Generation     int64
	Subject        string
	Body           string
	Payload        json.RawMessage
	IdempotencyKey string
}

// CompleteMessageRequestResult returns the completed request, claim, and result message.
type CompleteMessageRequestResult struct {
	Request MessageRequest
	Claim   MessageRequestClaim
	Message Message
}

// MessageRequestFilter selects one authorized metadata page.
type MessageRequestFilter struct {
	State  string
	Phase  string
	Role   string
	Limit  int
	Cursor string
}

// MessageRequestPage contains a bounded request page and opaque continuation cursor.
type MessageRequestPage struct {
	Requests   []MessageRequest
	NextCursor string
}

func normalizeOpenMessageRequestInput(in OpenMessageRequestInput) (OpenMessageRequestInput, SendMessageInput, error) {
	in.SelectionPolicy = strings.TrimSpace(in.SelectionPolicy)
	if in.SelectionPolicy == "" {
		in.SelectionPolicy = MessageRequestSelectionClientRanked
	}
	if in.SelectionPolicy != MessageRequestSelectionClientRanked {
		return OpenMessageRequestInput{}, SendMessageInput{}, fmt.Errorf("%w: selection policy must be client_ranked", ErrMessageRequestInputInvalid)
	}
	if in.MaxAssignees == 0 {
		in.MaxAssignees = 1
	}
	if in.MaxAssignees < 1 || in.MaxAssignees > maxMessageRequestAssignees {
		return OpenMessageRequestInput{}, SendMessageInput{}, fmt.Errorf("%w: max assignees must be 1-%d", ErrMessageRequestInputInvalid, maxMessageRequestAssignees)
	}
	if in.OfferWindow == 0 {
		in.OfferWindow = defaultMessageRequestOfferWindow
	}
	if in.OfferWindow < minMessageRequestOfferWindow || in.OfferWindow > maxMessageRequestOfferWindow || in.OfferWindow%time.Second != 0 {
		return OpenMessageRequestInput{}, SendMessageInput{}, fmt.Errorf("%w: offer window must be whole seconds between %s and %s", ErrMessageRequestInputInvalid, minMessageRequestOfferWindow, maxMessageRequestOfferWindow)
	}
	if in.ExpiresIn == 0 {
		in.ExpiresIn = defaultMessageRequestExpiry
	}
	if in.ExpiresIn <= in.OfferWindow || in.ExpiresIn > maxMessageRequestExpiry || in.ExpiresIn%time.Second != 0 {
		return OpenMessageRequestInput{}, SendMessageInput{}, fmt.Errorf("%w: expiry must be whole seconds after the offer window and no more than %s", ErrMessageRequestInputInvalid, maxMessageRequestExpiry)
	}
	draft, err := normalizeSendMessageInput(SendMessageInput{
		AudienceKind:   MessageRecipientRealm,
		Subject:        in.Subject,
		Kind:           "open_request",
		Body:           in.Body,
		Payload:        in.Payload,
		IdempotencyKey: in.IdempotencyKey,
	})
	if err != nil {
		return OpenMessageRequestInput{}, SendMessageInput{}, fmt.Errorf("%w: %v", ErrMessageRequestInputInvalid, err)
	}
	if draft.IdempotencyKey == "" {
		return OpenMessageRequestInput{}, SendMessageInput{}, fmt.Errorf("%w: idempotency key is required", ErrMessageRequestInputInvalid)
	}
	in.Subject, in.Body, in.Payload, in.IdempotencyKey = draft.Subject, draft.Body, draft.Payload, draft.IdempotencyKey
	return in, draft, nil
}

func normalizeMessageRequestFilter(filter MessageRequestFilter) (MessageRequestFilter, time.Time, string, error) {
	filter.State = strings.TrimSpace(filter.State)
	filter.Phase = strings.TrimSpace(filter.Phase)
	filter.Role = strings.TrimSpace(filter.Role)
	if filter.State != "" && filter.State != MessageRequestStateOpen && filter.State != MessageRequestStateCompleted && filter.State != MessageRequestStateCancelled && filter.State != MessageRequestStateExpired {
		return MessageRequestFilter{}, time.Time{}, "", fmt.Errorf("%w: unsupported state", ErrMessageRequestInputInvalid)
	}
	if filter.Phase != "" && filter.Phase != MessageRequestPhaseCollectingOffers && filter.Phase != MessageRequestPhaseAwaitingSelection && filter.Phase != MessageRequestPhaseAssigned {
		return MessageRequestFilter{}, time.Time{}, "", fmt.Errorf("%w: unsupported phase", ErrMessageRequestInputInvalid)
	}
	if filter.Role != "" && filter.Role != "candidate" && filter.Role != "coordinator" {
		return MessageRequestFilter{}, time.Time{}, "", fmt.Errorf("%w: role must be candidate or coordinator", ErrMessageRequestInputInvalid)
	}
	if filter.Limit == 0 {
		filter.Limit = defaultMessageRequestPageSize
	}
	if filter.Limit < 1 || filter.Limit > maxMessageRequestPageSize {
		return MessageRequestFilter{}, time.Time{}, "", fmt.Errorf("%w: limit must be 1-%d", ErrMessageRequestInputInvalid, maxMessageRequestPageSize)
	}
	if filter.Cursor == "" {
		return filter, time.Time{}, "", nil
	}
	cursorTime, cursorID, err := decodeMessageRequestCursor(filter.Cursor)
	if err != nil {
		return MessageRequestFilter{}, time.Time{}, "", ErrMessageRequestCursorInvalid
	}
	return filter, cursorTime, cursorID, nil
}

func normalizeMessageRequestID(requestID string) (string, error) {
	requestID = strings.TrimSpace(requestID)
	if !strings.HasPrefix(requestID, "mrq_") || len(requestID) > 128 {
		return "", fmt.Errorf("%w: invalid request id", ErrMessageRequestInputInvalid)
	}
	return requestID, nil
}

func normalizeMessageRequestClaimFence(claimID string, generation int64) (string, int64, error) {
	claimID = strings.TrimSpace(claimID)
	if !strings.HasPrefix(claimID, "mrc_") || len(claimID) > 128 {
		return "", 0, fmt.Errorf("%w: invalid claim id", ErrMessageRequestInputInvalid)
	}
	if generation < 1 || generation > maxMessageProcessingGeneration {
		return "", 0, fmt.Errorf("%w: invalid claim generation", ErrMessageRequestInputInvalid)
	}
	return claimID, generation, nil
}

func normalizeSelectedAgentIDs(ids []string) ([]string, error) {
	if len(ids) == 0 || len(ids) > maxMessageRequestAssignees {
		return nil, fmt.Errorf("%w: selected_agent_ids must contain 1-%d agents", ErrMessageRequestInputInvalid, maxMessageRequestAssignees)
	}
	out := make([]string, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for i, raw := range ids {
		agentID := strings.TrimSpace(raw)
		if !strings.HasPrefix(agentID, "agent_") || len(agentID) > maxMessageRecipientBytes {
			return nil, fmt.Errorf("%w: invalid selected agent id", ErrMessageRequestInputInvalid)
		}
		if _, ok := seen[agentID]; ok {
			return nil, fmt.Errorf("%w: duplicate selected agent id", ErrMessageRequestInputInvalid)
		}
		seen[agentID] = struct{}{}
		out[i] = agentID
	}
	sort.Strings(out)
	return out, nil
}

func messageRequestMutationHash(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func messageRequestSelect(includeContent bool) string {
	body := `''::text`
	payload := `NULL::jsonb`
	if includeContent {
		body = `opening.body`
		payload = `opening.payload`
	}
	return fmt.Sprintf(`
		WITH request_clock AS MATERIALIZED (
		  SELECT clock_timestamp() AS now
		)
		SELECT r.id, r.account_id, r.realm_id, r.opening_message_id,
		       r.coordinator_agent_id, coordinator.name,
		       r.selection_policy, r.state, r.max_assignees,
		       r.offer_window_seconds, r.expires_in_seconds,
		       r.offer_deadline, r.expires_at, r.selection_generation,
		       r.completed_at, r.cancelled_at, r.expired_at,
		       r.created_at, r.updated_at,
		       opening.subject, %s, %s, opening.thread_id,
		       stats.candidate_count, stats.pending_count, stats.offer_count,
		       stats.decline_count, stats.active_claim_count,
		       stats.completed_claim_count, stats.selected_agent_ids,
		       request_clock.now
		FROM agent_message_requests r
		JOIN agents coordinator ON coordinator.id=r.coordinator_agent_id
		JOIN agent_messages opening ON opening.id=r.opening_message_id
		CROSS JOIN request_clock
		JOIN LATERAL (
		  SELECT
		    (SELECT count(*)::integer FROM agent_message_request_candidates c
		     WHERE c.request_id=r.id) AS candidate_count,
		    (SELECT count(*)::integer FROM agent_message_request_candidates c
		     WHERE c.request_id=r.id AND c.response_state='pending') AS pending_count,
		    (SELECT count(*)::integer FROM agent_message_request_candidates c
		     WHERE c.request_id=r.id AND c.response_state='offered') AS offer_count,
		    (SELECT count(*)::integer FROM agent_message_request_candidates c
		     WHERE c.request_id=r.id AND c.response_state='declined') AS decline_count,
		    (SELECT count(*)::integer FROM agent_message_request_claims c
		     WHERE c.request_id=r.id AND c.state IN ('reserved','claimed')
		       AND c.lease_expires_at > request_clock.now) AS active_claim_count,
		    (SELECT count(*)::integer FROM agent_message_request_claims c
		     WHERE c.request_id=r.id AND c.state='completed') AS completed_claim_count,
		    COALESCE((SELECT array_agg(c.agent_id ORDER BY c.agent_id)
		     FROM agent_message_request_claims c
		     WHERE c.request_id=r.id AND c.state IN ('reserved','claimed')
		       AND c.lease_expires_at > request_clock.now), ARRAY[]::text[]) AS selected_agent_ids
		) stats ON true
	`, body, payload)
}

func scanMessageRequest(row rowScanner) (MessageRequest, error) {
	var request MessageRequest
	var now time.Time
	if err := row.Scan(
		&request.ID, &request.AccountID, &request.RealmID, &request.OpeningMessageID,
		&request.Coordinator.ID, &request.Coordinator.Name,
		&request.SelectionPolicy, &request.State, &request.MaxAssignees,
		&request.offerWindowSeconds, &request.expiresInSeconds,
		&request.OfferDeadline, &request.ExpiresAt, &request.SelectionGeneration,
		&request.CompletedAt, &request.CancelledAt, &request.ExpiredAt,
		&request.CreatedAt, &request.UpdatedAt,
		&request.Subject, &request.Body, &request.Payload, &request.ThreadID,
		&request.CandidateCount, &request.PendingCount, &request.OfferCount,
		&request.DeclineCount, &request.ActiveClaimCount,
		&request.CompletedClaimCount, &request.SelectedAgentIDs, &now,
	); err != nil {
		return MessageRequest{}, err
	}
	if request.State == MessageRequestStateOpen && !request.ExpiresAt.After(now) {
		request.State = MessageRequestStateExpired
		if request.ExpiredAt == nil {
			expiredAt := request.ExpiresAt
			request.ExpiredAt = &expiredAt
		}
	}
	request.Phase = deriveMessageRequestPhase(request, now)
	return request, nil
}

func projectMessageRequestForPrincipal(request MessageRequest, p Principal) MessageRequest {
	if request.Coordinator.ID == p.ID {
		return request
	}
	selectedSelf := false
	for _, agentID := range request.SelectedAgentIDs {
		if agentID == p.ID {
			selectedSelf = true
			break
		}
	}
	request.SelectedAgentIDs = []string{}
	if selectedSelf {
		request.SelectedAgentIDs = []string{p.ID}
	}
	return request
}

func deriveMessageRequestPhase(request MessageRequest, now time.Time) string {
	if request.State != MessageRequestStateOpen {
		return ""
	}
	if request.ActiveClaimCount > 0 {
		return MessageRequestPhaseAssigned
	}
	if now.Before(request.OfferDeadline) && request.PendingCount > 0 {
		return MessageRequestPhaseCollectingOffers
	}
	return MessageRequestPhaseAwaitingSelection
}

// OpenMessageRequest creates one realm-audience opening message and the
// immutable candidate snapshot in the same transaction.
func (s *Store) OpenMessageRequest(ctx context.Context, p Principal, in OpenMessageRequestInput) (OpenMessageRequestResult, error) {
	if p.Kind != PrincipalAgent {
		return OpenMessageRequestResult{}, ErrMessageRequestForbidden
	}
	normalized, draft, err := normalizeOpenMessageRequestInput(in)
	if err != nil {
		return OpenMessageRequestResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OpenMessageRequestResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return OpenMessageRequestResult{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return OpenMessageRequestResult{}, err
	}
	if existing, replay, err := messageReplayByIdempotencyKey(ctx, tx, p, draft.IdempotencyKey); err != nil {
		return OpenMessageRequestResult{}, err
	} else if replay {
		if !messageMatchesSendReplay(existing, draft) {
			return OpenMessageRequestResult{}, ErrMessageRequestConflict
		}
		request, err := getMessageRequestByOpeningTx(ctx, tx, p, existing.ID, true)
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, ErrMessageRequestNotFound) {
			return OpenMessageRequestResult{}, ErrMessageRequestConflict
		}
		if err != nil {
			return OpenMessageRequestResult{}, err
		}
		if request.MaxAssignees != normalized.MaxAssignees ||
			request.SelectionPolicy != normalized.SelectionPolicy ||
			request.offerWindowSeconds != int(normalized.OfferWindow/time.Second) ||
			request.expiresInSeconds != int(normalized.ExpiresIn/time.Second) {
			return OpenMessageRequestResult{}, ErrMessageRequestConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return OpenMessageRequestResult{}, err
		}
		return OpenMessageRequestResult{Request: request, OpeningMessage: redactMessageProcessingFence(existing)}, nil
	}

	recipients, err := resolveMessageAudience(ctx, tx, p, draft)
	if err != nil {
		return OpenMessageRequestResult{}, err
	}
	opening, err := s.insertMessageAudienceTx(ctx, tx, p, recipients, draft)
	if err != nil {
		return OpenMessageRequestResult{}, err
	}
	// insertMessageAudienceTx converges concurrent uses of one idempotency key
	// onto the winning message. If this transaction lost that race, the winner
	// committed its request in the same transaction before its message became
	// visible here. Replay that request instead of colliding with the request's
	// unique opening-message constraint.
	if request, findErr := getMessageRequestByOpeningTx(ctx, tx, p, opening.ID, true); findErr == nil {
		if request.MaxAssignees != normalized.MaxAssignees ||
			request.SelectionPolicy != normalized.SelectionPolicy ||
			request.offerWindowSeconds != int(normalized.OfferWindow/time.Second) ||
			request.expiresInSeconds != int(normalized.ExpiresIn/time.Second) {
			return OpenMessageRequestResult{}, ErrMessageRequestConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return OpenMessageRequestResult{}, err
		}
		return OpenMessageRequestResult{
			Request: request, OpeningMessage: redactMessageProcessingFence(opening),
		}, nil
	} else if !errors.Is(findErr, ErrMessageRequestNotFound) {
		return OpenMessageRequestResult{}, findErr
	}
	requestID, err := id.New("mrq")
	if err != nil {
		return OpenMessageRequestResult{}, err
	}
	offerDeadline := opening.CreatedAt.Add(normalized.OfferWindow)
	expiresAt := opening.CreatedAt.Add(normalized.ExpiresIn)
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_message_requests
		  (id, account_id, realm_id, opening_message_id, coordinator_agent_id,
		   selection_policy, max_assignees, offer_window_seconds,
		   expires_in_seconds, offer_deadline, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		requestID, p.AccountID, p.RealmID, opening.ID, p.ID,
		normalized.SelectionPolicy, normalized.MaxAssignees,
		int(normalized.OfferWindow/time.Second), int(normalized.ExpiresIn/time.Second),
		offerDeadline, expiresAt); err != nil {
		return OpenMessageRequestResult{}, fmt.Errorf("insert message request: %w", err)
	}
	for _, candidate := range recipients {
		if _, err := tx.Exec(ctx, `
			INSERT INTO agent_message_request_candidates
			  (request_id, account_id, realm_id, agent_id)
			VALUES ($1,$2,$3,$4)`, requestID, p.AccountID, p.RealmID, candidate.ID); err != nil {
			return OpenMessageRequestResult{}, fmt.Errorf("insert message request candidate: %w", err)
		}
	}
	if err := logMessageRequestAudit(ctx, tx, p, VerbMessageRequestOpened, map[string]any{
		"request_id": requestID, "opening_message_id": opening.ID,
		"coordinator_agent_id": p.ID, "max_assignees": strconv.Itoa(normalized.MaxAssignees),
	}); err != nil {
		return OpenMessageRequestResult{}, err
	}
	request, err := getMessageRequestByIDTx(ctx, tx, p, requestID, true)
	if err != nil {
		return OpenMessageRequestResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return OpenMessageRequestResult{}, err
	}
	return OpenMessageRequestResult{Request: request, OpeningMessage: redactMessageProcessingFence(opening)}, nil
}

func getMessageRequestByOpeningTx(ctx context.Context, tx pgx.Tx, p Principal, openingMessageID string, includeContent bool) (MessageRequest, error) {
	request, err := scanMessageRequest(tx.QueryRow(ctx, messageRequestSelect(includeContent)+`
		LEFT JOIN agent_message_request_candidates mine
		  ON mine.request_id=r.id AND mine.agent_id=$4
		WHERE r.opening_message_id=$1 AND r.account_id=$2 AND r.realm_id=$3
		  AND (r.coordinator_agent_id=$4 OR mine.agent_id IS NOT NULL)`,
		openingMessageID, p.AccountID, p.RealmID, p.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return MessageRequest{}, ErrMessageRequestNotFound
	}
	return projectMessageRequestForPrincipal(request, p), err
}

func getMessageRequestByIDTx(ctx context.Context, tx pgx.Tx, p Principal, requestID string, includeContent bool) (MessageRequest, error) {
	request, err := scanMessageRequest(tx.QueryRow(ctx, messageRequestSelect(includeContent)+`
		LEFT JOIN agent_message_request_candidates mine
		  ON mine.request_id=r.id AND mine.agent_id=$4
		WHERE r.id=$1 AND r.account_id=$2 AND r.realm_id=$3
		  AND (r.coordinator_agent_id=$4 OR mine.agent_id IS NOT NULL)`,
		requestID, p.AccountID, p.RealmID, p.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return MessageRequest{}, ErrMessageRequestNotFound
	}
	if err != nil {
		return MessageRequest{}, fmt.Errorf("read message request: %w", err)
	}
	return projectMessageRequestForPrincipal(request, p), nil
}

// ListMessageRequests returns a metadata-only page visible to the coordinator
// or one of the immutable candidates. It never broadens access to the realm.
func (s *Store) ListMessageRequests(ctx context.Context, p Principal, filter MessageRequestFilter) (MessageRequestPage, error) {
	if p.Kind != PrincipalAgent {
		return MessageRequestPage{}, ErrMessageRequestForbidden
	}
	filter, cursorTime, cursorID, err := normalizeMessageRequestFilter(filter)
	if err != nil {
		return MessageRequestPage{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MessageRequestPage{}, fmt.Errorf("begin message request list snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return MessageRequestPage{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return MessageRequestPage{}, err
	}
	if _, _, _, err := reconcileMessageRequestBatchTx(
		ctx, tx, p.AccountID, p.RealmID, maxMessageRequestReconcileBatch,
	); err != nil {
		return MessageRequestPage{}, err
	}
	var query strings.Builder
	query.WriteString(messageRequestSelect(false))
	query.WriteString(`
		LEFT JOIN agent_message_request_candidates mine
		  ON mine.request_id=r.id AND mine.agent_id=$4
		WHERE r.account_id=$1 AND r.realm_id=$2
		  AND (r.coordinator_agent_id=$4 OR mine.agent_id IS NOT NULL)`)
	args := []any{p.AccountID, p.RealmID, filter.Limit + 1, p.ID}
	switch filter.Role {
	case "coordinator":
		query.WriteString(` AND r.coordinator_agent_id=$4`)
	case "candidate":
		query.WriteString(` AND mine.agent_id IS NOT NULL`)
	}
	if filter.State != "" {
		args = append(args, filter.State)
		parameter := len(args)
		fmt.Fprintf(&query, ` AND (CASE WHEN r.state='open' AND r.expires_at<=request_clock.now
		  THEN 'expired' ELSE r.state END)=$%d`, parameter)
	}
	if filter.Phase != "" {
		args = append(args, filter.Phase)
		parameter := len(args)
		fmt.Fprintf(&query, ` AND r.state='open' AND r.expires_at>request_clock.now
		  AND (CASE
		    WHEN EXISTS (SELECT 1 FROM agent_message_request_claims active
		      WHERE active.request_id=r.id AND active.state IN ('reserved','claimed')
		        AND active.lease_expires_at>request_clock.now) THEN 'assigned'
		    WHEN r.offer_deadline>request_clock.now AND EXISTS (
		      SELECT 1 FROM agent_message_request_candidates pending
		      WHERE pending.request_id=r.id AND pending.response_state='pending')
		      THEN 'collecting_offers'
		    ELSE 'awaiting_selection'
		  END)=$%d`, parameter)
	}
	if !cursorTime.IsZero() {
		args = append(args, cursorTime, cursorID)
		timeParameter := len(args) - 1
		idParameter := len(args)
		fmt.Fprintf(&query, ` AND (r.created_at<$%d OR (r.created_at=$%d AND r.id<$%d))`, timeParameter, timeParameter, idParameter)
	}
	query.WriteString(` ORDER BY r.created_at DESC, r.id DESC LIMIT $3`)
	rows, err := tx.Query(ctx, query.String(), args...)
	if err != nil {
		return MessageRequestPage{}, fmt.Errorf("list message requests: %w", err)
	}
	defer rows.Close()
	requests := make([]MessageRequest, 0, filter.Limit+1)
	for rows.Next() {
		request, err := scanMessageRequest(rows)
		if err != nil {
			return MessageRequestPage{}, fmt.Errorf("scan message request: %w", err)
		}
		requests = append(requests, projectMessageRequestForPrincipal(request, p))
	}
	if err := rows.Err(); err != nil {
		return MessageRequestPage{}, fmt.Errorf("list message requests: %w", err)
	}
	rows.Close()
	page := MessageRequestPage{Requests: requests}
	if len(page.Requests) > filter.Limit {
		last := page.Requests[filter.Limit-1]
		page.Requests = page.Requests[:filter.Limit]
		page.NextCursor = encodeMessageRequestCursor(last.CreatedAt, last.ID)
	}
	if err := tx.Commit(ctx); err != nil {
		return MessageRequestPage{}, err
	}
	return page, nil
}

// GetMessageRequest returns the principal-authorized coordination detail. A
// candidate sees its own response/claims; the coordinator sees the snapshot.
func (s *Store) GetMessageRequest(ctx context.Context, p Principal, requestID string) (MessageRequestDetail, error) {
	if p.Kind != PrincipalAgent {
		return MessageRequestDetail{}, ErrMessageRequestForbidden
	}
	var err error
	requestID, err = normalizeMessageRequestID(requestID)
	if err != nil {
		return MessageRequestDetail{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return MessageRequestDetail{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return MessageRequestDetail{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return MessageRequestDetail{}, err
	}
	if _, _, err := reconcileMessageRequestsTx(ctx, tx, p.AccountID, p.RealmID, requestID); err != nil {
		return MessageRequestDetail{}, err
	}
	detail, err := loadMessageRequestDetailTx(ctx, tx, p, requestID)
	if err != nil {
		return MessageRequestDetail{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MessageRequestDetail{}, err
	}
	return detail, nil
}

func loadMessageRequestDetailTx(ctx context.Context, tx pgx.Tx, p Principal, requestID string) (MessageRequestDetail, error) {
	request, err := getMessageRequestByIDTx(ctx, tx, p, requestID, true)
	if err != nil {
		return MessageRequestDetail{}, err
	}
	isCoordinator := request.Coordinator.ID == p.ID
	var opening Message
	if isCoordinator {
		opening, err = messageByScopedID(ctx, tx, p.AccountID, p.RealmID, request.OpeningMessageID, true)
	} else {
		opening, err = messageDeliveryByScopedID(
			ctx, tx, p.AccountID, p.RealmID, p.ID, request.OpeningMessageID, true,
		)
	}
	if err != nil {
		return MessageRequestDetail{}, fmt.Errorf("read message request opening: %w", err)
	}

	candidateQuery := `
		SELECT c.agent_id, a.name, c.response_state,
		       COALESCE(c.offer_message_id,''), c.responded_at, c.created_at
		FROM agent_message_request_candidates c
		JOIN agents a ON a.id=c.agent_id
		WHERE c.request_id=$1 AND c.account_id=$2 AND c.realm_id=$3`
	args := []any{request.ID, p.AccountID, p.RealmID}
	if !isCoordinator {
		candidateQuery += ` AND c.agent_id=$4`
		args = append(args, p.ID)
	}
	candidateQuery += ` ORDER BY c.agent_id`
	rows, err := tx.Query(ctx, candidateQuery, args...)
	if err != nil {
		return MessageRequestDetail{}, fmt.Errorf("list message request candidates: %w", err)
	}
	candidates := make([]MessageRequestCandidate, 0, request.CandidateCount)
	for rows.Next() {
		var candidate MessageRequestCandidate
		if err := rows.Scan(&candidate.Agent.ID, &candidate.Agent.Name, &candidate.ResponseState,
			&candidate.OfferMessageID, &candidate.RespondedAt, &candidate.CreatedAt); err != nil {
			rows.Close()
			return MessageRequestDetail{}, fmt.Errorf("scan message request candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return MessageRequestDetail{}, fmt.Errorf("list message request candidates: %w", err)
	}
	rows.Close()
	offers := make([]MessageRequestOffer, 0, request.OfferCount)
	for _, candidate := range candidates {
		if candidate.OfferMessageID == "" {
			continue
		}
		message, err := messageByScopedID(ctx, tx, p.AccountID, p.RealmID, candidate.OfferMessageID, true)
		if err != nil {
			return MessageRequestDetail{}, fmt.Errorf("read message request offer: %w", err)
		}
		offers = append(offers, MessageRequestOffer{
			Agent: candidate.Agent, Message: redactMessageProcessingFence(message), OfferedAt: message.CreatedAt,
		})
	}

	selectionQuery := `
		SELECT s.id, s.generation, s.coordinator_agent_id, a.name,
		       COALESCE(array_agg(c.agent_id ORDER BY c.agent_id)
		         FILTER (WHERE c.agent_id IS NOT NULL), ARRAY[]::text[]), s.created_at
		FROM agent_message_request_selections s
		JOIN agents a ON a.id=s.coordinator_agent_id
		LEFT JOIN agent_message_request_claims c ON c.selection_id=s.id
		WHERE s.request_id=$1 AND s.account_id=$2 AND s.realm_id=$3`
	selectionArgs := []any{request.ID, p.AccountID, p.RealmID}
	if !isCoordinator {
		selectionQuery += ` AND EXISTS (SELECT 1 FROM agent_message_request_claims own
		  WHERE own.selection_id=s.id AND own.agent_id=$4)`
		selectionArgs = append(selectionArgs, p.ID)
	}
	selectionQuery += fmt.Sprintf(
		` GROUP BY s.id,a.name ORDER BY s.generation LIMIT %d`,
		maxMessageRequestSelectionHistory+1,
	)
	rows, err = tx.Query(ctx, selectionQuery, selectionArgs...)
	if err != nil {
		return MessageRequestDetail{}, fmt.Errorf("list message request selections: %w", err)
	}
	selections := make([]MessageRequestSelection, 0)
	for rows.Next() {
		var selection MessageRequestSelection
		if err := rows.Scan(&selection.ID, &selection.Generation, &selection.Coordinator.ID,
			&selection.Coordinator.Name, &selection.SelectedAgentIDs, &selection.CreatedAt); err != nil {
			rows.Close()
			return MessageRequestDetail{}, fmt.Errorf("scan message request selection: %w", err)
		}
		if !isCoordinator {
			selection.SelectedAgentIDs = []string{p.ID}
		}
		selections = append(selections, selection)
		if len(selections) > maxMessageRequestSelectionHistory {
			rows.Close()
			return MessageRequestDetail{}, fmt.Errorf(
				"message request selection history exceeds %d entries",
				maxMessageRequestSelectionHistory,
			)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return MessageRequestDetail{}, fmt.Errorf("list message request selections: %w", err)
	}
	rows.Close()

	claimQuery := messageRequestClaimSelect() + `
		WHERE c.request_id=$1 AND c.account_id=$2 AND c.realm_id=$3`
	claimArgs := []any{request.ID, p.AccountID, p.RealmID}
	if !isCoordinator {
		claimQuery += ` AND c.agent_id=$4`
		claimArgs = append(claimArgs, p.ID)
	}
	claimQuery += fmt.Sprintf(
		` ORDER BY c.selected_at,c.id LIMIT %d`,
		maxMessageRequestClaimHistory+1,
	)
	rows, err = tx.Query(ctx, claimQuery, claimArgs...)
	if err != nil {
		return MessageRequestDetail{}, fmt.Errorf("list message request claims: %w", err)
	}
	claims := make([]MessageRequestClaim, 0)
	for rows.Next() {
		claim, err := scanMessageRequestClaim(rows)
		if err != nil {
			rows.Close()
			return MessageRequestDetail{}, fmt.Errorf("scan message request claim: %w", err)
		}
		claims = append(claims, claim)
		if len(claims) > maxMessageRequestClaimHistory {
			rows.Close()
			return MessageRequestDetail{}, fmt.Errorf(
				"message request claim history exceeds %d entries",
				maxMessageRequestClaimHistory,
			)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return MessageRequestDetail{}, fmt.Errorf("list message request claims: %w", err)
	}
	rows.Close()

	return MessageRequestDetail{
		Request: request, OpeningMessage: redactMessageProcessingFence(opening),
		Candidates: candidates, Offers: offers, Selections: selections, Claims: claims,
	}, nil
}

func encodeMessageRequestCursor(t time.Time, requestID string) string {
	return fmt.Sprintf("%d:%s", t.UnixNano(), requestID)
}

func decodeMessageRequestCursor(cursor string) (time.Time, string, error) {
	i := strings.IndexByte(cursor, ':')
	if i <= 0 || i == len(cursor)-1 {
		return time.Time{}, "", ErrMessageRequestCursorInvalid
	}
	ns, err := strconv.ParseInt(cursor[:i], 10, 64)
	requestID := cursor[i+1:]
	if err != nil || !strings.HasPrefix(requestID, "mrq_") {
		return time.Time{}, "", ErrMessageRequestCursorInvalid
	}
	return time.Unix(0, ns).UTC(), requestID, nil
}

type lockedMessageRequestCandidate struct {
	coordinator      MessageAgent
	state            string
	openingMessageID string
	threadID         string
	offerDeadline    time.Time
	expiresAt        time.Time
	responseState    string
	offerMessageID   string
	offerKeyHash     string
	offerRequestHash string
	now              time.Time
}

// Lock the coordinator before the request row. DeleteAgent follows the same
// account -> agent -> request order, so a candidate offer cannot deadlock a
// concurrent coordinator deletion while holding the request row.
func lockMessageRequestCandidateCoordinatorTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	requestID string,
) (Agent, error) {
	var coordinator Agent
	err := tx.QueryRow(ctx, `
		SELECT coordinator.id,coordinator.name
		FROM agent_message_requests r
		JOIN agent_message_request_candidates c
		  ON c.request_id=r.id AND c.agent_id=$4
		JOIN agents coordinator ON coordinator.id=r.coordinator_agent_id
		WHERE r.id=$1 AND r.account_id=$2 AND r.realm_id=$3
		FOR SHARE OF coordinator`, requestID, p.AccountID, p.RealmID, p.ID).
		Scan(&coordinator.ID, &coordinator.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, ErrMessageRequestNotFound
	}
	if err != nil {
		return Agent{}, fmt.Errorf("lock message request coordinator: %w", err)
	}
	return coordinator, nil
}

func lockMessageRequestCandidateTx(ctx context.Context, tx pgx.Tx, p Principal, requestID string) (lockedMessageRequestCandidate, error) {
	var locked lockedMessageRequestCandidate
	err := tx.QueryRow(ctx, `
		SELECT r.coordinator_agent_id, coordinator.name, r.state,
		       r.opening_message_id, opening.thread_id,
		       r.offer_deadline, r.expires_at,
		       c.response_state, COALESCE(c.offer_message_id,''),
		       c.offer_key_hash, c.offer_request_hash, clock_timestamp()
		FROM agent_message_requests r
		JOIN agents coordinator ON coordinator.id=r.coordinator_agent_id
		JOIN agent_messages opening ON opening.id=r.opening_message_id
		JOIN agent_message_request_candidates c
		  ON c.request_id=r.id AND c.agent_id=$4
		WHERE r.id=$1 AND r.account_id=$2 AND r.realm_id=$3
		FOR UPDATE OF r,c`, requestID, p.AccountID, p.RealmID, p.ID).Scan(
		&locked.coordinator.ID, &locked.coordinator.Name, &locked.state,
		&locked.openingMessageID, &locked.threadID,
		&locked.offerDeadline, &locked.expiresAt,
		&locked.responseState, &locked.offerMessageID,
		&locked.offerKeyHash, &locked.offerRequestHash, &locked.now,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedMessageRequestCandidate{}, ErrMessageRequestNotFound
	}
	if err != nil {
		return lockedMessageRequestCandidate{}, fmt.Errorf("lock message request candidate: %w", err)
	}
	return locked, nil
}

// expireDueMessageRequestsTx is the lazy scheduler for request expiry. It may
// target one request or every due request in a realm. Expiry and active-fence
// cancellation are committed by the caller in the same transaction, making
// the terminal state durable without any backend worker.
func expireDueMessageRequestsTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
	realmID string,
	requestID string,
) (int64, error) {
	rows, err := tx.Query(ctx, `
		UPDATE agent_message_requests
		SET state='expired', expired_at=expires_at, updated_at=clock_timestamp()
		WHERE account_id=$1 AND ($2='' OR realm_id=$2) AND ($3='' OR id=$3)
		  AND state='open' AND expires_at<=clock_timestamp()
		RETURNING id, opening_message_id, coordinator_agent_id, max_assignees`,
		accountID, realmID, requestID)
	if err != nil {
		return 0, fmt.Errorf("expire message requests: %w", err)
	}
	type expiredRequestAudit struct {
		requestID          string
		openingMessageID   string
		coordinatorAgentID string
		maxAssignees       int
	}
	expired := make([]expiredRequestAudit, 0)
	for rows.Next() {
		var record expiredRequestAudit
		if err := rows.Scan(
			&record.requestID, &record.openingMessageID,
			&record.coordinatorAgentID, &record.maxAssignees,
		); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan expired message request: %w", err)
		}
		expired = append(expired, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("expire message request rows: %w", err)
	}
	rows.Close()
	// Also repair any previously persisted/imported expired request that still
	// carries an active-looking fence. Authority ends at the request deadline.
	if _, err := tx.Exec(ctx, `
		UPDATE agent_message_request_claims c
		SET state='cancelled', lease_expires_at=NULL,
		    cancelled_at=r.expires_at, updated_at=clock_timestamp()
		FROM agent_message_requests r
		WHERE r.id=c.request_id AND r.account_id=$1 AND ($2='' OR r.realm_id=$2)
		  AND ($3='' OR r.id=$3) AND r.state='expired'
		  AND c.state IN ('reserved','claimed')`, accountID, realmID, requestID); err != nil {
		return 0, fmt.Errorf("cancel expired message request claims: %w", err)
	}
	for _, record := range expired {
		if err := logEventTx(ctx, tx, EventInput{
			AccountID: accountID, ActorKind: ActorSystem, Verb: VerbMessageRequestExpired,
			Metadata: map[string]any{
				"request_id": record.requestID, "opening_message_id": record.openingMessageID,
				"coordinator_agent_id": record.coordinatorAgentID,
				"max_assignees":        strconv.Itoa(record.maxAssignees),
			},
		}); err != nil {
			return 0, fmt.Errorf("log expired message request: %w", err)
		}
	}
	return int64(len(expired)), nil
}

// settleMessageRequestClaimsTx durably removes expired claim authority and
// closes a selected batch once it has produced at least one result and has no
// remaining live work. MaxAssignees is only a capacity ceiling: a released,
// expired, or cancelled sibling must not keep a successful request open.
func settleMessageRequestClaimsTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
	realmID string,
	requestID string,
) (int64, error) {
	// Lock parents before children, matching claim/complete/delete lock order.
	// Work only on the locked ID snapshot so a concurrent newly-opened request
	// cannot be reached by the child updates below.
	rows, err := tx.Query(ctx, `
		SELECT r.id
		FROM agent_message_requests r
		WHERE r.account_id=$1 AND ($2='' OR r.realm_id=$2) AND ($3='' OR r.id=$3)
		  AND r.state='open'
		  AND (
		    EXISTS (
		      SELECT 1 FROM agent_message_request_claims stale
		      WHERE stale.request_id=r.id AND stale.state IN ('reserved','claimed')
		        AND stale.lease_expires_at<=clock_timestamp()
		    )
		    OR (
		      EXISTS (
		        SELECT 1 FROM agent_message_request_claims completed
		        WHERE completed.request_id=r.id AND completed.state='completed'
		      )
		      AND NOT EXISTS (
		        SELECT 1 FROM agent_message_request_claims active
		        WHERE active.request_id=r.id AND active.state IN ('reserved','claimed')
		          AND active.lease_expires_at>clock_timestamp()
		      )
		    )
		  )
		ORDER BY r.id
		FOR UPDATE OF r`, accountID, realmID, requestID)
	if err != nil {
		return 0, fmt.Errorf("lock settleable message requests: %w", err)
	}
	requestIDs := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan settleable message request: %w", err)
		}
		requestIDs = append(requestIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("lock settleable message request rows: %w", err)
	}
	rows.Close()

	var completedCount int64
	for _, id := range requestIDs {
		if _, err := tx.Exec(ctx, `
			UPDATE agent_message_request_claims
			SET state='cancelled',lease_expires_at=NULL,
			    cancelled_at=clock_timestamp(),updated_at=clock_timestamp()
			WHERE request_id=$1 AND state IN ('reserved','claimed')
			  AND lease_expires_at<=clock_timestamp()`, id); err != nil {
			return 0, fmt.Errorf("cancel expired message request claim leases: %w", err)
		}
		completed, err := tx.Exec(ctx, `
			UPDATE agent_message_requests r
			SET state='completed',completed_at=clock_timestamp(),updated_at=clock_timestamp()
			WHERE r.id=$1 AND r.state='open' AND r.expires_at>clock_timestamp()
			  AND EXISTS (
			    SELECT 1 FROM agent_message_request_claims completed
			    WHERE completed.request_id=r.id AND completed.state='completed'
			  )
			  AND NOT EXISTS (
			    SELECT 1 FROM agent_message_request_claims active
			    WHERE active.request_id=r.id AND active.state IN ('reserved','claimed')
			  )`, id)
		if err != nil {
			return 0, fmt.Errorf("settle completed message request: %w", err)
		}
		completedCount += completed.RowsAffected()
	}
	return completedCount, nil
}

func reconcileMessageRequestsTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
	realmID string,
	requestID string,
) (int64, int64, error) {
	expired, err := expireDueMessageRequestsTx(ctx, tx, accountID, realmID, requestID)
	if err != nil {
		return 0, 0, err
	}
	completed, err := settleMessageRequestClaimsTx(ctx, tx, accountID, realmID, requestID)
	if err != nil {
		return 0, 0, err
	}
	return expired, completed, nil
}

// reconcileMessageRequestBatchTx bounds realm-wide lazy scheduling. It locks
// one stable request-id batch and skips rows another transaction is already
// reconciling; every selected request is then passed through the exact
// per-request expiry/settlement path, preserving audit and fence semantics.
func reconcileMessageRequestBatchTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
	realmID string,
	limit int,
) (int, int64, int64, error) {
	if limit < 1 || limit > maxMessageRequestReconcileBatch {
		return 0, 0, 0, fmt.Errorf("invalid message request reconcile batch size %d", limit)
	}
	rows, err := tx.Query(ctx, `
		SELECT r.id
		FROM agent_message_requests r
		WHERE r.account_id=$1 AND ($2='' OR r.realm_id=$2) AND r.state='open'
		  AND (
		    r.expires_at<=clock_timestamp()
		    OR EXISTS (
		      SELECT 1 FROM agent_message_request_claims stale
		      WHERE stale.request_id=r.id AND stale.state IN ('reserved','claimed')
		        AND stale.lease_expires_at<=clock_timestamp()
		    )
		    OR (
		      EXISTS (
		        SELECT 1 FROM agent_message_request_claims completed
		        WHERE completed.request_id=r.id AND completed.state='completed'
		      )
		      AND NOT EXISTS (
		        SELECT 1 FROM agent_message_request_claims active
		        WHERE active.request_id=r.id AND active.state IN ('reserved','claimed')
		          AND active.lease_expires_at>clock_timestamp()
		      )
		    )
		  )
		ORDER BY r.expires_at,r.id
		LIMIT $3
		FOR UPDATE OF r SKIP LOCKED`, accountID, realmID, limit)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("select message request reconcile batch: %w", err)
	}
	requestIDs := make([]string, 0, limit)
	for rows.Next() {
		var requestID string
		if err := rows.Scan(&requestID); err != nil {
			rows.Close()
			return 0, 0, 0, fmt.Errorf("scan message request reconcile batch: %w", err)
		}
		requestIDs = append(requestIDs, requestID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, 0, fmt.Errorf("read message request reconcile batch: %w", err)
	}
	rows.Close()

	var expired, completed int64
	for _, requestID := range requestIDs {
		requestExpired, requestCompleted, err := reconcileMessageRequestsTx(
			ctx, tx, accountID, realmID, requestID,
		)
		if err != nil {
			return 0, 0, 0, err
		}
		expired += requestExpired
		completed += requestCompleted
	}
	return len(requestIDs), expired, completed, nil
}

func drainMessageRequestReconciliationTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
) (int64, int64, error) {
	var expired, completed int64
	for {
		processed, batchExpired, batchCompleted, err := reconcileMessageRequestBatchTx(
			ctx, tx, accountID, "", maxMessageRequestReconcileBatch,
		)
		if err != nil {
			return 0, 0, err
		}
		expired += batchExpired
		completed += batchCompleted
		if processed < maxMessageRequestReconcileBatch {
			return expired, completed, nil
		}
	}
}

// commitDueMessageRequestExpiryTx persists a deadline observed while holding
// the authorized request row and also settles a successful batch whose final
// sibling lease disappeared. Callers return their operation-specific conflict
// only after this transaction has committed the terminal state.
func commitDueMessageRequestExpiryTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	requestID string,
	state string,
	expiresAt time.Time,
	now time.Time,
) (bool, error) {
	if state != MessageRequestStateOpen {
		return false, nil
	}
	expired, completed, err := reconcileMessageRequestsTx(ctx, tx, p.AccountID, p.RealmID, requestID)
	if err != nil {
		return false, err
	}
	if expired == 0 && completed == 0 && expiresAt.After(now) {
		return false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func commitLostMessageRequestFenceTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	requestID string,
) error {
	if _, _, err := reconcileMessageRequestsTx(ctx, tx, p.AccountID, p.RealmID, requestID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// cancelMessageRequestsForDeletedAgentTx removes every open-request authority
// that depends on an agent being live. The caller must hold that agent row in
// the account -> agent -> request lock order used by DeleteAgent.
func cancelMessageRequestsForDeletedAgentTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
	realmID string,
	agentID string,
) error {
	// Lock the affected request set in stable order before touching its child
	// rows. Candidate selection may finish first, but this cleanup then sees and
	// cancels the newly-created reservation in the same deletion transaction.
	rows, err := tx.Query(ctx, `
		SELECT r.id
		FROM agent_message_requests r
		WHERE r.account_id=$1 AND r.realm_id=$2 AND r.state='open'
		  AND (
		    r.coordinator_agent_id=$3 OR
		    EXISTS (SELECT 1 FROM agent_message_request_candidates c
		      WHERE c.request_id=r.id AND c.agent_id=$3) OR
		    EXISTS (SELECT 1 FROM agent_message_request_claims c
		      WHERE c.request_id=r.id AND c.agent_id=$3)
		  )
		ORDER BY r.id
		FOR UPDATE OF r`, accountID, realmID, agentID)
	if err != nil {
		return fmt.Errorf("lock deleted agent message requests: %w", err)
	}
	affectedRequestIDs := make([]string, 0)
	for rows.Next() {
		var requestID string
		if err := rows.Scan(&requestID); err != nil {
			rows.Close()
			return fmt.Errorf("scan deleted agent message request: %w", err)
		}
		affectedRequestIDs = append(affectedRequestIDs, requestID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("lock deleted agent message request rows: %w", err)
	}
	rows.Close()

	for _, requestID := range affectedRequestIDs {
		if _, err := expireDueMessageRequestsTx(ctx, tx, accountID, realmID, requestID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE agent_message_request_candidates c
		SET response_state='declined',offer_message_id=NULL,
		    offer_key_hash='',offer_request_hash='',
		    responded_at=COALESCE(c.responded_at,clock_timestamp())
		FROM agent_message_requests r
		WHERE r.id=c.request_id AND r.account_id=$1 AND r.realm_id=$2
		  AND r.state='open' AND c.agent_id=$3
		  AND c.response_state='pending'`,
		accountID, realmID, agentID); err != nil {
		return fmt.Errorf("decline deleted agent message request candidates: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE agent_message_request_claims c
		SET state='cancelled',lease_expires_at=NULL,
		    cancelled_at=clock_timestamp(),updated_at=clock_timestamp()
		FROM agent_message_requests r
		WHERE r.id=c.request_id AND r.account_id=$1 AND r.realm_id=$2
		  AND r.state='open' AND c.agent_id=$3
		  AND c.state IN ('reserved','claimed')`, accountID, realmID, agentID); err != nil {
		return fmt.Errorf("cancel deleted agent message request claims: %w", err)
	}
	// A deleted coordinator terminally cancels every request it owns, including
	// live claims held by other agents.
	if _, err := tx.Exec(ctx, `
		UPDATE agent_message_request_claims c
		SET state='cancelled',lease_expires_at=NULL,
		    cancelled_at=clock_timestamp(),updated_at=clock_timestamp()
		FROM agent_message_requests r
		WHERE r.id=c.request_id AND r.account_id=$1 AND r.realm_id=$2
		  AND r.state='open' AND r.coordinator_agent_id=$3
		  AND c.state IN ('reserved','claimed')`, accountID, realmID, agentID); err != nil {
		return fmt.Errorf("cancel deleted coordinator message request claims: %w", err)
	}
	rows, err = tx.Query(ctx, `
		UPDATE agent_message_requests
		SET state='cancelled',cancelled_at=clock_timestamp(),updated_at=clock_timestamp()
		WHERE account_id=$1 AND realm_id=$2 AND coordinator_agent_id=$3
		  AND state='open'
		RETURNING id,opening_message_id,coordinator_agent_id,max_assignees`,
		accountID, realmID, agentID)
	if err != nil {
		return fmt.Errorf("cancel deleted coordinator message requests: %w", err)
	}
	type cancelledRequestAudit struct {
		requestID          string
		openingMessageID   string
		coordinatorAgentID string
		maxAssignees       int
	}
	cancelled := make([]cancelledRequestAudit, 0)
	for rows.Next() {
		var record cancelledRequestAudit
		if err := rows.Scan(
			&record.requestID, &record.openingMessageID,
			&record.coordinatorAgentID, &record.maxAssignees,
		); err != nil {
			rows.Close()
			return fmt.Errorf("scan cancelled coordinator message request: %w", err)
		}
		cancelled = append(cancelled, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("cancel deleted coordinator message request rows: %w", err)
	}
	rows.Close()
	for _, record := range cancelled {
		if err := logEventTx(ctx, tx, EventInput{
			AccountID: accountID, ActorKind: ActorSystem, Verb: VerbMessageRequestCancelledSystem,
			Metadata: map[string]any{
				"request_id": record.requestID, "opening_message_id": record.openingMessageID,
				"coordinator_agent_id": record.coordinatorAgentID,
				"max_assignees":        strconv.Itoa(record.maxAssignees),
				"reason_code":          "coordinator_deleted",
			},
		}); err != nil {
			return fmt.Errorf("log deleted coordinator message request cancellation: %w", err)
		}
	}
	// Assignee deletion may have removed the last live sibling from a batch
	// that already produced a result. Coordinator-owned requests were cancelled
	// above and therefore cannot be accidentally converted to completed here.
	for _, requestID := range affectedRequestIDs {
		if _, err := settleMessageRequestClaimsTx(ctx, tx, accountID, realmID, requestID); err != nil {
			return err
		}
	}
	return nil
}

// OfferMessageRequest records one idempotent ordinary kind=offer reply from a
// candidate during the bounded offer window.
func (s *Store) OfferMessageRequest(ctx context.Context, p Principal, requestID string, in OfferMessageRequestInput) (OfferMessageRequestResult, error) {
	if p.Kind != PrincipalAgent {
		return OfferMessageRequestResult{}, ErrMessageRequestForbidden
	}
	var err error
	requestID, err = normalizeMessageRequestID(requestID)
	if err != nil {
		return OfferMessageRequestResult{}, err
	}
	draft, err := normalizeSendMessageInput(SendMessageInput{
		ToAgent: "coordinator", Subject: in.Subject, Kind: "offer", Body: in.Body,
		Payload: in.Payload, IdempotencyKey: in.IdempotencyKey,
	})
	if err != nil {
		return OfferMessageRequestResult{}, fmt.Errorf("%w: %v", ErrMessageRequestInputInvalid, err)
	}
	keyHash, err := normalizeMessageProcessingKey(draft.IdempotencyKey, "offer")
	if err != nil {
		return OfferMessageRequestResult{}, fmt.Errorf("%w: %v", ErrMessageRequestInputInvalid, err)
	}
	offerHash, err := messageRequestMutationHash(struct {
		Subject string          `json:"subject"`
		Body    string          `json:"body"`
		Payload json.RawMessage `json:"payload,omitempty"`
	}{draft.Subject, draft.Body, draft.Payload})
	if err != nil {
		return OfferMessageRequestResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OfferMessageRequestResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return OfferMessageRequestResult{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return OfferMessageRequestResult{}, err
	}
	coordinator, err := lockMessageRequestCandidateCoordinatorTx(ctx, tx, p, requestID)
	if err != nil {
		return OfferMessageRequestResult{}, err
	}
	locked, err := lockMessageRequestCandidateTx(ctx, tx, p, requestID)
	if err != nil {
		return OfferMessageRequestResult{}, err
	}
	if coordinator.ID != locked.coordinator.ID {
		return OfferMessageRequestResult{}, ErrMessageRequestConflict
	}
	if locked.responseState == MessageRequestCandidateOffered {
		if locked.offerKeyHash != keyHash || locked.offerRequestHash != offerHash {
			return OfferMessageRequestResult{}, ErrMessageRequestConflict
		}
		message, err := messageByScopedID(ctx, tx, p.AccountID, p.RealmID, locked.offerMessageID, true)
		if err != nil {
			return OfferMessageRequestResult{}, fmt.Errorf("read message request offer replay: %w", err)
		}
		request, err := getMessageRequestByIDTx(ctx, tx, p, requestID, true)
		if err != nil {
			return OfferMessageRequestResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return OfferMessageRequestResult{}, err
		}
		return OfferMessageRequestResult{Request: request, Offer: MessageRequestOffer{
			Agent: MessageAgent{ID: p.ID, Name: p.AgentName}, Message: redactMessageProcessingFence(message), OfferedAt: message.CreatedAt,
		}}, nil
	}
	if locked.responseState == MessageRequestCandidateDeclined {
		return OfferMessageRequestResult{}, ErrMessageRequestConflict
	}
	if locked.state != MessageRequestStateOpen {
		return OfferMessageRequestResult{}, ErrMessageRequestConflict
	}
	if expired, err := commitDueMessageRequestExpiryTx(
		ctx, tx, p, requestID, locked.state, locked.expiresAt, locked.now,
	); err != nil {
		return OfferMessageRequestResult{}, err
	} else if expired {
		return OfferMessageRequestResult{}, ErrMessageRequestConflict
	}
	if !locked.offerDeadline.After(locked.now) {
		return OfferMessageRequestResult{}, ErrMessageRequestConflict
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT message_request_offer`); err != nil {
		return OfferMessageRequestResult{}, fmt.Errorf("save message request offer point: %w", err)
	}
	draft.ToAgent = coordinator.ID
	draft.ThreadID = locked.threadID
	draft.ReplyToMessageID = locked.openingMessageID
	message, err := s.insertMessageTx(ctx, tx, p, coordinator, draft)
	if err != nil {
		if errors.Is(err, ErrMessageConflict) {
			return OfferMessageRequestResult{}, ErrMessageRequestConflict
		}
		return OfferMessageRequestResult{}, err
	}
	offered, err := tx.Exec(ctx, `
		UPDATE agent_message_request_candidates c
		SET response_state='offered', offer_message_id=$4, offer_key_hash=$5,
		    offer_request_hash=$6, responded_at=clock_timestamp()
		FROM agent_message_requests r
		WHERE c.request_id=$1 AND c.account_id=$2 AND c.realm_id=$3 AND c.agent_id=$7
		  AND c.response_state='pending'
		  AND r.id=c.request_id AND r.account_id=c.account_id AND r.realm_id=c.realm_id
		  AND r.state='open' AND r.offer_deadline>clock_timestamp()
		  AND r.expires_at>clock_timestamp()`,
		requestID, p.AccountID, p.RealmID, message.ID, keyHash, offerHash, p.ID)
	if err != nil {
		return OfferMessageRequestResult{}, fmt.Errorf("record message request offer: %w", err)
	}
	if offered.RowsAffected() != 1 {
		if _, rollbackErr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT message_request_offer`); rollbackErr != nil {
			return OfferMessageRequestResult{}, fmt.Errorf("rollback stale message request offer: %w", rollbackErr)
		}
		if err := commitLostMessageRequestFenceTx(ctx, tx, p, requestID); err != nil {
			return OfferMessageRequestResult{}, err
		}
		return OfferMessageRequestResult{}, ErrMessageRequestConflict
	}
	if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT message_request_offer`); err != nil {
		return OfferMessageRequestResult{}, fmt.Errorf("release message request offer point: %w", err)
	}
	if err := logMessageRequestAudit(ctx, tx, p, VerbMessageRequestOffered, map[string]any{
		"request_id": requestID, "opening_message_id": locked.openingMessageID,
		"coordinator_agent_id": locked.coordinator.ID, "agent_id": p.ID,
	}); err != nil {
		return OfferMessageRequestResult{}, err
	}
	request, err := getMessageRequestByIDTx(ctx, tx, p, requestID, true)
	if err != nil {
		return OfferMessageRequestResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return OfferMessageRequestResult{}, err
	}
	return OfferMessageRequestResult{Request: request, Offer: MessageRequestOffer{
		Agent: MessageAgent{ID: p.ID, Name: p.AgentName}, Message: redactMessageProcessingFence(message), OfferedAt: message.CreatedAt,
	}}, nil
}

// DeclineMessageRequest records a terminal candidate response. The operation
// is naturally idempotent because there is only one response row per candidate.
func (s *Store) DeclineMessageRequest(ctx context.Context, p Principal, requestID string, _ DeclineMessageRequestInput) (MessageRequest, error) {
	if p.Kind != PrincipalAgent {
		return MessageRequest{}, ErrMessageRequestForbidden
	}
	var err error
	requestID, err = normalizeMessageRequestID(requestID)
	if err != nil {
		return MessageRequest{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MessageRequest{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return MessageRequest{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return MessageRequest{}, err
	}
	locked, err := lockMessageRequestCandidateTx(ctx, tx, p, requestID)
	if err != nil {
		return MessageRequest{}, err
	}
	if locked.responseState == MessageRequestCandidateOffered {
		return MessageRequest{}, ErrMessageRequestConflict
	}
	if locked.responseState == MessageRequestCandidatePending {
		if expired, err := commitDueMessageRequestExpiryTx(
			ctx, tx, p, requestID, locked.state, locked.expiresAt, locked.now,
		); err != nil {
			return MessageRequest{}, err
		} else if expired {
			return MessageRequest{}, ErrMessageRequestConflict
		}
		if locked.state != MessageRequestStateOpen || !locked.offerDeadline.After(locked.now) {
			return MessageRequest{}, ErrMessageRequestConflict
		}
		declined, err := tx.Exec(ctx, `
			UPDATE agent_message_request_candidates c
			SET response_state='declined', responded_at=clock_timestamp()
			FROM agent_message_requests r
			WHERE c.request_id=$1 AND c.account_id=$2 AND c.realm_id=$3 AND c.agent_id=$4
			  AND c.response_state='pending'
			  AND r.id=c.request_id AND r.account_id=c.account_id AND r.realm_id=c.realm_id
			  AND r.state='open' AND r.offer_deadline>clock_timestamp()
			  AND r.expires_at>clock_timestamp()`, requestID, p.AccountID, p.RealmID, p.ID)
		if err != nil {
			return MessageRequest{}, fmt.Errorf("decline message request: %w", err)
		}
		if declined.RowsAffected() != 1 {
			if err := commitLostMessageRequestFenceTx(ctx, tx, p, requestID); err != nil {
				return MessageRequest{}, err
			}
			return MessageRequest{}, ErrMessageRequestConflict
		}
		if err := logMessageRequestAudit(ctx, tx, p, VerbMessageRequestDeclined, map[string]any{
			"request_id": requestID, "opening_message_id": locked.openingMessageID,
			"coordinator_agent_id": locked.coordinator.ID, "agent_id": p.ID,
		}); err != nil {
			return MessageRequest{}, err
		}
	}
	request, err := getMessageRequestByIDTx(ctx, tx, p, requestID, true)
	if err != nil {
		return MessageRequest{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MessageRequest{}, err
	}
	return request, nil
}

type lockedMessageRequest struct {
	coordinator         MessageAgent
	state               string
	openingMessageID    string
	threadID            string
	maxAssignees        int
	selectionGeneration int64
	expiresAt           time.Time
	now                 time.Time
}

func lockCoordinatorMessageRequestTx(ctx context.Context, tx pgx.Tx, p Principal, requestID string) (lockedMessageRequest, error) {
	var locked lockedMessageRequest
	err := tx.QueryRow(ctx, `
		SELECT r.coordinator_agent_id, a.name, r.state, r.opening_message_id,
		       opening.thread_id, r.max_assignees, r.selection_generation,
		       r.expires_at, clock_timestamp()
		FROM agent_message_requests r
		JOIN agents a ON a.id=r.coordinator_agent_id
		JOIN agent_messages opening ON opening.id=r.opening_message_id
		WHERE r.id=$1 AND r.account_id=$2 AND r.realm_id=$3
		  AND r.coordinator_agent_id=$4
		FOR UPDATE OF r`, requestID, p.AccountID, p.RealmID, p.ID).Scan(
		&locked.coordinator.ID, &locked.coordinator.Name, &locked.state,
		&locked.openingMessageID, &locked.threadID, &locked.maxAssignees,
		&locked.selectionGeneration, &locked.expiresAt, &locked.now,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedMessageRequest{}, ErrMessageRequestNotFound
	}
	if err != nil {
		return lockedMessageRequest{}, fmt.Errorf("lock coordinator message request: %w", err)
	}
	return locked, nil
}

func messageRequestClaimSelect() string {
	return `
		SELECT c.id, c.request_id, c.selection_id, c.agent_id, a.name,
		       c.state, c.generation, c.failure_count, c.lease_expires_at,
		       COALESCE(c.result_message_id,''), c.selected_at, c.claimed_at,
		       c.released_at, c.completed_at, c.cancelled_at, c.updated_at,
		       c.claim_key_hash, c.complete_key_hash
		FROM agent_message_request_claims c
		JOIN agents a ON a.id=c.agent_id
	`
}

func scanMessageRequestClaim(row rowScanner) (MessageRequestClaim, error) {
	var claim MessageRequestClaim
	if err := row.Scan(
		&claim.ClaimID, &claim.RequestID, &claim.SelectionID,
		&claim.Agent.ID, &claim.Agent.Name, &claim.State, &claim.Generation,
		&claim.FailureCount, &claim.LeaseExpiresAt, &claim.ResultMessageID,
		&claim.SelectedAt, &claim.ClaimedAt, &claim.ReleasedAt,
		&claim.CompletedAt, &claim.CancelledAt, &claim.UpdatedAt,
		&claim.claimKeyHash, &claim.completeKeyHash,
	); err != nil {
		return MessageRequestClaim{}, err
	}
	return claim, nil
}

func loadMessageRequestClaimsBySelectionTx(ctx context.Context, tx pgx.Tx, p Principal, requestID, selectionID string) ([]MessageRequestClaim, error) {
	rows, err := tx.Query(ctx, messageRequestClaimSelect()+`
		WHERE c.request_id=$1 AND c.selection_id=$2
		  AND c.account_id=$3 AND c.realm_id=$4
		ORDER BY c.agent_id`, requestID, selectionID, p.AccountID, p.RealmID)
	if err != nil {
		return nil, fmt.Errorf("list selected message request claims: %w", err)
	}
	defer rows.Close()
	claims := make([]MessageRequestClaim, 0)
	for rows.Next() {
		claim, err := scanMessageRequestClaim(rows)
		if err != nil {
			return nil, fmt.Errorf("scan selected message request claim: %w", err)
		}
		claims = append(claims, claim)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list selected message request claims: %w", err)
	}
	return claims, nil
}

// SelectMessageRequest persists one immutable client-ranked decision and
// creates bounded reservations atomically. The backend validates the choice;
// it never ranks or chooses a candidate.
func (s *Store) SelectMessageRequest(ctx context.Context, p Principal, requestID string, in SelectMessageRequestInput) (SelectMessageRequestResult, error) {
	if p.Kind != PrincipalAgent {
		return SelectMessageRequestResult{}, ErrMessageRequestForbidden
	}
	var err error
	requestID, err = normalizeMessageRequestID(requestID)
	if err != nil {
		return SelectMessageRequestResult{}, err
	}
	selected, err := normalizeSelectedAgentIDs(in.SelectedAgentIDs)
	if err != nil {
		return SelectMessageRequestResult{}, err
	}
	reservation, err := normalizeMessageLeaseDuration(in.Reservation)
	if err != nil {
		return SelectMessageRequestResult{}, fmt.Errorf("%w: %v", ErrMessageRequestInputInvalid, err)
	}
	keyHash, err := normalizeMessageProcessingKey(in.IdempotencyKey, "selection")
	if err != nil {
		return SelectMessageRequestResult{}, fmt.Errorf("%w: %v", ErrMessageRequestInputInvalid, err)
	}
	selectionHash, err := messageRequestMutationHash(struct {
		Agents             []string `json:"selected_agent_ids"`
		ReservationSeconds int64    `json:"reservation_seconds"`
	}{selected, int64(reservation / time.Second)})
	if err != nil {
		return SelectMessageRequestResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SelectMessageRequestResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return SelectMessageRequestResult{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return SelectMessageRequestResult{}, err
	}
	locked, err := lockCoordinatorMessageRequestTx(ctx, tx, p, requestID)
	if err != nil {
		return SelectMessageRequestResult{}, err
	}
	var replay MessageRequestSelection
	var replayHash string
	err = tx.QueryRow(ctx, `
		SELECT s.id,s.generation,s.coordinator_agent_id,a.name,s.selection_hash,s.created_at
		FROM agent_message_request_selections s
		JOIN agents a ON a.id=s.coordinator_agent_id
		WHERE s.request_id=$1 AND s.idempotency_key_hash=$2`, requestID, keyHash).Scan(
		&replay.ID, &replay.Generation, &replay.Coordinator.ID,
		&replay.Coordinator.Name, &replayHash, &replay.CreatedAt)
	if err == nil {
		if replayHash != selectionHash {
			return SelectMessageRequestResult{}, ErrMessageRequestConflict
		}
		claims, err := loadMessageRequestClaimsBySelectionTx(ctx, tx, p, requestID, replay.ID)
		if err != nil {
			return SelectMessageRequestResult{}, err
		}
		replay.SelectedAgentIDs = make([]string, len(claims))
		for i := range claims {
			replay.SelectedAgentIDs[i] = claims[i].Agent.ID
		}
		request, err := getMessageRequestByIDTx(ctx, tx, p, requestID, true)
		if err != nil {
			return SelectMessageRequestResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return SelectMessageRequestResult{}, err
		}
		return SelectMessageRequestResult{Request: request, Selection: replay, Claims: claims}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return SelectMessageRequestResult{}, fmt.Errorf("find message request selection replay: %w", err)
	}
	if expired, err := commitDueMessageRequestExpiryTx(
		ctx, tx, p, requestID, locked.state, locked.expiresAt, locked.now,
	); err != nil {
		return SelectMessageRequestResult{}, err
	} else if expired {
		return SelectMessageRequestResult{}, ErrMessageRequestConflict
	}
	if locked.state != MessageRequestStateOpen {
		return SelectMessageRequestResult{}, ErrMessageRequestConflict
	}
	if locked.selectionGeneration >= maxMessageRequestSelectionHistory {
		return SelectMessageRequestResult{}, ErrMessageRequestConflict
	}
	if len(selected) > locked.maxAssignees {
		return SelectMessageRequestResult{}, ErrMessageRequestConflict
	}
	var validOffers int
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		FROM agent_message_request_candidates c
		JOIN agents a ON a.id=c.agent_id
		WHERE c.request_id=$1 AND c.agent_id=ANY($2::text[])
		  AND c.response_state='offered' AND a.deleted_at IS NULL`, requestID, selected).Scan(&validOffers); err != nil {
		return SelectMessageRequestResult{}, fmt.Errorf("validate selected message request offers: %w", err)
	}
	if validOffers != len(selected) {
		return SelectMessageRequestResult{}, ErrMessageRequestConflict
	}
	var usedCapacity int
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		FROM agent_message_request_claims
		WHERE request_id=$1 AND (
		  state='completed' OR
		  (state IN ('reserved','claimed') AND lease_expires_at>clock_timestamp())
		)`, requestID).Scan(&usedCapacity); err != nil {
		return SelectMessageRequestResult{}, fmt.Errorf("count message request capacity: %w", err)
	}
	if usedCapacity+len(selected) > locked.maxAssignees {
		return SelectMessageRequestResult{}, ErrMessageRequestConflict
	}
	var alreadyAssigned int
	if err := tx.QueryRow(ctx, `
		SELECT count(DISTINCT agent_id)
		FROM agent_message_request_claims
		WHERE request_id=$1 AND agent_id=ANY($2::text[]) AND (
		  state='completed' OR
		  (state IN ('reserved','claimed') AND lease_expires_at>clock_timestamp())
		)`, requestID, selected).Scan(&alreadyAssigned); err != nil {
		return SelectMessageRequestResult{}, fmt.Errorf("check selected message request agents: %w", err)
	}
	if alreadyAssigned != 0 {
		return SelectMessageRequestResult{}, ErrMessageRequestConflict
	}
	selectionID, err := id.New("msel")
	if err != nil {
		return SelectMessageRequestResult{}, err
	}
	generation := locked.selectionGeneration + 1
	if generation < 1 || generation > maxMessageProcessingGeneration {
		return SelectMessageRequestResult{}, ErrMessageRequestConflict
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT message_request_selection`); err != nil {
		return SelectMessageRequestResult{}, fmt.Errorf("save message request selection point: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_message_request_selections
		  (id,request_id,account_id,realm_id,coordinator_agent_id,generation,
		   idempotency_key_hash,selection_hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, selectionID, requestID,
		p.AccountID, p.RealmID, p.ID, generation, keyHash, selectionHash); err != nil {
		return SelectMessageRequestResult{}, fmt.Errorf("insert message request selection: %w", err)
	}
	for _, agentID := range selected {
		claimID, err := id.New("mrc")
		if err != nil {
			return SelectMessageRequestResult{}, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO agent_message_request_claims
			  (id,request_id,selection_id,account_id,realm_id,agent_id,lease_expires_at)
			SELECT $1,$2,$3,$4,$5,$6,
			  LEAST(r.expires_at,clock_timestamp()+($7::bigint*interval '1 second'))
			FROM agent_message_requests r WHERE r.id=$2`, claimID, requestID, selectionID,
			p.AccountID, p.RealmID, agentID, int64(reservation/time.Second)); err != nil {
			return SelectMessageRequestResult{}, fmt.Errorf("reserve message request claim: %w", err)
		}
		if err := logMessageRequestAudit(ctx, tx, p, VerbMessageRequestSelected, map[string]any{
			"request_id": requestID, "opening_message_id": locked.openingMessageID,
			"coordinator_agent_id": locked.coordinator.ID, "agent_id": agentID,
			"selection_id": selectionID, "generation": strconv.FormatInt(generation, 10),
			"max_assignees": strconv.Itoa(locked.maxAssignees),
		}); err != nil {
			return SelectMessageRequestResult{}, err
		}
	}
	advanced, err := tx.Exec(ctx, `
		UPDATE agent_message_requests
		SET selection_generation=$2,updated_at=clock_timestamp()
		WHERE id=$1 AND account_id=$3 AND realm_id=$4 AND coordinator_agent_id=$5
		  AND state='open' AND expires_at>clock_timestamp()
		  AND (offer_deadline<=clock_timestamp() OR NOT EXISTS (
		    SELECT 1 FROM agent_message_request_candidates pending
		    WHERE pending.request_id=agent_message_requests.id
		      AND pending.response_state='pending'
		  ))
		  AND selection_generation=$6`, requestID, generation, p.AccountID,
		p.RealmID, p.ID, locked.selectionGeneration)
	if err != nil {
		return SelectMessageRequestResult{}, fmt.Errorf("advance message request selection: %w", err)
	}
	if advanced.RowsAffected() != 1 {
		if _, rollbackErr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT message_request_selection`); rollbackErr != nil {
			return SelectMessageRequestResult{}, fmt.Errorf("rollback stale message request selection: %w", rollbackErr)
		}
		if err := commitLostMessageRequestFenceTx(ctx, tx, p, requestID); err != nil {
			return SelectMessageRequestResult{}, err
		}
		return SelectMessageRequestResult{}, ErrMessageRequestConflict
	}
	if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT message_request_selection`); err != nil {
		return SelectMessageRequestResult{}, fmt.Errorf("release message request selection point: %w", err)
	}
	claims, err := loadMessageRequestClaimsBySelectionTx(ctx, tx, p, requestID, selectionID)
	if err != nil {
		return SelectMessageRequestResult{}, err
	}
	request, err := getMessageRequestByIDTx(ctx, tx, p, requestID, true)
	if err != nil {
		return SelectMessageRequestResult{}, err
	}
	selection := MessageRequestSelection{
		ID: selectionID, Generation: generation, Coordinator: locked.coordinator,
		SelectedAgentIDs: selected, CreatedAt: claims[0].SelectedAt,
	}
	if err := tx.Commit(ctx); err != nil {
		return SelectMessageRequestResult{}, err
	}
	return SelectMessageRequestResult{Request: request, Selection: selection, Claims: claims}, nil
}

// CancelMessageRequest terminally closes an open request and invalidates every
// live reservation/claim. Repeating cancellation is idempotent.
func (s *Store) CancelMessageRequest(ctx context.Context, p Principal, requestID string) (MessageRequest, error) {
	if p.Kind != PrincipalAgent {
		return MessageRequest{}, ErrMessageRequestForbidden
	}
	var err error
	requestID, err = normalizeMessageRequestID(requestID)
	if err != nil {
		return MessageRequest{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MessageRequest{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return MessageRequest{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return MessageRequest{}, err
	}
	locked, err := lockCoordinatorMessageRequestTx(ctx, tx, p, requestID)
	if err != nil {
		return MessageRequest{}, err
	}
	if locked.state == MessageRequestStateCancelled {
		request, err := getMessageRequestByIDTx(ctx, tx, p, requestID, true)
		if err != nil {
			return MessageRequest{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return MessageRequest{}, err
		}
		return request, nil
	}
	if expired, err := commitDueMessageRequestExpiryTx(
		ctx, tx, p, requestID, locked.state, locked.expiresAt, locked.now,
	); err != nil {
		return MessageRequest{}, err
	} else if expired {
		return MessageRequest{}, ErrMessageRequestConflict
	}
	if locked.state != MessageRequestStateOpen {
		return MessageRequest{}, ErrMessageRequestConflict
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT message_request_cancellation`); err != nil {
		return MessageRequest{}, fmt.Errorf("save message request cancellation point: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE agent_message_request_claims
		SET state='cancelled', lease_expires_at=NULL,
		    cancelled_at=clock_timestamp(), updated_at=clock_timestamp()
		WHERE request_id=$1 AND state IN ('reserved','claimed')`, requestID); err != nil {
		return MessageRequest{}, fmt.Errorf("cancel message request claims: %w", err)
	}
	cancelled, err := tx.Exec(ctx, `
		UPDATE agent_message_requests
		SET state='cancelled',cancelled_at=clock_timestamp(),updated_at=clock_timestamp()
		WHERE id=$1 AND account_id=$2 AND realm_id=$3 AND coordinator_agent_id=$4
		  AND state='open' AND expires_at>clock_timestamp()`,
		requestID, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return MessageRequest{}, fmt.Errorf("cancel message request: %w", err)
	}
	if cancelled.RowsAffected() != 1 {
		if _, rollbackErr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT message_request_cancellation`); rollbackErr != nil {
			return MessageRequest{}, fmt.Errorf("rollback stale message request cancellation: %w", rollbackErr)
		}
		if err := commitLostMessageRequestFenceTx(ctx, tx, p, requestID); err != nil {
			return MessageRequest{}, err
		}
		return MessageRequest{}, ErrMessageRequestConflict
	}
	if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT message_request_cancellation`); err != nil {
		return MessageRequest{}, fmt.Errorf("release message request cancellation point: %w", err)
	}
	if err := logMessageRequestAudit(ctx, tx, p, VerbMessageRequestCancelled, map[string]any{
		"request_id": requestID, "opening_message_id": locked.openingMessageID,
		"coordinator_agent_id": locked.coordinator.ID, "max_assignees": strconv.Itoa(locked.maxAssignees),
	}); err != nil {
		return MessageRequest{}, err
	}
	request, err := getMessageRequestByIDTx(ctx, tx, p, requestID, true)
	if err != nil {
		return MessageRequest{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MessageRequest{}, err
	}
	return request, nil
}

type lockedMessageRequestClaim struct {
	claim            MessageRequestClaim
	requestState     string
	coordinator      MessageAgent
	openingMessageID string
	threadID         string
	expiresAt        time.Time
	maxAssignees     int
	now              time.Time
}

func lockMessageRequestClaimCoordinatorTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	requestID string,
	claimID string,
) (Agent, error) {
	var coordinator Agent
	err := tx.QueryRow(ctx, `
		SELECT coordinator.id,coordinator.name
		FROM agent_message_request_claims c
		JOIN agent_message_requests r ON r.id=c.request_id
		JOIN agents coordinator ON coordinator.id=r.coordinator_agent_id
		WHERE c.id=$1 AND c.request_id=$2 AND c.account_id=$3
		  AND c.realm_id=$4 AND c.agent_id=$5
		FOR SHARE OF coordinator`, claimID, requestID, p.AccountID, p.RealmID, p.ID).
		Scan(&coordinator.ID, &coordinator.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, ErrMessageRequestNotFound
	}
	if err != nil {
		return Agent{}, fmt.Errorf("lock message request coordinator: %w", err)
	}
	return coordinator, nil
}

func lockMessageRequestClaimTx(ctx context.Context, tx pgx.Tx, p Principal, requestID, claimID string) (lockedMessageRequestClaim, error) {
	query := `
		SELECT c.id, c.request_id, c.selection_id, c.agent_id, a.name,
		       c.state, c.generation, c.failure_count, c.lease_expires_at,
		       COALESCE(c.result_message_id,''), c.selected_at, c.claimed_at,
		       c.released_at, c.completed_at, c.cancelled_at, c.updated_at,
		       c.claim_key_hash, c.complete_key_hash,
		       r.state, r.coordinator_agent_id, coordinator.name,
		       r.opening_message_id, opening.thread_id, r.expires_at,
		       r.max_assignees, clock_timestamp()
		FROM agent_message_request_claims c
		JOIN agent_message_request_selections selection
		  ON selection.id=c.selection_id AND selection.request_id=c.request_id
		JOIN agents a ON a.id=c.agent_id
		JOIN agent_message_requests r ON r.id=c.request_id
		JOIN agents coordinator ON coordinator.id=r.coordinator_agent_id
		JOIN agent_messages opening ON opening.id=r.opening_message_id
		WHERE c.request_id=$1 AND c.account_id=$2 AND c.realm_id=$3
		  AND c.agent_id=$4`
	args := []any{requestID, p.AccountID, p.RealmID, p.ID}
	if claimID != "" {
		query += ` AND c.id=$5`
		args = append(args, claimID)
	}
	query += ` ORDER BY selection.generation DESC,c.id DESC LIMIT 1 FOR UPDATE OF r,c`
	var locked lockedMessageRequestClaim
	err := tx.QueryRow(ctx, query, args...).Scan(
		&locked.claim.ClaimID, &locked.claim.RequestID, &locked.claim.SelectionID,
		&locked.claim.Agent.ID, &locked.claim.Agent.Name, &locked.claim.State,
		&locked.claim.Generation, &locked.claim.FailureCount,
		&locked.claim.LeaseExpiresAt, &locked.claim.ResultMessageID,
		&locked.claim.SelectedAt, &locked.claim.ClaimedAt,
		&locked.claim.ReleasedAt, &locked.claim.CompletedAt,
		&locked.claim.CancelledAt, &locked.claim.UpdatedAt,
		&locked.claim.claimKeyHash, &locked.claim.completeKeyHash,
		&locked.requestState, &locked.coordinator.ID, &locked.coordinator.Name,
		&locked.openingMessageID, &locked.threadID, &locked.expiresAt,
		&locked.maxAssignees, &locked.now,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedMessageRequestClaim{}, ErrMessageRequestNotFound
	}
	if err != nil {
		return lockedMessageRequestClaim{}, fmt.Errorf("lock message request claim: %w", err)
	}
	return locked, nil
}

func messageRequestClaimByIDTx(ctx context.Context, tx pgx.Tx, p Principal, requestID, claimID string) (MessageRequestClaim, error) {
	claim, err := scanMessageRequestClaim(tx.QueryRow(ctx, messageRequestClaimSelect()+`
		WHERE c.id=$1 AND c.request_id=$2 AND c.account_id=$3
		  AND c.realm_id=$4 AND c.agent_id=$5`,
		claimID, requestID, p.AccountID, p.RealmID, p.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return MessageRequestClaim{}, ErrMessageRequestNotFound
	}
	if err != nil {
		return MessageRequestClaim{}, fmt.Errorf("read message request claim: %w", err)
	}
	return claim, nil
}

// ClaimMessageRequest converts the selected agent's latest live reservation
// into a fenced processing lease.
func (s *Store) ClaimMessageRequest(ctx context.Context, p Principal, requestID string, in ClaimMessageRequestInput) (MessageRequestClaim, error) {
	if p.Kind != PrincipalAgent {
		return MessageRequestClaim{}, ErrMessageRequestForbidden
	}
	var err error
	requestID, err = normalizeMessageRequestID(requestID)
	if err != nil {
		return MessageRequestClaim{}, err
	}
	lease, err := normalizeMessageLeaseDuration(in.LeaseDuration)
	if err != nil {
		return MessageRequestClaim{}, fmt.Errorf("%w: %v", ErrMessageRequestInputInvalid, err)
	}
	keyHash, err := normalizeMessageProcessingKey(in.IdempotencyKey, "claim")
	if err != nil {
		return MessageRequestClaim{}, fmt.Errorf("%w: %v", ErrMessageRequestInputInvalid, err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MessageRequestClaim{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return MessageRequestClaim{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return MessageRequestClaim{}, err
	}
	locked, err := lockMessageRequestClaimTx(ctx, tx, p, requestID, "")
	if err != nil {
		return MessageRequestClaim{}, err
	}
	if expired, err := commitDueMessageRequestExpiryTx(
		ctx, tx, p, requestID, locked.requestState, locked.expiresAt, locked.now,
	); err != nil {
		return MessageRequestClaim{}, err
	} else if expired {
		return MessageRequestClaim{}, ErrMessageRequestClaimLost
	}
	if locked.claim.State == MessageRequestClaimClaimed {
		if locked.claim.LeaseExpiresAt != nil && locked.claim.LeaseExpiresAt.After(locked.now) {
			if locked.claim.claimKeyHash != keyHash {
				return MessageRequestClaim{}, ErrMessageRequestBusy
			}
			if err := tx.Commit(ctx); err != nil {
				return MessageRequestClaim{}, err
			}
			return locked.claim, nil
		}
		return MessageRequestClaim{}, ErrMessageRequestClaimLost
	}
	if locked.claim.State != MessageRequestClaimReserved || locked.claim.LeaseExpiresAt == nil || !locked.claim.LeaseExpiresAt.After(locked.now) ||
		locked.requestState != MessageRequestStateOpen || !locked.expiresAt.After(locked.now) {
		return MessageRequestClaim{}, ErrMessageRequestClaimLost
	}
	claimed, err := tx.Exec(ctx, `
		UPDATE agent_message_request_claims c
		SET state='claimed',generation=1,claim_key_hash=$6,
		    lease_expires_at=LEAST(
		      r.expires_at, clock_timestamp()+($7::bigint*interval '1 second')
		    ),
		    claimed_at=clock_timestamp(),updated_at=clock_timestamp()
		FROM agent_message_requests r
		WHERE c.id=$1 AND c.request_id=$2 AND c.account_id=$3
		  AND c.realm_id=$4 AND c.agent_id=$5
		  AND c.state='reserved' AND c.generation=0
		  AND c.lease_expires_at>clock_timestamp()
		  AND r.id=c.request_id AND r.account_id=c.account_id AND r.realm_id=c.realm_id
		  AND r.state='open' AND r.expires_at>clock_timestamp()`,
		locked.claim.ClaimID, requestID, p.AccountID, p.RealmID, p.ID,
		keyHash, int64(lease/time.Second))
	if err != nil {
		return MessageRequestClaim{}, fmt.Errorf("claim message request: %w", err)
	}
	if claimed.RowsAffected() != 1 {
		if err := commitLostMessageRequestFenceTx(ctx, tx, p, requestID); err != nil {
			return MessageRequestClaim{}, err
		}
		return MessageRequestClaim{}, ErrMessageRequestClaimLost
	}
	if err := logMessageRequestAudit(ctx, tx, p, VerbMessageRequestClaimed, map[string]any{
		"request_id": requestID, "opening_message_id": locked.openingMessageID,
		"coordinator_agent_id": locked.coordinator.ID, "agent_id": p.ID,
		"selection_id": locked.claim.SelectionID, "generation": "1",
		"failure_count": strconv.FormatInt(locked.claim.FailureCount, 10),
	}); err != nil {
		return MessageRequestClaim{}, err
	}
	claim, err := messageRequestClaimByIDTx(ctx, tx, p, requestID, locked.claim.ClaimID)
	if err != nil {
		return MessageRequestClaim{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MessageRequestClaim{}, err
	}
	return claim, nil
}

// RenewMessageRequest extends one exact, still-live generation.
func (s *Store) RenewMessageRequest(ctx context.Context, p Principal, requestID string, in RenewMessageRequestInput) (MessageRequestClaim, error) {
	if p.Kind != PrincipalAgent {
		return MessageRequestClaim{}, ErrMessageRequestForbidden
	}
	var err error
	requestID, err = normalizeMessageRequestID(requestID)
	if err != nil {
		return MessageRequestClaim{}, err
	}
	claimID, generation, err := normalizeMessageRequestClaimFence(in.ClaimID, in.Generation)
	if err != nil {
		return MessageRequestClaim{}, err
	}
	lease, err := normalizeMessageLeaseDuration(in.LeaseDuration)
	if err != nil {
		return MessageRequestClaim{}, fmt.Errorf("%w: %v", ErrMessageRequestInputInvalid, err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MessageRequestClaim{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return MessageRequestClaim{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return MessageRequestClaim{}, err
	}
	locked, err := lockMessageRequestClaimTx(ctx, tx, p, requestID, claimID)
	if err != nil {
		return MessageRequestClaim{}, err
	}
	if expired, err := commitDueMessageRequestExpiryTx(
		ctx, tx, p, requestID, locked.requestState, locked.expiresAt, locked.now,
	); err != nil {
		return MessageRequestClaim{}, err
	} else if expired {
		return MessageRequestClaim{}, ErrMessageRequestClaimLost
	}
	if locked.claim.State != MessageRequestClaimClaimed || locked.claim.Generation != generation ||
		locked.claim.LeaseExpiresAt == nil || !locked.claim.LeaseExpiresAt.After(locked.now) ||
		locked.requestState != MessageRequestStateOpen || !locked.expiresAt.After(locked.now) {
		return MessageRequestClaim{}, ErrMessageRequestClaimLost
	}
	renewed, err := tx.Exec(ctx, `
		UPDATE agent_message_request_claims c
		SET lease_expires_at=LEAST(
		      r.expires_at, clock_timestamp()+($7::bigint*interval '1 second')
		    ),
		    updated_at=clock_timestamp()
		FROM agent_message_requests r
		WHERE c.id=$1 AND c.request_id=$2 AND c.account_id=$3
		  AND c.realm_id=$4 AND c.agent_id=$5 AND c.generation=$6
		  AND c.state='claimed' AND c.lease_expires_at>clock_timestamp()
		  AND r.id=c.request_id AND r.account_id=c.account_id AND r.realm_id=c.realm_id
		  AND r.state='open' AND r.expires_at>clock_timestamp()`,
		claimID, requestID, p.AccountID, p.RealmID, p.ID, generation,
		int64(lease/time.Second))
	if err != nil {
		return MessageRequestClaim{}, fmt.Errorf("renew message request claim: %w", err)
	}
	if renewed.RowsAffected() != 1 {
		if err := commitLostMessageRequestFenceTx(ctx, tx, p, requestID); err != nil {
			return MessageRequestClaim{}, err
		}
		return MessageRequestClaim{}, ErrMessageRequestClaimLost
	}
	if err := logMessageRequestAudit(ctx, tx, p, VerbMessageRequestRenewed, map[string]any{
		"request_id": requestID, "opening_message_id": locked.openingMessageID,
		"coordinator_agent_id": locked.coordinator.ID, "agent_id": p.ID,
		"selection_id": locked.claim.SelectionID, "generation": strconv.FormatInt(generation, 10),
		"failure_count": strconv.FormatInt(locked.claim.FailureCount, 10),
	}); err != nil {
		return MessageRequestClaim{}, err
	}
	claim, err := messageRequestClaimByIDTx(ctx, tx, p, requestID, claimID)
	if err != nil {
		return MessageRequestClaim{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MessageRequestClaim{}, err
	}
	return claim, nil
}

// ReleaseMessageRequest gives up one exact live claim. Only deterministic
// request-local failures increment the poison counter.
func (s *Store) ReleaseMessageRequest(ctx context.Context, p Principal, requestID string, in ReleaseMessageRequestInput) (MessageRequestClaim, error) {
	if p.Kind != PrincipalAgent {
		return MessageRequestClaim{}, ErrMessageRequestForbidden
	}
	var err error
	requestID, err = normalizeMessageRequestID(requestID)
	if err != nil {
		return MessageRequestClaim{}, err
	}
	claimID, generation, err := normalizeMessageRequestClaimFence(in.ClaimID, in.Generation)
	if err != nil {
		return MessageRequestClaim{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MessageRequestClaim{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return MessageRequestClaim{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return MessageRequestClaim{}, err
	}
	locked, err := lockMessageRequestClaimTx(ctx, tx, p, requestID, claimID)
	if err != nil {
		return MessageRequestClaim{}, err
	}
	if expired, err := commitDueMessageRequestExpiryTx(
		ctx, tx, p, requestID, locked.requestState, locked.expiresAt, locked.now,
	); err != nil {
		return MessageRequestClaim{}, err
	} else if expired {
		return MessageRequestClaim{}, ErrMessageRequestClaimLost
	}
	if locked.claim.State != MessageRequestClaimClaimed || locked.claim.Generation != generation ||
		locked.claim.LeaseExpiresAt == nil || !locked.claim.LeaseExpiresAt.After(locked.now) ||
		locked.requestState != MessageRequestStateOpen || !locked.expiresAt.After(locked.now) {
		return MessageRequestClaim{}, ErrMessageRequestClaimLost
	}
	failureIncrement := 0
	if in.DeterministicFailure {
		failureIncrement = 1
		if locked.claim.FailureCount >= maxMessageFailureCount {
			return MessageRequestClaim{}, ErrMessageRequestConflict
		}
	}
	released, err := tx.Exec(ctx, `
		UPDATE agent_message_request_claims c
		SET state='released',lease_expires_at=NULL,released_at=clock_timestamp(),
		    failure_count=failure_count+$7,updated_at=clock_timestamp()
		FROM agent_message_requests r
		WHERE c.id=$1 AND c.request_id=$2 AND c.account_id=$3
		  AND c.realm_id=$4 AND c.agent_id=$5 AND c.generation=$6
		  AND c.state='claimed' AND c.lease_expires_at>clock_timestamp()
		  AND r.id=c.request_id AND r.account_id=c.account_id AND r.realm_id=c.realm_id
		  AND r.state='open' AND r.expires_at>clock_timestamp()`,
		claimID, requestID, p.AccountID, p.RealmID, p.ID, generation, failureIncrement)
	if err != nil {
		return MessageRequestClaim{}, fmt.Errorf("release message request claim: %w", err)
	}
	if released.RowsAffected() != 1 {
		if err := commitLostMessageRequestFenceTx(ctx, tx, p, requestID); err != nil {
			return MessageRequestClaim{}, err
		}
		return MessageRequestClaim{}, ErrMessageRequestClaimLost
	}
	if err := logMessageRequestAudit(ctx, tx, p, VerbMessageRequestReleased, map[string]any{
		"request_id": requestID, "opening_message_id": locked.openingMessageID,
		"coordinator_agent_id": locked.coordinator.ID, "agent_id": p.ID,
		"selection_id": locked.claim.SelectionID, "generation": strconv.FormatInt(generation, 10),
		"failure_count": strconv.FormatInt(locked.claim.FailureCount+int64(failureIncrement), 10),
	}); err != nil {
		return MessageRequestClaim{}, err
	}
	if _, err := settleMessageRequestClaimsTx(ctx, tx, p.AccountID, p.RealmID, requestID); err != nil {
		return MessageRequestClaim{}, err
	}
	claim, err := messageRequestClaimByIDTx(ctx, tx, p, requestID, claimID)
	if err != nil {
		return MessageRequestClaim{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MessageRequestClaim{}, err
	}
	return claim, nil
}

// CompleteMessageRequest atomically publishes one server-routed result reply,
// closes the exact claim fence, and completes the request when its currently
// selected batch has no other live reservation or claim. MaxAssignees is a
// capacity ceiling, not a required result count.
func (s *Store) CompleteMessageRequest(ctx context.Context, p Principal, requestID string, in CompleteMessageRequestInput) (CompleteMessageRequestResult, error) {
	if p.Kind != PrincipalAgent {
		return CompleteMessageRequestResult{}, ErrMessageRequestForbidden
	}
	var err error
	requestID, err = normalizeMessageRequestID(requestID)
	if err != nil {
		return CompleteMessageRequestResult{}, err
	}
	claimID, generation, err := normalizeMessageRequestClaimFence(in.ClaimID, in.Generation)
	if err != nil {
		return CompleteMessageRequestResult{}, err
	}
	draft, err := normalizeSendMessageInput(SendMessageInput{
		ToAgent: "coordinator", Subject: in.Subject, Kind: "result", Body: in.Body,
		Payload: in.Payload, IdempotencyKey: in.IdempotencyKey,
	})
	if err != nil {
		return CompleteMessageRequestResult{}, fmt.Errorf("%w: %v", ErrMessageRequestInputInvalid, err)
	}
	completeKeyHash, err := normalizeMessageProcessingKey(draft.IdempotencyKey, "completion")
	if err != nil {
		return CompleteMessageRequestResult{}, fmt.Errorf("%w: %v", ErrMessageRequestInputInvalid, err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CompleteMessageRequestResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return CompleteMessageRequestResult{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return CompleteMessageRequestResult{}, err
	}
	coordinator, err := lockMessageRequestClaimCoordinatorTx(ctx, tx, p, requestID, claimID)
	if err != nil {
		return CompleteMessageRequestResult{}, err
	}
	locked, err := lockMessageRequestClaimTx(ctx, tx, p, requestID, claimID)
	if err != nil {
		return CompleteMessageRequestResult{}, err
	}
	if coordinator.ID != locked.coordinator.ID {
		return CompleteMessageRequestResult{}, ErrMessageRequestConflict
	}
	if locked.claim.Generation != generation {
		return CompleteMessageRequestResult{}, ErrMessageRequestClaimLost
	}
	if locked.claim.State == MessageRequestClaimCompleted {
		if locked.claim.completeKeyHash != completeKeyHash || locked.claim.ResultMessageID == "" {
			return CompleteMessageRequestResult{}, ErrMessageRequestConflict
		}
		message, err := messageByScopedID(ctx, tx, p.AccountID, p.RealmID, locked.claim.ResultMessageID, true)
		if err != nil {
			return CompleteMessageRequestResult{}, fmt.Errorf("read message request completion replay: %w", err)
		}
		if !messageMatchesCompletion(message, draft) {
			return CompleteMessageRequestResult{}, ErrMessageRequestConflict
		}
		request, err := getMessageRequestByIDTx(ctx, tx, p, requestID, true)
		if err != nil {
			return CompleteMessageRequestResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return CompleteMessageRequestResult{}, err
		}
		return CompleteMessageRequestResult{Request: request, Claim: locked.claim, Message: redactMessageProcessingFence(message)}, nil
	}
	if expired, err := commitDueMessageRequestExpiryTx(
		ctx, tx, p, requestID, locked.requestState, locked.expiresAt, locked.now,
	); err != nil {
		return CompleteMessageRequestResult{}, err
	} else if expired {
		return CompleteMessageRequestResult{}, ErrMessageRequestClaimLost
	}
	if locked.claim.State != MessageRequestClaimClaimed || locked.claim.LeaseExpiresAt == nil ||
		!locked.claim.LeaseExpiresAt.After(locked.now) || locked.requestState != MessageRequestStateOpen ||
		!locked.expiresAt.After(locked.now) {
		return CompleteMessageRequestResult{}, ErrMessageRequestClaimLost
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT message_request_completion`); err != nil {
		return CompleteMessageRequestResult{}, fmt.Errorf("save message request completion point: %w", err)
	}
	draft.ToAgent = coordinator.ID
	draft.ThreadID = locked.threadID
	draft.ReplyToMessageID = locked.openingMessageID
	message, err := s.insertMessageTx(ctx, tx, p, coordinator, draft)
	if err != nil {
		if errors.Is(err, ErrMessageConflict) {
			return CompleteMessageRequestResult{}, ErrMessageRequestConflict
		}
		return CompleteMessageRequestResult{}, err
	}
	var completedAt time.Time
	err = tx.QueryRow(ctx, `
		UPDATE agent_message_request_claims c
		SET state='completed',lease_expires_at=NULL,complete_key_hash=$6,
		    result_message_id=$7,completed_at=clock_timestamp(),updated_at=clock_timestamp()
		FROM agent_message_requests r
		WHERE c.id=$1 AND c.request_id=$2 AND c.account_id=$3
		  AND c.realm_id=$4 AND c.agent_id=$5 AND c.generation=$8
		  AND c.state='claimed' AND c.lease_expires_at>clock_timestamp()
		  AND r.id=c.request_id AND r.account_id=c.account_id AND r.realm_id=c.realm_id
		  AND r.state='open' AND r.expires_at>clock_timestamp()
		RETURNING c.completed_at`, claimID, requestID, p.AccountID, p.RealmID,
		p.ID, completeKeyHash, message.ID, generation).Scan(&completedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, rollbackErr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT message_request_completion`); rollbackErr != nil {
			return CompleteMessageRequestResult{}, fmt.Errorf("rollback stale message request completion: %w", rollbackErr)
		}
		if err := commitLostMessageRequestFenceTx(ctx, tx, p, requestID); err != nil {
			return CompleteMessageRequestResult{}, err
		}
		return CompleteMessageRequestResult{}, ErrMessageRequestClaimLost
	}
	if err != nil {
		return CompleteMessageRequestResult{}, fmt.Errorf("complete message request claim: %w", err)
	}
	if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT message_request_completion`); err != nil {
		return CompleteMessageRequestResult{}, fmt.Errorf("release message request completion point: %w", err)
	}
	if err := logMessageRequestAudit(ctx, tx, p, VerbMessageRequestCompleted, map[string]any{
		"request_id": requestID, "opening_message_id": locked.openingMessageID,
		"coordinator_agent_id": locked.coordinator.ID, "agent_id": p.ID,
		"selection_id": locked.claim.SelectionID, "generation": strconv.FormatInt(generation, 10),
		"failure_count":     strconv.FormatInt(locked.claim.FailureCount, 10),
		"result_message_id": message.ID,
	}); err != nil {
		return CompleteMessageRequestResult{}, err
	}
	// Expired sibling reservations no longer represent outstanding work. Make
	// that durable before deciding whether this selected batch is finished.
	if _, err := tx.Exec(ctx, `
		UPDATE agent_message_request_claims
		SET state='cancelled',lease_expires_at=NULL,
		    cancelled_at=clock_timestamp(),updated_at=clock_timestamp()
		WHERE request_id=$1 AND state IN ('reserved','claimed')
		  AND lease_expires_at<=clock_timestamp()`, requestID); err != nil {
		return CompleteMessageRequestResult{}, fmt.Errorf("cancel expired sibling message request claims: %w", err)
	}
	var outstandingCount int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM agent_message_request_claims
		WHERE request_id=$1 AND state IN ('reserved','claimed')
		  AND lease_expires_at>clock_timestamp()`, requestID).Scan(&outstandingCount); err != nil {
		return CompleteMessageRequestResult{}, fmt.Errorf("count outstanding message request claims: %w", err)
	}
	if outstandingCount == 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE agent_message_requests
			SET state='completed',completed_at=$2,updated_at=$2
			WHERE id=$1 AND state='open' AND expires_at>$2`, requestID, completedAt); err != nil {
			return CompleteMessageRequestResult{}, fmt.Errorf("complete message request: %w", err)
		}
	}
	claim, err := messageRequestClaimByIDTx(ctx, tx, p, requestID, claimID)
	if err != nil {
		return CompleteMessageRequestResult{}, err
	}
	request, err := getMessageRequestByIDTx(ctx, tx, p, requestID, true)
	if err != nil {
		return CompleteMessageRequestResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CompleteMessageRequestResult{}, err
	}
	return CompleteMessageRequestResult{Request: request, Claim: claim, Message: redactMessageProcessingFence(message)}, nil
}

func logMessageRequestAudit(ctx context.Context, tx pgx.Tx, p Principal, verb string, metadata map[string]any) error {
	if err := logEventTx(ctx, tx, EventInput{
		AccountID: p.AccountID, ActorKind: ActorAgent, ActorID: p.ID,
		Verb: verb, Metadata: metadata,
	}); err != nil {
		return fmt.Errorf("log message request audit event: %w", err)
	}
	return nil
}
