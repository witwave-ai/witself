package store

import (
	"fmt"
	"sort"
	"strings"
)

// archiveTableIntroduction is the manifest-level portability contract. The
// schema coordinate is the migration that first created the table, so an old
// archive is required to carry exactly the streams that existed when it was
// written while a current archive cannot silently omit a newer stream merely
// because that stream happened to be empty.
//
// Keep this list in ExportAccount dependency order. Contract tests pin every
// entry to the embedded CREATE TABLE migration, the exporter queries, and the
// import allowlist.
type archiveTableIntroduction struct {
	name             string
	introducedSchema int
}

// avatarStyleRolloutArchiveSchema is the first archive schema that carries
// lazy profile style revisions and their resumable rollout job.
const avatarStyleRolloutArchiveSchema = 52

var canonicalArchiveTables = []archiveTableIntroduction{
	{name: "accounts", introducedSchema: 1},
	{name: "operators", introducedSchema: 2},
	{name: "realms", introducedSchema: 4},
	{name: "avatar_style_packs", introducedSchema: 50},
	{name: "avatar_style_pack_versions", introducedSchema: 50},
	{name: "realm_avatar_styles", introducedSchema: 50},
	{name: "avatar_style_rollout_jobs", introducedSchema: avatarStyleRolloutArchiveSchema},
	{name: "agents", introducedSchema: 5},
	{name: "agent_vault_keys", introducedSchema: 55},
	{name: "agent_vault_key_enrollments", introducedSchema: 56},
	{name: "vault_key_enrollment_receipts", introducedSchema: 56},
	{name: "secrets", introducedSchema: 55},
	{name: "secret_fields", introducedSchema: 55},
	{name: "secret_deks", introducedSchema: 55},
	{name: "agent_vault_key_rotations", introducedSchema: 56},
	{name: "agent_vault_key_rotation_items", introducedSchema: 56},
	{name: "vault_key_rotation_receipts", introducedSchema: 56},
	{name: "secret_mutation_receipts", introducedSchema: 55},
	{name: "agent_avatar_profiles", introducedSchema: 50},
	{name: "agent_avatar_versions", introducedSchema: 50},
	{name: "agent_avatar_activations", introducedSchema: 50},
	{name: "agent_avatar_rejections", introducedSchema: 50},
	{name: "agent_avatar_resets", introducedSchema: 50},
	{name: "avatar_mutation_receipts", introducedSchema: 50},
	{name: "agent_activity", introducedSchema: 39},
	{name: "agent_dashboard_preferences", introducedSchema: 57},
	{name: "fact_subjects", introducedSchema: 22},
	{name: "facts", introducedSchema: 22},
	{name: "fact_mutation_tombstones", introducedSchema: 27},
	{name: "fact_assertions", introducedSchema: 22},
	{name: "fact_candidates", introducedSchema: 23},
	{name: "tokens", introducedSchema: 3},
	{name: "transcript_conversations", introducedSchema: 19},
	{name: "transcript_entries", introducedSchema: 19},
	{name: "usage_events", introducedSchema: 21},
	{name: "usage_rollups", introducedSchema: 21},
	{name: "agent_messages", introducedSchema: 20},
	{name: "agent_message_deliveries", introducedSchema: 20},
	{name: "agent_message_requests", introducedSchema: 38},
	{name: "agent_message_request_candidates", introducedSchema: 38},
	{name: "agent_message_request_selections", introducedSchema: 38},
	{name: "agent_message_request_claims", introducedSchema: 38},
	{name: "memory_change_clocks", introducedSchema: 29},
	{name: "memories", introducedSchema: 29},
	{name: "memory_versions", introducedSchema: 29},
	{name: "memory_vector_profiles", introducedSchema: 32},
	{name: "memory_vectors", introducedSchema: 32},
	{name: "memory_evidence", introducedSchema: 29},
	{name: "memory_relations", introducedSchema: 29},
	{name: "memory_deleted_references", introducedSchema: 29},
	{name: "memory_curation_lanes", introducedSchema: 30},
	{name: "memory_curation_cursors", introducedSchema: 30},
	{name: "memory_curation_requests", introducedSchema: 30},
	{name: "memory_curation_runs", introducedSchema: 30},
	{name: "memory_curation_run_inputs", introducedSchema: 30},
	{name: "memory_curation_actions", introducedSchema: 30},
	{name: "memory_curation_mutations", introducedSchema: 30},
	{name: "account_events", introducedSchema: 14},
	{name: "support_tickets", introducedSchema: 15},
	{name: "support_ticket_messages", introducedSchema: 15},
}

func canonicalArchiveTableNamesForSchema(schema int) []string {
	names := make([]string, 0, len(canonicalArchiveTables))
	for _, table := range canonicalArchiveTables {
		if table.introducedSchema <= schema {
			names = append(names, table.name)
		}
	}
	return names
}

// validateArchiveManifestTables refuses a checksummed archive whose manifest
// presents only a subset (or a superset) of the canonical streams for its
// claimed schema. Row checksums cannot prove the presence of an omitted empty
// table, so this validation must happen against the manifest itself.
//
// Membership is exact but order is deliberately not: legitimate old writers
// used the dependency order current at their release, which need not equal the
// order obtained by filtering today's exporter list.
func validateArchiveManifestTables(schema int, tables []string) error {
	expected := make(map[string]bool)
	for _, name := range canonicalArchiveTableNamesForSchema(schema) {
		expected[name] = true
	}

	seen := make(map[string]bool, len(tables))
	unexpected := make([]string, 0)
	for _, table := range tables {
		if seen[table] {
			return fmt.Errorf(
				"%w: manifest lists table %q more than once",
				ErrArchiveContent, table,
			)
		}
		seen[table] = true
		if !expected[table] {
			unexpected = append(unexpected, table)
		}
	}

	missing := make([]string, 0)
	for table := range expected {
		if !seen[table] {
			missing = append(missing, table)
		}
	}
	if len(missing) == 0 && len(unexpected) == 0 {
		return nil
	}

	sort.Strings(missing)
	sort.Strings(unexpected)
	parts := make([]string, 0, 2)
	if len(missing) > 0 {
		parts = append(parts, "missing "+strings.Join(missing, ", "))
	}
	if len(unexpected) > 0 {
		parts = append(parts, "unexpected "+strings.Join(unexpected, ", "))
	}
	return fmt.Errorf(
		"%w: manifest tables do not match schema %d (%s)",
		ErrArchiveContent, schema, strings.Join(parts, "; "),
	)
}
