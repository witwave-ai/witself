package main

import "strings"

const tenantSameAccountPathOnlyReason = "FU122d2-1: body-reference same-account boundary not exercised"

// These probes retain main's prerequisite. The memory-vector prerequisite is
// in the body rather than the URL: internal/store/memory_vector.go:317-319
// loads main's profile before the selected memory at :327-338.
func tenantSameAccountLimitations(pattern string, p *tenantProbe) {
	pathOnly := false
	switch pattern {
	case "POST /v1/email/{action}", "POST /v1/messages/{action}":
		pathOnly = p.Name == "complete-claim-body" || p.Name == "release-claim-body" || p.Name == "renew-claim-body"
	case "POST /v1/message-requests/{action}":
		pathOnly = p.Name == "complete-claim-body" || p.Name == "release-claim-body" || p.Name == "renew-claim-body" || p.Name == "selected-agent-body"
	case "POST /v1/memories/{memory}/supersede", "POST /v1/memory-evidence/{evidence}/resolution":
		pathOnly = p.Name == "memory-body" || p.Name == "message-body" || p.Name == "transcript-body"
	case "POST /v1/memory-curation-runs/{run}/plan":
		pathOnly = p.Name == "input-evidence-body" || p.Name == "source-transcript-body" || p.Name == "target-memory-body"
	case "POST /v1/memory-curation-runs/{run}/rollback":
		pathOnly = p.Name == "produced-head-body" || p.Name == "receipt-body"
	case "POST /v1/transcripts/{transcript}/entries", "POST /v1/transcripts/{transcript}/entries:batch":
		pathOnly = p.Name == "reply-target"
	case "POST /v1/vault/enrollments/{action}":
		pathOnly = p.Name == "consume-location-body" || p.Name == "receive-location-body"
	case "POST /v1/vault/rotations/{action}":
		pathOnly = p.Name == "stage-dek"
	case "POST /v1/memory-vectors":
		pathOnly = p.Name == "memory-body"
	}
	if pathOnly {
		for _, rule := range []*tenantSameAccountRule{p.SameRealm, p.OtherRealm} {
			if rule != nil {
				rule.PathOnly = tenantSameAccountPathOnlyReason
			}
		}
	}
}

// Independent pins prevent the limitation declarations from silently growing
// or shrinking. Body-only operations that reach their reference stay unmarked.
var tenantSameAccountPathOnly = map[string]string{
	"POST /v1/email/{action}/complete-claim-body":                     tenantSameAccountPathOnlyReason,
	"POST /v1/email/{action}/release-claim-body":                      tenantSameAccountPathOnlyReason,
	"POST /v1/email/{action}/renew-claim-body":                        tenantSameAccountPathOnlyReason,
	"POST /v1/memories/{memory}/supersede/memory-body":                tenantSameAccountPathOnlyReason,
	"POST /v1/memories/{memory}/supersede/message-body":               tenantSameAccountPathOnlyReason,
	"POST /v1/memories/{memory}/supersede/transcript-body":            tenantSameAccountPathOnlyReason,
	"POST /v1/memory-curation-runs/{run}/plan/input-evidence-body":    tenantSameAccountPathOnlyReason,
	"POST /v1/memory-curation-runs/{run}/plan/source-transcript-body": tenantSameAccountPathOnlyReason,
	"POST /v1/memory-curation-runs/{run}/plan/target-memory-body":     tenantSameAccountPathOnlyReason,
	"POST /v1/memory-curation-runs/{run}/rollback/produced-head-body": tenantSameAccountPathOnlyReason,
	"POST /v1/memory-curation-runs/{run}/rollback/receipt-body":       tenantSameAccountPathOnlyReason,
	"POST /v1/memory-evidence/{evidence}/resolution/memory-body":      tenantSameAccountPathOnlyReason,
	"POST /v1/memory-evidence/{evidence}/resolution/message-body":     tenantSameAccountPathOnlyReason,
	"POST /v1/memory-evidence/{evidence}/resolution/transcript-body":  tenantSameAccountPathOnlyReason,
	"POST /v1/memory-vectors/memory-body":                             tenantSameAccountPathOnlyReason,
	"POST /v1/message-requests/{action}/complete-claim-body":          tenantSameAccountPathOnlyReason,
	"POST /v1/message-requests/{action}/release-claim-body":           tenantSameAccountPathOnlyReason,
	"POST /v1/message-requests/{action}/renew-claim-body":             tenantSameAccountPathOnlyReason,
	"POST /v1/message-requests/{action}/selected-agent-body":          tenantSameAccountPathOnlyReason,
	"POST /v1/messages/{action}/complete-claim-body":                  tenantSameAccountPathOnlyReason,
	"POST /v1/messages/{action}/release-claim-body":                   tenantSameAccountPathOnlyReason,
	"POST /v1/messages/{action}/renew-claim-body":                     tenantSameAccountPathOnlyReason,
	"POST /v1/transcripts/{transcript}/entries/reply-target":          tenantSameAccountPathOnlyReason,
	"POST /v1/transcripts/{transcript}/entries:batch/reply-target":    tenantSameAccountPathOnlyReason,
	"POST /v1/vault/enrollments/{action}/consume-location-body":       tenantSameAccountPathOnlyReason,
	"POST /v1/vault/enrollments/{action}/receive-location-body":       tenantSameAccountPathOnlyReason,
	"POST /v1/vault/rotations/{action}/stage-dek":                     tenantSameAccountPathOnlyReason,
}

func tenantSameAccountSkipOracle(row tenantRow, p tenantProbe, rule *tenantSameAccountRule) bool {
	return p.ConfigBound != "" || (strings.HasPrefix(row.Pattern, "GET /v1/transcripts/") || strings.HasPrefix(row.Pattern, "POST /v1/transcripts/")) && tenantStatusIn(403, rule.Refuse)
}

// FU122-6 is the known transcript 403/404 distinction. Configuration-bound
// probes have no request-selectable owner and cannot build a missing-owner pair.
var tenantSameAccountOracleSkips = map[string]string{
	"GET /v1/transcripts/{transcript}/agent-read":                  "FU122-6: internal/store/transcript.go:554-556 checks owner after lookup.",
	"POST /v1/transcripts/{transcript}/entries/target":             "FU122-6: internal/store/transcript.go:249-250 checks owner after lookup.",
	"POST /v1/transcripts/{transcript}/entries/reply-target":       "FU122-6: internal/store/transcript.go:249-250 checks main's path owner first.",
	"POST /v1/transcripts/{transcript}/entries:batch/target":       "FU122-6: internal/store/transcript.go:249-250 checks owner after lookup.",
	"POST /v1/transcripts/{transcript}/entries:batch/reply-target": "FU122-6: internal/store/transcript.go:249-250 checks main's path owner first.",
	"POST /v1/email/retry-canary:arm/configured-owner":             "internal/server/agent_email.go:1206-1207 checks the configured identity; no request owner selector.",
	"POST /v1/email/retry-canary:status/configured-owner":          "internal/server/agent_email.go:1206-1207 checks the configured identity; no request owner selector.",
}
