package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// AgentEmailSendControl is the value-free operator projection of the agent
// layer plus its independently managed realm layer.
type AgentEmailSendControl struct {
	AccountID       string     `json:"account_id"`
	RealmID         string     `json:"realm_id"`
	AgentID         string     `json:"agent_id"`
	SendState       string     `json:"send_state"`
	AgentSendState  string     `json:"agent_send_state"`
	RealmSendState  string     `json:"realm_send_state"`
	RowVersion      int64      `json:"row_version"`
	RealmRowVersion int64      `json:"realm_row_version"`
	UpdatedAt       time.Time  `json:"updated_at"`
	DisabledAt      *time.Time `json:"disabled_at,omitempty"`
	RealmDisabledAt *time.Time `json:"realm_disabled_at,omitempty"`
}

// AgentEmailRealmSendControl is the durable realm aggregate. AgentCount is
// informational and does not duplicate per-agent state.
type AgentEmailRealmSendControl struct {
	AccountID  string     `json:"account_id"`
	RealmID    string     `json:"realm_id"`
	SendState  string     `json:"send_state"`
	AgentCount int64      `json:"agent_count"`
	RowVersion int64      `json:"row_version"`
	UpdatedAt  time.Time  `json:"updated_at"`
	DisabledAt *time.Time `json:"disabled_at,omitempty"`
}

// GetAgentEmailSendControl returns the effective and component agent controls.
func (s *Store) GetAgentEmailSendControl(
	ctx context.Context,
	accountID, operatorID, agentID string,
) (AgentEmailSendControl, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEmailSendControl{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForSafetyWrite(ctx, tx, accountID); err != nil {
		return AgentEmailSendControl{}, err
	}
	realmID, err := lockAgentEmailSendOperatorAgentTx(
		ctx, tx, accountID, operatorID, agentID,
	)
	if err != nil {
		return AgentEmailSendControl{}, err
	}
	control, err := ensureAndReadAgentEmailSendControlTx(
		ctx, tx, accountID, realmID, agentID, false,
	)
	if err != nil {
		return AgentEmailSendControl{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailSendControl{}, err
	}
	return control, nil
}

// SetAgentEmailSendControl changes only the agent layer. expectedRowVersion=0
// is an unconditional operator write; positive values provide optimistic
// concurrency and return ErrAgentEmailOutboundConflict when stale.
func (s *Store) SetAgentEmailSendControl(
	ctx context.Context,
	accountID, operatorID, agentID, desiredState string,
	expectedRowVersion int64,
) (AgentEmailSendControl, error) {
	desiredState, err := normalizeAgentEmailSendState(desiredState)
	if err != nil {
		return AgentEmailSendControl{}, err
	}
	if expectedRowVersion < 0 || expectedRowVersion > maximumAgentEmailOutboundGeneration {
		return AgentEmailSendControl{}, fmt.Errorf(
			"%w: expected row version is invalid", ErrAgentEmailOutboundInputInvalid)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEmailSendControl{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgentEmailSendControlAccount(ctx, tx, accountID, desiredState); err != nil {
		return AgentEmailSendControl{}, err
	}
	realmID, err := lockAgentEmailSendOperatorAgentTx(
		ctx, tx, accountID, operatorID, agentID,
	)
	if err != nil {
		return AgentEmailSendControl{}, err
	}
	control, err := ensureAndReadAgentEmailSendControlTx(
		ctx, tx, accountID, realmID, agentID, true,
	)
	if err != nil {
		return AgentEmailSendControl{}, err
	}
	if expectedRowVersion > 0 && control.RowVersion != expectedRowVersion {
		return AgentEmailSendControl{}, ErrAgentEmailOutboundConflict
	}
	if control.AgentSendState == desiredState {
		if err := tx.Commit(ctx); err != nil {
			return AgentEmailSendControl{}, err
		}
		return control, nil
	}
	err = tx.QueryRow(ctx, `
		UPDATE agent_email_send_controls
		   SET send_state=$1,
		       disabled_at=CASE WHEN $1='enabled' THEN NULL
		                        ELSE COALESCE(disabled_at,clock_timestamp()) END,
		       row_version=row_version+1,updated_at=clock_timestamp()
		 WHERE account_id=$2 AND realm_id=$3 AND owner_agent_id=$4
		 RETURNING send_state,row_version,updated_at,disabled_at`,
		desiredState, accountID, realmID, agentID).
		Scan(&control.AgentSendState, &control.RowVersion,
			&control.UpdatedAt, &control.DisabledAt)
	if err != nil {
		return AgentEmailSendControl{}, fmt.Errorf("set agent-email send control: %w", err)
	}
	control.SendState = effectiveAgentEmailSendState(
		control.AgentSendState, control.RealmSendState,
	)
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailSendControl{}, err
	}
	return control, nil
}

// GetAgentEmailRealmSendControl returns one realm-level send control.
func (s *Store) GetAgentEmailRealmSendControl(
	ctx context.Context,
	accountID, operatorID, realmID string,
) (AgentEmailRealmSendControl, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEmailRealmSendControl{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForSafetyWrite(ctx, tx, accountID); err != nil {
		return AgentEmailRealmSendControl{}, err
	}
	if err := lockAgentEmailSendOperatorRealmTx(
		ctx, tx, accountID, operatorID, realmID,
	); err != nil {
		return AgentEmailRealmSendControl{}, err
	}
	control, err := ensureAndReadAgentEmailRealmSendControlTx(
		ctx, tx, accountID, realmID, false,
	)
	if err != nil {
		return AgentEmailRealmSendControl{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailRealmSendControl{}, err
	}
	return control, nil
}

// SetAgentEmailRealmSendControl changes only the realm aggregate. A disabled
// agent remains disabled after its realm is re-enabled.
func (s *Store) SetAgentEmailRealmSendControl(
	ctx context.Context,
	accountID, operatorID, realmID, desiredState string,
	expectedRowVersion int64,
) (AgentEmailRealmSendControl, error) {
	desiredState, err := normalizeAgentEmailSendState(desiredState)
	if err != nil {
		return AgentEmailRealmSendControl{}, err
	}
	if expectedRowVersion < 0 || expectedRowVersion > maximumAgentEmailOutboundGeneration {
		return AgentEmailRealmSendControl{}, fmt.Errorf(
			"%w: expected row version is invalid", ErrAgentEmailOutboundInputInvalid)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEmailRealmSendControl{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgentEmailSendControlAccount(ctx, tx, accountID, desiredState); err != nil {
		return AgentEmailRealmSendControl{}, err
	}
	if err := lockAgentEmailSendOperatorRealmTx(
		ctx, tx, accountID, operatorID, realmID,
	); err != nil {
		return AgentEmailRealmSendControl{}, err
	}
	control, err := ensureAndReadAgentEmailRealmSendControlTx(
		ctx, tx, accountID, realmID, true,
	)
	if err != nil {
		return AgentEmailRealmSendControl{}, err
	}
	if expectedRowVersion > 0 && control.RowVersion != expectedRowVersion {
		return AgentEmailRealmSendControl{}, ErrAgentEmailOutboundConflict
	}
	if control.SendState == desiredState {
		if err := tx.Commit(ctx); err != nil {
			return AgentEmailRealmSendControl{}, err
		}
		return control, nil
	}
	err = tx.QueryRow(ctx, `
		UPDATE agent_email_realm_send_controls
		   SET send_state=$1,
		       disabled_at=CASE WHEN $1='enabled' THEN NULL
		                        ELSE COALESCE(disabled_at,clock_timestamp()) END,
		       row_version=row_version+1,updated_at=clock_timestamp()
		 WHERE account_id=$2 AND realm_id=$3
		 RETURNING send_state,row_version,updated_at,disabled_at`,
		desiredState, accountID, realmID).
		Scan(&control.SendState, &control.RowVersion,
			&control.UpdatedAt, &control.DisabledAt)
	if err != nil {
		return AgentEmailRealmSendControl{}, fmt.Errorf(
			"set agent-email realm send control: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailRealmSendControl{}, err
	}
	return control, nil
}

func requireAgentEmailSendControlsTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
) error {
	control, err := ensureAndReadAgentEmailSendControlTx(
		ctx, tx, p.AccountID, p.RealmID, p.ID, false,
	)
	if err != nil {
		return err
	}
	if control.SendState != AgentEmailSendEnabled {
		return ErrAgentEmailSendDisabled
	}
	return nil
}

// readAgentEmailSendControlsTx evaluates the default-enabled control layers
// without materializing missing rows. Owner read/entitlement checks use this
// path; queue admission and the provider boundary continue to use the
// ensure-and-lock path above so policy changes remain race-safe for sends.
func readAgentEmailSendControlsTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
) error {
	realmState := AgentEmailSendEnabled
	err := tx.QueryRow(ctx, `
		SELECT send_state FROM agent_email_realm_send_controls
		 WHERE account_id=$1 AND realm_id=$2
		 FOR SHARE`, p.AccountID, p.RealmID).Scan(&realmState)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read agent-email realm send control: %w", err)
	}
	agentState := AgentEmailSendEnabled
	err = tx.QueryRow(ctx, `
		SELECT send_state FROM agent_email_send_controls
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		 FOR SHARE`, p.AccountID, p.RealmID, p.ID).Scan(&agentState)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read agent-email send control: %w", err)
	}
	if effectiveAgentEmailSendState(agentState, realmState) != AgentEmailSendEnabled {
		return ErrAgentEmailSendDisabled
	}
	return nil
}

func ensureAndReadAgentEmailSendControlTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID, realmID, agentID string,
	lock bool,
) (AgentEmailSendControl, error) {
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_email_realm_send_controls (account_id,realm_id)
		VALUES ($1,$2) ON CONFLICT (account_id,realm_id) DO NOTHING`,
		accountID, realmID); err != nil {
		return AgentEmailSendControl{}, fmt.Errorf(
			"ensure agent-email realm send control: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_email_send_controls
		  (account_id,realm_id,owner_agent_id)
		VALUES ($1,$2,$3)
		ON CONFLICT (account_id,realm_id,owner_agent_id) DO NOTHING`,
		accountID, realmID, agentID); err != nil {
		return AgentEmailSendControl{}, fmt.Errorf(
			"ensure agent-email send control: %w", err)
	}

	// Lock the realm before the agent on every path. Realm changes therefore
	// serialize with queue admission and with agent changes without inversion.
	realmLock := " FOR SHARE"
	if lock {
		realmLock = " FOR UPDATE"
	}
	var control AgentEmailSendControl
	err := tx.QueryRow(ctx, `
		SELECT send_state,row_version,disabled_at
		  FROM agent_email_realm_send_controls
		 WHERE account_id=$1 AND realm_id=$2`+realmLock,
		accountID, realmID).
		Scan(&control.RealmSendState, &control.RealmRowVersion,
			&control.RealmDisabledAt)
	if err != nil {
		return AgentEmailSendControl{}, fmt.Errorf(
			"lock agent-email realm send control: %w", err)
	}
	agentLock := " FOR SHARE"
	if lock {
		agentLock = " FOR UPDATE"
	}
	err = tx.QueryRow(ctx, `
		SELECT send_state,row_version,updated_at,disabled_at
		  FROM agent_email_send_controls
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3`+agentLock,
		accountID, realmID, agentID).
		Scan(&control.AgentSendState, &control.RowVersion,
			&control.UpdatedAt, &control.DisabledAt)
	if err != nil {
		return AgentEmailSendControl{}, fmt.Errorf(
			"lock agent-email send control: %w", err)
	}
	control.AccountID = accountID
	control.RealmID = realmID
	control.AgentID = agentID
	control.SendState = effectiveAgentEmailSendState(
		control.AgentSendState, control.RealmSendState,
	)
	return control, nil
}

func ensureAndReadAgentEmailRealmSendControlTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID, realmID string,
	lock bool,
) (AgentEmailRealmSendControl, error) {
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_email_realm_send_controls (account_id,realm_id)
		VALUES ($1,$2) ON CONFLICT (account_id,realm_id) DO NOTHING`,
		accountID, realmID); err != nil {
		return AgentEmailRealmSendControl{}, fmt.Errorf(
			"ensure agent-email realm send control: %w", err)
	}
	query := `SELECT send_state,row_version,updated_at,disabled_at
		FROM agent_email_realm_send_controls
		WHERE account_id=$1 AND realm_id=$2`
	if lock {
		query += " FOR UPDATE"
	} else {
		query += " FOR SHARE"
	}
	control := AgentEmailRealmSendControl{AccountID: accountID, RealmID: realmID}
	if err := tx.QueryRow(ctx, query, accountID, realmID).Scan(
		&control.SendState, &control.RowVersion,
		&control.UpdatedAt, &control.DisabledAt,
	); err != nil {
		return AgentEmailRealmSendControl{}, fmt.Errorf(
			"read agent-email realm send control: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM agents
		 WHERE realm_id=$1 AND deleted_at IS NULL`, realmID).
		Scan(&control.AgentCount); err != nil {
		return AgentEmailRealmSendControl{}, fmt.Errorf(
			"count realm agents for send control: %w", err)
	}
	return control, nil
}

func lockAgentEmailSendOperatorAgentTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID, operatorID, agentID string,
) (string, error) {
	if err := requireAgentEmailSendOperatorTx(ctx, tx, accountID, operatorID); err != nil {
		return "", err
	}
	var realmID string
	err := tx.QueryRow(ctx, `
		SELECT agent.realm_id
		  FROM agents agent
		  JOIN realms realm ON realm.id=agent.realm_id
		 WHERE agent.id=$1 AND realm.account_id=$2
		   AND agent.deleted_at IS NULL AND realm.deleted_at IS NULL
		 FOR SHARE OF agent,realm`, agentID, accountID).Scan(&realmID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrAgentEmailOutboundNotFound
	}
	if err != nil {
		return "", fmt.Errorf("resolve agent-email send-control agent: %w", err)
	}
	return realmID, nil
}

func lockAgentEmailSendOperatorRealmTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID, operatorID, realmID string,
) error {
	if err := requireAgentEmailSendOperatorTx(ctx, tx, accountID, operatorID); err != nil {
		return err
	}
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT true FROM realms
		 WHERE account_id=$1 AND id=$2 AND deleted_at IS NULL
		 FOR SHARE`, accountID, realmID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAgentEmailOutboundNotFound
	}
	if err != nil {
		return fmt.Errorf("resolve agent-email send-control realm: %w", err)
	}
	return nil
}

func requireAgentEmailSendOperatorTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID, operatorID string,
) error {
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(operatorID) == "" {
		return ErrAgentEmailOutboundForbidden
	}
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT true FROM operators
		 WHERE account_id=$1 AND id=$2 AND deleted_at IS NULL
		 FOR SHARE`, accountID, operatorID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAgentEmailOutboundForbidden
	}
	if err != nil {
		return fmt.Errorf("authorize agent-email send operator: %w", err)
	}
	return nil
}

func lockAgentEmailSendControlAccount(
	ctx context.Context,
	tx pgx.Tx,
	accountID, desiredState string,
) error {
	if desiredState == AgentEmailSendDisabled {
		return lockAccountForSafetyWrite(ctx, tx, accountID)
	}
	return lockAccountForMint(ctx, tx, accountID, false)
}

func normalizeAgentEmailSendState(state string) (string, error) {
	state = strings.TrimSpace(state)
	if state != AgentEmailSendEnabled && state != AgentEmailSendDisabled {
		return "", fmt.Errorf(
			"%w: send_state must be enabled or disabled",
			ErrAgentEmailOutboundInputInvalid)
	}
	return state, nil
}

func effectiveAgentEmailSendState(agentState, realmState string) string {
	if agentState == AgentEmailSendEnabled && realmState == AgentEmailSendEnabled {
		return AgentEmailSendEnabled
	}
	return AgentEmailSendDisabled
}
