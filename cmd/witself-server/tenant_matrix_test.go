package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/routeinventory"
)

// The rows are independent of the source inventory: adding a production route
// must require an explicit decision here, not manufacture its own passing row.
type tenantClass string
type probeKind string
type tenantCred string
type tenantSide string

const (
	tenant              tenantClass = "tenant"
	exemptPublic        tenantClass = "public"
	exemptSystemToken   tenantClass = "system-token"
	exemptSignedRelay   tenantClass = "signed-relay"
	exemptProviderToken tenantClass = "provider-token"
	exemptBootstrap     tenantClass = "bootstrap"
	exemptNotMounted    tenantClass = "not-mounted"
	foreign             probeKind   = "foreign"
	own                 probeKind   = "own"
	exemption           probeKind   = "exemption"
	credNone            tenantCred  = "none"
	credAgent           tenantCred  = "agent"
	credOperator        tenantCred  = "operator"
	credCuratorPreview  tenantCred  = "curatorPreview"
	credCuratorApply    tenantCred  = "curatorApply"
	credScratchOperator tenantCred  = "scratchOperator"
	credProvision       tenantCred  = "provision"
	credBackup          tenantCred  = "backup"
	sideB               tenantSide  = "b"
	sideMissing         tenantSide  = "m"
)

type tenantRow struct {
	Pattern      string
	Class        tenantClass
	Reason       string
	NoForeignWhy string
	Handler      string
	Actions      []string
	Probes       []tenantProbe
}

type tenantProbe struct {
	SameRealm           *tenantSameAccountRule
	OtherRealm          *tenantSameAccountRule
	SameAccountAllowed  map[string]string // Peer-visible fixture keys, each with a source citation.
	OtherRealmAllowed   map[string]string // Separate exceptions for the other-realm caller.
	Name                string
	Kind                probeKind
	Cred                tenantCred
	Build               func(*tenantFixture, tenantSide) tenantRequest
	Refuse              []int
	RefuseWhy           string
	Fresh               string
	Scratch             string
	ShowsOwn            bool
	ConfigBound         string
	EmptyList           bool
	EmptyListWhy        string
	BodyRef             string // Dot-separated JSON path; array indices are numeric components.
	NoIdempotencyKey    bool
	NoIdempotencyKeyWhy string
	CallerNamespaced    bool
	// NamespaceBuild challenges a caller-local key using B and missing values.
	// Build remains an ordinary own probe with dedicated missing-side slots.
	NamespaceBuild func(*tenantFixture, tenantSide) tenantRequest
	// Want overrides the default 2xx control status for own probes only.
	Want []int
}

type tenantRequest struct {
	Method   string
	Path     string
	RawQuery string
	Headers  http.Header
	Body     []byte
}

func tenantRows() []tenantRow {
	rows := append(tenantRowsF1(), tenantRowsF2()...)
	for i := range rows {
		tenantSameAccountF1(&rows[i])
		tenantSameAccountF2(&rows[i])
		for j := range rows[i].Probes {
			probe := &rows[i].Probes[j]
			tenantSameAccountLimitations(rows[i].Pattern, probe)
			if probe.Kind == own && probe.Build != nil {
				probe.Build = tenantOwnBuild(rows[i].Pattern+"/"+probe.Name, probe.Build)
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Pattern < rows[j].Pattern })
	return rows
}

// tenantFamily is the single family definition used by the coverage guard and
// by later runtime parts. It deliberately does not depend on the row's class.
func tenantFamily(pattern string) string {
	_, p := tenantPatternParts(pattern)
	for _, prefix := range []string{"/v1/vault/", "/v1/secrets", "/v1/memories", "/v1/memory-", "/v1/messages", "/v1/message-requests", "/v1/email"} {
		if strings.HasPrefix(p, prefix) {
			return "F2"
		}
	}
	if strings.Contains(p, "/email-receive") || strings.Contains(p, "/email-send") {
		return "F2"
	}
	return "F1"
}

func tenantPatternParts(pattern string) (string, string) {
	if method, p, ok := strings.Cut(pattern, " "); ok {
		return method, p
	}
	return "", pattern
}

// This closed policy list is separate from the rows themselves. A row cannot
// opt out of tenant coverage just by changing its Class.
var tenantExemptPatterns = map[string]tenantClass{
	"/v1/version":                          exemptPublic,
	"/v1/capabilities":                     exemptPublic,
	"/livez":                               exemptPublic,
	"/startupz":                            exemptPublic,
	"/healthz":                             exemptPublic,
	"/readyz":                              exemptPublic,
	"/metrics":                             exemptPublic,
	"POST /v1/accounts:provision-exact":    exemptSystemToken,
	"GET /v1/accounts/":                    exemptSystemToken,
	"PATCH /v1/accounts/":                  exemptSystemToken,
	"POST /v1/accounts/":                   exemptSystemToken,
	"POST /v1/support/admin:list-tickets":  exemptSystemToken,
	"POST /v1/support/admin:match-contact": exemptSystemToken,
	"POST /v1/events/admin:list":           exemptSystemToken,
	"POST /v1/internal/agent-email:ingest": exemptSignedRelay,
	"POST /v1/internal/agent-email-send:provider-event": exemptProviderToken,
	"POST /v1/auth/bootstrap":                           exemptBootstrap,
	"POST /v1/accounts":                                 exemptNotMounted,
}

func tenantInventory(t *testing.T) []routeinventory.Route {
	t.Helper()
	root, err := routeinventory.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	routes, err := routeinventory.ServerRoutes(root)
	if err != nil {
		t.Fatal(err)
	}
	return routes
}

func TestTenantMatrixCoversEveryRoute(t *testing.T) {
	f := syntheticTenantFixture()
	rows := tenantRows()
	inventory := map[string]routeinventory.Route{}
	for _, route := range tenantInventory(t) {
		inventory[route.Pattern] = route
	}
	seen := map[string]bool{}
	scratch := map[string]string{}
	classes := map[tenantClass]int{}
	families := map[string]int{}
	kinds := map[probeKind]int{}
	exemptCount := 0
	for _, row := range rows {
		if seen[row.Pattern] {
			t.Errorf("duplicate tenant matrix row %q", row.Pattern)
		}
		seen[row.Pattern] = true
		if _, ok := inventory[row.Pattern]; !ok {
			t.Errorf("stale tenant matrix row %q has no inventory pattern", row.Pattern)
		}
		classes[row.Class]++
		if row.Class == tenant {
			families[tenantFamily(row.Pattern)]++
			if _, exempt := tenantExemptPatterns[row.Pattern]; exempt {
				t.Errorf("route %q must use its declared exempt class", row.Pattern)
			}
		} else {
			exemptCount++
			if class, ok := tenantExemptPatterns[row.Pattern]; !ok || class != row.Class {
				t.Errorf("route %q has unauthorized exempt class %q", row.Pattern, row.Class)
			}
			if !tenantReasonOK(row.Reason) {
				t.Errorf("exempt route %q needs a reason of 1..300 bytes", row.Pattern)
			}
		}
		if len(row.Probes) == 0 {
			t.Errorf("route %q has no probes", row.Pattern)
		}
		names := map[string]bool{}
		// hasPathForeign counts only probes that name B's object in the path. A
		// BodyRef probe uses an A-owned path, so it must not stand in for the
		// path-object probe an id-bearing route or an {action} needs.
		hasForeign, hasPathForeign, hasCurator := false, false, false
		actionProbes := map[string]bool{}
		for _, probe := range row.Probes {
			kinds[probe.Kind]++
			label := fmt.Sprintf("route %q probe %q", row.Pattern, probe.Name)
			if probe.Name == "" || names[probe.Name] {
				t.Errorf("%s has empty or duplicate probe name", label)
			}
			names[probe.Name] = true
			switch probe.Kind {
			case foreign:
				hasForeign = true
				if probe.BodyRef == "" {
					hasPathForeign = true
				}
			case own, exemption:
			default:
				t.Errorf("%s has unknown probe kind %q", label, probe.Kind)
			}
			if (row.Class == tenant) == (probe.Kind == exemption) {
				t.Errorf("%s has a kind inconsistent with its class", label)
			}
			switch probe.Cred {
			case credNone, credAgent, credOperator, credScratchOperator, credProvision, credBackup:
			case credCuratorPreview, credCuratorApply:
				hasCurator = true
			default:
				t.Errorf("%s has unknown credential %q", label, probe.Cred)
			}
			if probe.Refuse != nil {
				if len(probe.Refuse) == 0 || !tenantReasonOK(probe.RefuseWhy) || !tenantSourceCitation.MatchString(probe.RefuseWhy) {
					t.Errorf("%s has Refuse without statuses and a cited 1..300 byte RefuseWhy", label)
				}
				if probe.Kind != foreign {
					t.Errorf("%s uses Refuse on a non-foreign probe", label)
				}
			}
			for _, status := range probe.Want {
				if probe.Kind != own || status == 401 || status == 403 {
					t.Errorf("%s has forbidden own status override %d", label, status)
				}
			}
			if probe.ConfigBound != "" {
				if row.Pattern != "POST /v1/email/retry-canary:arm" && row.Pattern != "POST /v1/email/retry-canary:status" {
					t.Errorf("%s uses ConfigBound outside retry canary", label)
				}
				if probe.Kind != foreign || probe.Cred != credAgent || len(probe.Refuse) != 1 || probe.Refuse[0] != 403 || !tenantReasonOK(probe.ConfigBound) {
					t.Errorf("%s has invalid ConfigBound refusal", label)
				}
			}
			if probe.Kind == own && tenantScratchRoute(row.Pattern) {
				if probe.Cred != credScratchOperator || probe.Scratch == "" {
					t.Errorf("%s needs scratchOperator and a Scratch key", label)
				}
				if prior, ok := scratch[probe.Scratch]; ok {
					t.Errorf("%s reuses scratch account from %s", label, prior)
				}
				scratch[probe.Scratch] = label
			}
			if probe.Build == nil {
				t.Errorf("%s has no Build", label)
				continue
			}
			checkTenantProbePolicy(t, f, row, probe)
			if probe.Kind == foreign && probe.BodyRef == "" {
				last := path.Base(probe.Build(f, sideB).Path)
				for _, action := range row.Actions {
					if strings.HasSuffix(last, ":"+action) {
						actionProbes[action] = true
					}
				}
			}
		}
		_, p := tenantPatternParts(row.Pattern)
		if row.Class == tenant {
			if (strings.Contains(p, "{") || strings.HasSuffix(p, "/")) && !hasPathForeign && tenantCallerNamespaces[row.Pattern] == "" {
				t.Errorf("tenant route %q is id-bearing and needs a foreign probe", row.Pattern)
			}
			if !hasForeign && !tenantReasonOK(row.NoForeignWhy) {
				t.Errorf("tenant route %q needs NoForeignWhy of 1..300 bytes", row.Pattern)
			}
			if strings.HasPrefix(p, "/v1/memory-curation-") && tenantCurationFullOnly[row.Pattern] == "" && !hasCurator {
				t.Errorf("route %q needs a curator credential probe", row.Pattern)
			}
		}
		if citation := tenantCurationFullOnly[row.Pattern]; citation != "" {
			if hasCurator {
				t.Errorf("route %q is full-agent-only (%s)", row.Pattern, citation)
			}
			for _, probe := range row.Probes {
				if probe.Cred != credAgent {
					t.Errorf("route %q requires a full-agent probe (%s)", row.Pattern, citation)
				}
			}
		}
		if row.Pattern == "POST /v1/accounts/" {
			hasBackup := false
			for _, probe := range row.Probes {
				if probe.Cred == credBackup {
					hasBackup = true
					req := probe.Build(f, sideB)
					if probe.Kind != exemption || req.Method != "POST" || req.Path != "/v1/accounts/"+f.value(sideB, "account")+":export-backup" || string(req.Body) != "{" {
						t.Error("backup-token exemption must target export-backup with malformed JSON")
					}
				}
			}
			if !hasBackup {
				t.Error("POST /v1/accounts/ needs a backup credential probe")
			}
		}
		if row.Pattern == "POST /v1/fact-candidates" {
			if hasForeign || !strings.Contains(row.NoForeignWhy, "fact_candidate.go:168-183") {
				t.Error("fact candidate provenance must remain an own row with its opaque-source citation")
			}
		}
		if strings.Contains(p, "{action}") {
			if row.Handler == "" || len(row.Actions) == 0 {
				t.Errorf("action route %q needs Handler and Actions", row.Pattern)
			}
			actions := map[string]bool{}
			for _, action := range row.Actions {
				if action == "" || actions[action] {
					t.Errorf("route %q has empty or duplicate action", row.Pattern)
				}
				actions[action] = true
				if !actionProbes[action] {
					t.Errorf("route %q action %q has no foreign probe", row.Pattern, action)
				}
			}
		}
	}
	for pattern := range tenantRuntimePending {
		t.Errorf("runtime pending must be empty after 122c; found %q", pattern)
	}
	checkTenantPolicySets(t, rows)
	if exemptCount != 18 {
		t.Errorf("exempt route count = %d, want 18", exemptCount)
	}
	for _, route := range tenantInventory(t) {
		if !seen[route.Pattern] {
			site := route.Sites[0]
			t.Errorf("route %q (%s:%d) has no tenant matrix row; add one to cmd/witself-server/tenant_matrix_test.go", route.Pattern, site.File, site.Line)
		}
	}
	t.Logf("rows=%d classes=%v families=%v probes=%v", len(rows), classes, families, kinds)
}

var tenantSourceCitation = regexp.MustCompile(`[^[:space:]]+\.go:[0-9]+`)

func tenantReasonOK(s string) bool { return len(strings.TrimSpace(s)) > 0 && len(s) <= 300 }

// Closed amendment policies are independent of the row declarations. Each
// exception cites the account/owner scoping or permission rule permitting it.
var tenantCurationFullOnly = map[string]string{
	"POST /v1/memory-curation-requests":            "internal/server/memory_curation.go:665-685",
	"POST /v1/memory-curation-runs/{run}/cancel":   "internal/server/memory_curation.go:665-685",
	"POST /v1/memory-curation-runs/{run}/rollback": "internal/server/memory_curation.go:665-685",
}

var tenantCallerNamespaces = map[string]string{
	"PUT /v1/fact-subjects/{subject}":        "internal/store/fact_subject.go:47-99",
	"GET /v1/self/avatar/versions/{version}": "internal/store/avatar_query.go:179-184",
	"POST /v1/self/avatar:activate":          "internal/store/avatar_mutation.go:358-360",
	"POST /v1/self/avatar:rollback":          "internal/store/avatar_mutation.go:489-491",
}

var tenantEmptyLists = map[string]string{
	"GET /v1/realms/{realm}/agents/target":    "internal/store/agent.go:193-214",
	"GET /v1/fact-occurrences/subject-filter": "internal/store/fact_temporal.go:79-98",
	"GET /v1/messages/from-filter":            "internal/store/message.go:1163-1184",
	"GET /v1/messages/thread-filter":          "internal/store/message.go:1163-1184",
}

var tenantNoIdempotencyKeys = map[string]string{
	"DELETE /v1/facts/address-preview":     "internal/server/fact.go:413-425",
	"DELETE /v1/facts/{fact}/id-preview":   "internal/server/fact.go:413-425",
	"DELETE /v1/memories/{memory}/preview": "internal/server/memory.go:842-845",
}

type tenantReferencePin struct{ Field, Citation string }

type tenantQueryPin struct {
	Field, Key, Citation string
	Cred                 tenantCred
}

var tenantQueryReferences = map[string]tenantQueryPin{
	"GET /v1/memory-curation-status/run-filter": {"run_id", "curation_run", "internal/store/memory_curation.go:2462-2467", credCuratorPreview},
	"GET /v1/messages/from-filter":              {"from", "peer_agent", "internal/store/message.go:1163-1184", credAgent},
	"GET /v1/messages/thread-filter":            {"thread_id", "message-main-to-peer", "internal/store/message.go:1163-1184", credAgent},
	"GET /v1/fact-occurrences/subject-filter":   {"subject", "fact.subject", "internal/store/fact_temporal.go:79-98", credAgent},
}

func tenantProbeID(row tenantRow, probe tenantProbe) string { return row.Pattern + "/" + probe.Name }

func checkTenantPolicySets(t *testing.T, rows []tenantRow) {
	t.Helper()
	seen := map[string]bool{}
	patterns := map[string]bool{}
	for _, row := range rows {
		patterns[row.Pattern] = true
		for _, probe := range row.Probes {
			seen[tenantProbeID(row, probe)] = true
		}
	}
	for _, policies := range []map[string]string{tenantEmptyLists, tenantNoIdempotencyKeys} {
		for id, citation := range policies {
			if !seen[id] || !tenantSourceCitation.MatchString(citation) {
				t.Errorf("closed probe policy %q is missing or lacks a citation", id)
			}
		}
	}
	for id, pin := range tenantBodyRefs {
		if !seen[id] || pin.Field == "" || !tenantSourceCitation.MatchString(pin.Citation) {
			t.Errorf("required BodyRef probe %q is missing or uncited", id)
		}
	}
	for id, pin := range tenantQueryReferences {
		if !seen[id] || !tenantSourceCitation.MatchString(pin.Citation) {
			t.Errorf("required query probe %q is missing or uncited", id)
		}
	}
	for _, policies := range []map[string]string{tenantCurationFullOnly, tenantCallerNamespaces} {
		for pattern, citation := range policies {
			if !patterns[pattern] || !tenantSourceCitation.MatchString(citation) {
				t.Errorf("closed route policy %q is missing or lacks a citation", pattern)
			}
		}
	}
}

func checkTenantProbePolicy(t *testing.T, f *tenantFixture, row tenantRow, probe tenantProbe) {
	t.Helper()
	id := tenantProbeID(row, probe)
	req := probe.Build(f, sideB)
	if cite := tenantEmptyLists[id]; probe.EmptyList != (cite != "") || (!probe.EmptyList && probe.EmptyListWhy != "") || (probe.EmptyList && (probe.Kind != foreign || probe.Refuse != nil || !tenantReasonOK(probe.EmptyListWhy) || !strings.Contains(probe.EmptyListWhy, cite))) {
		t.Errorf("probe %q violates the closed EmptyList policy", id)
	}
	if cite := tenantNoIdempotencyKeys[id]; probe.NoIdempotencyKey != (cite != "") || (!probe.NoIdempotencyKey && probe.NoIdempotencyKeyWhy != "") || (probe.NoIdempotencyKey && (!tenantReasonOK(probe.NoIdempotencyKeyWhy) || !strings.Contains(probe.NoIdempotencyKeyWhy, cite))) {
		t.Errorf("probe %q violates the closed preview Idempotency-Key policy", id)
	}
	if probe.NoIdempotencyKey {
		for _, side := range []tenantSide{sideB, sideMissing} {
			r := probe.Build(f, side)
			q, err := url.ParseQuery(r.RawQuery)
			if err != nil || q.Get("dry_run") != "true" || r.Headers.Get("Idempotency-Key") != "" {
				t.Errorf("probe %q omits Idempotency-Key without a valid dry-run preview", id)
			}
		}
	}
	cite := tenantCallerNamespaces[row.Pattern]
	if probe.CallerNamespaced != (cite != "") || (probe.CallerNamespaced && (probe.Kind != own || probe.Fresh == "" || probe.NamespaceBuild == nil || !strings.Contains(row.NoForeignWhy, cite))) || (!probe.CallerNamespaced && probe.NamespaceBuild != nil) {
		t.Errorf("probe %q violates the closed caller-namespace policy", id)
	}
	if probe.ShowsOwn && (probe.Kind != own || !tenantOwnRead(row.Pattern, req.Method)) {
		t.Errorf("probe %q uses ShowsOwn outside an own read/list", id)
	}
	if probe.Kind == own {
		if err := tenantOwnProbeError(f, probe); err != nil {
			t.Errorf("probe %q: %v", id, err)
		}
	}
	var accesses []tenantFixtureAccess
	observed := *f
	observed.accesses = &accesses
	for _, side := range []tenantSide{sideB, sideMissing} {
		probe.Build(&observed, side)
		if probe.NamespaceBuild != nil {
			probe.NamespaceBuild(&observed, side)
		}
	}
	for _, access := range accesses {
		if tenantSyntheticShapes[access.key] == "token" {
			t.Errorf("probe %q reads a fixture credential; use ${cred}", id)
		}
		if probe.Kind == foreign && access.source == "own-probe" {
			t.Errorf("probe %q reads a dedicated own-probe slot", id)
		}
	}
	if row.Pattern == "POST /v1/auth/bootstrap" {
		var body map[string]string
		if json.Unmarshal(req.Body, &body) != nil || body["bootstrap_token"] != "${cred}" || probe.Cred != credAgent {
			t.Error("bootstrap exemption must defer ${cred} to the runner's agent credential")
		}
	}
	if pin := tenantBodyRefs[id]; probe.BodyRef != pin.Field || (probe.BodyRef != "" && probe.Kind != foreign) {
		t.Errorf("probe %q violates the closed BodyRef field policy", id)
	}
	if probe.BodyRef != "" && strings.Contains(row.Pattern, "{") {
		ownPath := false
		for _, access := range accesses {
			if access.source == "own" && strings.Contains(req.Path, f.own(access.key)) {
				ownPath = true
			}
		}
		if !ownPath {
			t.Errorf("probe %q BodyRef must use an A-owned path fixture", id)
		}
	}
	if pin, ok := tenantQueryReferences[id]; ok {
		for _, side := range []tenantSide{sideB, sideMissing} {
			r := probe.Build(f, side)
			q, err := url.ParseQuery(r.RawQuery)
			if err != nil || q.Get(pin.Field) != f.value(side, pin.Key) || probe.Kind != foreign || probe.Cred != pin.Cred || r.Method != "GET" {
				t.Errorf("probe %q lost its required query reference", id)
			}
			if pin.Field == "thread_id" && q.Get("direction") != "outbox" {
				t.Errorf("probe %q needs outbox for its main-to-peer control", id)
			}
		}
	}
	if probe.CallerNamespaced {
		if err := tenantNamespaceError(f, row, probe); err != nil {
			t.Errorf("probe %q: %v", id, err)
		}
	}
}

func tenantNamespaceError(f *tenantFixture, row tenantRow, probe tenantProbe) error {
	key, fresh := "avatar.version", ""
	switch row.Pattern {
	case "PUT /v1/fact-subjects/{subject}":
		key, fresh = "fact.subject", "agent.subject.upsert"
	case "GET /v1/self/avatar/versions/{version}":
		fresh = "agent.self.read"
	case "POST /v1/self/avatar:activate":
		fresh = "agent.self.activate"
	case "POST /v1/self/avatar:rollback":
		fresh = "agent.self.rollback"
	}
	if fresh == "" || probe.Fresh != fresh || probe.Cred != credAgent || probe.NamespaceBuild == nil {
		return fmt.Errorf("namespace challenge lacks its dedicated A principal")
	}
	var normalized []tenantRequest
	for _, side := range []tenantSide{sideB, sideMissing} {
		var accesses []tenantFixtureAccess
		observed := *f
		observed.accesses = &accesses
		req := probe.NamespaceBuild(&observed, side)
		refs := 0
		for _, access := range accesses {
			if access.source == "side" && access.side == side && access.key == key {
				refs++
				continue
			}
			if access.source != "own" {
				return fmt.Errorf("namespace challenge uses another side fixture")
			}
		}
		value := f.value(side, key)
		if refs != 1 || !tenantRequestContains(req, value) {
			return fmt.Errorf("namespace challenge must carry its requested-side key exactly once")
		}
		req.Path = strings.ReplaceAll(req.Path, value, "<namespace>")
		req.RawQuery = strings.ReplaceAll(req.RawQuery, value, "<namespace>")
		req.Body = []byte(strings.ReplaceAll(string(req.Body), value, "<namespace>"))
		normalized = append(normalized, req)
	}
	if !reflect.DeepEqual(normalized[0], normalized[1]) {
		return fmt.Errorf("namespace challenge varies more than its caller-local key")
	}
	return nil
}

func tenantOwnRead(pattern, method string) bool {
	return method == "GET" || (method == "POST" && (pattern == "POST /v1/memories:recall" || pattern == "POST /v1/messages:listen" || pattern == "POST /v1/email:listen"))
}

func tenantOwnProbeError(f *tenantFixture, probe tenantProbe) error {
	var accesses []tenantFixtureAccess
	observed := *f
	observed.accesses = &accesses
	b, m := probe.Build(&observed, sideB), probe.Build(&observed, sideMissing)
	if !reflect.DeepEqual(b, m) {
		return fmt.Errorf("own Build depends on the foreign side")
	}
	for _, access := range accesses {
		if access.source != "own-probe" || access.side != sideMissing || access.scope == "" {
			return fmt.Errorf("own Build must use dedicated sideMissing slots")
		}
	}
	for _, value := range f.values(sideB) {
		if tenantRequestContains(b, value) {
			return fmt.Errorf("own Build contains a B value")
		}
	}
	return nil
}

func tenantScratchRoute(pattern string) bool {
	switch pattern {
	case "POST /v1/account:close", "POST /v1/account:suspend", "POST /v1/account:resume", "POST /v1/account:rename", "PATCH /v1/account/placement-policy", "POST /v1/operators/self/tokens":
		return true
	default:
		return false
	}
}

func TestTenantMatrixProbesTargetTheirOwnRoute(t *testing.T) {
	f := syntheticTenantFixture()
	mux := http.NewServeMux()
	var matched string
	for _, route := range tenantInventory(t) {
		mux.HandleFunc(route.Pattern, func(w http.ResponseWriter, r *http.Request) { matched = r.Pattern; w.WriteHeader(204) })
	}
	for _, row := range tenantRows() {
		for _, probe := range row.Probes {
			t.Run(row.Pattern+"/"+probe.Name, func(t *testing.T) {
				if probe.Build == nil {
					t.Fatal("missing Build")
				}
				if probe.BodyRef != "" {
					if err := tenantBodyRefError(f, probe); err != nil {
						t.Error(err)
					}
				}
				if probe.NamespaceBuild != nil {
					for _, side := range []tenantSide{sideB, sideMissing} {
						req := probe.NamespaceBuild(f, side)
						u := &url.URL{Path: req.Path, RawQuery: req.RawQuery}
						matched = ""
						mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(req.Method, u.String(), strings.NewReader(string(req.Body))))
						if matched != row.Pattern {
							t.Errorf("namespace challenge resolved %q, want %q", matched, row.Pattern)
						}
					}
				}
				for _, side := range []tenantSide{sideB, sideMissing} {
					req := probe.Build(f, side)
					if row.Class == tenant && len(req.Body) > 0 && !json.Valid(req.Body) {
						t.Error("tenant request body is not valid JSON")
					}
					if req.Method == "" {
						t.Fatal("request needs an explicit method")
					}
					u := &url.URL{Path: req.Path, RawQuery: req.RawQuery}
					r := httptest.NewRequest(req.Method, u.String(), strings.NewReader(string(req.Body)))
					r.Header = req.Headers.Clone()
					matched = ""
					mux.ServeHTTP(httptest.NewRecorder(), r)
					if matched != row.Pattern {
						t.Errorf("side %s resolved %q, want %q", side, matched, row.Pattern)
					}
					if probe.Kind == foreign && probe.ConfigBound == "" {
						carriesOwner := false
						for _, value := range f.values(sideB) {
							if tenantRequestContains(req, value) {
								carriesOwner = true
								break
							}
						}
						if side == sideB && !carriesOwner {
							t.Error("foreign request contains no owner-side value")
						}
						if side == sideMissing && carriesOwner {
							t.Error("missing request contains an owner-side value")
						}
					}
				}
			})
		}
	}
}

// A BodyRef probe must reach an A-owned path object and isolate exactly one
// owner-side scalar, once, in the declared JSON field. Diagnostics are value-free.
func tenantBodyRefError(f *tenantFixture, probe tenantProbe) error {
	for _, side := range []tenantSide{sideB, sideMissing} {
		req := probe.Build(f, side)
		var body any
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return fmt.Errorf("BodyRef requires valid JSON")
		}
		leaf := body
		for _, part := range strings.Split(probe.BodyRef, ".") {
			switch node := leaf.(type) {
			case map[string]any:
				leaf = node[part]
			case []any:
				i, err := strconv.Atoi(part)
				if err != nil || i < 0 || i >= len(node) {
					return fmt.Errorf("BodyRef array path is absent")
				}
				leaf = node[i]
			default:
				return fmt.Errorf("BodyRef field is absent")
			}
		}
		value, ok := leaf.(string)
		if !ok {
			return fmt.Errorf("BodyRef must name a string reference")
		}
		matches := 0
		for key, candidate := range f.bySide[side] {
			shape := tenantSyntheticShapes[key]
			if shape == "number" || shape == "version" || shape == "time" {
				continue
			}
			if value == candidate {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("BodyRef must carry exactly one requested-side reference")
		}
		encoded, _ := json.Marshal(value)
		if strings.Count(string(req.Body), string(encoded)) != 1 {
			return fmt.Errorf("BodyRef reference occurs outside its declared field")
		}
		// Remove the one allowed field value and search all remaining request surfaces.
		masked := req
		masked.Body = []byte(strings.Replace(string(req.Body), string(encoded), `""`, 1))
		for _, candidate := range f.values(side) {
			if tenantRequestContains(masked, candidate) {
				return fmt.Errorf("BodyRef request contains another requested-side value")
			}
		}
		if side == sideMissing {
			for _, candidate := range f.values(sideB) {
				if tenantRequestContains(req, candidate) {
					return fmt.Errorf("BodyRef missing request carries a B value")
				}
			}
		}
	}
	return nil
}
