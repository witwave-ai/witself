package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/token"
)

var (
	// ErrProvisionIDInvalid means the caller did not supply one bounded opaque
	// control-plane provisioning operation id.
	ErrProvisionIDInvalid = errors.New("invalid provision id")
	// ErrProvisionRequestConflict means an existing receipt binds this
	// provision id to different normalized signup input.
	ErrProvisionRequestConflict = errors.New(
		"provision id is already bound to a different request",
	)
	// ErrProvisionReplayUnsafe means the exact receipt exists, but its account,
	// root operator, or receipt-bound bootstrap token is no longer in the
	// narrow pending/unclaimed state that may safely be rotated.
	ErrProvisionReplayUnsafe = errors.New(
		"provision receipt is no longer safely replayable",
	)
	// ErrProvisionRequestInvalid means a direct store caller supplied an empty
	// or malformed normalized signup request.
	ErrProvisionRequestInvalid = errors.New("invalid provision request")
)

// ProvisionedAccount is the result of provisioning a new (non-default)
// account: the account, its root operator, and a short-lived bootstrap token
// that the new owner exchanges for their operator token. Replayed responses
// carry a newly minted plaintext token; plaintext is returned once and never
// persisted.
type ProvisionedAccount struct {
	ProvisionID    string
	AccountID      string
	OperatorID     string
	Email          string
	Status         string
	BootstrapToken string
	Replayed       bool
}

// ProvisionAccount creates a distinct provisioning operation for local and
// test callers that do not cross an ambiguous HTTP boundary. Production
// control-plane requests must call ProvisionAccountExact with their
// caller-stable provision id.
func (s *Store) ProvisionAccount(
	ctx context.Context,
	email, displayName string,
	bootstrapTTL time.Duration,
) (ProvisionedAccount, error) {
	email, displayName, err := normalizeProvisionRequest(email, displayName)
	if err != nil {
		return ProvisionedAccount{}, err
	}
	if bootstrapTTL <= 0 {
		return ProvisionedAccount{}, ErrProvisionRequestInvalid
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProvisionedAccount{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Keep the historical in-process provisioning path independent of the
	// schema-72 receipt table. Migration and downgrade rehearsals deliberately
	// exercise older schemas, while every production request that can cross an
	// ambiguous HTTP boundary uses ProvisionAccountExact below.
	return createProvisionedAccountTx(
		ctx, tx, "", "", email, displayName, bootstrapTTL, false,
	)
}

// ProvisionAccountExact creates or safely replays one cell-local signup
// operation. The durable receipt binds provisionID to the exact normalized
// request and account. A replay returns the same account/root operator, but
// atomically revokes the prior unclaimed bootstrap and mints a fresh one so a
// committed response lost in transit is recoverable without storing plaintext
// credentials.
func (s *Store) ProvisionAccountExact(
	ctx context.Context,
	provisionID, email, displayName string,
	bootstrapTTL time.Duration,
) (ProvisionedAccount, error) {
	provisionID = strings.TrimSpace(provisionID)
	if !evacuationIDPattern.MatchString(provisionID) {
		return ProvisionedAccount{}, ErrProvisionIDInvalid
	}
	email, displayName, err := normalizeProvisionRequest(email, displayName)
	if err != nil {
		return ProvisionedAccount{}, err
	}
	if bootstrapTTL <= 0 {
		return ProvisionedAccount{}, ErrProvisionRequestInvalid
	}
	requestFingerprint := provisionRequestFingerprint(
		provisionID, email, displayName,
	)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProvisionedAccount{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The receipt row does not exist on the first attempt, so a row lock alone
	// cannot serialize two simultaneous first calls. This transaction-scoped
	// advisory lock gives every exact provision id one creation/replay lane.
	// Hash collisions only serialize unrelated signups conservatively.
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(
		       hashtextextended('witself-provision:' || $1, 0)
		)`,
		provisionID,
	); err != nil {
		return ProvisionedAccount{},
			fmt.Errorf("lock provision operation: %w", err)
	}

	var receiptAccountID, receiptFingerprint, receiptTokenID string
	err = tx.QueryRow(ctx, `
		SELECT account_id, request_fingerprint, bootstrap_token_id
		  FROM account_provision_receipts
		 WHERE provision_id = $1`,
		provisionID,
	).Scan(
		&receiptAccountID, &receiptFingerprint, &receiptTokenID,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return createProvisionedAccountTx(
			ctx, tx, provisionID, requestFingerprint,
			email, displayName, bootstrapTTL, true,
		)
	case err != nil:
		return ProvisionedAccount{},
			fmt.Errorf("read provision receipt: %w", err)
	case receiptFingerprint != requestFingerprint:
		return ProvisionedAccount{}, ErrProvisionRequestConflict
	default:
		return replayProvisionedAccountTx(
			ctx, tx, provisionID, receiptAccountID, receiptTokenID,
			email, displayName, bootstrapTTL,
		)
	}
}

func provisionRequestFingerprint(
	provisionID, email, displayName string,
) string {
	hasher := sha256.New()
	for _, value := range []string{provisionID, email, displayName} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = hasher.Write(length[:])
		_, _ = hasher.Write([]byte(value))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func normalizeProvisionRequest(
	email, displayName string,
) (string, string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	displayName = strings.TrimSpace(displayName)
	if email == "" || !strings.Contains(email, "@") {
		return "", "", ErrProvisionRequestInvalid
	}
	if displayName == "" {
		displayName = email
	}
	return email, displayName, nil
}

func createProvisionedAccountTx(
	ctx context.Context,
	tx pgx.Tx,
	provisionID, requestFingerprint, email, displayName string,
	bootstrapTTL time.Duration,
	recordReceipt bool,
) (ProvisionedAccount, error) {
	acctID, err := id.New("acc")
	if err != nil {
		return ProvisionedAccount{}, err
	}
	oprID, err := id.New("opr")
	if err != nil {
		return ProvisionedAccount{}, err
	}
	tokID, bootTok, err := newProvisionBootstrap()
	if err != nil {
		return ProvisionedAccount{}, err
	}

	// Emails may repeat across accounts (contact info, not identity). New
	// accounts start pending: nothing works until activation gates pass.
	if _, err := tx.Exec(ctx,
		`INSERT INTO accounts (id, is_default, display_name, email, status)
		 VALUES ($1, false, $2, $3, 'pending')`,
		acctID, displayName, email,
	); err != nil {
		return ProvisionedAccount{}, fmt.Errorf("create account: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO operators (id, account_id, role, is_root, display_name)
		 VALUES ($1, $2, 'account_owner', true, 'owner')`,
		oprID, acctID,
	); err != nil {
		return ProvisionedAccount{},
			fmt.Errorf("create root operator: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO tokens(
			id, account_id, operator_id, kind, token_hash, expires_at
		) VALUES ($1, $2, $3, 'bootstrap', $4, $5)`,
		tokID, acctID, oprID, hashToken(bootTok),
		time.Now().UTC().Add(bootstrapTTL),
	); err != nil {
		return ProvisionedAccount{},
			fmt.Errorf("bind bootstrap token: %w", err)
	}
	if recordReceipt {
		if _, err := tx.Exec(ctx, `
			INSERT INTO account_provision_receipts(
				provision_id, account_id, request_fingerprint,
				bootstrap_token_id
			) VALUES ($1, $2, $3, $4)`,
			provisionID, acctID, requestFingerprint, tokID,
		); err != nil {
			return ProvisionedAccount{},
				fmt.Errorf("record provision receipt: %w", err)
		}
	}

	// The signup that just created this account is the first entry in its
	// audit trail. Email is masked; plaintext never lands in the ledger.
	if err := logEventTx(ctx, tx, EventInput{
		AccountID: acctID, ActorKind: ActorControlPlane,
		Verb: VerbAccountProvisioned,
		Metadata: map[string]any{
			"email_masked": MaskEmail(email),
			"operator_id":  oprID,
		},
	}); err != nil {
		return ProvisionedAccount{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ProvisionedAccount{}, err
	}
	return ProvisionedAccount{
		ProvisionID: provisionID, AccountID: acctID, OperatorID: oprID,
		Email: email, Status: "pending", BootstrapToken: bootTok,
	}, nil
}

func replayProvisionedAccountTx(
	ctx context.Context,
	tx pgx.Tx,
	provisionID, accountID, priorTokenID, email, displayName string,
	bootstrapTTL time.Duration,
) (ProvisionedAccount, error) {
	var accountEmail *string
	var accountDisplayName, status string
	var isDefault bool
	err := tx.QueryRow(ctx, `
		SELECT email, display_name, status, is_default
		  FROM accounts
		 WHERE id = $1
		 FOR UPDATE`,
		accountID,
	).Scan(&accountEmail, &accountDisplayName, &status, &isDefault)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProvisionedAccount{}, ErrProvisionReplayUnsafe
	}
	if err != nil {
		return ProvisionedAccount{},
			fmt.Errorf("lock provisioned account: %w", err)
	}
	if isDefault || status != "pending" || accountEmail == nil ||
		*accountEmail != email || accountDisplayName != displayName {
		return ProvisionedAccount{}, ErrProvisionReplayUnsafe
	}

	// Re-read and lock the receipt only after the account lock so replay uses
	// the same accounts-before-token lock order as activation, reap, close, and
	// bootstrap exchange.
	var lockedAccountID, lockedTokenID string
	if err := tx.QueryRow(ctx, `
		SELECT account_id, bootstrap_token_id
		  FROM account_provision_receipts
		 WHERE provision_id = $1
		 FOR UPDATE`,
		provisionID,
	).Scan(&lockedAccountID, &lockedTokenID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ProvisionedAccount{}, ErrProvisionReplayUnsafe
		}
		return ProvisionedAccount{},
			fmt.Errorf("lock provision receipt: %w", err)
	}
	if lockedAccountID != accountID || lockedTokenID != priorTokenID {
		return ProvisionedAccount{}, ErrProvisionReplayUnsafe
	}

	var operatorID string
	if err := tx.QueryRow(ctx, `
		SELECT id
		  FROM operators
		 WHERE account_id = $1
		   AND is_root
		   AND deleted_at IS NULL`,
		accountID,
	).Scan(&operatorID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ProvisionedAccount{}, ErrProvisionReplayUnsafe
		}
		return ProvisionedAccount{},
			fmt.Errorf("read provisioned root operator: %w", err)
	}

	var tokenAccountID, tokenKind string
	var tokenOperatorID *string
	var consumedAt *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT account_id, operator_id, kind, consumed_at
		  FROM tokens
		 WHERE id = $1
		 FOR UPDATE`,
		priorTokenID,
	).Scan(
		&tokenAccountID, &tokenOperatorID, &tokenKind, &consumedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ProvisionedAccount{}, ErrProvisionReplayUnsafe
		}
		return ProvisionedAccount{},
			fmt.Errorf("lock provisioned bootstrap: %w", err)
	}
	if tokenAccountID != accountID || tokenOperatorID == nil ||
		*tokenOperatorID != operatorID || tokenKind != "bootstrap" ||
		consumedAt != nil {
		return ProvisionedAccount{}, ErrProvisionReplayUnsafe
	}
	var liveBootstrapCount int
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		  FROM tokens
		 WHERE account_id = $1
		   AND kind = 'bootstrap'
		   AND consumed_at IS NULL`,
		accountID,
	).Scan(&liveBootstrapCount); err != nil {
		return ProvisionedAccount{},
			fmt.Errorf("count provisioned bootstraps: %w", err)
	}
	if liveBootstrapCount != 1 {
		return ProvisionedAccount{}, ErrProvisionReplayUnsafe
	}

	newTokenID, newBootstrap, err := newProvisionBootstrap()
	if err != nil {
		return ProvisionedAccount{}, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE tokens
		   SET consumed_at = clock_timestamp()
		 WHERE id = $1
		   AND consumed_at IS NULL`,
		priorTokenID,
	)
	if err != nil {
		return ProvisionedAccount{},
			fmt.Errorf("revoke prior provisioned bootstrap: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ProvisionedAccount{}, ErrProvisionReplayUnsafe
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO tokens(
			id, account_id, operator_id, kind, token_hash, expires_at
		) VALUES ($1, $2, $3, 'bootstrap', $4, $5)`,
		newTokenID, accountID, operatorID, hashToken(newBootstrap),
		time.Now().UTC().Add(bootstrapTTL),
	); err != nil {
		return ProvisionedAccount{},
			fmt.Errorf("bind replayed bootstrap token: %w", err)
	}
	tag, err = tx.Exec(ctx, `
		UPDATE account_provision_receipts
		   SET bootstrap_token_id = $2,
		       issue_count = issue_count + 1,
		       last_issued_at = clock_timestamp()
		 WHERE provision_id = $1
		   AND bootstrap_token_id = $3`,
		provisionID, newTokenID, priorTokenID,
	)
	if err != nil {
		return ProvisionedAccount{},
			fmt.Errorf("advance provision receipt: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ProvisionedAccount{}, ErrProvisionReplayUnsafe
	}
	if err := tx.Commit(ctx); err != nil {
		return ProvisionedAccount{}, err
	}
	return ProvisionedAccount{
		ProvisionID: provisionID, AccountID: accountID,
		OperatorID: operatorID, Email: email, Status: status,
		BootstrapToken: newBootstrap, Replayed: true,
	}, nil
}

func newProvisionBootstrap() (tokenID, plaintext string, err error) {
	plaintext, err = token.New(token.KindBootstrap)
	if err != nil {
		return "", "", err
	}
	tokenID, err = id.New("tok")
	if err != nil {
		return "", "", err
	}
	return tokenID, plaintext, nil
}
