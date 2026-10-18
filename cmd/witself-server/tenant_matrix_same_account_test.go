package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/store"
)

// sideAMain is an explicit target side, independent of the request credential.
const sideAMain tenantSide = "a-main"

type tenantSameAccountRule struct {
	Mode        string
	Want        []int
	AllowedKeys map[string]string
	Refuse      []int
	Reason      string
	PathOnly    string
}

func tenantSameAccountCredential(cred tenantCred) bool {
	return cred == credAgent || cred == credCuratorPreview || cred == credCuratorApply
}

func tenantSameAccountOwnRead(row tenantRow, p tenantProbe) bool {
	if p.Kind != own || !tenantSameAccountCredential(p.Cred) {
		return false
	}
	method, _ := tenantPatternParts(row.Pattern)
	return method == http.MethodGet || row.Pattern == "POST /v1/memories:recall" || row.Pattern == "POST /v1/messages:listen" || row.Pattern == "POST /v1/email:listen"
}

// A missing classification never inherits a permissive family default.
func TestTenantMatrixSameAccountFieldsComplete(t *testing.T) {
	cite := regexp.MustCompile(`(?:internal/(?:store|server)|cmd/witself-server)/[a-zA-Z0-9_]+\.go:[1-9][0-9]*`)
	f := syntheticTenantFixture()
	f.bySide[sideAMain] = tenantSyntheticValues(sideA, "same-account")
	for _, row := range tenantRows() {
		for _, p := range row.Probes {
			t.Run(tenantProbeID(row, p), func(t *testing.T) {
				required := row.Class == tenant && p.Kind == foreign && tenantSameAccountCredential(p.Cred)
				for name, rule := range map[string]*tenantSameAccountRule{"SameRealm": p.SameRealm, "OtherRealm": p.OtherRealm} {
					if !required {
						if rule != nil {
							t.Errorf("%s is only valid for agent/curator foreign probes", name)
						}
						continue
					}
					if rule == nil {
						t.Errorf("missing %s classification", name)
						continue
					}
					pathOnly := tenantSameAccountPathOnly[tenantProbeID(row, p)]
					if rule.PathOnly != pathOnly || (pathOnly != "" && p.BodyRef == "") {
						t.Errorf("%s PathOnly does not match the closed FU122d2-1 set", name)
					}
					switch rule.Mode {
					case "allowed":
						entry, _ := json.Marshal([]string{row.Pattern, p.Name, name, rule.Reason})
						t.Logf("same-account allowed %s", entry)
						if !cite.MatchString(rule.Reason) {
							t.Errorf("%s allowed classification needs a store/handler file:line citation", name)
						}
						if len(rule.Refuse) != 0 {
							t.Errorf("%s allowed classification has refusal statuses", name)
						}
						if len(rule.Want) == 0 {
							t.Errorf("%s allowed classification needs explicit Want statuses", name)
						}
						for _, status := range rule.Want {
							if status != http.StatusOK && status != http.StatusCreated {
								t.Errorf("%s allowed status %d is outside the reviewed 200/201 set", name, status)
							}
						}
						for key, reason := range rule.AllowedKeys {
							if key == "" || !cite.MatchString(reason) {
								t.Errorf("%s AllowedKeys entry %q needs a fixture key and citation", name, key)
							}
						}
					case "refuse":
						if len(rule.Want) != 0 || len(rule.AllowedKeys) != 0 {
							t.Errorf("%s refusal cannot declare allowed statuses or keys", name)
						}
						for _, status := range rule.Refuse {
							if status == http.StatusForbidden && !strings.Contains(rule.Reason, "FU122-6") {
								t.Errorf("%s 403 refusal must cite FU122-6", name)
							}
							if status < 400 || status >= 500 {
								if status != 200 || !p.EmptyList {
									t.Errorf("%s has invalid refusal status", name)
								}
							}
						}
						if len(rule.Refuse) > 0 && !cite.MatchString(rule.Reason) {
							t.Errorf("%s custom refusal needs a source citation", name)
						}
						_, pinnedSkip := tenantSameAccountOracleSkips[tenantProbeID(row, p)]
						if tenantSameAccountSkipOracle(row, p, rule) != pinnedSkip {
							t.Errorf("%s existence-oracle skip differs from the closed cited set", name)
						}
					default:
						t.Errorf("%s has unknown classification", name)
					}
				}
				for scope, exceptions := range map[string]map[string]string{"peer": p.SameAccountAllowed, "other": p.OtherRealmAllowed} {
					if len(exceptions) > 0 && !tenantSameAccountOwnRead(row, p) {
						t.Errorf("%s exceptions are only valid on own reads", scope)
					}
					for key, reason := range exceptions {
						entry, _ := json.Marshal([]string{row.Pattern, p.Name, scope, key, reason})
						t.Logf("same-account exception %s", entry)
						if key == "" || !cite.MatchString(reason) {
							t.Errorf("%s exception %q needs a fixture key and citation", scope, key)
						}
					}
				}
				if p.Kind == own {
					accesses := []tenantFixtureAccess{}
					f.accesses = &accesses
					_ = p.Build(f, sideAMain)
					f.accesses = nil
					for _, a := range accesses {
						if a.source != "own-probe" || a.side != sideMissing || a.scope != tenantProbeID(row, p) {
							t.Error("own probe escaped its dedicated sideMissing slots")
						}
					}
				}
			})
		}
	}
	for id, reason := range tenantSameAccountOracleSkips {
		if !cite.MatchString(reason) {
			t.Errorf("oracle skip %s needs a source citation", id)
		}
	}
	// A deleted or renamed probe must not silently shrink either closed set.
	for label, pins := range map[string]map[string]string{"PathOnly": tenantSameAccountPathOnly, "oracle skip": tenantSameAccountOracleSkips} {
		for id := range pins {
			matches := 0
			for _, row := range tenantRows() {
				for _, p := range row.Probes {
					if tenantProbeID(row, p) == id && p.SameRealm != nil && p.OtherRealm != nil {
						matches++
					}
				}
			}
			if matches != 1 {
				t.Errorf("%s pin %s matches %d classified probes, want 1", label, id, matches)
			}
		}
	}
}

// Preserve the original request slots and merge every base and Fresh value for
// scanning. BodyRef prerequisites remain the seeded A-owned path objects; T6
// separately proves those references with caller-owned prerequisites.
func (rt *tenantRuntime) sameAccountFixture(row tenantRow, p tenantProbe) *tenantFixture {
	f := rt.fixture(row, p, false)
	f.bySide[sideAMain] = f.bySide[sideA]
	return f
}

func (rt *tenantRuntime) sameAccountValues(f *tenantFixture) []string {
	seen := map[string]bool{}
	for _, source := range []*tenantFixture{rt.f, f} {
		for _, value := range source.values(sideA) {
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

func TestTenantMatrixSameAccountLeakInventoryKeepsBaseValues(t *testing.T) {
	base := syntheticTenantFixture()
	base.bySide[sideA] = tenantSyntheticValues(sideA, "main-base")
	base.bySide[sideA]["seed.runtime.token"] = "main-base-token"
	rt := &tenantRuntime{f: base, probeValues: map[tenantSide]map[string]map[string]string{sideA: {}}}
	for _, row := range tenantRows() {
		for _, p := range row.Probes {
			t.Run(tenantProbeID(row, p), func(t *testing.T) {
				overlay := tenantSyntheticValues(sideA, "fresh:"+tenantProbeID(row, p))
				overlay["seed.runtime.token"] = "main-overlay-token"
				rt.probeValues[sideA][tenantProbeID(row, p)] = overlay
				f := rt.sameAccountFixture(row, p)
				inventory := map[string]bool{}
				for _, value := range rt.sameAccountValues(f) {
					inventory[value] = true
				}
				for _, source := range []*tenantFixture{base, f} {
					for _, value := range source.values(sideA) {
						if !inventory[value] {
							t.Error("same-account inventory lost a base or Fresh value")
						}
					}
				}
				if f.value(sideAMain, "token.full") != overlay["token.full"] {
					t.Error("main target lost Fresh overlay")
				}
			})
		}
	}
}

func (rt *tenantRuntime) sameAccountNoLeak(t *testing.T, f *tenantFixture, resp *capturedResponse, req tenantRequest, exceptions map[string]string) {
	t.Helper()
	allowed := map[string]bool{}
	for key := range exceptions {
		known := false
		for _, source := range []*tenantFixture{rt.f, f} {
			value := source.bySide[sideA][key]
			if value == "" {
				continue
			}
			known = true
			allowed[value] = true
			for _, canary := range tenantCanary.FindAllString(value, -1) {
				allowed[canary] = true
			}
		}
		if !known {
			t.Errorf("unknown same-account exception fixture key %s", key)
		}
	}
	var private []string
	for _, value := range rt.sameAccountValues(f) {
		if !allowed[value] {
			private = append(private, value)
		}
	}
	found := leaks(resp, req, private)
	if len(found) == 0 {
		return
	}
	kinds := map[string]bool{}
	for _, source := range []*tenantFixture{rt.f, f} {
		for key, value := range source.bySide[sideA] {
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
	t.Errorf("ISOLATION CANDIDATE: same-account status=%d leaked fixture keys=%v (values withheld)", resp.Status, keys)
}

func tenantSameAccountSubstitutions(f *tenantFixture, req, missing tenantRequest) map[string]string {
	target := *f
	target.bySide = map[tenantSide]map[string]string{sideB: f.bySide[sideAMain], sideMissing: f.bySide[sideMissing]}
	return tenantSubstitutions(&target, req, missing)
}

func tenantSameAccountRowName(row tenantRow) string {
	return strings.NewReplacer("/", "_", " ", "_").Replace(row.Pattern)
}

func TestTenantIsolationMatrixSameAccountPostgres(t *testing.T) {
	rt := tenantNewRuntime(t)
	tenantSeedIdentities(t, rt)
	tenantStartChild(t, rt)
	tenantSeedAvatars(t, rt)
	tenantSeedSameAccountObjects(t, rt)
	tenantSeedSameAccountCurationLanes(t, rt)
	rows := tenantRows()
	controls := map[string]bool{}
	// Read controls precede all same-account writes. Every selected own probe
	// requires its recorded main control, including under a -run phase filter.
	t.Run("control", func(t *testing.T) {
		for _, row := range rows {
			t.Run(tenantSameAccountRowName(row), func(t *testing.T) {
				for _, p := range row.Probes {
					if !tenantSameAccountOwnRead(row, p) {
						continue
					}
					t.Run(p.Name, func(t *testing.T) {
						f := rt.sameAccountFixture(row, p)
						resp := rt.send(t, row, p, p.Build(f, sideMissing), rt.credential(sideA, p))
						valid := resp.Status >= 200 && resp.Status < 300
						if p.Want != nil {
							valid = tenantStatusIn(resp.Status, p.Want)
						}
						if !valid || isMuxFallback(resp) {
							t.Fatalf("main own-read control status=%d violates the declared own status", resp.Status)
						}
						var canaries []string
						for _, value := range rt.sameAccountValues(f) {
							canaries = append(canaries, tenantCanary.FindAllString(value, -1)...)
						}
						if p.ShowsOwn && !tenantBodyHas(resp, canaries) {
							t.Fatal("main own-read control has no main canary")
						}
						controls[tenantProbeID(row, p)] = true
					})
				}
			})
		}
	})
	t.Run("own", func(t *testing.T) {
		for _, row := range rows {
			if rt.isolationDetected {
				break
			}
			t.Run(tenantSameAccountRowName(row), func(t *testing.T) {
				for _, p := range row.Probes {
					if !tenantSameAccountOwnRead(row, p) || rt.isolationDetected {
						continue
					}
					t.Run(p.Name, func(t *testing.T) {
						if !controls[tenantProbeID(row, p)] {
							t.Fatal("main own-read control not recorded; select control too")
						}
						for _, actor := range []string{"peer", "other"} {
							if rt.isolationDetected {
								break
							}
							t.Run(actor, func(t *testing.T) {
								f := rt.sameAccountFixture(row, p)
								req := p.Build(f, sideMissing)
								resp := rt.send(t, row, p, req, rt.f.bySide[sideA]["token."+actor])
								allowed := p.SameAccountAllowed
								if actor == "other" {
									allowed = p.OtherRealmAllowed
								}
								rt.sameAccountNoLeak(t, f, resp, req, allowed)
								// Peer and other have no retained avatar version. The only
								// own-read exception is the owner-scoped missing version:
								// internal/store/avatar_query.go:567-580; internal/server/avatar.go:963-964.
								missingVersion := row.Pattern == "GET /v1/self/avatar/versions/{version}" && resp.Status == 404
								if isMuxFallback(resp) || (!missingVersion && (resp.Status < 200 || resp.Status >= 300)) {
									t.Errorf("own read status=%d does not reach caller's route", resp.Status)
								}
							})
						}
					})
				}
			})
		}
	})
	if rt.isolationDetected {
		t.Fatal("stopped after same-account isolation finding")
	}
	t.Run("curator-full-only", func(t *testing.T) { rt.sameAccountCuratorFullOnly(t, rows) })
	t.Run("foreign", func(t *testing.T) {
		for _, row := range rows {
			if rt.isolationDetected {
				break
			}
			t.Run(tenantSameAccountRowName(row), func(t *testing.T) {
				for _, p := range row.Probes {
					if row.Class != tenant || p.Kind != foreign || !tenantSameAccountCredential(p.Cred) || rt.isolationDetected {
						continue
					}
					t.Run(p.Name, func(t *testing.T) {
						for _, actor := range []string{"peer", "other"} {
							if rt.isolationDetected {
								break
							}
							t.Run(actor, func(t *testing.T) {
								rule := p.SameRealm
								if actor == "other" {
									rule = p.OtherRealm
								}
								if rule == nil {
									t.Fatal("missing same-account classification")
								}
								f := rt.sameAccountFixture(row, p)
								req := p.Build(f, sideAMain)
								cred := rt.f.bySide[sideA]["token."+actor]
								resp := rt.send(t, row, p, req, cred)
								if rule.Mode == "allowed" {
									if !tenantStatusIn(resp.Status, rule.Want) || isMuxFallback(resp) {
										t.Errorf("allowed same-account status=%d want=%v", resp.Status, rule.Want)
									}
									rt.sameAccountNoLeak(t, f, resp, req, rule.AllowedKeys)
									return
								}
								rt.sameAccountNoLeak(t, f, resp, req, nil)
								want := rule.Refuse
								if want == nil {
									want = []int{404}
								}
								if !tenantStatusIn(resp.Status, want) {
									rt.isolationDetected = true
									t.Errorf("same-account refusal status=%d want=%v", resp.Status, want)
								}
								if !tenantSameAccountSkipOracle(row, p, rule) {
									missingReq := p.Build(f, sideMissing)
									missing := rt.send(t, row, p, missingReq, cred)
									rt.sameAccountNoLeak(t, f, missing, missingReq, nil)
									if err := sameRefusal(resp, missing, tenantSameAccountSubstitutions(f, req, missingReq)); err != nil {
										rt.isolationDetected = true
										t.Errorf("ISOLATION CANDIDATE: same-account real status=%d missing status=%d: %v", resp.Status, missing.Status, err)
									}
								}
							})
						}
					})
				}
			})
		}
	})
}

// A1's closed full-only set is gated before the handler decodes its body:
// internal/server/memory_curation.go:651-653,664-682 excludes request/cancel/rollback.
func (rt *tenantRuntime) sameAccountCuratorFullOnly(t *testing.T, rows []tenantRow) {
	t.Helper()
	matched := 0
	for _, pattern := range []string{"POST /v1/memory-curation-requests", "POST /v1/memory-curation-runs/{run}/cancel", "POST /v1/memory-curation-runs/{run}/rollback"} {
		matches := 0
		for _, row := range rows {
			if row.Pattern != pattern {
				continue
			}
			matches++
			if len(row.Probes) == 0 {
				t.Fatalf("full-only pattern %s has no probes", pattern)
			}
			p := row.Probes[0]
			for _, cred := range []tenantCred{credCuratorPreview, credCuratorApply} {
				t.Run(fmt.Sprintf("%s/%s", tenantSameAccountRowName(row), cred), func(t *testing.T) {
					f := rt.sameAccountFixture(row, p)
					req := p.Build(f, sideAMain)
					resp := rt.send(t, row, p, req, rt.creds[sideA][cred])
					if resp.Status != http.StatusForbidden {
						t.Errorf("main curator full-only status=%d want=403", resp.Status)
					}
					rt.sameAccountNoLeak(t, f, resp, req, nil)
				})
			}
		}
		if matches != 1 {
			t.Fatalf("full-only pattern %s matched %d rows, want 1", pattern, matches)
		}
		matched++
	}
	if matched != 3 {
		t.Fatalf("full-only coverage matched %d patterns, want 3", matched)
	}
}

// RequestCuration initializes a caller's lane without starting a run. Seeding
// both lanes keeps refusal statuses stable when -run selects curation alone,
// or an earlier allowed evidence capture has enqueued curation work.
func tenantSeedSameAccountCurationLanes(t *testing.T, rt *tenantRuntime) {
	t.Helper()
	v := rt.f.bySide[sideA]
	for _, actor := range []string{"peer", "other"} {
		p := tenantSeedPrincipal(rt, sideA, credAgent, "")
		p.ID, p.AgentName = v[actor+"_agent"], v[actor+"-name"]
		if actor == "other" {
			p.RealmID, p.RealmName = v["realm2"], v["realm2-name"]
		}
		_, err := rt.st.RequestCuration(context.Background(), p, store.RequestMemoryCurationInput{
			Scope:         store.MemoryCurationScope{Sources: []string{store.MemoryCurationSourceMemory, store.MemoryCurationSourceEvidence}},
			CoalescingKey: "same-account-" + actor, TriggerReason: "manual", IdempotencyKey: "tm-" + tenantRandom(),
		})
		tenantSeedObjectError(t, "RequestCuration same-account caller lane", err)
	}
}

// Only A's domain objects are targets in this dimension. Identity seeding still
// creates B's configured retry-canary principal, but no probe uses B's objects.
// Reuse the production-path seeders without duplicating unrelated B fixtures.
func tenantSeedSameAccountObjects(t *testing.T, rt *tenantRuntime) {
	t.Helper()
	tenantSeedFacts(t, rt, sideA)
	tenantSeedTranscripts(t, rt, sideA)
	tenantSeedTickets(t, rt, sideA)
	tenantSeedEmailControls(t, rt, sideA)
	tenantSeedVault(t, rt, sideA)
	tenantSeedMessages(t, rt, sideA)
	tenantSeedMemories(t, rt, sideA)
	for key, value := range rt.f.bySide[sideA] {
		rt.f.ownValues[key] = value
	}
}
