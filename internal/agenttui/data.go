// Package agenttui presents the agent console as a native, observational terminal workspace.
// All authority stays in Source. The package opens no files, sockets, browsers, or subprocesses.
package agenttui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/witwave-ai/witself/internal/dashboard"
)

type object map[string]any

func obj(v any) object {
	if x, ok := v.(map[string]any); ok {
		return x
	}
	if x, ok := v.(object); ok {
		return x
	}
	return object{}
}
func str(o object, k string) string {
	switch v := o[k].(type) {
	case string:
		return v
	case json.Number:
		return string(v)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}
func num(o object, k string) int   { n, _ := strconv.Atoi(str(o, k)); return n }
func flag(o object, k string) bool { b, _ := o[k].(bool); return b }
func list(o object, k string) []object {
	a, _ := o[k].([]any)
	out := make([]object, 0, len(a))
	for _, v := range a {
		out = append(out, obj(v))
	}
	return out
}
func stringsOf(v any) string {
	a, _ := v.([]any)
	ss := make([]string, 0, len(a))
	for _, v := range a {
		if s, ok := v.(string); ok {
			ss = append(ss, s)
		}
	}
	return strings.Join(ss, ", ")
}
func first(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
func single(s string) string { return strings.Join(strings.Fields(clean(s)), " ") }

// clean consumes terminal sequences before removing controls. In particular, a
// C1 OSC introducer must not leave its contents executable after normalization.
// Newlines are the only retained controls; tabs become ordinary spaces.
func clean(s string) string {
	var b strings.Builder
	state := 0 // 0 text; 1 ESC; 2 CSI; 3 control string; 4 ESC in control string
	for _, r := range strings.ToValidUTF8(s, "�") {
		switch state {
		case 1:
			switch r {
			case '[':
				state = 2
			case ']', 'P', 'X', '^', '_':
				state = 3
			default:
				if r >= 0x30 && r <= 0x7e {
					state = 0
				}
			}
			continue
		case 2:
			if r >= 0x40 && r <= 0x7e {
				state = 0
			}
			continue
		case 3:
			switch r {
			case 7, 0x9c:
				state = 0
			case 27:
				state = 4
			}
			continue
		case 4:
			if r == '\\' || r == 7 || r == 0x9c {
				state = 0
			} else if r != 27 {
				state = 3
			}
			continue
		}
		switch r {
		case 27:
			state = 1
			continue
		case 0x9b:
			state = 2
			continue
		case 0x90, 0x98, 0x9d, 0x9e, 0x9f:
			state = 3
			continue
		case '\t':
			b.WriteByte(' ')
			continue
		case '\n':
			b.WriteByte('\n')
			continue
		}
		if unicode.IsControl(r) || r == 0x061c || r == 0x200e || r == 0x200f || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x206f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func safeValue(v any) any {
	switch x := v.(type) {
	case string:
		return clean(x)
	case []any:
		out := make([]any, 0, len(x))
		for _, v := range x {
			out = append(out, safeValue(v))
		}
		return out
	case map[string]any:
		out := object{}
		for k, v := range x {
			out[clean(k)] = safeValue(v)
		}
		return out
	case object:
		out := object{}
		for k, v := range x {
			out[clean(k)] = safeValue(v)
		}
		return out
	default:
		return v
	}
}

// Metadata slots accept scalar values only. A hostile object in a numeric or
// string slot must never sneak an unprojected body/value into a generic renderer.
func pick(o object, fields string) object {
	out := object{}
	for _, k := range strings.Fields(fields) {
		v, ok := o[k]
		if !ok {
			continue
		}
		switch v.(type) {
		case nil, string, bool, json.Number, float64, int, int64:
			out[k] = safeValue(v)
		case []any:
			if k != "tags" && k != "links" && k != "themes" {
				continue
			}
			a := []any{}
			for _, x := range v.([]any) {
				if text, ok := x.(string); ok {
					a = append(a, clean(text))
				}
			}
			out[k] = a
		}
	}
	return out
}
func identity(o object, fields string) object {
	out := pick(o, fields)
	for _, k := range strings.Fields(fields) {
		if s, ok := o[k].(string); ok && (clean(s) != s || strings.ContainsAny(s, "\n\t\r")) {
			out[k] = ""
		}
	}
	return out
}
func projectRows(o object, key string, f func(object) object) []any {
	out := []any{}
	for _, v := range list(o, key) {
		out = append(out, f(v))
	}
	return out
}
func fact(o object) object {
	out := pick(o, "value_type cardinality source_kind confidence sensitive redacted usage_count created_at updated_at observed_at")
	for k, v := range identity(o, "id subject predicate") {
		out[k] = v
	}
	if !flag(o, "sensitive") && !flag(o, "redacted") {
		out["value"] = safeValue(o["value"])
		out["source_ref"] = safeValue(o["source_ref"])
	}
	return out
}
func memory(o object) object {
	out := pick(o, "kind state state_reason salience version origin operation sensitive redacted tags links created_at updated_at occurred_from occurred_until")
	out["id"] = identity(o, "id")["id"]
	if flag(o, "sensitive") || flag(o, "redacted") {
		out["redacted"] = true
		delete(out, "tags")
		delete(out, "links")
		delete(out, "state_reason")
		delete(out, "occurred_from")
		delete(out, "occurred_until")
		return out
	}
	if !flag(o, "redacted") {
		out["content"] = safeValue(o["content"])
		out["snippet"] = safeValue(o["snippet"])
	}
	out["evidence"] = projectRows(o, "evidence", func(e object) object {
		p := pick(e, "role state type entry_from_sequence entry_until_sequence external_locator unavailable_reason unresolvable_reason source_memory_version")
		for k, v := range identity(e, "id transcript_id source_memory_id message_id import_artifact_id") {
			p[k] = v
		}
		return p
	})
	return out
}
func secret(o object) object {
	out := pick(o, "name description template tags lifecycle field_count sensitive_field_count created_at updated_at archived_at")
	out["id"] = identity(o, "id")["id"]
	out["fields"] = projectRows(o, "fields", func(f object) object {
		p := pick(f, "name kind sensitive")
		p["id"] = identity(f, "id")["id"]
		return p
	})
	return out
}

const emailReceivedFields = "envelope_sender subject raw_size_bytes parse_state parse_error_code attachment_count attachment_storage_bytes retained_attachment_storage_bytes payload_retention_state spf_result dkim_result dmarc_result spam_verdict sender_verification_state possible_duplicate received_at delivered_at folder"
const emailSentFields = "from reply_to to subject state provider_state error_code request_kind attempt_count queued_at created_at updated_at provider_started_at accepted_at delivered_at deferred_at failed_at ambiguous_at canceled_at"

func receivedEmail(o object) object {
	out := pick(o, emailReceivedFields)
	out["read_state"] = pick(obj(o["read_state"]), "state read_at acked_at code_consumed_at")
	out["processing"] = pick(obj(o["processing"]), "state failure_count completed_at")
	return out
}
func message(o object) object {
	out := pick(o, "subject kind created_at updated_at")
	out["id"] = identity(o, "id")["id"]
	out["from"] = pick(obj(o["from"]), "kind agent_id agent_name")
	out["to"] = pick(obj(o["to"]), "kind agent_id agent_name count")
	out["read_state"] = pick(obj(o["read_state"]), "state")
	out["delivery"] = pick(obj(o["delivery"]), "state")
	return out
}

const capacityFields = "used max remaining unlimited near_limit at_limit over_limit unavailable"

// decode retains only each view's declared fields. Unknown future fields never
// enter the UI cache, filter corpus, or generic render paths.
func decode(r dashboard.Resource, raw json.RawMessage) (object, error) {
	if len(raw) > 8<<20 {
		return nil, fmt.Errorf("response too large")
	}
	var o object
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	if !json.Valid(raw) || d.Decode(&o) != nil || o == nil {
		return nil, fmt.Errorf("invalid response")
	}
	out := object{}
	switch r {
	case dashboard.ResourceSummary:
		return projectSummary(raw)
	case dashboard.ResourceSelf:
		out = pick(o, "observational dashboard_version poll_interval_ms")
		out["identity"] = pick(obj(o["identity"]), "agent_name realm_name agent_id realm_id")
		out["index"] = object{"counts": pick(obj(obj(o["index"])["counts"]), "transcripts facts memories messages secrets")}
		for _, k := range []string{"memory_capacity", "fact_capacity"} {
			out[k] = pick(obj(o[k]), capacityFields)
		}
		for _, k := range []string{"memory_checkpoint", "message_checkpoint", "email_checkpoint", "avatar_checkpoint"} {
			out[k] = pick(obj(o[k]), "pending enabled unavailable reason receive_state agent_receive_state realm_receive_state")
		}
		if e, ok := o["plan_entitlements"]; ok {
			p := pick(obj(e), "state enforced_plan_id source")
			p["features"] = pick(obj(obj(e)["features"]), "memory facts secrets messaging collaboration agent_email_receive agent_email_send")
			p["retention_days"] = pick(obj(obj(e)["retention_days"]), "transcript_retention_days message_retention_days agent_email_retention_days")
			out["plan_entitlements"] = p
		}
		out["salient_memories"] = projectRows(o, "salient_memories", memory)
	case dashboard.ResourcePreferences:
		out["prefs"] = pick(obj(obj(o["preferences"])["prefs"]), "theme")
	case dashboard.ResourceThemes:
		out = pick(o, "themes")
	case dashboard.ResourceTranscripts:
		out["transcripts"] = projectRows(o, "transcripts", projectTranscript)
	case dashboard.ResourceTranscript:
		out["transcript"] = projectTranscript(obj(o["transcript"]))
		out["entries"] = projectRows(o, "entries", func(t object) object { return pick(t, "role sequence body created_at") })
	case dashboard.ResourceFacts:
		out["facts"] = projectRows(o, "facts", fact)
	case dashboard.ResourceFactHistory:
		out["truncated"] = flag(o, "truncated")
		out["assertions"] = projectRows(o, "assertions", func(t object) object {
			p := pick(t, "source_kind confidence observed_at created_at sensitive redacted")
			if !flag(t, "sensitive") && !flag(t, "redacted") {
				p["value"] = safeValue(t["value"])
			}
			return p
		})
	case dashboard.ResourceMemories:
		out["items"] = projectRows(o, "items", memory)
	case dashboard.ResourceMemory:
		out["memory"] = memory(obj(o["memory"]))
	case dashboard.ResourceMemoryHistory:
		out["versions"] = projectRows(o, "versions", func(t object) object { return pick(t, "version operation state created_at") })
	case dashboard.ResourceMessages:
		out["messages"] = projectRows(o, "messages", message)
	case dashboard.ResourceEmailAddress:
		out["address"] = pick(obj(o["address"]), "address provisioning_kind receive_state agent_receive_state realm_receive_state created_at updated_at disabled_at retired_at")
	case dashboard.ResourceEmailStatus:
		status := obj(o["status"])
		out = pick(status, "maximum_raw_bytes")
		out["attachment_capacity"] = pick(obj(status["attachment_capacity"]), capacityFields)
	case dashboard.ResourceEmailReceived:
		out["messages"] = projectRows(o, "messages", receivedEmail)
	case dashboard.ResourceEmailSent:
		out["messages"] = projectRows(o, "messages", func(t object) object { return pick(t, emailSentFields) })
	case dashboard.ResourceSecrets:
		out["secrets"] = projectRows(o, "secrets", secret)
	case dashboard.ResourceSecret:
		out["secret"] = secret(obj(o["secret"]))
		out["vault_key"] = pick(obj(o["vault_key"]), "id key_version algorithm fingerprint lifecycle_state created_at")
	}
	return out, nil
}
func valueText(v any) string {
	if v == nil {
		return "not reported"
	}
	if s, ok := v.(string); ok {
		return clean(s)
	}
	b, e := json.Marshal(v)
	if e != nil {
		return "unavailable"
	}
	return clean(string(b))
}

type item struct {
	key, title, subtitle, search string
	data                         object
	messages                     []object
}

func makeItem(key, title, subtitle string, d object) item {
	return item{key: key, title: single(title), subtitle: single(subtitle), search: strings.ToLower(single(title + " " + subtitle)), data: d}
}
func makeRows(panel int, data map[dashboard.Resource]object, self object, sent bool) []item {
	rows := []item{}
	switch panel {
	case 0:
		for _, m := range list(self, "salient_memories") {
			title := first(str(m, "snippet"), str(m, "content"), str(m, "id"))
			if flag(m, "redacted") {
				title = "[redacted memory]"
			}
			rows = append(rows, makeItem(str(m, "id"), title, str(m, "kind")+" · "+str(m, "salience"), m))
		}
	case 1:
		for _, t := range list(data[dashboard.ResourceTranscripts], "transcripts") {
			rows = append(rows, makeItem(str(t, "id"), first(str(t, "title"), str(t, "external_id"), str(t, "id")), str(t, "updated_at"), t))
			rows[len(rows)-1].search += " " + strings.ToLower(single(str(t, "id")+" "+str(t, "external_id")+" "+strings.Join(transcriptColumns(t), " ")))
		}
	case 2:
		for _, f := range list(data[dashboard.ResourceFacts], "facts") {
			v := "[sensitive · hidden]"
			if !flag(f, "sensitive") && !flag(f, "redacted") {
				v = valueText(f["value"])
			}
			rows = append(rows, makeItem(str(f, "id"), str(f, "subject")+" / "+str(f, "predicate"), v, f))
		}
	case 3:
		for _, m := range list(data[dashboard.ResourceMemories], "items") {
			s := str(m, "content")
			if flag(m, "redacted") {
				s = "[redacted memory]"
			}
			rows = append(rows, makeItem(str(m, "id"), first(s, str(m, "id")), str(m, "kind")+" · "+str(m, "state")+" · "+str(m, "salience")+" · "+stringsOf(m["tags"]), m))
		}
	case 4:
		groups := map[string]*item{}
		for _, msg := range list(data[dashboard.ResourceMessages], "messages") {
			to := obj(msg["to"])
			peer := obj(msg["from"])
			if str(msg, "_dir") == "sent" {
				peer = to
			}
			k := "peer:" + str(peer, "agent_id")
			label := first(str(peer, "agent_name"), str(peer, "agent_id"), "Unknown peer")
			if str(to, "kind") == "realm" {
				k = "audience:realm"
				label = "Realm broadcast"
			}
			if str(to, "kind") == "agents" {
				k = "audience:agents"
				label = "Multiple agents"
			}
			g := groups[k]
			if g == nil {
				x := makeItem(k, label, "", object{})
				g = &x
				groups[k] = g
			}
			g.messages = append(g.messages, msg)
		}
		for _, g := range groups {
			sort.SliceStable(g.messages, func(i, j int) bool {
				a, b := g.messages[i], g.messages[j]
				if str(a, "created_at") == str(b, "created_at") {
					return str(a, "id") < str(b, "id")
				}
				return str(a, "created_at") < str(b, "created_at")
			})
			n := 0
			for _, msg := range g.messages {
				if str(msg, "_dir") == "received" && str(obj(msg["read_state"]), "state") == "unread" {
					n++
				}
				g.search += " " + strings.ToLower(single(str(msg, "subject")))
			}
			latest := g.messages[len(g.messages)-1]
			g.data["updated_at"] = str(latest, "created_at")
			g.subtitle = fmt.Sprintf("%d messages · %d unread", len(g.messages), n)
			rows = append(rows, *g)
		}
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := str(rows[i].data, "updated_at"), str(rows[j].data, "updated_at")
			if a == b {
				return rows[i].key < rows[j].key
			}
			return a > b
		})
	case 5:
		r := dashboard.ResourceEmailReceived
		if sent {
			r = dashboard.ResourceEmailSent
		}
		// Email projections intentionally have no identity. Keep only the positional
		// selection on refresh; never manufacture an action identity from metadata.
		for _, e := range list(data[r], "messages") {
			sub := "unverified: " + str(e, "envelope_sender")
			if sent {
				sub = str(e, "state") + " · " + str(e, "to")
			}
			rows = append(rows, makeItem("", first(str(e, "subject"), "(no subject)"), sub, e))
		}
	case 6:
		for _, s := range list(data[dashboard.ResourceSecrets], "secrets") {
			rows = append(rows, makeItem(str(s, "id"), first(str(s, "name"), str(s, "id")), str(s, "lifecycle")+" · "+str(s, "field_count")+" fields · "+stringsOf(s["tags"]), s))
			rows[len(rows)-1].search += " " + strings.ToLower(single(str(s, "description")+" "+str(s, "template")))
			for _, field := range list(s, "fields") {
				rows[len(rows)-1].search += " " + strings.ToLower(single(str(field, "name")+" "+str(field, "kind")))
			}

		}
	}
	return rows
}
