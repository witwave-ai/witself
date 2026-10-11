package main

import "strings"

// F1's declarations are intentionally closed: an added agent probe is left
// unclassified so TestTenantMatrixSameAccountFieldsComplete requires review.
func tenantSameAccountF1(row *tenantRow) {
	if row.Class != tenant || tenantFamily(row.Pattern) != "F1" {
		return
	}
	for i := range row.Probes {
		p := &row.Probes[i]
		if p.Cred != credAgent {
			continue
		}
		switch p.Kind {
		case foreign:
			tenantSameAccountF1Foreign(row.Pattern, p)
		case own:
			tenantSameAccountF1Own(row.Pattern, p)
		}
	}
}

func tenantSameAccountF1Foreign(pattern string, p *tenantProbe) {
	var reason string
	var statuses []int
	switch pattern {
	case "DELETE /v1/facts", "DELETE /v1/facts/{fact}":
		reason = "internal/store/fact_delete.go:271 scopes deletion preview to account, realm and owner agent."
	case "GET /v1/facts":
		if p.Name == "subject-address" {
			reason = "internal/store/fact.go:481-493 scopes exact subject lookup to account, realm and owner agent."
		}
	case "GET /v1/facts/{fact}/history":
		reason = "internal/store/fact.go:427 checks the owner-scoped fact lookup at fact.go:481 before reading history."
	case "GET /v1/fact-occurrences":
		if p.Name == "subject-filter" {
			statuses = []int{200}
			reason = "internal/store/fact_temporal.go:79-98 filters occurrences by account, realm and owner; another owner's subject yields an empty list."
		}
	case "GET /v1/fact-candidates/{candidate}":
		reason = "internal/store/fact_candidate.go:221 scopes candidate lookup to account, realm and owner agent."
	case "POST /v1/fact-candidates/{action}":
		switch p.Name {
		case "confirm":
			reason = "internal/store/fact_candidate.go:450-451 scopes confirmation to account, realm and owner agent."
		case "reject":
			reason = "internal/store/fact_candidate.go:357 scopes rejection to account, realm and owner agent."
		}
	case "POST /v1/fact-subjects/{subject}/aliases":
		reason = "internal/store/fact_subject.go:132 resolves the subject through the account, realm and owner filter at fact_subject.go:231."
	case "GET /v1/transcripts/{transcript}":
		statuses = []int{403}
		reason = "internal/store/transcript.go:554-556 forbids another agent's transcript after account lookup; the known existence distinction is FU122-6."
	case "POST /v1/transcripts/{transcript}/entries", "POST /v1/transcripts/{transcript}/entries:batch":
		statuses = []int{403}
		reason = "internal/store/transcript.go:249-250 forbids a different realm or owner before reply validation; both target and own-path body-reference probes retain main's path. FU122-6."
	case "POST /v1/self/avatar/proposals":
		if p.Name == "foreign-realm-style" {
			p.SameRealm = &tenantSameAccountRule{
				Mode: "allowed", Want: []int{200, 201},
				Reason: "internal/store/avatar_mutation.go:97-98,1167-1172 loads styles by account and realm without an agent-owner predicate, permitting the peer's own proposal with the shared realm style.",
				AllowedKeys: tenantF1AllowedKeys("internal/store/avatar_mutation.go:291-298 returns the caller's post-proposal view; internal/store/avatar_query.go:235-262,286-287 projects its account, realm, owner and selected style.",
					"account", "realm", "peer_agent", "avatar.style.id"),
			}
			p.SameRealm.AllowedKeys["peer-name"] = "internal/store/avatar.go:379-384,509 populates the mutation receipt actor with the authenticated caller's name; cmd/witself-server/avatar.go:292-295 returns that name."
			p.OtherRealm = &tenantSameAccountRule{Mode: "refuse", Reason: "internal/store/avatar_mutation.go:1171 scopes the style lookup to the caller's realm; main's style is absent in the other realm."}
		}
		return
	}
	if reason != "" {
		p.SameRealm = &tenantSameAccountRule{Mode: "refuse", Refuse: statuses, Reason: reason}
		p.OtherRealm = &tenantSameAccountRule{Mode: "refuse", Refuse: statuses, Reason: reason}
	}
}

func tenantSameAccountF1Own(pattern string, p *tenantProbe) {
	switch pattern {
	case "GET /v1/self":
		cite := "internal/server/server.go:3737-3742 returns the authenticated caller's account, realm and agent identity."
		p.SameAccountAllowed = tenantF1AllowedKeys(cite, "account", "realm", "realm-name", "peer_agent", "peer-name")
		p.OtherRealmAllowed = tenantF1AllowedKeys(cite, "account", "realm2", "realm2-name", "other_agent", "other-name")
		p.SameAccountAllowed["avatar.style.id"] = "internal/store/avatar_query.go:88-99 returns the peer profile's realm-selected style ID in the self checkpoint."
	case "GET /v1/self/peers":
		p.SameAccountAllowed = tenantF1PeerAllowed()
	case "GET /v1/self/avatar":
		cite := "internal/store/avatar_query.go:235-262 returns the caller's own profile account, realm, agent and realm-selected style."
		p.SameAccountAllowed = tenantF1AllowedKeys(cite, "account", "realm", "peer_agent", "avatar.style.id")
		p.OtherRealmAllowed = tenantF1AllowedKeys(cite, "account", "realm2", "other_agent")
	case "GET /v1/self/avatar/style":
		cite := "internal/store/avatar_style.go:61-80,310-323 returns the caller's realm ID and its shared style specification."
		p.SameAccountAllowed = tenantF1AllowedKeys(cite, "realm", "avatar.style.id", "canary.avatar.style.name", "canary.avatar.style.description")
		p.OtherRealmAllowed = tenantF1AllowedKeys(cite, "realm2")
	case "GET /v1/activity":
		cite := "internal/store/activity.go:76 returns the authenticated caller's account, realm and agent IDs."
		p.SameAccountAllowed = tenantF1AllowedKeys(cite, "account", "realm", "peer_agent")
		p.OtherRealmAllowed = tenantF1AllowedKeys(cite, "account", "realm2", "other_agent")
	case "GET /v1/usage":
		cite := "internal/store/usage.go:171-175 returns the authenticated caller's account, realm and agent identity."
		p.SameAccountAllowed = tenantF1AllowedKeys(cite, "account", "realm", "realm-name", "peer_agent", "peer-name")
		p.OtherRealmAllowed = tenantF1AllowedKeys(cite, "account", "realm2", "realm2-name", "other_agent", "other-name")
	}
}

func tenantF1AllowedKeys(cite string, keys ...string) map[string]string {
	allowed := make(map[string]string, len(keys))
	for _, key := range keys {
		allowed[key] = cite
	}
	return allowed
}

func tenantF1PeerAllowed() map[string]string {
	const cite = "internal/store/agent_activity.go:160-179 returns live same-realm peers' public IDs and names, excluding the caller."
	allowed := tenantF1AllowedKeys(cite, "agent", "agent-name")
	// These exact principals are minted by tenantSeedIdentities. Their names
	// contain canaries, but peer discovery explicitly exposes those names.
	for _, key := range []string{
		"agent.delete", "agent.avatar.activate", "agent.avatar.policy", "agent.avatar.propose", "agent.avatar.quota", "agent.avatar.reject", "agent.avatar.reset", "agent.avatar.rollback",
		"agent.self.read", "agent.self.activate", "agent.self.failure", "agent.self.propose", "agent.self.propose.foreign", "agent.self.reset", "agent.self.rollback", "agent.subject.upsert",
		"agent_email_receive", "agent_email_send",
	} {
		allowed[key], allowed["seed."+key+".name"] = cite, cite
	}
	// Match the two existing Fresh-principal seeders exactly. These name/ID
	// entries permit no unrelated object payloads or token material.
	for _, row := range tenantRowsF2() {
		for _, probe := range row.Probes {
			if probe.Fresh == "" {
				continue
			}
			if strings.Contains(row.Pattern, "/v1/vault/") || strings.Contains(row.Pattern, "/v1/secrets") {
				prefix := "seed.vault." + tenantProbeID(row, probe) + "."
				allowed[prefix+"agent"], allowed[prefix+"agent-name"] = cite, cite
			}
			if strings.Contains(row.Pattern, "memory-curation") && probe.Kind == foreign {
				prefix := "seed.memory." + probe.Fresh + "."
				allowed[prefix+"agent"], allowed[prefix+"agent-name"] = cite, cite
			}
		}
	}
	return allowed
}
