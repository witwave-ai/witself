package activity

// Schema identifies the activity wire format.
const Schema = "witself.agent-activity.v1"

// CatalogVersion identifies the closed set of recorded core operations.
const CatalogVersion = "core-records.v1"

// Descriptor contains only fixed category/action and read/write classification.
type Descriptor struct {
	Category, Action string
	Write            bool
}

// Catalog is a closed, value-free vocabulary. Return a copy so callers cannot
// change the process-wide catalog.
func Catalog() map[string]Descriptor {
	out := make(map[string]Descriptor)
	for category, actions := range map[string][]string{
		"facts":       {"exact", "list", "history", "upcoming"},
		"memories":    {"get", "list", "recall", "history"},
		"transcripts": {"list", "page"},
		"messages":    {"list", "read", "peek"},
		"email":       {"list", "read", "outbox_list", "outbox_detail"},
		"secrets":     {"inventory", "show", "access"},
	} {
		for _, action := range actions {
			out[category+"."+action] = Descriptor{Category: category, Action: action}
		}
	}
	for category, actions := range map[string][]string{
		"facts":       {"set", "propose", "confirm", "reject", "delete"},
		"memories":    {"capture", "adjust", "supersede", "forget", "restore", "reactivate", "delete", "curation_apply"},
		"transcripts": {"create", "append"},
		"messages":    {"send", "reply"},
		"email":       {"send", "reply"},
		"secrets":     {"create", "archive", "restore", "delete"},
	} {
		for _, action := range actions {
			out[category+"."+action] = Descriptor{Category: category, Action: action, Write: true}
		}
	}
	return out
}

var catalog = Catalog()

// Lookup resolves a fixed catalog operation.
func Lookup(operation string) (Descriptor, bool) { d, ok := catalog[operation]; return d, ok }

// ExcludedCatalog documents deliberate exclusions, including nested internal
// work; these are not estimates of successfully tracked requests.
func ExcludedCatalog() []string {
	return []string{
		"legacy_reads_without_activity_id", "passive_observation", "self_hydration", "activity", "usage", "status", "counts",
		"mailbox_listen", "claims", "acknowledgments", "checkpoints", "leases", "provider_callbacks", "internal_fanout",
		"fact_candidates_read", "fact_subjects", "memory_evidence", "memory_vectors", "curation_bookkeeping", "curation_zero_action_apply", "curation_rollback_operation",
		"account_administration", "archive_import", "retention", "avatar", "message_requests", "vault_key_registration",
	}
}

// Unit returns the only allowed unit for an activity dimension.
func Unit(d string) string {
	switch d {
	case "operation_read", "operation_write":
		return "operation"
	case "operation_read_record", "operation_write_record":
		return "record"
	case "memory_created", "memory_revised", "memory_archived", "memory_restored", "memory_deleted":
		return "change"
	case "activity_tracking_started":
		return "activation"
	}
	return ""
}

// Dimensions returns the rollup dimensions, excluding the activation marker.
func Dimensions() []string {
	return []string{"operation_read", "operation_write", "operation_read_record", "operation_write_record", "memory_created", "memory_revised", "memory_archived", "memory_restored", "memory_deleted"}
}
