package store

import (
	"fmt"
	"slices"
	"strings"
)

// syncArchiveDisposition names the five distinct movement decisions. An inert
// history copy carries evidence, never a live writer capability or authority.
type syncArchiveDisposition string

const (
	syncArchivePreserveInert syncArchiveDisposition = "preserve_historical_inert"
	syncArchiveRebuildLocal  syncArchiveDisposition = "exclude_rebuild_local"
	syncArchiveRefuseFloors  syncArchiveDisposition = "refuse_pending_retained_floors"
)

type syncArchiveMovementPolicy struct {
	snapshotProjection syncArchiveDisposition
	provenMove         syncArchiveDisposition
	backupRestore      syncArchiveDisposition
	sourceFinalization syncArchiveDisposition
	accountPurge       syncArchiveDisposition
	evacuationGuard    bool
}

func portableSyncMovementPolicy() syncArchiveMovementPolicy {
	return syncArchiveMovementPolicy{
		snapshotProjection: syncArchivePreserveInert,
		provenMove:         syncArchivePreserveInert,
		backupRestore:      syncArchivePreserveInert,
		sourceFinalization: syncArchiveRefuseFloors,
		accountPurge:       syncArchiveRefuseFloors,
		evacuationGuard:    true,
	}
}

func excludedSyncMovementPolicy() syncArchiveMovementPolicy {
	return syncArchiveMovementPolicy{
		snapshotProjection: syncArchiveRebuildLocal,
		provenMove:         syncArchiveRebuildLocal,
		backupRestore:      syncArchiveRebuildLocal,
		sourceFinalization: syncArchiveRefuseFloors,
		accountPurge:       syncArchiveRefuseFloors,
		evacuationGuard:    true,
	}
}

// syncArchiveTable is the independent sync archive contract. This list is in
// foreign-key insertion order; its columns are the export and import allowlist,
// and orderBy is the full primary key, making empty and populated streams
// deterministic. It must never be appended to canonicalArchiveTables: the core
// format and blanket portable deletion helper do not own sync evidence.
type syncArchiveTable struct {
	name                 string
	introducedSyncSchema int
	columns              []string
	orderBy              []string
	movement             syncArchiveMovementPolicy
}

func newSyncArchiveTable(name, columns, orderBy string) syncArchiveTable {
	return syncArchiveTable{
		name:                 name,
		introducedSyncSchema: 1,
		columns:              strings.Fields(columns),
		orderBy:              strings.Fields(orderBy),
		movement:             portableSyncMovementPolicy(),
	}
}

var syncArchiveTables = []syncArchiveTable{
	newSyncArchiveTable("sync_incarnations",
		"account_id lifecycle_epoch restore_marker predecessor_epoch predecessor_marker allocation_id transition_id authority_installation state publication_proof_ref",
		"account_id lifecycle_epoch restore_marker"),
	newSyncArchiveTable("sync_operations",
		"account_id operation_id original_lifecycle_epoch original_restore_marker original_capture_epoch original_installation original_replica original_write_lineage original_sequence operation_kind envelope_version schema_version protocol_version comparison_key_id input_commitment admission_state receipt_ref",
		"account_id operation_id"),
	newSyncArchiveTable("sync_operation_payloads",
		"account_id operation_id canonical_semantic_envelope governed_retirement",
		"account_id operation_id"),
	newSyncArchiveTable("sync_verifier_history",
		"account_id issuer_id authority_stream signing_key_id public_key valid_from retired_at sealed_prefix_proof",
		"account_id issuer_id authority_stream signing_key_id"),
	newSyncArchiveTable("sync_receipts",
		"account_id operation_id original_lifecycle_epoch original_restore_marker original_capture_epoch original_installation original_replica original_write_lineage original_sequence issuer_id issuer_lifecycle_epoch issuer_restore_marker authority_stream issuer_authority_epoch acceptance_ordinal comparison_key_id input_commitment result_commitment status reason causal_result signing_key_id canonical_body signed_bytes settled_at retired_at retain_until",
		"account_id operation_id"),
	newSyncArchiveTable("sync_reuse_refusals",
		"account_id operation_id comparison_key_id conflicting_commitment canonical_body signed_bytes retain_until",
		"account_id operation_id comparison_key_id conflicting_commitment"),
	newSyncArchiveTable("sync_finalized_counter",
		"account_id capture_epoch last_finalized",
		"account_id capture_epoch"),
	newSyncArchiveTable("sync_bundles",
		"account_id bundle_id tx_id capture_epoch lifecycle_epoch restore_marker finalized sealed origin_class semantic_effects dependencies",
		"account_id bundle_id"),
	newSyncArchiveTable("sync_bundle_operations",
		"account_id bundle_id member_order operation_id atomic_group",
		"account_id bundle_id member_order"),
	newSyncArchiveTable("sync_publication_outbox",
		"account_id issuer_stream through_ordinal prefix_commitment prefix_members operation_id proof_kind state witness_proof_ref",
		"account_id issuer_stream through_ordinal"),
	newSyncArchiveTable("sync_acceptance_counter",
		"account_id issuer_stream last_accepted",
		"account_id issuer_stream"),
	newSyncArchiveTable("sync_resource_budgets",
		"account_id max_pending_operations max_retained_bytes pending_operations retained_bytes",
		"account_id"),
	newSyncArchiveTable("sync_deletion_knowledge",
		"account_id family object_id generation deletion_generation accepted_delete_id issuer_stream frontier evidence_ref deleted_at retain_until",
		"account_id family object_id generation deletion_generation"),
	newSyncArchiveTable("sync_retained_evidence",
		"account_id evidence_kind evidence_id retain_until",
		"account_id evidence_kind evidence_id"),
	newSyncArchiveTable("sync_evidence_pins",
		"account_id evidence_kind evidence_id pin_owner reason released_at",
		"account_id evidence_kind evidence_id pin_owner"),
	newSyncArchiveTable("sync_payload_retirements",
		"account_id operation_id retired_at",
		"account_id operation_id"),
}

type syncArchiveExclusion struct {
	introducedSyncSchema int
	reason               string
	movement             syncArchiveMovementPolicy
}

func newSyncArchiveExclusion(reason string) syncArchiveExclusion {
	return syncArchiveExclusion{introducedSyncSchema: 1, reason: reason, movement: excludedSyncMovementPolicy()}
}

// These are current local control/scratch state, not portable historical
// evidence. Historical epochs, bundles, outcomes, floors and pending publication
// evidence remain portable. A destination must establish a fresh capture barrier
// and reconcile resource reservations before later capture activation.
var syncLocalArchiveExclusions = map[string]syncArchiveExclusion{
	"sync_capture": newSyncArchiveExclusion(
		"current local capture barrier; destination must establish fresh coverage and readiness"),
	"sync_writer_capabilities": newSyncArchiveExclusion(
		"database-principal capability registrations cannot authorize destination writers"),
	"sync_tx_admissions": newSyncArchiveExclusion(
		"physical transaction admissions cannot survive export or authorize a new session"),
	"sync_tx_touches": newSyncArchiveExclusion(
		"transaction-local unsealed touch scratch; sealed semantic history lives in portable bundles"),
	"sync_resource_reservations": newSyncArchiveExclusion(
		"local capacity reservations must be reconciled from retained operations and payloads before activation"),
}

func syncArchiveTableNamesForSchema(schema int) []string {
	names := make([]string, 0, len(syncArchiveTables))
	for _, table := range syncArchiveTables {
		if table.introducedSyncSchema <= schema {
			names = append(names, table.name)
		}
	}
	return names
}

func syncArchiveTableByName(name string) (syncArchiveTable, bool) {
	for _, table := range syncArchiveTables {
		if table.name == name {
			return table, true
		}
	}
	return syncArchiveTable{}, false
}

// exportQuery contains only compile-time registry identifiers. Explicit JSON
// keys prevent future DDL columns from silently changing the archive contract.
func (table syncArchiveTable) exportQuery() string {
	fields := make([]string, 0, 2*len(table.columns))
	for _, column := range table.columns {
		fields = append(fields, "'"+column+"'", column)
	}
	return "SELECT jsonb_build_object(" + strings.Join(fields, ", ") + ") FROM " + table.name +
		" WHERE account_id = $1 ORDER BY " + strings.Join(table.orderBy, ", ")
}

// Membership is exact and independent of row count; omitted empty evidence
// streams are as invalid as missing populated streams. Errors contain no
// archive-supplied names or values.
func validateSyncArchiveManifestTables(schema int, tables []string) error {
	if schema <= 0 || schema > SyncSchemaVersion() {
		return fmt.Errorf("%w: unsupported sync schema extension", ErrArchiveContent)
	}
	expected := make(map[string]bool)
	for _, name := range syncArchiveTableNamesForSchema(schema) {
		expected[name] = true
	}
	for _, table := range tables {
		if !expected[table] {
			return fmt.Errorf("%w: invalid sync extension streams", ErrArchiveContent)
		}
		delete(expected, table)
	}
	if len(expected) != 0 {
		return fmt.Errorf("%w: incomplete sync extension streams", ErrArchiveContent)
	}
	if !slices.Equal(tables, syncArchiveTableNamesForSchema(schema)) {
		return fmt.Errorf("%w: invalid sync extension stream order", ErrArchiveContent)
	}
	return nil
}
