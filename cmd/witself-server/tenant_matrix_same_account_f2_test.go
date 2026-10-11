package main

// tenantSameAccountF2 is deliberately keyed by both pattern and probe name.
// A new foreign probe receives no classification until its own scope is reviewed.
func tenantSameAccountF2(row *tenantRow) {
	for i := range row.Probes {
		p := &row.Probes[i]
		if rule, ok := tenantSameAccountF2Refusals[row.Pattern+"/"+p.Name]; ok {
			same, other := rule, rule
			p.SameRealm, p.OtherRealm = &same, &other
		}
		switch row.Pattern + "/" + p.Name {
		case "POST /v1/messages/recipient":
			p.SameRealm = &tenantSameAccountRule{
				Mode: "allowed", Want: []int{200, 201},
				Reason: "internal/store/message.go:1603-1605 resolves any live agent in the caller's account and realm, including the caller.",
				AllowedKeys: tenantF1AllowedKeys("internal/store/message.go:1054-1068 returns the caller's account, realm and sender identity, the resolved direct recipient, and submitted subject/body.",
					"account", "realm", "peer_agent", "peer-name", "message_subject", "message_body"),
			}
			p.OtherRealm = &tenantSameAccountRule{Mode: "refuse", Reason: "internal/store/message.go:1603-1605 requires the recipient's realm to match the caller."}
		case "POST /v1/messages/recipient-set":
			p.SameRealm = &tenantSameAccountRule{
				Mode: "allowed", Want: []int{200, 201},
				Reason: "internal/store/message.go:1690-1692 resolves explicit recipients in the caller's account and realm without excluding the caller.",
				AllowedKeys: tenantF1AllowedKeys("internal/store/message.go:1054-1068 returns the caller's account, realm and sender identity with the submitted body; the agents audience projects only its count.",
					"account", "realm", "peer_agent", "peer-name", "message_set_body"),
			}
			p.OtherRealm = &tenantSameAccountRule{Mode: "refuse", Reason: "internal/store/message.go:1690-1692 requires every explicit recipient in the caller's realm."}
		case "POST /v1/memories/message-evidence":
			p.SameRealm = &tenantSameAccountRule{
				Mode: "allowed", Want: []int{200, 201},
				Reason: "internal/store/memory.go:1736-1737 permits message evidence for either sender or recipient; peer sent the seeded message to main.",
				AllowedKeys: map[string]string{
					"account":                 "cmd/witself-server/memory.go:493-496 returns the captured memory's caller-owned account, realm and owner identity.",
					"realm":                   "cmd/witself-server/memory.go:493-496 returns the captured memory's caller-owned account, realm and owner identity.",
					"peer_agent":              "cmd/witself-server/memory.go:495-497 returns the caller as owner and original author.",
					"peer-name":               "cmd/witself-server/memory.go:495-497 projects the authenticated owner's name.",
					"capture_message_content": "cmd/witself-server/memory.go:499 returns the submitted memory content.",
					"message":                 "internal/store/memory.go:1274 and cmd/witself-server/memory.go:560 return the validated message evidence source ID.",
				},
			}
			p.OtherRealm = &tenantSameAccountRule{Mode: "refuse", Refuse: []int{400}, Reason: "internal/store/memory.go:1736-1737 requires the evidence message's realm and sender or recipient; internal/server/memory.go:1004 maps invalid evidence to 400."}
		case "GET /v1/message-requests/{request}/request":
			p.SameRealm = &tenantSameAccountRule{
				Mode: "allowed", Want: []int{200, 201},
				Reason:      "internal/store/message_request.go:634-637 permits an immutable candidate to inspect the request; peer is a candidate on main's base request.",
				AllowedKeys: tenantSameAccountRequestDetail(),
			}
			p.OtherRealm = &tenantSameAccountRule{Mode: "refuse", Reason: "internal/store/message_request.go:636-637 requires the request's realm and an authorized coordinator or candidate."}
		case "GET /v1/messages/own", "POST /v1/messages:listen/own":
			p.SameAccountAllowed = tenantSameAccountMessageMetadata()
		case "GET /v1/message-requests/own":
			p.SameAccountAllowed = tenantSameAccountRequestMetadata()
		case "GET /v1/memory-curation-preflight/own":
			p.SameAccountAllowed = map[string]string{
				"account":            "internal/server/memory_curation.go:282 returns the authenticated caller's account ID.",
				"realm":              "internal/server/memory_curation.go:282 returns the authenticated caller's realm ID.",
				"peer_agent":         "internal/server/memory_curation.go:282 returns the authenticated caller's agent ID.",
				"peer-name":          "internal/server/memory_curation.go:282 returns the authenticated caller's agent name.",
				"seed.token.peer.id": "internal/server/memory_curation.go:285 returns the authenticated caller's token ID, never the token value.",
			}
			p.OtherRealmAllowed = map[string]string{
				"account":             "internal/server/memory_curation.go:282 returns the authenticated caller's account ID.",
				"realm2":              "internal/server/memory_curation.go:282 returns the authenticated caller's realm ID.",
				"other_agent":         "internal/server/memory_curation.go:282 returns the authenticated caller's agent ID.",
				"other-name":          "internal/server/memory_curation.go:282 returns the authenticated caller's agent name.",
				"seed.token.other.id": "internal/server/memory_curation.go:285 returns the authenticated caller's token ID, never the token value.",
			}
		case "GET /v1/memory-curation-requests/own", "GET /v1/memory-curation-status/own":
			p.SameAccountAllowed = map[string]string{
				"account":    "internal/store/memory_curation_list.go:76 and internal/store/memory_curation.go:2448 scope the projection at memory_curation.go:363-368, which returns the owner-scoped lane/request account ID.",
				"realm":      "internal/store/memory_curation_list.go:76 and internal/store/memory_curation.go:2448 scope the projection at memory_curation.go:363-368, which returns the owner-scoped lane/request realm ID.",
				"peer_agent": "internal/store/memory_curation_list.go:76 and internal/store/memory_curation.go:2448 scope the projection at memory_curation.go:363-368, which returns the owner-scoped lane/request owner ID.",
			}
			p.OtherRealmAllowed = map[string]string{
				"account":     "internal/store/memory_curation_list.go:76 and internal/store/memory_curation.go:2448 scope the projection at memory_curation.go:363-368, which returns the owner-scoped lane/request account ID.",
				"realm2":      "internal/store/memory_curation_list.go:76 and internal/store/memory_curation.go:2448 scope the projection at memory_curation.go:363-368, which returns the owner-scoped lane/request realm ID.",
				"other_agent": "internal/store/memory_curation_list.go:76 and internal/store/memory_curation.go:2448 scope the projection at memory_curation.go:363-368, which returns the owner-scoped lane/request owner ID.",
			}
		case "GET /v1/email/address/own":
			p.SameAccountAllowed = map[string]string{
				"account":               "internal/store/agent_email.go:2385,2564 returns the caller-owned mailbox's account ID.",
				"realm":                 "internal/store/agent_email.go:2385,2564 returns the caller-owned mailbox's shared realm ID.",
				"peer_agent":            "internal/store/agent_email.go:2385,2564 returns the caller-owned mailbox's owner ID.",
				"peer-name":             "internal/store/agent_email.go:645 derives peer's mailbox segment from peer's own agent name.",
				"seed.peer_agent.email": "internal/store/agent_email.go:2385,2565 returns peer's own mailbox address.",
			}
			p.OtherRealmAllowed = map[string]string{
				"account":                "internal/store/agent_email.go:2385,2564 returns the caller-owned mailbox's account ID.",
				"realm2":                 "internal/store/agent_email.go:2385,2564 returns other's own realm ID.",
				"other_agent":            "internal/store/agent_email.go:2385,2564 returns the caller-owned mailbox's owner ID.",
				"other-name":             "internal/store/agent_email.go:645 derives other's mailbox segment from other's own agent name.",
				"seed.other_agent.email": "internal/store/agent_email.go:2385,2565 returns other's own mailbox address.",
			}
		}
	}
}

// Body-reference requests retain main's path prerequisite. Another principal
// therefore normally fails the path owner check before reaching the body field.
var tenantSameAccountF2Refusals = func() map[string]tenantSameAccountRule {
	m := map[string]tenantSameAccountRule{}
	add := func(pattern string, names []string, status int, reason string) {
		for _, name := range names {
			rule := tenantSameAccountRule{Mode: "refuse", Reason: reason}
			if status != 404 {
				rule.Refuse = []int{status}
			}
			m[pattern+"/"+name] = rule
		}
	}
	add("GET /v1/vault/enrollments/{enrollment}", []string{"enrollment"}, 404, "internal/store/vault_enrollment.go:797 scopes enrollment lookup by account, realm and owner agent.")
	add("POST /v1/vault/enrollments/{action}", []string{"approve", "receive", "receive-location-body", "consume", "consume-location-body", "cancel"}, 404, "internal/store/vault_enrollment.go:797 scopes every enrollment action's prerequisite to its owner agent.")
	add("POST /v1/vault/rotations", []string{"source-key"}, 409, "internal/store/vault_key_rotation_mutation.go:89-94 refuses a missing or mismatched caller key; internal/server/secret.go:646-653 maps it to 409.")
	add("GET /v1/vault/rotations/{rotation}", []string{"rotation"}, 404, "internal/store/vault_key_rotation_query.go:110 scopes rotations to the caller's owner agent.")
	add("GET /v1/vault/rotations/{rotation}/items", []string{"rotation"}, 404, "internal/store/vault_key_rotation_query.go:177 loads the owner-scoped rotation before its items.")
	add("POST /v1/vault/rotations/{action}", []string{"stage", "stage-dek", "commit", "cancel"}, 404, "internal/store/vault_key_rotation_query.go:146 locks the rotation only for its owner agent.")
	add("POST /v1/secrets", []string{"wrapping-key"}, 409, "internal/store/secret_mutation.go:203-209 refuses a missing or mismatched caller key; internal/server/secret.go:646-649 maps it to 409.")
	add("GET /v1/secrets/{secret}", []string{"secret"}, 404, "internal/store/secret_query.go:88-90 scopes secrets to their owner agent.")
	add("POST /v1/secrets/{action}", []string{"archive", "restore", "delete"}, 404, "internal/store/secret_query.go:88-90 scopes lifecycle prerequisites to their owner agent.")
	add("POST /v1/secrets/{secret}/fields/{action}", []string{"access"}, 404, "internal/store/secret_query.go:88-90 scopes the secret prerequisite to its owner agent.")
	add("POST /v1/memories", []string{"transcript-evidence", "memory-evidence"}, 400, "internal/store/memory.go:1711-1724 checks transcript and memory evidence owners; internal/server/memory.go:1004 returns invalid-evidence 400.")
	add("GET /v1/memories", []string{"owner-filter"}, 403, "internal/store/memory.go:2141-2142 forbids an owner filter different from the caller; internal/server/memory.go:1006 maps it to 403. FU122-6 records this 403 classification; the never-issued comparison still runs for this owner filter.")
	add("POST /v1/memories:recall", []string{"vector-profile"}, 404, "internal/store/memory_vector.go:390-391 scopes vector profiles to the owner agent.")
	add("GET /v1/memories/{memory}", []string{"memory"}, 404, "internal/store/memory.go:1220-1224 scopes current memories to their owner agent.")
	add("GET /v1/memories/{memory}/history", []string{"history"}, 404, "internal/store/memory.go:1220-1224 scopes the history's memory prerequisite to its owner agent.")
	add("PATCH /v1/memories/{memory}", []string{"adjust"}, 404, "internal/store/memory.go:1220-1224 scopes the mutation's memory prerequisite to its owner agent.")
	add("POST /v1/memories/{memory}/supersede", []string{"source-and-evidence", "transcript-body", "memory-evidence", "memory-body", "message-evidence", "message-body"}, 404, "internal/store/memory.go:1220-1224 scopes the source memory before replacement evidence validation.")
	add("POST /v1/memories/{action}", []string{"forget", "restore", "reactivate"}, 404, "internal/store/memory.go:1220-1224 scopes lifecycle memory access to its owner agent.")
	add("POST /v1/memory-evidence/{evidence}/resolution", []string{"transcript", "transcript-body", "memory", "memory-body", "message", "message-body"}, 404, "internal/store/memory.go:1074-1075 scopes evidence lookup to the caller's owner agent before source validation.")
	add("DELETE /v1/memories/{memory}", []string{"preview"}, 404, "internal/store/memory.go:1220-1224 scopes the deletion-preview memory prerequisite to its owner agent.")
	add("POST /v1/memory-vectors", []string{"profile-and-memory", "profile-body", "memory-body"}, 404, "internal/store/memory_vector.go:390-391 scopes the profile prerequisite to the caller's owner agent.")
	add("GET /v1/memory-curation-requests/{request}", []string{"request"}, 404, "internal/store/memory_curation.go:487 scopes request lookup to the owner agent.")
	add("POST /v1/memory-curation-requests/{request}/start", []string{"start"}, 404, "internal/store/memory_curation.go:475-481 requires the caller's own lane before starting its request.")
	add("GET /v1/memory-curation-runs/{run}", []string{"run"}, 404, "internal/store/memory_curation.go:502 scopes run lookup to the owner agent.")
	add("GET /v1/memory-curation-runs/{run}/inputs", []string{"inputs"}, 409, "internal/store/memory_curation.go:2113-2114 compares the full caller's idle lane fence before loading the requested run; internal/server/memory_curation.go:825-826 maps fence mismatch to 409.")
	add("GET /v1/memory-curation-runs/{run}/plan", []string{"plan"}, 404, "internal/store/memory_curation.go:502 scopes the planned run to its owner agent.")
	add("POST /v1/memory-curation-runs/{run}/renew", []string{"renew"}, 409, "internal/store/memory_curation.go:1780-1781 refuses a run outside the caller's seeded idle lane; internal/server/memory_curation.go:825-826 maps the fence mismatch to 409.")
	add("POST /v1/memory-curation-runs/{run}/plan", []string{"plan-and-memory-target", "target-memory-body", "input-evidence-body", "source-transcript-body"}, 409, "internal/store/memory_curation_plan_store.go:349-350 refuses a run outside the caller's seeded idle lane; internal/server/memory_curation.go:825-826 maps the fence mismatch to 409.")
	add("POST /v1/memory-curation-runs/{run}/apply", []string{"apply"}, 409, "internal/store/memory_curation_apply.go:194-195 refuses a run outside the caller's seeded idle lane; internal/server/memory_curation.go:825-826 maps the fence mismatch to 409.")
	add("POST /v1/memory-curation-runs/{run}/cancel", []string{"cancel"}, 409, "internal/store/memory_curation.go:1937-1938 refuses a run outside the caller's seeded idle lane; internal/server/memory_curation.go:825-826 maps the fence mismatch to 409.")
	add("POST /v1/memory-curation-runs/{run}/abandon", []string{"abandon"}, 409, "internal/store/memory_curation.go:1937-1938 refuses a run outside the caller's seeded idle lane; internal/server/memory_curation.go:825-826 maps the fence mismatch to 409.")
	add("POST /v1/memory-curation-runs/{run}/rollback", []string{"receipt-and-head", "receipt-body", "produced-head-body"}, 404, "internal/store/memory_curation_rollback.go:152-154 requires the caller's own lane before inspecting a receipt.")
	add("GET /v1/memory-curation-status", []string{"run-filter"}, 404, "internal/store/memory_curation.go:2448-2452 scopes the caller lane before the selected run.")
	add("GET /v1/messages", []string{"from-filter", "thread-filter"}, 200, "internal/store/message.go:1167-1184 filters only the caller's inbox or outbox; a foreign filter has an empty successful result.")
	add("GET /v1/messages/{action}", []string{"peek"}, 404, "internal/store/message.go:1849-1851 requires the delivery recipient; the seeded message was sent by peer to main.")
	add("POST /v1/messages/{action}", []string{"read", "ack", "reply", "claim", "renew", "renew-claim-body", "release", "release-claim-body", "complete", "complete-claim-body"}, 404, "internal/store/message.go:1307-1308,1857-1858 requires main's delivery recipient before any transition or claim fence.")
	add("POST /v1/email/retry-canary:arm", []string{"configured-owner"}, 403, "internal/server/agent_email.go:1206-1207 permits only the configured B-main canary, never A-peer or A-other. FU122-6: this ConfigBound probe has no caller-selectable owner to replace with a never-issued ID.")
	add("POST /v1/email/retry-canary:status", []string{"configured-owner"}, 403, "internal/server/agent_email.go:1206-1207 permits only the configured B-main canary, never A-peer or A-other. FU122-6: this ConfigBound probe has no caller-selectable owner to replace with a never-issued ID.")
	add("POST /v1/email/{action}", []string{"read", "ack", "code-consumed", "claim", "renew", "renew-claim-body", "release", "release-claim-body", "complete", "complete-claim-body", "reply"}, 404, "internal/store/agent_email.go:2261-2262 scopes email deliveries to the owner agent before transitions.")
	add("GET /v1/email/sent/{id}", []string{"outbound-message"}, 404, "internal/store/agent_email_outbound.go:742 scopes outbound messages to their owner agent.")
	add("POST /v1/message-requests/{action}", []string{"offer", "decline"}, 404, "internal/store/message_request.go:979-981 requires immutable candidate membership; peer coordinates these fixtures and is not its own candidate.")
	add("POST /v1/message-requests/{action}", []string{"select", "selected-agent-body", "cancel"}, 404, "internal/store/message_request.go:1712-1713 requires main's coordinator identity before inspecting candidates.")
	add("POST /v1/message-requests/{action}", []string{"claim", "renew", "renew-claim-body", "release", "release-claim-body", "complete", "complete-claim-body"}, 404, "internal/store/message_request.go:2145-2146 scopes claims to the reserved agent, which is main in these fixtures.")
	return m
}()

// This closed list mirrors only the existing request fixtures. It does not
// derive exemptions from arbitrary fixture keys or permit all realm data.
var tenantSameAccountRequestSeeds = []string{
	"message_request", "message_request_offer", "message_request_decline",
	"message_request_select", "message_request_select_selected-agent-body", "message_request_cancel",
	"message_request_claim", "message_request_renew", "message_request_renew_claim-body",
	"message_request_release", "message_request_release_claim-body",
	"message_request_complete", "message_request_complete_claim-body",
}

func tenantSameAccountRequestMetadata() map[string]string {
	const cite = "internal/store/message_request.go:679-680 allows peer as coordinator or immutable candidate; internal/store/message_request.go:403-410 projects identity, opening message, subject and thread metadata."
	m := map[string]string{}
	for _, key := range []string{"account", "realm", "agent", "agent-name", "peer_agent", "peer-name", "message_request"} {
		m[key] = cite
	}
	for _, seed := range tenantSameAccountRequestSeeds {
		for _, suffix := range []string{"id", "opening", "thread"} {
			m["seed."+seed+"."+suffix] = cite
		}
		m["canary.seed."+seed+".subject"] = cite
	}
	return m
}

// The non-coordinator detail contains only peer's candidate row. The store
// redacts SelectedAgentIDs to peer at message_request.go:471-487, so neither
// candidate membership nor selection permits any other candidate's identity.
func tenantSameAccountRequestDetail() map[string]string {
	const request = "internal/store/message_request.go:403-410 returns the authorized request's account, realm, coordinator, opening, subject, body and thread; message_request.go:780-790 loads the content-bearing detail."
	m := tenantF1AllowedKeys(request,
		"account", "realm", "agent", "agent-name", "message_request", "seed.message_request.id",
		"seed.message_request.opening", "seed.message_request.thread",
		"canary.seed.message_request.subject", "canary.seed.message_request.body")
	m["peer_agent"] = "internal/store/message_request.go:797-808 returns only the caller's candidate agent ID when peer is not the coordinator."
	m["peer-name"] = "internal/store/message_request.go:797-808 returns only the caller's candidate agent name when peer is not the coordinator."
	return m
}

func tenantSameAccountMessageMetadata() map[string]string {
	const cite = "internal/store/message.go:1167-1168 allows peer's inbox deliveries; internal/store/message.go:1522-1530 projects sender, recipient, subject, thread and reply-parent metadata."
	m := map[string]string{}
	for _, key := range []string{"account", "realm", "agent", "agent-name", "peer_agent", "peer-name", "message-main-to-peer", "seed.message-main-to-peer.id", "seed.message-main-to-peer.thread", "canary.seed.message-main-to-peer.subject"} {
		m[key] = cite
	}
	// Only main's request broadcasts and main's candidate offers are in peer's
	// inbox. The other openings are peer's outbox, but may be reply parents.
	for _, seed := range []string{"message_request", "message_request_select", "message_request_select_selected-agent-body", "message_request_cancel"} {
		m["seed."+seed+".opening"], m["seed."+seed+".thread"] = cite, cite
		m["canary.seed."+seed+".subject"] = cite
	}
	for _, seed := range []string{"message_request_claim", "message_request_renew", "message_request_renew_claim-body", "message_request_release", "message_request_release_claim-body", "message_request_complete", "message_request_complete_claim-body"} {
		m["seed."+seed+".offer.message"] = cite
		m["seed."+seed+".opening"], m["seed."+seed+".thread"] = cite, cite
	}
	return m
}
