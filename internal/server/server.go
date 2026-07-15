// Package server runs the witself-server backend listeners. This first slice
// serves a minimal version endpoint on the API listener, Kubernetes-compatible
// health probes on the health listener, and a single Prometheus "up" metric on
// the metrics listener. Domain behavior is specified under docs/ and lands in
// later slices.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/witwave-ai/witself/internal/placement"
	"github.com/witwave-ai/witself/internal/version"
)

// Config holds the listen addresses for the three witself-server listeners.
type Config struct {
	APIAddr     string // public /v1 API
	HealthAddr  string // Kubernetes liveness/readiness/startup probes
	MetricsAddr string // Prometheus metrics

	// Ready, when set, gates /readyz: it returns 200 only when Ready returns
	// nil, else 503. nil means always-ready. Liveness/startup never gate on it.
	Ready func(context.Context) error

	// AccountID, when set, is surfaced as the account block in /v1/capabilities
	// (the seeded default account on single-account backends).
	AccountID string

	// Login, when set, enables POST /v1/auth/bootstrap to exchange a bootstrap
	// token for an operator token.
	Login LoginFunc

	// Authenticate, when set, enables bearer-token auth (e.g. GET /v1/whoami):
	// it resolves an operator token to its principal.
	Authenticate AuthFunc

	// AuthenticatePrincipal accepts either an operator or agent token for
	// domain surfaces that intentionally support both. Transcript writes still
	// require an agent principal; operators receive read-only account-wide
	// visibility. Keeping this separate from Authenticate prevents agent tokens
	// from widening any existing operator-only route.
	AuthenticatePrincipal PrincipalAuthFunc

	// CreateRealm / ListRealms, when set (with Authenticate), enable the
	// operator-authenticated /v1/realms endpoints, scoped to the caller's account.
	CreateRealm func(ctx context.Context, accountID, name string) (Realm, error)
	ListRealms  func(ctx context.Context, accountID string) ([]Realm, error)
	DeleteRealm func(ctx context.Context, accountID, realmID string) error

	// CreateAgent / ListAgents, when set, enable POST/GET /v1/realms/{realm}/agents.
	CreateAgent func(ctx context.Context, accountID, realmID, name string) (Agent, error)
	ListAgents  func(ctx context.Context, accountID, realmID string) ([]Agent, error)
	DeleteAgent func(ctx context.Context, accountID, realmID, agentID string) error

	// CreateAgentToken, when set, enables POST /v1/agents/{agent}/tokens to mint a
	// durable agent token (returned once, with the agent's name for client-side
	// file naming).
	CreateAgentToken func(ctx context.Context, accountID, actorOperatorID, agentID string) (token, tokenID, agentName string, err error)

	// CreateCuratorToken, when set, enables an operator to mint a short-lived,
	// server-restricted agent credential for a client-side memory curator. The
	// access profile is immutable for the life of the token.
	CreateCuratorToken func(ctx context.Context, accountID, actorOperatorID, agentID, accessProfile, displayName string, ttl time.Duration) (token, tokenID, agentName string, expiresAt time.Time, err error)

	// CreateOperatorToken, when set, enables POST /v1/operators/self/tokens to mint
	// an additional operator token for the authenticated operator (returned once).
	CreateOperatorToken func(ctx context.Context, accountID, operatorID, displayName string, ttl *time.Duration) (token string, tokenID string, expiresAt *time.Time, err error)

	// Operator lifecycle endpoints manage named operator principals.
	ListOperators  func(ctx context.Context, accountID string) ([]Operator, error)
	CreateOperator func(ctx context.Context, accountID, actorOperatorID, displayName, tokenDisplayName string, ttl *time.Duration) (Operator, string, *time.Time, error)
	DeleteOperator func(ctx context.Context, accountID, actorOperatorID, targetOperatorID string) error

	// RevokeToken, when set, enables POST /v1/tokens/{token}:revoke.
	RevokeToken func(ctx context.Context, accountID, actorOperatorID, tokenID string) error

	// CloseAccount, when set, enables POST /v1/account:close — the permanent,
	// owner-only tombstone of the authenticated operator's account.
	CloseAccount func(ctx context.Context, accountID, operatorID, reason string) error

	// GetAccount, when set, enables GET /v1/account — the authenticated
	// operator's account lifecycle record. Reachable at any status: a pending
	// account checks here whether its activation gates have passed.
	GetAccount func(ctx context.Context, accountID string) (AccountRecord, error)

	// RenameAccount, when set, enables POST /v1/account:rename — the
	// owner-only change of the account's server-side display name.
	RenameAccount func(ctx context.Context, accountID, operatorID, displayName string) error

	// Placement policy is the account owner's preference/pinning source of
	// truth for future cell placement. This only stores policy; movement is
	// still driven by the control plane.
	GetPlacementPolicy func(ctx context.Context, accountID, operatorID string) (placement.Policy, error)
	SetPlacementPolicy func(ctx context.Context, accountID, operatorID string, policy placement.Policy) (placement.Policy, error)

	// ProvisionToken + ProvisionAccount, when both set, enable POST /v1/accounts:
	// the control-plane -> cell trust link that creates a new (non-default)
	// account with its root operator and a one-shot bootstrap token. Self-hosted
	// cells never set the token, so the route is not even mounted there.
	ProvisionToken   string
	ProvisionAccount func(ctx context.Context, email, displayName string) (ProvisionedAccount, error)

	// ReapAccount, when set (with the provisioning pair), enables POST
	// /v1/accounts/{id}:reap — the control plane's expiry sweep closing an
	// account that never activated. Same trust link and same cloud-only gating
	// as provisioning; the only-if-pending guard lives in the implementation,
	// which returns ErrConflict for an activated account.
	ReapAccount func(ctx context.Context, accountID string) (reaped bool, err error)

	// ActivateAccount, when set (with the provisioning pair), enables POST
	// /v1/accounts/{id}:activate — the control plane flipping a pending
	// account to active after its verification gate passes. The mirror of
	// ReapAccount: only-if-pending, idempotent when already active
	// (activated=false), ErrConflict when the account is closed or otherwise
	// ineligible.
	ActivateAccount func(ctx context.Context, accountID string) (activated bool, err error)

	// AccountContact, when set (with the provisioning pair), enables POST
	// /v1/accounts/{id}:contact — the control plane reading an account's
	// contact email when no operator token exists (the recovery request).
	// Read-only, machine-authorized.
	AccountContact func(ctx context.Context, accountID string) (AccountRecord, error)

	// GetPlacementPolicySystem, when set (with the provisioning pair), enables
	// GET /v1/accounts/{id}/placement-policy — the control plane reading the
	// account-owned placement policy during evacuation, without an owner token.
	GetPlacementPolicySystem func(ctx context.Context, accountID string) (placement.Policy, error)
	// PATCH /v1/accounts/{id}/placement-policy — the control plane restoring an
	// operator-rescued policy after importing an archived account. Unlike the
	// owner route, this is allowed while the account is evacuation-suspended.
	SetPlacementPolicySystem func(ctx context.Context, accountID string, policy placement.Policy) (placement.Policy, error)

	// RecoverAccount, when set (with the provisioning pair), enables POST
	// /v1/accounts/{id}:recover — after the control plane verifies inbox
	// control, the cell rotates the root operator's credentials: all live
	// root tokens die and a fresh one-shot bootstrap token is returned.
	// Only-if-active; ErrConflict otherwise.
	RecoverAccount func(ctx context.Context, accountID string) (ProvisionedAccount, error)

	// UpdateAccountEmail, when set (with the provisioning pair), enables POST
	// /v1/accounts/{id}:update-email — the control plane committing an email
	// change after proving the new inbox can receive. The acting operator id
	// travels in the body; the store enforces owner-only and active-only.
	// ErrNotAccountOwner -> 403, ErrConflict -> 409. Also serves the undo
	// variant ({undo:true, expected_current, new_email}) — the control plane
	// applies it after the 48-hour undo link is clicked.
	UpdateAccountEmail func(ctx context.Context, accountID, operatorID, newEmail string) error
	UndoAccountEmail   func(ctx context.Context, accountID, expectedCurrent, newEmail string) error

	// SuspendAccountOwner, when set, enables POST /v1/account:suspend — the
	// owner freezing every write on the account (reads and status still
	// work). ErrConflict when the account is not active.
	SuspendAccountOwner func(ctx context.Context, accountID, operatorID, reason string) error

	// SuspendAccountSystem, when set (with the provisioning pair), enables
	// POST /v1/accounts/{id}:suspend — machine-initiated suspension with a
	// category ("evacuation"). Idempotent; preserves an existing suspension's
	// category. ErrConflict for pending accounts.
	SuspendAccountSystem func(ctx context.Context, accountID, category, reason string) error

	// StreamAccountExport, when set (with the provisioning pair), enables
	// POST /v1/accounts/{id}:export — streaming the account's complete
	// logical archive. Refuses unless the account is suspended or closed
	// (ErrConflict): the write freeze is what makes the snapshot consistent.
	// Errors after the first byte can only be signaled in-stream; the
	// archive's trailing checksums entry is the truncation detector.
	StreamAccountExport func(ctx context.Context, accountID string, w io.Writer) error

	// ResumeAccountOwner, when set, enables POST /v1/account:resume — the
	// owner un-freezing a self-suspended account. Refuses to un-suspend a
	// fleet-admin/migration/etc. suspension (ErrCannotSelfResume -> 403); a
	// not-suspended account gets ErrConflict.
	ResumeAccountOwner func(ctx context.Context, accountID, operatorID string) error

	// ListAccountEvents, when set, enables GET /v1/account/events — the
	// owner's audit trail for their own account. Non-owner operators are
	// refused with ErrNotAccountOwner (-> 403). Paginated with an opaque
	// cursor, filterable by since/until/verb.
	ListAccountEvents func(ctx context.Context, accountID, operatorID string, filter EventFilter) (EventPage, error)

	// LogAccountEvent, when set (with the provisioning pair), enables
	// POST /v1/accounts/{id}:events — the Worker records events on the
	// tenant's cell for actions that have no store mutation of their
	// own (email dispatched, recovery form submitted). Provision-token
	// authorized. verb, actor_kind, and metadata are validated by the
	// store's verb registry before the row lands.
	LogAccountEvent func(ctx context.Context, accountID, verb, actorKind string, metadata map[string]any) error

	// ImportAccountArchive, when set (with the provisioning pair), enables
	// POST /v1/accounts/{id}:import — the restore half of the archive story:
	// the request body streams a logical archive (the :export format), and
	// the account lands in its exported state, suspended or closed. Refusals:
	// ErrConflict when the account already exists here, ErrArchiveTooNew when
	// the archive's schema outruns this cell, ErrBadArchive for anything
	// structurally wrong (corrupt, truncated, or naming a different account).
	ImportAccountArchive func(ctx context.Context, accountID string, body io.Reader) (ImportSummary, error)

	// ResumeAccountSystem, when set (with the provisioning pair), enables
	// POST /v1/accounts/{id}:resume — machine-initiated resume scoped by
	// category, the mirror of SuspendAccountSystem: only the authority that
	// suspended may resume. ErrResumeWrongCategory when the categories
	// disagree, ErrAccountNotSuspended when there is nothing to lift.
	ResumeAccountSystem func(ctx context.Context, accountID, category string) error

	// SetAccountPlan, when set (with the provisioning pair), enables
	// POST /v1/accounts/{id}:plan — the control plane applying a plan
	// SNAPSHOT (plan label + resolved account-wide limits + feature list) to
	// the account. The cell stores and enforces the snapshot exactly as
	// computed — it never consults a plan catalog, so comped and
	// custom-limit accounts need nothing special here. A missing limits key
	// means unlimited. ErrNotFound for unknown accounts.
	SetAccountPlan func(ctx context.Context, accountID, plan string, limits map[string]int64, features []string) error

	// PlanInfo, when set, surfaces the deployment account's applied plan in
	// GET /v1/capabilities: the plan label on the account block, the limits
	// snapshot in "limits", and the feature list. Single-account backends
	// wire this to the default account's record; without it capabilities
	// reports no plan (self-host default).
	PlanInfo func(ctx context.Context) (plan string, limits map[string]int64, features []string, err error)

	// OpenSupportTicket / ListSupportTickets / GetSupportTicket /
	// ReplySupportTicket / ChangeSupportTicketState, when set (with
	// Authenticate), enable /v1/support/tickets. Any operator on the
	// account may open, list, read, reply, and transition; the account
	// owner's audit trail (via ListAccountEvents) shows who did what.
	// ErrSupportDisabled -> 409, ErrTicketNotFound -> 404,
	// ErrTicketStateInvalid -> 409, ErrTicketInputInvalid -> 400.
	OpenSupportTicket        func(ctx context.Context, in OpenTicketRequest) (SupportTicket, SupportTicketMessage, error)
	ListSupportTickets       func(ctx context.Context, accountID, operatorID string) ([]SupportTicket, error)
	GetSupportTicket         func(ctx context.Context, accountID, operatorID, ticketID string) (SupportTicket, []SupportTicketMessage, error)
	ReplySupportTicket       func(ctx context.Context, accountID, operatorID, ticketID, body string) (SupportTicketMessage, error)
	ChangeSupportTicketState func(ctx context.Context, in ChangeTicketStateRequest) (SupportTicket, error)

	// ListAdminTickets / GetAdminTicket / ReplyAdminTicket /
	// ChangeAdminTicketState / ListAdminTicketsAll, when set (with the
	// provisioning pair), enable the admin-side counterparts of the
	// support-ticket endpoints. Provision-token authorized: the caller
	// is the control plane, which has already authenticated the admin
	// against its own credential store. admin_handle travels on every
	// request body and is validated shape-only at the store.
	//
	// The first four hang off /v1/accounts/{id}/admin:{action} —
	// account-scoped mirrors of the tenant flow. The fifth serves
	// /v1/support/admin:list-tickets which is cell-wide (no account
	// id) and cursor-paginated — the CP fans this call out to every
	// cell in parallel to build the fleet-wide admin queue.
	ListAdminTickets       func(ctx context.Context, accountID string) ([]SupportTicket, error)
	GetAdminTicket         func(ctx context.Context, accountID, ticketID string) (SupportTicket, []SupportTicketMessage, error)
	ReplyAdminTicket       func(ctx context.Context, in ReplyAdminTicketRequest) (SupportTicketMessage, error)
	ChangeAdminTicketState func(ctx context.Context, in ChangeAdminTicketStateRequest) (SupportTicket, error)
	ListAdminTicketsAll    func(ctx context.Context, in ListAdminTicketsAllRequest) (ListAdminTicketsAllResult, error)

	// GetAdminSupportPolicy / SetAdminSupportPolicy, when set (with
	// the provisioning pair), enable admin-side reads and writes of
	// the account's support_policy. This is what the CP calls when a
	// fleet admin runs `witself-admin account support-policy [--set]`.
	// SetAdminSupportPolicy emits VerbAccountSupportPolicyChanged on
	// a genuine transition; idempotent no-op when the target policy
	// equals the current one.
	GetAdminSupportPolicy func(ctx context.Context, accountID string) (string, error)
	SetAdminSupportPolicy func(ctx context.Context, in SetAdminSupportPolicyRequest) (SetAdminSupportPolicyResult, error)

	// ListAdminEventsAll, when set (with the provisioning pair),
	// enables POST /v1/events/admin:list — the cell-wide audit-event
	// tail (every account, newest first, cursor-paginated) behind the
	// fleet dashboard's events pane and `witself-admin events`. Same
	// filter + cursor semantics as the owner's per-account view; the
	// fleet-wide scope is exactly the point.
	ListAdminEventsAll func(ctx context.Context, filter EventFilter) (EventPage, error)

	// Transcript ledger: append-only visible interaction capture. Agent tokens
	// create/append their own transcripts. Agents read their own; operators read
	// the whole account. Bodies and payloads are never copied into audit events.
	CreateTranscript        func(ctx context.Context, p DomainPrincipal, in CreateTranscriptRequest) (Transcript, error)
	AppendTranscriptEntry   func(ctx context.Context, p DomainPrincipal, transcriptID string, in AppendTranscriptEntryRequest) (TranscriptEntry, error)
	AppendTranscriptEntries func(ctx context.Context, p DomainPrincipal, transcriptID string, in []AppendTranscriptEntryRequest) ([]TranscriptEntry, error)
	ListTranscripts         func(ctx context.Context, p DomainPrincipal) ([]Transcript, error)
	GetTranscript           func(ctx context.Context, p DomainPrincipal, transcriptID string) (Transcript, []TranscriptEntry, error)
	GetTranscriptPage       func(ctx context.Context, p DomainPrincipal, transcriptID string, opts TranscriptPageOptions) (TranscriptPage, error)
	GetUsage                func(ctx context.Context, p DomainPrincipal, query UsageQuery) (UsageReport, error)
	SetFact                 func(ctx context.Context, p DomainPrincipal, in SetFactRequest) (Fact, error)
	DeleteFact              func(ctx context.Context, p DomainPrincipal, in DeleteFactRequest) (FactDeletionReceipt, error)
	GetFact                 func(ctx context.Context, p DomainPrincipal, subject, predicate string) (Fact, error)
	ListFacts               func(ctx context.Context, p DomainPrincipal, opts FactListOptions) ([]Fact, error)
	GetFactHistory          func(ctx context.Context, p DomainPrincipal, factID string) ([]FactAssertion, error)
	ProposeFact             func(ctx context.Context, p DomainPrincipal, in ProposeFactRequest) (FactCandidate, error)
	GetFactCandidate        func(ctx context.Context, p DomainPrincipal, candidateID string) (FactCandidate, error)
	ListFactCandidates      func(ctx context.Context, p DomainPrincipal, opts FactCandidateListOptions) ([]FactCandidate, error)
	ConfirmFactCandidate    func(ctx context.Context, p DomainPrincipal, candidateID, idempotencyKey string) (Fact, error)
	RejectFactCandidate     func(ctx context.Context, p DomainPrincipal, candidateID, idempotencyKey string) (FactCandidate, error)
	GetSelfFacts            func(ctx context.Context, p DomainPrincipal, limit int) ([]SelfFact, int, error)
	CountSelfFacts          func(ctx context.Context, p DomainPrincipal) (int, error)
	GetSelfMemories         func(ctx context.Context, p DomainPrincipal, limit int) ([]SelfMemory, int, error)
	CountSelfMemories       func(ctx context.Context, p DomainPrincipal) (int, error)
	UpcomingFacts           func(ctx context.Context, p DomainPrincipal, opts UpcomingFactOptions) ([]FactOccurrence, error)
	UpsertFactSubject       func(ctx context.Context, p DomainPrincipal, canonicalKey string, in UpsertFactSubjectRequest) (FactSubject, error)
	AddFactSubjectAlias     func(ctx context.Context, p DomainPrincipal, canonicalKey, alias string) (FactSubject, error)
	ListFactSubjects        func(ctx context.Context, p DomainPrincipal) ([]FactSubject, error)

	// Narrative memory is agent-owned in the first vertical slice. The server
	// authenticates the principal and validates the HTTP contract; callbacks
	// enforce tenant/owner isolation, optimistic concurrency, and idempotency.
	CaptureMemory             func(ctx context.Context, p DomainPrincipal, in CaptureMemoryRequest) (MemoryMutationResult, error)
	GetMemory                 func(ctx context.Context, p DomainPrincipal, memoryID string) (Memory, error)
	ListMemories              func(ctx context.Context, p DomainPrincipal, opts MemoryListOptions) (MemoryPage, error)
	RecallMemories            func(ctx context.Context, p DomainPrincipal, in MemoryRecallRequest) (MemoryRecallPage, error)
	GetMemoryHistory          func(ctx context.Context, p DomainPrincipal, memoryID string, opts MemoryHistoryOptions) (MemoryHistoryPage, error)
	AdjustMemory              func(ctx context.Context, p DomainPrincipal, memoryID string, in AdjustMemoryRequest) (MemoryMutationResult, error)
	SupersedeMemory           func(ctx context.Context, p DomainPrincipal, memoryID string, in SupersedeMemoryRequest) (SupersedeMemoryResult, error)
	ForgetMemory              func(ctx context.Context, p DomainPrincipal, memoryID string, in MemoryLifecycleRequest) (MemoryMutationResult, error)
	RestoreMemory             func(ctx context.Context, p DomainPrincipal, memoryID string, in MemoryLifecycleRequest) (MemoryMutationResult, error)
	ReactivateMemory          func(ctx context.Context, p DomainPrincipal, memoryID string, in MemoryLifecycleRequest) (MemoryMutationResult, error)
	ResolveMemoryEvidence     func(ctx context.Context, p DomainPrincipal, evidenceID string, in ResolveMemoryEvidenceRequest) (MemoryEvidence, error)
	DeleteMemory              func(ctx context.Context, p DomainPrincipal, in DeleteMemoryRequest) (MemoryDeletionReceipt, error)
	CreateMemoryVectorProfile func(ctx context.Context, p DomainPrincipal, in CreateMemoryVectorProfileRequest) (MemoryVectorProfile, error)
	ListMemoryVectorProfiles  func(ctx context.Context, p DomainPrincipal) ([]MemoryVectorProfile, error)
	PutMemoryVector           func(ctx context.Context, p DomainPrincipal, in PutMemoryVectorRequest) (MemoryVectorReceipt, error)

	// Memory curation is a client-inference work queue. The HTTP boundary owns
	// authentication, bounded strict JSON, seconds-on-the-wire conversion, and
	// retry headers; implementations own the fenced transactional state machine.
	RequestMemoryCuration      func(ctx context.Context, p DomainPrincipal, in RequestMemoryCurationRequest) (any, error)
	ListMemoryCurationRequests func(ctx context.Context, p DomainPrincipal, opts MemoryCurationRequestListOptions) (any, error)
	GetMemoryCurationRequest   func(ctx context.Context, p DomainPrincipal, requestID string) (any, error)
	StartMemoryCuration        func(ctx context.Context, p DomainPrincipal, in StartMemoryCurationRequest) (any, error)
	GetMemoryCurationRun       func(ctx context.Context, p DomainPrincipal, runID string) (any, error)
	GetMemoryCurationRunInputs func(ctx context.Context, p DomainPrincipal, runID string, opts MemoryCurationRunInputOptions) (any, error)
	RenewMemoryCuration        func(ctx context.Context, p DomainPrincipal, runID string, in RenewMemoryCurationRequest) (any, error)
	PlanMemoryCuration         func(ctx context.Context, p DomainPrincipal, runID string, in PlanMemoryCurationRequest) (any, error)
	ApplyMemoryCuration        func(ctx context.Context, p DomainPrincipal, runID string, in ApplyMemoryCurationRequest) (any, error)
	CancelMemoryCuration       func(ctx context.Context, p DomainPrincipal, runID string, in FinishMemoryCurationRequest) (any, error)
	AbandonMemoryCuration      func(ctx context.Context, p DomainPrincipal, runID string, in FinishMemoryCurationRequest) (any, error)
	RollbackMemoryCuration     func(ctx context.Context, p DomainPrincipal, runID string, in RollbackMemoryCurationRequest) (any, error)
	GetMemoryCurationStatus    func(ctx context.Context, p DomainPrincipal, runID string) (any, error)

	// Realm-local direct messaging. All hooks require an agent principal; the
	// store derives sender/account/realm from that principal and never from the
	// request body. List is metadata-only; Read is the content boundary.
	SendMessage  func(ctx context.Context, p DomainPrincipal, in SendMessageRequest) (Message, error)
	ReplyMessage func(ctx context.Context, p DomainPrincipal, parentMessageID string, in ReplyMessageRequest) (Message, error)
	ListMessages func(ctx context.Context, p DomainPrincipal, opts MessageListOptions) (MessagePage, error)
	ReadMessage  func(ctx context.Context, p DomainPrincipal, messageID string) (Message, error)
	AckMessage   func(ctx context.Context, p DomainPrincipal, messageID string) (Message, error)

	// Message processing is a recipient-only fenced lease. The claim id and
	// generation returned by ClaimMessage must be presented by every later
	// transition; implementations atomically fence stale workers.
	ClaimMessage        func(ctx context.Context, p DomainPrincipal, messageID string, in ClaimMessageRequest) (MessageProcessing, error)
	RenewMessageClaim   func(ctx context.Context, p DomainPrincipal, messageID string, in RenewMessageClaimRequest) (MessageProcessing, error)
	ReleaseMessageClaim func(ctx context.Context, p DomainPrincipal, messageID string, in MessageClaimRequest) (MessageProcessing, error)
	CompleteMessage     func(ctx context.Context, p DomainPrincipal, messageID string, in CompleteMessageRequest) (CompleteMessageResult, error)
}

// SetAdminSupportPolicyRequest is the payload for the admin
// support-policy write hook.
type SetAdminSupportPolicyRequest struct {
	AccountID   string
	AdminHandle string
	NewPolicy   string
}

// SetAdminSupportPolicyResult reports the transition.
type SetAdminSupportPolicyResult struct {
	AccountID  string
	PolicyFrom string
	PolicyTo   string
}

// ReplyAdminTicketRequest is the payload for the admin reply hook.
type ReplyAdminTicketRequest struct {
	AccountID   string
	AdminHandle string
	TicketID    string
	Body        string
}

// ChangeAdminTicketStateRequest is the payload for the admin state hook.
type ChangeAdminTicketStateRequest struct {
	AccountID   string
	AdminHandle string
	TicketID    string
	NewState    string
}

// ListAdminTicketsAllRequest constrains the cell-wide admin list. All
// fields optional. Limit is clamped to [1, 500] at the store.
type ListAdminTicketsAllRequest struct {
	States    []string
	Since     *time.Time
	Limit     int
	PageToken string
}

// ListAdminTicketsAllResult carries a page of tickets + the opaque
// continuation cursor.
type ListAdminTicketsAllResult struct {
	Tickets       []SupportTicket
	NextPageToken string
}

// OpenTicketRequest is the input to the OpenSupportTicket Config hook.
type OpenTicketRequest struct {
	AccountID  string
	OperatorID string
	Subject    string
	Category   string
	Priority   string
	Body       string
}

// ChangeTicketStateRequest is the input to the ChangeSupportTicketState hook.
type ChangeTicketStateRequest struct {
	AccountID  string
	OperatorID string
	TicketID   string
	NewState   string
}

// SupportTicket is the API view of one support ticket. Shape mirrors the
// store row so wiring is a straight-through copy.
type SupportTicket struct {
	ID              string          `json:"id"`
	AccountID       string          `json:"account_id"`
	OpenedAt        time.Time       `json:"opened_at"`
	OpenedByKind    string          `json:"opened_by_kind"`
	OpenedByID      string          `json:"opened_by_id"`
	Subject         string          `json:"subject"`
	Category        string          `json:"category"`
	State           string          `json:"state"`
	Priority        string          `json:"priority"`
	FirstResponseAt *time.Time      `json:"first_response_at,omitempty"`
	ResolvedAt      *time.Time      `json:"resolved_at,omitempty"`
	ClosedAt        *time.Time      `json:"closed_at,omitempty"`
	LastActivityAt  time.Time       `json:"last_activity_at"`
	LastMessageID   string          `json:"last_message_id,omitempty"`
	Correlation     json.RawMessage `json:"correlation"`
	Metadata        json.RawMessage `json:"metadata"`
}

// SupportTicketMessage is the API view of one support_ticket_messages row.
type SupportTicketMessage struct {
	ID          string          `json:"id"`
	TicketID    string          `json:"ticket_id"`
	AccountID   string          `json:"account_id"`
	PostedAt    time.Time       `json:"posted_at"`
	AuthorKind  string          `json:"author_kind"`
	AuthorID    string          `json:"author_id,omitempty"`
	Body        string          `json:"body"`
	Attachments json.RawMessage `json:"attachments"`
	Metadata    json.RawMessage `json:"metadata"`
}

// ImportSummary is the API view of a completed archive restore.
type ImportSummary struct {
	AccountID     string `json:"account_id"`
	Status        string `json:"status"`
	SchemaVersion int    `json:"schema_version"` // the ARCHIVE's schema coordinate
}

// Event is the server-visible shape of one account_events row. Same
// wire fields as the store's row, JSON-serialized straight through.
type Event struct {
	ID         string          `json:"id"`
	AccountID  string          `json:"account_id"`
	OccurredAt time.Time       `json:"occurred_at"`
	ActorKind  string          `json:"actor_kind"`
	ActorID    string          `json:"actor_id,omitempty"`
	Verb       string          `json:"verb"`
	Metadata   json.RawMessage `json:"metadata"`
}

// EventFilter constrains ListAccountEvents queries. All fields optional.
// Limit is clamped to [1, 500] in the store.
type EventFilter struct {
	Since  *time.Time
	Until  *time.Time
	Verb   string
	Limit  int
	Cursor string
}

// EventPage is one page of ListAccountEvents results plus an opaque
// NextCursor. Empty cursor means the last page.
type EventPage struct {
	Events     []Event `json:"events"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

// AccountRecord is the API view of an account's lifecycle record.
type AccountRecord struct {
	ID              string     `json:"id"`
	Email           string     `json:"email,omitempty"`
	DisplayName     string     `json:"display_name,omitempty"`
	Status          string     `json:"status"`
	CreatedAt       time.Time  `json:"created_at"`
	ClosedAt        *time.Time `json:"closed_at,omitempty"`
	ClosedReason    string     `json:"closed_reason,omitempty"`
	SuspendedAt     *time.Time `json:"suspended_at,omitempty"`
	SuspendedFor    string     `json:"suspended_for,omitempty"`
	SuspendedReason string     `json:"suspended_reason,omitempty"`
	SupportPolicy   string     `json:"support_policy,omitempty"`
	// Plan snapshot as applied by the control plane (empty = never applied:
	// the self-host / pre-provisioning default, unlimited).
	Plan         string           `json:"plan,omitempty"`
	PlanLimits   map[string]int64 `json:"plan_limits,omitempty"`
	PlanFeatures []string         `json:"plan_features,omitempty"`

	PlacementPolicy placement.Policy `json:"placement_policy,omitempty"`
}

// ProvisionedAccount is the API view of a freshly provisioned account. The
// bootstrap token is returned exactly once; the new owner exchanges it for an
// operator token via the ordinary POST /v1/auth/bootstrap.
type ProvisionedAccount struct {
	AccountID      string `json:"account_id"`
	OperatorID     string `json:"operator_id"`
	Email          string `json:"email"`
	Status         string `json:"status"`
	BootstrapToken string `json:"bootstrap_token"`
}

// LoginFunc exchanges a bootstrap token for an operator token. ok is false when
// the token is invalid or already used (-> 401); a non-nil error is a server
// fault (-> 500).
type LoginFunc func(ctx context.Context, bootstrapToken string) (operatorToken, operatorID string, ok bool, err error)

// AuthFunc resolves a bearer token to its operator principal, including the
// account's lifecycle status ("pending"/"active"/"closed") — status is part of
// the principal so handlers can gate on it without a second lookup. ok is
// false when the token is missing/invalid (-> 401); a non-nil error is a
// server fault.
type AuthFunc func(ctx context.Context, token string) (operatorID, accountID, accountStatus string, ok bool, err error)

// PrincipalKind values distinguish operator and agent bearer credentials.
const (
	PrincipalKindOperator = "operator"
	PrincipalKindAgent    = "agent"

	// Access profiles are immutable properties of a bearer token. Full is the
	// existing operator/agent credential behavior. Curator profiles are
	// deliberately narrow and are admitted only by the memory-curation
	// handlers that name an allowed operation.
	AccessProfileFull           = "full"
	AccessProfileCuratorPreview = "curator-preview"
	AccessProfileCuratorApply   = "curator-apply"
)

// DomainPrincipal is a bearer-token-derived identity. RealmID is populated for
// agents and empty for operators.
type DomainPrincipal struct {
	Kind           string
	ID             string
	AccountID      string
	RealmID        string
	AgentName      string
	RealmName      string
	AccountStatus  string
	TokenID        string
	AccessProfile  string
	TokenExpiresAt *time.Time
}

// PrincipalAuthFunc resolves either an operator or agent bearer token.
type PrincipalAuthFunc func(ctx context.Context, token string) (DomainPrincipal, bool, error)

// SelfDigest is the bounded, token-derived agent identity and open-plane
// session-start view returned by GET /v1/self. Facts and memories are empty in
// the identity-only slice and can be populated without changing this envelope.
type SelfDigest struct {
	SchemaVersion   string       `json:"schema_version"`
	Identity        SelfIdentity `json:"identity"`
	PrimaryFacts    []SelfFact   `json:"primary_facts"`
	SalientMemories []SelfMemory `json:"salient_memories"`
	Index           SelfIndex    `json:"index"`
	Elided          bool         `json:"elided"`
}

// SelfIdentity is the account, realm, and agent identity derived from the
// bearer token used to request a self digest.
type SelfIdentity struct {
	AccountID string `json:"account_id"`
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
	RealmID   string `json:"realm_id"`
	RealmName string `json:"realm_name"`
}

// SelfFact is the bounded primary-fact representation included in a self
// digest once the fact store is implemented.
type SelfFact struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Value     any    `json:"value"`
	Primary   bool   `json:"primary"`
	Sensitive bool   `json:"sensitive,omitempty"`
	Redacted  bool   `json:"redacted,omitempty"`
	Source    string `json:"source,omitempty"`
}

// SelfMemory is the bounded salient-memory representation included in a self
// digest once the memory store is implemented.
type SelfMemory struct {
	ID        string   `json:"id"`
	Snippet   string   `json:"snippet"`
	Kind      string   `json:"kind"`
	Tags      []string `json:"tags,omitempty"`
	Salience  float64  `json:"salience"`
	Sensitive bool     `json:"sensitive,omitempty"`
	Redacted  bool     `json:"redacted,omitempty"`
	Source    string   `json:"source,omitempty"`
}

// SelfIndex summarizes the kinds, tags, and open-plane record counts available
// to the authenticated agent.
type SelfIndex struct {
	Kinds  []string       `json:"kinds"`
	Tags   []string       `json:"tags"`
	Counts map[string]int `json:"counts"`
}

// Transcript is the API view of one visible interaction thread.
type Transcript struct {
	ID           string          `json:"id"`
	AccountID    string          `json:"account_id"`
	RealmID      string          `json:"realm_id"`
	OwnerAgentID string          `json:"owner_agent_id"`
	ExternalID   string          `json:"external_id,omitempty"`
	Title        string          `json:"title,omitempty"`
	Metadata     json.RawMessage `json:"metadata"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// TranscriptEntry is one immutable visible turn or explicit system/tool trace.
type TranscriptEntry struct {
	ID                string          `json:"id"`
	AccountID         string          `json:"account_id"`
	TranscriptID      string          `json:"transcript_id"`
	RealmID           string          `json:"realm_id"`
	RecordedByAgentID string          `json:"recorded_by_agent_id"`
	Sequence          int64           `json:"sequence"`
	ExternalID        string          `json:"external_id,omitempty"`
	Role              string          `json:"role"`
	Body              string          `json:"body,omitempty"`
	Payload           json.RawMessage `json:"payload,omitempty"`
	Model             string          `json:"model,omitempty"`
	ReplyToEntryID    string          `json:"reply_to_entry_id,omitempty"`
	Artifacts         json.RawMessage `json:"artifacts"`
	CreatedAt         time.Time       `json:"created_at"`
}

// CreateTranscriptRequest is the POST /v1/transcripts body.
type CreateTranscriptRequest struct {
	ExternalID string          `json:"external_id,omitempty"`
	Title      string          `json:"title,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

// AppendTranscriptEntryRequest is the transcript entry append body.
type AppendTranscriptEntryRequest struct {
	ExternalID        string          `json:"external_id,omitempty"`
	Role              string          `json:"role"`
	Body              string          `json:"body,omitempty"`
	Payload           json.RawMessage `json:"payload,omitempty"`
	Model             string          `json:"model,omitempty"`
	ReplyToEntryID    string          `json:"reply_to_entry_id,omitempty"`
	ReplyToExternalID string          `json:"reply_to_external_id,omitempty"`
	Artifacts         json.RawMessage `json:"artifacts,omitempty"`
}

// TranscriptPageOptions selects one bounded transcript read.
type TranscriptPageOptions struct {
	AfterSequence int64
	Limit         int
	Tail          bool
}

// TranscriptPage is the API-neutral result of a bounded transcript read.
type TranscriptPage struct {
	Transcript        Transcript
	Entries           []TranscriptEntry
	NextAfterSequence int64
}

// UsageQuery selects the authenticated agent's hourly or daily usage rollups.
type UsageQuery struct {
	Since      time.Time
	Until      time.Time
	Bucket     string
	Dimensions []string
}

// UsagePoint is one dimension total in a UTC time bucket.
type UsagePoint struct {
	Dimension   string    `json:"dimension"`
	Unit        string    `json:"unit"`
	BucketStart time.Time `json:"bucket_start"`
	Quantity    int64     `json:"quantity"`
	EventCount  int64     `json:"event_count"`
}

// UsageTotal is a dimension total across the report window.
type UsageTotal struct {
	Dimension  string `json:"dimension"`
	Unit       string `json:"unit"`
	Quantity   int64  `json:"quantity"`
	EventCount int64  `json:"event_count"`
}

// UsageReport is the token-derived per-agent usage view.
type UsageReport struct {
	AccountID string       `json:"account_id"`
	RealmID   string       `json:"realm_id"`
	RealmName string       `json:"realm_name,omitempty"`
	AgentID   string       `json:"agent_id"`
	AgentName string       `json:"agent_name,omitempty"`
	Since     time.Time    `json:"since"`
	Until     time.Time    `json:"until"`
	Bucket    string       `json:"bucket"`
	Points    []UsagePoint `json:"points"`
	Totals    []UsageTotal `json:"totals"`
}

// Message is one durable realm-local direct message and the recipient's
// delivery/read state. Body and Payload are omitted from list responses.
type Message struct {
	ID               string            `json:"id"`
	AccountID        string            `json:"account_id"`
	RealmID          string            `json:"realm_id"`
	From             MessageAgent      `json:"from"`
	To               MessageRecipient  `json:"to"`
	Subject          string            `json:"subject,omitempty"`
	Kind             string            `json:"kind"`
	Body             string            `json:"body,omitempty"`
	Payload          json.RawMessage   `json:"payload,omitempty"`
	ThreadID         string            `json:"thread_id"`
	ReplyToMessageID string            `json:"reply_to_message_id,omitempty"`
	CausalDepth      int64             `json:"causal_depth"`
	CreatedAt        time.Time         `json:"created_at"`
	Delivery         MessageDelivery   `json:"delivery"`
	ReadState        MessageReadState  `json:"read_state"`
	Processing       MessageProcessing `json:"processing"`
}

// MessageAgent identifies the token-derived sender in the wire shape.
type MessageAgent struct {
	Kind      string `json:"kind"`
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
}

// MessageRecipient identifies the resolved direct recipient in the wire shape.
type MessageRecipient struct {
	Kind      string `json:"kind"`
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
}

// MessageDelivery is the recipient delivery state in the wire shape.
type MessageDelivery struct {
	State       string     `json:"state"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
}

// MessageReadState is the recipient's unread/read/acked state.
type MessageReadState struct {
	State   string     `json:"state"`
	ReadAt  *time.Time `json:"read_at,omitempty"`
	AckedAt *time.Time `json:"acked_at,omitempty"`
}

// MessageRecipientRequest selects a direct recipient by name or id.
type MessageRecipientRequest struct {
	Kind      string          `json:"kind"`
	ID        string          `json:"id"`
	Realm     json.RawMessage `json:"realm,omitempty"`
	RealmID   json.RawMessage `json:"realm_id,omitempty"`
	AccountID json.RawMessage `json:"account_id,omitempty"`
}

// SendMessageRequest intentionally includes rejected actor fields so a caller
// cannot rely on permissive JSON decoding to smuggle or spoof identity.
type SendMessageRequest struct {
	To             MessageRecipientRequest `json:"to"`
	Subject        string                  `json:"subject,omitempty"`
	Kind           string                  `json:"kind,omitempty"`
	Body           string                  `json:"body"`
	Payload        json.RawMessage         `json:"payload,omitempty"`
	ThreadID       string                  `json:"thread_id,omitempty"`
	IdempotencyKey string                  `json:"-"`
	DryRun         bool                    `json:"dry_run,omitempty"`
	From           json.RawMessage         `json:"from,omitempty"`
	Sender         json.RawMessage         `json:"sender,omitempty"`
	Actor          json.RawMessage         `json:"actor,omitempty"`
	Realm          json.RawMessage         `json:"realm,omitempty"`
	RealmID        json.RawMessage         `json:"realm_id,omitempty"`
	CausalDepth    json.RawMessage         `json:"causal_depth,omitempty"`
}

// ReplyMessageRequest carries only content. Routing and causality fields are
// represented solely so permissive JSON decoding cannot silently accept them;
// the handler rejects every non-empty caller-supplied identity/routing field.
type ReplyMessageRequest struct {
	Subject          string          `json:"subject,omitempty"`
	Kind             string          `json:"kind,omitempty"`
	Body             string          `json:"body"`
	Payload          json.RawMessage `json:"payload,omitempty"`
	IdempotencyKey   string          `json:"-"`
	To               json.RawMessage `json:"to,omitempty"`
	ThreadID         json.RawMessage `json:"thread_id,omitempty"`
	ReplyToMessageID json.RawMessage `json:"reply_to_message_id,omitempty"`
	From             json.RawMessage `json:"from,omitempty"`
	Sender           json.RawMessage `json:"sender,omitempty"`
	Actor            json.RawMessage `json:"actor,omitempty"`
	Account          json.RawMessage `json:"account,omitempty"`
	AccountID        json.RawMessage `json:"account_id,omitempty"`
	Realm            json.RawMessage `json:"realm,omitempty"`
	RealmID          json.RawMessage `json:"realm_id,omitempty"`
	CausalDepth      json.RawMessage `json:"causal_depth,omitempty"`
}

// MessageListenRequest is a bounded, inbound-only, metadata-only waitable list.
// A pointer distinguishes omitted wait_seconds (server default) from an
// explicit zero-second non-blocking check.
type MessageListenRequest struct {
	WaitSeconds *int   `json:"wait_seconds,omitempty"`
	ThreadID    string `json:"thread_id,omitempty"`
	FromAgent   string `json:"from_agent,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Limit       int    `json:"limit,omitempty"`
}

// MessageListOptions selects one bounded inbox or outbox page.
type MessageListOptions struct {
	Direction   string
	Unread      bool
	Unacked     bool
	OldestFirst bool
	From        string
	ThreadID    string
	Kind        string
	Limit       int
	Cursor      string
}

// MessagePage is the API-neutral mailbox page returned by the store adapter.
type MessagePage struct {
	Messages   []Message
	NextCursor string
}

// MessageListenResult reports a waitable metadata-only mailbox page.
type MessageListenResult struct {
	Messages []Message `json:"messages"`
	TimedOut bool      `json:"timed_out"`
}

// MessageProcessing is the fenced processing state for one inbound message.
// A claim id is an opaque capability and generation is its monotonic fence.
type MessageProcessing struct {
	State           string     `json:"state"`
	ClaimID         string     `json:"claim_id,omitempty"`
	Generation      int64      `json:"generation"`
	FailureCount    int64      `json:"failure_count"`
	LeaseExpiresAt  *time.Time `json:"lease_expires_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	ResultMessageID string     `json:"result_message_id,omitempty"`
}

// ClaimMessageRequest starts or idempotently resumes one processing claim.
type ClaimMessageRequest struct {
	LeaseSeconds   int    `json:"lease_seconds"`
	IdempotencyKey string `json:"-"`
}

// MessageClaimRequest identifies one exact processing lease generation.
type MessageClaimRequest struct {
	ClaimID              string `json:"claim_id"`
	Generation           int64  `json:"generation"`
	DeterministicFailure bool   `json:"deterministic_failure"`
}

// RenewMessageClaimRequest extends one exact processing lease generation.
type RenewMessageClaimRequest struct {
	ClaimID      string `json:"claim_id"`
	Generation   int64  `json:"generation"`
	LeaseSeconds int    `json:"lease_seconds"`
}

// CompleteMessageRequest atomically completes one exact processing claim and
// creates its recipient-only result reply. Routing and identity fields are
// represented so the handler can reject attempts to spoof derived values.
type CompleteMessageRequest struct {
	ClaimID          string          `json:"claim_id"`
	Generation       int64           `json:"generation"`
	Subject          string          `json:"subject,omitempty"`
	Kind             string          `json:"kind,omitempty"`
	Body             string          `json:"body"`
	Payload          json.RawMessage `json:"payload,omitempty"`
	IdempotencyKey   string          `json:"-"`
	To               json.RawMessage `json:"to,omitempty"`
	ThreadID         json.RawMessage `json:"thread_id,omitempty"`
	ReplyToMessageID json.RawMessage `json:"reply_to_message_id,omitempty"`
	From             json.RawMessage `json:"from,omitempty"`
	Sender           json.RawMessage `json:"sender,omitempty"`
	Actor            json.RawMessage `json:"actor,omitempty"`
	Account          json.RawMessage `json:"account,omitempty"`
	AccountID        json.RawMessage `json:"account_id,omitempty"`
	Realm            json.RawMessage `json:"realm,omitempty"`
	RealmID          json.RawMessage `json:"realm_id,omitempty"`
	CausalDepth      json.RawMessage `json:"causal_depth,omitempty"`
}

// CompleteMessageResult is the atomic processing transition and durable reply.
type CompleteMessageResult struct {
	Processing MessageProcessing `json:"processing"`
	Message    Message           `json:"message"`
}

// Realm is the API view of a realm.
type Realm struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ErrConflict signals a uniqueness conflict (-> 409). The wiring layer returns
// it (e.g. for a duplicate realm name) without coupling the server to the store.
var ErrConflict = errors.New("conflict")

// ErrBusy signals an active lease held by another processing worker (-> 409).
var ErrBusy = errors.New("busy")

// ErrIdempotencyConflict signals reuse of one retry key for a different
// logical mutation. Handlers return a stable 409 without echoing request data.
var ErrIdempotencyConflict = errors.New("idempotency conflict")

// ErrFactDeleted signals a request against a fact tombstone (-> 410). Keeping
// this distinct from not-found lets clients explain that recreation must be
// explicitly authorized without exposing the former fact value.
var ErrFactDeleted = errors.New("fact deleted")

// ErrMemoryDeleted signals a request against a permanently deleted narrative
// memory tombstone (-> 410).
var ErrMemoryDeleted = errors.New("memory deleted")

// ErrMemoryDependency signals that permanent deletion would silently alter a
// distinct live memory or active lineage edge (-> 409).
var ErrMemoryDependency = errors.New("memory has live dependencies")

// ErrNotFound signals a missing resource (-> 404), e.g. a realm not in the account.
var ErrNotFound = errors.New("not found")

// ErrNotAccountOwner signals an owner-only action attempted by a non-owner (-> 403).
var ErrNotAccountOwner = errors.New("only the account owner may do this")

// ErrEmailChangedSinceUndo signals a stale undo link — the current email no
// longer matches what the undo token snapshotted (a subsequent legitimate
// change ran first) so the revert must not roll back the newer state.
var ErrEmailChangedSinceUndo = errors.New("email has changed since this undo was issued")

// ErrAccountNotSuspended signals a resume attempt against an account that is
// not currently suspended.
var ErrAccountNotSuspended = errors.New("account is not suspended")

// ErrCannotSelfResume signals the owner trying to un-suspend a suspension
// they did not initiate (fleet-admin, migration, etc.).
var ErrCannotSelfResume = errors.New("this suspension is not owner-resumable")

// ErrAccountNotActive signals a store-level refusal to mint credentials for a
// non-active account (-> 403). requireOperator normally refuses first; this
// surfaces the race where a close commits while the request is in flight.
var ErrAccountNotActive = errors.New("account is not active")

// ErrArchiveTooNew signals an import whose archive was written at a schema
// version newer than this cell understands (-> 409): upgrade the cell, don't
// downgrade the data.
var ErrArchiveTooNew = errors.New("archive schema is newer than this cell")

// ErrBadArchive signals a structurally unusable import stream — corrupt,
// truncated, or naming a different account than the request (-> 400).
var ErrBadArchive = errors.New("invalid archive")

// ErrResumeWrongCategory signals a system resume whose category does not
// match what the account is suspended for (-> 409).
var ErrResumeWrongCategory = errors.New("suspension category does not match")

// ErrBadInput signals a client-side input error the caller can fix
// (malformed pagination cursor, out-of-range parameter, unparseable
// filter). Maps to 400.
var ErrBadInput = errors.New("bad input")

// ErrForbidden signals an authenticated principal crossing a resource's
// authorization boundary (-> 403).
var ErrForbidden = errors.New("forbidden")

// ErrCannotCloseDefault signals an attempt to close the deployment's seeded
// default account (-> 403).
var ErrCannotCloseDefault = errors.New("the default account cannot be closed")

// ErrSupportDisabled signals that the account's support_policy blocks new
// ticket creation (-> 409). Existing threads remain readable.
var ErrSupportDisabled = errors.New("support is not enabled for this account")

// ErrTicketNotFound signals a ticket read/mutate against an id that does
// not exist on the caller's account (-> 404).
var ErrTicketNotFound = errors.New("ticket not found")

// ErrTicketStateInvalid signals a rejected state transition or a reply
// against a closed ticket (-> 409).
var ErrTicketStateInvalid = errors.New("invalid ticket state transition")

// ErrTicketInputInvalid signals a caller-side input violation (empty
// subject, oversized body, unknown category, etc.) (-> 400).
var ErrTicketInputInvalid = errors.New("invalid support ticket input")

// ErrPlanLimit signals that creating a resource would exceed the account's
// plan-limit snapshot (-> 403). The wrapped message carries the detail
// ("plan limit reached: agents 25/25 on the free plan") and is surfaced
// verbatim so the refusal explains itself and names the upgrade path.
var ErrPlanLimit = errors.New("plan limit reached")

// Agent is the API view of an agent.
type Agent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// OperatorToken is safe token metadata shown in operator listings.
type OperatorToken struct {
	ID          string     `json:"id"`
	DisplayName string     `json:"display_name"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

// Operator is the API view of a human/admin operator principal.
type Operator struct {
	ID          string          `json:"id"`
	DisplayName string          `json:"display_name"`
	Role        string          `json:"role"`
	IsRoot      bool            `json:"is_root"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	Tokens      []OperatorToken `json:"tokens"`
}

var (
	// ErrLastOperator signals a rejected delete that would leave the account
	// without any live operator.
	ErrLastOperator = errors.New("last operator")
	// ErrCannotDeleteSelf signals a rejected self-delete.
	ErrCannotDeleteSelf = errors.New("cannot delete self")
	// ErrCannotDeleteRoot signals a rejected root operator delete.
	ErrCannotDeleteRoot = errors.New("cannot delete root operator")
)

// ConfigFromEnv builds a Config from WITSELF_* env vars, defaulting to the
// canonical ports :8080 (api), :8081 (health), and :9090 (metrics).
func ConfigFromEnv() Config {
	return Config{
		APIAddr:     envOr("WITSELF_API_ADDR", ":8080"),
		HealthAddr:  envOr("WITSELF_HEALTH_ADDR", ":8081"),
		MetricsAddr: envOr("WITSELF_METRICS_ADDR", ":9090"),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Run binds the three listeners, serves until ctx is cancelled (or a listener
// fails), then shuts them down gracefully.
func Run(ctx context.Context, cfg Config) error {
	defs := []struct {
		name, addr string
		handler    http.Handler
	}{
		{"api", cfg.APIAddr, apiMux(cfg)},
		{"health", cfg.HealthAddr, healthMux(cfg.Ready)},
		{"metrics", cfg.MetricsAddr, metricsMux()},
	}

	type running struct {
		name string
		srv  *http.Server
		ln   net.Listener
	}
	var servers []running
	for _, d := range defs {
		ln, err := net.Listen("tcp", d.addr)
		if err != nil {
			for _, r := range servers {
				_ = r.ln.Close()
			}
			return fmt.Errorf("%s listener %s: %w", d.name, d.addr, err)
		}
		servers = append(servers, running{
			name: d.name,
			srv:  &http.Server{Handler: d.handler, ReadHeaderTimeout: 5 * time.Second},
			ln:   ln,
		})
	}

	errc := make(chan error, len(servers))
	for _, r := range servers {
		fmt.Fprintf(os.Stderr, "witself-server: %s listening on %s\n", r.name, r.ln.Addr())
		go func() {
			if err := r.srv.Serve(r.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s: %w", r.name, err)
			}
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, r := range servers {
		_ = r.srv.Shutdown(shutCtx)
	}
	return runErr
}

func apiMux(cfg Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, "{\"schema_version\":\"witself.v0\",\"version\":%q,\"commit\":%q,\"date\":%q}\n",
			version.Version, version.Commit, version.Date)
	})
	selfDigestSupported := cfg.AuthenticatePrincipal != nil
	transcriptsSupported := selfDigestSupported &&
		cfg.CreateTranscript != nil && cfg.AppendTranscriptEntry != nil &&
		cfg.ListTranscripts != nil && cfg.GetTranscript != nil
	messagingSupported := selfDigestSupported && cfg.SendMessage != nil &&
		cfg.ListMessages != nil && cfg.ReadMessage != nil && cfg.AckMessage != nil
	messageListenSupported := selfDigestSupported && cfg.ListMessages != nil
	messageReplySupported := selfDigestSupported && cfg.ReplyMessage != nil
	messageProcessingSupported := selfDigestSupported &&
		cfg.ClaimMessage != nil && cfg.RenewMessageClaim != nil &&
		cfg.ReleaseMessageClaim != nil && cfg.CompleteMessage != nil
	memoriesSupported := selfDigestSupported && cfg.CaptureMemory != nil &&
		cfg.GetMemory != nil && cfg.ListMemories != nil && cfg.RecallMemories != nil &&
		cfg.GetMemoryHistory != nil && cfg.AdjustMemory != nil &&
		cfg.SupersedeMemory != nil &&
		cfg.ForgetMemory != nil && cfg.RestoreMemory != nil &&
		cfg.ReactivateMemory != nil && cfg.ResolveMemoryEvidence != nil &&
		cfg.DeleteMemory != nil
	memoryRecallSupported := selfDigestSupported && cfg.RecallMemories != nil
	memorySupersedeSupported := selfDigestSupported && cfg.SupersedeMemory != nil
	memoryDeleteSupported := selfDigestSupported && cfg.DeleteMemory != nil
	memoryVectorsSupported := selfDigestSupported && cfg.CreateMemoryVectorProfile != nil &&
		cfg.ListMemoryVectorProfiles != nil && cfg.PutMemoryVector != nil
	memoryCurationSupported := selfDigestSupported &&
		cfg.RequestMemoryCuration != nil && cfg.ListMemoryCurationRequests != nil &&
		cfg.GetMemoryCurationRequest != nil && cfg.StartMemoryCuration != nil &&
		cfg.GetMemoryCurationRun != nil && cfg.GetMemoryCurationRunInputs != nil &&
		cfg.RenewMemoryCuration != nil && cfg.PlanMemoryCuration != nil &&
		cfg.ApplyMemoryCuration != nil && cfg.CancelMemoryCuration != nil &&
		cfg.AbandonMemoryCuration != nil && cfg.RollbackMemoryCuration != nil &&
		cfg.GetMemoryCurationStatus != nil
	mux.HandleFunc("/v1/capabilities", capabilitiesHandler(cfg.AccountID,
		cfg.PlanInfo, selfDigestSupported, transcriptsSupported,
		messagingSupported, messageListenSupported, messageReplySupported, messageProcessingSupported,
		memoriesSupported, memoryRecallSupported, memorySupersedeSupported,
		memoryDeleteSupported, memoryCurationSupported, memoryVectorsSupported))
	if cfg.Login != nil {
		mux.HandleFunc("POST /v1/auth/bootstrap", bootstrapLoginHandler(cfg.Login))
	}
	if cfg.ProvisionToken != "" && cfg.ProvisionAccount != nil {
		mux.HandleFunc("POST /v1/accounts", provisionAccountHandler(cfg.ProvisionToken, cfg.ProvisionAccount))
		if cfg.ReapAccount != nil || cfg.ActivateAccount != nil || cfg.AccountContact != nil || cfg.RecoverAccount != nil || cfg.UpdateAccountEmail != nil || cfg.SuspendAccountSystem != nil || cfg.StreamAccountExport != nil || cfg.ImportAccountArchive != nil || cfg.ResumeAccountSystem != nil || cfg.LogAccountEvent != nil || cfg.SetAccountPlan != nil {
			mux.HandleFunc("POST /v1/accounts/", accountLifecycleHandler(cfg))
		}
		if cfg.GetPlacementPolicySystem != nil {
			mux.HandleFunc("GET /v1/accounts/", accountPlacementPolicySystemHandler(cfg.ProvisionToken, cfg.GetPlacementPolicySystem))
		}
		if cfg.SetPlacementPolicySystem != nil {
			mux.HandleFunc("PATCH /v1/accounts/", accountPlacementPolicySystemSetHandler(cfg.ProvisionToken, cfg.SetPlacementPolicySystem))
		}
	}
	if cfg.Authenticate != nil {
		whoami := whoamiHandler(cfg.Authenticate)
		mux.HandleFunc("GET /v1/whoami", whoami)
		mux.HandleFunc("GET /v1/auth/whoami", whoami)
		if cfg.CreateRealm != nil {
			mux.HandleFunc("POST /v1/realms", createRealmHandler(cfg.Authenticate, cfg.CreateRealm))
		}
		if cfg.ListRealms != nil {
			mux.HandleFunc("GET /v1/realms", listRealmsHandler(cfg.Authenticate, cfg.ListRealms))
		}
		if cfg.DeleteRealm != nil {
			mux.HandleFunc("DELETE /v1/realms/{realm}", deleteRealmHandler(cfg.Authenticate, cfg.DeleteRealm))
		}
		if cfg.CreateAgent != nil {
			mux.HandleFunc("POST /v1/realms/{realm}/agents", createAgentHandler(cfg.Authenticate, cfg.CreateAgent))
		}
		if cfg.ListAgents != nil {
			mux.HandleFunc("GET /v1/realms/{realm}/agents", listAgentsHandler(cfg.Authenticate, cfg.ListAgents))
		}
		if cfg.DeleteAgent != nil {
			mux.HandleFunc("DELETE /v1/realms/{realm}/agents/{agent}", deleteAgentHandler(cfg.Authenticate, cfg.DeleteAgent))
		}
		if cfg.CreateAgentToken != nil {
			mux.HandleFunc("POST /v1/agents/{agent}/tokens", createAgentTokenHandler(cfg.Authenticate, cfg.CreateAgentToken))
		}
		if cfg.CreateCuratorToken != nil {
			mux.HandleFunc("POST /v1/agents/{agent}/curator-tokens", createCuratorTokenHandler(cfg.Authenticate, cfg.CreateCuratorToken))
		}
		if cfg.CreateOperatorToken != nil {
			mux.HandleFunc("POST /v1/operators/self/tokens", createOperatorTokenHandler(cfg.Authenticate, cfg.CreateOperatorToken))
		}
		if cfg.ListAccountEvents != nil {
			mux.HandleFunc("GET /v1/account/events", listAccountEventsHandler(cfg.Authenticate, cfg.ListAccountEvents))
		}
		if cfg.ListOperators != nil {
			mux.HandleFunc("GET /v1/operators", listOperatorsHandler(cfg.Authenticate, cfg.ListOperators))
		}
		if cfg.CreateOperator != nil {
			mux.HandleFunc("POST /v1/operators", createOperatorHandler(cfg.Authenticate, cfg.CreateOperator))
		}
		if cfg.DeleteOperator != nil {
			mux.HandleFunc("DELETE /v1/operators/{operator}", deleteOperatorHandler(cfg.Authenticate, cfg.DeleteOperator))
		}
		if cfg.RevokeToken != nil {
			mux.HandleFunc("POST /v1/tokens/", revokeTokenHandler(cfg.Authenticate, cfg.RevokeToken))
		}
		if cfg.CloseAccount != nil {
			mux.HandleFunc("POST /v1/account:close", closeAccountHandler(cfg.Authenticate, cfg.CloseAccount))
		}
		if cfg.GetAccount != nil {
			mux.HandleFunc("GET /v1/account", getAccountHandler(cfg.Authenticate, cfg.GetAccount))
		}
		if cfg.RenameAccount != nil {
			mux.HandleFunc("POST /v1/account:rename", renameAccountHandler(cfg.Authenticate, cfg.RenameAccount))
		}
		if cfg.GetPlacementPolicy != nil {
			mux.HandleFunc("GET /v1/account/placement-policy", getPlacementPolicyHandler(cfg.Authenticate, cfg.GetPlacementPolicy))
		}
		if cfg.SetPlacementPolicy != nil {
			mux.HandleFunc("PATCH /v1/account/placement-policy", setPlacementPolicyHandler(cfg.Authenticate, cfg.SetPlacementPolicy))
		}
		if cfg.SuspendAccountOwner != nil {
			mux.HandleFunc("POST /v1/account:suspend", suspendAccountHandler(cfg.Authenticate, cfg.SuspendAccountOwner))
		}
		if cfg.ResumeAccountOwner != nil {
			mux.HandleFunc("POST /v1/account:resume", resumeAccountHandler(cfg.Authenticate, cfg.ResumeAccountOwner))
		}
		if cfg.OpenSupportTicket != nil {
			mux.HandleFunc("POST /v1/support/tickets", openSupportTicketHandler(cfg.Authenticate, cfg.OpenSupportTicket))
		}
		if cfg.ListSupportTickets != nil {
			mux.HandleFunc("GET /v1/support/tickets", listSupportTicketsHandler(cfg.Authenticate, cfg.ListSupportTickets))
		}
		if cfg.GetSupportTicket != nil {
			mux.HandleFunc("GET /v1/support/tickets/{ticket}", getSupportTicketHandler(cfg.Authenticate, cfg.GetSupportTicket))
		}
		if cfg.ReplySupportTicket != nil {
			mux.HandleFunc("POST /v1/support/tickets/{ticket}/messages", replySupportTicketHandler(cfg.Authenticate, cfg.ReplySupportTicket))
		}
		if cfg.ChangeSupportTicketState != nil {
			mux.HandleFunc("PATCH /v1/support/tickets/{ticket}/state", changeSupportTicketStateHandler(cfg.Authenticate, cfg.ChangeSupportTicketState))
		}
	}
	if cfg.AuthenticatePrincipal != nil {
		mux.HandleFunc("GET /v1/self", selfHandler(
			cfg.AuthenticatePrincipal,
			cfg.GetSelfFacts,
			cfg.CountSelfFacts,
			cfg.GetSelfMemories,
			cfg.CountSelfMemories,
		))
		if memoryCurationSupported {
			mux.HandleFunc("GET /v1/memory-curation-preflight", getMemoryCurationPreflightHandler(cfg.AuthenticatePrincipal))
		}
		if cfg.CaptureMemory != nil {
			mux.HandleFunc("POST /v1/memories", captureMemoryHandler(cfg.AuthenticatePrincipal, cfg.CaptureMemory))
		}
		if cfg.ListMemories != nil {
			mux.HandleFunc("GET /v1/memories", listMemoriesHandler(cfg.AuthenticatePrincipal, cfg.ListMemories))
		}
		if cfg.RecallMemories != nil {
			mux.HandleFunc("POST /v1/memories:recall", recallMemoriesHandler(cfg.AuthenticatePrincipal, cfg.RecallMemories))
		}
		if cfg.GetMemory != nil {
			mux.HandleFunc("GET /v1/memories/{memory}", getMemoryHandler(cfg.AuthenticatePrincipal, cfg.GetMemory))
		}
		if cfg.GetMemoryHistory != nil {
			mux.HandleFunc("GET /v1/memories/{memory}/history", memoryHistoryHandler(cfg.AuthenticatePrincipal, cfg.GetMemoryHistory))
		}
		if cfg.AdjustMemory != nil {
			mux.HandleFunc("PATCH /v1/memories/{memory}", adjustMemoryHandler(cfg.AuthenticatePrincipal, cfg.AdjustMemory))
		}
		if cfg.SupersedeMemory != nil {
			mux.HandleFunc("POST /v1/memories/{memory}/supersede", supersedeMemoryHandler(cfg.AuthenticatePrincipal, cfg.SupersedeMemory))
		}
		if cfg.ForgetMemory != nil || cfg.RestoreMemory != nil || cfg.ReactivateMemory != nil {
			mux.HandleFunc("POST /v1/memories/{action}", memoryLifecycleHandler(cfg.AuthenticatePrincipal,
				cfg.ForgetMemory, cfg.RestoreMemory, cfg.ReactivateMemory))
		}
		if cfg.ResolveMemoryEvidence != nil {
			mux.HandleFunc("POST /v1/memory-evidence/{evidence}/resolution", resolveMemoryEvidenceHandler(cfg.AuthenticatePrincipal, cfg.ResolveMemoryEvidence))
		}
		if cfg.DeleteMemory != nil {
			mux.HandleFunc("DELETE /v1/memories/{memory}", deleteMemoryHandler(cfg.AuthenticatePrincipal, cfg.DeleteMemory))
		}
		if cfg.CreateMemoryVectorProfile != nil {
			mux.HandleFunc("POST /v1/memory-vector-profiles", createMemoryVectorProfileHandler(cfg.AuthenticatePrincipal, cfg.CreateMemoryVectorProfile))
		}
		if cfg.ListMemoryVectorProfiles != nil {
			mux.HandleFunc("GET /v1/memory-vector-profiles", listMemoryVectorProfilesHandler(cfg.AuthenticatePrincipal, cfg.ListMemoryVectorProfiles))
		}
		if cfg.PutMemoryVector != nil {
			mux.HandleFunc("POST /v1/memory-vectors", putMemoryVectorHandler(cfg.AuthenticatePrincipal, cfg.PutMemoryVector))
		}
		if cfg.RequestMemoryCuration != nil {
			mux.HandleFunc("POST /v1/memory-curation-requests", requestMemoryCurationHandler(cfg.AuthenticatePrincipal, cfg.RequestMemoryCuration))
		}
		if cfg.ListMemoryCurationRequests != nil {
			mux.HandleFunc("GET /v1/memory-curation-requests", listMemoryCurationRequestsHandler(cfg.AuthenticatePrincipal, cfg.ListMemoryCurationRequests))
		}
		if cfg.GetMemoryCurationRequest != nil {
			mux.HandleFunc("GET /v1/memory-curation-requests/{request}", getMemoryCurationRequestHandler(cfg.AuthenticatePrincipal, cfg.GetMemoryCurationRequest))
		}
		if cfg.StartMemoryCuration != nil {
			mux.HandleFunc("POST /v1/memory-curation-requests/{request}/start", startMemoryCurationHandler(cfg.AuthenticatePrincipal, cfg.StartMemoryCuration))
		}
		if cfg.GetMemoryCurationRun != nil {
			mux.HandleFunc("GET /v1/memory-curation-runs/{run}", getMemoryCurationRunHandler(cfg.AuthenticatePrincipal, cfg.GetMemoryCurationRun))
		}
		if cfg.GetMemoryCurationRunInputs != nil {
			mux.HandleFunc("GET /v1/memory-curation-runs/{run}/inputs", getMemoryCurationRunInputsHandler(cfg.AuthenticatePrincipal, cfg.GetMemoryCurationRunInputs))
		}
		if cfg.RenewMemoryCuration != nil {
			mux.HandleFunc("POST /v1/memory-curation-runs/{run}/renew", renewMemoryCurationHandler(cfg.AuthenticatePrincipal, cfg.RenewMemoryCuration))
		}
		if cfg.PlanMemoryCuration != nil {
			mux.HandleFunc("POST /v1/memory-curation-runs/{run}/plan", planMemoryCurationHandler(cfg.AuthenticatePrincipal, cfg.PlanMemoryCuration))
		}
		if cfg.ApplyMemoryCuration != nil {
			mux.HandleFunc("POST /v1/memory-curation-runs/{run}/apply", applyMemoryCurationHandler(cfg.AuthenticatePrincipal, cfg.ApplyMemoryCuration))
		}
		if cfg.CancelMemoryCuration != nil {
			mux.HandleFunc("POST /v1/memory-curation-runs/{run}/cancel", finishMemoryCurationHandler(cfg.AuthenticatePrincipal, memoryCurationPermissionCancel, cfg.CancelMemoryCuration))
		}
		if cfg.AbandonMemoryCuration != nil {
			mux.HandleFunc("POST /v1/memory-curation-runs/{run}/abandon", finishMemoryCurationHandler(cfg.AuthenticatePrincipal, memoryCurationPermissionAbandon, cfg.AbandonMemoryCuration))
		}
		if cfg.RollbackMemoryCuration != nil {
			mux.HandleFunc("POST /v1/memory-curation-runs/{run}/rollback", rollbackMemoryCurationHandler(cfg.AuthenticatePrincipal, cfg.RollbackMemoryCuration))
		}
		if cfg.GetMemoryCurationStatus != nil {
			mux.HandleFunc("GET /v1/memory-curation-status", getMemoryCurationStatusHandler(cfg.AuthenticatePrincipal, cfg.GetMemoryCurationStatus))
		}
		if cfg.SetFact != nil {
			mux.HandleFunc("POST /v1/facts", setFactHandler(cfg.AuthenticatePrincipal, cfg.SetFact))
		}
		if cfg.DeleteFact != nil {
			mux.HandleFunc("DELETE /v1/facts", deleteFactHandler(cfg.AuthenticatePrincipal, cfg.DeleteFact))
			mux.HandleFunc("DELETE /v1/facts/{fact}", deleteFactHandler(cfg.AuthenticatePrincipal, cfg.DeleteFact))
		}
		if cfg.GetFact != nil && cfg.ListFacts != nil {
			mux.HandleFunc("GET /v1/facts", factsReadHandler(cfg.AuthenticatePrincipal, cfg.GetFact, cfg.ListFacts))
		}
		if cfg.GetFactHistory != nil {
			mux.HandleFunc("GET /v1/facts/{fact}/history", factHistoryHandler(cfg.AuthenticatePrincipal, cfg.GetFactHistory))
		}
		if cfg.UpcomingFacts != nil {
			mux.HandleFunc("GET /v1/fact-occurrences", upcomingFactsHandler(cfg.AuthenticatePrincipal, cfg.UpcomingFacts))
		}
		if cfg.ProposeFact != nil {
			mux.HandleFunc("POST /v1/fact-candidates", proposeFactHandler(cfg.AuthenticatePrincipal, cfg.ProposeFact))
		}
		if cfg.ListFactCandidates != nil {
			mux.HandleFunc("GET /v1/fact-candidates", listFactCandidatesHandler(cfg.AuthenticatePrincipal, cfg.ListFactCandidates))
		}
		if cfg.GetFactCandidate != nil {
			mux.HandleFunc("GET /v1/fact-candidates/{candidate}", getFactCandidateHandler(cfg.AuthenticatePrincipal, cfg.GetFactCandidate))
		}
		if cfg.ConfirmFactCandidate != nil && cfg.RejectFactCandidate != nil {
			mux.HandleFunc("POST /v1/fact-candidates/{action}", factCandidateActionHandler(cfg.AuthenticatePrincipal, cfg.ConfirmFactCandidate, cfg.RejectFactCandidate))
		}
		if cfg.UpsertFactSubject != nil {
			mux.HandleFunc("PUT /v1/fact-subjects/{subject}", upsertFactSubjectHandler(cfg.AuthenticatePrincipal, cfg.UpsertFactSubject))
		}
		if cfg.AddFactSubjectAlias != nil {
			mux.HandleFunc("POST /v1/fact-subjects/{subject}/aliases", addFactSubjectAliasHandler(cfg.AuthenticatePrincipal, cfg.AddFactSubjectAlias))
		}
		if cfg.ListFactSubjects != nil {
			mux.HandleFunc("GET /v1/fact-subjects", listFactSubjectsHandler(cfg.AuthenticatePrincipal, cfg.ListFactSubjects))
		}
		if cfg.GetUsage != nil {
			mux.HandleFunc("GET /v1/usage", usageHandler(cfg.AuthenticatePrincipal, cfg.GetUsage))
		}
		if cfg.CreateTranscript != nil {
			mux.HandleFunc("POST /v1/transcripts", createTranscriptHandler(cfg.AuthenticatePrincipal, cfg.CreateTranscript))
		}
		if cfg.ListTranscripts != nil {
			mux.HandleFunc("GET /v1/transcripts", listTranscriptsHandler(cfg.AuthenticatePrincipal, cfg.ListTranscripts))
		}
		if cfg.GetTranscriptPage != nil {
			mux.HandleFunc("GET /v1/transcripts/{transcript}", getTranscriptPageHandler(cfg.AuthenticatePrincipal, cfg.GetTranscriptPage))
		} else if cfg.GetTranscript != nil {
			mux.HandleFunc("GET /v1/transcripts/{transcript}", getTranscriptHandler(cfg.AuthenticatePrincipal, cfg.GetTranscript))
		}
		if cfg.AppendTranscriptEntry != nil {
			mux.HandleFunc("POST /v1/transcripts/{transcript}/entries", appendTranscriptEntryHandler(cfg.AuthenticatePrincipal, cfg.AppendTranscriptEntry))
		}
		if cfg.AppendTranscriptEntries != nil {
			mux.HandleFunc("POST /v1/transcripts/{transcript}/entries:batch", appendTranscriptEntriesHandler(cfg.AuthenticatePrincipal, cfg.AppendTranscriptEntries))
		}
		if cfg.SendMessage != nil {
			mux.HandleFunc("POST /v1/messages", sendMessageHandler(cfg.AuthenticatePrincipal, cfg.SendMessage))
		}
		if cfg.ListMessages != nil {
			mux.HandleFunc("GET /v1/messages", listMessagesHandler(cfg.AuthenticatePrincipal, cfg.ListMessages))
			mux.HandleFunc("POST /v1/messages:listen", messageListenHandler(cfg.AuthenticatePrincipal, cfg.ListMessages))
		}
		if cfg.ReadMessage != nil || cfg.AckMessage != nil || cfg.ReplyMessage != nil ||
			cfg.ClaimMessage != nil || cfg.RenewMessageClaim != nil ||
			cfg.ReleaseMessageClaim != nil || cfg.CompleteMessage != nil {
			mux.HandleFunc("POST /v1/messages/{action}", messageActionHandler(
				cfg.AuthenticatePrincipal, cfg.ReadMessage, cfg.AckMessage, cfg.ReplyMessage,
				cfg.ClaimMessage, cfg.RenewMessageClaim, cfg.ReleaseMessageClaim, cfg.CompleteMessage))
		}
	}
	// Provision-token-authorized cell-wide admin ticket list (feeds
	// the CP's fan-out for /v1/admin/tickets). Mounted independently of
	// the tenant support endpoints — a self-hosted cell without a
	// provision token never sees it.
	if cfg.ProvisionToken != "" && cfg.ListAdminTicketsAll != nil {
		mux.HandleFunc("POST /v1/support/admin:list-tickets",
			supportAdminCellHandler(cfg.ProvisionToken, cfg.ListAdminTicketsAll))
	}
	// Cell-wide audit tail for the fleet dashboard (provision-token
	// authorized, same pattern as the ticket fan-out source above).
	if cfg.ProvisionToken != "" && cfg.ListAdminEventsAll != nil {
		mux.HandleFunc("POST /v1/events/admin:list",
			eventsAdminCellHandler(cfg.ProvisionToken, cfg.ListAdminEventsAll))
	}
	return messagingNoStoreMux(mux)
}

// bootstrapLoginHandler exchanges a bootstrap token (JSON {"bootstrap_token"})
// for an operator token, shown once.
func bootstrapLoginHandler(login LoginFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			BootstrapToken string `json:"bootstrap_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.BootstrapToken == "" {
			writeJSONError(w, http.StatusBadRequest, "missing bootstrap_token")
			return
		}
		opTok, opID, ok, err := login(r.Context(), req.BootstrapToken)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "invalid or already-used bootstrap token")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"schema_version": "witself.v0",
			"operator_token": opTok,
			"operator_id":    opID,
		})
	}
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"schema_version": "witself.v0",
		"error":          msg,
	})
}

// backendInfo, feature, and capabilities describe the bare /v1/capabilities
// document. Like /v1/version it is flat — schema_version at the top level, no
// ok/data envelope — because the meta/discovery endpoints stay bare while the
// domain API uses the standard envelope. The feature map is static while
// subsystems are unbuilt and becomes config-driven as they land. backend.kind
// is a configured value (WITSELF_BACKEND_KIND), never something the server
// infers, and it is advisory: each feature is independently gated, so a
// mislabeled kind unlocks nothing — clients should branch on feature flags.
type backendInfo struct {
	Kind       string `json:"kind"`
	Version    string `json:"version"`
	APIVersion string `json:"api_version"`
}

type feature struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
}

// accountInfo identifies the deployment's account. On single-account backends
// (local, self-managed) it is the seeded default/root account; it is omitted
// when no database is configured.
type accountInfo struct {
	ID string `json:"id"`
	// Plan snapshot surfaced from the account record (empty = none applied).
	Plan         string   `json:"plan,omitempty"`
	PlanFeatures []string `json:"plan_features,omitempty"`
}

type capabilities struct {
	SchemaVersion string             `json:"schema_version"`
	Backend       backendInfo        `json:"backend"`
	Account       *accountInfo       `json:"account,omitempty"`
	Principal     any                `json:"principal"` // null until token auth exists
	Features      map[string]feature `json:"features"`
	Limits        map[string]any     `json:"limits"`
	Billing       billingInfo        `json:"billing"`
}

// billingInfo is the CLI's discovery block for plan/billing verbs: where to
// send them (the control plane) or why they are unavailable. The value comes
// from deployment CONFIG (WITSELF_BILLING_ENDPOINT), never from a billing
// provider — cells stay billing-ignorant; they only advertise what their
// deployment told them.
type billingInfo struct {
	Supported bool   `json:"supported"`
	Endpoint  string `json:"endpoint,omitempty"` // the control plane's API base
	Reason    string `json:"reason,omitempty"`   // e.g. "self_hosted"
}

func capabilitiesHandler(accountID string, planInfo func(ctx context.Context) (string, map[string]int64, []string, error), selfDigestSupported, transcriptsSupported, messagingSupported, messageListenSupported, messageReplySupported, messageProcessingSupported, memoriesSupported, memoryRecallSupported, memorySupersedeSupported, memoryDeleteSupported, memoryCurationSupported, memoryVectorsSupported bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		notImpl := feature{Reason: "not_implemented"}
		featureState := func(supported bool) feature {
			if supported {
				return feature{Supported: true}
			}
			return notImpl
		}
		caps := capabilities{
			SchemaVersion: "witself.v0",
			Backend: backendInfo{
				Kind:       envOr("WITSELF_BACKEND_KIND", "self-hosted"),
				Version:    version.Version,
				APIVersion: "v1",
			},
			Features: map[string]feature{
				"memories":                featureState(memoriesSupported),
				"memory_recall":           featureState(memoryRecallSupported),
				"memory_supersede":        featureState(memorySupersedeSupported),
				"memory_permanent_delete": featureState(memoryDeleteSupported),
				"memory_vector_profiles":  featureState(memoryVectorsSupported),
				"client_vector_recall":    featureState(memoryVectorsSupported && memoryRecallSupported),
				"automatic_capture":       notImpl,
				"opportunistic_curation":  featureState(memoryCurationSupported),
				"scheduled_curation":      notImpl,
				"transcript_capture":      featureState(transcriptsSupported),
				"facts":                   notImpl,
				"self_digest":             {Supported: selfDigestSupported},
				"semantic_recall":         featureState(memoryVectorsSupported && memoryRecallSupported),
				"policies":                notImpl,
				"groups":                  notImpl,
				"messaging":               {Supported: messagingSupported},
				"message_listen":          featureState(messageListenSupported),
				"message_reply":           featureState(messageReplySupported),
				"message_processing":      featureState(messageProcessingSupported),
				"transcripts":             {Supported: transcriptsSupported},
				"audit":                   notImpl,
			},
			Limits:  map[string]any{},
			Billing: billingInfo{Supported: false, Reason: "self_hosted"},
		}
		if ep := envOr("WITSELF_BILLING_ENDPOINT", ""); ep != "" {
			caps.Billing = billingInfo{Supported: true, Endpoint: ep}
		}
		if !transcriptsSupported {
			caps.Features["transcripts"] = notImpl
		}
		if !selfDigestSupported {
			caps.Features["self_digest"] = notImpl
		}
		if !messagingSupported {
			caps.Features["messaging"] = notImpl
		}
		if !memoriesSupported {
			caps.Features["memories"] = notImpl
		}
		if accountID != "" {
			caps.Account = &accountInfo{ID: accountID}
		}
		if planInfo != nil && caps.Account != nil {
			// Best-effort: capabilities is a discovery document, not a health
			// probe — a store hiccup degrades to "no plan surfaced".
			if plan, limits, features, err := planInfo(r.Context()); err == nil && plan != "" {
				caps.Account.Plan = plan
				for k, v := range limits {
					caps.Limits[k] = v
				}
				if len(features) > 0 {
					caps.Account.PlanFeatures = features
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(caps)
	}
}

type principal struct {
	operatorID    string
	accountID     string
	accountStatus string
}

// requireOperator authenticates the bearer token and passes the principal to
// h, or writes 401 (missing/invalid) / 500 (server fault). It also requires
// the account to be ACTIVE (-> 403 otherwise): a pending account can do
// nothing until its activation gates pass, and a suspended account can do
// nothing until it resumes. The exceptions — check status, close, suspend,
// resume — use requireOperatorAnyStatus instead. The refusal message names
// the current status verbatim so a suspended owner sees "account is
// suspended" and knows to reach for `witself account resume`.
func requireOperator(auth AuthFunc, h func(http.ResponseWriter, *http.Request, principal)) http.HandlerFunc {
	return requireOperatorAnyStatus(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		if p.accountStatus != "active" {
			writeJSONError(w, http.StatusForbidden,
				fmt.Sprintf("account is %s — this action requires an active account", p.accountStatus))
			return
		}
		h(w, r, p)
	})
}

// requireOperatorAnyStatus authenticates without gating on account status. Use
// only for the endpoints a not-yet-active or suspended account must still
// reach: checking its own status, closing itself, and (owner-initiated)
// suspending or resuming.
func requireOperatorAnyStatus(auth AuthFunc, h func(http.ResponseWriter, *http.Request, principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		operatorID, accountID, accountStatus, ok, err := auth(r.Context(), tok)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		h(w, r, principal{operatorID: operatorID, accountID: accountID, accountStatus: accountStatus})
	}
}

// requireDomainPrincipal authenticates an ordinary operator-or-agent token for
// domain surfaces and requires an active account. Restricted credentials fail
// closed here: only the purpose-built curation wrappers below may admit a
// curator profile. This means a future domain route cannot accidentally become
// available to an unattended curator merely because it uses the shared domain
// authentication helper.
func requireDomainPrincipal(auth PrincipalAuthFunc, h func(http.ResponseWriter, *http.Request, DomainPrincipal)) http.HandlerFunc {
	return requireDomainPrincipalAnyProfile(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if profile := effectiveAccessProfile(p); profile != AccessProfileFull {
			writeJSONError(w, http.StatusForbidden, "credential profile is not authorized for this route")
			return
		}
		h(w, r, p)
	})
}

// requireDomainPrincipalAnyProfile performs authentication and account-state
// gating without granting any route permission. Callers must make an explicit
// access-profile decision before invoking the domain handler.
func requireDomainPrincipalAnyProfile(auth PrincipalAuthFunc, h func(http.ResponseWriter, *http.Request, DomainPrincipal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		p, ok, err := auth(r.Context(), tok)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		if p.AccountStatus != "active" {
			writeJSONError(w, http.StatusForbidden,
				fmt.Sprintf("account is %s — this action requires an active account", p.AccountStatus))
			return
		}
		h(w, r, p)
	}
}

func effectiveAccessProfile(p DomainPrincipal) string {
	if strings.TrimSpace(p.AccessProfile) == "" {
		// Empty is accepted only for compatibility with embedders and tests that
		// predate token profiles. PostgreSQL-authenticated principals always
		// carry an explicit profile after migration 0031.
		return AccessProfileFull
	}
	return p.AccessProfile
}

func selfHandler(
	auth PrincipalAuthFunc,
	getFacts func(context.Context, DomainPrincipal, int) ([]SelfFact, int, error),
	countFacts func(context.Context, DomainPrincipal) (int, error),
	getMemories func(context.Context, DomainPrincipal, int) ([]SelfMemory, int, error),
	countMemories func(context.Context, DomainPrincipal) (int, error),
) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		// Self digests can contain durable personal context and must never be
		// retained by shared or private HTTP caches.
		w.Header().Set("Cache-Control", "private, no-store")
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may show self")
			return
		}

		q := r.URL.Query()
		for _, name := range []string{"include_facts", "include_salient"} {
			if value := q.Get(name); value != "" {
				if _, err := strconv.ParseBool(value); err != nil {
					writeJSONError(w, http.StatusBadRequest, name+" must be true or false")
					return
				}
			}
		}
		if value := q.Get("salient_limit"); value != "" {
			limit, err := strconv.Atoi(value)
			if err != nil || limit < 1 || limit > 100 {
				writeJSONError(w, http.StatusBadRequest, "salient_limit must be between 1 and 100")
				return
			}
		}
		salientLimit := 12
		if value := q.Get("salient_limit"); value != "" {
			salientLimit, _ = strconv.Atoi(value)
		}
		maximumBytes := 8192
		if value := q.Get("max_bytes"); value != "" {
			maximum, err := strconv.Atoi(value)
			if err != nil || maximum < 1024 || maximum > 65536 {
				writeJSONError(w, http.StatusBadRequest, "max_bytes must be between 1024 and 65536")
				return
			}
			maximumBytes = maximum
		}

		facts := []SelfFact{}
		factCount := 0
		includeFacts := true
		if raw := q.Get("include_facts"); raw != "" {
			includeFacts, _ = strconv.ParseBool(raw)
		}
		if includeFacts && getFacts != nil {
			hydratedFacts, total, err := getFacts(r.Context(), p, 50)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "could not hydrate facts")
				return
			}
			factCount = total
			if hydratedFacts != nil {
				facts = hydratedFacts
			}
			for i := range facts {
				facts[i].Primary = true
				if facts[i].Sensitive {
					facts[i].Value = nil
					facts[i].Redacted = true
				}
			}
		} else if countFacts != nil {
			var err error
			factCount, err = countFacts(r.Context(), p)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "could not count facts")
				return
			}
		} else if getFacts != nil {
			// Compatibility fallback for embedders that have not yet supplied the
			// count-only hook. The production adapter avoids loading fact values.
			_, total, err := getFacts(r.Context(), p, 50)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "could not count facts")
				return
			}
			factCount = total
		}

		memories := []SelfMemory{}
		memoryCount := 0
		includeSalient := true
		if raw := q.Get("include_salient"); raw != "" {
			includeSalient, _ = strconv.ParseBool(raw)
		}
		if includeSalient && getMemories != nil {
			hydratedMemories, total, err := getMemories(r.Context(), p, salientLimit)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "could not hydrate memories")
				return
			}
			memoryCount = total
			if hydratedMemories != nil {
				memories = hydratedMemories
			}
			for i := range memories {
				if memories[i].Sensitive {
					memories[i].Snippet = ""
					memories[i].Tags = nil
					memories[i].Redacted = true
				}
			}
		} else if countMemories != nil {
			var err error
			memoryCount, err = countMemories(r.Context(), p)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "could not count memories")
				return
			}
		} else if getMemories != nil {
			_, total, err := getMemories(r.Context(), p, salientLimit)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "could not count memories")
				return
			}
			memoryCount = total
		}

		kindSet := map[string]struct{}{}
		tagSet := map[string]struct{}{}
		for _, memory := range memories {
			if memory.Kind != "" {
				kindSet[memory.Kind] = struct{}{}
			}
			if memory.Sensitive {
				continue
			}
			for _, tag := range memory.Tags {
				if tag != "" {
					tagSet[tag] = struct{}{}
				}
			}
		}
		kinds := sortedSelfIndexKeys(kindSet)
		tags := sortedSelfIndexKeys(tagSet)
		digest := SelfDigest{
			SchemaVersion: "witself.v0",
			Identity: SelfIdentity{
				AccountID: p.AccountID,
				AgentID:   p.ID,
				AgentName: p.AgentName,
				RealmID:   p.RealmID,
				RealmName: p.RealmName,
			},
			PrimaryFacts:    facts,
			SalientMemories: memories,
			Index: SelfIndex{
				Kinds:  kinds,
				Tags:   tags,
				Counts: map[string]int{"facts": factCount, "memories": memoryCount},
			},
			Elided: (includeFacts && factCount > len(facts)) ||
				(includeSalient && memoryCount > len(memories)),
		}
		encoded, err := marshalBoundedSelfDigest(digest, maximumBytes)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not render self digest")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encoded)
	})
}

func sortedSelfIndexKeys(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// marshalBoundedSelfDigest applies the wire-size budget to the encoded JSON,
// preserving identity and the index while dropping the least essential
// hydrated entries from the end. A selected section that was already bounded
// by its store query must set Elided before calling this helper.
func marshalBoundedSelfDigest(digest SelfDigest, maximumBytes int) ([]byte, error) {
	for {
		encoded, err := json.Marshal(digest)
		if err != nil {
			return nil, err
		}
		if len(encoded) <= maximumBytes {
			return encoded, nil
		}
		if !digest.Elided {
			digest.Elided = true
			continue
		}
		switch {
		case len(digest.SalientMemories) > 0:
			digest.SalientMemories = digest.SalientMemories[:len(digest.SalientMemories)-1]
		case len(digest.PrimaryFacts) > 0:
			digest.PrimaryFacts = digest.PrimaryFacts[:len(digest.PrimaryFacts)-1]
		default:
			return nil, fmt.Errorf("self digest identity and index exceed %d bytes", maximumBytes)
		}
	}
}

func usageHandler(auth PrincipalAuthFunc, get func(context.Context, DomainPrincipal, UsageQuery) (UsageReport, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may read per-agent usage")
			return
		}
		q := r.URL.Query()
		query := UsageQuery{Bucket: strings.TrimSpace(q.Get("group_by"))}
		if raw := strings.TrimSpace(q.Get("since")); raw != "" {
			since, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "since must be RFC3339")
				return
			}
			query.Since = since
		}
		if raw := strings.TrimSpace(q.Get("until")); raw != "" {
			until, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "until must be RFC3339")
				return
			}
			query.Until = until
		}
		for _, raw := range q["dimension"] {
			for _, dimension := range strings.Split(raw, ",") {
				if dimension = strings.TrimSpace(dimension); dimension != "" {
					query.Dimensions = append(query.Dimensions, dimension)
				}
			}
		}
		report, err := get(r.Context(), p, query)
		switch {
		case errors.Is(err, ErrBadInput):
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, ErrForbidden):
			writeJSONError(w, http.StatusForbidden, "usage access forbidden")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not read usage")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"usage":          report,
		})
	})
}

func createTranscriptHandler(auth PrincipalAuthFunc, create func(context.Context, DomainPrincipal, CreateTranscriptRequest) (Transcript, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may create a transcript")
			return
		}
		var req CreateTranscriptRequest
		if err := decodeLimitedJSON(w, r, &req, 64*1024); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		metadata, err := transcriptMetadataWithPrincipal(req.Metadata, p)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "metadata must be a JSON object")
			return
		}
		req.Metadata = metadata
		tr, err := create(r.Context(), p, req)
		switch {
		case errors.Is(err, ErrBadInput):
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, ErrConflict):
			writeJSONError(w, http.StatusConflict, "a transcript with that external id already exists")
			return
		case errors.Is(err, ErrForbidden):
			writeJSONError(w, http.StatusForbidden, "transcript access forbidden")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not create transcript")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "transcript": tr})
	})
}

func transcriptMetadataWithPrincipal(raw json.RawMessage, p DomainPrincipal) (json.RawMessage, error) {
	metadata := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &metadata); err != nil || metadata == nil {
			return nil, errors.New("metadata is not an object")
		}
	}
	metadata["agent_id"] = p.ID
	metadata["agent_name"] = p.AgentName
	return json.Marshal(metadata)
}

func appendTranscriptEntryHandler(auth PrincipalAuthFunc, appendEntry func(context.Context, DomainPrincipal, string, AppendTranscriptEntryRequest) (TranscriptEntry, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may append a transcript entry")
			return
		}
		var req AppendTranscriptEntryRequest
		if err := decodeLimitedJSON(w, r, &req, 512*1024); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		entry, err := appendEntry(r.Context(), p, r.PathValue("transcript"), req)
		switch {
		case errors.Is(err, ErrBadInput):
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "transcript not found")
			return
		case errors.Is(err, ErrForbidden):
			writeJSONError(w, http.StatusForbidden, "transcript access forbidden")
			return
		case errors.Is(err, ErrConflict):
			writeJSONError(w, http.StatusConflict, "a transcript entry with that external id already exists")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not append transcript entry")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "entry": entry})
	})
}

func appendTranscriptEntriesHandler(auth PrincipalAuthFunc, appendEntries func(context.Context, DomainPrincipal, string, []AppendTranscriptEntryRequest) ([]TranscriptEntry, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may append transcript entries")
			return
		}
		var req struct {
			Entries []AppendTranscriptEntryRequest `json:"entries"`
		}
		if err := decodeLimitedJSON(w, r, &req, 8*1024*1024); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		entries, err := appendEntries(r.Context(), p, r.PathValue("transcript"), req.Entries)
		switch {
		case errors.Is(err, ErrBadInput):
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "transcript not found")
			return
		case errors.Is(err, ErrForbidden):
			writeJSONError(w, http.StatusForbidden, "transcript access forbidden")
			return
		case errors.Is(err, ErrConflict):
			writeJSONError(w, http.StatusConflict, "a transcript entry external id was reused with different content")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not append transcript entries")
			return
		}
		if entries == nil {
			entries = []TranscriptEntry{}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "entries": entries})
	})
}

func listTranscriptsHandler(auth PrincipalAuthFunc, list func(context.Context, DomainPrincipal) ([]Transcript, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		transcripts, err := list(r.Context(), p)
		if errors.Is(err, ErrForbidden) {
			writeJSONError(w, http.StatusForbidden, "transcript access forbidden")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not list transcripts")
			return
		}
		if transcripts == nil {
			transcripts = []Transcript{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "transcripts": transcripts})
	})
}

func getTranscriptHandler(auth PrincipalAuthFunc, get func(context.Context, DomainPrincipal, string) (Transcript, []TranscriptEntry, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		tr, entries, err := get(r.Context(), p, r.PathValue("transcript"))
		switch {
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "transcript not found")
			return
		case errors.Is(err, ErrForbidden):
			writeJSONError(w, http.StatusForbidden, "transcript access forbidden")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not read transcript")
			return
		}
		if entries == nil {
			entries = []TranscriptEntry{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"transcript":     tr,
			"entries":        entries,
		})
	})
}

func getTranscriptPageHandler(auth PrincipalAuthFunc, get func(context.Context, DomainPrincipal, string, TranscriptPageOptions) (TranscriptPage, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		opts, err := transcriptPageOptions(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		page, err := get(r.Context(), p, r.PathValue("transcript"), opts)
		switch {
		case errors.Is(err, ErrBadInput):
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "transcript not found")
			return
		case errors.Is(err, ErrForbidden):
			writeJSONError(w, http.StatusForbidden, "transcript access forbidden")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not read transcript")
			return
		}
		if page.Entries == nil {
			page.Entries = []TranscriptEntry{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version":      "witself.v0",
			"transcript":          page.Transcript,
			"entries":             page.Entries,
			"next_after_sequence": page.NextAfterSequence,
		})
	})
}

func transcriptPageOptions(r *http.Request) (TranscriptPageOptions, error) {
	var opts TranscriptPageOptions
	q := r.URL.Query()
	if raw := strings.TrimSpace(q.Get("after_sequence")); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			return TranscriptPageOptions{}, errors.New("after_sequence must be a non-negative integer")
		}
		opts.AfterSequence = value
	}
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 500 {
			return TranscriptPageOptions{}, errors.New("limit must be between 1 and 500")
		}
		opts.Limit = value
	}
	if raw := strings.TrimSpace(q.Get("tail")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return TranscriptPageOptions{}, errors.New("tail must be true or false")
		}
		opts.Tail = value
	}
	if opts.Tail && opts.AfterSequence != 0 {
		return TranscriptPageOptions{}, errors.New("tail and after_sequence are mutually exclusive")
	}
	return opts, nil
}

func decodeLimitedJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.HasPrefix(h, prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

// provisionAccountHandler creates a new account on this cell. It is authorized
// by the pre-shared provision token (the control plane's credential), never by
// operator/agent tokens — provisioning is instance-level authority, above any
// account.
func provisionAccountHandler(provisionToken string, provision func(ctx context.Context, email, displayName string) (ProvisionedAccount, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(provisionToken)) != 1 {
			writeJSONError(w, http.StatusUnauthorized, "invalid provision token")
			return
		}
		var req struct {
			Email       string `json:"email"`
			DisplayName string `json:"display_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		req.Email = strings.TrimSpace(strings.ToLower(req.Email))
		if req.Email == "" || !strings.Contains(req.Email, "@") {
			writeJSONError(w, http.StatusBadRequest, "valid email required")
			return
		}
		if req.DisplayName == "" {
			req.DisplayName = req.Email
		}
		acct, err := provision(r.Context(), req.Email, req.DisplayName)
		if errors.Is(err, ErrConflict) {
			writeJSONError(w, http.StatusConflict, "an account with this email already exists")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not provision account")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"account":        acct,
		})
	}
}

func accountPlacementPolicySystemHandler(provisionToken string, get func(ctx context.Context, accountID string) (placement.Policy, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(provisionToken)) != 1 {
			writeJSONError(w, http.StatusUnauthorized, "invalid provision token")
			return
		}
		accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "placement-policy")
		if !ok {
			writeJSONError(w, http.StatusNotFound, "not found")
			return
		}
		policy, err := get(r.Context(), accountID)
		switch {
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "account not found")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not read placement policy")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version":   "witself.v0",
			"account_id":       accountID,
			"placement_policy": policy,
		})
	}
}

func accountPlacementPolicySystemSetHandler(provisionToken string, set func(ctx context.Context, accountID string, policy placement.Policy) (placement.Policy, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(provisionToken)) != 1 {
			writeJSONError(w, http.StatusUnauthorized, "invalid provision token")
			return
		}
		accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "placement-policy")
		if !ok {
			writeJSONError(w, http.StatusNotFound, "not found")
			return
		}
		var req placement.Policy
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		policy, err := set(r.Context(), accountID, req)
		switch {
		case errors.Is(err, placement.ErrInvalidPolicy):
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "account not found")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not restore placement policy")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version":   "witself.v0",
			"account_id":       accountID,
			"placement_policy": policy,
		})
	}
}

// accountLifecycleHandler serves the provision-token-authorized lifecycle
// verbs on POST /v1/accounts/{id}:reap|:activate|:contact|:update-email|
// :recover|:suspend|:resume|:export|:import|:events — instance-level authority,
// the same trust link as provisioning, so only the control plane can call
// them.
//
// reap: 200 whether it reaped just now or found the account already closed
// (idempotent); 409 when the account activated first (the sweep's view was
// stale — the cell is truth); 404 for an unknown id.
//
// activate: 200 whether it activated just now or was already active (an
// idempotent second click); 409 when the account is closed or otherwise
// ineligible (the link outlived its account); 404 for an unknown id.
//
// contact: 200 with the account's email and status — the read the recovery
// request needs when no operator token exists.
//
// recover: 200 with a fresh root-bound bootstrap token after rotating every
// live root credential; 409 unless the account is active.
func accountLifecycleHandler(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(cfg.ProvisionToken)) != 1 {
			writeJSONError(w, http.StatusUnauthorized, "invalid provision token")
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "reap"); ok && cfg.ReapAccount != nil {
			reaped, err := cfg.ReapAccount(r.Context(), accountID)
			switch {
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case errors.Is(err, ErrConflict):
				writeJSONError(w, http.StatusConflict, "account is active")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not reap account")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"account_id":     accountID,
				"status":         "closed",
				"reaped":         reaped,
			})
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "activate"); ok && cfg.ActivateAccount != nil {
			activated, err := cfg.ActivateAccount(r.Context(), accountID)
			switch {
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case errors.Is(err, ErrConflict):
				writeJSONError(w, http.StatusConflict, "account cannot be activated")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not activate account")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"account_id":     accountID,
				"status":         "active",
				"activated":      activated,
			})
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "contact"); ok && cfg.AccountContact != nil {
			rec, err := cfg.AccountContact(r.Context(), accountID)
			switch {
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not read account")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"account_id":     rec.ID,
				"email":          rec.Email,
				"status":         rec.Status,
			})
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "update-email"); ok && cfg.UpdateAccountEmail != nil {
			var req struct {
				OperatorID      string `json:"operator_id"`
				NewEmail        string `json:"new_email"`
				Undo            bool   `json:"undo"`
				ExpectedCurrent string `json:"expected_current"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
				return
			}
			req.NewEmail = strings.TrimSpace(strings.ToLower(req.NewEmail))
			req.ExpectedCurrent = strings.TrimSpace(strings.ToLower(req.ExpectedCurrent))
			if req.NewEmail == "" || !strings.Contains(req.NewEmail, "@") {
				writeJSONError(w, http.StatusBadRequest, "a valid new_email required")
				return
			}
			var err error
			if req.Undo {
				if req.ExpectedCurrent == "" || cfg.UndoAccountEmail == nil {
					writeJSONError(w, http.StatusBadRequest, "expected_current required for undo")
					return
				}
				err = cfg.UndoAccountEmail(r.Context(), accountID, req.ExpectedCurrent, req.NewEmail)
			} else {
				if req.OperatorID == "" {
					writeJSONError(w, http.StatusBadRequest, "operator_id required")
					return
				}
				err = cfg.UpdateAccountEmail(r.Context(), accountID, req.OperatorID, req.NewEmail)
			}
			switch {
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case errors.Is(err, ErrNotAccountOwner):
				writeJSONError(w, http.StatusForbidden, "only the account owner can change the email")
				return
			case errors.Is(err, ErrConflict):
				writeJSONError(w, http.StatusConflict, "account is not active")
				return
			case errors.Is(err, ErrEmailChangedSinceUndo):
				writeJSONError(w, http.StatusConflict, "the email has changed since this undo link was issued")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not update email")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"schema_version": "witself.v0",
				"account_id":     accountID,
				"email":          req.NewEmail,
			})
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "recover"); ok && cfg.RecoverAccount != nil {
			acct, err := cfg.RecoverAccount(r.Context(), accountID)
			switch {
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case errors.Is(err, ErrConflict):
				writeJSONError(w, http.StatusConflict, "account is not active")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not recover account")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"account":        acct,
			})
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "plan"); ok && cfg.SetAccountPlan != nil {
			var req struct {
				Plan     string           `json:"plan"`
				Limits   map[string]int64 `json:"limits"`
				Features []string         `json:"features"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Plan) == "" {
				writeJSONError(w, http.StatusBadRequest, "a plan is required")
				return
			}
			err := cfg.SetAccountPlan(r.Context(), accountID, strings.TrimSpace(req.Plan), req.Limits, req.Features)
			switch {
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not set account plan")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"account_id":     accountID,
				"plan":           strings.TrimSpace(req.Plan),
				"limits":         req.Limits,
				"features":       req.Features,
			})
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "suspend"); ok && cfg.SuspendAccountSystem != nil {
			var req struct {
				For    string `json:"for"`
				Reason string `json:"reason"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.For == "" {
				writeJSONError(w, http.StatusBadRequest, "a suspension category (for) is required")
				return
			}
			err := cfg.SuspendAccountSystem(r.Context(), accountID, req.For, strings.TrimSpace(req.Reason))
			switch {
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case errors.Is(err, ErrCannotCloseDefault):
				writeJSONError(w, http.StatusForbidden, "the deployment's default account cannot be suspended")
				return
			case errors.Is(err, ErrConflict):
				writeJSONError(w, http.StatusConflict, "account is pending — not suspendable")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not suspend account")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"schema_version": "witself.v0",
				"account_id":     accountID,
				"status":         "suspended",
			})
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "resume"); ok && cfg.ResumeAccountSystem != nil {
			var req struct {
				For string `json:"for"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.For == "" {
				writeJSONError(w, http.StatusBadRequest, "a suspension category (for) is required")
				return
			}
			err := cfg.ResumeAccountSystem(r.Context(), accountID, req.For)
			switch {
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case errors.Is(err, ErrAccountNotSuspended):
				writeJSONError(w, http.StatusConflict, "account is not suspended")
				return
			case errors.Is(err, ErrResumeWrongCategory):
				writeJSONError(w, http.StatusConflict, "suspension category does not match")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not resume account")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"schema_version": "witself.v0",
				"account_id":     accountID,
				"status":         "active",
			})
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "import"); ok && cfg.ImportAccountArchive != nil {
			sum, err := cfg.ImportAccountArchive(r.Context(), accountID, r.Body)
			switch {
			case errors.Is(err, ErrConflict):
				writeJSONError(w, http.StatusConflict, "account already exists on this cell")
				return
			case errors.Is(err, ErrArchiveTooNew):
				writeJSONError(w, http.StatusConflict, "archive schema is newer than this cell — upgrade the cell first")
				return
			case errors.Is(err, ErrBadArchive):
				writeJSONError(w, http.StatusBadRequest, "invalid or corrupt archive")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not import account")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version":         "witself.v0",
				"account_id":             sum.AccountID,
				"status":                 sum.Status,
				"archive_schema_version": sum.SchemaVersion,
			})
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "events"); ok && cfg.LogAccountEvent != nil {
			var req struct {
				Verb      string         `json:"verb"`
				ActorKind string         `json:"actor_kind"`
				Metadata  map[string]any `json:"metadata"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
				return
			}
			if req.Verb == "" || req.ActorKind == "" {
				writeJSONError(w, http.StatusBadRequest, "verb and actor_kind are required")
				return
			}
			err := cfg.LogAccountEvent(r.Context(), accountID, req.Verb, req.ActorKind, req.Metadata)
			switch {
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case errors.Is(err, ErrBadInput):
				// verb/actor/metadata mismatch — the caller has to fix
				// the caller, not swallow the error.
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not record account event")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"schema_version": "witself.v0",
				"account_id":     accountID,
				"logged":         "true",
			})
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "export"); ok && cfg.StreamAccountExport != nil {
			// Preconditions surface as JSON errors; once streaming begins,
			// the archive's own trailing checksums are the integrity story.
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("X-Witself-Export-Format", "1")
			err := cfg.StreamAccountExport(r.Context(), accountID, w)
			switch {
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case errors.Is(err, ErrConflict):
				writeJSONError(w, http.StatusConflict, "account must be suspended (or closed) to export")
				return
			case err != nil:
				// Headers may already be sent; the truncated stream fails
				// the trailing-checksum verification downstream.
				return
			}
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "admin:list-tickets"); ok && cfg.ListAdminTickets != nil {
			var req struct {
				AdminHandle string `json:"admin_handle"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if err := validateAdminHandle(req.AdminHandle); err != nil {
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			}
			tickets, err := cfg.ListAdminTickets(r.Context(), accountID)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "could not list support tickets")
				return
			}
			if tickets == nil {
				tickets = []SupportTicket{}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"tickets":        tickets,
			})
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "admin:get-ticket"); ok && cfg.GetAdminTicket != nil {
			var req struct {
				AdminHandle string `json:"admin_handle"`
				TicketID    string `json:"ticket_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
				return
			}
			if err := validateAdminHandle(req.AdminHandle); err != nil {
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			}
			if strings.TrimSpace(req.TicketID) == "" {
				writeJSONError(w, http.StatusBadRequest, "missing ticket_id")
				return
			}
			ticket, messages, err := cfg.GetAdminTicket(r.Context(), accountID, req.TicketID)
			switch {
			case errors.Is(err, ErrTicketNotFound):
				writeJSONError(w, http.StatusNotFound, "ticket not found")
				return
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not read support ticket")
				return
			}
			if messages == nil {
				messages = []SupportTicketMessage{}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"ticket":         ticket,
				"messages":       messages,
			})
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "admin:reply-ticket"); ok && cfg.ReplyAdminTicket != nil {
			var req struct {
				AdminHandle string `json:"admin_handle"`
				TicketID    string `json:"ticket_id"`
				Body        string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
				return
			}
			if err := validateAdminHandle(req.AdminHandle); err != nil {
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			}
			if strings.TrimSpace(req.TicketID) == "" {
				writeJSONError(w, http.StatusBadRequest, "missing ticket_id")
				return
			}
			msg, err := cfg.ReplyAdminTicket(r.Context(), ReplyAdminTicketRequest{
				AccountID:   accountID,
				AdminHandle: req.AdminHandle,
				TicketID:    req.TicketID,
				Body:        req.Body,
			})
			switch {
			case errors.Is(err, ErrTicketInputInvalid):
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			case errors.Is(err, ErrTicketNotFound):
				writeJSONError(w, http.StatusNotFound, "ticket not found")
				return
			case errors.Is(err, ErrTicketStateInvalid):
				writeJSONError(w, http.StatusConflict, err.Error())
				return
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not reply to support ticket")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"message":        msg,
			})
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "admin:change-ticket-state"); ok && cfg.ChangeAdminTicketState != nil {
			var req struct {
				AdminHandle string `json:"admin_handle"`
				TicketID    string `json:"ticket_id"`
				State       string `json:"state"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
				return
			}
			if err := validateAdminHandle(req.AdminHandle); err != nil {
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			}
			if strings.TrimSpace(req.TicketID) == "" {
				writeJSONError(w, http.StatusBadRequest, "missing ticket_id")
				return
			}
			if strings.TrimSpace(req.State) == "" {
				writeJSONError(w, http.StatusBadRequest, "missing state")
				return
			}
			ticket, err := cfg.ChangeAdminTicketState(r.Context(), ChangeAdminTicketStateRequest{
				AccountID:   accountID,
				AdminHandle: req.AdminHandle,
				TicketID:    req.TicketID,
				NewState:    strings.TrimSpace(req.State),
			})
			switch {
			case errors.Is(err, ErrTicketStateInvalid):
				writeJSONError(w, http.StatusConflict, err.Error())
				return
			case errors.Is(err, ErrTicketNotFound):
				writeJSONError(w, http.StatusNotFound, "ticket not found")
				return
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not change ticket state")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"ticket":         ticket,
			})
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "admin:get-support-policy"); ok && cfg.GetAdminSupportPolicy != nil {
			var req struct {
				AdminHandle string `json:"admin_handle"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if err := validateAdminHandle(req.AdminHandle); err != nil {
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			}
			policy, err := cfg.GetAdminSupportPolicy(r.Context(), accountID)
			switch {
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not read support_policy")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"account_id":     accountID,
				"support_policy": policy,
			})
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "admin:set-support-policy"); ok && cfg.SetAdminSupportPolicy != nil {
			var req struct {
				AdminHandle string `json:"admin_handle"`
				Policy      string `json:"policy"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
				return
			}
			if err := validateAdminHandle(req.AdminHandle); err != nil {
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			}
			if strings.TrimSpace(req.Policy) == "" {
				writeJSONError(w, http.StatusBadRequest, "missing policy")
				return
			}
			result, err := cfg.SetAdminSupportPolicy(r.Context(), SetAdminSupportPolicyRequest{
				AccountID:   accountID,
				AdminHandle: req.AdminHandle,
				NewPolicy:   strings.TrimSpace(req.Policy),
			})
			switch {
			case errors.Is(err, ErrTicketInputInvalid):
				// Reused: store returns ErrTicketInputInvalid for
				// unknown policy strings — same "bad input" bucket.
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not set support_policy")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"account_id":     result.AccountID,
				"policy_from":    result.PolicyFrom,
				"policy_to":      result.PolicyTo,
			})
			return
		}
		// Deliberately distinct from the handlers' "account not found": the
		// control plane treats that exact string as authoritative and this
		// one as retryable, so a cell that doesn't serve an action yet never
		// burns a verification link or a reap candidate.
		writeJSONError(w, http.StatusNotFound, "unknown account action")
	}
}

// eventsAdminCellHandler serves POST /v1/events/admin:list — the
// cell-wide audit-event tail the control plane fans out to for the
// fleet dashboard's events pane. Body carries optional filters
// (since, until, verb, limit) and a page_token cursor.
func eventsAdminCellHandler(provisionToken string, list func(ctx context.Context, filter EventFilter) (EventPage, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(provisionToken)) != 1 {
			writeJSONError(w, http.StatusUnauthorized, "invalid provision token")
			return
		}
		var req struct {
			AdminHandle string `json:"admin_handle"`
			Since       string `json:"since"`
			Until       string `json:"until"`
			Verb        string `json:"verb"`
			Limit       int    `json:"limit"`
			PageToken   string `json:"page_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if err := validateAdminHandle(req.AdminHandle); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		filter := EventFilter{
			Verb:   req.Verb,
			Limit:  req.Limit,
			Cursor: req.PageToken,
		}
		if s := strings.TrimSpace(req.Since); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "since must be RFC3339")
				return
			}
			filter.Since = &t
		}
		if s := strings.TrimSpace(req.Until); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "until must be RFC3339")
				return
			}
			filter.Until = &t
		}
		page, err := list(r.Context(), filter)
		switch {
		case errors.Is(err, ErrBadInput):
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not list events")
			return
		}
		if page.Events == nil {
			page.Events = []Event{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version":  "witself.v0",
			"events":          page.Events,
			"next_page_token": page.NextCursor,
		})
	}
}

// validateAdminHandle checks that s matches the ADMIN_HANDLE shape the
// control plane uses when minting an admin. Cell-side is defense in
// depth; the CP already runs the same check.
func validateAdminHandle(s string) error {
	if len(s) < 2 || len(s) > 32 {
		return errors.New("invalid admin_handle (2-32 chars)")
	}
	if s[0] < 'a' || s[0] > 'z' {
		return errors.New("invalid admin_handle (must start with a lowercase letter)")
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-'
		if !ok {
			return errors.New("invalid admin_handle (letters/digits/underscore/hyphen only)")
		}
	}
	return nil
}

// supportAdminCellHandler serves POST /v1/support/admin:list-tickets —
// the cell-wide admin ticket list used by the control-plane fan-out.
// Provision-token authorized like the other admin routes; body carries
// optional filters (states, since, limit) and a page_token for cursor
// pagination.
func supportAdminCellHandler(provisionToken string, list func(ctx context.Context, in ListAdminTicketsAllRequest) (ListAdminTicketsAllResult, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(provisionToken)) != 1 {
			writeJSONError(w, http.StatusUnauthorized, "invalid provision token")
			return
		}
		var req struct {
			AdminHandle string   `json:"admin_handle"`
			States      []string `json:"states"`
			Since       string   `json:"since"`
			Limit       int      `json:"limit"`
			PageToken   string   `json:"page_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if err := validateAdminHandle(req.AdminHandle); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		var since *time.Time
		if s := strings.TrimSpace(req.Since); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "since must be RFC3339")
				return
			}
			since = &t
		}
		result, err := list(r.Context(), ListAdminTicketsAllRequest{
			States:    req.States,
			Since:     since,
			Limit:     req.Limit,
			PageToken: req.PageToken,
		})
		switch {
		case errors.Is(err, ErrTicketStateInvalid):
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, ErrBadInput):
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not list support tickets")
			return
		}
		if result.Tickets == nil {
			result.Tickets = []SupportTicket{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version":  "witself.v0",
			"tickets":         result.Tickets,
			"next_page_token": result.NextPageToken,
		})
	}
}

// suspendAccountHandler freezes every write on the account at the owner's
// request. Owner-only; the seeded default account is refused; only-if-active.
// Reachable at any status the caller reaches with (a pending caller gets a
// 409, not a 403) — the auth gate lets it through, the store adjudicates.
func suspendAccountHandler(auth AuthFunc, suspend func(ctx context.Context, accountID, operatorID, reason string) error) http.HandlerFunc {
	return requireOperatorAnyStatus(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		var req struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req) // body optional
		err := suspend(r.Context(), p.accountID, p.operatorID, strings.TrimSpace(req.Reason))
		switch {
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "account not found")
			return
		case errors.Is(err, ErrNotAccountOwner):
			writeJSONError(w, http.StatusForbidden, "only the account owner can suspend the account")
			return
		case errors.Is(err, ErrCannotCloseDefault):
			writeJSONError(w, http.StatusForbidden, "the deployment's default account cannot be suspended")
			return
		case errors.Is(err, ErrAccountNotActive):
			writeJSONError(w, http.StatusConflict, "account is not active — nothing to suspend")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not suspend account")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"schema_version": "witself.v0",
			"account_id":     p.accountID,
			"status":         "suspended",
		})
	})
}

// resumeAccountHandler undoes an owner-initiated suspension. Owner-only, and
// refuses to un-suspend a suspension initiated by a different authority
// (fleet-admin, migration, ...) — the authority that suspended is the one
// that resumes.
func resumeAccountHandler(auth AuthFunc, resume func(ctx context.Context, accountID, operatorID string) error) http.HandlerFunc {
	return requireOperatorAnyStatus(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		err := resume(r.Context(), p.accountID, p.operatorID)
		switch {
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "account not found")
			return
		case errors.Is(err, ErrNotAccountOwner):
			writeJSONError(w, http.StatusForbidden, "only the account owner can resume the account")
			return
		case errors.Is(err, ErrCannotSelfResume):
			writeJSONError(w, http.StatusForbidden, "this suspension was not initiated by you — the authority that suspended must resume")
			return
		case errors.Is(err, ErrAccountNotSuspended):
			writeJSONError(w, http.StatusConflict, "account is not suspended")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not resume account")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"schema_version": "witself.v0",
			"account_id":     p.accountID,
			"status":         "active",
		})
	})
}

// closeAccountHandler permanently closes the authenticated operator's account:
// tombstone + revoke every live credential. Owner-only; the seeded default
// account is refused. Idempotent. Not status-gated: a pending account can
// always be abandoned by its owner.
func closeAccountHandler(auth AuthFunc, closeAccount func(ctx context.Context, accountID, operatorID, reason string) error) http.HandlerFunc {
	return requireOperatorAnyStatus(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		var req struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req) // body optional
		err := closeAccount(r.Context(), p.accountID, p.operatorID, req.Reason)
		switch {
		case errors.Is(err, ErrNotAccountOwner):
			writeJSONError(w, http.StatusForbidden, "only the account owner can close the account")
			return
		case errors.Is(err, ErrCannotCloseDefault):
			writeJSONError(w, http.StatusForbidden, "the deployment's default account cannot be closed")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not close account")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"schema_version": "witself.v0",
			"account_id":     p.accountID,
			"status":         "closed",
		})
	})
}

// getAccountHandler returns the authenticated operator's account lifecycle
// record. Deliberately not status-gated: a pending account's owner checks here
// whether activation has happened yet.
func getAccountHandler(auth AuthFunc, get func(ctx context.Context, accountID string) (AccountRecord, error)) http.HandlerFunc {
	return requireOperatorAnyStatus(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		rec, err := get(r.Context(), p.accountID)
		if errors.Is(err, ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "account not found")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not read account")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "account": rec})
	})
}

// renameAccountHandler changes the account's server-side display name.
// Owner-only, active-only (requireOperator's gate covers the latter).
func renameAccountHandler(auth AuthFunc, rename func(ctx context.Context, accountID, operatorID, displayName string) error) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		var req struct {
			DisplayName string `json:"display_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		req.DisplayName = strings.TrimSpace(req.DisplayName)
		if req.DisplayName == "" {
			writeJSONError(w, http.StatusBadRequest, "missing display_name")
			return
		}
		err := rename(r.Context(), p.accountID, p.operatorID, req.DisplayName)
		switch {
		case errors.Is(err, ErrNotAccountOwner):
			writeJSONError(w, http.StatusForbidden, "only the account owner can rename the account")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not rename account")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"schema_version": "witself.v0",
			"account_id":     p.accountID,
			"display_name":   req.DisplayName,
		})
	})
}

func getPlacementPolicyHandler(auth AuthFunc, get func(ctx context.Context, accountID, operatorID string) (placement.Policy, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		policy, err := get(r.Context(), p.accountID, p.operatorID)
		switch {
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "account not found")
			return
		case errors.Is(err, ErrNotAccountOwner):
			writeJSONError(w, http.StatusForbidden, "only the account owner may view placement policy")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not read placement policy")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version":   "witself.v0",
			"placement_policy": policy,
		})
	})
}

func setPlacementPolicyHandler(auth AuthFunc, set func(ctx context.Context, accountID, operatorID string, policy placement.Policy) (placement.Policy, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		var req placement.Policy
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		policy, err := set(r.Context(), p.accountID, p.operatorID, req)
		switch {
		case errors.Is(err, placement.ErrInvalidPolicy):
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "account not found")
			return
		case errors.Is(err, ErrNotAccountOwner):
			writeJSONError(w, http.StatusForbidden, "only the account owner may change placement policy")
			return
		case errors.Is(err, ErrAccountNotActive):
			writeJSONError(w, http.StatusForbidden, "account is not active")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not change placement policy")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version":   "witself.v0",
			"placement_policy": policy,
		})
	})
}

// whoamiHandler returns the authenticated operator principal, or 401.
func whoamiHandler(auth AuthFunc) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, _ *http.Request, p principal) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"principal": map[string]string{
				"kind":        "operator",
				"operator_id": p.operatorID,
				"account_id":  p.accountID,
			},
		})
	})
}

func createRealmHandler(auth AuthFunc, create func(ctx context.Context, accountID, name string) (Realm, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			writeJSONError(w, http.StatusBadRequest, "missing name")
			return
		}
		realm, err := create(r.Context(), p.accountID, req.Name)
		if errors.Is(err, ErrConflict) {
			writeJSONError(w, http.StatusConflict, "realm already exists")
			return
		}
		if errors.Is(err, ErrPlanLimit) {
			// The message names the cap and the plan; pass it through so the
			// refusal explains itself.
			writeJSONError(w, http.StatusForbidden, err.Error())
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not create realm")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "realm": realm})
	})
}

// listAccountEventsHandler serves the owner's audit trail. Query params
// (all optional): since, until (RFC3339), verb (exact), limit (default
// 50, capped at 500 in the store), after (opaque cursor from a previous
// page's next_cursor). Non-owner operators are refused with 403 —
// audit visibility is an owner-tier privilege, not a general operator
// one.
func listAccountEventsHandler(auth AuthFunc, list func(ctx context.Context, accountID, operatorID string, filter EventFilter) (EventPage, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		q := r.URL.Query()
		filter := EventFilter{
			Verb:   q.Get("verb"),
			Cursor: q.Get("after"),
		}
		if s := q.Get("since"); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "since must be RFC3339 (e.g. 2026-01-02T15:04:05Z)")
				return
			}
			filter.Since = &t
		}
		if s := q.Get("until"); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "until must be RFC3339 (e.g. 2026-01-02T15:04:05Z)")
				return
			}
			filter.Until = &t
		}
		if s := q.Get("limit"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 {
				writeJSONError(w, http.StatusBadRequest, "limit must be a positive integer")
				return
			}
			filter.Limit = n
		}
		page, err := list(r.Context(), p.accountID, p.operatorID, filter)
		if err != nil {
			switch {
			case errors.Is(err, ErrNotAccountOwner):
				writeJSONError(w, http.StatusForbidden, "only the account owner may view the audit trail")
			case errors.Is(err, ErrBadInput):
				writeJSONError(w, http.StatusBadRequest, err.Error())
			default:
				writeJSONError(w, http.StatusInternalServerError, "could not list account events")
			}
			return
		}
		if page.Events == nil {
			page.Events = []Event{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"events":         page.Events,
			"next_cursor":    page.NextCursor,
		})
	})
}

func listRealmsHandler(auth AuthFunc, list func(ctx context.Context, accountID string) ([]Realm, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		realms, err := list(r.Context(), p.accountID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not list realms")
			return
		}
		if realms == nil {
			realms = []Realm{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "realms": realms})
	})
}

func deleteRealmHandler(auth AuthFunc, deleteRealm func(ctx context.Context, accountID, realmID string) error) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		err := deleteRealm(r.Context(), p.accountID, r.PathValue("realm"))
		switch {
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "realm not found")
			return
		case errors.Is(err, ErrConflict):
			writeJSONError(w, http.StatusConflict, "realm is not empty")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not delete realm")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func createAgentHandler(auth AuthFunc, create func(ctx context.Context, accountID, realmID, name string) (Agent, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			writeJSONError(w, http.StatusBadRequest, "missing name")
			return
		}
		agent, err := create(r.Context(), p.accountID, r.PathValue("realm"), req.Name)
		switch {
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "realm not found")
			return
		case errors.Is(err, ErrConflict):
			writeJSONError(w, http.StatusConflict, "agent already exists")
			return
		case errors.Is(err, ErrPlanLimit):
			// The message names the cap and the plan; pass it through so the
			// refusal explains itself.
			writeJSONError(w, http.StatusForbidden, err.Error())
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not create agent")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "agent": agent})
	})
}

func listAgentsHandler(auth AuthFunc, list func(ctx context.Context, accountID, realmID string) ([]Agent, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		agents, err := list(r.Context(), p.accountID, r.PathValue("realm"))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not list agents")
			return
		}
		if agents == nil {
			agents = []Agent{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "agents": agents})
	})
}

func deleteAgentHandler(auth AuthFunc, deleteAgent func(ctx context.Context, accountID, realmID, agentID string) error) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		err := deleteAgent(r.Context(), p.accountID, r.PathValue("realm"), r.PathValue("agent"))
		switch {
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not delete agent")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func createAgentTokenHandler(auth AuthFunc, create func(ctx context.Context, accountID, actorOperatorID, agentID string) (string, string, string, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		agentID := r.PathValue("agent")
		tok, tokenID, agentName, err := create(r.Context(), p.accountID, p.operatorID, agentID)
		if errors.Is(err, ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		}
		if errors.Is(err, ErrAccountNotActive) {
			writeJSONError(w, http.StatusForbidden, "account is not active")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not create token")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"schema_version": "witself.v0",
			"agent_token":    tok,
			"token_id":       tokenID,
			"agent_id":       agentID,
			"agent_name":     agentName,
		})
	})
}

func createCuratorTokenHandler(
	auth AuthFunc,
	create func(context.Context, string, string, string, string, string, time.Duration) (string, string, string, time.Time, error),
) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		var req struct {
			AccessProfile string `json:"access_profile"`
			DisplayName   string `json:"display_name"`
			TTL           string `json:"ttl"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid curator token request")
			return
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			writeJSONError(w, http.StatusBadRequest, "invalid curator token request")
			return
		}
		req.AccessProfile = strings.TrimSpace(req.AccessProfile)
		req.DisplayName = strings.TrimSpace(req.DisplayName)
		if req.AccessProfile != AccessProfileCuratorPreview && req.AccessProfile != AccessProfileCuratorApply {
			writeJSONError(w, http.StatusBadRequest, "access_profile must be curator-preview or curator-apply")
			return
		}
		if req.DisplayName == "" || len(req.DisplayName) > 255 {
			writeJSONError(w, http.StatusBadRequest, "display_name is required and must not exceed 255 bytes")
			return
		}
		for _, char := range req.DisplayName {
			if unicode.IsControl(char) {
				writeJSONError(w, http.StatusBadRequest, "display_name must not contain control characters")
				return
			}
		}
		ttl, err := time.ParseDuration(strings.TrimSpace(req.TTL))
		if err != nil || ttl <= 0 || ttl > 24*time.Hour {
			writeJSONError(w, http.StatusBadRequest, "ttl must be greater than zero and no more than 24h")
			return
		}

		agentID := strings.TrimSpace(r.PathValue("agent"))
		if agentID == "" {
			writeJSONError(w, http.StatusBadRequest, "agent id is required")
			return
		}
		tok, tokenID, agentName, expiresAt, err := create(
			r.Context(), p.accountID, p.operatorID, agentID,
			req.AccessProfile, req.DisplayName, ttl,
		)
		switch {
		case errors.Is(err, ErrBadInput):
			writeJSONError(w, http.StatusBadRequest, "invalid curator token request")
			return
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		case errors.Is(err, ErrAccountNotActive):
			writeJSONError(w, http.StatusForbidden, "account is not active")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not create curator token")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, no-store")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"agent_token":    tok,
			"token_id":       tokenID,
			"agent_id":       agentID,
			"agent_name":     agentName,
			"access_profile": req.AccessProfile,
			"display_name":   req.DisplayName,
			"expires_at":     expiresAt,
		})
	})
}

func createOperatorTokenHandler(auth AuthFunc, create func(ctx context.Context, accountID, operatorID, displayName string, ttl *time.Duration) (string, string, *time.Time, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		var req struct {
			DisplayName string `json:"display_name,omitempty"`
			TTL         string `json:"ttl,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		displayName := strings.TrimSpace(req.DisplayName)

		var ttl *time.Duration
		if req.TTL != "" {
			d, err := time.ParseDuration(req.TTL)
			if err != nil || d <= 0 {
				writeJSONError(w, http.StatusBadRequest, "invalid ttl")
				return
			}
			ttl = &d
		}

		tok, tokenID, expiresAt, err := create(r.Context(), p.accountID, p.operatorID, displayName, ttl)
		if errors.Is(err, ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "operator not found")
			return
		}
		if errors.Is(err, ErrAccountNotActive) {
			writeJSONError(w, http.StatusForbidden, "account is not active")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not create token")
			return
		}
		out := map[string]string{
			"schema_version": "witself.v0",
			"operator_token": tok,
			"operator_id":    p.operatorID,
			"token_id":       tokenID,
			"display_name":   displayName,
		}
		if expiresAt != nil {
			out["expires_at"] = expiresAt.Format(time.RFC3339)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(out)
	})
}

func listOperatorsHandler(auth AuthFunc, list func(ctx context.Context, accountID string) ([]Operator, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		operators, err := list(r.Context(), p.accountID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not list operators")
			return
		}
		if operators == nil {
			operators = []Operator{}
		}
		for i := range operators {
			if operators[i].Tokens == nil {
				operators[i].Tokens = []OperatorToken{}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "operators": operators})
	})
}

func createOperatorHandler(auth AuthFunc, create func(ctx context.Context, accountID, actorOperatorID, displayName, tokenDisplayName string, ttl *time.Duration) (Operator, string, *time.Time, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		var req struct {
			DisplayName      string `json:"display_name"`
			TokenDisplayName string `json:"token_display_name,omitempty"`
			TTL              string `json:"ttl,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		displayName := strings.TrimSpace(req.DisplayName)
		if displayName == "" {
			writeJSONError(w, http.StatusBadRequest, "missing display_name")
			return
		}
		tokenDisplayName := strings.TrimSpace(req.TokenDisplayName)
		if tokenDisplayName == "" {
			tokenDisplayName = displayName
		}

		var ttl *time.Duration
		if req.TTL != "" {
			d, err := time.ParseDuration(req.TTL)
			if err != nil || d <= 0 {
				writeJSONError(w, http.StatusBadRequest, "invalid ttl")
				return
			}
			ttl = &d
		}

		operator, token, expiresAt, err := create(r.Context(), p.accountID, p.operatorID, displayName, tokenDisplayName, ttl)
		if errors.Is(err, ErrAccountNotActive) {
			writeJSONError(w, http.StatusForbidden, "account is not active")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not create operator")
			return
		}
		out := map[string]any{
			"schema_version":   "witself.v0",
			"operator":         operator,
			"operator_token":   token,
			"token_expires_at": expiresAt,
		}
		if expiresAt == nil {
			delete(out, "token_expires_at")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(out)
	})
}

func deleteOperatorHandler(auth AuthFunc, deleteOperator func(ctx context.Context, accountID, actorOperatorID, targetOperatorID string) error) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		err := deleteOperator(r.Context(), p.accountID, p.operatorID, r.PathValue("operator"))
		switch {
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "operator not found")
			return
		case errors.Is(err, ErrCannotDeleteSelf):
			writeJSONError(w, http.StatusConflict, "cannot delete the authenticated operator")
			return
		case errors.Is(err, ErrCannotDeleteRoot):
			writeJSONError(w, http.StatusConflict, "cannot delete the root operator")
			return
		case errors.Is(err, ErrLastOperator):
			writeJSONError(w, http.StatusConflict, "cannot delete the last operator")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not delete operator")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func revokeTokenHandler(auth AuthFunc, revoke func(ctx context.Context, accountID, actorOperatorID, tokenID string) error) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		tokenID, ok := tokenActionID(r.URL.Path, "revoke")
		if !ok {
			writeJSONError(w, http.StatusNotFound, "token not found")
			return
		}
		err := revoke(r.Context(), p.accountID, p.operatorID, tokenID)
		switch {
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "token not found")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not revoke token")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func tokenActionID(path, action string) (string, bool) {
	return pathActionID(path, "/v1/tokens/", action)
}

// pathActionID extracts the id from "{prefix}{id}:{action}" (or the
// "/{action}" spelling), refusing empty or multi-segment ids.
func pathActionID(path, prefix, action string) (string, bool) {
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(path, prefix)
	for _, suffix := range []string{":" + action, "/" + action} {
		if strings.HasSuffix(rest, suffix) {
			id := strings.TrimSuffix(rest, suffix)
			if id != "" && !strings.Contains(id, "/") {
				return id, true
			}
		}
	}
	return "", false
}

// Support-ticket handlers. Every one runs behind requireOperator so the
// account membership + active-status check happens up front. Ticket
// visibility is any-operator (locked with the product decision) so no
// extra role guard is needed here.

func openSupportTicketHandler(auth AuthFunc, open func(ctx context.Context, in OpenTicketRequest) (SupportTicket, SupportTicketMessage, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		var req struct {
			Subject  string `json:"subject"`
			Category string `json:"category"`
			Priority string `json:"priority"`
			Body     string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		ticket, msg, err := open(r.Context(), OpenTicketRequest{
			AccountID:  p.accountID,
			OperatorID: p.operatorID,
			Subject:    req.Subject,
			Category:   req.Category,
			Priority:   req.Priority,
			Body:       req.Body,
		})
		switch {
		case errors.Is(err, ErrTicketInputInvalid):
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, ErrSupportDisabled):
			writeJSONError(w, http.StatusConflict, "support is not enabled for this account")
			return
		case errors.Is(err, ErrNotAccountOwner):
			// Store returns this when the operator no longer belongs
			// to the account (race with a delete). It's not truly an
			// owner-only rule here, just a membership check.
			writeJSONError(w, http.StatusForbidden, "operator is not a member of this account")
			return
		case errors.Is(err, ErrAccountNotActive):
			writeJSONError(w, http.StatusForbidden, "account is not active")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not open support ticket")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version":  "witself.v0",
			"ticket":          ticket,
			"initial_message": msg,
		})
	})
}

func listSupportTicketsHandler(auth AuthFunc, list func(ctx context.Context, accountID, operatorID string) ([]SupportTicket, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		tickets, err := list(r.Context(), p.accountID, p.operatorID)
		switch {
		case errors.Is(err, ErrNotAccountOwner):
			writeJSONError(w, http.StatusForbidden, "operator is not a member of this account")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not list support tickets")
			return
		}
		if tickets == nil {
			tickets = []SupportTicket{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"tickets":        tickets,
		})
	})
}

func getSupportTicketHandler(auth AuthFunc, get func(ctx context.Context, accountID, operatorID, ticketID string) (SupportTicket, []SupportTicketMessage, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		ticketID := r.PathValue("ticket")
		if ticketID == "" {
			writeJSONError(w, http.StatusBadRequest, "missing ticket id")
			return
		}
		ticket, messages, err := get(r.Context(), p.accountID, p.operatorID, ticketID)
		switch {
		case errors.Is(err, ErrTicketNotFound):
			writeJSONError(w, http.StatusNotFound, "ticket not found")
			return
		case errors.Is(err, ErrNotAccountOwner):
			writeJSONError(w, http.StatusForbidden, "operator is not a member of this account")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not read support ticket")
			return
		}
		if messages == nil {
			messages = []SupportTicketMessage{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"ticket":         ticket,
			"messages":       messages,
		})
	})
}

func replySupportTicketHandler(auth AuthFunc, reply func(ctx context.Context, accountID, operatorID, ticketID, body string) (SupportTicketMessage, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		ticketID := r.PathValue("ticket")
		if ticketID == "" {
			writeJSONError(w, http.StatusBadRequest, "missing ticket id")
			return
		}
		var req struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		msg, err := reply(r.Context(), p.accountID, p.operatorID, ticketID, req.Body)
		switch {
		case errors.Is(err, ErrTicketInputInvalid):
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, ErrTicketNotFound):
			writeJSONError(w, http.StatusNotFound, "ticket not found")
			return
		case errors.Is(err, ErrTicketStateInvalid):
			writeJSONError(w, http.StatusConflict, err.Error())
			return
		case errors.Is(err, ErrNotAccountOwner):
			writeJSONError(w, http.StatusForbidden, "operator is not a member of this account")
			return
		case errors.Is(err, ErrAccountNotActive):
			writeJSONError(w, http.StatusForbidden, "account is not active")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not reply to support ticket")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"message":        msg,
		})
	})
}

func changeSupportTicketStateHandler(auth AuthFunc, changeState func(ctx context.Context, in ChangeTicketStateRequest) (SupportTicket, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		ticketID := r.PathValue("ticket")
		if ticketID == "" {
			writeJSONError(w, http.StatusBadRequest, "missing ticket id")
			return
		}
		var req struct {
			State string `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.State) == "" {
			writeJSONError(w, http.StatusBadRequest, "missing state")
			return
		}
		ticket, err := changeState(r.Context(), ChangeTicketStateRequest{
			AccountID:  p.accountID,
			OperatorID: p.operatorID,
			TicketID:   ticketID,
			NewState:   strings.TrimSpace(req.State),
		})
		switch {
		case errors.Is(err, ErrTicketStateInvalid):
			writeJSONError(w, http.StatusConflict, err.Error())
			return
		case errors.Is(err, ErrTicketNotFound):
			writeJSONError(w, http.StatusNotFound, "ticket not found")
			return
		case errors.Is(err, ErrNotAccountOwner):
			writeJSONError(w, http.StatusForbidden, "operator is not a member of this account")
			return
		case errors.Is(err, ErrAccountNotActive):
			writeJSONError(w, http.StatusForbidden, "account is not active")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not change ticket state")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"ticket":         ticket,
		})
	})
}

func healthMux(ready func(context.Context) error) http.Handler {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	}
	// Liveness and startup never gate on dependencies — a DB blip must not
	// restart the pod, only pull it from the load balancer via readiness.
	mux.HandleFunc("/livez", ok)
	mux.HandleFunc("/startupz", ok)
	mux.HandleFunc("/healthz", ok) // convenience alias
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if ready != nil {
			if err := ready(r.Context()); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprintf(w, "not ready: %v\n", err)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

func metricsMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = fmt.Fprint(w,
			"# HELP witself_up 1 if the witself-server process is up.\n"+
				"# TYPE witself_up gauge\n"+
				"witself_up 1\n")
	})
	return mux
}
