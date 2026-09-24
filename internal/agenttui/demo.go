package agenttui

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/witwave-ai/witself/internal/dashboard"
)

// demoSource is deliberately closed: all content is authored below, with no
// client, endpoint, environment, filesystem, or credential lookup.
type demoSource struct {
	mu             sync.Mutex
	theme          string
	accountChecked bool
}

// NewDemoSource returns a deterministic, entirely in-memory synthetic workspace.
func NewDemoSource() Source                 { return &demoSource{theme: "console"} }
func encode(v any) (json.RawMessage, error) { b, e := json.Marshal(v); return b, e }

const demoTime = "2026-09-21T10:40:00Z"

func demoSelf() object {
	return object{
		"identity":          object{"agent_name": "Atlas", "realm_name": "studio", "agent_id": "agent_atlas_demo", "realm_id": "realm_studio_demo"},
		"dashboard_version": "0.1.0-demo", "observational": true, "poll_interval_ms": 2000,
		"index":             object{"counts": object{"transcripts": 3, "facts": 3, "memories": 3, "messages": 5, "secrets": 2}},
		"fact_capacity":     object{"used": 3, "max": nil, "remaining": nil, "unlimited": true},
		"memory_capacity":   object{"used": 3, "max": 20, "remaining": 17, "unlimited": false},
		"memory_checkpoint": object{"pending": true, "enabled": true}, "message_checkpoint": object{"pending": true, "enabled": true},
		"email_checkpoint":  object{"pending": false, "enabled": true, "receive_state": "enabled", "agent_receive_state": "enabled", "realm_receive_state": "enabled"},
		"avatar_checkpoint": object{"pending": false, "reason": "active"},
		"plan_entitlements": object{"state": "applied", "source": "cell_applied_snapshot", "enforced_plan_id": "studio-demo", "features": object{"memory": true, "facts": true, "secrets": true, "messaging": true, "collaboration": true, "agent_email_receive": true, "agent_email_send": true}, "retention_days": object{"transcript_retention_days": 90, "message_retention_days": 30, "agent_email_retention_days": nil}},
		"salient_memories":  []any{object{"id": "mem_wayfinding", "snippet": "Wayfinding should feel calm, even when the workspace is busy.", "kind": "decision", "salience": 0.96}, object{"id": "mem_release", "snippet": "The field guide is ready for its first reader walkthrough.", "kind": "milestone", "salience": 0.88}, object{"id": "mem_notes", "snippet": "Preserve keyboard focus when live data changes.", "kind": "lesson", "salience": 0.82}},
	}
}
func demoTranscripts() []any {
	return []any{
		object{"id": "tr_wayfinding", "title": "Designing a calmer workspace", "external_id": "studio/design/042", "updated_at": demoTime},
		object{"id": "tr_fieldguide", "title": "Field guide · reader walkthrough", "external_id": "studio/review/018", "updated_at": "2026-09-20T16:20:00Z"},
		object{"id": "tr_keyboard", "title": "Keyboard navigation audit", "external_id": "studio/quality/009", "updated_at": "2026-09-19T11:15:00Z"},
	}
}
func demoEntries(id string) []any {
	type turn struct{ role, body string }
	var turns []turn
	switch id {
	case "tr_wayfinding":
		turns = []turn{
			{"user", "Can we make the workspace feel quieter without hiding the useful details?"},
			{"assistant", "Yes. I will keep navigation in a stable rail, give the current task most of the width, and let secondary details recede."},
			{"assistant", `{"tool_name":"design.inspect","arguments":{"surface":"workspace","viewports":["80x24","120x40"],"checks":["focus order","contrast","content hierarchy"]}}`},
			{"tool", "The compact layout needs one clear selected state. The wide layout has room for a persistent inventory and a generous reading pane."},
			{"user", "Keep the keyboard controls visible. A clean interface should still explain itself."},
			{"assistant", "Decision: use one accent for selection, one for information, and reserve the footer for actions that work in the current pane. All seven sections remain one key away."},
			{"assistant", "The first review is ready. Next: test refresh while a reader is halfway through a long transcript."},
		}
	case "tr_fieldguide":
		turns = []turn{{"user", "Review the field guide for someone joining the studio today."}, {"assistant", "I checked the introduction, first task, and reference pages. The examples use a single small project so the reader can follow the whole path."}, {"assistant", `{"tool_name":"guide.check","results":{"broken_links":0,"examples":8,"sections":["Start here","Make a change","Share the result"]}}`}, {"tool", "All eight examples have a concrete starting point and a visible outcome."}, {"assistant", "The guide is ready for a reader walkthrough. We will record unclear steps as observations before changing the text."}}
	default:
		turns = []turn{{"user", "What happens if new items arrive while I am reading?"}, {"assistant", "The current row stays selected by its stable identity, and the reading position stays put. At the end of a live transcript, new turns continue the tail."}, {"tool", "Checked navigation, filtering, detail focus, and evidence return."}, {"assistant", "Keep selection and scroll independent from the incoming inventory order."}}
	}
	out := []any{}
	for i, t := range turns {
		out = append(out, object{"sequence": i + 1, "role": t.role, "body": t.body, "created_at": demoTime})
	}
	return out
}
func demoMemories() []any {
	return []any{
		object{"id": "mem_wayfinding", "kind": "decision", "state": "active", "salience": 0.96, "version": 2, "origin": "agent", "sensitive": false, "tags": []any{"design", "workspace", "accessibility"}, "created_at": "2026-09-19T10:00:00Z", "updated_at": demoTime, "content": "Wayfinding should feel calm, even when the workspace is busy.\n\nKeep all seven sections within one keystroke. Give the active task a generous reading pane, preserve selection during refresh, and use a single bright accent for the current focus.\n\nThe footer explains the actions that work here. Detailed keyboard help remains one key away.", "evidence": []any{object{"id": "ev_design", "transcript_id": "tr_wayfinding", "entry_from_sequence": 4, "entry_until_sequence": 6, "role": "supports", "state": "resolved"}, object{"id": "ev_guide", "external_locator": "https://studio.example.test/notes/wayfinding", "role": "reference", "state": "external"}}},
		object{"id": "mem_release", "kind": "milestone", "state": "active", "salience": 0.88, "version": 1, "origin": "agent", "sensitive": false, "tags": []any{"field-guide", "review"}, "created_at": "2026-09-20T16:20:00Z", "updated_at": "2026-09-20T16:20:00Z", "content": "The field guide is ready for its first reader walkthrough. Eight examples now share one small project, with a concrete starting point and a visible outcome.\n\nNext step: watch a new reader complete the first task and capture confusing steps without coaching.", "evidence": []any{object{"id": "ev_release", "transcript_id": "tr_fieldguide", "entry_from_sequence": 2, "entry_until_sequence": 5, "role": "supports", "state": "resolved"}}},
		object{"id": "mem_notes", "kind": "lesson", "state": "active", "salience": 0.82, "version": 1, "origin": "agent", "sensitive": false, "tags": []any{"keyboard", "live-refresh"}, "created_at": "2026-09-19T11:15:00Z", "updated_at": "2026-09-19T11:15:00Z", "content": "Preserve keyboard focus when live data changes. A refresh updates the record beneath the reader; it should not move their place in the workspace.", "evidence": []any{object{"id": "ev_focus", "transcript_id": "tr_keyboard", "entry_from_sequence": 1, "entry_until_sequence": 4, "role": "supports", "state": "resolved"}}},
	}
}
func demoFacts() []any {
	return []any{
		object{"id": "fact_name", "subject": "self", "predicate": "identity/name", "value": "Atlas", "value_type": "string", "cardinality": "one", "source_kind": "user", "confidence": 1, "sensitive": false, "usage_count": 12, "updated_at": demoTime},
		object{"id": "fact_review", "subject": "project_fieldguide", "predicate": "review/format", "value": "A short walkthrough, followed by written observations", "value_type": "string", "cardinality": "one", "source_kind": "user", "confidence": 1, "sensitive": false, "usage_count": 4, "updated_at": demoTime},
		object{"id": "fact_private", "subject": "project_sandbox", "predicate": "label/internal", "value_type": "string", "cardinality": "one", "source_kind": "user", "confidence": 1, "sensitive": true, "redacted": true, "usage_count": 0, "updated_at": demoTime},
	}
}
func demoMessages() []any {
	atlas := object{"kind": "agent", "agent_id": "agent_atlas_demo", "agent_name": "Atlas"}
	mira := object{"kind": "agent", "agent_id": "agent_mira_demo", "agent_name": "Mira"}
	return []any{
		object{"id": "msg_mira_1", "_dir": "received", "from": mira, "to": atlas, "subject": "Field guide walkthrough", "kind": "note", "created_at": "2026-09-21T09:00:00Z", "read_state": object{"state": "unread"}, "delivery": object{"state": "delivered"}},
		object{"id": "msg_mira_2", "_dir": "sent", "from": atlas, "to": mira, "subject": "Review checklist ready", "kind": "note", "created_at": "2026-09-21T09:10:00Z", "delivery": object{"state": "delivered"}},
		object{"id": "msg_mira_3", "_dir": "received", "from": mira, "to": atlas, "subject": "One useful observation", "kind": "result", "created_at": "2026-09-21T10:30:00Z", "read_state": object{"state": "unread"}, "delivery": object{"state": "delivered"}},
		object{"id": "msg_studio", "_dir": "received", "from": mira, "to": object{"kind": "realm"}, "subject": "Studio notes · Monday", "kind": "announcement", "created_at": "2026-09-21T08:30:00Z", "read_state": object{"state": "read"}, "delivery": object{"state": "delivered"}},
		object{"id": "msg_group", "_dir": "sent", "from": atlas, "to": object{"kind": "agents", "count": 3}, "subject": "Accessibility review packet", "kind": "handoff", "created_at": "2026-09-20T17:00:00Z", "delivery": object{"state": "delivered"}},
	}
}
func demoReceived() []any {
	return []any{
		object{"subject": "Field guide: reader notes", "envelope_sender": "reviewer@example.test", "sender_verification_state": "unverified", "raw_size_bytes": 4218, "parse_state": "parsed", "attachment_count": 1, "attachment_storage_bytes": 2048, "retained_attachment_storage_bytes": 2048, "payload_retention_state": "retained", "spf_result": "pass", "dkim_result": "pass", "dmarc_result": "pass", "spam_verdict": "not_reported", "received_at": demoTime, "delivered_at": demoTime, "read_state": object{"state": "unread"}, "processing": object{"state": "pending", "failure_count": 0}},
		object{"subject": "Workshop agenda · Thursday", "envelope_sender": "organizer@example.test", "sender_verification_state": "unverified", "raw_size_bytes": 1830, "parse_state": "parsed", "attachment_count": 0, "attachment_storage_bytes": 0, "retained_attachment_storage_bytes": 0, "payload_retention_state": "retained", "possible_duplicate": true, "received_at": "2026-09-20T15:00:00Z", "delivered_at": "2026-09-20T15:00:01Z", "read_state": object{"state": "read", "read_at": "2026-09-20T16:00:00Z", "acked_at": "2026-09-20T16:10:00Z"}, "processing": object{"state": "completed", "completed_at": "2026-09-20T16:10:00Z"}},
	}
}
func demoSent() []any {
	return []any{
		object{"from": "atlas@studio.example.test", "reply_to": "studio@example.test", "to": "reviewer@example.test", "subject": "Your walkthrough checklist", "request_kind": "send", "state": "delivered", "provider_state": "delivered", "attempt_count": 1, "queued_at": "2026-09-21T08:40:00Z", "created_at": "2026-09-21T08:40:00Z", "provider_started_at": "2026-09-21T08:40:01Z", "accepted_at": "2026-09-21T08:40:02Z", "delivered_at": "2026-09-21T08:40:04Z", "updated_at": "2026-09-21T08:40:04Z"},
		object{"from": "atlas@studio.example.test", "to": "organizer@example.test", "subject": "Workshop notes attached in the shared guide", "request_kind": "reply", "state": "queued", "provider_state": "not_started", "attempt_count": 0, "queued_at": demoTime, "created_at": demoTime, "updated_at": demoTime},
	}
}
func demoSecrets() []any {
	return []any{
		object{"id": "secret_preview", "name": "Preview workspace", "description": "Credential metadata for the isolated design preview.", "template": "login", "tags": []any{"sandbox", "design"}, "lifecycle": "active", "field_count": 2, "sensitive_field_count": 1, "created_at": "2026-09-10T09:00:00Z", "updated_at": demoTime, "fields": []any{object{"id": "field_user", "name": "username", "kind": "text", "sensitive": false}, object{"id": "field_password", "name": "password", "kind": "password", "sensitive": true}}},
		object{"id": "secret_archive", "name": "Retired review sandbox", "description": "Historical binding; no field values are available here.", "template": "api", "tags": []any{"archive"}, "lifecycle": "archived", "field_count": 1, "sensitive_field_count": 1, "created_at": "2026-08-10T09:00:00Z", "updated_at": "2026-09-01T09:00:00Z", "archived_at": "2026-09-01T09:00:00Z", "fields": []any{object{"id": "field_token", "name": "access token", "kind": "api_key", "sensitive": true}}},
	}
}
func (d *demoSource) Read(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch r.Resource {
	case dashboard.ResourceAccountContext, dashboard.ResourceAccountOverview, dashboard.ResourceAccountClients, dashboard.ResourceAccountClientsScan, dashboard.ResourceAccountPlan, dashboard.ResourceAccountBilling, dashboard.ResourceAccountSupport, dashboard.ResourceAccountSupportTicket, dashboard.ResourceAccountAccess:
		return d.accountRead(r)
	case dashboard.ResourceSummary:
		return encode(object{"summary": demoSummary()})
	case dashboard.ResourceSelf:
		return encode(demoSelf())
	case dashboard.ResourceThemes:
		return encode(object{"themes": themeNames[1:]})
	case dashboard.ResourcePreferences:
		d.mu.Lock()
		defer d.mu.Unlock()
		return encode(object{"preferences": object{"prefs": object{"schema": "witself.dashboard-prefs.v1", "theme": d.theme}}})
	case dashboard.ResourceTranscripts:
		return encode(object{"transcripts": demoTranscripts()})
	case dashboard.ResourceTranscript:
		for _, t := range demoTranscripts() {
			if str(obj(t), "id") == r.ID {
				entries := demoEntries(r.ID)
				from := num(object{"n": r.Query.Get("after_sequence")}, "n")
				limit := num(object{"n": r.Query.Get("limit")}, "n")
				if limit <= 0 {
					limit = 200
				}
				out := []any{}
				for _, e := range entries {
					if num(obj(e), "sequence") > from {
						out = append(out, e)
					}
				}
				if r.Query.Get("tail") == "true" && len(out) > limit {
					out = out[len(out)-limit:]
				} else if len(out) > limit {
					out = out[:limit]
				}
				return encode(object{"transcript": t, "entries": out})
			}
		}
	case dashboard.ResourceFacts:
		return encode(object{"facts": demoFacts()})
	case dashboard.ResourceFactHistory:
		for _, f := range demoFacts() {
			if str(obj(f), "id") == r.ID {
				a := object{"source_kind": "user", "confidence": 1, "observed_at": demoTime}
				if !flag(obj(f), "sensitive") {
					a["value"] = obj(f)["value"]
				}
				return encode(object{"assertions": []any{a}})
			}
		}
	case dashboard.ResourceMemories:
		return encode(object{"items": demoMemories()})
	case dashboard.ResourceMemory:
		for _, m := range demoMemories() {
			if str(obj(m), "id") == r.ID {
				return encode(object{"memory": m})
			}
		}
	case dashboard.ResourceMemoryHistory:
		return encode(object{"versions": []any{object{"version": 2, "operation": "replace", "state": "active", "created_at": demoTime}, object{"version": 1, "operation": "create", "state": "active", "created_at": "2026-09-19T10:00:00Z"}}})
	case dashboard.ResourceMessages:
		out := []any{}
		for _, v := range demoMessages() {
			dir := "inbox"
			if str(obj(v), "_dir") == "sent" {
				dir = "outbox"
			}
			if dir == r.Query.Get("direction") {
				out = append(out, v)
			}
		}
		return encode(object{"messages": out})
	case dashboard.ResourceEmailAddress:
		return encode(object{"address": object{"address": "atlas@studio.example.test", "receive_state": "enabled", "agent_receive_state": "enabled", "realm_receive_state": "enabled"}})
	case dashboard.ResourceEmailStatus:
		return encode(object{"available": true, "status": object{"maximum_raw_bytes": 25 << 20, "attachment_capacity": object{"used": 2048, "max": 10 << 20, "remaining": (10 << 20) - 2048, "unlimited": false}}})
	case dashboard.ResourceEmailReceived:
		out := []any{}
		for _, v := range demoReceived() {
			rs := obj(obj(v)["read_state"])
			if r.Query.Get("unread") == "true" && str(rs, "state") != "unread" {
				continue
			}
			if r.Query.Get("unacked") == "true" && str(rs, "acked_at") != "" {
				continue
			}
			out = append(out, v)
		}
		return encode(object{"messages": out})
	case dashboard.ResourceEmailSent:
		return encode(object{"messages": demoSent()})
	case dashboard.ResourceSecrets:
		return encode(object{"secrets": demoSecrets()})
	case dashboard.ResourceSecret:
		for _, s := range demoSecrets() {
			if str(obj(s), "id") == r.ID {
				return encode(object{"secret": s, "vault_key": object{"id": "vault_demo_public_binding", "key_version": 1, "algorithm": "XChaCha20-Poly1305", "lifecycle_state": "active", "created_at": "2026-09-10T09:00:00Z"}})
			}
		}
	}
	return nil, &dashboard.ReaderError{Status: 404, Code: "unavailable"}
}
func (d *demoSource) RevealFact(ctx context.Context, subject, predicate string) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, v := range demoFacts() {
		f := obj(v)
		if str(f, "subject") == subject && str(f, "predicate") == predicate {
			if flag(f, "sensitive") {
				f["value"] = "Copper Finch · synthetic demo only"
			}
			return encode(object{"fact": f})
		}
	}
	return nil, errors.New("demo fact unavailable")
}
func (d *demoSource) PreviewMessage(ctx context.Context, id string) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bodies := map[string]string{"msg_mira_1": "The first reader walkthrough is booked.\n\nI will note where the reader pauses, what they expect next, and whether the example gives enough context.", "msg_mira_3": "One useful observation: the reader found the right section immediately, then used the keyboard footer without prompting.\n\nThe quiet layout is helping.", "msg_studio": "Monday studio notes\n\n• Field guide walkthrough\n• Keyboard navigation review\n• Share one observation from real reading"}
	if b, ok := bodies[id]; ok {
		return encode(object{"body": b})
	}
	return nil, errors.New("demo preview unavailable")
}
func (d *demoSource) StoreTheme(ctx context.Context, theme string) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !ValidTheme(theme) {
		return nil, errors.New("invalid theme")
	}
	d.mu.Lock()
	d.theme = theme
	d.mu.Unlock()
	return encode(object{"preferences": object{"prefs": object{"schema": "witself.dashboard-prefs.v1", "theme": theme}}})
}
