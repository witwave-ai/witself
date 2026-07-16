package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// MemoryCurationRequestListOptions bounds the owner-local work queue. An empty
// state means work that is presently due (queued or retry_wait); a named state
// includes that lifecycle state regardless of due_at.
type MemoryCurationRequestListOptions struct {
	State            string
	Limit            int
	Cursor           string
	ExcludeSensitive bool
}

// MemoryCurationRequestPage contains one bounded queue page and its continuation token.
type MemoryCurationRequestPage struct {
	Requests   []MemoryCurationRequest `json:"requests"`
	NextCursor string                  `json:"next_cursor,omitempty"`
}

type memoryCurationRequestCursor struct {
	Version          int       `json:"v"`
	State            string    `json:"state"`
	Restricted       bool      `json:"restricted"`
	ExcludeSensitive bool      `json:"exclude_sensitive,omitempty"`
	AsOf             time.Time `json:"as_of"`
	Priority         int       `json:"priority,omitempty"`
	DueAt            time.Time `json:"due_at,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	RequestID        string    `json:"request_id"`
}

// ListCurationRequests returns a bounded owner-local queue projection. It is a
// metadata read: plans and materialized input values are never joined here.
func (s *Store) ListCurationRequests(
	ctx context.Context,
	p Principal,
	opts MemoryCurationRequestListOptions,
) (MemoryCurationRequestPage, error) {
	if p.Kind != PrincipalAgent {
		return MemoryCurationRequestPage{}, ErrMemoryCurationForbidden
	}
	opts.State = strings.TrimSpace(opts.State)
	if opts.State != "" && !validMemoryCurationRequestState(opts.State) {
		return MemoryCurationRequestPage{}, ErrMemoryCurationInputInvalid
	}
	if opts.Limit == 0 {
		opts.Limit = defaultMemoryCurationPageSize
	}
	if opts.Limit < 1 || opts.Limit > maxMemoryCurationPageSize {
		return MemoryCurationRequestPage{}, ErrMemoryCurationInputInvalid
	}
	var asOf time.Time
	restricted := isRestrictedMemoryCurator(p)
	var afterDue, afterCreated time.Time
	var afterPriority int
	var afterID string
	if strings.TrimSpace(opts.Cursor) != "" {
		cursor, err := decodeMemoryCurationRequestCursor(opts.Cursor, opts.State, restricted, opts.ExcludeSensitive)
		if err != nil {
			return MemoryCurationRequestPage{}, err
		}
		asOf, afterPriority, afterDue, afterCreated, afterID = cursor.AsOf, cursor.Priority, cursor.DueAt, cursor.CreatedAt, cursor.RequestID
	} else if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&asOf); err != nil {
		return MemoryCurationRequestPage{}, fmt.Errorf("read memory curation queue clock: %w", err)
	}

	query := memoryCurationRequestSelectSQL + `
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND created_at <= $4`
	args := []any{p.AccountID, p.RealmID, p.ID, asOf}
	if restricted {
		// A restricted worker must not select a request that it can never
		// consume. Filtering at the persisted queue boundary avoids both scope
		// disclosure and starvation when sensitive work sorts ahead of ordinary
		// due work for the same owner. Transcript entries have no trustworthy
		// sensitivity label yet, so every transcript-bearing scope is treated as
		// sensitive-by-default for curator-preview and curator-apply tokens.
		query += ` AND scope->>'include_sensitive' IS DISTINCT FROM 'true'
		           AND NOT (scope->'sources' @> '["transcript"]'::jsonb)`
	} else if opts.ExcludeSensitive {
		// A full credential may deliberately run with a non-sensitive local
		// policy. Exclude only scopes that explicitly request sensitive values;
		// transcript-bearing scopes remain eligible because their separate
		// sensitive-by-default restriction applies to restricted credentials.
		query += ` AND scope->>'include_sensitive' IS DISTINCT FROM 'true'`
	}
	if opts.State == "" {
		query += ` AND state IN ('queued','retry_wait') AND due_at <= $4`
	} else {
		args = append(args, opts.State)
		query += fmt.Sprintf(` AND state=$%d`, len(args))
	}
	if !afterCreated.IsZero() {
		if opts.State == "" {
			args = append(args, afterPriority, afterDue, afterCreated, afterID)
			query += fmt.Sprintf(` AND (
				priority < $%d OR
				(priority = $%d AND (due_at,created_at,id) > ($%d,$%d,$%d))
			)`, len(args)-3, len(args)-3, len(args)-2, len(args)-1, len(args))
		} else {
			args = append(args, afterCreated, afterID)
			query += fmt.Sprintf(` AND (created_at,id) < ($%d,$%d)`, len(args)-1, len(args))
		}
	}
	args = append(args, opts.Limit+1)
	if opts.State == "" {
		// Claim the highest-priority due work first, then the oldest due and
		// oldest created request. This matches the due-queue index and prevents
		// a steady stream of newer requests from starving older work.
		query += fmt.Sprintf(` ORDER BY priority DESC,due_at,created_at,id LIMIT $%d`, len(args))
	} else {
		query += fmt.Sprintf(` ORDER BY created_at DESC,id DESC LIMIT $%d`, len(args))
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return MemoryCurationRequestPage{}, fmt.Errorf("list memory curation requests: %w", err)
	}
	defer rows.Close()
	out := make([]MemoryCurationRequest, 0, opts.Limit+1)
	for rows.Next() {
		item, err := scanMemoryCurationRequest(rows)
		if err != nil {
			return MemoryCurationRequestPage{}, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return MemoryCurationRequestPage{}, err
	}
	page := MemoryCurationRequestPage{Requests: out}
	if len(out) > opts.Limit {
		page.Requests = out[:opts.Limit]
		last := page.Requests[len(page.Requests)-1]
		page.NextCursor, err = encodeMemoryCurationRequestCursor(memoryCurationRequestCursor{
			Version: 1, State: opts.State, Restricted: restricted,
			ExcludeSensitive: opts.ExcludeSensitive, AsOf: asOf,
			Priority: last.Priority, DueAt: last.DueAt,
			CreatedAt: last.CreatedAt, RequestID: last.ID,
		})
		if err != nil {
			return MemoryCurationRequestPage{}, err
		}
	}
	return page, nil
}

func encodeMemoryCurationRequestCursor(cursor memoryCurationRequestCursor) (string, error) {
	if cursor.Version != 1 || cursor.AsOf.IsZero() || cursor.CreatedAt.IsZero() ||
		!validCurationID(cursor.RequestID, "mcrq") ||
		(cursor.State != "" && !validMemoryCurationRequestState(cursor.State)) ||
		(cursor.State == "" && (cursor.DueAt.IsZero() || cursor.DueAt.After(cursor.AsOf))) {
		return "", ErrMemoryCurationInputInvalid
	}
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode memory curation request cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeMemoryCurationRequestCursor(raw, state string, restricted, excludeSensitive bool) (memoryCurationRequestCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return memoryCurationRequestCursor{}, ErrMemoryCurationInputInvalid
	}
	var cursor memoryCurationRequestCursor
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.Version != 1 ||
		cursor.State != state || cursor.Restricted != restricted || cursor.ExcludeSensitive != excludeSensitive ||
		cursor.AsOf.IsZero() || cursor.CreatedAt.IsZero() ||
		cursor.CreatedAt.After(cursor.AsOf) || !validCurationID(cursor.RequestID, "mcrq") ||
		(cursor.State != "" && !validMemoryCurationRequestState(cursor.State)) ||
		(cursor.State == "" && (cursor.DueAt.IsZero() || cursor.DueAt.After(cursor.AsOf))) {
		return memoryCurationRequestCursor{}, ErrMemoryCurationInputInvalid
	}
	cursor.AsOf = cursor.AsOf.UTC()
	cursor.DueAt = cursor.DueAt.UTC()
	cursor.CreatedAt = cursor.CreatedAt.UTC()
	return cursor, nil
}

func validMemoryCurationRequestState(state string) bool {
	switch state {
	case MemoryCurationRequestQueued,
		MemoryCurationRequestClaimed,
		MemoryCurationRequestRetryWait,
		MemoryCurationRequestFulfilled,
		MemoryCurationRequestCancelled,
		MemoryCurationRequestDeadLetter:
		return true
	default:
		return false
	}
}
