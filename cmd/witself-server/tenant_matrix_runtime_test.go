package main

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/routeinventory"
	"github.com/witwave-ai/witself/internal/store"
)

const sideA tenantSide = "a"

type tenantRuntime struct {
	f                 *tenantFixture
	st                *store.Store
	db                *sql.DB
	schema, dsn       string
	creds             map[tenantSide]map[tenantCred]string
	freshCreds        map[tenantSide]map[string]string
	freshProfileCreds map[tenantSide]map[string]map[tenantCred]string
	scratchCreds      map[string]string
	probeValues       map[tenantSide]map[string]map[string]string
	urls              map[routeinventory.Listener]string
	client            *http.Client
	secrets           []string
	isolationDetected bool
}

func tenantRandom() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("tenant matrix random source failed")
	}
	return hex.EncodeToString(b[:])
}

func (rt *tenantRuntime) credential(side tenantSide, p tenantProbe) string {
	if p.Cred == credNone {
		return ""
	}
	if p.Cred == credScratchOperator {
		return rt.scratchCreds[p.Scratch]
	}
	if credential := rt.freshProfileCreds[side][p.Fresh][p.Cred]; p.Fresh != "" && credential != "" {
		return credential
	}
	if p.Cred == credAgent && p.Fresh != "" && rt.freshCreds[side][p.Fresh] != "" {
		return rt.freshCreds[side][p.Fresh]
	}
	return rt.creds[side][p.Cred]
}

func (rt *tenantRuntime) fixture(row tenantRow, p tenantProbe, control bool) *tenantFixture {
	f := *rt.f
	f.bySide = make(map[tenantSide]map[string]string)
	for side, values := range rt.f.bySide {
		f.bySide[side] = make(map[string]string)
		for k, v := range values {
			f.bySide[side][k] = v
		}
		for k, v := range rt.probeValues[side][tenantProbeID(row, p)] {
			f.bySide[side][k] = v
		}
	}
	if control {
		// BodyRef controls use the equivalent owner-side accessor. Leaving A's
		// path here would test a second foreign path instead of B's valid control.
		f.ownValues = f.bySide[sideB]
	} else {
		f.ownValues = make(map[string]string)
		for k, v := range rt.f.ownValues {
			f.ownValues[k] = v
		}
		for k, v := range rt.probeValues[sideA][tenantProbeID(row, p)] {
			f.ownValues[k] = v
		}
	}
	return &f
}

func (rt *tenantRuntime) send(t *testing.T, row tenantRow, p tenantProbe, req tenantRequest, credential string) *capturedResponse {
	t.Helper()
	req = tenantResolveCredential(req, credential)
	listener := routeinventory.ListenerAPI
	switch row.Pattern {
	case "/livez", "/startupz", "/healthz", "/readyz":
		listener = routeinventory.ListenerHealth
	case "/metrics":
		listener = routeinventory.ListenerMetrics
	}
	target := rt.urls[listener] + req.Path
	if req.RawQuery != "" {
		target += "?" + req.RawQuery
	}
	r, err := http.NewRequest(req.Method, target, bytes.NewReader(req.Body))
	if err != nil {
		t.Fatal("construct matrix request failed")
	}
	r.Header = req.Headers.Clone()
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	if credential != "" {
		r.Header.Set("Authorization", "Bearer "+credential)
	}
	if !p.NoIdempotencyKey {
		r.Header.Set("Idempotency-Key", "tm-"+tenantRandom())
	}
	resp, err := rt.client.Do(r)
	if err != nil {
		t.Fatal("matrix HTTP request failed (details withheld to protect request credentials)")
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal("read matrix response failed")
	}
	body, err = tenantInspectionBody(resp.Header, body)
	if err != nil {
		t.Fatal("decode matrix archive response failed")
	}
	return &capturedResponse{Status: resp.StatusCode, Headers: resp.Header.Clone(), Body: body, RequestPath: req.Path}
}

func tenantStatusIn(status int, allowed []int) bool {
	for _, s := range allowed {
		if s == status {
			return true
		}
	}
	return false
}

func tenantSubstitutions(f *tenantFixture, req, missing tenantRequest) map[string]string {
	subst := map[string]string{}
	for key, value := range f.bySide[sideB] {
		// Local revisions, versions and dates are not owner identities (A8).
		shape := tenantSyntheticShapes[key]
		if shape == "number" || shape == "version" || shape == "time" {
			continue
		}
		if tenantRequestContains(req, value) && f.bySide[sideMissing][key] != "" && tenantRequestContains(missing, f.bySide[sideMissing][key]) {
			subst[value] = f.bySide[sideMissing][key]
			bCanary := tenantCanary.FindAllString(value, -1)
			mCanary := tenantCanary.FindAllString(f.bySide[sideMissing][key], -1)
			for i := 0; i < len(bCanary) && i < len(mCanary); i++ {
				subst[bCanary[i]] = mCanary[i]
			}
		}
	}
	return subst
}

// Fresh request fixtures replace role slots, but those replacements must never
// remove the base account's identities, credentials or sealed values from the
// response scan. Keep request construction scoped and leak detection cumulative.
func (rt *tenantRuntime) leakValues(f *tenantFixture) []string {
	seen := map[string]bool{}
	for _, source := range []*tenantFixture{rt.f, f} {
		for _, value := range source.values(sideB) {
			seen[value] = true
		}
	}
	values := make([]string, 0, len(seen))
	for value := range seen {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func (rt *tenantRuntime) noLeak(t *testing.T, f *tenantFixture, resp *capturedResponse, req tenantRequest) {
	t.Helper()
	if found := leaks(resp, req, rt.leakValues(f)); len(found) > 0 {
		kinds := map[string]bool{}
		for _, source := range []*tenantFixture{rt.f, f} {
			for key, value := range source.bySide[sideB] {
				for _, leaked := range found {
					if strings.Contains(value, leaked) {
						kinds[key] = true
					}
				}
			}
		}
		keys := make([]string, 0, len(kinds))
		for key := range kinds {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		rt.isolationDetected = true
		t.Errorf("ISOLATION DEFECT: response leaks %d foreign values; fixture keys=%v", len(found), keys)
	}
}

func tenantBodyHas(resp *capturedResponse, values []string) bool {
	for _, v := range values {
		if v != "" && bytes.Contains(resp.Body, []byte(v)) {
			return true
		}
	}
	return false
}

// Caller-namespaced upserts allocate local IDs and timestamps. Only generated
// fields are normalized; carried keys use sameRefusal's explicit substitution.
func tenantNamespaceResponse(resp *capturedResponse) *capturedResponse {
	out := *resp
	var body any
	if json.Unmarshal(resp.Body, &body) != nil {
		return &out
	}
	var normalize func(any)
	normalize = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, item := range v {
				switch key {
				case "id", "created_at", "updated_at":
					v[key] = "<generated>"
				default:
					normalize(item)
				}
			}
		case []any:
			for _, item := range v {
				normalize(item)
			}
		}
	}
	normalize(body)
	out.Body, _ = json.Marshal(body)
	return &out
}

func TestTenantIsolationMatrixPostgres(t *testing.T) {
	rt := tenantNewRuntime(t)
	tenantSeedIdentities(t, rt)
	tenantStartChild(t, rt)
	tenantSeedAvatars(t, rt)
	tenantSeedObjects(t, rt)
	rows := tenantRows()
	t.Logf("runtime pending rows=%d", len(tenantRuntimePending))
	before := tenantFingerprint(t, rt)
	statuses := map[string]int{}
	// Phases remain sequential even under -shuffle; selection still seeds and
	// fingerprints the complete fixture so row-specific mutation runs are valid.
	t.Run("phase1", func(t *testing.T) {
		for _, row := range rows {
			if rt.isolationDetected {
				break
			}
			if _, pending := tenantRuntimePending[row.Pattern]; pending {
				continue
			}
			t.Run(strings.NewReplacer("/", "_", " ", "_").Replace(row.Pattern), func(t *testing.T) {
				for _, p := range row.Probes {
					if rt.isolationDetected {
						break
					}
					t.Run(p.Name, func(t *testing.T) {
						f := rt.fixture(row, p, false)
						req := p.Build(f, sideB)
						credential := rt.credential(sideA, p)
						if p.Kind == exemption {
							rt.exempt(t, row, p, f)
							return
						}
						resp := rt.send(t, row, p, req, credential)
						statuses[tenantProbeID(row, p)] = resp.Status
						rt.noLeak(t, f, resp, req)
						if isMuxFallback(resp) {
							t.Errorf("route is not mounted: status=%d", resp.Status)
						}
						if p.Kind == foreign {
							allowed := p.Refuse
							if allowed == nil {
								allowed = []int{404}
							}
							if p.EmptyList {
								allowed = []int{200}
							}
							if !tenantStatusIn(resp.Status, allowed) {
								t.Errorf("foreign status=%d want=%v", resp.Status, allowed)
							}
							if p.ConfigBound == "" {
								missing := rt.send(t, row, p, p.Build(f, sideMissing), credential)
								rt.noLeak(t, f, missing, p.Build(f, sideMissing))
								if err := sameRefusal(resp, missing, tenantSubstitutions(f, req, p.Build(f, sideMissing))); err != nil {
									rt.isolationDetected = true
									t.Errorf("ISOLATION CANDIDATE: existence comparison: %v", err)
								}
							}
						} else {
							if p.Want == nil {
								if resp.Status < 200 || resp.Status >= 300 {
									t.Errorf("own status=%d want 2xx", resp.Status)
								}
							} else if !tenantStatusIn(resp.Status, p.Want) {
								t.Errorf("own status=%d want=%v", resp.Status, p.Want)
							}
							if p.ShowsOwn && row.Pattern == "GET /v1/export" {
								if err := tenantExportOwnError(f, resp); err != nil {
									t.Error(err)
								}
							} else if p.ShowsOwn {
								var canaries []string
								for _, v := range f.values(sideA) {
									canaries = append(canaries, tenantCanary.FindAllString(v, -1)...)
								}
								if !tenantBodyHas(resp, canaries) {
									t.Error("own read contains no seeded A canary")
								}
							}
							if p.CallerNamespaced {
								br, mr := p.NamespaceBuild(f, sideB), p.NamespaceBuild(f, sideMissing)
								b, m := rt.send(t, row, p, br, credential), rt.send(t, row, p, mr, credential)
								rt.noLeak(t, f, b, br)
								rt.noLeak(t, f, m, mr)
								if err := sameRefusal(tenantNamespaceResponse(b), tenantNamespaceResponse(m), tenantSubstitutions(f, br, mr)); err != nil {
									rt.isolationDetected = true
									t.Errorf("ISOLATION CANDIDATE: caller namespace comparison: %v", err)
								}
							}
						}
					})
				}
			})
		}
	})
	after := tenantFingerprint(t, rt)
	if !reflect.DeepEqual(before, after) {
		rt.isolationDetected = true
		for table, want := range before {
			if after[table] != want {
				t.Errorf("ISOLATION DEFECT: foreign account fingerprint changed in table %s", table)
			}
		}
		for table := range after {
			if _, ok := before[table]; !ok {
				t.Errorf("ISOLATION DEFECT: new foreign fingerprint table %s", table)
			}
		}
	}
	if rt.isolationDetected {
		t.Fatal("stopped before owner controls: investigate the isolation finding above")
	}
	t.Run("phase2", func(t *testing.T) {
		for _, row := range rows {
			if _, pending := tenantRuntimePending[row.Pattern]; pending {
				continue
			}
			t.Run(strings.NewReplacer("/", "_", " ", "_").Replace(row.Pattern), func(t *testing.T) {
				for _, p := range row.Probes {
					if p.Kind != foreign {
						continue
					}
					t.Run(p.Name, func(t *testing.T) {
						f := rt.fixture(row, p, true)
						resp := rt.send(t, row, p, p.Build(f, sideB), rt.credential(sideB, p))
						if tenantStatusIn(resp.Status, []int{401, 403, 404, 405}) || isMuxFallback(resp) {
							t.Errorf("owner control status=%d does not reach owned object", resp.Status)
						}
						prior, ran := statuses[tenantProbeID(row, p)]
						if !ran {
							// Includes EmptyList: phase2-only selection is never proof.
							t.Error("phase-1 refusal not recorded; select phase1 too")
						}
						if p.EmptyList {
							if !tenantBodyHas(resp, f.values(sideB)) {
								t.Error("empty-list control contains no B id or canary")
							}
						} else if ran && prior == resp.Status {
							t.Errorf("owner control status=%d equals foreign refusal", resp.Status)
						}
					})
				}
			})
		}
	})
}

func (rt *tenantRuntime) exempt(t *testing.T, row tenantRow, p tenantProbe, f *tenantFixture) {
	t.Helper()
	req := p.Build(f, sideB)
	check := func(req tenantRequest, cred string) *capturedResponse {
		resp := rt.send(t, row, p, req, cred)
		rt.noLeak(t, f, resp, req)
		return resp
	}
	json4xx := func(resp *capturedResponse) {
		if resp.Status < 400 || resp.Status >= 500 || !strings.HasPrefix(resp.Headers.Get("Content-Type"), "application/json") || !json.Valid(resp.Body) {
			t.Errorf("mounted exemption status=%d requires JSON 4xx", resp.Status)
		}
	}
	switch row.Class {
	case exemptPublic:
		for _, cred := range []string{"", rt.creds[sideA][credAgent]} {
			if r := check(req, cred); r.Status != 200 {
				t.Errorf("public status=%d want=200", r.Status)
			}
		}
	case exemptSystemToken:
		for _, cred := range []string{rt.creds[sideA][credOperator], rt.creds[sideA][credAgent]} {
			if r := check(req, cred); r.Status != 401 {
				t.Errorf("system route tenant status=%d want=401", r.Status)
			}
		}
		missing := p.Build(f, sideMissing)
		if missing.Method != "GET" {
			missing.Body = []byte("{")
			missing.Headers.Set("Content-Type", "application/json")
		}
		json4xx(check(missing, rt.creds[sideA][p.Cred]))
	case exemptSignedRelay, exemptProviderToken, exemptBootstrap:
		r := check(req, rt.creds[sideA][credAgent])
		json4xx(r)
		if !tenantStatusIn(r.Status, []int{400, 401, 403}) {
			t.Errorf("unauthorized exemption status=%d", r.Status)
		}
	case exemptNotMounted:
		for _, cred := range []string{"", rt.creds[sideA][credAgent], rt.creds[sideA][credOperator]} {
			if !isMuxFallback(check(req, cred)) {
				t.Error("legacy account route unexpectedly mounted")
			}
		}
	default:
		t.Fatal("unknown exemption class")
	}
}

// Numeric revisions can coincide with other fixture fields. They must not
// enter identity substitutions or manufacture an ambiguous existence check.
func TestTenantMatrixRuntimeSubstitution(t *testing.T) {
	f := syntheticTenantFixture()
	f.bySide[sideB]["avatar.profile.revision"] = "5"
	f.bySide[sideB]["avatar.version"] = "1"
	f.bySide[sideB]["avatar.style.version"] = "1"
	req := tenantBuild("POST", "/v1/agents/${agent.avatar.rollback}/avatar:rollback", "", tenantF1AvatarVersionMutation)(f, sideB)
	missing := tenantBuild("POST", "/v1/agents/${agent.avatar.rollback}/avatar:rollback", "", tenantF1AvatarVersionMutation)(f, sideMissing)
	subst := tenantSubstitutions(f, req, missing)
	if len(subst) != 1 || subst[f.value(sideB, "agent.avatar.rollback")] != f.value(sideMissing, "agent.avatar.rollback") {
		t.Fatal("substitution must contain only the carried foreign identity")
	}
	a := &capturedResponse{Status: 404, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"error":"not found"}`)}
	if err := sameRefusal(a, a, subst); err != nil {
		t.Fatal(err)
	}
}

// Runtime seeds can give multiple role aliases one actual ID. Only the alias
// carried by the missing request may choose that ID's replacement.
func TestTenantMatrixSubstitutionUsesRequestSlots(t *testing.T) {
	f := syntheticTenantFixture()
	f.bySide[sideB]["curation_run_renew"] = f.bySide[sideB]["curation_run"]
	for i := range 8 {
		key := "substitution.alias." + strconv.Itoa(i)
		f.bySide[sideB][key] = f.bySide[sideB]["curation_run"]
		f.bySide[sideMissing][key] = "missing-alias-" + strconv.Itoa(i)
	}
	build := tenantBuild("POST", "/v1/memory-curation-runs/${curation_run_renew}/renew", "", "")
	owner, missing := build(f, sideB), build(f, sideMissing)
	for range 64 {
		subst := tenantSubstitutions(f, owner, missing)
		if len(subst) != 1 || subst[f.value(sideB, "curation_run_renew")] != f.value(sideMissing, "curation_run_renew") {
			t.Fatal("substitution selected an alias absent from the missing request")
		}
	}
}

func TestTenantMatrixLeakInventoryKeepsBaseValues(t *testing.T) {
	base := syntheticTenantFixture()
	// A8 excludes shared sequence numbers and dates from identity scans. Every
	// remaining base slot must survive even a complete Fresh overlay, on every
	// row/probe, including own and exempt routes.
	for key := range base.bySide[sideB] {
		switch tenantSyntheticShapes[key] {
		case "number", "version", "time":
			delete(base.bySide[sideB], key)
		}
	}
	base.bySide[sideB]["seed.runtime.token"] = "base-runtime-token"
	rt := &tenantRuntime{f: base, probeValues: map[tenantSide]map[string]map[string]string{sideB: {}}}
	for _, row := range tenantRows() {
		for _, p := range row.Probes {
			t.Run(tenantProbeID(row, p), func(t *testing.T) {
				overlay := tenantSyntheticValues(sideB, "inventory-overlay:"+tenantProbeID(row, p))
				overlay["seed.runtime.token"] = "overlay-runtime-token"
				rt.probeValues[sideB][tenantProbeID(row, p)] = overlay
				f := rt.fixture(row, p, false)
				inventory := map[string]bool{}
				for _, value := range rt.leakValues(f) {
					inventory[value] = true
				}
				for key, value := range base.bySide[sideB] {
					if !inventory[value] {
						t.Errorf("leak inventory lost base fixture key %s", key)
					}
				}
				for _, value := range f.values(sideB) {
					if !inventory[value] {
						t.Error("leak inventory lost an overlaid fixture value")
					}
				}
				if f.bySide[sideB]["token.full"] != overlay["token.full"] {
					t.Error("request fixture lost its overlay")
				}
			})
		}
	}
}

// Account export is gzip-compressed. Inspect its expanded tar bytes so foreign
// plaintext values cannot disappear from leak checks behind compression.
func tenantInspectionBody(headers http.Header, body []byte) ([]byte, error) {
	if !strings.HasPrefix(headers.Get("Content-Type"), "application/gzip") {
		return body, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	return io.ReadAll(reader)
}

func TestTenantMatrixCompressedLeak(t *testing.T) {
	const canary = "tcb1234567890abcdef"
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(canary)); err != nil {
		t.Fatal("write test archive")
	}
	if err := zw.Close(); err != nil {
		t.Fatal("close test archive")
	}
	body, err := tenantInspectionBody(http.Header{"Content-Type": {"application/gzip"}}, buf.Bytes())
	if err != nil {
		t.Fatal("decode test archive")
	}
	if len(leaks(&capturedResponse{Body: body}, tenantRequest{}, []string{canary})) != 1 {
		t.Fatal("compressed account export hid a foreign canary")
	}
}
