package main

import (
	"encoding/json"
	"strings"

	"github.com/witwave-ai/witself/internal/avatar"
)

// These are independent declarations, not rows synthesized from the inventory.
// The fixture keys identify the precise pre-seeded target needed by 122b. Fresh
// targets survive destructive controls without invalidating another row.
func tenantRowsF1() []tenantRow {
	rows := []tenantRow{
		tenantF1Exempt("/v1/version", exemptPublic, credNone, "/v1/version", "", "Public build/protocol metadata; server.go:2429-2439 writes no tenant data."),
		tenantF1Exempt("/v1/capabilities", exemptPublic, credNone, "/v1/capabilities", "", "Public cell capability discovery in capabilitiesHandler; no object selector."),
		tenantF1Exempt("/livez", exemptPublic, credNone, "/livez", "", "Health-listener liveness, not a tenant API; server.go:7411."),
		tenantF1Exempt("/startupz", exemptPublic, credNone, "/startupz", "", "Health-listener startup check, not a tenant API; server.go:7412."),
		tenantF1Exempt("/healthz", exemptPublic, credNone, "/healthz", "", "Health-listener health check, not a tenant API; server.go:7413."),
		tenantF1Exempt("/readyz", exemptPublic, credNone, "/readyz", "", "Health-listener readiness, not a tenant API; server.go:7414."),
		tenantF1Exempt("/sync-readyz", exemptPublic, credNone, "/sync-readyz", "", "Health-listener route registered in healthMuxWithSync at server.go:7554; served only when the local runtime selects the sync schema."),
		tenantF1Exempt("/metrics", exemptPublic, credNone, "/metrics", "", "Dedicated metrics listener; server.go:7444 exposes cell metrics."),
		tenantF1Exempt("POST /v1/accounts:provision-exact", exemptSystemToken, credProvision, "/v1/accounts:provision-exact", `{"account_id":"${account}"}`, "Cell-wide provision token chooses an account by design; server.go:4252."),
		tenantF1Exempt("GET /v1/accounts/", exemptSystemToken, credProvision, "/v1/accounts/${account}:plan", "", "accountSystemGetHandler uses the cell provision token; server.go:4347-4360."),
		tenantF1Exempt("PATCH /v1/accounts/", exemptSystemToken, credProvision, "/v1/accounts/${account}:restore-placement-policy", `{}`, "System placement restore uses the provision token; server.go:4551-4570."),
		tenantF1AccountActions(),
		tenantF1Exempt("POST /v1/support/admin:list-tickets", exemptSystemToken, credProvision, "/v1/support/admin:list-tickets", `{"account_id":"${account}"}`, "Fleet support fan-out requires the cell provision token; server.go:6207."),
		tenantF1Exempt("POST /v1/support/admin:match-contact", exemptSystemToken, credProvision, "/v1/support/admin:match-contact", `{"account_id":"${account}","email":"${email.contact}"}`, "Contact matching is provision-token-only administration; server.go:6270."),
		tenantF1Exempt("POST /v1/events/admin:list", exemptSystemToken, credProvision, "/v1/events/admin:list", `{"account_id":"${account}"}`, "Cell-wide audit fan-out requires the provision token; server.go:6116."),
		tenantF1Exempt("POST /v1/internal/agent-email:ingest", exemptSignedRelay, credAgent, "/v1/internal/agent-email:ingest", `{}`, "Only a verified Ed25519 relay envelope authorizes ingestion; server.go:2446-2451."),
		tenantF1Exempt("POST /v1/internal/agent-email-send:provider-event", exemptProviderToken, credAgent, "/v1/internal/agent-email-send:provider-event", `{}`, "Provider event bearer is a cell-specific provider token; agent_email_outbound.go:215-222."),
		tenantF1Exempt("POST /v1/auth/bootstrap", exemptBootstrap, credAgent, "/v1/auth/bootstrap", `{"bootstrap_token":"${cred}"}`, "Login exchanges a one-time bootstrap token; server.go:3140. The probe uses an agent token instead."),
		tenantF1Exempt("POST /v1/accounts", exemptNotMounted, credNone, "/v1/accounts", `{}`, "Production wires ProvisionAccountExact only; cmd/witself-server/main.go:1889-1895 leaves legacy ProvisionAccount nil."),

		tenantF1Export(),
		tenantF1Own("GET /v1/whoami", credOperator, "Identity comes solely from the operator bearer.", ""),
		tenantF1Own("GET /v1/auth/whoami", credOperator, "Identity comes solely from the operator bearer.", ""),
		tenantF1Own("POST /v1/realms", credOperator, "Creates a named realm within the authenticated account; no referenced object.", `{"name":"${name.realm.create}"}`),
		tenantF1Own("GET /v1/realms", credOperator, "Lists the authenticated account's realms; no foreign selector.", ""),
		tenantF1Foreign("DELETE /v1/realms/{realm}", credOperator, "/v1/realms/${realm.delete}", "", "realm.delete"),
		tenantF1Foreign("POST /v1/realms/{realm}/agents", credOperator, "/v1/realms/${realm}/agents", `{"name":"${name.agent.create}"}`, ""),
		tenantF1Foreign("POST /v1/realms/{realm}/agents:with-email-segment", credOperator, "/v1/realms/${realm}/agents:with-email-segment", `{"name":"${name.agent.email}","email_agent_segment":"${name.agent.segment}"}`, ""),
		{Pattern: "GET /v1/realms/{realm}/agents", Class: tenant, Probes: []tenantProbe{{Name: "target", Kind: foreign, Cred: credOperator, EmptyList: true, EmptyListWhy: "internal/store/agent.go:193-214 filters agents by both realm and authenticated account; foreign and missing realms yield no rows.", Build: tenantBuild("GET", "/v1/realms/${realm}/agents", "", "")}}},
		tenantF1Foreign("DELETE /v1/realms/{realm}/agents/{agent}", credOperator, "/v1/realms/${realm}/agents/${agent.delete}", "", "agent.delete"),
		tenantF1Foreign("POST /v1/agents/{agent}/tokens", credOperator, "/v1/agents/${agent}/tokens", `{}`, ""),
		tenantF1Foreign("POST /v1/agents/{agent}/curator-tokens", credOperator, "/v1/agents/${agent}/curator-tokens", `{"access_profile":"curator-preview","display_name":"${name.token.curator}","ttl":"1h"}`, ""),
		tenantF1Scratch("POST /v1/operators/self/tokens", "S-optoken", `{"display_name":"${name.token.create}","ttl":"1h"}`),
		tenantF1Own("GET /v1/account/events", credOperator, "Audit list is scoped by the authenticated account; no foreign object selector.", ""),
		tenantF1Own("GET /v1/operators", credOperator, "Operator inventory belongs to the authenticated account.", ""),
		tenantF1Own("POST /v1/operators", credOperator, "Creates an operator in the authenticated account; body has names, not referenced objects.", `{"display_name":"${name.operator.create}","token_display_name":"${name.token.operator}"}`),
		tenantF1Foreign("DELETE /v1/operators/{operator}", credOperator, "/v1/operators/${operator.delete}", "", "operator.delete"),
		tenantF1Foreign("POST /v1/tokens/", credOperator, "/v1/tokens/${token.revoke}:revoke", "", "token.revoke"),
		tenantF1Scratch("POST /v1/account:close", "S-close", `{"reason":"${canary.account.close}"}`),
		tenantF1Own("GET /v1/account", credOperator, "Account identity comes solely from the authenticated operator.", ""),
		tenantF1CloseEligibility(),
		tenantF1Scratch("POST /v1/account:rename", "S-rename", `{"display_name":"${name.account.rename}"}`),
		tenantF1Own("GET /v1/account/placement-policy", credOperator, "Reads the authenticated account's placement policy.", ""),
		tenantF1Scratch("PATCH /v1/account/placement-policy", "S-placement", `{"preferred_clouds":[],"preferred_regions":[],"preferred_channels":[],"allowed_clouds":[],"allowed_regions":[],"allowed_channels":[],"rebalance_on":[]}`),
		tenantF1Scratch("POST /v1/account:suspend", "S-suspend", `{"reason":"${canary.account.suspend}"}`),
		tenantF1Scratch("POST /v1/account:resume", "S-resume", ""),
		tenantF1Own("POST /v1/support/tickets", credOperator, "Creates a ticket for the authenticated account; body has no foreign object reference.", `{"subject":"${canary.ticket.subject}","category":"other","priority":"normal","body":"${canary.ticket.body}"}`),
		tenantF1Own("GET /v1/support/tickets", credOperator, "Lists tickets in the authenticated account.", ""),
		tenantF1Foreign("GET /v1/support/tickets/{ticket}", credOperator, "/v1/support/tickets/${ticket}", "", ""),
		tenantF1Foreign("POST /v1/support/tickets/{ticket}/messages", credOperator, "/v1/support/tickets/${ticket.reply}/messages", `{"body":"${canary.ticket.reply}"}`, "ticket.reply"),
		tenantF1Foreign("PATCH /v1/support/tickets/{ticket}/state", credOperator, "/v1/support/tickets/${ticket.state}/state", `{"state":"closed"}`, "ticket.state"),

		tenantF1Foreign("GET /v1/agents/{agent}/avatar", credOperator, "/v1/agents/${agent}/avatar", "", ""),
		tenantF1Foreign("GET /v1/agents/{agent}/avatar/history", credOperator, "/v1/agents/${agent}/avatar/history", "", ""),
		tenantF1Foreign("GET /v1/agents/{agent}/avatar/versions/{version}", credOperator, "/v1/agents/${agent}/avatar/versions/${avatar.version}", "", ""),
		tenantF1ProposalForeign("POST /v1/agents/{agent}/avatar/proposals", credOperator, "/v1/agents/${agent.avatar.propose}/avatar/proposals", "agent.avatar.propose"),
		tenantF1Foreign("POST /v1/agents/{agent}/avatar:activate", credOperator, "/v1/agents/${agent.avatar.activate}/avatar:activate", tenantF1AvatarVersionMutation, "agent.avatar.activate"),
		tenantF1Foreign("POST /v1/agents/{agent}/avatar:reject", credOperator, "/v1/agents/${agent.avatar.reject}/avatar:reject", tenantF1AvatarVersionMutation, "agent.avatar.reject"),
		tenantF1Foreign("POST /v1/agents/{agent}/avatar:rollback", credOperator, "/v1/agents/${agent.avatar.rollback}/avatar:rollback", tenantF1AvatarVersionMutation, "agent.avatar.rollback"),
		tenantF1Foreign("POST /v1/agents/{agent}/avatar:reset", credOperator, "/v1/agents/${agent.avatar.reset}/avatar:reset", `{"expected_profile_revision":${avatar.profile.revision},"reason_code":"user_requested"}`, "agent.avatar.reset"),
		tenantF1Foreign("PATCH /v1/agents/{agent}/avatar-policy", credOperator, "/v1/agents/${agent.avatar.policy}/avatar-policy", `{"policy":"agent_self_managed","expected_profile_revision":${avatar.profile.revision}}`, "agent.avatar.policy"),
		tenantF1Foreign("PATCH /v1/agents/{agent}/avatar-quota", credOperator, "/v1/agents/${agent.avatar.quota}/avatar-quota", `{"retained_payload_count_limit":8,"retained_payload_byte_limit":1048576,"expected_profile_revision":${avatar.profile.revision}}`, "agent.avatar.quota"),
		tenantF1Foreign("GET /v1/realms/{realm}/avatar-style", credOperator, "/v1/realms/${realm}/avatar-style", "", ""),
		{Pattern: "POST /v1/realms/{realm}/avatar-style/versions", Class: tenant, Probes: []tenantProbe{{Name: "publish-style", Kind: foreign, Cred: credOperator, Fresh: "realm.style", Build: tenantF1StyleBuild}}},

		tenantF1Own("GET /v1/self", credAgent, "Self identity and hydration derive from the authenticated principal.", ""),
		tenantF1Own("GET /v1/self/peers", credAgent, "Lists peers in the authenticated realm; no foreign selector.", ""),
		tenantF1Own("POST /v1/self/activity", credAgent, "Updates activity only for the authenticated agent.", `{"runtime":"codex","location_id":"${name.activity.location}","location":"${canary.activity.location}","event":"SessionStart","event_id":"${name.activity.event}","event_occurred_at":"2026-10-10T00:00:00Z"}`),
		tenantF1Own("GET /v1/self/dashboard-preferences", credAgent, "Self preferences have no object selector; dashboard_preferences.go:51.", ""),
		tenantF1Own("PUT /v1/self/dashboard-preferences", credAgent, "Writes only the authenticated agent's preferences; no object references.", `{"prefs":{"schema":"witself.dashboard-prefs.v1","theme":"dark"}}`),
		tenantF1Own("GET /v1/self/avatar", credAgent, "Reads only the authenticated agent's avatar profile.", ""),
		tenantF1Own("GET /v1/self/avatar/history", credAgent, "Lists only the authenticated agent's avatar versions.", ""),
		tenantF1SelfVersionRead(),
		tenantF1Own("GET /v1/self/avatar/style", credAgent, "Reads the authenticated agent's realm-selected style.", ""),
		tenantF1SelfProposal(),
		tenantF1SelfVersionConflict("POST /v1/self/avatar:activate", "agent.self.activate", "internal/store/avatar_mutation.go:358-360"),
		tenantF1SelfVersionConflict("POST /v1/self/avatar:rollback", "agent.self.rollback", "internal/store/avatar_mutation.go:489-491"),
		tenantF1OwnFresh("POST /v1/self/avatar:reset", "agent.self.reset", "Resets only the authenticated agent's profile; a dedicated principal protects later rows.", `{"expected_profile_revision":${avatar.profile.revision},"reason_code":"user_requested"}`),
		tenantF1OwnFresh("POST /v1/self/avatar:generation-failed", "agent.self.failure", "Records failure only on the authenticated profile; no referenced object.", `{"expected_profile_revision":${avatar.profile.revision},"reason_code":"render_failed"}`),

		tenantF1Own("GET /v1/facts:status", credAgent, "Returns capacity for the authenticated owner; no referenced object.", ""),
		tenantF1Own("POST /v1/facts", credAgent, "Subject keys name the authenticated agent's namespace; set creates a local fact, not a foreign object lookup.", `{"subject":"${name.subject.create}","predicate":"matrix/value","value":"${canary.fact.value}","source_kind":"agent"}`),
		{Pattern: "DELETE /v1/facts", Class: tenant, Probes: []tenantProbe{{Name: "address-preview", Kind: foreign, Cred: credAgent, NoIdempotencyKey: true, NoIdempotencyKeyWhy: "internal/server/fact.go:413-425 rejects idempotency keys on dry-run fact previews.", Build: tenantBuild("DELETE", "/v1/facts", "dry_run=true&subject=${fact.subject}&predicate=matrix/value", "")}}},
		{Pattern: "DELETE /v1/facts/{fact}", Class: tenant, Probes: []tenantProbe{{Name: "id-preview", Kind: foreign, Cred: credAgent, NoIdempotencyKey: true, NoIdempotencyKeyWhy: "internal/server/fact.go:413-425 rejects idempotency keys on dry-run fact previews.", Build: tenantBuild("DELETE", "/v1/facts/${fact}", "dry_run=true", "")}}},
		{Pattern: "GET /v1/facts", Class: tenant, Probes: []tenantProbe{
			tenantProbeFor("list", own, credAgent, "GET", "/v1/facts", ""),
			{Name: "subject-address", Kind: foreign, Cred: credAgent, Build: tenantBuild("GET", "/v1/facts", "subject=${fact.subject}&predicate=matrix/value", "")},
		}},
		tenantF1Foreign("GET /v1/facts/{fact}/history", credAgent, "/v1/facts/${fact}/history", "", ""),
		{Pattern: "GET /v1/fact-occurrences", Class: tenant, Probes: []tenantProbe{
			tenantProbeFor("own", own, credAgent, "GET", "/v1/fact-occurrences", ""),
			{Name: "subject-filter", Kind: foreign, Cred: credAgent, EmptyList: true, EmptyListWhy: "internal/store/fact_temporal.go:79-98 scopes the subject-filtered projection to the authenticated account, realm and owner.", Build: tenantBuild("GET", "/v1/fact-occurrences", "subject=${fact.subject}", "")},
		}},
		tenantF1Own("POST /v1/fact-candidates", credAgent, "internal/store/transcript_redaction_fact.go:45-69 refuses redacted entries resolved within the caller scope; foreign, missing and unparseable source_ref provenance stays opaque.", `{"subject":"self","predicate":"matrix/proposal","value":"${canary.fact.proposal}","source_ref":"witself://transcript/${transcript}/entry/${transcript.entry}","reason":"${canary.fact.reason}"}`),
		tenantF1Own("GET /v1/fact-candidates", credAgent, "Lists candidate facts only for the authenticated owner.", ""),
		tenantF1Foreign("GET /v1/fact-candidates/{candidate}", credAgent, "/v1/fact-candidates/${fact.candidate}", "", ""),
		// Both suffixes are selected by server/fact.go:590-594.
		{Pattern: "POST /v1/fact-candidates/{action}", Class: tenant, Handler: "factCandidateActionHandler", Actions: []string{"confirm", "reject"}, Probes: []tenantProbe{
			{Name: "confirm", Kind: foreign, Cred: credAgent, Fresh: "fact.candidate.confirm", Build: tenantBuild("POST", "/v1/fact-candidates/${fact.candidate.confirm}:confirm", "", "")},
			{Name: "reject", Kind: foreign, Cred: credAgent, Fresh: "fact.candidate.reject", Build: tenantBuild("POST", "/v1/fact-candidates/${fact.candidate.reject}:reject", "", "")},
		}},
		tenantF1SubjectUpsert(),
		tenantF1Foreign("POST /v1/fact-subjects/{subject}/aliases", credAgent, "/v1/fact-subjects/${fact.subject.alias}/aliases", `{"alias":"${name.subject.alias}"}`, "fact.subject.alias"),
		tenantF1Own("GET /v1/fact-subjects", credAgent, "Lists canonical subjects in the authenticated agent namespace.", ""),
		tenantF1Own("GET /v1/activity", credAgent, "Activity scope comes from the bearer; no foreign object selector.", ""),
		tenantF1Own("GET /v1/usage", credAgent, "Usage scope comes from the bearer; no foreign object selector.", ""),
		tenantF1Own("POST /v1/transcripts", credAgent, "Creates a transcript owned by the authenticated agent; metadata is not foreign authority.", `{"title":"${canary.transcript.title}","metadata":{}}`),
		{Pattern: "GET /v1/transcripts", Class: tenant, NoForeignWhy: "Lists the authenticated agent's transcripts, or the authenticated operator's account; store/transcript.go:426-463.", Probes: []tenantProbe{
			tenantProbeFor("agent-list", own, credAgent, "GET", "/v1/transcripts", ""),
			tenantProbeFor("operator-list", own, credOperator, "GET", "/v1/transcripts", ""),
		}},
		{Pattern: "GET /v1/transcripts/{transcript}", Class: tenant, Probes: []tenantProbe{
			tenantProbeFor("agent-read", foreign, credAgent, "GET", "/v1/transcripts/${transcript}", ""),
			tenantProbeFor("operator-read", foreign, credOperator, "GET", "/v1/transcripts/${transcript}", ""),
		}},
		tenantF1TranscriptAppend(false),
		tenantF1TranscriptAppend(true),
		{Pattern: "POST /v1/transcripts/{transcript}/entries:redact", Class: tenant, Probes: []tenantProbe{
			{Name: "operator-preview", Kind: foreign, Cred: credOperator, NoIdempotencyKey: true, NoIdempotencyKeyWhy: "internal/server/transcript_redaction.go:97-100 rejects mutation guards on dry_run=true previews.", Build: tenantBuild("POST", "/v1/transcripts/${transcript}/entries:redact", "dry_run=true", `{"from_sequence":1,"until_sequence":1}`)},
		}},
	}
	// Mark the local-only route and seed positive list witnesses before the first fingerprint.
	for i := range rows {
		switch rows[i].Pattern {
		case "/sync-readyz":
			rows[i].LocalRuntimeOnly = true
		case "GET /v1/facts", "GET /v1/fact-subjects", "GET /v1/fact-candidates", "GET /v1/transcripts", "GET /v1/support/tickets", "GET /v1/fact-occurrences":
			for j := range rows[i].Probes {
				if rows[i].Probes[j].Kind == own {
					rows[i].Probes[j].ShowsOwn = true
				}
			}
		}
	}
	return rows
}

const tenantF1AvatarProposal = `{"expected_profile_revision":${avatar.profile.revision},"style_pack_id":"${avatar.style.id}","style_pack_version":${avatar.style.version},"subject_form":"human","description":"${canary.avatar.description}","visual_spec":{"expression":"calm"},"svg":"<svg xmlns=\"http://www.w3.org/2000/svg\" viewBox=\"0 0 512 512\"><circle cx=\"256\" cy=\"256\" r=\"160\" fill=\"#203247\"/></svg>"}`
const tenantF1AvatarVersionMutation = `{"version":${avatar.version},"expected_profile_revision":${avatar.profile.revision}}`

func tenantF1Foreign(pattern string, cred tenantCred, path, body, fresh string) tenantRow {
	method, _, _ := strings.Cut(pattern, " ")
	p := tenantProbeFor("target", foreign, cred, method, path, body)
	p.Fresh = fresh
	return tenantRow{Pattern: pattern, Class: tenant, Probes: []tenantProbe{p}}
}

func tenantF1Own(pattern string, cred tenantCred, why, body string) tenantRow {
	method, path, _ := strings.Cut(pattern, " ")
	return tenantRow{Pattern: pattern, Class: tenant, NoForeignWhy: why, Probes: []tenantProbe{tenantProbeFor("own", own, cred, method, path, body)}}
}

func tenantF1Scratch(pattern, scratch, body string) tenantRow {
	row := tenantF1Own(pattern, credScratchOperator, "Only the authenticated scratch operator's account is selected; no request field addresses another account.", body)
	row.Probes[0].Scratch = scratch
	return row
}

func tenantF1OwnFresh(pattern, fresh, why, body string) tenantRow {
	row := tenantF1Own(pattern, credAgent, why, body)
	row.Probes[0].Fresh = fresh
	return row
}

func tenantF1Exempt(pattern string, class tenantClass, cred tenantCred, path, body, reason string) tenantRow {
	method, _, hasMethod := strings.Cut(pattern, " ")
	if !hasMethod {
		method = "GET"
	}
	return tenantRow{Pattern: pattern, Class: class, Reason: reason, Probes: []tenantProbe{tenantProbeFor("exemption", exemption, cred, method, path, body)}}
}

func tenantF1StyleBuild(f *tenantFixture, side tenantSide) tenantRequest {
	// Reuse the repository's valid canonical references without copying their
	// digest strings into fixtures. All actual tenant metadata is side-scoped.
	pack := avatar.BuiltInFlatVectorStylePack()
	pack.ID = f.value(side, "avatar.style.id")
	pack.Name = f.value(side, "canary.avatar.style.name")
	pack.Description = f.value(side, "canary.avatar.style.description")
	body, err := json.Marshal(map[string]any{"expected_style_revision": 1, "style_pack": pack})
	if err != nil {
		panic(err)
	}
	return tenantBuild("POST", "/v1/realms/${realm.style}/avatar-style/versions", "", string(body))(f, side)
}

// A caller-namespaced probe has an ordinary own request and a separate paired
// key challenge. NamespaceBuild varies only the key under a Fresh A principal.
func tenantF1SubjectUpsert() tenantRow {
	row := tenantF1OwnFresh("PUT /v1/fact-subjects/{subject}", "agent.subject.upsert", "internal/store/fact_subject.go:47-99 upserts a canonical key only in the authenticated agent namespace.", `{"display_name":"${own.canary.subject.display}"}`)
	p := &row.Probes[0]
	p.CallerNamespaced = true
	p.Build = tenantBuild("PUT", "/v1/fact-subjects/${fact.subject}", "", `{"display_name":"${own.canary.subject.display}"}`)
	p.NamespaceBuild = p.Build
	return row
}

// tenantF1CloseEligibility answers the close-eligibility check for the own
// account. The F2 vault seed (tenant_matrix_seed_vault_test.go) leaves an open
// key rotation on every seeded account, and internal/store/close.go refuses to
// close while a rotation or enrollment is active, so the own control is the
// lifecycle 409 (writeAccountCloseError), never 2xx. A 2xx here would mean the
// vault lifecycle fence is gone. Identity still comes solely from the operator.
func tenantF1CloseEligibility() tenantRow {
	row := tenantF1Own("GET /v1/account/close-eligibility", credOperator, "Close eligibility and owner identity come solely from the authenticated operator.", "")
	row.Probes[0].Want = []int{409}
	return row
}

func tenantF1SelfVersionRead() tenantRow {
	row := tenantF1OwnFresh("GET /v1/self/avatar/versions/{version}", "agent.self.read", "internal/store/avatar_query.go:179-184 resolves numeric versions only through the authenticated agent; avatar_query.go:497-502 scopes its profile.", "")
	p := &row.Probes[0]
	p.CallerNamespaced = true
	p.Want = []int{404}
	p.Build = tenantBuild("GET", "/v1/self/avatar/versions/${avatar.version}", "", "")
	p.NamespaceBuild = p.Build
	return row
}

func tenantF1SelfVersionConflict(pattern, fresh, storeCite string) tenantRow {
	_, path, _ := strings.Cut(pattern, " ")
	row := tenantF1OwnFresh(pattern, fresh, storeCite+" scopes numeric versions to the fresh authenticated profile; absent active/proposed versions yield the same value-free conflict.", tenantF1AvatarVersionMutation)
	p := &row.Probes[0]
	p.CallerNamespaced = true
	p.Want = []int{409}
	p.Build = tenantBuild("POST", path, "", `{"version":${avatar.version},"expected_profile_revision":${own.avatar.profile.revision}}`)
	p.NamespaceBuild = p.Build
	return row
}

func tenantF1ProposalForeign(pattern string, cred tenantCred, path, fresh string) tenantRow {
	row := tenantF1Foreign(pattern, cred, path, tenantF1AvatarProposal, fresh)
	row.Probes[0].Build = tenantF1ProposalBuild(path)
	if strings.Contains(path, "/agents/") {
		p := tenantF1AvatarStyleReference(cred, strings.ReplaceAll(path, "${agent.avatar.propose}", "${own.agent.avatar.propose}"), fresh)
		row.Probes = append(row.Probes, p)
	}
	return row
}

func tenantF1AvatarStyleReference(cred tenantCred, path, fresh string) tenantProbe {
	// Style IDs resolve under the caller's account and realm, separately
	// from the operator path's agent (internal/store/avatar_mutation.go:1167-1177).
	p := tenantProbeFor("foreign-realm-style", foreign, cred, "POST", path, "")
	p.Fresh = fresh
	p.BodyRef = "style_pack_id"
	body := strings.ReplaceAll(tenantF1AvatarProposal, "${", "${own.")
	body = strings.ReplaceAll(body, "${own.avatar.style.id}", "${avatar.style.id}")
	p.Build = tenantF1ProposalBodyBuild(path, body)
	return p
}

func tenantF1SelfProposal() tenantRow {
	row := tenantRow{Pattern: "POST /v1/self/avatar/proposals", Class: tenant, Probes: []tenantProbe{tenantF1AvatarStyleReference(credAgent, "/v1/self/avatar/proposals", "agent.self.propose.foreign")}}
	p := tenantProbeFor("own-proposal", own, credAgent, "POST", "/v1/self/avatar/proposals", tenantF1AvatarProposal)
	p.Fresh = "agent.self.propose"
	p.Build = tenantF1ProposalBuild("/v1/self/avatar/proposals")
	row.Probes = append(row.Probes, p)
	return row
}

func tenantF1TranscriptAppend(batch bool) tenantRow {
	pattern := "POST /v1/transcripts/{transcript}/entries"
	path := "/v1/transcripts/${transcript.append}/entries"
	body := `{"role":"assistant","body":"${canary.transcript.entry}","reply_to_entry_id":"${transcript.entry}"}`
	bodyRef := "reply_to_entry_id"
	fresh := "transcript.append"
	if batch {
		pattern += ":batch"
		path = "/v1/transcripts/${transcript.batch}/entries:batch"
		body = `{"entries":[{"role":"assistant","body":"${canary.transcript.batch}","reply_to_entry_id":"${transcript.entry.batch}"}]}`
		bodyRef = "entries.0.reply_to_entry_id"
		fresh = "transcript.batch"
	}
	row := tenantF1Foreign(pattern, credAgent, path, body, fresh)
	ownPath := strings.ReplaceAll(path, "${", "${own.")
	body = strings.ReplaceAll(body, "${canary.", "${own.canary.")
	p := tenantProbeFor("reply-target", foreign, credAgent, "POST", ownPath, body)
	p.Fresh = fresh
	p.BodyRef = bodyRef
	p.Refuse = []int{400}
	p.RefuseWhy = "internal/store/transcript.go:277-289 rejects a reply outside the own transcript; internal/server/server.go:3980-3982 and 4018-4020 map invalid input to 400."
	row.Probes = append(row.Probes, p)
	return row
}

func tenantF1AccountActions() tenantRow {
	row := tenantF1Exempt("POST /v1/accounts/", exemptSystemToken, credProvision, "/v1/accounts/${account}:contact", `{}`, "Account lifecycle uses provision authority; server.go:4627-4631. Export-backup uses its dedicated token; server.go:5931-5941. Probes never send valid import/provision bodies.")
	row.Probes = append(row.Probes, tenantProbeFor("backup-export", exemption, credBackup, "POST", "/v1/accounts/${account}:export-backup", "{"))
	return row
}

func tenantF1ProposalBuild(path string) func(*tenantFixture, tenantSide) tenantRequest {
	return tenantF1ProposalBodyBuild(path, tenantF1AvatarProposal)
}

func tenantF1ProposalBodyBuild(path, bodyTemplate string) func(*tenantFixture, tenantSide) tenantRequest {
	return func(f *tenantFixture, side tenantSide) tenantRequest {
		req := tenantBuild("POST", path, "", bodyTemplate)(f, side)
		var body map[string]any
		if err := json.Unmarshal(req.Body, &body); err != nil {
			panic("invalid synthetic avatar proposal JSON")
		}
		// Canonical references already satisfy the rendering and locked-layer
		// baseline; a decorative SVG would be refused before an own success.
		body["svg"] = avatar.BuiltInFlatVectorStylePack().References[0].SVG
		encoded, err := json.Marshal(body)
		if err != nil {
			panic("cannot encode synthetic avatar proposal")
		}
		req.Body = encoded
		return req
	}
}

// ExportAccount refuses an entire account with an open vault lifecycle
// (internal/store/export.go:303-324). F2 requires those A/B prerequisites, so
// export gets a dedicated populated account with terminal vault lifecycles.
// ShowsOwn requires its object canaries, not merely the account display name.
func tenantF1Export() tenantRow {
	row := tenantF1Scratch("GET /v1/export", "S-export", "")
	row.Probes[0].ShowsOwn = true
	return row
}
