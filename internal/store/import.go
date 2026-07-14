package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/placement"
)

// ErrAccountExists is returned when an import targets an account id already
// present on this cell — restore never overwrites.
var ErrAccountExists = errors.New("account already exists on this cell")

// ErrArchiveAccountMismatch is returned when the archive's manifest names a
// different account than the caller asked to import.
var ErrArchiveAccountMismatch = errors.New("archive is for a different account")

// ErrArchiveContent is returned for archives that are structurally well-formed
// (gzip/tar/checksums all check out) but whose row payload violates the
// import contract: a row scoped to a different account, an off-list column,
// an agent pointing at a realm that never arrived, extra accounts rows.
// The condition is permanent for a given archive object, so it maps to a
// 400 (server.ErrBadArchive) — the caller should quarantine the archive,
// not retry it.
var ErrArchiveContent = errors.New("archive content is not importable")

// importColumns is the strict per-table allowlist of column names an archive
// may carry. It doubles as the SQL-identifier boundary — the row's JSON keys
// are looked up here and only allowlisted names are interpolated into the
// INSERT — and as the additive-migration safety net: unlisted (new) columns
// are refused rather than smuggled in with attacker-chosen values.
var importColumns = map[string]map[string]bool{
	"accounts": {
		"id": true, "is_default": true, "display_name": true, "email": true,
		"status": true, "created_at": true, "closed_at": true, "closed_reason": true,
		"suspended_at": true, "suspended_for": true, "suspended_reason": true,
		"support_policy": true,
		"plan":           true, "plan_limits": true, "plan_features": true,
		"placement_policy": true,
	},
	"operators": {
		"id": true, "account_id": true, "role": true, "is_root": true,
		"display_name": true, "created_at": true, "updated_at": true, "deleted_at": true,
	},
	"realms": {
		"id": true, "account_id": true, "name": true,
		"created_at": true, "updated_at": true, "deleted_at": true,
	},
	"agents": {
		"id": true, "realm_id": true, "name": true,
		"created_at": true, "updated_at": true, "deleted_at": true,
	},
	"tokens": {
		"id": true, "account_id": true, "operator_id": true, "agent_id": true,
		"kind": true, "token_hash": true, "display_name": true,
		"created_at": true, "expires_at": true, "consumed_at": true,
	},
	"account_events": {
		"id": true, "account_id": true, "occurred_at": true,
		"actor_kind": true, "actor_id": true,
		"verb": true, "metadata": true, "retain_until": true,
	},
	"support_tickets": {
		"id": true, "account_id": true, "opened_at": true,
		"opened_by_kind": true, "opened_by_id": true,
		"subject": true, "category": true, "state": true,
		"priority": true, "first_response_at": true, "resolved_at": true,
		"closed_at": true, "last_activity_at": true, "last_message_id": true,
		"correlation": true, "metadata": true, "retain_until": true,
	},
	"support_ticket_messages": {
		"id": true, "ticket_id": true, "account_id": true, "posted_at": true,
		"author_kind": true, "author_id": true,
		"body": true, "attachments": true, "metadata": true,
	},
	"transcript_conversations": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_agent_id": true, "external_id": true, "title": true,
		"metadata": true, "next_sequence": true,
		"created_at": true, "updated_at": true,
	},
	"transcript_entries": {
		"id": true, "account_id": true, "transcript_id": true,
		"realm_id": true, "recorded_by_agent_id": true,
		"sequence": true, "external_id": true, "role": true, "body": true,
		"payload": true, "model": true, "reply_to_entry_id": true,
		"artifacts": true, "created_at": true,
	},
	"usage_events": {
		"id": true, "account_id": true, "realm_id": true, "agent_id": true,
		"dimension": true, "quantity": true, "unit": true,
		"subject_type": true, "subject_id": true, "idempotency_key": true,
		"metadata": true, "occurred_at": true, "created_at": true,
	},
	"usage_rollups": {
		"account_id": true, "realm_id": true, "agent_id": true,
		"dimension": true, "unit": true, "bucket": true,
		"bucket_start": true, "quantity": true, "event_count": true,
		"updated_at": true,
	},
	"fact_subjects": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_agent_id": true, "canonical_key": true,
		"display_name": true, "aliases": true,
		"created_at": true, "updated_at": true,
	},
	"facts": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_agent_id": true, "subject_id": true, "predicate": true,
		"cardinality": true, "sensitive": true, "resolved_assertion_id": true,
		"deleted_at": true, "deleted_by_agent_id": true,
		"delete_receipt_id": true, "delete_idempotency_key_hash": true,
		"deleted_prior_assertion_id": true,
		"deleted_assertion_count":    true, "deleted_candidate_count": true,
		"deleted_usage_count": true, "deleted_mutation_key_count": true,
		"deleted_candidate_revision": true,
		"recreated_at":               true, "replacement_fact_id": true,
		"created_at": true, "updated_at": true,
	},
	"fact_mutation_tombstones": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_agent_id": true, "fact_id": true, "surface": true,
		"idempotency_key_hash": true, "deleted_at": true,
	},
	"fact_assertions": {
		"id": true, "fact_id": true, "account_id": true, "realm_id": true,
		"asserted_by_agent_id": true, "value_type": true, "value": true,
		"recurrence":  true,
		"source_kind": true, "source_ref": true, "confidence": true,
		"observed_at": true, "confirmed_at": true, "valid_from": true,
		"valid_until": true, "supersedes_id": true,
		"idempotency_key": true, "idempotency_fingerprint": true,
		"created_at": true,
	},
	"fact_candidates": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_agent_id": true, "subject_key": true, "predicate": true,
		"value_type": true, "value": true, "cardinality": true,
		"recurrence": true,
		"sensitive":  true, "source_ref": true, "confidence": true,
		"observed_at": true, "valid_from": true, "valid_until": true,
		"reason": true, "status": true, "conflict_fact_id": true,
		"observed_assertion_id": true,
		"resolved_fact_id":      true, "decision_assertion_id": true,
		"idempotency_key":         true,
		"idempotency_fingerprint": true, "decision_idempotency_key": true,
		"proposed_at": true, "decided_at": true,
	},
	"agent_messages": {
		"id": true, "account_id": true, "realm_id": true,
		"from_agent_id": true, "to_agent_id": true,
		"subject": true, "kind": true, "body": true, "payload": true,
		"thread_id": true, "idempotency_key": true, "created_at": true,
	},
	"agent_message_deliveries": {
		"message_id": true, "account_id": true, "realm_id": true,
		"recipient_agent_id": true, "state": true, "delivered_at": true,
		"read_at": true, "acked_at": true, "created_at": true,
	},
}

type transcriptImportScope struct {
	realmID      string
	ownerAgentID string
}

type messageImportScope struct {
	realmID     string
	fromAgentID string
	toAgentID   string
}

type factImportScope struct {
	realmID                 string
	ownerAgentID            string
	resolvedAssertionID     string
	subjectID               string
	subjectKey              string
	predicate               string
	sensitive               bool
	deleted                 bool
	replacementFactID       string
	deletedMutationKeyCount int64
}

// importCtx accumulates per-import state: how many accounts rows we have seen
// (must be exactly one), and the set of ids inserted for each table. The FK
// targets an incoming row references (agents.realm_id, tokens.operator_id,
// tokens.agent_id) must have already been inserted by THIS import — the FK
// constraint alone would accept a target belonging to any tenant on the cell.
type importCtx struct {
	accountID                   string
	accounts                    int
	operators                   map[string]bool
	realms                      map[string]bool
	agents                      map[string]bool
	agentRealms                 map[string]string
	tickets                     map[string]bool
	transcripts                 map[string]transcriptImportScope
	entries                     map[string]string
	factSubjects                map[string]factImportScope
	factSubjectNames            map[string]map[string]string
	facts                       map[string]factImportScope
	assertions                  map[string]string
	factMutationTombstoneCounts map[string]int64
	messages                    map[string]messageImportScope
	deliveries                  map[string]bool
}

func newImportCtx(accountID string) *importCtx {
	return &importCtx{
		accountID:                   accountID,
		operators:                   map[string]bool{},
		realms:                      map[string]bool{},
		agents:                      map[string]bool{},
		agentRealms:                 map[string]string{},
		tickets:                     map[string]bool{},
		transcripts:                 map[string]transcriptImportScope{},
		entries:                     map[string]string{},
		factSubjects:                map[string]factImportScope{},
		factSubjectNames:            map[string]map[string]string{},
		facts:                       map[string]factImportScope{},
		assertions:                  map[string]string{},
		factMutationTombstoneCounts: map[string]int64{},
		messages:                    map[string]messageImportScope{},
		deliveries:                  map[string]bool{},
	}
}

// validateAndRecord is the row-content boundary: it enforces the account
// scoping the FKs alone cannot, and records the id (if any) for later FK
// targets. Called BEFORE the INSERT so a bad row aborts the transaction
// without touching the database.
func (ic *importCtx) validateAndRecord(table string, obj map[string]any) error {
	badf := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrArchiveContent, fmt.Sprintf(format, args...))
	}
	// Tables that carry account_id must have it equal the manifest's.
	// Agents scope transitively: their realm_id lands in ic.realms only
	// for realms this archive itself just wrote, so the realm_id check
	// below is the FK-safety boundary for that table.
	switch table {
	case "operators", "realms", "tokens", "account_events",
		"support_tickets", "support_ticket_messages",
		"transcript_conversations", "transcript_entries",
		"usage_events", "usage_rollups",
		"fact_subjects", "facts", "fact_mutation_tombstones", "fact_assertions", "fact_candidates",
		"agent_messages", "agent_message_deliveries":
		id, err := requireStringField(obj, "account_id")
		if err != nil {
			return badf("%s row missing account_id", table)
		}
		if id != ic.accountID {
			return badf("%s row account_id %q does not match manifest %q", table, id, ic.accountID)
		}
	}
	switch table {
	case "accounts":
		id, err := requireStringField(obj, "id")
		if err != nil || id != ic.accountID {
			return badf("accounts row id %q does not match manifest %q", id, ic.accountID)
		}
		if v, ok := obj["is_default"]; ok {
			if b, _ := v.(bool); b {
				return badf("accounts row claims is_default=true")
			}
		}
		// Plan-snapshot shape checks: these jsonb columns are decoded into
		// typed Go values on every read (map[string]int64 / []string), so a
		// malformed value would import fine and then poison the account —
		// every GetAccount and every gated create fails until the control
		// plane re-applies a snapshot. Content-hostile streams must land
		// nothing, so refuse the shapes here (absent keys are fine: archives
		// from before migration 0017 fall back to the column defaults).
		if v, present := obj["plan"]; present {
			if _, ok := v.(string); !ok {
				return badf("accounts row plan must be a string")
			}
		}
		if v, present := obj["plan_limits"]; present {
			m, ok := v.(map[string]any)
			if !ok {
				return badf("accounts row plan_limits must be an object of integer limits")
			}
			for key, raw := range m {
				f, ok := raw.(float64)
				if !ok || f != math.Trunc(f) {
					return badf("accounts row plan_limits[%q] must be an integer", key)
				}
			}
		}
		if v, present := obj["plan_features"]; present {
			fs, ok := v.([]any)
			if !ok {
				return badf("accounts row plan_features must be an array of strings")
			}
			for _, raw := range fs {
				if _, ok := raw.(string); !ok {
					return badf("accounts row plan_features entries must be strings")
				}
			}
		}
		if v, present := obj["placement_policy"]; present {
			if _, err := placement.FromAny(v); err != nil {
				return badf("accounts row %v", err)
			}
		}
		ic.accounts++
		if ic.accounts > 1 {
			return badf("archive contains more than one accounts row")
		}
	case "operators":
		if id, ok := stringField(obj, "id"); ok {
			ic.operators[id] = true
		}
	case "realms":
		if id, ok := stringField(obj, "id"); ok {
			ic.realms[id] = true
		}
	case "agents":
		realmID, err := requireStringField(obj, "realm_id")
		if err != nil {
			return badf("agents row missing realm_id")
		}
		if !ic.realms[realmID] {
			return badf("agents row references realm %q not present in this archive", realmID)
		}
		if id, ok := stringField(obj, "id"); ok {
			ic.agents[id] = true
			ic.agentRealms[id] = realmID
		}
	case "tokens":
		if opID, present := optionalStringField(obj, "operator_id"); present && !ic.operators[opID] {
			return badf("tokens row references operator %q not present in this archive", opID)
		}
		if agID, present := optionalStringField(obj, "agent_id"); present && !ic.agents[agID] {
			return badf("tokens row references agent %q not present in this archive", agID)
		}
	case "fact_subjects":
		realmID, err := requireStringField(obj, "realm_id")
		agentID, agentErr := requireStringField(obj, "owner_agent_id")
		if err != nil || agentErr != nil || !ic.agents[agentID] || ic.agentRealms[agentID] != realmID {
			return badf("fact_subjects row owner %q is outside realm %q", agentID, realmID)
		}
		id, err := requireStringField(obj, "id")
		if err != nil {
			return badf("fact_subjects row missing id")
		}
		canonicalKey, err := validateImportedFactSubjectContent(obj)
		if err != nil {
			return badf("fact_subjects row %v", err)
		}
		namespace := ic.factSubjectNames[agentID]
		if namespace == nil {
			namespace = map[string]string{}
			ic.factSubjectNames[agentID] = namespace
		}
		names := []string{canonicalKey}
		for _, raw := range obj["aliases"].([]any) {
			names = append(names, raw.(string))
		}
		for _, name := range names {
			if existing := namespace[name]; existing != "" && existing != id {
				return badf("fact_subjects row name %q conflicts with subject %q", name, existing)
			}
			namespace[name] = id
		}
		ic.factSubjects[id] = factImportScope{realmID: realmID, ownerAgentID: agentID, subjectKey: canonicalKey}
	case "facts":
		subjectID, err := requireStringField(obj, "subject_id")
		scope, ok := ic.factSubjects[subjectID]
		if err != nil || !ok {
			return badf("facts row references subject %q not present in this archive", subjectID)
		}
		realmID, _ := requireStringField(obj, "realm_id")
		agentID, _ := requireStringField(obj, "owner_agent_id")
		if realmID != scope.realmID || agentID != scope.ownerAgentID {
			return badf("facts row scope does not match subject %q", subjectID)
		}
		id, err := requireStringField(obj, "id")
		if err != nil {
			return badf("facts row missing id")
		}
		resolvedID, deleted, replacementID, err := validateImportedFactContent(obj, scope.ownerAgentID)
		if err != nil {
			return badf("facts row %v", err)
		}
		scope.resolvedAssertionID = resolvedID
		scope.subjectID = subjectID
		scope.predicate, _ = requireStringField(obj, "predicate")
		scope.sensitive, _ = obj["sensitive"].(bool)
		scope.deleted = deleted
		scope.replacementFactID = replacementID
		scope.deletedMutationKeyCount, _ = importedNonnegativeInteger(obj["deleted_mutation_key_count"])
		ic.facts[id] = scope
	case "fact_mutation_tombstones":
		factID, err := requireStringField(obj, "fact_id")
		scope, ok := ic.facts[factID]
		if err != nil || !ok || !scope.deleted {
			return badf("fact_mutation_tombstones row references non-deleted fact %q", factID)
		}
		realmID, _ := requireStringField(obj, "realm_id")
		agentID, _ := requireStringField(obj, "owner_agent_id")
		if realmID != scope.realmID || agentID != scope.ownerAgentID {
			return badf("fact_mutation_tombstones row scope does not match fact %q", factID)
		}
		if err := validateImportedFactMutationTombstone(obj); err != nil {
			return badf("fact_mutation_tombstones row %v", err)
		}
		ic.factMutationTombstoneCounts[factID]++
	case "fact_assertions":
		factID, err := requireStringField(obj, "fact_id")
		scope, ok := ic.facts[factID]
		if err != nil || !ok {
			return badf("fact_assertions row references fact %q not present in this archive", factID)
		}
		if scope.deleted {
			return badf("fact_assertions row references deleted fact %q", factID)
		}
		realmID, _ := requireStringField(obj, "realm_id")
		if realmID != scope.realmID {
			return badf("fact_assertions row realm does not match fact %q", factID)
		}
		if agentID, present := optionalStringField(obj, "asserted_by_agent_id"); present && agentID != scope.ownerAgentID {
			return badf("fact_assertions row actor %q does not own fact %q", agentID, factID)
		}
		if err := validateImportedFactAssertionContent(obj); err != nil {
			return badf("fact_assertions row %v", err)
		}
		if prior, present := optionalStringField(obj, "supersedes_id"); present && ic.assertions[prior] != factID {
			return badf("fact_assertions row supersedes %q outside fact %q", prior, factID)
		}
		id, err := requireStringField(obj, "id")
		if err != nil {
			return badf("fact_assertions row missing id")
		}
		ic.assertions[id] = factID
	case "fact_candidates":
		realmID, err := requireStringField(obj, "realm_id")
		agentID, agentErr := requireStringField(obj, "owner_agent_id")
		if err != nil || agentErr != nil || !ic.agents[agentID] || ic.agentRealms[agentID] != realmID {
			return badf("fact_candidates row owner %q is outside realm %q", agentID, realmID)
		}
		if err := validateImportedFactCandidateContent(obj); err != nil {
			return badf("fact_candidates row %v", err)
		}
		subjectKey, _ := requireStringField(obj, "subject_key")
		predicate, _ := requireStringField(obj, "predicate")
		addressSubjectKey := subjectKey
		if subjectID := ic.factSubjectNames[agentID][subjectKey]; subjectID != "" {
			addressSubjectKey = ic.factSubjects[subjectID].subjectKey
		}
		for _, factScope := range ic.facts {
			if factScope.deleted && factScope.replacementFactID == "" &&
				factScope.realmID == realmID && factScope.ownerAgentID == agentID &&
				factScope.subjectKey == addressSubjectKey && factScope.predicate == predicate {
				return badf("fact_candidates row occupies an unrecreated deleted fact address")
			}
		}
		for _, field := range []string{"conflict_fact_id", "resolved_fact_id"} {
			factID, present := optionalStringField(obj, field)
			if !present {
				continue
			}
			scope, ok := ic.facts[factID]
			if !ok || scope.realmID != realmID || scope.ownerAgentID != agentID {
				return badf("fact_candidates row %s %q is outside its agent scope", field, factID)
			}
			if scope.subjectKey != addressSubjectKey || scope.predicate != predicate {
				return badf("fact_candidates row %s %q is at a different fact address", field, factID)
			}
		}
		observedAssertionID, observed := optionalStringField(obj, "observed_assertion_id")
		if observed {
			factID, ok := ic.assertions[observedAssertionID]
			scope, scoped := ic.facts[factID]
			if !ok || !scoped || scope.realmID != realmID || scope.ownerAgentID != agentID {
				return badf("fact_candidates row observed_assertion_id %q is outside its agent scope", observedAssertionID)
			}
			if scope.subjectKey != addressSubjectKey || scope.predicate != predicate {
				return badf("fact_candidates row observed_assertion_id %q is at a different fact address", observedAssertionID)
			}
			if conflictFactID, conflict := optionalStringField(obj, "conflict_fact_id"); conflict && factID != conflictFactID {
				return badf("fact_candidates row observed_assertion_id %q does not belong to conflict fact %q", observedAssertionID, conflictFactID)
			}
		}
		decisionAssertionID, decided := optionalStringField(obj, "decision_assertion_id")
		if decided {
			factID, ok := ic.assertions[decisionAssertionID]
			resolvedFactID, resolved := optionalStringField(obj, "resolved_fact_id")
			if !ok || !resolved || factID != resolvedFactID {
				return badf("fact_candidates row decision_assertion_id %q does not belong to resolved fact %q", decisionAssertionID, resolvedFactID)
			}
			scope := ic.facts[factID]
			if scope.subjectKey != addressSubjectKey || scope.predicate != predicate {
				return badf("fact_candidates row decision_assertion_id %q is at a different fact address", decisionAssertionID)
			}
		}
	case "account_events":
		// The account_id scoping check already ran in the first switch;
		// no downstream table references account_events, so nothing to
		// record here. Metadata is opaque JSONB — the write-time verb
		// contract was enforced when the event was created and doesn't
		// need to be re-enforced at import time (an old cell may have
		// written events under a schema this cell no longer knows).
	case "support_tickets":
		// Record the ticket id so incoming support_ticket_messages
		// rows can be FK-validated against this-archive tickets only.
		if id, ok := stringField(obj, "id"); ok {
			ic.tickets[id] = true
		}
	case "support_ticket_messages":
		// FK-scope check: the ticket_id must belong to a ticket this
		// same archive already inserted. Cross-tenant grafting is
		// blocked the same way agents.realm_id is checked against
		// ic.realms.
		ticketID, err := requireStringField(obj, "ticket_id")
		if err != nil {
			return badf("support_ticket_messages row missing ticket_id")
		}
		if !ic.tickets[ticketID] {
			return badf("support_ticket_messages row references ticket %q not present in this archive", ticketID)
		}
	case "transcript_conversations":
		realmID, err := requireStringField(obj, "realm_id")
		if err != nil || !ic.realms[realmID] {
			return badf("transcript_conversations row references realm %q not present in this archive", realmID)
		}
		agentID, err := requireStringField(obj, "owner_agent_id")
		if err != nil || !ic.agents[agentID] {
			return badf("transcript_conversations row references agent %q not present in this archive", agentID)
		}
		id, err := requireStringField(obj, "id")
		if err != nil {
			return badf("transcript_conversations row missing id")
		}
		ic.transcripts[id] = transcriptImportScope{realmID: realmID, ownerAgentID: agentID}
	case "transcript_entries":
		transcriptID, err := requireStringField(obj, "transcript_id")
		if err != nil {
			return badf("transcript_entries row missing transcript_id")
		}
		scope, ok := ic.transcripts[transcriptID]
		if !ok {
			return badf("transcript_entries row references transcript %q not present in this archive", transcriptID)
		}
		realmID, err := requireStringField(obj, "realm_id")
		if err != nil || realmID != scope.realmID {
			return badf("transcript_entries row realm %q does not match transcript realm %q", realmID, scope.realmID)
		}
		agentID, err := requireStringField(obj, "recorded_by_agent_id")
		if err != nil || agentID != scope.ownerAgentID || !ic.agents[agentID] {
			return badf("transcript_entries row recorder %q does not match transcript owner %q", agentID, scope.ownerAgentID)
		}
		if replyID, present := optionalStringField(obj, "reply_to_entry_id"); present {
			if parentTranscript, ok := ic.entries[replyID]; !ok || parentTranscript != transcriptID {
				return badf("transcript_entries row reply target %q is not an earlier entry in transcript %q", replyID, transcriptID)
			}
		}
		id, err := requireStringField(obj, "id")
		if err != nil {
			return badf("transcript_entries row missing id")
		}
		ic.entries[id] = transcriptID
	case "usage_events":
		if err := ic.validateUsageScope(obj, badf, "usage_events"); err != nil {
			return err
		}
		dimension, _ := stringField(obj, "dimension")
		if strings.HasPrefix(dimension, "transcript_") {
			subjectType, _ := stringField(obj, "subject_type")
			subjectID, _ := stringField(obj, "subject_id")
			scope, ok := ic.transcripts[subjectID]
			agentID, _ := stringField(obj, "agent_id")
			realmID, _ := stringField(obj, "realm_id")
			if subjectType != "transcript" || !ok || scope.ownerAgentID != agentID || scope.realmID != realmID {
				return badf("usage_events row transcript subject %q does not belong to its agent scope", subjectID)
			}
		}
		if strings.HasPrefix(dimension, "fact_") {
			subjectType, _ := stringField(obj, "subject_type")
			subjectID, _ := stringField(obj, "subject_id")
			scope, ok := ic.facts[subjectID]
			agentID, _ := stringField(obj, "agent_id")
			realmID, _ := stringField(obj, "realm_id")
			if subjectType != "fact" || !ok || scope.ownerAgentID != agentID || scope.realmID != realmID {
				return badf("usage_events row fact subject %q does not belong to its agent scope", subjectID)
			}
		}
	case "usage_rollups":
		if err := ic.validateUsageScope(obj, badf, "usage_rollups"); err != nil {
			return err
		}
	case "agent_messages":
		realmID, err := requireStringField(obj, "realm_id")
		if err != nil || !ic.realms[realmID] {
			return badf("agent_messages row references realm %q not present in this archive", realmID)
		}
		fromAgentID, err := requireStringField(obj, "from_agent_id")
		if err != nil || !ic.agents[fromAgentID] {
			return badf("agent_messages row references sender %q not present in this archive", fromAgentID)
		}
		toAgentID, err := requireStringField(obj, "to_agent_id")
		if err != nil || !ic.agents[toAgentID] {
			return badf("agent_messages row references recipient %q not present in this archive", toAgentID)
		}
		if ic.agentRealms[fromAgentID] != realmID || ic.agentRealms[toAgentID] != realmID {
			return badf("agent_messages row agents must belong to realm %q", realmID)
		}
		id, err := requireStringField(obj, "id")
		if err != nil {
			return badf("agent_messages row missing id")
		}
		ic.messages[id] = messageImportScope{
			realmID: realmID, fromAgentID: fromAgentID, toAgentID: toAgentID,
		}
	case "agent_message_deliveries":
		messageID, err := requireStringField(obj, "message_id")
		if err != nil {
			return badf("agent_message_deliveries row missing message_id")
		}
		scope, ok := ic.messages[messageID]
		if !ok {
			return badf("agent_message_deliveries row references message %q not present in this archive", messageID)
		}
		realmID, err := requireStringField(obj, "realm_id")
		if err != nil || realmID != scope.realmID {
			return badf("agent_message_deliveries row realm %q does not match message realm %q", realmID, scope.realmID)
		}
		recipientID, err := requireStringField(obj, "recipient_agent_id")
		if err != nil || recipientID != scope.toAgentID {
			return badf("agent_message_deliveries recipient %q does not match message recipient %q", recipientID, scope.toAgentID)
		}
		ic.deliveries[messageID] = true
	default:
		return badf("table %q is not importable", table)
	}
	return nil
}

func (ic *importCtx) validateUsageScope(obj map[string]any, badf func(string, ...any) error, table string) error {
	realmID, err := requireStringField(obj, "realm_id")
	if err != nil || !ic.realms[realmID] {
		return badf("%s row references realm %q not present in this archive", table, realmID)
	}
	agentID, err := requireStringField(obj, "agent_id")
	if err != nil || !ic.agents[agentID] || ic.agentRealms[agentID] != realmID {
		return badf("%s row references agent %q outside realm %q", table, agentID, realmID)
	}
	return nil
}

// requireStringField reads a JSON string field; treats JSON null / missing / wrong-type as absent.
func requireStringField(obj map[string]any, key string) (string, error) {
	s, ok := stringField(obj, key)
	if !ok {
		return "", fmt.Errorf("required %s absent", key)
	}
	return s, nil
}

func stringField(obj map[string]any, key string) (string, bool) {
	v, present := obj[key]
	if !present || v == nil {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// optionalStringField distinguishes "the field is a non-empty string" from
// "the field is absent or JSON null" (both legal — FKs are nullable). A
// present-but-non-string value is treated as absent, since it can't be a
// valid FK target anyway; the subsequent INSERT will fail its type coercion.
func optionalStringField(obj map[string]any, key string) (string, bool) {
	return stringField(obj, key)
}

func validateImportedFactSubjectContent(obj map[string]any) (string, error) {
	canonicalKey, ok := obj["canonical_key"].(string)
	if !ok || !factSubjectPattern.MatchString(canonicalKey) || normalizeFactSubject(canonicalKey) != canonicalKey {
		return "", fmt.Errorf("canonical_key is invalid")
	}
	displayName, ok := obj["display_name"].(string)
	if !ok || !validFactSubjectDisplayName(displayName) {
		return "", fmt.Errorf("display_name is invalid")
	}
	aliases, ok := obj["aliases"].([]any)
	if !ok {
		return "", fmt.Errorf("aliases must be an array of strings")
	}
	encodedAliases, err := json.Marshal(aliases)
	if err != nil || len(encodedAliases) > maxFactSubjectAliasesJSONBytes {
		return "", fmt.Errorf("aliases exceed the size limit")
	}
	seen := map[string]bool{canonicalKey: true}
	for _, raw := range aliases {
		alias, ok := raw.(string)
		if !ok || !validFactSubjectAlias(alias) || normalizeFactSubjectAlias(alias) != alias {
			return "", fmt.Errorf("aliases must contain normalized strings")
		}
		if normalizeFactSubject(alias) == "self" && canonicalKey != "self" {
			return "", fmt.Errorf("alias %q is reserved for self", alias)
		}
		if seen[alias] {
			return "", fmt.Errorf("alias %q is duplicated", alias)
		}
		seen[alias] = true
	}
	return canonicalKey, nil
}

func validateImportedFactContent(obj map[string]any, ownerAgentID string) (resolvedID string, deleted bool, replacementID string, err error) {
	predicate, ok := obj["predicate"].(string)
	if !ok || !validFactPredicate(predicate) {
		return "", false, "", fmt.Errorf("predicate is invalid")
	}
	if _, ok := obj["sensitive"].(bool); !ok {
		return "", false, "", fmt.Errorf("sensitive must be a boolean")
	}
	cardinality, ok := obj["cardinality"].(string)
	if !ok || (cardinality != FactCardinalityOne && cardinality != FactCardinalityMany && cardinality != FactCardinalityOneAtTime) {
		return "", false, "", fmt.Errorf("cardinality is invalid")
	}
	resolvedID, _ = optionalStringField(obj, "resolved_assertion_id")
	deletedAt, hasDeletedAt, err := importedOptionalTimestamp(obj, "deleted_at")
	if err != nil {
		return "", false, "", err
	}
	_ = deletedAt
	deleted = hasDeletedAt
	deletedBy, hasDeletedBy := optionalStringField(obj, "deleted_by_agent_id")
	receipt, _ := obj["delete_receipt_id"].(string)
	deleteKeyHash, _ := obj["delete_idempotency_key_hash"].(string)
	priorAssertionID, _ := obj["deleted_prior_assertion_id"].(string)
	assertionCount, assertionCountOK := importedNonnegativeInteger(obj["deleted_assertion_count"])
	candidateCount, candidateCountOK := importedNonnegativeInteger(obj["deleted_candidate_count"])
	usageCount, usageCountOK := importedNonnegativeInteger(obj["deleted_usage_count"])
	mutationKeyCount, mutationKeyCountOK := importedNonnegativeInteger(obj["deleted_mutation_key_count"])
	candidateRevision, _ := obj["deleted_candidate_revision"].(string)
	_, hasRecreatedAt, err := importedOptionalTimestamp(obj, "recreated_at")
	if err != nil {
		return "", false, "", err
	}
	replacementID, hasReplacement := optionalStringField(obj, "replacement_fact_id")

	if !deleted {
		if resolvedID == "" {
			return "", false, "", fmt.Errorf("active fact requires resolved_assertion_id")
		}
		if hasDeletedBy || receipt != "" || deleteKeyHash != "" || priorAssertionID != "" ||
			!assertionCountOK || assertionCount != 0 || !candidateCountOK || candidateCount != 0 ||
			!usageCountOK || usageCount != 0 ||
			!mutationKeyCountOK || mutationKeyCount != 0 ||
			candidateRevision != "" ||
			hasRecreatedAt || hasReplacement {
			return "", false, "", fmt.Errorf("active fact carries deletion metadata")
		}
		return resolvedID, false, "", nil
	}
	if resolvedID != "" || !hasDeletedBy || deletedBy != ownerAgentID ||
		!strings.HasPrefix(receipt, "fdel_") || !validFactSHA256(deleteKeyHash) ||
		!strings.HasPrefix(priorAssertionID, "fas_") || !assertionCountOK || assertionCount < 1 ||
		!candidateCountOK || !usageCountOK || !mutationKeyCountOK || !validFactSHA256(candidateRevision) {
		return "", false, "", fmt.Errorf("deleted fact receipt is invalid")
	}
	if hasRecreatedAt != hasReplacement {
		return "", false, "", fmt.Errorf("deleted fact replacement metadata is incomplete")
	}
	if hasReplacement && (!strings.HasPrefix(replacementID, "fact_") || replacementID == obj["id"]) {
		return "", false, "", fmt.Errorf("replacement_fact_id is invalid")
	}
	return "", true, replacementID, nil
}

func validateImportedFactMutationTombstone(obj map[string]any) error {
	id, ok := obj["id"].(string)
	if !ok || !strings.HasPrefix(id, "fmt_") {
		return fmt.Errorf("id is invalid")
	}
	surface, ok := obj["surface"].(string)
	if !ok || (surface != "set" && surface != "proposal") {
		return fmt.Errorf("surface is invalid")
	}
	hash, ok := obj["idempotency_key_hash"].(string)
	if !ok || !validFactSHA256(hash) {
		return fmt.Errorf("idempotency_key_hash is invalid")
	}
	if _, present, err := importedOptionalTimestamp(obj, "deleted_at"); err != nil || !present {
		return fmt.Errorf("deleted_at must be an RFC3339 timestamp")
	}
	return nil
}

func importedOptionalTimestamp(obj map[string]any, key string) (*time.Time, bool, error) {
	raw, present := obj[key]
	if !present || raw == nil {
		return nil, false, nil
	}
	value, ok := raw.(string)
	if !ok || value == "" {
		return nil, false, fmt.Errorf("%s must be an RFC3339 timestamp", key)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, false, fmt.Errorf("%s must be an RFC3339 timestamp", key)
	}
	return &parsed, true, nil
}

func importedNonnegativeInteger(raw any) (int64, bool) {
	value, ok := raw.(float64)
	if !ok || value < 0 || value != math.Trunc(value) || value > math.MaxInt64 {
		return 0, false
	}
	return int64(value), true
}

func validFactSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validateImportedFactAssertionContent(obj map[string]any) error {
	requiredString := func(key string) (string, error) {
		value, ok := obj[key].(string)
		if !ok || value == "" {
			return "", fmt.Errorf("%s must be a non-empty string", key)
		}
		return value, nil
	}
	stringValue := func(key string) (string, error) {
		value, ok := obj[key].(string)
		if !ok {
			return "", fmt.Errorf("%s must be a string", key)
		}
		return value, nil
	}
	parseTimestamp := func(key string, required bool) (*time.Time, error) {
		raw, present := obj[key]
		if !present || raw == nil {
			if required {
				return nil, fmt.Errorf("%s must be an RFC3339 timestamp", key)
			}
			return nil, nil
		}
		value, ok := raw.(string)
		if !ok || value == "" {
			return nil, fmt.Errorf("%s must be an RFC3339 timestamp", key)
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return nil, fmt.Errorf("%s must be an RFC3339 timestamp", key)
		}
		return &parsed, nil
	}

	valueType, err := requiredString("value_type")
	if err != nil {
		return err
	}
	recurrence := ""
	if raw, present := obj["recurrence"]; present {
		var ok bool
		recurrence, ok = raw.(string)
		if !ok {
			return fmt.Errorf("recurrence must be a string")
		}
	}
	sourceKind, err := requiredString("source_kind")
	if err != nil {
		return err
	}
	sourceRef, err := stringValue("source_ref")
	if err != nil {
		return err
	}
	confidence, ok := obj["confidence"].(float64)
	if !ok {
		return fmt.Errorf("confidence must be a number")
	}
	value, present := obj["value"]
	if !present {
		return fmt.Errorf("value is required")
	}
	rawValue, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("value is not JSON: %v", err)
	}
	observedAt, err := parseTimestamp("observed_at", true)
	if err != nil {
		return err
	}
	confirmedAt, err := parseTimestamp("confirmed_at", false)
	if err != nil {
		return err
	}
	validFrom, err := parseTimestamp("valid_from", false)
	if err != nil {
		return err
	}
	validUntil, err := parseTimestamp("valid_until", false)
	if err != nil {
		return err
	}
	if _, err := parseTimestamp("created_at", true); err != nil {
		return err
	}
	normalized, err := normalizeSetFactInput(SetFactInput{
		Subject: "self", Predicate: "archive/value", ValueType: valueType,
		Value: rawValue, Recurrence: recurrence, Cardinality: FactCardinalityOne,
		SourceKind: sourceKind, SourceRef: sourceRef, Confidence: &confidence,
		ObservedAt: *observedAt, ConfirmedAt: confirmedAt,
		ValidFrom: validFrom, ValidUntil: validUntil,
	})
	if err != nil {
		return err
	}
	if normalized.ValueType != valueType || normalized.Recurrence != recurrence ||
		normalized.SourceKind != sourceKind || !jsonValuesEqual(normalized.Value, rawValue) {
		return fmt.Errorf("logical content is not canonical")
	}
	return nil
}

func validateImportedFactCandidateContent(obj map[string]any) error {
	required := func(key string) (string, error) {
		value, ok := obj[key].(string)
		if !ok || value == "" {
			return "", fmt.Errorf("%s must be a non-empty string", key)
		}
		return value, nil
	}
	stringValue := func(key string) (string, error) {
		value, ok := obj[key].(string)
		if !ok {
			return "", fmt.Errorf("%s must be a string", key)
		}
		return value, nil
	}
	parseTimestamp := func(key string, required bool) (*time.Time, error) {
		raw, present := obj[key]
		if !present || raw == nil {
			if required {
				return nil, fmt.Errorf("%s must be an RFC3339 timestamp", key)
			}
			return nil, nil
		}
		value, ok := raw.(string)
		if !ok || value == "" {
			return nil, fmt.Errorf("%s must be an RFC3339 timestamp", key)
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return nil, fmt.Errorf("%s must be an RFC3339 timestamp", key)
		}
		return &parsed, nil
	}
	optionalID := func(key string) (string, bool, error) {
		raw, present := obj[key]
		if !present || raw == nil {
			return "", false, nil
		}
		value, ok := raw.(string)
		if !ok || value == "" {
			return "", false, fmt.Errorf("%s must be null or a non-empty string", key)
		}
		return value, true, nil
	}

	subject, err := required("subject_key")
	if err != nil {
		return err
	}
	predicate, err := required("predicate")
	if err != nil {
		return err
	}
	valueType, err := required("value_type")
	if err != nil {
		return err
	}
	recurrence := ""
	if raw, present := obj["recurrence"]; present {
		var ok bool
		recurrence, ok = raw.(string)
		if !ok {
			return fmt.Errorf("recurrence must be a string")
		}
	}
	cardinality, err := required("cardinality")
	if err != nil {
		return err
	}
	sourceRef, err := stringValue("source_ref")
	if err != nil {
		return err
	}
	reason, err := stringValue("reason")
	if err != nil {
		return err
	}
	if len(reason) > 1024 {
		return fmt.Errorf("reason exceeds 1024 bytes")
	}
	sensitive, ok := obj["sensitive"].(bool)
	if !ok {
		return fmt.Errorf("sensitive must be a boolean")
	}
	confidence, ok := obj["confidence"].(float64)
	if !ok {
		return fmt.Errorf("confidence must be a number")
	}
	value, present := obj["value"]
	if !present {
		return fmt.Errorf("value is required")
	}
	rawValue, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("value is not JSON: %v", err)
	}
	observedAt, err := parseTimestamp("observed_at", true)
	if err != nil {
		return err
	}
	validFrom, err := parseTimestamp("valid_from", false)
	if err != nil {
		return err
	}
	validUntil, err := parseTimestamp("valid_until", false)
	if err != nil {
		return err
	}
	proposedAt, err := parseTimestamp("proposed_at", true)
	if err != nil {
		return err
	}
	decidedAt, err := parseTimestamp("decided_at", false)
	if err != nil {
		return err
	}
	if decidedAt != nil && decidedAt.Before(*proposedAt) {
		return fmt.Errorf("decided_at precedes proposed_at")
	}
	normalized, err := normalizeSetFactInput(SetFactInput{
		Subject: subject, Predicate: predicate, ValueType: valueType,
		Recurrence: recurrence,
		Value:      rawValue, Cardinality: cardinality, Sensitive: sensitive,
		SourceKind: FactSourceInference, SourceRef: sourceRef,
		Confidence: &confidence, ObservedAt: *observedAt,
		ValidFrom: validFrom, ValidUntil: validUntil,
	})
	if err != nil {
		return err
	}
	if normalized.Subject != subject || normalized.Predicate != predicate ||
		normalized.ValueType != valueType || normalized.Recurrence != recurrence || normalized.Cardinality != cardinality ||
		!jsonValuesEqual(normalized.Value, rawValue) {
		return fmt.Errorf("logical content is not canonical")
	}

	status, err := required("status")
	if err != nil {
		return err
	}
	_, hasConflict, err := optionalID("conflict_fact_id")
	if err != nil {
		return err
	}
	_, hasObserved, err := optionalID("observed_assertion_id")
	if err != nil {
		return err
	}
	_, hasResolved, err := optionalID("resolved_fact_id")
	if err != nil {
		return err
	}
	switch status {
	case "pending":
		if hasConflict || hasResolved || decidedAt != nil {
			return fmt.Errorf("pending lifecycle fields are inconsistent")
		}
	case "conflict":
		if !hasConflict || !hasObserved || hasResolved || decidedAt != nil {
			return fmt.Errorf("conflict lifecycle fields are inconsistent")
		}
	case "confirmed":
		if !hasResolved || decidedAt == nil {
			return fmt.Errorf("confirmed lifecycle fields are inconsistent")
		}
	case "rejected":
		if hasResolved || decidedAt == nil {
			return fmt.Errorf("rejected lifecycle fields are inconsistent")
		}
	default:
		return fmt.Errorf("status %q is invalid", status)
	}
	return nil
}

// ImportAccount restores one account's logical archive from r into this cell.
// The entire restore is a single transaction committed only after the
// archive's trailing checksums verify AND every row's account/FK scoping
// checks pass, so a truncated, tampered, or content-hostile stream lands
// nothing. The account arrives in its exported state — suspended (or a
// closed tombstone); resuming is the caller's separate, explicit step.
//
// expectedAccountID pins the archive to the account the caller believes it
// is restoring; a manifest naming anyone else refuses before rows stream.
func (s *Store) ImportAccount(ctx context.Context, expectedAccountID string, r io.Reader) (export.Manifest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return export.Manifest{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ic := newImportCtx(expectedAccountID)

	m, err := export.Read(ctx, r, export.ImportOptions{
		CurrentSchema: SchemaVersion(),
		OnManifest: func(m export.Manifest) error {
			if m.AccountID == "" || m.AccountID != expectedAccountID {
				return fmt.Errorf("%w: archive is for %q", ErrArchiveAccountMismatch, m.AccountID)
			}
			if m.Status != "suspended" && m.Status != "closed" {
				return fmt.Errorf("%w: manifest status %q — exports are only taken frozen", ErrArchiveContent, m.Status)
			}
			var exists bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM accounts WHERE id = $1)`,
				m.AccountID).Scan(&exists); err != nil {
				return fmt.Errorf("check import target: %w", err)
			}
			if exists {
				return ErrAccountExists
			}
			return nil
		},
		Row: func(table string, row []byte) error {
			if _, ok := importColumns[table]; !ok {
				return fmt.Errorf("%w: table %q not importable", ErrArchiveContent, table)
			}
			var obj map[string]any
			if err := json.Unmarshal(row, &obj); err != nil {
				return fmt.Errorf("%w: %s row is not JSON: %v", ErrArchiveContent, table, err)
			}
			if err := ic.validateAndRecord(table, obj); err != nil {
				return err
			}
			return insertProjected(ctx, tx, table, obj, row)
		},
	})
	if err != nil {
		return export.Manifest{}, err
	}
	for messageID := range ic.messages {
		if !ic.deliveries[messageID] {
			return export.Manifest{}, fmt.Errorf("%w: message %q has no recipient delivery row", ErrArchiveContent, messageID)
		}
	}
	for factID, scope := range ic.facts {
		if scope.deleted {
			if scope.resolvedAssertionID != "" {
				return export.Manifest{}, fmt.Errorf("%w: deleted fact %q has a resolved assertion", ErrArchiveContent, factID)
			}
			if scope.replacementFactID != "" {
				replacement, ok := ic.facts[scope.replacementFactID]
				if !ok || replacement.subjectID != scope.subjectID || replacement.predicate != scope.predicate ||
					replacement.realmID != scope.realmID || replacement.ownerAgentID != scope.ownerAgentID {
					return export.Manifest{}, fmt.Errorf("%w: deleted fact %q has invalid replacement %q", ErrArchiveContent, factID, scope.replacementFactID)
				}
				if scope.sensitive && !replacement.sensitive {
					return export.Manifest{}, fmt.Errorf("%w: sensitive deleted fact %q has non-sensitive replacement %q", ErrArchiveContent, factID, scope.replacementFactID)
				}
			}
			continue
		}
		if scope.resolvedAssertionID == "" || ic.assertions[scope.resolvedAssertionID] != factID {
			return export.Manifest{}, fmt.Errorf("%w: fact %q has no valid resolved assertion", ErrArchiveContent, factID)
		}
	}
	if err := validateImportedFactReplacementTopology(ic.facts); err != nil {
		return export.Manifest{}, fmt.Errorf("%w: fact replacement topology: %v", ErrArchiveContent, err)
	}
	if err := validateImportedFactMutationTombstoneCompleteness(ic.facts, ic.factMutationTombstoneCounts); err != nil {
		return export.Manifest{}, fmt.Errorf("%w: fact mutation tombstones: %v", ErrArchiveContent, err)
	}
	if err := validateImportedFactDecisionAssertions(ctx, tx, m.AccountID); err != nil {
		return export.Manifest{}, err
	}
	if err := validateImportedUsageRollups(ctx, tx, m.AccountID); err != nil {
		return export.Manifest{}, err
	}

	// The archive's own account row must have actually landed, and must not
	// claim the deployment's default seat. These are all permanent
	// archive-content defects (missing accounts row, is_default lie,
	// status mismatch), so they wrap ErrArchiveContent and surface as
	// 400 — retrying against the same object cannot recover.
	var isDefault bool
	var status string
	err = tx.QueryRow(ctx,
		`SELECT is_default, status FROM accounts WHERE id = $1`,
		m.AccountID).Scan(&isDefault, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return export.Manifest{}, fmt.Errorf("%w: no accounts row landed for %s", ErrArchiveContent, m.AccountID)
	}
	if err != nil {
		return export.Manifest{}, fmt.Errorf("verify landed account: %w", err)
	}
	if isDefault {
		return export.Manifest{}, fmt.Errorf("%w: landed row claims the default seat", ErrArchiveContent)
	}
	if status != m.Status {
		return export.Manifest{}, fmt.Errorf("%w: landed account row status %q disagrees with manifest %q", ErrArchiveContent, status, m.Status)
	}

	if err := tx.Commit(ctx); err != nil {
		return export.Manifest{}, err
	}
	return m, nil
}

func validateImportedFactMutationTombstoneCompleteness(facts map[string]factImportScope, counts map[string]int64) error {
	for factID, scope := range facts {
		got := counts[factID]
		want := scope.deletedMutationKeyCount
		if got != want {
			return fmt.Errorf("fact %q carries %d retry tombstones, receipt commits to %d", factID, got, want)
		}
	}
	return nil
}

// validateImportedFactReplacementTopology proves that each fact address is one
// linear history. A checksummed but hostile archive must not be able to create
// duplicate unrecreated tombstones (which wedge explicit recreation), a cycle
// (which removes the required tail), or disconnected histories (which let an
// ordinary set bypass a deletion guard).
func validateImportedFactReplacementTopology(facts map[string]factImportScope) error {
	type address struct {
		realmID, ownerAgentID, subjectID, predicate string
	}
	groups := map[address]map[string]factImportScope{}
	for factID, scope := range facts {
		key := address{scope.realmID, scope.ownerAgentID, scope.subjectID, scope.predicate}
		if groups[key] == nil {
			groups[key] = map[string]factImportScope{}
		}
		groups[key][factID] = scope
	}
	for _, nodes := range groups {
		activeID := ""
		incoming := map[string]int{}
		for factID, scope := range nodes {
			if !scope.deleted {
				if activeID != "" {
					return fmt.Errorf("address has multiple active facts %q and %q", activeID, factID)
				}
				activeID = factID
			}
			if scope.replacementFactID != "" {
				replacement, ok := nodes[scope.replacementFactID]
				if !ok {
					return fmt.Errorf("fact %q replacement %q is outside its address", factID, scope.replacementFactID)
				}
				if scope.sensitive && !replacement.sensitive {
					return fmt.Errorf("sensitive fact %q has non-sensitive replacement %q", factID, scope.replacementFactID)
				}
				incoming[scope.replacementFactID]++
				if incoming[scope.replacementFactID] > 1 {
					return fmt.Errorf("fact %q has multiple replacement predecessors", scope.replacementFactID)
				}
			}
		}

		tailID := activeID
		if tailID == "" {
			for factID, scope := range nodes {
				if scope.replacementFactID == "" {
					if tailID != "" {
						return fmt.Errorf("address has multiple unrecreated tombstones %q and %q", tailID, factID)
					}
					tailID = factID
				}
			}
			if tailID == "" {
				return fmt.Errorf("deleted address has no unrecreated tail")
			}
		}

		for start := range nodes {
			seen := map[string]bool{}
			current := start
			for nodes[current].replacementFactID != "" {
				if seen[current] {
					return fmt.Errorf("replacement cycle contains fact %q", current)
				}
				seen[current] = true
				current = nodes[current].replacementFactID
			}
			if current != tailID {
				return fmt.Errorf("fact %q replacement chain ends at %q, want %q", start, current, tailID)
			}
		}
	}
	return nil
}

// validateImportedFactDecisionAssertions proves that every persisted confirm
// retry points at the exact assertion promoted by that candidate. A foreign
// key to the same fact is insufficient: a content-hostile but correctly
// checksummed archive could otherwise repoint the retry to an older assertion
// and make a restored confirm return the wrong historical result.
func validateImportedFactDecisionAssertions(ctx context.Context, q factQuerier, accountID string) error {
	var mismatch bool
	err := q.QueryRow(ctx, `
		WITH RECURSIVE fact_chain(fact_id, assertion_id) AS (
		  SELECT id, resolved_assertion_id
		  FROM facts
		  WHERE account_id = $1
		  UNION
		  SELECT chain.fact_id, assertion.supersedes_id
		  FROM fact_chain chain
		  JOIN fact_assertions assertion ON assertion.id = chain.assertion_id
		  WHERE assertion.supersedes_id IS NOT NULL
		)
		SELECT EXISTS (
		  SELECT 1
		  FROM fact_candidates candidate
		  LEFT JOIN fact_assertions assertion ON assertion.id = candidate.decision_assertion_id
		  LEFT JOIN facts fact ON fact.id = candidate.resolved_fact_id
		  LEFT JOIN fact_subjects subject ON subject.id = fact.subject_id
		  WHERE candidate.account_id = $1
		    AND candidate.decision_assertion_id IS NOT NULL
		    AND (
		      candidate.status <> 'confirmed'
		      OR assertion.id IS NULL
		      OR fact.id IS NULL
		      OR assertion.fact_id IS DISTINCT FROM fact.id
		      OR assertion.account_id IS DISTINCT FROM candidate.account_id
		      OR assertion.realm_id IS DISTINCT FROM candidate.realm_id
		      OR assertion.asserted_by_agent_id IS DISTINCT FROM candidate.owner_agent_id
		      OR fact.account_id IS DISTINCT FROM candidate.account_id
		      OR fact.realm_id IS DISTINCT FROM candidate.realm_id
		      OR fact.owner_agent_id IS DISTINCT FROM candidate.owner_agent_id
		      OR subject.canonical_key IS DISTINCT FROM candidate.subject_key
		      OR fact.predicate IS DISTINCT FROM candidate.predicate
		      OR assertion.value_type IS DISTINCT FROM candidate.value_type
		      OR assertion.value IS DISTINCT FROM candidate.value
		      OR assertion.recurrence IS DISTINCT FROM candidate.recurrence
		      OR assertion.source_kind IS DISTINCT FROM 'inference'
		      OR assertion.source_ref IS DISTINCT FROM candidate.source_ref
		      OR assertion.confidence IS DISTINCT FROM candidate.confidence
		      OR assertion.observed_at IS DISTINCT FROM candidate.observed_at
		      OR assertion.confirmed_at IS NULL
		      OR assertion.valid_from IS DISTINCT FROM candidate.valid_from
		      OR assertion.valid_until IS DISTINCT FROM candidate.valid_until
		      OR assertion.supersedes_id IS DISTINCT FROM candidate.observed_assertion_id
		      OR NOT EXISTS (
		        SELECT 1 FROM fact_chain chain
		        WHERE chain.fact_id = fact.id
		          AND chain.assertion_id = assertion.id
		      )
		    )
		)`, accountID).Scan(&mismatch)
	if err != nil {
		return fmt.Errorf("validate imported fact decision assertions: %w", err)
	}
	if mismatch {
		return fmt.Errorf("%w: confirmed fact candidate does not match its decision assertion", ErrArchiveContent)
	}
	return nil
}

// validateImportedUsageRollups prevents a stale or edited projection from
// landing beside an otherwise valid event ledger. date_bin uses a fixed UTC
// epoch, matching usageBucketStart without depending on the DB session zone.
func validateImportedUsageRollups(ctx context.Context, tx pgx.Tx, accountID string) error {
	var mismatch bool
	err := tx.QueryRow(ctx, `
		WITH expected AS (
		  SELECT account_id, realm_id, agent_id, dimension, unit,
		         'hour'::text AS bucket,
		         date_bin('1 hour', occurred_at, '1970-01-01 00:00:00+00'::timestamptz) AS bucket_start,
		         sum(quantity)::bigint AS quantity, count(*)::bigint AS event_count
		  FROM usage_events WHERE account_id = $1
		  GROUP BY account_id, realm_id, agent_id, dimension, unit, bucket_start
		  UNION ALL
		  SELECT account_id, realm_id, agent_id, dimension, unit,
		         'day'::text AS bucket,
		         date_bin('1 day', occurred_at, '1970-01-01 00:00:00+00'::timestamptz) AS bucket_start,
		         sum(quantity)::bigint AS quantity, count(*)::bigint AS event_count
		  FROM usage_events WHERE account_id = $1
		  GROUP BY account_id, realm_id, agent_id, dimension, unit, bucket_start
		), actual AS (
		  SELECT account_id, realm_id, agent_id, dimension, unit, bucket,
		         bucket_start, quantity, event_count
		  FROM usage_rollups WHERE account_id = $1
		)
		SELECT EXISTS (
		  SELECT 1 FROM expected e
		  FULL OUTER JOIN actual a
		    USING (account_id, realm_id, agent_id, dimension, unit, bucket, bucket_start)
		  WHERE e.account_id IS NULL OR a.account_id IS NULL
		     OR e.quantity <> a.quantity OR e.event_count <> a.event_count
		)`, accountID).Scan(&mismatch)
	if err != nil {
		return fmt.Errorf("validate imported usage rollups: %w", err)
	}
	if mismatch {
		return fmt.Errorf("%w: usage rollups do not match usage events", ErrArchiveContent)
	}
	return nil
}

// insertProjected inserts one row using ONLY the columns the archive
// actually carries, so columns the archive omits take their destination
// DEFAULT — the additive-migration contract in export/upgrade.go. The set
// of legal column names per table is fixed at compile time (importColumns);
// any JSON key outside it is refused, so no attacker-chosen identifier
// reaches the SQL text.
//
// Concurrent same-account imports collide on the accounts primary-key
// insert: the loser's INSERT blocks on the winner's uncommitted tuple, and
// on the winner's commit fails with unique_violation (23505). That maps to
// ErrAccountExists here — a clean 409 for the retry — instead of falling to
// the generic 500 arm.
func insertProjected(ctx context.Context, tx pgxExec, table string, obj map[string]any, raw []byte) error {
	allowed := importColumns[table]
	keys := make([]string, 0, len(obj))
	for k := range obj {
		if !allowed[k] {
			return fmt.Errorf("%w: %s row has unknown column %q", ErrArchiveContent, table, k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic SQL text, useful for logs
	if len(keys) == 0 {
		return fmt.Errorf("%w: empty %s row", ErrArchiveContent, table)
	}
	colList := strings.Join(keys, ", ")
	// INSERT INTO t (c1,c2) SELECT c1,c2 FROM jsonb_populate_record(NULL::t, $1)
	// projects only the columns present in the JSON. Unlisted columns take
	// their DEFAULT — new NOT NULL DEFAULT columns land correctly without
	// needing an upgrader; new nullable-with-default columns land at their
	// default instead of the silent NULL a full-record insert would leave.
	stmt := fmt.Sprintf(
		`INSERT INTO %s (%s) SELECT %s FROM jsonb_populate_record(NULL::%s, $1::jsonb)`,
		table, colList, colList, table)
	if _, err := tx.Exec(ctx, stmt, raw); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && table == "accounts" {
			// A concurrent import (or a retry racing the winner) landed
			// this account first. Surface it as the same 409 an early
			// EXISTS-check would give.
			return ErrAccountExists
		}
		return fmt.Errorf("import %s row: %w", table, err)
	}
	return nil
}

// pgxExec is the minimal Exec surface insertProjected needs from a pgx.Tx —
// declared here so the helper can be unit-tested with an in-memory fake
// without pulling in a live database.
type pgxExec interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}
