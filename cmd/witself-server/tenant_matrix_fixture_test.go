package main

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Runtime seeding can populate these same side-indexed slots without changing
// Build functions. No Build may select a concrete side or reach into this map.
type tenantFixture struct {
	bySide map[tenantSide]map[string]string
	// ownValues are existing A objects used beside one foreign body reference.
	ownValues map[string]string
	// Own probes get disjoint, dedicated sideMissing slots seeded by 122b.
	ownProbeValues map[string]map[string]string
	ownScope       string
	accesses       *[]tenantFixtureAccess
}

type tenantFixtureAccess struct {
	source string
	side   tenantSide
	scope  string
	key    string
}

func (f *tenantFixture) value(side tenantSide, key string) string {
	source := "side"
	values := f.bySide[side]
	if f.ownScope != "" && side == sideMissing {
		source = "own-probe"
		values = f.ownProbeValues[f.ownScope]
	}
	if f.accesses != nil {
		*f.accesses = append(*f.accesses, tenantFixtureAccess{source, side, f.ownScope, key})
	}
	value, ok := values[key]
	if !ok || value == "" {
		panic(fmt.Sprintf("unseeded tenant fixture key %q on side %q", key, side))
	}
	return value
}

func (f *tenantFixture) own(key string) string {
	if f.ownScope != "" {
		return f.value(sideMissing, key)
	}
	if f.accesses != nil {
		*f.accesses = append(*f.accesses, tenantFixtureAccess{source: "own", key: key})
	}
	value := f.ownValues[key]
	if value == "" {
		panic(fmt.Sprintf("unseeded own fixture key %q", key))
	}
	return value
}

func tenantOwnBuild(scope string, build func(*tenantFixture, tenantSide) tenantRequest) func(*tenantFixture, tenantSide) tenantRequest {
	return func(f *tenantFixture, _ tenantSide) tenantRequest {
		scoped := *f
		scoped.ownScope = scope
		return build(&scoped, sideMissing)
	}
}

func (f *tenantFixture) values(side tenantSide) []string {
	values := make([]string, 0, len(f.bySide[side]))
	for key, value := range f.bySide[side] {
		if tenantSyntheticShapes[key] == "number" || tenantSyntheticShapes[key] == "time" || tenantSyntheticShapes[key] == "version" {
			continue
		}
		values = append(values, value)
		values = append(values, tenantCanary.FindAllString(value, -1)...)
	}
	sort.Strings(values)
	return values
}

// Fake values are deterministic and never issued. The paired missing side has
// identical prefixes, lengths and alphabets, but shares no IDs/names/canaries.
// Actual IDs use 80 bits encoded as lowercase base32 (internal/id/id.go).
func syntheticTenantFixture() *tenantFixture {
	f := &tenantFixture{bySide: map[tenantSide]map[string]string{}, ownProbeValues: map[string]map[string]string{}}
	for _, side := range []tenantSide{sideB, sideMissing} {
		f.bySide[side] = tenantSyntheticValues(side, "foreign")
	}
	f.ownValues = tenantSyntheticValues("a", "body-reference")
	for _, row := range tenantRows() {
		for _, probe := range row.Probes {
			if probe.Kind == own {
				scope := row.Pattern + "/" + probe.Name
				f.ownProbeValues[scope] = tenantSyntheticValues(sideMissing, scope)
			}
		}
	}
	return f
}

func tenantSyntheticValues(side tenantSide, scope string) map[string]string {
	values := map[string]string{}
	for key, shape := range tenantSyntheticShapes {
		digest := sha256.Sum256([]byte(string(side) + ":" + scope + ":" + key))
		hexSuffix := hex.EncodeToString(digest[:8])
		canary := "tc" + string(side) + hexSuffix
		var value string
		switch shape {
		case "canary":
			value = canary
		case "name":
			value = string(side) + "-" + strings.NewReplacer(".", "-", "_", "-").Replace(key) + "-" + canary
		case "display":
			value = string(side) + " " + key + " " + canary
		case "email":
			value = string(side) + "-" + canary + "@example.invalid"
		case "token":
			value = "tm-" + string(side) + "-" + key + "-" + hexSuffix
		case "subject":
			value = "subject_" + string(side) + "_" + hexSuffix
		case "number":
			value = "1"
		case "version":
			value = strconv.FormatUint(10000000+uint64(binary.BigEndian.Uint32(digest[:4]))%90000000, 10)
		case "time":
			value = "2099-01-01T00:00:00Z"
		case "hash":
			value = hex.EncodeToString(digest[:])
		case "base64url32":
			value = base64.RawURLEncoding.EncodeToString(digest[:])
		case "base64-32":
			value = base64.StdEncoding.EncodeToString(digest[:])
		case "base64-48":
			value = base64.StdEncoding.EncodeToString(append(digest[:], digest[:16]...))
		case "base64-60":
			value = base64.StdEncoding.EncodeToString(append(digest[:], digest[:28]...))
		case "base64-64":
			value = base64.StdEncoding.EncodeToString(append(digest[:], digest[:]...))
		case "uuid":
			b := digest[:16]
			b[6] = (b[6] & 0x0f) | 0x40
			b[8] = (b[8] & 0x3f) | 0x80
			value = fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
		default:
			value = shape + "_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:10]))
		}
		values[key] = value
	}
	return values
}

var tenantCanary = regexp.MustCompile(`tc[abm][0-9a-f]{16}`)

var tenantPlaceholder = regexp.MustCompile(`\$\{([a-zA-Z0-9_.-]+)\}`)

// Templates name fixture slots rather than embedding side-specific data. Path,
// query and JSON string escaping are separate so later real seed names remain
// well-formed. Every invocation returns fresh headers and body bytes.
func tenantBuild(method, requestPath, rawQuery, body string) func(*tenantFixture, tenantSide) tenantRequest {
	return func(f *tenantFixture, side tenantSide) tenantRequest {
		expand := func(s string, escape func(string) string) string {
			return tenantPlaceholder.ReplaceAllStringFunc(s, func(placeholder string) string {
				key := placeholder[2 : len(placeholder)-1]
				if key == "cred" {
					return placeholder // Only the runner resolves the probe credential.
				}
				if strings.HasPrefix(key, "own.") {
					return escape(f.own(strings.TrimPrefix(key, "own.")))
				}
				return escape(f.value(side, key))
			})
		}
		jsonEscape := func(value string) string {
			encoded, _ := json.Marshal(value)
			return string(encoded[1 : len(encoded)-1])
		}
		headers := make(http.Header)
		if body != "" {
			headers.Set("Content-Type", "application/json")
		}
		return tenantRequest{Method: method, Path: expand(requestPath, url.PathEscape), RawQuery: expand(rawQuery, url.QueryEscape), Headers: headers, Body: []byte(expand(body, jsonEscape))}
	}
}

// tenantResolveCredential is pure: the runner supplies exactly the credential
// selected by probe.Cred after choosing the request principal. No fixture token
// is consulted, including for bootstrap's body credential.
func tenantResolveCredential(req tenantRequest, credential string) tenantRequest {
	req.Path = strings.ReplaceAll(req.Path, "${cred}", url.PathEscape(credential))
	req.RawQuery = strings.ReplaceAll(req.RawQuery, "${cred}", url.QueryEscape(credential))
	encoded, _ := json.Marshal(credential)
	req.Body = []byte(strings.ReplaceAll(string(req.Body), "${cred}", string(encoded[1:len(encoded)-1])))
	req.Headers = req.Headers.Clone()
	for key, values := range req.Headers {
		for i, value := range values {
			req.Headers[key][i] = strings.ReplaceAll(value, "${cred}", credential)
		}
	}
	return req
}

func tenantProbeFor(name string, kind probeKind, cred tenantCred, method, requestPath, body string) tenantProbe {
	return tenantProbe{Name: name, Kind: kind, Cred: cred, Build: tenantBuild(method, requestPath, "", body)}
}

// Explicit registry: omitted or misspelled slots panic instead of silently
// inventing an object which the eventual runtime seeder cannot provide.
var tenantSyntheticShapes = map[string]string{
	"account":                         "acc",
	"agent.self.read":                 "agent",
	"agent.subject.upsert":            "agent",
	"adjust_content":                  "canary",
	"agent":                           "agent",
	"agent-name":                      "name",
	"agent.avatar.activate":           "agent",
	"agent.avatar.policy":             "agent",
	"agent.avatar.propose":            "agent",
	"agent.avatar.quota":              "agent",
	"agent.avatar.reject":             "agent",
	"agent.avatar.reset":              "agent",
	"agent.avatar.rollback":           "agent",
	"agent.delete":                    "agent",
	"agent.self.activate":             "agent",
	"agent.self.failure":              "agent",
	"agent.self.propose":              "agent",
	"agent.self.reset":                "agent",
	"agent.self.rollback":             "agent",
	"agent_email_receive":             "agent",
	"agent_email_send":                "agent",
	"avatar-version-id":               "avver",
	"avatar.profile.revision":         "number",
	"avatar.style.id":                 "name",
	"avatar.style.version":            "number",
	"avatar.version":                  "version",
	"canary.account.close":            "canary",
	"canary.account.suspend":          "canary",
	"canary.avatar.description":       "canary",
	"canary.avatar.style.description": "canary",
	"canary.avatar.style.name":        "canary",
	"canary.fact.proposal":            "canary",
	"canary.fact.reason":              "canary",
	"canary.fact.value":               "canary",
	"canary.subject.display":          "canary",
	"canary.ticket.body":              "canary",
	"canary.ticket.reply":             "canary",
	"canary.ticket.subject":           "canary",
	"canary.transcript.batch":         "canary",
	"canary.transcript.entry":         "canary",
	"canary.transcript.title":         "canary",
	"canary_challenge":                "uuid",
	"capture_memory_content":          "canary",
	"capture_message_content":         "canary",
	"capture_transcript_content":      "canary",
	"consume_commitment":              "hash",
	"consume_proof":                   "base64-32",
	"content_hash":                    "hash",
	"curation_key":                    "canary",
	"curation_receipt":                "mrec",
	"curation_request":                "mcrq",
	"curation_request_start":          "mcrq",
	"curation_run":                    "mrun",
	"curation_run_abandon":            "mrun",
	"curation_run_apply":              "mrun",
	"curation_run_cancel":             "mrun",
	"curation_run_plan":               "mrun",
	"curation_run_renew":              "mrun",
	"curation_run_rollback":           "mrun",
	"dek":                             "dek",
	"dek_new":                         "dek",
	"email.contact":                   "email",
	"email_body":                      "canary",
	"email_claim":                     "ecl",
	"email_reply_body":                "canary",
	"email_subject":                   "canary",
	"email_to":                        "email",
	"enrollment":                      "enr",
	"enrollment_approve":              "enr",
	"enrollment_cancel":               "enr",
	"enrollment_consume":              "enr",
	"enrollment_create":               "agent",
	"enrollment_expiry":               "time",
	"enrollment_new":                  "enr",
	"enrollment_receive":              "enr",
	"evidence":                        "mev",
	"fact":                            "fact",
	"fact-subject-id":                 "sub",
	"fact.candidate":                  "fcand",
	"fact.candidate.confirm":          "fcand",
	"fact.candidate.reject":           "fcand",
	"fact.subject":                    "subject",
	"fact.subject.alias":              "subject",
	"field":                           "fld",
	"field_new":                       "fld",
	"inbound_email":                   "emsg",
	"inbound_email_ack":               "emsg",
	"inbound_email_claim":             "emsg",
	"inbound_email_code_consumed":     "emsg",
	"inbound_email_complete":          "emsg",
	"inbound_email_read":              "emsg",
	"inbound_email_release":           "emsg",
	"inbound_email_renew":             "emsg",
	"inbound_email_reply":             "emsg",
	"location_id":                     "loc",
	"location_name":                   "display",
	"memory":                          "mem",
	"memory-version-2-content":        "canary",
	"memory_adjust":                   "mem",
	"memory_delete":                   "mem",
	"memory_forget":                   "mem",
	"memory_reactivate":               "mem",
	"memory_restore":                  "mem",
	"memory_supersede":                "mem",
	"message":                         "msg",
	"message-main-to-peer":            "thr",
	"message-peer-to-main":            "msg",
	"message_ack":                     "msg",
	"message_body":                    "canary",
	"message_claim":                   "msg",
	"message_claim_id":                "mcl",
	"message_complete":                "msg",
	"message_complete_body":           "canary",
	"message_read":                    "msg",
	"message_release":                 "msg",
	"message_renew":                   "msg",
	"message_reply":                   "msg",
	"message_reply_body":              "canary",
	"message_request":                 "mrq",
	"message_request_cancel":          "mrq",
	"message_request_claim":           "mrq",
	"message_request_claim_id":        "mrc",
	"message_request_complete":        "mrq",
	"message_request_decline":         "mrq",
	"message_request_offer":           "mrq",
	"message_request_release":         "mrq",
	"message_request_renew":           "mrq",
	"message_request_select":          "mrq",
	"message_set_body":                "canary",
	"message_subject":                 "canary",
	"name.account.rename":             "name",
	"name.agent.create":               "name",
	"name.agent.email":                "name",
	"name.agent.segment":              "name",
	"name.operator.create":            "name",
	"name.realm.create":               "name",
	"name.subject.alias":              "name",
	"name.subject.create":             "name",
	"name.token.create":               "name",
	"name.token.curator":              "name",
	"name.token.operator":             "name",
	"operator":                        "opr",
	"operator-name":                   "display",
	"operator-token":                  "token",
	"operator.delete":                 "opr",
	"other-name":                      "name",
	"other_agent":                     "agent",
	"outbound_email":                  "esnd",
	"pairing_commitment":              "hash",
	"peer-name":                       "name",
	"peer_agent":                      "agent",
	"plan_hash":                       "hash",
	"realm":                           "realm",
	"realm-name":                      "name",
	"realm.delete":                    "realm",
	"realm.style":                     "realm",
	"realm2":                          "realm",
	"realm2-name":                     "name",
	"realm_email_receive":             "realm",
	"realm_email_send":                "realm",
	"request_body":                    "canary",
	"request_complete_body":           "canary",
	"request_offer_body":              "canary",
	"request_subject":                 "canary",
	"rotation":                        "vkr",
	"rotation_cancel":                 "vkr",
	"rotation_commit":                 "vkr",
	"rotation_new":                    "vkr",
	"rotation_stage":                  "vkr",
	"secret":                          "sec",
	"secret-field-value":              "canary",
	"secret_archive":                  "sec",
	"secret_delete":                   "sec",
	"secret_description":              "canary",
	"secret_name":                     "display",
	"secret_new":                      "sec",
	"secret_restore":                  "sec",
	"source_location_id":              "loc",
	"supersede_content":               "canary",
	"ticket":                          "tkt",
	"ticket.reply":                    "tkt",
	"ticket.state":                    "tkt",
	"token":                           "tok",
	"token.curator-apply":             "token",
	"token.curator-preview":           "token",
	"token.full":                      "token",
	"token.other":                     "token",
	"token.peer":                      "token",
	"token.revoke":                    "tok",
	"transcript":                      "trn",
	"transcript.append":               "trn",
	"transcript.batch":                "trn",
	"transcript.entry":                "ent",
	"transcript.entry.batch":          "ent",
	"vault_ciphertext":                "base64-48",
	"vault_fingerprint":               "hash",
	"vault_key":                       "avk",
	"vault_key_new":                   "avk",
	"vault_key_target":                "avk",
	"vault_public_key":                "base64url32",
	"vault_register":                  "agent",
	"vault_transfer":                  "base64-64",
	"vault_wrapped_dek":               "base64-60",
	"vector_model":                    "canary",
	"vector_profile":                  "mvp",
	"vector_provider":                 "canary",
	"vector_recipe":                   "canary",
	"canary.activity.location":        "canary",
	"curation_plan_content":           "canary",
	"evidence_memory":                 "mev",
	"evidence_message":                "mev",
	"memory_supersede_memory":         "mem",
	"memory_supersede_message":        "mem",
	"name.activity.event":             "name",
	"name.activity.location":          "name",
	"supersede_memory_content":        "canary",
	"supersede_message_content":       "canary",
	"rotation_start":                  "agent",
	"secret_create":                   "agent",
	"agent.self.propose.foreign":      "agent",
}
