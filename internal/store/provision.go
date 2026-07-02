package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/token"
)

// ErrAccountEmailExists is returned when an account with the email already
// exists on this cell.
var ErrAccountEmailExists = errors.New("an account with this email already exists")

// ProvisionedAccount is the result of provisioning a new (non-default) account:
// the account, its root operator, and a short-lived bootstrap token (returned
// once) that the new owner exchanges for their operator token — the same
// exchange a self-hosted bootstrap uses.
type ProvisionedAccount struct {
	AccountID      string
	OperatorID     string
	Email          string
	Status         string
	BootstrapToken string
}

// ProvisionAccount creates a new account with its root operator and a bound
// bootstrap token, atomically. It is the per-signup generalization of the
// boot-time seed (EnsureDefaultAccount + EnsureRootOperator + adopt): same
// shape, but the cell mints the bootstrap token server-side and the account is
// never the default.
func (s *Store) ProvisionAccount(ctx context.Context, email, displayName string, bootstrapTTL time.Duration) (ProvisionedAccount, error) {
	acctID, err := id.New("acc")
	if err != nil {
		return ProvisionedAccount{}, err
	}
	oprID, err := id.New("opr")
	if err != nil {
		return ProvisionedAccount{}, err
	}
	bootTok, err := token.New(token.KindBootstrap)
	if err != nil {
		return ProvisionedAccount{}, err
	}
	tokID, err := id.New("tok")
	if err != nil {
		return ProvisionedAccount{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProvisionedAccount{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`INSERT INTO accounts (id, is_default, display_name, email, status)
		 VALUES ($1, false, $2, $3, 'active')`,
		acctID, displayName, email); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			return ProvisionedAccount{}, ErrAccountEmailExists
		}
		return ProvisionedAccount{}, fmt.Errorf("create account: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO operators (id, account_id, role, is_root, display_name)
		 VALUES ($1, $2, 'account_owner', true, 'owner')`,
		oprID, acctID); err != nil {
		return ProvisionedAccount{}, fmt.Errorf("create root operator: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO tokens (id, account_id, operator_id, kind, token_hash, expires_at)
		 VALUES ($1, $2, $3, 'bootstrap', $4, $5)`,
		tokID, acctID, oprID, hashToken(bootTok), time.Now().UTC().Add(bootstrapTTL)); err != nil {
		return ProvisionedAccount{}, fmt.Errorf("bind bootstrap token: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return ProvisionedAccount{}, err
	}
	return ProvisionedAccount{
		AccountID:      acctID,
		OperatorID:     oprID,
		Email:          email,
		Status:         "active",
		BootstrapToken: bootTok,
	}, nil
}
