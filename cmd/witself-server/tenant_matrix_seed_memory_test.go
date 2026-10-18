package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/store"
)

// tenantSeedMemories follows TestMemoryCurationLanePostgres for transcript
// evidence and TestMemoryVectorHybridRecallPostgres for client-side vectors. All source
// objects are committed before the matrix takes its first account fingerprint.
func tenantSeedMemories(t *testing.T, rt *tenantRuntime, side tenantSide) {
	t.Helper()
	v := rt.f.bySide[side]
	p := tenantSeedPrincipal(rt, side, credAgent, "")
	memory := tenantMemorySeedCapture(t, rt, p, v, side, "memory", tenantMemoryTranscriptEvidence(v["transcript"]))
	memory = tenantMemorySeedAdjust(t, rt, p, v, side, "memory", memory)
	v["memory"], v["content_hash"] = memory.ID, memory.ContentHash
	profile, err := rt.st.CreateMemoryVectorProfile(context.Background(), p, store.CreateMemoryVectorProfileInput{
		Provider: v["vector_provider"], Model: v["vector_model"], Recipe: v["vector_recipe"], RecipeVersion: "1", Dimensions: 2, DistanceMetric: "cosine", Normalization: "none",
	})
	tenantSeedObjectError(t, "CreateMemoryVectorProfile", err)
	v["vector_profile"] = profile.ID
	_, err = rt.st.PutMemoryVector(context.Background(), p, store.PutMemoryVectorInput{ProfileID: profile.ID, MemoryID: memory.ID, MemoryVersion: memory.Version, ContentHash: memory.ContentHash, Vector: []float64{1, 0}})
	tenantSeedObjectError(t, "PutMemoryVector", err)

	for _, row := range tenantRows() {
		for _, probe := range row.Probes {
			if probe.Fresh == "" || strings.Contains(row.Pattern, "memory-curation") {
				continue
			}
			if !strings.Contains(row.Pattern, "/memories/") && !strings.Contains(row.Pattern, "/memory-evidence/") {
				continue
			}
			values := map[string]string{}
			key := probe.Fresh
			switch {
			case strings.Contains(row.Pattern, "/memory-evidence/"):
				evidenceKey := "evidence"
				if strings.HasPrefix(key, "evidence_memory") {
					evidenceKey = "evidence_memory"
				}
				if strings.HasPrefix(key, "evidence_message") {
					evidenceKey = "evidence_message"
				}
				pending := tenantMemorySeedCapture(t, rt, p, v, side, key, []store.MemoryEvidenceInput{{Type: "conversation", ResolutionState: store.MemoryEvidencePending, ExternalLocator: tenantDomainCanary(v, side, key+".locator")}})
				values[evidenceKey] = pending.Evidence[0].ID
			default:
				memoryKey := key
				for _, candidate := range []string{"memory_supersede_message", "memory_supersede_memory", "memory_supersede"} {
					if strings.HasPrefix(key, candidate) {
						memoryKey = candidate
						break
					}
				}
				m := tenantMemorySeedCapture(t, rt, p, v, side, key, tenantMemoryTranscriptEvidence(v["transcript"]))
				switch memoryKey {
				case "memory_restore":
					forgotten, e := rt.st.ForgetMemory(context.Background(), p, m.ID, store.MemoryLifecycleInput{ExpectedVersion: 1, Reason: "matrix fixture", IdempotencyKey: "tm-" + tenantRandom()})
					tenantSeedObjectError(t, "ForgetMemory restore prerequisite", e)
					m = forgotten.Memory
				case "memory_reactivate":
					replacement := store.CaptureMemoryInput{Content: tenantDomainCanary(v, side, key+".replacement"), Kind: "observation", Evidence: tenantMemoryTranscriptEvidence(v["transcript"]), IdempotencyKey: "tm-" + tenantRandom()}
					superseded, e := rt.st.SupersedeMemory(context.Background(), p, store.SupersedeMemoryInput{MemoryID: m.ID, ExpectedVersion: 1, Replacements: []store.CaptureMemoryInput{replacement}, IdempotencyKey: "tm-" + tenantRandom()})
					tenantSeedObjectError(t, "SupersedeMemory reactivate prerequisite", e)
					m = superseded.Source
					for i := range superseded.Replacements {
						v["seed.memory."+key+".replacement"] = superseded.Replacements[i].ID
					}
				default:
					m = tenantMemorySeedAdjust(t, rt, p, v, side, key, m)
				}
				values[memoryKey] = m.ID
			}
			rt.probeValues[side][tenantProbeID(row, probe)] = values
			tenantMemoryRemember(v, values, key)
		}
	}

	// Main owns one planned run for all curation reads. Mutating curation probes
	// get distinct owner lanes: StartCuration forbids concurrent runs per owner.
	base := map[string]string{"memory": memory.ID, "transcript": v["transcript"], "evidence": memory.Evidence[0].ID}
	tenantMemorySeedCuration(t, rt, side, p, base, "main", "planned", 2)
	for _, key := range []string{"curation_request", "curation_run"} {
		v[key] = base[key]
	}
	tenantMemoryRemember(v, base, "main-curation")
	queued := map[string]string{}
	tenantMemorySeedCuration(t, rt, side, p, queued, "queued", "request", 2)
	tenantMemoryRemember(v, queued, "queued-curation")
	for _, row := range tenantRows() {
		if !strings.Contains(row.Pattern, "memory-curation") {
			continue
		}
		for _, probe := range row.Probes {
			if probe.Kind != foreign || probe.Fresh == "" {
				continue
			}
			values, owner := tenantMemorySeedCurationOwner(t, rt, side, probe.Fresh)
			version := int64(2)
			mode := "open"
			switch {
			case strings.HasSuffix(row.Pattern, "/start"):
				mode = "request"
			case strings.HasSuffix(row.Pattern, "/apply"):
				mode = "planned"
			case strings.HasSuffix(row.Pattern, "/rollback"):
				mode, version = "applied", 1
			}
			m := tenantMemorySeedCapture(t, rt, owner, v, side, probe.Fresh, tenantMemoryTranscriptEvidence(values["transcript"]))
			if version == 2 {
				m = tenantMemorySeedAdjust(t, rt, owner, v, side, probe.Fresh, m)
			}
			values["memory"], values["evidence"], values["content_hash"] = m.ID, m.Evidence[0].ID, m.ContentHash
			tenantMemorySeedCuration(t, rt, side, owner, values, probe.Fresh, mode, version)
			// Each template uses a role-specific run/request key, while the
			// seed helper returns the generic pair for its isolated lane.
			for _, key := range []string{"curation_request_start", "curation_run_renew", "curation_run_plan", "curation_run_apply", "curation_run_cancel", "curation_run_abandon", "curation_run_rollback"} {
				if strings.HasPrefix(key, "curation_request") {
					values[key] = values["curation_request"]
				} else {
					values[key] = values["curation_run"]
				}
			}
			rt.probeValues[side][tenantProbeID(row, probe)] = values
			tenantMemoryRemember(v, values, probe.Fresh)
		}
	}
}

func tenantMemoryTranscriptEvidence(transcript string) []store.MemoryEvidenceInput {
	return []store.MemoryEvidenceInput{{Type: "conversation", ResolutionState: store.MemoryEvidenceResolved, ResolvedKind: "transcript", SourceTranscriptID: transcript, SourceSequenceFrom: 1, SourceSequenceUntil: 1}}
}

// CaptureMemory and AdjustMemory preserve genuine two-version histories and
// their evidence; direct SQL never invents a memory lifecycle state.
func tenantMemorySeedCapture(t *testing.T, rt *tenantRuntime, p store.Principal, values map[string]string, side tenantSide, key string, evidence []store.MemoryEvidenceInput) store.Memory {
	t.Helper()
	result, err := rt.st.CaptureMemory(context.Background(), p, store.CaptureMemoryInput{Content: tenantDomainCanary(values, side, key+".initial"), Kind: "observation", CaptureReason: "user_request", Evidence: evidence, IdempotencyKey: "tm-" + tenantRandom()})
	tenantSeedObjectError(t, "CaptureMemory "+key, err)
	values["seed.memory."+key+".id"] = result.Memory.ID
	for i := range result.Memory.Evidence {
		values["seed.memory."+key+".evidence"] = result.Memory.Evidence[i].ID
	}
	return result.Memory
}

func tenantMemorySeedAdjust(t *testing.T, rt *tenantRuntime, p store.Principal, values map[string]string, side tenantSide, key string, memory store.Memory) store.Memory {
	t.Helper()
	content := tenantDomainCanary(values, side, key+".current")
	adjusted, err := rt.st.AdjustMemory(context.Background(), p, memory.ID, store.AdjustMemoryInput{ExpectedVersion: memory.Version, Content: &content, IdempotencyKey: "tm-" + tenantRandom()})
	tenantSeedObjectError(t, "AdjustMemory "+key, err)
	// Adjustment creates no new evidence rows; retain the captured evidence
	// locator for curation input membership and later resolution probes.
	adjusted.Memory.Evidence = memory.Evidence
	return adjusted.Memory
}

func tenantMemoryRemember(all, values map[string]string, scope string) {
	for key, value := range values {
		if value != "" {
			all["seed.memory."+scope+"."+key] = value
		}
	}
}

// Dedicated agents use the same account and realm and authentic full/curator
// token issuance. The scoped profiles are needed for the corresponding control.
func tenantMemorySeedCurationOwner(t *testing.T, rt *tenantRuntime, side tenantSide, key string) (map[string]string, store.Principal) {
	t.Helper()
	v := rt.f.bySide[side]
	ctx := context.Background()
	name := "tm-curation-" + tenantDomainCanary(v, side, key+".agent")
	agent, err := rt.st.CreateAgent(ctx, v["account"], v["realm"], name)
	tenantSeedObjectError(t, "CreateAgent curation lane", err)
	values := map[string]string{"agent": agent.ID, "agent-name": name}
	if rt.freshProfileCreds == nil {
		rt.freshProfileCreds = map[tenantSide]map[string]map[tenantCred]string{}
	}
	if rt.freshProfileCreds[side] == nil {
		rt.freshProfileCreds[side] = map[string]map[tenantCred]string{}
	}
	rt.freshProfileCreds[side][key] = map[tenantCred]string{}
	for _, credential := range []struct {
		cred    tenantCred
		profile string
	}{{credAgent, store.AccessProfileFull}, {credCuratorPreview, store.AccessProfileCuratorPreview}, {credCuratorApply, store.AccessProfileCuratorApply}} {
		var token, tokenID string
		if credential.cred == credAgent {
			token, tokenID, _, err = rt.st.CreateAgentToken(ctx, v["account"], v["operator"], agent.ID)
		} else {
			token, tokenID, _, _, err = rt.st.CreateCuratorToken(ctx, v["account"], v["operator"], agent.ID, credential.profile, name+"-"+credential.profile, time.Hour)
		}
		tenantSeedObjectError(t, "CreateCuratorToken isolated lane", err)
		rt.freshProfileCreds[side][key][credential.cred] = token
		values["seed.token."+string(credential.cred)], values["seed.token-id."+string(credential.cred)] = token, tokenID
		rt.secrets = append(rt.secrets, token)
	}
	p := tenantSeedPrincipal(rt, side, credAgent, "")
	p.ID, p.AgentName = agent.ID, name
	tr, err := rt.st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID, store.CreateTranscriptInput{Title: tenantDomainCanary(v, side, key+".transcript")})
	tenantSeedObjectError(t, "CreateTranscript curation lane", err)
	entry, err := rt.st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID, tr.ID, store.AppendTranscriptEntryInput{Role: store.TranscriptRoleUser, Body: tenantDomainCanary(v, side, key+".entry")})
	tenantSeedObjectError(t, "AppendTranscriptEntry curation lane", err)
	values["transcript"], values["transcript.entry"] = tr.ID, entry.ID
	return values, p
}

// RequestCuration, StartCuration, PlanCuration and ApplyCuration mirror the
// store's curation lane/apply integration fixtures. Rollback uses a real apply
// receipt and a version-two head produced by that one accepted replacement.
func tenantMemorySeedCuration(t *testing.T, rt *tenantRuntime, side tenantSide, p store.Principal, values map[string]string, key, mode string, version int64) {
	t.Helper()
	ctx := context.Background()
	v := rt.f.bySide[side]
	requested, err := rt.st.RequestCuration(ctx, p, store.RequestMemoryCurationInput{Scope: store.MemoryCurationScope{Sources: []string{store.MemoryCurationSourceMemory, store.MemoryCurationSourceEvidence}}, CoalescingKey: tenantDomainCanary(v, side, key+".coalescing"), TriggerReason: "manual", IdempotencyKey: "tm-" + tenantRandom()})
	tenantSeedObjectError(t, "RequestCuration "+key, err)
	values["curation_request"] = requested.Request.ID
	if mode == "request" {
		return
	}
	started, err := rt.st.StartCuration(ctx, p, store.StartMemoryCurationInput{RequestID: requested.Request.ID, LeaseDuration: 30 * time.Minute, IdempotencyKey: "tm-" + tenantRandom()})
	tenantSeedObjectError(t, "StartCuration "+key, err)
	values["curation_run"] = started.Run.ID
	if mode == "open" {
		return
	}
	draft := store.MemoryCurationPlanDraft{Schema: store.MemoryCurationPlanSchemaV1, DraftRevision: 1, Actions: []store.MemoryCurationPlanAction{{Ordinal: 1, Operation: store.MemoryCurationOperationReplace, Replace: &store.MemoryCurationReplaceAction{Target: store.MemoryCurationTargetReference{MemoryID: values["memory"], ExpectedVersion: version}, Snapshot: store.MemoryCurationMemorySnapshot{Content: tenantDomainCanary(v, side, key+".plan"), Kind: "observation"}}}}}
	raw, err := json.Marshal(draft)
	tenantSeedObjectError(t, "marshal curation plan", err)
	planned, err := rt.st.PlanCuration(ctx, p, started.Run.ID, store.PlanMemoryCurationInput{FencingGeneration: started.Run.FencingGeneration, Draft: raw, IdempotencyKey: "tm-" + tenantRandom()})
	tenantSeedObjectError(t, "PlanCuration "+key, err)
	values["plan_hash"], values["seed.plan.receipt"] = planned.Receipt.PlanHash, planned.Receipt.ID
	if mode != "applied" {
		return
	}
	applied, err := rt.st.ApplyCuration(ctx, p, started.Run.ID, store.ApplyMemoryCurationInput{FencingGeneration: started.Run.FencingGeneration, PlanRevision: planned.Receipt.PlanRevision, PlanHash: planned.Receipt.PlanHash, IdempotencyKey: "tm-" + tenantRandom()})
	tenantSeedObjectError(t, "ApplyCuration rollback prerequisite", err)
	values["curation_receipt"] = applied.Receipt.ID
}
