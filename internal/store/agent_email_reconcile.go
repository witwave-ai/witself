package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// ReconcileAgentEmailPilot preflights the complete process allowlist and then
// idempotently provisions exactly those mailboxes. It is intended for bounded
// startup reconciliation, not a public request path. A failure returns no
// partial result and keeps the server fail-closed; already-created enrolled
// mailboxes are safe and make a later retry convergent.
func (s *Store) ReconcileAgentEmailPilot(
	ctx context.Context,
	scope AgentEmailPilotScope,
) ([]AgentEmailAddress, error) {
	if !scope.Enabled {
		return nil, ErrAgentEmailPilotDisabled
	}
	domain, err := normalizeAgentEmailPilotScope(scope)
	if err != nil {
		return nil, err
	}
	realmIDs := enabledAgentEmailPilotIDs(scope.RealmIDs)
	agentIDs := enabledAgentEmailPilotIDs(scope.AgentIDs)
	// normalizeAgentEmailPilotScope already pins these cardinalities. Keep the
	// explicit check here so this method cannot silently widen if that shared
	// validator changes later.
	if len(realmIDs) != 1 || len(agentIDs) < 5 || len(agentIDs) > 10 {
		return nil, fmt.Errorf("%w: reconciliation scope is outside pilot bounds", ErrAgentEmailInputInvalid)
	}
	realmID := realmIDs[0]
	if _, err := requireAgentEmailPilotEnrollment(scope, realmID, agentIDs[0]); err != nil {
		return nil, err
	}

	// Preflight in one short read transaction. It closes before Ensure takes
	// account->agent write locks, preserving the global lock order.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var accountID, accountStatus string
	err = tx.QueryRow(ctx, `
		SELECT r.account_id,a.status
		FROM realms r
		JOIN accounts a ON a.id=r.account_id
		WHERE r.id=$1 AND r.deleted_at IS NULL`, realmID).
		Scan(&accountID, &accountStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: enrolled pilot realm is not live", ErrAgentEmailPilotNotEnrolled)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve agent-email pilot realm: %w", err)
	}
	if accountStatus != "active" && accountStatus != "suspended" {
		return nil, ErrAccountNotActive
	}
	rows, err := tx.Query(ctx, `
		SELECT id
		FROM agents
		WHERE realm_id=$1 AND deleted_at IS NULL AND id=ANY($2::text[])
		ORDER BY id`, realmID, agentIDs)
	if err != nil {
		return nil, fmt.Errorf("preflight agent-email pilot agents: %w", err)
	}
	found := make(map[string]bool, len(agentIDs))
	for rows.Next() {
		var agentID string
		if err := rows.Scan(&agentID); err != nil {
			rows.Close()
			return nil, err
		}
		found[agentID] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, agentID := range agentIDs {
		if !found[agentID] {
			return nil, fmt.Errorf(
				"%w: enrolled pilot agent %s is not live in realm %s",
				ErrAgentEmailPilotNotEnrolled, agentID, realmID,
			)
		}
	}
	// Suspension already pauses ingestion through the account lifecycle gate,
	// but it must not make a routine server restart fail. While suspended, only
	// verify the exact pre-existing enrollment and return it read-only. Never
	// provision or repair mailboxes for a frozen account: any missing or drifted
	// row remains a startup failure until an operator resumes the account. After
	// resume, the next explicit startup reconciliation adds any missing primary
	// domain routes through the normal active Ensure path.
	if accountStatus == "suspended" {
		addresses := make([]AgentEmailAddress, 0, len(agentIDs))
		for _, agentID := range agentIDs {
			address, err := agentEmailAddressForOperatorAgentTx(
				ctx, tx, scope, accountID, agentID, false,
			)
			if err != nil {
				return nil, fmt.Errorf(
					"verify suspended agent-email mailbox for %s: %w",
					agentID, err,
				)
			}
			if err := populateSuspendedAgentEmailCanonicalAddressesTx(
				ctx, tx, scope, &address,
			); err != nil {
				return nil, fmt.Errorf(
					"verify suspended agent-email domain routes for %s: %w",
					agentID, err,
				)
			}
			if address.RealmID != realmID {
				return nil, fmt.Errorf(
					"%w: suspended agent-email mailbox for %s drifted from configured realm",
					ErrAgentEmailPilotNotEnrolled, agentID,
				)
			}
			addresses = append(addresses, address)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return addresses, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	addresses := make([]AgentEmailAddress, 0, len(agentIDs))
	for _, agentID := range agentIDs {
		address, err := s.EnsureAgentEmailMailbox(ctx, scope, accountID, realmID, agentID, "")
		if errors.Is(err, ErrAgentEmailAddressConflict) {
			// A second cell replica can race the same startup reconciliation.
			// PostgreSQL resolves the unique insert before returning the conflict;
			// accept only an exact same-owner/domain row committed by that peer.
			existing, lookupErr := s.GetAgentEmailAddress(ctx, scope, Principal{
				Kind: PrincipalAgent, ID: agentID, AccessProfile: AccessProfileFull,
				AccountID: accountID, RealmID: realmID, AccountStatus: "active",
			})
			if lookupErr == nil && existing.Domain == domain {
				address, err = existing, nil
			}
		}
		if err != nil {
			return nil, fmt.Errorf("reconcile agent-email mailbox for %s: %w", agentID, err)
		}
		addresses = append(addresses, address)
	}
	return addresses, nil
}

// AgentEmailProductionPreflight is a value-free, read-only summary of one
// exact production receive cohort. Missing mailboxes are reported instead of
// failing startup; the explicit backfill command is the only process that may
// reconcile them.
type AgentEmailProductionPreflight struct {
	AccountCount        int
	LiveAgentCount      int64
	ReadyMailboxCount   int64
	MissingMailboxCount int64
	RetryCanaryReady    bool
}

// AgentEmailProductionCanaryAgent is one actual, currently enabled canonical
// mailbox projection suitable for a literal primary-domain receive canary.
type AgentEmailProductionCanaryAgent struct {
	AgentID string
	RealmID string
	Address string
}

type agentEmailProductionQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// ValidateAgentEmailProductionCohort performs the bounded O(cohort) serving
// startup check: every configured account must be present in this cell and an
// optional retry canary must belong to the exact cohort. It never scans the
// cohort's agents or mailboxes and never writes.
func (s *Store) ValidateAgentEmailProductionCohort(
	ctx context.Context,
	scope AgentEmailReceiveScope,
) error {
	_, _, _, _, err := s.preflightAgentEmailProductionCohort(ctx, scope)
	return err
}

// PreflightAgentEmailProductionCohort adds aggregate agent and mailbox counts
// for an explicit backfill operation. Serving API replicas use the lightweight
// Validate method above instead, so an unlimited Founder account cannot turn
// every pod startup into a cohort-wide database scan.
func (s *Store) PreflightAgentEmailProductionCohort(
	ctx context.Context,
	scope AgentEmailReceiveScope,
) (AgentEmailProductionPreflight, error) {
	_, accountIDs, _, result, err := s.preflightAgentEmailProductionCohort(ctx, scope)
	if err != nil {
		return AgentEmailProductionPreflight{}, err
	}
	return summarizeAgentEmailProductionCohort(ctx, s.pool, accountIDs, result)
}

func summarizeAgentEmailProductionCohort(
	ctx context.Context,
	queryer agentEmailProductionQueryer,
	accountIDs []string,
	result AgentEmailProductionPreflight,
) (AgentEmailProductionPreflight, error) {
	if err := queryer.QueryRow(ctx, `
		SELECT
		  count(*),
		  count(*) FILTER (WHERE EXISTS (
		    SELECT 1
		      FROM agent_email_mailboxes mb
		      JOIN agent_email_addresses address
		        ON address.id=mb.address_id
		       AND address.account_id=mb.account_id
		       AND address.realm_id=mb.realm_id
		       AND address.provisioned_agent_id=mb.owner_agent_id
		     WHERE mb.account_id=r.account_id AND mb.realm_id=r.id
		       AND mb.owner_agent_id=a.id AND mb.retired_at IS NULL
		       AND address.retired_at IS NULL
		  ))
		FROM realms r
		JOIN agents a ON a.realm_id=r.id
		WHERE r.account_id=ANY($1::text[])
		  AND r.deleted_at IS NULL AND r.email_route_state='live'
		  AND a.deleted_at IS NULL`, accountIDs).
		Scan(&result.LiveAgentCount, &result.ReadyMailboxCount); err != nil {
		return AgentEmailProductionPreflight{}, fmt.Errorf(
			"summarize production agent-email cohort: %w", err,
		)
	}
	result.MissingMailboxCount = result.LiveAgentCount - result.ReadyMailboxCount
	return result, nil
}

// ListAgentEmailProductionCanaryAgents returns a deterministic, bounded set of
// 5-10 actual primary-domain mailboxes from the exact configured cohort. It is
// read-only and refuses to return a manifest while any live cohort agent is
// missing a mailbox. Disabled accounts, realms, and agents are not candidates;
// an optional retry-canary agent is always included or the operation fails.
func (s *Store) ListAgentEmailProductionCanaryAgents(
	ctx context.Context,
	scope AgentEmailReceiveScope,
) ([]AgentEmailProductionCanaryAgent, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	domain, accountIDs, _, preflight, err :=
		preflightAgentEmailProductionCohortQuery(ctx, tx, scope)
	if err != nil {
		return nil, err
	}
	preflight, err = summarizeAgentEmailProductionCohort(
		ctx, tx, accountIDs, preflight,
	)
	if err != nil {
		return nil, err
	}
	if preflight.MissingMailboxCount != 0 {
		return nil, fmt.Errorf(
			"%w: production canary requires zero missing cohort mailboxes",
			ErrAgentEmailPilotNotEnrolled,
		)
	}

	eligibleAccountIDs, err := agentEmailReceiveEnabledAccountIDs(
		ctx, tx, accountIDs,
	)
	if err != nil {
		return nil, err
	}
	if len(eligibleAccountIDs) == 0 {
		return nil, fmt.Errorf(
			"%w: production canary requires an active receive-enabled account",
			ErrAgentEmailPilotNotEnrolled,
		)
	}

	const maximumCanaryAgents = 10
	rows, err := tx.Query(ctx, `
		SELECT a.id,r.id,route.local_part || '@' || route.domain
		FROM realms r
		JOIN agents a ON a.realm_id=r.id
		JOIN agent_email_mailboxes mb
		  ON mb.account_id=r.account_id AND mb.realm_id=r.id
		 AND mb.owner_agent_id=a.id
		JOIN agent_email_addresses address
		  ON address.id=mb.address_id AND address.account_id=mb.account_id
		 AND address.realm_id=mb.realm_id
		 AND address.provisioned_agent_id=mb.owner_agent_id
		JOIN agent_email_address_domains route
		  ON route.address_id=address.id AND route.account_id=address.account_id
		 AND route.realm_id=address.realm_id
		 AND route.provisioned_agent_id=address.provisioned_agent_id
		JOIN agent_email_realm_receive_controls realm_control
		  ON realm_control.account_id=r.account_id AND realm_control.realm_id=r.id
		WHERE r.account_id=ANY($1::text[])
		  AND r.deleted_at IS NULL AND r.email_route_state='live'
		  AND a.deleted_at IS NULL AND mb.retired_at IS NULL
		  AND mb.receive_state='enabled' AND address.retired_at IS NULL
		  AND realm_control.receive_state='enabled' AND route.domain=$2
		ORDER BY CASE WHEN a.id=$3 THEN 0 ELSE 1 END,
		         route.local_part || '@' || route.domain,a.id
		LIMIT $4`, eligibleAccountIDs, domain, scope.RetryCanaryAgentID, maximumCanaryAgents)
	if err != nil {
		return nil, fmt.Errorf("list production agent-email canary mailboxes: %w", err)
	}
	defer rows.Close()
	agents := make([]AgentEmailProductionCanaryAgent, 0, maximumCanaryAgents)
	canaryFound := scope.RetryCanaryAgentID == ""
	for rows.Next() {
		var agent AgentEmailProductionCanaryAgent
		if err := rows.Scan(&agent.AgentID, &agent.RealmID, &agent.Address); err != nil {
			return nil, fmt.Errorf("scan production agent-email canary mailbox: %w", err)
		}
		if agent.AgentID == scope.RetryCanaryAgentID {
			canaryFound = true
		}
		agents = append(agents, agent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list production agent-email canary mailboxes: %w", err)
	}
	if !canaryFound {
		return nil, fmt.Errorf(
			"%w: configured retry canary mailbox is not receive-enabled",
			ErrAgentEmailPilotNotEnrolled,
		)
	}
	if len(agents) < 5 {
		return nil, fmt.Errorf(
			"%w: production canary requires 5-10 receive-enabled mailboxes",
			ErrAgentEmailPilotNotEnrolled,
		)
	}
	sort.Slice(agents, func(i, j int) bool {
		if agents[i].Address == agents[j].Address {
			return agents[i].AgentID < agents[j].AgentID
		}
		return agents[i].Address < agents[j].Address
	})

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit production agent-email canary snapshot: %w", err)
	}
	return agents, nil
}

func agentEmailReceiveEnabledAccountIDs(
	ctx context.Context,
	queryer agentEmailProductionQueryer,
	accountIDs []string,
) ([]string, error) {
	rows, err := queryer.Query(ctx, `
		SELECT id,plan_policies,plan_features,plan_applied_at
		FROM accounts
		WHERE id=ANY($1::text[]) AND status='active'
		ORDER BY id`, accountIDs)
	if err != nil {
		return nil, fmt.Errorf("list receive-enabled canary accounts: %w", err)
	}
	defer rows.Close()
	result := make([]string, 0, len(accountIDs))
	for rows.Next() {
		var accountID string
		var policiesJSON, featuresJSON []byte
		var appliedAt *time.Time
		if err := rows.Scan(&accountID, &policiesJSON, &featuresJSON, &appliedAt); err != nil {
			return nil, fmt.Errorf("scan receive-enabled canary account: %w", err)
		}
		if appliedAt == nil {
			result = append(result, accountID)
			continue
		}
		var policies map[string]int64
		if err := json.Unmarshal(policiesJSON, &policies); err != nil {
			return nil, fmt.Errorf("decode canary account plan policies: %w", err)
		}
		var features []string
		if err := json.Unmarshal(featuresJSON, &features); err != nil {
			return nil, fmt.Errorf("decode canary account plan features: %w", err)
		}
		if agentEmailReceiveEnabledForSnapshot(appliedAt, policies, features) {
			result = append(result, accountID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list receive-enabled canary accounts: %w", err)
	}
	return result, nil
}

// ReconcileAgentEmailProductionCohort backfills every live agent in each exact
// configured account. Account and canary preflight finishes before the first
// write; agents are reconciled in fixed-size keyset pages so an unlimited
// Founder account never becomes an unbounded memory operation. This is an
// explicit operator action and must not run in serving-pod startup. Suspended
// accounts are verified read-only, matching legacy pilot safety.
func (s *Store) ReconcileAgentEmailProductionCohort(
	ctx context.Context,
	scope AgentEmailReceiveScope,
) (int64, error) {
	domain, accountIDs, accountStatuses, _, err :=
		s.preflightAgentEmailProductionCohort(ctx, scope)
	if err != nil {
		return 0, err
	}

	const pageSize = 100
	var reconciled int64
	for _, accountID := range accountIDs {
		lastRealmID, lastAgentID := "", ""
		for {
			rows, err := s.pool.Query(ctx, `
				SELECT r.id,a.id
				FROM realms r
				JOIN agents a ON a.realm_id=r.id
				WHERE r.account_id=$1 AND r.deleted_at IS NULL
				  AND r.email_route_state='live' AND a.deleted_at IS NULL
				  AND (r.id>$2 OR (r.id=$2 AND a.id>$3))
				ORDER BY r.id,a.id
				LIMIT $4`, accountID, lastRealmID, lastAgentID, pageSize)
			if err != nil {
				return reconciled, fmt.Errorf("page production agent-email agents: %w", err)
			}
			page := make([][2]string, 0, pageSize)
			for rows.Next() {
				var identity [2]string
				if err := rows.Scan(&identity[0], &identity[1]); err != nil {
					rows.Close()
					return reconciled, err
				}
				page = append(page, identity)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return reconciled, err
			}
			rows.Close()
			for _, identity := range page {
				if err := s.reconcileProductionAgentEmailMailbox(
					ctx, scope, domain, accountID, accountStatuses[accountID],
					identity[0], identity[1],
				); err != nil {
					return reconciled, err
				}
				reconciled++
			}
			if len(page) < pageSize {
				break
			}
			lastRealmID = page[len(page)-1][0]
			lastAgentID = page[len(page)-1][1]
		}
	}
	return reconciled, nil
}

func (s *Store) preflightAgentEmailProductionCohort(
	ctx context.Context,
	scope AgentEmailReceiveScope,
) (string, []string, map[string]string, AgentEmailProductionPreflight, error) {
	return preflightAgentEmailProductionCohortQuery(ctx, s.pool, scope)
}

func preflightAgentEmailProductionCohortQuery(
	ctx context.Context,
	queryer agentEmailProductionQueryer,
	scope AgentEmailReceiveScope,
) (string, []string, map[string]string, AgentEmailProductionPreflight, error) {
	if !scope.Enabled {
		return "", nil, nil, AgentEmailProductionPreflight{}, ErrAgentEmailPilotDisabled
	}
	domain, err := normalizeAgentEmailPilotScope(scope)
	if err != nil {
		return "", nil, nil, AgentEmailProductionPreflight{}, err
	}
	if agentEmailReceiveMode(scope) != AgentEmailReceiveModeProduction {
		return "", nil, nil, AgentEmailProductionPreflight{}, fmt.Errorf(
			"%w: production reconciliation requires production receive mode",
			ErrAgentEmailInputInvalid,
		)
	}

	accountIDs := enabledAgentEmailPilotIDs(scope.AccountIDs)
	accountStatuses := make(map[string]string, len(accountIDs))
	result := AgentEmailProductionPreflight{
		AccountCount:     len(accountIDs),
		RetryCanaryReady: scope.RetryCanaryAgentID == "",
	}
	for _, accountID := range accountIDs {
		var status string
		err := queryer.QueryRow(ctx, `SELECT status FROM accounts WHERE id=$1`, accountID).
			Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil, nil, AgentEmailProductionPreflight{}, fmt.Errorf(
				"%w: production receive account %s is not present in this cell",
				ErrAgentEmailPilotNotEnrolled, accountID,
			)
		}
		if err != nil {
			return "", nil, nil, AgentEmailProductionPreflight{}, fmt.Errorf(
				"resolve production agent-email account: %w", err,
			)
		}
		if status != "active" && status != "suspended" {
			return "", nil, nil, AgentEmailProductionPreflight{}, ErrAccountNotActive
		}
		accountStatuses[accountID] = status
	}
	if scope.RetryCanaryAgentID != "" {
		var canaryFound bool
		if err := queryer.QueryRow(ctx, `
			SELECT EXISTS (
			  SELECT 1
			  FROM agents a
			  JOIN realms r ON r.id=a.realm_id
			  WHERE a.id=$1 AND a.deleted_at IS NULL
			    AND r.deleted_at IS NULL AND r.email_route_state='live'
			    AND r.account_id=ANY($2::text[])
			)`, scope.RetryCanaryAgentID, accountIDs).Scan(&canaryFound); err != nil {
			return "", nil, nil, AgentEmailProductionPreflight{}, fmt.Errorf(
				"preflight production retry canary: %w", err,
			)
		}
		if !canaryFound {
			return "", nil, nil, AgentEmailProductionPreflight{}, fmt.Errorf(
				"%w: production retry canary agent is not live in the exact account cohort",
				ErrAgentEmailPilotNotEnrolled,
			)
		}
		result.RetryCanaryReady = true
	}
	return domain, accountIDs, accountStatuses, result, nil
}

func (s *Store) reconcileProductionAgentEmailMailbox(
	ctx context.Context,
	scope AgentEmailReceiveScope,
	domain, accountID, accountStatus, realmID, agentID string,
) error {
	if accountStatus == "suspended" {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		address, err := agentEmailAddressForOperatorAgentTx(
			ctx, tx, scope, accountID, agentID, false,
		)
		if err == nil {
			err = populateSuspendedAgentEmailCanonicalAddressesTx(ctx, tx, scope, &address)
		}
		if err == nil {
			err = populateAgentEmailRealmAliasAddressesTx(ctx, tx, &address)
		}
		if err == nil && address.RealmID != realmID {
			err = ErrAgentEmailPilotNotEnrolled
		}
		if err != nil {
			return fmt.Errorf("verify suspended production agent-email mailbox: %w", err)
		}
		return tx.Commit(ctx)
	}

	address, err := s.EnsureAgentEmailMailbox(ctx, scope, accountID, realmID, agentID, "")
	if errors.Is(err, ErrAgentEmailAddressConflict) {
		existing, lookupErr := s.GetAgentEmailAddress(ctx, scope, Principal{
			Kind: PrincipalAgent, ID: agentID, AccessProfile: AccessProfileFull,
			AccountID: accountID, RealmID: realmID, AccountStatus: "active",
		})
		if lookupErr == nil && agentEmailAddressHasPrimaryDomain(existing, domain) {
			address, err = existing, nil
		}
	}
	if err != nil {
		return fmt.Errorf("reconcile production agent-email mailbox: %w", err)
	}
	if !agentEmailAddressHasPrimaryDomain(address, domain) {
		return fmt.Errorf("%w: production mailbox primary domain did not converge", ErrAgentEmailConflict)
	}
	return nil
}

func agentEmailAddressHasPrimaryDomain(address AgentEmailAddress, domain string) bool {
	for _, canonical := range address.Addresses {
		if canonical.Role == AgentEmailAddressRolePrimary && canonical.Domain == domain {
			return true
		}
	}
	return false
}

func enabledAgentEmailPilotIDs(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value, enabled := range values {
		if enabled {
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
