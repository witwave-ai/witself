package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type secretQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// GetCurrentVaultKey returns only the authenticated agent's public key
// binding. An absent row is a valid uninitialized state.
func (s *Store) GetCurrentVaultKey(ctx context.Context, p Principal) (*VaultKeyBinding, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return nil, err
	}
	return getCurrentVaultKey(ctx, s.pool, p)
}

func getCurrentVaultKey(ctx context.Context, q secretQuerier, p Principal) (*VaultKeyBinding, error) {
	var key VaultKeyBinding
	err := q.QueryRow(ctx, `
		SELECT id, account_id, realm_id, owner_agent_id, key_version,
		       algorithm, fingerprint, lifecycle_state, row_version,
		       created_at, retired_at
		  FROM agent_vault_keys
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND lifecycle_state='current'`, p.AccountID, p.RealmID, p.ID).Scan(
		&key.ID, &key.AccountID, &key.RealmID, &key.OwnerAgentID,
		&key.KeyVersion, &key.Algorithm, &key.Fingerprint,
		&key.LifecycleState, &key.RowVersion, &key.CreatedAt, &key.RetiredAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read current vault key binding: %w", err)
	}
	return &key, nil
}

// GetSecret returns one redacted structured secret owned by the authenticated
// agent. Sensitive values and envelopes are never selected by this path.
func (s *Store) GetSecret(ctx context.Context, p Principal, secretID string) (Secret, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return Secret{}, err
	}
	if !validSecretGeneratedID(strings.TrimSpace(secretID), "sec") {
		return Secret{}, ErrSecretNotFound
	}
	return getSecret(ctx, s.pool, p, strings.TrimSpace(secretID), true, false)
}

func getSecret(ctx context.Context, q secretQuerier, p Principal, secretID string, includeFields, includeArchived bool) (Secret, error) {
	return getSecretWithDeleted(ctx, q, p, secretID, includeFields, includeArchived, false)
}

// getSecretWithDeleted is reserved for guarded delete results and receipt
// replay. Ordinary Get/List/Access paths always call getSecret and cannot see
// tombstones.
func getSecretWithDeleted(ctx context.Context, q secretQuerier, p Principal, secretID string, includeFields, includeArchived, includeDeleted bool) (Secret, error) {
	var out Secret
	var tagsJSON []byte
	err := q.QueryRow(ctx, `
		SELECT id, account_id, realm_id, owner_agent_id, name, description,
		       template, tags, row_version, created_at, updated_at,
		       archived_at, deleted_at,
		       (SELECT count(*) FROM secret_fields f
		         WHERE f.account_id=s.account_id AND f.realm_id=s.realm_id
		           AND f.owner_agent_id=s.owner_agent_id AND f.secret_id=s.id
		           AND f.sensitive)
		  FROM secrets s
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND id=$4 AND ($6 OR deleted_at IS NULL)
		   AND ($5 OR archived_at IS NULL)`, p.AccountID, p.RealmID, p.ID,
		secretID, includeArchived, includeDeleted).Scan(
		&out.ID, &out.AccountID, &out.RealmID, &out.OwnerAgentID,
		&out.Name, &out.Description, &out.Template, &tagsJSON,
		&out.RowVersion, &out.CreatedAt, &out.UpdatedAt,
		&out.ArchivedAt, &out.DeletedAt, &out.SensitiveCount,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Secret{}, ErrSecretNotFound
	}
	if err != nil {
		return Secret{}, fmt.Errorf("read secret: %w", err)
	}
	if err := json.Unmarshal(tagsJSON, &out.Tags); err != nil {
		return Secret{}, fmt.Errorf("decode secret tags: %w", err)
	}
	if out.Tags == nil {
		out.Tags = []string{}
	}
	out.Lifecycle = SecretLifecycleActive
	if out.DeletedAt != nil {
		out.Lifecycle = SecretLifecycleDeleted
	} else if out.ArchivedAt != nil {
		out.Lifecycle = SecretLifecycleArchived
	}
	if includeFields {
		out.Fields, err = listSecretFields(ctx, q, p, out.ID)
		if err != nil {
			return Secret{}, err
		}
	}
	return out, nil
}

func listSecretFields(ctx context.Context, q secretQuerier, p Principal, secretID string) ([]SecretField, error) {
	rows, err := q.Query(ctx, `
		SELECT id, name, field_kind, sensitive, value_encoding,
		       value_version, public_value, row_version,
		       coalesce(dek_generation, 0)
		  FROM secret_fields
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND secret_id=$4
		 ORDER BY name, id`, p.AccountID, p.RealmID, p.ID, secretID)
	if err != nil {
		return nil, fmt.Errorf("list secret fields: %w", err)
	}
	defer rows.Close()
	fields := []SecretField{}
	for rows.Next() {
		var field SecretField
		if err := rows.Scan(&field.ID, &field.Name, &field.Kind,
			&field.Sensitive, &field.Encoding, &field.ValueVersion,
			&field.PublicValue, &field.RowVersion, &field.DEKGeneration); err != nil {
			return nil, err
		}
		field.Redacted = field.Sensitive
		if field.Sensitive {
			field.PublicValue = nil
		}
		fields = append(fields, field)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return fields, nil
}

type secretCursor struct {
	UpdatedAt time.Time `json:"updated_at"`
	ID        string    `json:"id"`
}

// ListSecrets performs bounded PostgreSQL search over public metadata and
// explicitly non-sensitive field values only.
func (s *Store) ListSecrets(ctx context.Context, p Principal, options SecretListOptions) (SecretPage, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return SecretPage{}, err
	}
	options, err := normalizeSecretListOptions(options)
	if err != nil {
		return SecretPage{}, err
	}
	var before secretCursor
	if options.Cursor != "" {
		before, err = decodeSecretCursor(options.Cursor)
		if err != nil {
			return SecretPage{}, err
		}
	}
	tagsJSON, err := json.Marshal(options.Tags)
	if err != nil {
		return SecretPage{}, ErrSecretInputInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return SecretPage{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT s.id
		  FROM secrets s
		 WHERE s.account_id=$1 AND s.realm_id=$2 AND s.owner_agent_id=$3
		   AND s.deleted_at IS NULL
		   AND ($4='' OR ($4='active' AND s.archived_at IS NULL) OR
		                  ($4='archived' AND s.archived_at IS NOT NULL))
		   AND ($5='' OR s.template=$5)
		   AND ($6::jsonb='[]'::jsonb OR s.tags @> $6::jsonb)
		   AND ($7='' OR s.search_document @@ plainto_tsquery('simple', $7) OR
		        EXISTS (
		          SELECT 1 FROM secret_fields f
		           WHERE f.account_id=s.account_id AND f.realm_id=s.realm_id
		             AND f.owner_agent_id=s.owner_agent_id AND f.secret_id=s.id
		             AND f.public_search_document @@ plainto_tsquery('simple', $7)
		        ))
		   AND ($8::timestamptz IS NULL OR (s.updated_at, s.id) < ($8, $9))
		 ORDER BY s.updated_at DESC, s.id DESC
		 LIMIT $10`, p.AccountID, p.RealmID, p.ID, options.Lifecycle,
		options.Template, string(tagsJSON), options.Query, nullableSecretCursorTime(before),
		before.ID, options.Limit+1)
	if err != nil {
		return SecretPage{}, fmt.Errorf("list secrets: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0, options.Limit+1)
	for rows.Next() {
		var secretID string
		if err := rows.Scan(&secretID); err != nil {
			return SecretPage{}, err
		}
		ids = append(ids, secretID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return SecretPage{}, err
	}
	rows.Close()
	hasMore := len(ids) > options.Limit
	if hasMore {
		ids = ids[:options.Limit]
	}
	page := SecretPage{Secrets: []Secret{}}
	for _, secretID := range ids {
		secret, err := getSecret(ctx, tx, p, secretID, options.IncludeFields, true)
		if err != nil {
			return SecretPage{}, err
		}
		page.Secrets = append(page.Secrets, secret)
	}
	if hasMore && len(page.Secrets) > 0 {
		last := page.Secrets[len(page.Secrets)-1]
		page.NextCursor, err = encodeSecretCursor(secretCursor{UpdatedAt: last.UpdatedAt, ID: last.ID})
		if err != nil {
			return SecretPage{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return SecretPage{}, err
	}
	return page, nil
}

func nullableSecretCursorTime(cursor secretCursor) any {
	if cursor.UpdatedAt.IsZero() {
		return nil
	}
	return cursor.UpdatedAt
}

func encodeSecretCursor(cursor secretCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeSecretCursor(value string) (secretCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) > 1024 {
		return secretCursor{}, ErrSecretInputInvalid
	}
	var cursor secretCursor
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil || cursor.UpdatedAt.IsZero() ||
		!validSecretGeneratedID(cursor.ID, "sec") {
		return secretCursor{}, ErrSecretInputInvalid
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return secretCursor{}, ErrSecretInputInvalid
	}
	return cursor, nil
}

// AccessSecretField returns one ciphertext/wrapped-DEK package and records
// encrypted material delivery. Decryption remains entirely client-side.
func (s *Store) AccessSecretField(ctx context.Context, p Principal, secretID, fieldID string, in AccessSecretFieldInput) (SecretMaterial, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return SecretMaterial{}, err
	}
	secretID = strings.TrimSpace(secretID)
	fieldID = strings.TrimSpace(fieldID)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if !validSecretGeneratedID(secretID, "sec") || !validSecretGeneratedID(fieldID, "fld") ||
		len(in.IdempotencyKey) < 1 || len(in.IdempotencyKey) > maxSecretIdempotencyKeyBytes {
		return SecretMaterial{}, ErrSecretInputInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SecretMaterial{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return SecretMaterial{}, err
	}
	var out SecretMaterial
	err = tx.QueryRow(ctx, `
		SELECT s.id, f.id, f.name, f.field_kind, f.value_encoding,
		       f.value_version, f.envelope_version, f.ciphertext,
		       f.aead_algorithm, f.aad_version,
		       d.id, d.dek_generation, d.wrapped_dek, d.wrap_algorithm,
		       d.aad_version, d.wrap_revision, d.wrapping_key_id,
		       d.wrapping_key_version, s.row_version, f.row_version
		  FROM secrets s
		  JOIN secret_fields f
		    ON f.account_id=s.account_id AND f.realm_id=s.realm_id
		   AND f.owner_agent_id=s.owner_agent_id AND f.secret_id=s.id
		  JOIN secret_deks d
		    ON d.account_id=f.account_id AND d.realm_id=f.realm_id
		   AND d.owner_agent_id=f.owner_agent_id AND d.secret_id=f.secret_id
		   AND d.field_id=f.id AND d.id=f.dek_id
		   AND d.dek_generation=f.dek_generation AND d.retired_at IS NULL
		  JOIN agent_vault_keys k
		    ON k.account_id=d.account_id AND k.realm_id=d.realm_id
		   AND k.owner_agent_id=d.owner_agent_id AND k.id=d.wrapping_key_id
		   AND k.key_version=d.wrapping_key_version
		   AND k.lifecycle_state='current'
		 WHERE s.account_id=$1 AND s.realm_id=$2 AND s.owner_agent_id=$3
		   AND s.id=$4 AND s.deleted_at IS NULL AND s.archived_at IS NULL
		   AND f.id=$5 AND f.sensitive
		 FOR SHARE OF s, f, d, k`, p.AccountID, p.RealmID, p.ID, secretID, fieldID).Scan(
		&out.SecretID, &out.FieldID, &out.FieldName, &out.FieldKind,
		&out.Encoding, &out.ValueVersion, &out.EnvelopeVersion,
		&out.Ciphertext, &out.Algorithm,
		&out.AADVersion, &out.DEK.ID, &out.DEK.Generation,
		&out.DEK.WrappedDEK, &out.DEK.WrapAlgorithm, &out.DEK.AADVersion,
		&out.DEK.WrapRevision, &out.DEK.WrappingKeyID,
		&out.DEK.WrappingKeyVersion, &out.SecretRevision, &out.FieldRevision,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return SecretMaterial{}, ErrSecretFieldNotFound
	}
	if err != nil {
		return SecretMaterial{}, fmt.Errorf("authorize sealed field material: %w", err)
	}
	keyHash := secretIdempotencyKeyHash(in.IdempotencyKey)
	materialFingerprint := secretMaterialFingerprint(out)
	requestHash, err := secretMutationFingerprint(struct {
		SecretID            string `json:"secret_id"`
		FieldID             string `json:"field_id"`
		MaterialFingerprint string `json:"material_fingerprint"`
	}{out.SecretID, out.FieldID, materialFingerprint})
	if err != nil {
		return SecretMaterial{}, err
	}
	if err := lockSecretIdempotencyKey(ctx, tx, p, "field_access", keyHash); err != nil {
		return SecretMaterial{}, err
	}
	_, replayed, err := replaySecretReceiptTx(ctx, tx, p,
		"field_access", keyHash, requestHash)
	if err != nil {
		return SecretMaterial{}, err
	}
	if !replayed {
		if _, err := insertSecretReceiptTx(ctx, tx, p, "field_access", keyHash,
			requestHash, "field", out.FieldID, out.FieldRevision, out.ValueVersion); err != nil {
			return SecretMaterial{}, err
		}
	}
	usageMetadata, err := json.Marshal(map[string]any{
		"delivery":             "encrypted_material",
		"secret_id":            out.SecretID,
		"field_id":             out.FieldID,
		"value_version":        out.ValueVersion,
		"dek_generation":       out.DEK.Generation,
		"wrapping_key_id":      out.DEK.WrappingKeyID,
		"wrapping_key_version": out.DEK.WrappingKeyVersion,
		"secret_revision":      out.SecretRevision,
		"field_revision":       out.FieldRevision,
	})
	if err != nil {
		return SecretMaterial{}, err
	}
	if !replayed {
		if err := logEventTx(ctx, tx, EventInput{
			AccountID: p.AccountID, ActorKind: ActorAgent, ActorID: p.ID,
			Verb: VerbSecretMaterialDelivered,
			Metadata: map[string]any{
				"agent_id": p.ID, "secret_id": out.SecretID,
				"field_id":        out.FieldID,
				"value_version":   fmt.Sprint(out.ValueVersion),
				"key_version":     fmt.Sprint(out.DEK.WrappingKeyVersion),
				"encrypted_bytes": fmt.Sprint(len(out.Ciphertext) + len(out.DEK.WrappedDEK)),
			},
		}); err != nil {
			return SecretMaterial{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return SecretMaterial{}, err
	}
	// Delivery authorization, its exact retry fence, and audit record are
	// durable before metering. Usage is a best-effort projection: a rollup
	// failure cannot revoke already-authorized encrypted material.
	_ = s.recordUsage(ctx, usageEventInput{
		AccountID: p.AccountID, RealmID: p.RealmID, AgentID: p.ID,
		Dimension: UsageDimensionSecretRead, Quantity: 1, Unit: UsageUnitSecretAccess,
		SubjectType: "secret_field", SubjectID: out.FieldID,
		IdempotencyKey: "secret-access:" + p.ID + ":" + keyHash,
		Metadata:       usageMetadata,
	})
	return out, nil
}

func secretMaterialFingerprint(material SecretMaterial) string {
	h := sha256.New()
	_, _ = h.Write([]byte("witself/secret-material-delivery/v1\x00"))
	for _, value := range []string{
		material.SecretID, material.FieldID, material.Encoding, material.Algorithm,
		material.DEK.ID, material.DEK.WrapAlgorithm, material.DEK.WrappingKeyID,
	} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(value))
	}
	for _, value := range []int64{
		material.ValueVersion, material.EnvelopeVersion, material.AADVersion,
		material.DEK.Generation, material.DEK.AADVersion, material.DEK.WrapRevision,
		material.DEK.WrappingKeyVersion, material.SecretRevision, material.FieldRevision,
	} {
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], uint64(value))
		_, _ = h.Write(encoded[:])
	}
	for _, value := range [][]byte{material.Ciphertext, material.DEK.WrappedDEK} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = h.Write(length[:])
		_, _ = h.Write(value)
	}
	return hex.EncodeToString(h.Sum(nil))
}
