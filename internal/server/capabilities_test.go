package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// Routes are probed through apiMux, including dispatch inside shared action
// patterns. The expected flags come from actual dispatch, independently of the
// configuration predicates used by the discovery handler.
func TestCapabilitiesMatchRegisteredRoutes(t *testing.T) {
	type routeFamily struct {
		callbacks string
		routes    []string
	}
	families := map[string]routeFamily{
		"facts": {
			"SetFact GetFactLimitStatus DeleteFact GetFact ListFacts GetFactHistory UpcomingFacts ProposeFact ListFactCandidates GetFactCandidate ConfirmFactCandidate RejectFactCandidate UpsertFactSubject AddFactSubjectAlias ListFactSubjects",
			[]string{"POST /v1/facts", "GET /v1/facts:status", "DELETE /v1/facts", "DELETE /v1/facts/fact_1", "GET /v1/facts", "GET /v1/facts/fact_1/history", "GET /v1/fact-occurrences", "POST /v1/fact-candidates", "GET /v1/fact-candidates", "GET /v1/fact-candidates/candidate_1", "POST /v1/fact-candidates/candidate_1:confirm", "POST /v1/fact-candidates/candidate_1:reject", "PUT /v1/fact-subjects/subject_1", "POST /v1/fact-subjects/subject_1/aliases", "GET /v1/fact-subjects"},
		},
		"self_digest": {"", []string{"GET /v1/self"}},
		"memories": {
			"CaptureMemory ListMemories GetMemory GetMemoryLimitStatus GetMemoryHistory AdjustMemory SupersedeMemory ForgetMemory RestoreMemory ReactivateMemory ResolveMemoryEvidence DeleteMemory RecallMemories",
			[]string{"POST /v1/memories", "GET /v1/memories", "GET /v1/memories/mem_1", "GET /v1/memories:status", "GET /v1/memories/mem_1/history", "PATCH /v1/memories/mem_1", "POST /v1/memories/mem_1/supersede", "POST /v1/memories/mem_1:forget", "POST /v1/memories/mem_1:restore", "POST /v1/memories/mem_1:reactivate", "POST /v1/memory-evidence/evidence_1/resolution", "DELETE /v1/memories/mem_1", "POST /v1/memories:recall"},
		},
		"memory_recall":           {"RecallMemories", []string{"POST /v1/memories:recall"}},
		"memory_supersede":        {"SupersedeMemory", []string{"POST /v1/memories/mem_1/supersede"}},
		"memory_permanent_delete": {"DeleteMemory", []string{"DELETE /v1/memories/mem_1"}},
		"memory_vector_profiles": {
			"CreateMemoryVectorProfile ListMemoryVectorProfiles PutMemoryVector",
			[]string{"POST /v1/memory-vector-profiles", "GET /v1/memory-vector-profiles", "POST /v1/memory-vectors"},
		},
		"opportunistic_curation": {
			"RequestMemoryCuration ListMemoryCurationRequests GetMemoryCurationRequest StartMemoryCuration GetMemoryCurationRun GetMemoryCurationRunInputs GetMemoryCurationPlan RenewMemoryCuration PlanMemoryCuration ApplyMemoryCuration CancelMemoryCuration AbandonMemoryCuration RollbackMemoryCuration GetMemoryCurationStatus",
			[]string{"GET /v1/memory-curation-preflight", "POST /v1/memory-curation-requests", "GET /v1/memory-curation-requests", "GET /v1/memory-curation-requests/request_1", "POST /v1/memory-curation-requests/request_1/start", "GET /v1/memory-curation-runs/run_1", "GET /v1/memory-curation-runs/run_1/inputs", "GET /v1/memory-curation-runs/run_1/plan", "POST /v1/memory-curation-runs/run_1/renew", "POST /v1/memory-curation-runs/run_1/plan", "POST /v1/memory-curation-runs/run_1/apply", "POST /v1/memory-curation-runs/run_1/cancel", "POST /v1/memory-curation-runs/run_1/abandon", "POST /v1/memory-curation-runs/run_1/rollback", "GET /v1/memory-curation-status"},
		},
		"transcripts": {
			"CreateTranscript AppendTranscriptEntry AppendTranscriptEntries ListTranscripts GetTranscript GetTranscriptPage",
			[]string{"POST /v1/transcripts", "GET /v1/transcripts", "GET /v1/transcripts/transcript_1", "POST /v1/transcripts/transcript_1/entries", "POST /v1/transcripts/transcript_1/entries:batch"},
		},
		"messaging": {
			"SendMessage ListMessages ReadMessage AckMessage ReplyMessage ClaimMessage RenewMessageClaim ReleaseMessageClaim CompleteMessage",
			[]string{"POST /v1/messages", "GET /v1/messages", "POST /v1/messages:listen", "POST /v1/messages/message_1:read", "POST /v1/messages/message_1:ack", "POST /v1/messages/message_1:reply", "POST /v1/messages/message_1:claim", "POST /v1/messages/message_1:renew", "POST /v1/messages/message_1:release", "POST /v1/messages/message_1:complete"},
		},
		"message_listen": {"ListMessages", []string{"POST /v1/messages:listen"}},
		"message_reply":  {"ReplyMessage", []string{"POST /v1/messages/message_1:reply"}},
		"message_processing": {
			"ClaimMessage RenewMessageClaim ReleaseMessageClaim CompleteMessage",
			[]string{"POST /v1/messages/message_1:claim", "POST /v1/messages/message_1:renew", "POST /v1/messages/message_1:release", "POST /v1/messages/message_1:complete"},
		},
		"message_requests": {
			"CreateMessageRequest ListMessageRequests GetMessageRequest OfferMessageRequest DeclineMessageRequest SelectMessageRequest CancelMessageRequest ClaimMessageRequest RenewMessageRequest ReleaseMessageRequest CompleteMessageRequest",
			[]string{"POST /v1/message-requests", "GET /v1/message-requests", "GET /v1/message-requests/request_1", "POST /v1/message-requests/request_1:offer", "POST /v1/message-requests/request_1:decline", "POST /v1/message-requests/request_1:select", "POST /v1/message-requests/request_1:cancel", "POST /v1/message-requests/request_1:claim", "POST /v1/message-requests/request_1:renew", "POST /v1/message-requests/request_1:release", "POST /v1/message-requests/request_1:complete"},
		},
		"avatars": {
			"GetSelfAvatar GetSelfAvatarHistory GetSelfAvatarVersion GetSelfAvatarStyle ProposeSelfAvatar ActivateSelfAvatar RollbackSelfAvatar ResetSelfAvatar ReportSelfAvatarGenerationFailure GetAgentAvatar GetAgentAvatarHistory GetAgentAvatarVersion ProposeAgentAvatar ActivateAgentAvatar RejectAgentAvatar RollbackAgentAvatar ResetAgentAvatar UpdateAgentAvatarPolicy UpdateAgentAvatarQuota GetRealmAvatarStyle CreateRealmAvatarStyleVersion",
			[]string{"GET /v1/self/avatar", "GET /v1/self/avatar/history", "GET /v1/self/avatar/versions/1", "GET /v1/self/avatar/style", "POST /v1/self/avatar/proposals", "POST /v1/self/avatar:activate", "POST /v1/self/avatar:rollback", "POST /v1/self/avatar:reset", "POST /v1/self/avatar:generation-failed", "GET /v1/agents/agent_1/avatar", "GET /v1/agents/agent_1/avatar/history", "GET /v1/agents/agent_1/avatar/versions/1", "POST /v1/agents/agent_1/avatar/proposals", "POST /v1/agents/agent_1/avatar:activate", "POST /v1/agents/agent_1/avatar:reject", "POST /v1/agents/agent_1/avatar:rollback", "POST /v1/agents/agent_1/avatar:reset", "PATCH /v1/agents/agent_1/avatar-policy", "PATCH /v1/agents/agent_1/avatar-quota", "GET /v1/realms/realm_1/avatar-style", "POST /v1/realms/realm_1/avatar-style/versions"},
		},
		"secrets": {
			"GetCurrentVaultKey RegisterVaultKey CreateVaultKeyEnrollment ListVaultKeyEnrollments GetVaultKeyEnrollment ApproveVaultKeyEnrollment ReceiveVaultKeyEnrollment ConsumeVaultKeyEnrollment CancelVaultKeyEnrollment StartVaultKeyRotation GetOpenVaultKeyRotation GetVaultKeyRotation ListVaultKeyRotationItems StageVaultKeyRotation CommitVaultKeyRotation CancelVaultKeyRotation CreateSecret GetSecretLimitStatus ListSecrets GetSecret ArchiveSecret RestoreSecret DeleteSecret AccessSecretField",
			[]string{"GET /v1/vault/key-epochs/current", "POST /v1/vault/key-epochs", "POST /v1/vault/enrollments", "GET /v1/vault/enrollments", "GET /v1/vault/enrollments/enr_abcdefghijklmnop", "POST /v1/vault/enrollments/enr_abcdefghijklmnop:approve", "POST /v1/vault/enrollments/enr_abcdefghijklmnop:receive", "POST /v1/vault/enrollments/enr_abcdefghijklmnop:consume", "POST /v1/vault/enrollments/enr_abcdefghijklmnop:cancel", "POST /v1/vault/rotations", "GET /v1/vault/rotations/open", "GET /v1/vault/rotations/vkr_abcdefghijklmnop", "GET /v1/vault/rotations/vkr_abcdefghijklmnop/items", "POST /v1/vault/rotations/vkr_abcdefghijklmnop:stage", "POST /v1/vault/rotations/vkr_abcdefghijklmnop:commit", "POST /v1/vault/rotations/vkr_abcdefghijklmnop:cancel", "POST /v1/secrets", "GET /v1/secrets:status", "GET /v1/secrets", "GET /v1/secrets/sec_abcdefghijklmnop", "POST /v1/secrets/sec_abcdefghijklmnop:archive", "POST /v1/secrets/sec_abcdefghijklmnop:restore", "POST /v1/secrets/sec_abcdefghijklmnop:delete", "POST /v1/secrets/sec_abcdefghijklmnop/fields/fld_abcdefghijklmnop:access"},
		},
		"agent_email_send":         {"QueueAgentEmail", []string{"POST /v1/email:send"}},
		"agent_email_reply":        {"ReplyAgentEmail", []string{"POST /v1/email/email_1:reply"}},
		"agent_email_sent_history": {"ListAgentEmailOutbox GetAgentEmailOutbound", []string{"GET /v1/email/sent", "GET /v1/email/sent/email_1"}},
		"audit":                    {"ListAccountEvents ListAdminEventsAll", []string{"GET /v1/account/events", "POST /v1/events/admin:list"}},
		"automatic_capture":        {"", []string{"POST /v1/remember"}},
		"scheduled_curation":       {"", nil},
		"policies":                 {"", []string{"GET /v1/policies"}},
		"groups":                   {"", []string{"GET /v1/groups"}},
	}
	callbacks := map[string]bool{}
	for _, family := range families {
		for _, name := range strings.Fields(family.callbacks) {
			callbacks[name] = true
		}
	}
	all := make([]string, 0, len(callbacks))
	for name := range callbacks {
		all = append(all, name)
	}
	cases := map[string]Config{
		"disabled":             {},
		"authentication_only":  capabilityTestConfig(t, nil),
		"all_routes":           capabilityTestConfig(t, all),
		"fact_read_pair":       capabilityTestConfig(t, []string{"GetFact", "ListFacts"}),
		"fact_candidate_pair":  capabilityTestConfig(t, []string{"ConfirmFactCandidate", "RejectFactCandidate"}),
		"vector_recall_routes": capabilityTestConfig(t, []string{"RecallMemories", "PutMemoryVector"}),
	}
	noAuth := capabilityTestConfig(t, all)
	noAuth.Authenticate, noAuth.AuthenticatePrincipal, noAuth.ProvisionToken = nil, nil, ""
	cases["callbacks_without_authentication"] = noAuth
	principalOnly := capabilityTestConfig(t, all)
	principalOnly.Authenticate, principalOnly.ProvisionToken = nil, ""
	cases["principal_authentication_only"] = principalOnly
	operatorOnly := capabilityTestConfig(t, all)
	operatorOnly.AuthenticatePrincipal, operatorOnly.ProvisionToken = nil, ""
	cases["operator_authentication_only"] = operatorOnly
	provisionOnly := capabilityTestConfig(t, all)
	provisionOnly.Authenticate, provisionOnly.AuthenticatePrincipal = nil, nil
	cases["provision_authentication_only"] = provisionOnly
	for name := range callbacks {
		cases["only_"+name] = capabilityTestConfig(t, []string{name})
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			handler := apiMux(cfg)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
			var out capabilities
			if err := json.NewDecoder(recorder.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
			registered := map[string]bool{}
			for feature, family := range families {
				registered[feature] = false
				for _, route := range family.routes {
					method, path, _ := strings.Cut(route, " ")
					req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
					req.Header.Set("Authorization", "Bearer capability-test")
					req.Header.Set("Idempotency-Key", "capability-test")
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, req)
					if req.Pattern != "" && response.Code != http.StatusNotFound && response.Code != http.StatusMethodNotAllowed {
						registered[feature] = true
					}
				}
			}
			registered["transcript_capture"] = registered["transcripts"]
			registered["client_vector_recall"] = registered["memory_recall"] && registered["memory_vector_profiles"]
			registered["semantic_recall"] = registered["client_vector_recall"]
			if len(out.Features) != len(families)+3 {
				t.Fatalf("feature vocabulary changed without route coverage: got %d, want %d", len(out.Features), len(families)+3)
			}
			for feature := range registered {
				if _, ok := out.Features[feature]; !ok {
					t.Errorf("capability payload omitted feature %q", feature)
				}
			}
			for feature, state := range out.Features {
				if state.Supported != registered[feature] {
					t.Errorf("%s supported = %t, registered family route = %t", feature, state.Supported, registered[feature])
				}
				if state.Supported && state.Reason != "" || !state.Supported && state.Reason != "not_implemented" {
					t.Errorf("%s has inconsistent supported/reason fields: %+v", feature, state)
				}
			}
		})
	}
}

// Stubs only exercise dispatch and return a conflict if reached. No database,
// listener, background worker, or successful domain mutation is involved.
func capabilityTestConfig(t *testing.T, callbacks []string) Config {
	t.Helper()
	cfg := Config{
		Authenticate: func(context.Context, string) (string, string, string, bool, error) {
			return "operator_1", "account_1", "active", true, nil
		},
		AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) {
			return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "account_1", RealmID: "realm_1", AccountStatus: "active"}, true, nil
		},
		ProvisionToken: "capability-test",
	}
	value := reflect.ValueOf(&cfg).Elem()
	for _, name := range callbacks {
		field := value.FieldByName(name)
		if !field.IsValid() || field.Kind() != reflect.Func {
			t.Fatalf("unknown callback %q", name)
		}
		callbackType := field.Type()
		field.Set(reflect.MakeFunc(callbackType, func([]reflect.Value) []reflect.Value {
			results := make([]reflect.Value, callbackType.NumOut())
			for i := range results {
				results[i] = reflect.Zero(callbackType.Out(i))
			}
			results[len(results)-1] = reflect.ValueOf(ErrConflict)
			return results
		}))
	}
	return cfg
}
