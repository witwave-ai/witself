package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// AccountEvacuationFinalization is the source cell's durable, value-free
// acknowledgement that one exact frozen tenant copy was removed after its
// target became authoritative.
type AccountEvacuationFinalization struct {
	AccountID        string
	EvacuationID     string
	SourceStatus     string
	FinalizedAt      time.Time
	Finalized        bool
	AlreadyFinalized bool
}

// ErrAccountEvacuationIntegrity means source finalization discovered a
// cross-account database relationship that a parent delete could otherwise
// cascade through. The entire serializable transaction rolls back.
var ErrAccountEvacuationIntegrity = errors.New(
	"account evacuation source has cross-account dependencies",
)

// FinalizeAccountEvacuationSource atomically removes one exact source copy and
// persists a cell-local retry receipt. The persisted source role is essential:
// source and target share an evacuation id, so exact id alone cannot prevent a
// delayed or misrouted request from deleting the imported target.
func (s *Store) FinalizeAccountEvacuationSource(
	ctx context.Context,
	accountID, evacuationID string,
) (AccountEvacuationFinalization, error) {
	if err := validateEvacuationID(evacuationID); err != nil {
		return AccountEvacuationFinalization{}, err
	}
	const maxSerializationAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxSerializationAttempts; attempt++ {
		result, err := s.finalizeAccountEvacuationSourceOnce(
			ctx, accountID, evacuationID,
		)
		if err == nil || !isEvacuationSerializationFailure(err) {
			return result, err
		}
		lastErr = err
	}
	return AccountEvacuationFinalization{}, lastErr
}

func (s *Store) finalizeAccountEvacuationSourceOnce(
	ctx context.Context,
	accountID, evacuationID string,
) (AccountEvacuationFinalization, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.Serializable,
	})
	if err != nil {
		return AccountEvacuationFinalization{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if receipt, found, err := readAccountEvacuationFinalizationTx(
		ctx, tx, accountID, evacuationID,
	); err != nil {
		return AccountEvacuationFinalization{}, err
	} else if found {
		receipt.AlreadyFinalized = true
		if err := tx.Commit(ctx); err != nil {
			return AccountEvacuationFinalization{}, err
		}
		return receipt, nil
	}

	var status string
	var isDefault bool
	var currentID, currentRole *string
	err = tx.QueryRow(ctx, `
		SELECT status, is_default, evacuation_id, evacuation_role
		  FROM accounts
		 WHERE id = $1
		 FOR UPDATE`,
		accountID,
	).Scan(&status, &isDefault, &currentID, &currentRole)
	if errors.Is(err, pgx.ErrNoRows) {
		// A concurrent exact finalizer may have committed while this
		// transaction waited for the account row. Recheck its durable receipt
		// before reporting a missing source.
		if receipt, found, receiptErr := readAccountEvacuationFinalizationTx(
			ctx, tx, accountID, evacuationID,
		); receiptErr != nil {
			return AccountEvacuationFinalization{}, receiptErr
		} else if found {
			receipt.AlreadyFinalized = true
			if err := tx.Commit(ctx); err != nil {
				return AccountEvacuationFinalization{}, err
			}
			return receipt, nil
		}
		return AccountEvacuationFinalization{}, ErrAccountNotFound
	}
	if err != nil {
		return AccountEvacuationFinalization{},
			fmt.Errorf("lock evacuation source finalization: %w", err)
	}
	if isDefault {
		return AccountEvacuationFinalization{}, ErrCannotCloseDefault
	}
	if currentID == nil || *currentID != evacuationID ||
		currentRole == nil || *currentRole != "source" {
		return AccountEvacuationFinalization{},
			ErrAccountEvacuationMismatch
	}
	if status != "suspended" && status != "closed" {
		return AccountEvacuationFinalization{}, ErrAccountNotExportable
	}
	if err := setEvacuationAuthorityTx(ctx, tx, evacuationID); err != nil {
		return AccountEvacuationFinalization{}, err
	}
	if err := rejectCrossAccountEvacuationCascadesTx(
		ctx, tx, accountID,
	); err != nil {
		return AccountEvacuationFinalization{}, err
	}
	// The archive registry is also the deletion dependency contract: export
	// order is parent-before-child, so reverse order removes every portable
	// child before its parent. accounts is retained until the end so every
	// trigger can authenticate this exact source marker. The two indirectly
	// scoped tables use their canonical account joins.
	for i := len(canonicalArchiveTables) - 1; i >= 0; i-- {
		table := canonicalArchiveTables[i].name
		var query string
		switch table {
		case "accounts":
			continue
		case "agents":
			query = `
				DELETE FROM agents
				 WHERE realm_id IN (
				       SELECT id FROM realms WHERE account_id = $1
				 )`
		case "agent_activity":
			query = `
				DELETE FROM agent_activity
				 WHERE agent_id IN (
				       SELECT agent.id
				         FROM agents agent
				         JOIN realms realm ON realm.id = agent.realm_id
				        WHERE realm.account_id = $1
				 )`
		default:
			query = fmt.Sprintf(
				"DELETE FROM %s WHERE account_id = $1",
				pgx.Identifier{table}.Sanitize(),
			)
		}
		if _, err := tx.Exec(ctx, query, accountID); err != nil {
			return AccountEvacuationFinalization{},
				fmt.Errorf("purge evacuated source table %s: %w", table, err)
		}
	}

	var finalizedAt time.Time
	if err := tx.QueryRow(ctx, `
		INSERT INTO account_evacuation_finalizations(
			account_id, evacuation_id, source_status, finalized_at
		) VALUES ($1, $2, $3, clock_timestamp())
		RETURNING finalized_at`,
		accountID, evacuationID, status,
	).Scan(&finalizedAt); err != nil {
		return AccountEvacuationFinalization{},
			fmt.Errorf("record source finalization receipt: %w", err)
	}
	tag, err := tx.Exec(ctx,
		`DELETE FROM accounts
		  WHERE id = $1
		    AND evacuation_id = $2
		    AND evacuation_role = 'source'`,
		accountID, evacuationID,
	)
	if err != nil {
		return AccountEvacuationFinalization{},
			fmt.Errorf("delete evacuated source account: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return AccountEvacuationFinalization{},
			ErrAccountEvacuationMismatch
	}
	if err := tx.Commit(ctx); err != nil {
		return AccountEvacuationFinalization{}, err
	}
	return AccountEvacuationFinalization{
		AccountID: accountID, EvacuationID: evacuationID,
		SourceStatus: status, FinalizedAt: finalizedAt.UTC(),
		Finalized: true,
	}, nil
}

func isEvacuationSerializationFailure(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) &&
		postgresError.Code == "40001"
}

// rejectCrossAccountEvacuationCascadesTx defends the source purge against
// latent/manual corruption. Several legacy foreign keys predate composite
// tenant scoping; supported writes enforce the same-account relationship in
// store code, but a corrupt child row could otherwise be cascade-deleted when
// its parent belongs to this source. Discover every mutating FK dynamically so
// future portable tables inherit the same fail-closed check.
func rejectCrossAccountEvacuationCascadesTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
) error {
	portable := make(map[string]bool, len(canonicalArchiveTables))
	for _, table := range canonicalArchiveTables {
		portable[table.name] = true
	}

	rows, err := tx.Query(ctx, `
		SELECT constraint_record.conname,
		       child.relname,
		       parent.relname,
		       array_agg(child_attribute.attname ORDER BY key_column.ordinality),
		       array_agg(parent_attribute.attname ORDER BY key_column.ordinality)
		  FROM pg_constraint constraint_record
		  JOIN pg_class child
		    ON child.oid = constraint_record.conrelid
		  JOIN pg_namespace child_namespace
		    ON child_namespace.oid = child.relnamespace
		  JOIN pg_class parent
		    ON parent.oid = constraint_record.confrelid
		  JOIN pg_namespace parent_namespace
		    ON parent_namespace.oid = parent.relnamespace
		  JOIN LATERAL unnest(
		       constraint_record.conkey,
		       constraint_record.confkey
		  ) WITH ORDINALITY AS key_column(
		       child_attribute_number,
		       parent_attribute_number,
		       ordinality
		  ) ON true
		  JOIN pg_attribute child_attribute
		    ON child_attribute.attrelid = constraint_record.conrelid
		   AND child_attribute.attnum = key_column.child_attribute_number
		  JOIN pg_attribute parent_attribute
		    ON parent_attribute.attrelid = constraint_record.confrelid
		   AND parent_attribute.attnum = key_column.parent_attribute_number
		 WHERE constraint_record.contype = 'f'
		   AND constraint_record.confdeltype IN ('c', 'n', 'd')
		   AND child_namespace.nspname = 'public'
		   AND parent_namespace.nspname = 'public'
		 GROUP BY constraint_record.oid,
		          constraint_record.conname,
		          child.relname,
		          parent.relname
		 ORDER BY child.relname, parent.relname, constraint_record.conname`)
	if err != nil {
		return fmt.Errorf("inspect source purge foreign keys: %w", err)
	}

	type evacuationForeignKey struct {
		name, childTable, parentTable string
		childColumns, parentColumns   []string
	}
	var foreignKeys []evacuationForeignKey
	for rows.Next() {
		var foreignKey evacuationForeignKey
		if err := rows.Scan(
			&foreignKey.name,
			&foreignKey.childTable,
			&foreignKey.parentTable,
			&foreignKey.childColumns,
			&foreignKey.parentColumns,
		); err != nil {
			rows.Close()
			return fmt.Errorf("read source purge foreign key: %w", err)
		}
		foreignKeys = append(foreignKeys, foreignKey)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("scan source purge foreign keys: %w", err)
	}
	rows.Close()

	for _, foreignKey := range foreignKeys {
		if !portable[foreignKey.childTable] ||
			!portable[foreignKey.parentTable] {
			continue
		}
		childOwner, ok := portableAccountOwnerExpression(
			foreignKey.childTable, "evacuation_child",
		)
		if !ok {
			continue
		}
		parentOwner, ok := portableAccountOwnerExpression(
			foreignKey.parentTable, "evacuation_parent",
		)
		if !ok {
			continue
		}
		if len(foreignKey.childColumns) == 0 ||
			len(foreignKey.childColumns) !=
				len(foreignKey.parentColumns) {
			return fmt.Errorf(
				"%w: foreign key %s has an invalid column mapping",
				ErrAccountEvacuationIntegrity, foreignKey.name,
			)
		}
		joinParts := make([]string, len(foreignKey.childColumns))
		for index := range foreignKey.childColumns {
			joinParts[index] = fmt.Sprintf(
				"%s = %s",
				pgx.Identifier{
					"evacuation_child",
					foreignKey.childColumns[index],
				}.Sanitize(),
				pgx.Identifier{
					"evacuation_parent",
					foreignKey.parentColumns[index],
				}.Sanitize(),
			)
		}
		query := fmt.Sprintf(`
			SELECT EXISTS (
				SELECT 1
				  FROM %s AS %s
				  JOIN %s AS %s
				    ON %s
				 WHERE (%s) = $1
				   AND (%s) IS DISTINCT FROM $1
			)`,
			pgx.Identifier{foreignKey.childTable}.Sanitize(),
			pgx.Identifier{"evacuation_child"}.Sanitize(),
			pgx.Identifier{foreignKey.parentTable}.Sanitize(),
			pgx.Identifier{"evacuation_parent"}.Sanitize(),
			strings.Join(joinParts, " AND "),
			parentOwner,
			childOwner,
		)
		var crossesAccount bool
		if err := tx.QueryRow(ctx, query, accountID).Scan(
			&crossesAccount,
		); err != nil {
			return fmt.Errorf(
				"check source purge foreign key %s: %w",
				foreignKey.name, err,
			)
		}
		if crossesAccount {
			return fmt.Errorf(
				"%w: foreign key %s crosses the source boundary",
				ErrAccountEvacuationIntegrity, foreignKey.name,
			)
		}
	}
	return nil
}

func portableAccountOwnerExpression(
	table, alias string,
) (string, bool) {
	qualified := func(column string) string {
		return pgx.Identifier{alias, column}.Sanitize()
	}
	switch table {
	case "accounts":
		return qualified("id"), true
	case "agents":
		return fmt.Sprintf(`(
			SELECT owner_realm.account_id
			  FROM realms AS owner_realm
			 WHERE owner_realm.id = %s
		)`, qualified("realm_id")), true
	case "agent_activity":
		return fmt.Sprintf(`(
			SELECT owner_realm.account_id
			  FROM agents AS owner_agent
			  JOIN realms AS owner_realm
			    ON owner_realm.id = owner_agent.realm_id
			 WHERE owner_agent.id = %s
		)`, qualified("agent_id")), true
	default:
		return qualified("account_id"), true
	}
}

func readAccountEvacuationFinalizationTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID, evacuationID string,
) (AccountEvacuationFinalization, bool, error) {
	var status string
	var finalizedAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT source_status, finalized_at
		  FROM account_evacuation_finalizations
		 WHERE account_id = $1
		   AND evacuation_id = $2`,
		accountID, evacuationID,
	).Scan(&status, &finalizedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountEvacuationFinalization{}, false, nil
	}
	if err != nil {
		return AccountEvacuationFinalization{}, false,
			fmt.Errorf("read source finalization receipt: %w", err)
	}
	return AccountEvacuationFinalization{
		AccountID: accountID, EvacuationID: evacuationID,
		SourceStatus: status, FinalizedAt: finalizedAt.UTC(),
		Finalized: true,
	}, true, nil
}
