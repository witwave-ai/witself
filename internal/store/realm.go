package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/plans"
)

// ErrRealmExists is returned when a realm name already exists in the account.
var ErrRealmExists = errors.New("realm already exists")

// ErrRealmNotEmpty is returned when a realm still has live agents.
var ErrRealmNotEmpty = errors.New("realm is not empty")

var (
	// ErrRealmEmailRouteInputInvalid identifies a malformed inventory cursor or
	// lifecycle fence.  The provision-token HTTP boundary maps it to 400.
	ErrRealmEmailRouteInputInvalid = errors.New("invalid realm email route request")
	// ErrRealmEmailRouteConflict fences stale generations, another operation,
	// and any close attempted while child resources remain live.
	ErrRealmEmailRouteConflict = errors.New("realm email route lifecycle conflict")
	// ErrRealmEmailRouteRetirementRequired prevents a managed cell's ordinary
	// operator DELETE from bypassing the control-plane route-removal handshake.
	ErrRealmEmailRouteRetirementRequired = errors.New("realm email route retirement is required")
)

const (
	// RealmEmailRouteLive identifies a canonical route that accepts agents.
	RealmEmailRouteLive = "live"
	// RealmEmailRouteClosing identifies a route frozen for exact retirement.
	RealmEmailRouteClosing = "closing"
	// RealmEmailRouteRetired identifies a permanently tombstoned route.
	RealmEmailRouteRetired = "retired"

	defaultRealmEmailRoutePageSize   = 100
	maximumRealmEmailRoutePageSize   = 100
	maximumRealmEmailRouteGeneration = int64(4611686018427387903)
)

var realmEmailRouteOperationIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// Realm is a realm row (id + name).
type Realm struct {
	ID   string
	Name string
}

// RealmEmailRouteLifecycle is the portable, value-free cell fence for one
// realm's canonical email route.  Generation advances exactly once when a
// retirement operation is prepared and remains stable through commit so a
// controller can retry either half without ambiguity.
type RealmEmailRouteLifecycle struct {
	AccountID   string `json:"account_id"`
	RealmID     string `json:"realm_id"`
	State       string `json:"state"`
	Generation  int64  `json:"generation"`
	OperationID string `json:"operation_id,omitempty"`
}

// RealmEmailRouteLifecyclePage is a bounded keyset page.  NextCursor is
// opaque to callers and empty only on the final page.
type RealmEmailRouteLifecyclePage struct {
	Routes     []RealmEmailRouteLifecycle
	NextCursor string
}

// RealmEmailRouteRetirementInput carries the exact controller operation and
// generation fence used by prepare and commit.
type RealmEmailRouteRetirementInput struct {
	RealmID            string
	OperationID        string
	ExpectedGeneration int64
}

// CreateRealm creates a realm in the account and returns it. A duplicate name
// in the same account returns ErrRealmExists.
func (s *Store) CreateRealm(ctx context.Context, accountID, name string) (Realm, error) {
	realmID, err := id.New("realm")
	if err != nil {
		return Realm{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Realm{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The plan-gate lock subsumes the mint lock's status check and also
	// serializes concurrent creates, so the count below cannot race past the
	// account's realm cap.
	plan, limits, err := lockAccountForPlanGate(ctx, tx, accountID)
	if err != nil {
		return Realm{}, err
	}
	// Resolve a reserved live or tombstoned name before the cap so a retry is
	// reported as a name conflict rather than a misleading upgrade request.
	var nameReserved bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM realms WHERE account_id = $1 AND name = $2
		 )`,
		accountID, name).Scan(&nameReserved); err != nil {
		return Realm{}, fmt.Errorf("check realm name: %w", err)
	}
	if nameReserved {
		return Realm{}, ErrRealmExists
	}
	if _, capped := limits[plans.RealmLimit]; capped {
		n, err := countLiveRealms(ctx, tx, accountID)
		if err != nil {
			return Realm{}, err
		}
		if err := checkPlanLimit(plan, limits, plans.RealmLimit, n); err != nil {
			return Realm{}, err
		}
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO realms (id, account_id, name) VALUES ($1, $2, $3)`,
		realmID, accountID, name); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			return Realm{}, ErrRealmExists
		}
		return Realm{}, fmt.Errorf("create realm: %w", err)
	}
	if err := createDefaultRealmAvatarStyleTx(ctx, tx, accountID, realmID); err != nil {
		return Realm{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Realm{}, err
	}
	return Realm{ID: realmID, Name: name}, nil
}

// ListRealms returns the account's realms, oldest first.
func (s *Store) ListRealms(ctx context.Context, accountID string) ([]Realm, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name FROM realms
		 WHERE account_id = $1 AND deleted_at IS NULL
		 ORDER BY created_at`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list realms: %w", err)
	}
	defer rows.Close()

	var out []Realm
	for rows.Next() {
		var r Realm
		if err := rows.Scan(&r.ID, &r.Name); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRealm soft-deletes an empty realm in the account. Realms with live
// agents are left intact so agent identity and token revocation happen
// explicitly before the container is retired.
func (s *Store) DeleteRealm(ctx context.Context, accountID, realmID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockAccountForMint(ctx, tx, accountID, false); err != nil {
		return err
	}
	var routeState string
	var routeGeneration int64
	err = tx.QueryRow(ctx,
		`SELECT email_route_state,email_route_generation FROM realms
		 WHERE id = $1 AND account_id = $2 AND deleted_at IS NULL
		 FOR UPDATE`,
		realmID, accountID).Scan(&routeState, &routeGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRealmNotFound
	}
	if err != nil {
		return fmt.Errorf("verify realm: %w", err)
	}
	if routeState != RealmEmailRouteLive ||
		routeGeneration >= maximumRealmEmailRouteGeneration {
		return ErrRealmEmailRouteRetirementRequired
	}

	var agentCount int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM agents
		 WHERE realm_id = $1 AND deleted_at IS NULL`,
		realmID).Scan(&agentCount); err != nil {
		return fmt.Errorf("count realm agents: %w", err)
	}
	if agentCount > 0 {
		return ErrRealmNotEmpty
	}

	// A live control-plane realm alias is destination authority just like a
	// live agent. Refuse the soft delete until the globally fenced controller
	// has projected a terminal retirement; otherwise a realm could disappear
	// after cell acknowledgement but before the edge route is published.
	var liveAliasCount int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_realm_aliases
		 WHERE account_id=$1 AND realm_id=$2 AND state<>'retired'`,
		accountID, realmID).Scan(&liveAliasCount); err != nil {
		return fmt.Errorf("count realm email aliases: %w", err)
	}
	if liveAliasCount > 0 {
		return ErrRealmNotEmpty
	}

	if _, err := tx.Exec(ctx,
		`UPDATE realms
		 SET deleted_at = now(), updated_at = now(),
		     email_route_state = 'retired',
		     email_route_generation = email_route_generation + 1,
		     email_route_operation_id = 'selfhosted_delete'
		 WHERE id = $1 AND account_id = $2 AND deleted_at IS NULL
		   AND email_route_state = 'live'`,
		realmID, accountID); err != nil {
		return fmt.Errorf("delete realm: %w", err)
	}
	return tx.Commit(ctx)
}

// RefuseManagedRealmDelete preserves the public DELETE surface for API
// compatibility while making it read-only on managed cells.  The only write
// path there is CommitRealmEmailRouteRetirement with its exact operation
// fence.  A missing/tombstoned realm still reports not found.
func (s *Store) RefuseManagedRealmDelete(ctx context.Context, accountID, realmID string) error {
	accountID = strings.TrimSpace(accountID)
	realmID = strings.TrimSpace(realmID)
	if accountID == "" || !validRealmEmailRouteRealmID(realmID) {
		return ErrRealmNotFound
	}
	var live bool
	if err := s.pool.QueryRow(ctx, `
		SELECT deleted_at IS NULL
		  FROM realms
		 WHERE account_id=$1 AND id=$2`, accountID, realmID).Scan(&live); errors.Is(err, pgx.ErrNoRows) {
		return ErrRealmNotFound
	} else if err != nil {
		return fmt.Errorf("read managed realm deletion fence: %w", err)
	}
	if !live {
		return ErrRealmNotFound
	}
	return ErrRealmEmailRouteRetirementRequired
}

// GetRealmEmailRouteLifecycle returns one exact route lifecycle, including a
// retired tombstone.  It is the control plane's preflight for a close and does
// not expose the realm name or any account content.
func (s *Store) GetRealmEmailRouteLifecycle(
	ctx context.Context,
	accountID, realmID string,
) (RealmEmailRouteLifecycle, error) {
	accountID = strings.TrimSpace(accountID)
	realmID = strings.TrimSpace(realmID)
	if accountID == "" || !validRealmEmailRouteRealmID(realmID) {
		return RealmEmailRouteLifecycle{}, ErrRealmEmailRouteInputInvalid
	}
	var route RealmEmailRouteLifecycle
	err := s.pool.QueryRow(ctx, `
		SELECT account_id,id,email_route_state,email_route_generation,
		       COALESCE(email_route_operation_id,'')
		  FROM realms
		 WHERE account_id=$1 AND id=$2`, accountID, realmID).
		Scan(&route.AccountID, &route.RealmID, &route.State,
			&route.Generation, &route.OperationID)
	if errors.Is(err, pgx.ErrNoRows) {
		var accountExists bool
		if accountErr := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM accounts WHERE id=$1)`, accountID).
			Scan(&accountExists); accountErr != nil {
			return RealmEmailRouteLifecycle{}, fmt.Errorf("resolve realm email route account: %w", accountErr)
		}
		if !accountExists {
			return RealmEmailRouteLifecycle{}, ErrAccountNotFound
		}
		return RealmEmailRouteLifecycle{}, ErrRealmNotFound
	}
	if err != nil {
		return RealmEmailRouteLifecycle{}, fmt.Errorf("get realm email route lifecycle: %w", err)
	}
	return route, nil
}

// ListRealmEmailRouteLifecycles returns live, closing, and retired realms so
// recovery can both provision missing routes and preserve terminal tombstones.
func (s *Store) ListRealmEmailRouteLifecycles(
	ctx context.Context,
	accountID, cursor string,
	limit int,
) (RealmEmailRouteLifecyclePage, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return RealmEmailRouteLifecyclePage{}, ErrRealmEmailRouteInputInvalid
	}
	if limit == 0 {
		limit = defaultRealmEmailRoutePageSize
	}
	if limit < 1 || limit > maximumRealmEmailRoutePageSize {
		return RealmEmailRouteLifecyclePage{}, ErrRealmEmailRouteInputInvalid
	}
	afterRealmID := ""
	if cursor != "" {
		var err error
		afterRealmID, err = decodeRealmEmailRouteCursor(cursor)
		if err != nil {
			return RealmEmailRouteLifecyclePage{}, err
		}
	}
	var accountExists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM accounts WHERE id=$1)`, accountID).
		Scan(&accountExists); err != nil {
		return RealmEmailRouteLifecyclePage{}, fmt.Errorf("resolve realm email route account: %w", err)
	}
	if !accountExists {
		return RealmEmailRouteLifecyclePage{}, ErrAccountNotFound
	}
	rows, err := s.pool.Query(ctx, `
		SELECT account_id,id,email_route_state,email_route_generation,
		       COALESCE(email_route_operation_id,'')
		  FROM realms
		 WHERE account_id=$1 AND id>$2
		 ORDER BY id
		 LIMIT $3`, accountID, afterRealmID, limit+1)
	if err != nil {
		return RealmEmailRouteLifecyclePage{}, fmt.Errorf("list realm email route lifecycles: %w", err)
	}
	defer rows.Close()
	routes := make([]RealmEmailRouteLifecycle, 0, limit+1)
	for rows.Next() {
		var route RealmEmailRouteLifecycle
		if err := rows.Scan(&route.AccountID, &route.RealmID, &route.State,
			&route.Generation, &route.OperationID); err != nil {
			return RealmEmailRouteLifecyclePage{}, fmt.Errorf("scan realm email route lifecycle: %w", err)
		}
		routes = append(routes, route)
	}
	if err := rows.Err(); err != nil {
		return RealmEmailRouteLifecyclePage{}, fmt.Errorf("iterate realm email route lifecycles: %w", err)
	}
	page := RealmEmailRouteLifecyclePage{Routes: routes}
	if len(routes) > limit {
		page.Routes = routes[:limit]
		page.NextCursor = encodeRealmEmailRouteCursor(page.Routes[len(page.Routes)-1].RealmID)
	}
	return page, nil
}

// PrepareRealmEmailRouteRetirement atomically proves the realm is empty,
// fences all account-serialized creates/projections, and binds one operation
// to the next generation.  Exact retries return the stored fence.
func (s *Store) PrepareRealmEmailRouteRetirement(
	ctx context.Context,
	accountID string,
	in RealmEmailRouteRetirementInput,
) (RealmEmailRouteLifecycle, error) {
	accountID = strings.TrimSpace(accountID)
	in.RealmID = strings.TrimSpace(in.RealmID)
	in.OperationID = strings.TrimSpace(in.OperationID)
	if accountID == "" || !validRealmEmailRouteRealmID(in.RealmID) ||
		!realmEmailRouteOperationIDPattern.MatchString(in.OperationID) ||
		in.ExpectedGeneration < 1 ||
		in.ExpectedGeneration >= maximumRealmEmailRouteGeneration {
		return RealmEmailRouteLifecycle{}, ErrRealmEmailRouteInputInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RealmEmailRouteLifecycle{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockRealmEmailRouteAccount(ctx, tx, accountID); err != nil {
		return RealmEmailRouteLifecycle{}, err
	}
	route, deleted, err := realmEmailRouteLifecycleTx(ctx, tx, accountID, in.RealmID, true)
	if err != nil {
		return RealmEmailRouteLifecycle{}, err
	}
	if route.State == RealmEmailRouteClosing || route.State == RealmEmailRouteRetired {
		if route.OperationID == in.OperationID &&
			route.Generation == in.ExpectedGeneration+1 {
			if err := tx.Commit(ctx); err != nil {
				return RealmEmailRouteLifecycle{}, err
			}
			return route, nil
		}
		return RealmEmailRouteLifecycle{}, ErrRealmEmailRouteConflict
	}
	if deleted || route.State != RealmEmailRouteLive ||
		route.OperationID != "" || route.Generation != in.ExpectedGeneration {
		return RealmEmailRouteLifecycle{}, ErrRealmEmailRouteConflict
	}
	if err := requireRealmEmailRouteEmptyTx(ctx, tx, accountID, in.RealmID); err != nil {
		return RealmEmailRouteLifecycle{}, err
	}
	route, err = scanRealmEmailRouteLifecycle(tx.QueryRow(ctx, `
		UPDATE realms
		   SET email_route_state='closing',
		       email_route_generation=email_route_generation+1,
		       email_route_operation_id=$3,
		       updated_at=now()
		 WHERE account_id=$1 AND id=$2
		   AND deleted_at IS NULL
		   AND email_route_state='live'
		   AND email_route_generation=$4
		RETURNING account_id,id,email_route_state,email_route_generation,
		          COALESCE(email_route_operation_id,'')`,
		accountID, in.RealmID, in.OperationID, in.ExpectedGeneration))
	if errors.Is(err, pgx.ErrNoRows) {
		return RealmEmailRouteLifecycle{}, ErrRealmEmailRouteConflict
	}
	if err != nil {
		return RealmEmailRouteLifecycle{}, fmt.Errorf("prepare realm email route retirement: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RealmEmailRouteLifecycle{}, err
	}
	return route, nil
}

// CommitRealmEmailRouteRetirement is the sole managed deletion mutation.  It
// requires the exact closing generation and operation, rechecks emptiness,
// and writes the retired route tombstone and realm soft-delete atomically.
func (s *Store) CommitRealmEmailRouteRetirement(
	ctx context.Context,
	accountID string,
	in RealmEmailRouteRetirementInput,
) (RealmEmailRouteLifecycle, error) {
	accountID = strings.TrimSpace(accountID)
	in.RealmID = strings.TrimSpace(in.RealmID)
	in.OperationID = strings.TrimSpace(in.OperationID)
	if accountID == "" || !validRealmEmailRouteRealmID(in.RealmID) ||
		!realmEmailRouteOperationIDPattern.MatchString(in.OperationID) ||
		in.ExpectedGeneration < 2 ||
		in.ExpectedGeneration > maximumRealmEmailRouteGeneration {
		return RealmEmailRouteLifecycle{}, ErrRealmEmailRouteInputInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RealmEmailRouteLifecycle{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockRealmEmailRouteAccount(ctx, tx, accountID); err != nil {
		return RealmEmailRouteLifecycle{}, err
	}
	route, deleted, err := realmEmailRouteLifecycleTx(ctx, tx, accountID, in.RealmID, true)
	if err != nil {
		return RealmEmailRouteLifecycle{}, err
	}
	if route.State == RealmEmailRouteRetired && deleted &&
		route.OperationID == in.OperationID &&
		route.Generation == in.ExpectedGeneration {
		if err := tx.Commit(ctx); err != nil {
			return RealmEmailRouteLifecycle{}, err
		}
		return route, nil
	}
	if deleted || route.State != RealmEmailRouteClosing ||
		route.OperationID != in.OperationID ||
		route.Generation != in.ExpectedGeneration {
		return RealmEmailRouteLifecycle{}, ErrRealmEmailRouteConflict
	}
	if err := requireRealmEmailRouteEmptyTx(ctx, tx, accountID, in.RealmID); err != nil {
		return RealmEmailRouteLifecycle{}, err
	}
	route, err = scanRealmEmailRouteLifecycle(tx.QueryRow(ctx, `
		UPDATE realms
		   SET email_route_state='retired',
		       deleted_at=now(),
		       updated_at=now()
		 WHERE account_id=$1 AND id=$2
		   AND deleted_at IS NULL
		   AND email_route_state='closing'
		   AND email_route_generation=$3
		   AND email_route_operation_id=$4
		RETURNING account_id,id,email_route_state,email_route_generation,
		          COALESCE(email_route_operation_id,'')`,
		accountID, in.RealmID, in.ExpectedGeneration, in.OperationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return RealmEmailRouteLifecycle{}, ErrRealmEmailRouteConflict
	}
	if err != nil {
		return RealmEmailRouteLifecycle{}, fmt.Errorf("commit realm email route retirement: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RealmEmailRouteLifecycle{}, err
	}
	return route, nil
}

func lockRealmEmailRouteAccount(ctx context.Context, tx pgx.Tx, accountID string) error {
	var locked string
	err := tx.QueryRow(ctx,
		`SELECT id FROM accounts WHERE id=$1 FOR NO KEY UPDATE`, accountID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccountNotFound
	}
	if err != nil {
		return fmt.Errorf("lock realm email route account: %w", err)
	}
	return nil
}

func realmEmailRouteLifecycleTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID, realmID string,
	lock bool,
) (RealmEmailRouteLifecycle, bool, error) {
	query := `
		SELECT account_id,id,email_route_state,email_route_generation,
		       COALESCE(email_route_operation_id,''),deleted_at IS NOT NULL
		  FROM realms
		 WHERE account_id=$1 AND id=$2`
	if lock {
		query += ` FOR UPDATE`
	}
	var route RealmEmailRouteLifecycle
	var deleted bool
	err := tx.QueryRow(ctx, query, accountID, realmID).Scan(
		&route.AccountID, &route.RealmID, &route.State,
		&route.Generation, &route.OperationID, &deleted,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RealmEmailRouteLifecycle{}, false, ErrRealmNotFound
	}
	if err != nil {
		return RealmEmailRouteLifecycle{}, false, fmt.Errorf("read realm email route lifecycle: %w", err)
	}
	return route, deleted, nil
}

func requireRealmEmailRouteEmptyTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID, realmID string,
) error {
	var liveAgents, liveAliases int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM agents
		 WHERE realm_id=$1 AND deleted_at IS NULL`, realmID).Scan(&liveAgents); err != nil {
		return fmt.Errorf("count realm agents for route retirement: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_realm_aliases
		 WHERE account_id=$1 AND realm_id=$2 AND state<>'retired'`,
		accountID, realmID).Scan(&liveAliases); err != nil {
		return fmt.Errorf("count realm aliases for route retirement: %w", err)
	}
	if liveAgents > 0 || liveAliases > 0 {
		return ErrRealmEmailRouteConflict
	}
	return nil
}

type realmEmailRouteScanner interface {
	Scan(dest ...any) error
}

func scanRealmEmailRouteLifecycle(row realmEmailRouteScanner) (RealmEmailRouteLifecycle, error) {
	var route RealmEmailRouteLifecycle
	err := row.Scan(&route.AccountID, &route.RealmID, &route.State,
		&route.Generation, &route.OperationID)
	return route, err
}

func encodeRealmEmailRouteCursor(realmID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(realmID))
}

func decodeRealmEmailRouteCursor(cursor string) (string, error) {
	if strings.TrimSpace(cursor) != cursor {
		return "", ErrRealmEmailRouteInputInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != cursor {
		return "", ErrRealmEmailRouteInputInvalid
	}
	realmID := string(raw)
	if !validRealmEmailRouteRealmID(realmID) {
		return "", ErrRealmEmailRouteInputInvalid
	}
	return realmID, nil
}

func validRealmEmailRouteRealmID(value string) bool {
	return validRealmID(value)
}
