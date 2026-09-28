package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The importer must accept every request shape the server itself writes under
// the reserved automatic coalescing key, and nothing else.
func TestValidateImportedCurationRequestContentAutomaticShapes(t *testing.T) {
	defaultScope, err := normalizeMemoryCurationScope(MemoryCurationScope{})
	if err != nil {
		t.Fatal(err)
	}
	scopeJSON, _ := json.Marshal(defaultScope)
	var scope map[string]any
	_ = json.Unmarshal(scopeJSON, &scope)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	base := func(key, trigger string) map[string]any {
		return map[string]any{
			"scope": scope, "coalescing_key": automaticMemoryCurationCoalescingKey,
			"trigger_reason": trigger, "state": "queued", "attempt_count": float64(0),
			"max_attempts": float64(5), "read_only_replay": false, "priority": float64(0),
			"idempotency_key": key, "request_hash": strings.Repeat("a", 64),
			"due_at": now, "created_at": now, "updated_at": now,
		}
	}
	cases := []struct {
		name    string
		key     string
		trigger string
		ok      bool
	}{
		{"source-trigger", automaticMemoryCurationKeyPrefix + "memory:" + strings.Repeat("b", 64), "memory_changed", true},
		{"generation-follow-up", memoryCurationFollowUpKeyPrefix + "mcrn_followup", memoryCurationFollowUpTrigger, true},
		{"source-backlog-follow-up", memoryCurationFollowUpKeyPrefix + "mcrn_followup", memoryCurationSourceBacklogTrigger, true},
		{"follow-up-key-with-source-trigger", memoryCurationFollowUpKeyPrefix + "mcrn_followup", "memory_changed", false},
		{"client-shaped-key", "client:" + strings.Repeat("c", 32), memoryCurationFollowUpTrigger, false},
		{"empty-key", "", memoryCurationFollowUpTrigger, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateImportedCurationRequestContent(base(tc.key, tc.trigger))
			if tc.ok && err != nil {
				t.Fatalf("server-written shape rejected: %v", err)
			}
			if !tc.ok && (err == nil || !strings.Contains(err.Error(), "reserved automatic request shape is invalid")) {
				t.Fatalf("non-server shape accepted or misclassified: %v", err)
			}
		})
	}
}
