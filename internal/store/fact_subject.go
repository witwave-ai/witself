package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
)

const (
	maxFactSubjectDisplayNameBytes = 255
	maxFactSubjectAliasBytes       = 255
	maxFactSubjectAliasesJSONBytes = 8192
)

// FactSubject names the person, project, or other entity described by facts.
// CanonicalKey is the stable address; Aliases are normalized lookup-only names.
type FactSubject struct {
	ID           string    `json:"id"`
	AccountID    string    `json:"account_id"`
	RealmID      string    `json:"realm_id"`
	OwnerAgentID string    `json:"owner_agent_id"`
	CanonicalKey string    `json:"canonical_key"`
	DisplayName  string    `json:"display_name,omitempty"`
	Aliases      []string  `json:"aliases"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// UpsertFactSubjectInput creates or updates one stable subject identity.
type UpsertFactSubjectInput struct {
	CanonicalKey string
	DisplayName  string
}

// UpsertFactSubject creates a subject or updates its display name. Canonical
// keys and aliases share one agent-scoped namespace and may not collide.
func (s *Store) UpsertFactSubject(ctx context.Context, p Principal, in UpsertFactSubjectInput) (FactSubject, error) {
	if p.Kind != PrincipalAgent {
		return FactSubject{}, ErrFactForbidden
	}
	in.CanonicalKey = normalizeFactSubject(in.CanonicalKey)
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	if !factSubjectPattern.MatchString(in.CanonicalKey) || !validFactSubjectDisplayName(in.DisplayName) {
		return FactSubject{}, ErrFactInputInvalid
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FactSubject{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return FactSubject{}, err
	}
	if err := lockFactSubjectNamespace(ctx, tx, p, true); err != nil {
		return FactSubject{}, err
	}

	var conflict bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM fact_subjects
		  WHERE account_id = $1 AND realm_id = $2 AND owner_agent_id = $3
		    AND canonical_key <> $4 AND aliases ? $4
		)`, p.AccountID, p.RealmID, p.ID, in.CanonicalKey).Scan(&conflict); err != nil {
		return FactSubject{}, fmt.Errorf("check fact subject key: %w", err)
	}
	if conflict {
		return FactSubject{}, fmt.Errorf("%w: canonical key is already a subject alias", ErrFactInputInvalid)
	}

	subjectID, err := id.New("sub")
	if err != nil {
		return FactSubject{}, err
	}
	out, err := scanFactSubject(tx.QueryRow(ctx, `
		INSERT INTO fact_subjects
		  (id, account_id, realm_id, owner_agent_id, canonical_key, display_name,
		   created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, clock_timestamp(), clock_timestamp())
		ON CONFLICT (owner_agent_id, canonical_key)
		DO UPDATE SET display_name = EXCLUDED.display_name, updated_at = clock_timestamp()
		RETURNING id, account_id, realm_id, owner_agent_id, canonical_key,
		          display_name, aliases, created_at, updated_at`,
		subjectID, p.AccountID, p.RealmID, p.ID, in.CanonicalKey, in.DisplayName))
	if err != nil {
		return FactSubject{}, fmt.Errorf("upsert fact subject: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return FactSubject{}, err
	}
	return out, nil
}

// AddFactSubjectAlias attaches a normalized conversational name to a canonical
// subject. Re-adding an existing alias is idempotent.
func (s *Store) AddFactSubjectAlias(ctx context.Context, p Principal, canonicalKey, alias string) (FactSubject, error) {
	if p.Kind != PrincipalAgent {
		return FactSubject{}, ErrFactForbidden
	}
	canonicalKey = normalizeFactSubject(canonicalKey)
	alias = normalizeFactSubjectAlias(alias)
	if !factSubjectPattern.MatchString(canonicalKey) || !validFactSubjectAlias(alias) {
		return FactSubject{}, ErrFactInputInvalid
	}
	if normalized := normalizeFactSubject(alias); normalized == "self" && canonicalKey != "self" {
		return FactSubject{}, fmt.Errorf("%w: alias is reserved for self", ErrFactInputInvalid)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FactSubject{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return FactSubject{}, err
	}
	if err := lockFactSubjectNamespace(ctx, tx, p, true); err != nil {
		return FactSubject{}, err
	}

	target, err := getFactSubjectByCanonicalKey(ctx, tx, p, canonicalKey)
	if err != nil {
		return FactSubject{}, err
	}
	if alias == target.CanonicalKey || (target.CanonicalKey == "self" && normalizeFactSubject(alias) == "self") {
		if err := tx.Commit(ctx); err != nil {
			return FactSubject{}, err
		}
		return target, nil
	}

	var conflict bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM fact_subjects
		  WHERE account_id = $1 AND realm_id = $2 AND owner_agent_id = $3
		    AND id <> $4 AND (canonical_key = $5 OR aliases ? $5)
		)`, p.AccountID, p.RealmID, p.ID, target.ID, alias).Scan(&conflict); err != nil {
		return FactSubject{}, fmt.Errorf("check fact subject alias: %w", err)
	}
	if conflict {
		return FactSubject{}, fmt.Errorf("%w: alias is already assigned to another subject", ErrFactInputInvalid)
	}
	for _, existing := range target.Aliases {
		if existing == alias {
			if err := tx.Commit(ctx); err != nil {
				return FactSubject{}, err
			}
			return target, nil
		}
	}
	// A proposal may predate this alias (for example, propose "spouse" before
	// attaching that conversational name to person_spouse). Canonicalize those
	// candidate addresses in the same namespace-locked transaction so later
	// review and permanent deletion cannot strand their content under the old
	// alias string.
	if _, err := tx.Exec(ctx, `
		UPDATE fact_candidates SET subject_key=$1
		WHERE account_id=$2 AND realm_id=$3 AND owner_agent_id=$4
		  AND subject_key=$5`, target.CanonicalKey, p.AccountID, p.RealmID, p.ID,
		alias); err != nil {
		return FactSubject{}, fmt.Errorf("canonicalize fact candidate alias: %w", err)
	}
	target.Aliases = append(target.Aliases, alias)
	sort.Strings(target.Aliases)
	aliases, err := json.Marshal(target.Aliases)
	if err != nil {
		return FactSubject{}, err
	}
	if len(aliases) > maxFactSubjectAliasesJSONBytes {
		return FactSubject{}, fmt.Errorf("%w: subject aliases exceed %d bytes", ErrFactInputInvalid, maxFactSubjectAliasesJSONBytes)
	}

	out, err := scanFactSubject(tx.QueryRow(ctx, `
		UPDATE fact_subjects SET aliases = $1::jsonb, updated_at = clock_timestamp()
		WHERE id = $2 AND account_id = $3 AND realm_id = $4 AND owner_agent_id = $5
		RETURNING id, account_id, realm_id, owner_agent_id, canonical_key,
		          display_name, aliases, created_at, updated_at`,
		string(aliases), target.ID, p.AccountID, p.RealmID, p.ID))
	if err != nil {
		return FactSubject{}, fmt.Errorf("add fact subject alias: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return FactSubject{}, err
	}
	return out, nil
}

// ListFactSubjects returns the authenticated agent's subjects by canonical key.
func (s *Store) ListFactSubjects(ctx context.Context, p Principal) ([]FactSubject, error) {
	if p.Kind != PrincipalAgent {
		return nil, ErrFactForbidden
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, account_id, realm_id, owner_agent_id, canonical_key,
		       display_name, aliases, created_at, updated_at
		FROM fact_subjects
		WHERE account_id = $1 AND realm_id = $2 AND owner_agent_id = $3
		ORDER BY canonical_key, id`, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return nil, fmt.Errorf("list fact subjects: %w", err)
	}
	defer rows.Close()
	out := []FactSubject{}
	for rows.Next() {
		subject, err := scanFactSubject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, subject)
	}
	return out, rows.Err()
}

func getFactSubjectByCanonicalKey(ctx context.Context, q factQuerier, p Principal, canonicalKey string) (FactSubject, error) {
	out, err := scanFactSubject(q.QueryRow(ctx, `
		SELECT id, account_id, realm_id, owner_agent_id, canonical_key,
		       display_name, aliases, created_at, updated_at
		FROM fact_subjects
		WHERE account_id = $1 AND realm_id = $2 AND owner_agent_id = $3
		  AND canonical_key = $4`, p.AccountID, p.RealmID, p.ID, canonicalKey))
	if errors.Is(err, pgx.ErrNoRows) {
		return FactSubject{}, ErrFactNotFound
	}
	return out, err
}

func scanFactSubject(row factScanner) (FactSubject, error) {
	var out FactSubject
	var aliases json.RawMessage
	err := row.Scan(&out.ID, &out.AccountID, &out.RealmID, &out.OwnerAgentID,
		&out.CanonicalKey, &out.DisplayName, &aliases, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return FactSubject{}, err
	}
	if err := json.Unmarshal(aliases, &out.Aliases); err != nil {
		return FactSubject{}, fmt.Errorf("decode fact subject aliases: %w", err)
	}
	if out.Aliases == nil {
		out.Aliases = []string{}
	}
	return out, nil
}

// lockFactSubjectNamespace prevents an alias mutation from racing a canonical
// subject creation or an alias-aware proposal. The agent row is the namespace
// lock and is already naturally scoped to one fact owner.
func lockFactSubjectNamespace(ctx context.Context, tx pgx.Tx, p Principal, exclusive bool) error {
	lock := "FOR SHARE OF a"
	if exclusive {
		lock = "FOR UPDATE OF a"
	}
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT true FROM agents a
		JOIN realms r ON r.id = a.realm_id
		WHERE a.id = $1 AND a.realm_id = $2 AND r.account_id = $3
		  AND a.deleted_at IS NULL AND r.deleted_at IS NULL
		`+lock, p.ID, p.RealmID, p.AccountID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAgentNotFound
	}
	if err != nil {
		return fmt.Errorf("lock fact subject namespace: %w", err)
	}
	return nil
}

func resolveFactSubjectCanonicalKey(ctx context.Context, q factQuerier, p Principal, reference string) (string, error) {
	reference = normalizeFactSubject(reference)
	if !validFactSubjectAlias(reference) {
		return "", ErrFactInputInvalid
	}
	var canonicalKey string
	err := q.QueryRow(ctx, `
		SELECT canonical_key FROM fact_subjects
		WHERE account_id = $1 AND realm_id = $2 AND owner_agent_id = $3
		  AND (canonical_key = $4 OR aliases ? $4)
		ORDER BY (canonical_key = $4) DESC, canonical_key
		LIMIT 1`, p.AccountID, p.RealmID, p.ID, reference).Scan(&canonicalKey)
	if err == nil {
		return canonicalKey, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if factSubjectPattern.MatchString(reference) {
		return reference, nil
	}
	return "", ErrFactInputInvalid
}

func normalizeFactSubjectAlias(alias string) string {
	return strings.ToLower(strings.Join(strings.Fields(alias), " "))
}

func validFactSubjectAlias(alias string) bool {
	if alias == "" || len(alias) > maxFactSubjectAliasBytes || !utf8.ValidString(alias) {
		return false
	}
	for _, r := range alias {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validFactSubjectDisplayName(name string) bool {
	if len(name) > maxFactSubjectDisplayNameBytes || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
