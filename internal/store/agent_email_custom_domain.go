package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/witwave-ai/witself/internal/agentemail"
)

const (
	// AgentEmailRecipientRouteCustomDomain records delivery through a verified
	// account domain bound to an existing realm-alias identity.
	AgentEmailRecipientRouteCustomDomain = "custom_domain"

	// AgentEmailCustomDomainRouteApplied makes the exact route available.
	AgentEmailCustomDomainRouteApplied = "applied"
	// AgentEmailCustomDomainRouteSuspended preserves a temporarily unavailable route.
	AgentEmailCustomDomainRouteSuspended = "suspended"
	// AgentEmailCustomDomainRouteRetired permanently tombstones the route identity.
	AgentEmailCustomDomainRouteRetired = "retired"

	// AgentEmailCustomDomainSuspensionRetry preserves a known route but asks
	// the provider to retry while its global authority is temporarily unsafe.
	AgentEmailCustomDomainSuspensionRetry = "retry"
	// AgentEmailCustomDomainSuspensionInactive treats the route as inactive and
	// returns a permanent unknown-recipient result without revealing why.
	AgentEmailCustomDomainSuspensionInactive = "inactive"
)

var (
	// ErrAgentEmailCustomDomainRouteNotFound reports an unknown exact route
	// projection identity.
	ErrAgentEmailCustomDomainRouteNotFound = errors.New("agent-email custom-domain route not found")
	// ErrAgentEmailCustomDomainRouteConflict reports a stale, misbound, or
	// terminal route projection.
	ErrAgentEmailCustomDomainRouteConflict = errors.New("agent-email custom-domain route conflict")
)

// AgentEmailCustomDomainRoute is the cell's exact acknowledgement of the
// control-plane projection joining one domain allocation to one realm alias.
// The source revisions are part of the fence and retired rows are permanent
// identity tombstones.
type AgentEmailCustomDomainRoute struct {
	DomainRequestID          string     `json:"domain_request_id"`
	RealmAliasClaimID        string     `json:"realm_alias_claim_id"`
	AccountID                string     `json:"account_id"`
	RealmID                  string     `json:"realm_id"`
	Domain                   string     `json:"domain"`
	RealmLabel               string     `json:"realm_label"`
	DomainAllocationRevision int64      `json:"domain_allocation_revision"`
	DomainStateRevision      int64      `json:"domain_state_revision"`
	RealmAliasRevision       int64      `json:"realm_alias_revision"`
	State                    string     `json:"state"`
	SuspensionDisposition    string     `json:"suspension_disposition,omitempty"`
	ControllerRevision       int64      `json:"controller_revision"`
	CreatedAt                time.Time  `json:"created_at"`
	UpdatedAt                time.Time  `json:"updated_at"`
	SuspendedAt              *time.Time `json:"suspended_at,omitempty"`
	RetiredAt                *time.Time `json:"retired_at,omitempty"`
}

// ApplyAgentEmailCustomDomainRouteInput carries one exact, monotonic desired
// projection. AccountID remains path-bound at the system API.
type ApplyAgentEmailCustomDomainRouteInput struct {
	DomainRequestID          string
	DomainAllocationRevision int64
	DomainStateRevision      int64
	RealmAliasClaimID        string
	RealmAliasRevision       int64
	RealmID                  string
	Domain                   string
	RealmLabel               string
	State                    string
	SuspensionDisposition    string
	ControllerRevision       int64
}

// ApplyAgentEmailCustomDomainRoute converges one control-plane projection.
// Same-revision exact replay is idempotent; all lower revisions and any
// same-revision mismatch fail closed. The referenced realm-alias projection
// must be present at the exact advertised source revision before an applied
// custom route can become routable.
func (s *Store) ApplyAgentEmailCustomDomainRoute(
	ctx context.Context,
	accountID string,
	in ApplyAgentEmailCustomDomainRouteInput,
) (AgentEmailCustomDomainRoute, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" || !validAgentEmailGeneratedID(in.DomainRequestID, "aedr") ||
		!validAgentEmailGeneratedID(in.RealmAliasClaimID, "era") ||
		!validRealmID(in.RealmID) ||
		!validAgentEmailProjectionRevision(in.DomainAllocationRevision) ||
		!validAgentEmailProjectionRevision(in.DomainStateRevision) ||
		!validAgentEmailProjectionRevision(in.RealmAliasRevision) ||
		!validAgentEmailProjectionRevision(in.ControllerRevision) {
		return AgentEmailCustomDomainRoute{}, fmt.Errorf(
			"%w: invalid custom-domain route projection envelope",
			ErrAgentEmailInputInvalid,
		)
	}
	domain, err := agentemail.ValidateDomain(in.Domain)
	if err != nil || domain != in.Domain {
		return AgentEmailCustomDomainRoute{}, fmt.Errorf(
			"%w: domain must be canonical lowercase ASCII", ErrAgentEmailInputInvalid,
		)
	}
	label, err := agentemail.ValidateRealmAliasLabel(in.RealmLabel)
	if err != nil || label != in.RealmLabel {
		return AgentEmailCustomDomainRoute{}, fmt.Errorf(
			"%w: realm label is invalid", ErrAgentEmailInputInvalid,
		)
	}
	if !validAgentEmailCustomDomainRouteState(in.State) ||
		!validAgentEmailCustomDomainSuspension(in.State, in.SuspensionDisposition) {
		return AgentEmailCustomDomainRoute{}, fmt.Errorf(
			"%w: custom-domain route lifecycle is invalid", ErrAgentEmailInputInvalid,
		)
	}
	in.DomainRequestID = strings.TrimSpace(in.DomainRequestID)
	in.RealmAliasClaimID = strings.TrimSpace(in.RealmAliasClaimID)
	in.RealmID = strings.TrimSpace(in.RealmID)
	in.Domain = domain
	in.RealmLabel = label
	in.State = strings.TrimSpace(in.State)
	in.SuspensionDisposition = strings.TrimSpace(in.SuspensionDisposition)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEmailCustomDomainRoute{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialize all custom-domain projections for one account and inherit the
	// account evacuation fence. Projection convergence is allowed while the
	// account is pending or suspended; ingestion independently requires active
	// account state, entitlement, enrolled realm, and enrolled agent.
	var lockedAccountID string
	err = tx.QueryRow(ctx, `SELECT id FROM accounts WHERE id=$1 FOR NO KEY UPDATE`, accountID).
		Scan(&lockedAccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailCustomDomainRoute{}, ErrAccountNotFound
	}
	if err != nil {
		return AgentEmailCustomDomainRoute{}, fmt.Errorf("lock custom-domain route account: %w", err)
	}
	if err := lockAgentEmailRouteNamespaceTx(ctx, tx, domain, label); err != nil {
		return AgentEmailCustomDomainRoute{}, err
	}
	_, managedAliasErr := agentEmailRealmAliasByLabelTx(
		ctx, tx, domain, label, true,
	)
	if managedAliasErr != nil && !errors.Is(
		managedAliasErr, ErrAgentEmailRealmAliasNotFound,
	) {
		return AgentEmailCustomDomainRoute{}, managedAliasErr
	}
	if managedAliasErr == nil {
		return AgentEmailCustomDomainRoute{}, ErrAgentEmailCustomDomainRouteConflict
	}
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(
		  hashtextextended('witself:agent-email:custom-domain-request:' || $1,0)
		)`, in.DomainRequestID); err != nil {
		return AgentEmailCustomDomainRoute{}, fmt.Errorf("lock custom-domain request identity: %w", err)
	}
	var requestAccountID, requestDomain string
	err = tx.QueryRow(ctx, `
		SELECT account_id,domain
		  FROM agent_email_custom_domain_routes
		 WHERE domain_request_id=$1
		 ORDER BY realm_alias_claim_id
		 LIMIT 1`, in.DomainRequestID).Scan(&requestAccountID, &requestDomain)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailCustomDomainRoute{}, fmt.Errorf("read custom-domain request identity: %w", err)
	}
	if err == nil && (requestAccountID != accountID || requestDomain != domain) {
		return AgentEmailCustomDomainRoute{}, ErrAgentEmailCustomDomainRouteConflict
	}

	alias, err := agentEmailRealmAliasByClaimTx(ctx, tx, in.RealmAliasClaimID, true)
	if errors.Is(err, ErrAgentEmailRealmAliasNotFound) {
		return AgentEmailCustomDomainRoute{}, ErrAgentEmailCustomDomainRouteConflict
	}
	if err != nil {
		return AgentEmailCustomDomainRoute{}, err
	}
	if alias.AccountID != accountID || alias.RealmID != in.RealmID ||
		alias.RealmLabel != label || alias.Domain == domain {
		return AgentEmailCustomDomainRoute{}, ErrAgentEmailCustomDomainRouteConflict
	}

	existing, existingErr := agentEmailCustomDomainRouteByIdentityTx(
		ctx, tx, in.DomainRequestID, in.RealmAliasClaimID, true,
	)
	if existingErr != nil && !errors.Is(existingErr, ErrAgentEmailCustomDomainRouteNotFound) {
		return AgentEmailCustomDomainRoute{}, existingErr
	}
	byLabel, labelErr := agentEmailCustomDomainRouteByLabelTx(ctx, tx, domain, label, true)
	if labelErr != nil && !errors.Is(labelErr, ErrAgentEmailCustomDomainRouteNotFound) {
		return AgentEmailCustomDomainRoute{}, labelErr
	}

	if existingErr == nil {
		if !agentEmailCustomDomainRouteBindingMatches(existing, accountID, in) ||
			(labelErr == nil && (byLabel.DomainRequestID != existing.DomainRequestID ||
				byLabel.RealmAliasClaimID != existing.RealmAliasClaimID)) ||
			in.ControllerRevision < existing.ControllerRevision ||
			(in.ControllerRevision == existing.ControllerRevision &&
				!agentEmailCustomDomainRouteDesiredMatches(existing, in)) ||
			in.DomainAllocationRevision < existing.DomainAllocationRevision ||
			in.DomainStateRevision < existing.DomainStateRevision ||
			in.RealmAliasRevision < existing.RealmAliasRevision ||
			(existing.State == AgentEmailCustomDomainRouteRetired &&
				in.State != AgentEmailCustomDomainRouteRetired) {
			return AgentEmailCustomDomainRoute{}, ErrAgentEmailCustomDomainRouteConflict
		}
		if in.ControllerRevision == existing.ControllerRevision {
			// A lost acknowledgement remains replayable after its source alias or
			// realm has advanced. The immutable route row is the authoritative
			// proof for this already-accepted controller revision.
			if err := tx.Commit(ctx); err != nil {
				return AgentEmailCustomDomainRoute{}, err
			}
			return existing, nil
		}
	}

	// New projections and revision advances must fence against the exact source
	// alias revision currently projected into this cell. Exact replays above do
	// not revalidate newer source state.
	if alias.ControllerRevision != in.RealmAliasRevision {
		return AgentEmailCustomDomainRoute{}, ErrAgentEmailCustomDomainRouteConflict
	}
	var realmRouteState string
	var realmDeletedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT email_route_state,deleted_at
		  FROM realms
		 WHERE account_id=$1 AND id=$2`, accountID, in.RealmID).
		Scan(&realmRouteState, &realmDeletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailCustomDomainRoute{}, ErrRealmNotFound
	}
	if err != nil {
		return AgentEmailCustomDomainRoute{}, fmt.Errorf("resolve custom-domain route realm: %w", err)
	}
	if in.State == AgentEmailCustomDomainRouteApplied &&
		(alias.State != AgentEmailRealmAliasApplied || realmDeletedAt != nil ||
			realmRouteState != RealmEmailRouteLive) {
		return AgentEmailCustomDomainRoute{}, ErrAgentEmailCustomDomainRouteConflict
	}

	if existingErr == nil {
		updated, err := updateAgentEmailCustomDomainRouteTx(ctx, tx, existing, in)
		if err != nil {
			return AgentEmailCustomDomainRoute{}, err
		}
		if err := s.logAgentEmailCustomDomainRouteProjectionTx(ctx, tx, updated); err != nil {
			return AgentEmailCustomDomainRoute{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return AgentEmailCustomDomainRoute{}, err
		}
		return updated, nil
	}
	if labelErr == nil {
		return AgentEmailCustomDomainRoute{}, ErrAgentEmailCustomDomainRouteConflict
	}
	created, err := insertAgentEmailCustomDomainRouteTx(ctx, tx, accountID, in)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "23503" || pgErr.Code == "23505") {
			return AgentEmailCustomDomainRoute{}, ErrAgentEmailCustomDomainRouteConflict
		}
		return AgentEmailCustomDomainRoute{}, err
	}
	if err := s.logAgentEmailCustomDomainRouteProjectionTx(ctx, tx, created); err != nil {
		return AgentEmailCustomDomainRoute{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailCustomDomainRoute{}, err
	}
	return created, nil
}

// GetAgentEmailCustomDomainRoute returns the exact cell-side projection used
// by the control plane's read-after-write fence.
func (s *Store) GetAgentEmailCustomDomainRoute(
	ctx context.Context,
	accountID, domainRequestID, realmAliasClaimID string,
) (AgentEmailCustomDomainRoute, error) {
	accountID = strings.TrimSpace(accountID)
	domainRequestID = strings.TrimSpace(domainRequestID)
	realmAliasClaimID = strings.TrimSpace(realmAliasClaimID)
	if accountID == "" || !validAgentEmailGeneratedID(domainRequestID, "aedr") ||
		!validAgentEmailGeneratedID(realmAliasClaimID, "era") {
		return AgentEmailCustomDomainRoute{}, fmt.Errorf(
			"%w: invalid custom-domain route lookup", ErrAgentEmailInputInvalid,
		)
	}
	route, err := scanAgentEmailCustomDomainRoute(s.pool.QueryRow(ctx,
		agentEmailCustomDomainRouteSelect()+`
		WHERE account_id=$1 AND domain_request_id=$2 AND realm_alias_claim_id=$3`,
		accountID, domainRequestID, realmAliasClaimID))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailCustomDomainRoute{}, ErrAgentEmailCustomDomainRouteNotFound
	}
	if err != nil {
		return AgentEmailCustomDomainRoute{}, fmt.Errorf("get agent-email custom-domain route: %w", err)
	}
	return route, nil
}

func agentEmailCustomDomainRouteSelect() string {
	return `SELECT domain_request_id,realm_alias_claim_id,account_id,realm_id,
	              domain,realm_label,domain_allocation_revision,
	              domain_state_revision,realm_alias_revision,state,
	              COALESCE(suspension_disposition,''),controller_revision,
	              created_at,updated_at,suspended_at,retired_at
	         FROM agent_email_custom_domain_routes`
}

func scanAgentEmailCustomDomainRoute(row rowScanner) (AgentEmailCustomDomainRoute, error) {
	var route AgentEmailCustomDomainRoute
	err := row.Scan(
		&route.DomainRequestID, &route.RealmAliasClaimID, &route.AccountID,
		&route.RealmID, &route.Domain, &route.RealmLabel,
		&route.DomainAllocationRevision, &route.DomainStateRevision,
		&route.RealmAliasRevision, &route.State, &route.SuspensionDisposition,
		&route.ControllerRevision, &route.CreatedAt, &route.UpdatedAt,
		&route.SuspendedAt, &route.RetiredAt,
	)
	return route, err
}

func agentEmailCustomDomainRouteByIdentityTx(
	ctx context.Context,
	tx pgx.Tx,
	domainRequestID, realmAliasClaimID string,
	lock bool,
) (AgentEmailCustomDomainRoute, error) {
	query := agentEmailCustomDomainRouteSelect() +
		` WHERE domain_request_id=$1 AND realm_alias_claim_id=$2`
	if lock {
		query += ` FOR UPDATE`
	}
	route, err := scanAgentEmailCustomDomainRoute(tx.QueryRow(
		ctx, query, domainRequestID, realmAliasClaimID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailCustomDomainRoute{}, ErrAgentEmailCustomDomainRouteNotFound
	}
	if err != nil {
		return AgentEmailCustomDomainRoute{}, fmt.Errorf("read custom-domain route identity: %w", err)
	}
	return route, nil
}

func agentEmailCustomDomainRouteByLabelTx(
	ctx context.Context,
	tx pgx.Tx,
	domain, realmLabel string,
	lock bool,
) (AgentEmailCustomDomainRoute, error) {
	query := agentEmailCustomDomainRouteSelect() + ` WHERE domain=$1 AND realm_label=$2`
	if lock {
		query += ` FOR UPDATE`
	}
	route, err := scanAgentEmailCustomDomainRoute(tx.QueryRow(ctx, query, domain, realmLabel))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailCustomDomainRoute{}, ErrAgentEmailCustomDomainRouteNotFound
	}
	if err != nil {
		return AgentEmailCustomDomainRoute{}, fmt.Errorf("read custom-domain route label: %w", err)
	}
	return route, nil
}

func insertAgentEmailCustomDomainRouteTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
	in ApplyAgentEmailCustomDomainRouteInput,
) (AgentEmailCustomDomainRoute, error) {
	route, err := scanAgentEmailCustomDomainRoute(tx.QueryRow(ctx, `
		WITH applied_at AS (SELECT clock_timestamp() AS value)
		INSERT INTO agent_email_custom_domain_routes
		  (domain_request_id,realm_alias_claim_id,account_id,realm_id,
		   domain,realm_label,domain_allocation_revision,domain_state_revision,
		   realm_alias_revision,state,suspension_disposition,controller_revision,
		   created_at,updated_at,suspended_at,retired_at)
		SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''),$12,value,value,
		       CASE WHEN $10='suspended' THEN value END,
		       CASE WHEN $10='retired' THEN value END
		  FROM applied_at
		RETURNING domain_request_id,realm_alias_claim_id,account_id,realm_id,
		          domain,realm_label,domain_allocation_revision,
		          domain_state_revision,realm_alias_revision,state,
		          COALESCE(suspension_disposition,''),controller_revision,
		          created_at,updated_at,suspended_at,retired_at`,
		in.DomainRequestID, in.RealmAliasClaimID, accountID, in.RealmID,
		in.Domain, in.RealmLabel, in.DomainAllocationRevision,
		in.DomainStateRevision, in.RealmAliasRevision, in.State,
		in.SuspensionDisposition, in.ControllerRevision))
	if err != nil {
		return AgentEmailCustomDomainRoute{}, fmt.Errorf("insert agent-email custom-domain route: %w", err)
	}
	return route, nil
}

func updateAgentEmailCustomDomainRouteTx(
	ctx context.Context,
	tx pgx.Tx,
	existing AgentEmailCustomDomainRoute,
	in ApplyAgentEmailCustomDomainRouteInput,
) (AgentEmailCustomDomainRoute, error) {
	route, err := scanAgentEmailCustomDomainRoute(tx.QueryRow(ctx, `
		WITH applied_at AS (SELECT clock_timestamp() AS value)
		UPDATE agent_email_custom_domain_routes AS route
		   SET domain_allocation_revision=$3,
		       domain_state_revision=$4,
		       realm_alias_revision=$5,
		       state=$6,
		       suspension_disposition=NULLIF($7,''),
		       controller_revision=$8,
		       updated_at=applied_at.value,
		       suspended_at=CASE
		         WHEN $6='applied' THEN NULL
		         WHEN $6='suspended' THEN COALESCE(route.suspended_at,applied_at.value)
		         ELSE route.suspended_at
		       END,
		       retired_at=CASE
		         WHEN $6='retired' THEN COALESCE(route.retired_at,applied_at.value)
		         ELSE NULL
		       END
		  FROM applied_at
		 WHERE route.domain_request_id=$1 AND route.realm_alias_claim_id=$2
		   AND route.controller_revision<$8
		RETURNING route.domain_request_id,route.realm_alias_claim_id,
		          route.account_id,route.realm_id,route.domain,route.realm_label,
		          route.domain_allocation_revision,route.domain_state_revision,
		          route.realm_alias_revision,route.state,
		          COALESCE(route.suspension_disposition,''),route.controller_revision,
		          route.created_at,route.updated_at,route.suspended_at,route.retired_at`,
		existing.DomainRequestID, existing.RealmAliasClaimID,
		in.DomainAllocationRevision, in.DomainStateRevision, in.RealmAliasRevision,
		in.State, in.SuspensionDisposition, in.ControllerRevision))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailCustomDomainRoute{}, ErrAgentEmailCustomDomainRouteConflict
	}
	if err != nil {
		return AgentEmailCustomDomainRoute{}, fmt.Errorf("update agent-email custom-domain route: %w", err)
	}
	return route, nil
}

func (s *Store) logAgentEmailCustomDomainRouteProjectionTx(
	ctx context.Context,
	tx pgx.Tx,
	route AgentEmailCustomDomainRoute,
) error {
	return s.logEventTx(ctx, tx, EventInput{
		AccountID: route.AccountID,
		ActorKind: ActorControlPlane,
		Verb:      VerbAgentEmailCustomDomainRouteProjected,
		Metadata: map[string]any{
			"domain_request_id":    route.DomainRequestID,
			"realm_alias_claim_id": route.RealmAliasClaimID,
			"realm_id":             route.RealmID,
			"state":                route.State,
			"controller_revision":  strconv.FormatInt(route.ControllerRevision, 10),
		},
	})
}

func agentEmailCustomDomainRouteBindingMatches(
	route AgentEmailCustomDomainRoute,
	accountID string,
	in ApplyAgentEmailCustomDomainRouteInput,
) bool {
	return route.DomainRequestID == in.DomainRequestID &&
		route.RealmAliasClaimID == in.RealmAliasClaimID &&
		route.AccountID == accountID && route.RealmID == in.RealmID &&
		route.Domain == in.Domain && route.RealmLabel == in.RealmLabel
}

func agentEmailCustomDomainRouteDesiredMatches(
	route AgentEmailCustomDomainRoute,
	in ApplyAgentEmailCustomDomainRouteInput,
) bool {
	return agentEmailCustomDomainRouteBindingMatches(route, route.AccountID, in) &&
		route.DomainAllocationRevision == in.DomainAllocationRevision &&
		route.DomainStateRevision == in.DomainStateRevision &&
		route.RealmAliasRevision == in.RealmAliasRevision &&
		route.State == in.State &&
		route.SuspensionDisposition == in.SuspensionDisposition &&
		route.ControllerRevision == in.ControllerRevision
}

func validAgentEmailProjectionRevision(value int64) bool {
	return value >= 1 && value <= maximumAgentEmailGeneration
}

func validAgentEmailCustomDomainRouteState(value string) bool {
	switch value {
	case AgentEmailCustomDomainRouteApplied,
		AgentEmailCustomDomainRouteSuspended,
		AgentEmailCustomDomainRouteRetired:
		return true
	default:
		return false
	}
}

func validAgentEmailCustomDomainSuspension(state, disposition string) bool {
	disposition = strings.TrimSpace(disposition)
	if state != AgentEmailCustomDomainRouteSuspended {
		return disposition == ""
	}
	return disposition == AgentEmailCustomDomainSuspensionRetry ||
		disposition == AgentEmailCustomDomainSuspensionInactive
}

// lockAgentEmailRouteNamespaceTx serializes the namespace shared by managed
// realm aliases and custom-domain routes. Migration 0088's schema trigger uses
// the exact same key, so projections and archive restores cannot race the
// cross-table uniqueness check.
func lockAgentEmailRouteNamespaceTx(
	ctx context.Context,
	tx pgx.Tx,
	domain, realmLabel string,
) error {
	key := "witself:agent-email:route-namespace:" + domain + ":" + realmLabel
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
		return fmt.Errorf("lock agent-email route namespace: %w", err)
	}
	return nil
}
