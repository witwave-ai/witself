package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/store"
)

// tenantSeedObjects uses the child HTTP API before the first fingerprint. Every
// generated object ID and every free-text canary remains in the leak inventory.
func tenantSeedObjects(t *testing.T, rt *tenantRuntime) {
	t.Helper()
	for _, side := range []tenantSide{sideA, sideB} {
		tenantSeedFacts(t, rt, side)
		tenantSeedTranscripts(t, rt, side)
		tenantSeedTickets(t, rt, side)
		tenantSeedEmailControls(t, rt, side)
		tenantSeedVault(t, rt, side)
		tenantSeedMessages(t, rt, side)
		tenantSeedMemories(t, rt, side)
	}
	// A-own path objects accompany exactly one foreign body reference. They
	// are separate from the never-issued slots used by ordinary own probes.
	for key, value := range rt.f.bySide[sideA] {
		rt.f.ownValues[key] = value
	}
}

// tenantSeedFacts follows TestFactSubjectRoutes (internal/server/fact_subject_test.go)
// and the JSON fact request shapes in internal/server/fact_test.go. The second
// assertion gives history real content; the temporal sibling makes EmptyList's
// owner control positive.
func tenantSeedFacts(t *testing.T, rt *tenantRuntime, side tenantSide) {
	t.Helper()
	v := rt.f.bySide[side]
	for _, key := range []string{"fact.subject", "fact.subject.alias"} {
		var out struct {
			Subject store.FactSubject `json:"subject"`
		}
		tenantSeedHTTP(t, rt, side, credAgent, "PUT", "/v1/fact-subjects/"+url.PathEscape(v[key]), map[string]any{"display_name": tenantDomainCanary(v, side, key+".display")}, &out)
		v[key], v[key+".id"] = out.Subject.CanonicalKey, out.Subject.ID
	}
	for _, revision := range []string{"initial", "current"} {
		var out struct {
			Fact store.Fact `json:"fact"`
		}
		tenantSeedHTTP(t, rt, side, credAgent, "POST", "/v1/facts", map[string]any{
			"subject": v["fact.subject"], "predicate": "matrix/value", "value_type": "string",
			"value": tenantDomainCanary(v, side, "fact."+revision), "source_kind": "agent",
		}, &out)
		v["fact"], v["fact.assertion."+revision], v["fact-subject-id"] = out.Fact.ID, out.Fact.ResolvedAssertionID, out.Fact.SubjectID
	}
	// Tomorrow remains inside the default upcoming window at every time of day.
	var date struct {
		Fact store.Fact `json:"fact"`
	}
	tenantSeedHTTP(t, rt, side, credAgent, "POST", "/v1/facts", map[string]any{
		"subject": v["fact.subject"], "predicate": "matrix/upcoming", "value_type": "date",
		"value":       time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02"),
		"source_kind": "agent", "source_ref": tenantDomainCanary(v, side, "fact.temporal.source"),
	}, &date)
	v["fact.temporal"], v["fact.temporal.assertion"] = date.Fact.ID, date.Fact.ResolvedAssertionID
	for _, key := range []string{"fact.candidate", "fact.candidate.confirm", "fact.candidate.reject"} {
		var out struct {
			Candidate store.FactCandidate `json:"candidate"`
		}
		tenantSeedHTTP(t, rt, side, credAgent, "POST", "/v1/fact-candidates", map[string]any{
			"subject": v["fact.subject"], "predicate": "matrix/" + key, "value_type": "string",
			"value": tenantDomainCanary(v, side, key+".value"), "source_ref": tenantDomainCanary(v, side, key+".source"),
			"reason": tenantDomainCanary(v, side, key+".reason"),
		}, &out)
		v[key] = out.Candidate.ID
	}
}

// tenantSeedTranscripts follows TestTranscriptAgentWritesAndOperatorReads in
// internal/server/transcript_test.go. Each reply entry belongs to its dedicated
// append/batch transcript, giving B a valid parent reference. Production writes
// also create authentic usage and activity records.
func tenantSeedTranscripts(t *testing.T, rt *tenantRuntime, side tenantSide) {
	t.Helper()
	v := rt.f.bySide[side]
	for _, item := range []struct{ transcriptKey, entryKey string }{
		{"transcript", "transcript.read.entry"}, {"transcript.append", "transcript.entry"}, {"transcript.batch", "transcript.entry.batch"},
	} {
		var tr struct {
			Transcript store.Transcript `json:"transcript"`
		}
		tenantSeedHTTP(t, rt, side, credAgent, "POST", "/v1/transcripts", map[string]any{
			"title": tenantDomainCanary(v, side, item.transcriptKey+".title"), "metadata": map[string]any{},
		}, &tr)
		v[item.transcriptKey] = tr.Transcript.ID
		var entry struct {
			Entry store.TranscriptEntry `json:"entry"`
		}
		tenantSeedHTTP(t, rt, side, credAgent, "POST", "/v1/transcripts/"+url.PathEscape(tr.Transcript.ID)+"/entries", map[string]any{
			"role": "user", "body": tenantDomainCanary(v, side, item.entryKey+".body"),
		}, &entry)
		v[item.entryKey] = entry.Entry.ID
	}
}

// tenantSeedTickets follows TestSupportTicketRequestBodyPreservesValidInputs in
// internal/server/support_request_bounds_test.go. Store.ChangeAdminTicketState
// prepares the dedicated close target: a valid system-token HTTP body is forbidden
// in this matrix, so the admin-only prerequisite uses the store directly.
func tenantSeedTickets(t *testing.T, rt *tenantRuntime, side tenantSide) {
	t.Helper()
	v := rt.f.bySide[side]
	for _, key := range []string{"ticket", "ticket.reply", "ticket.state"} {
		var out struct {
			Ticket  store.Ticket        `json:"ticket"`
			Message store.TicketMessage `json:"initial_message"`
		}
		tenantSeedHTTP(t, rt, side, credOperator, "POST", "/v1/support/tickets", map[string]any{
			"subject": tenantDomainCanary(v, side, key+".subject"), "category": "other", "priority": "normal", "body": tenantDomainCanary(v, side, key+".body"),
		}, &out)
		v[key], v[key+".message"] = out.Ticket.ID, out.Message.ID
		if key == "ticket.state" {
			_, err := rt.st.ChangeAdminTicketState(context.Background(), store.ChangeAdminStateInput{
				AccountID: v["account"], AdminHandle: "tm-seeder", TicketID: out.Ticket.ID, NewState: store.TicketStateResolved,
			})
			tenantSeedObjectError(t, "ChangeAdminTicketState", err)
		}
	}
}

func tenantSeedHTTP(t *testing.T, rt *tenantRuntime, side tenantSide, cred tenantCred, method, path string, body any, out any) {
	t.Helper()
	raw, err := json.Marshal(body)
	tenantSeedObjectError(t, "marshal HTTP seed", err)
	req := tenantRequest{Method: method, Path: path, Body: raw, Headers: http.Header{"Content-Type": []string{"application/json"}}}
	response := rt.send(t, tenantRow{}, tenantProbe{}, req, rt.creds[side][cred])
	if response.Status < 200 || response.Status >= 300 {
		t.Fatalf("tenant HTTP seed %s failed with status %d", method, response.Status)
	}
	tenantSeedObjectError(t, "decode HTTP seed", json.Unmarshal(response.Body, out))
}

func tenantDomainCanary(values map[string]string, side tenantSide, key string) string {
	value := tenantSyntheticValues(side, "domain-seed:"+key)["canary.fact.value"]
	values["canary.seed."+key] = value
	return value
}

func tenantSeedObjectError(t *testing.T, operation string, err error) {
	t.Helper()
	if err != nil {
		// Store errors can include caller content; fixture failures identify
		// the operation and error type without printing private values.
		t.Fatalf("tenant seed %s failed (%T)", operation, err)
	}
}
