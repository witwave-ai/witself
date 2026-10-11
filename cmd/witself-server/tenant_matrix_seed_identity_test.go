package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/avatar"
	"github.com/witwave-ai/witself/internal/plans"
	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

// tenantSeedIdentities follows startupCohortStore's ProvisionAccount /
// ActivateAccount identity setup, then uses the production token and plan store
// methods. It runs before the child because the receive cohort needs these IDs.
func tenantSeedIdentities(t *testing.T, rt *tenantRuntime) {
	t.Helper()
	ctx := context.Background()
	catalog, err := plans.Load()
	tenantSeedCheck(t, err, "load plan catalog")
	plan, ok := catalog.Get("team")
	if !ok {
		t.Fatal("tenant matrix requires canonical team plan")
	}
	for _, feature := range plans.SupportedFeatureKeys() {
		if !plan.HasFeature(feature) {
			t.Fatalf("team plan omits required feature %q", feature)
		}
	}
	if rt.creds == nil {
		rt.creds = map[tenantSide]map[tenantCred]string{}
	}
	if rt.freshCreds == nil {
		rt.freshCreds = map[tenantSide]map[string]string{}
	}
	if rt.scratchCreds == nil {
		rt.scratchCreds = map[string]string{}
	}
	if rt.probeValues == nil {
		rt.probeValues = map[tenantSide]map[string]map[string]string{}
	}
	for _, side := range []tenantSide{sideA, sideB} {
		v := rt.f.bySide[side]
		if v == nil {
			v = tenantSyntheticValues(side, "foreign")
			rt.f.bySide[side] = v
		}
		rt.creds[side] = map[tenantCred]string{}
		rt.freshCreds[side] = map[string]string{}
		rt.probeValues[side] = map[string]map[string]string{}
		v["seed.account.name"] = string(side) + " account " + tenantSeedCanary(side, "account.name")
		account, opToken := tenantSeedAccount(t, rt, v["email.contact"], v["seed.account.name"], plan)
		v["account"], v["operator"], v["operator-token"] = account.AccountID, account.OperatorID, opToken
		// ProvisionAccount hardcodes the otherwise shared root display name
		// "owner" (store/provision.go:326); there is no operator rename API.
		_, err = rt.db.ExecContext(ctx, "UPDATE "+strconv.Quote(rt.schema)+".operators SET display_name=$1 WHERE account_id=$2 AND id=$3", v["operator-name"], account.AccountID, account.OperatorID)
		tenantSeedCheck(t, err, "name root operator")
		v["seed.bootstrap-token"] = account.BootstrapToken
		rt.creds[side][credOperator] = opToken
		opPrincipal, authenticated, err := rt.st.AuthenticatePrincipal(ctx, opToken)
		tenantSeedCheck(t, err, "authenticate seeded operator")
		if !authenticated {
			t.Fatal("seeded operator did not authenticate")
		}
		v["seed.operator.token-id"] = opPrincipal.TokenID
		operator := tenantSeedPrincipal(rt, side, credOperator, "")
		for _, role := range []struct{ key, name string }{{"realm", "realm-name"}, {"realm2", "realm2-name"}, {"realm.delete", "seed.realm.delete.name"}, {"realm.style", "seed.realm.style.name"}} {
			if v[role.name] == "" {
				v[role.name] = string(side) + "-" + strings.ReplaceAll(role.key, ".", "-") + "-" + tenantSeedCanary(side, role.name)
			}
			realm, err := rt.st.CreateRealm(ctx, account.AccountID, v[role.name])
			tenantSeedCheck(t, err, "create "+role.key)
			v[role.key] = realm.ID
		}
		// Publish the tenant-local custom style before its agents are created:
		// createAgentAvatarProfileTx copies the realm's selected style at mint.
		pack := avatar.BuiltInFlatVectorStylePack()
		pack.ID, pack.Name, pack.Description = v["avatar.style.id"], v["canary.avatar.style.name"], v["canary.avatar.style.description"]
		_, err = rt.st.SetRealmAvatarStyle(ctx, operator, v["realm"], store.CreateAvatarStyleVersionInput{ExpectedStyleRevision: 1, StylePack: pack, IdempotencyKey: "matrix-seed-style"})
		tenantSeedCheck(t, err, "publish tenant style")
		v["avatar.style.version"], v["avatar.profile.revision"], v["avatar.version"] = "1", "1", "1"
		for _, role := range []struct{ key, name, realm, token string }{
			{"agent", "agent-name", "realm", "token.full"},
			{"peer_agent", "peer-name", "realm", "token.peer"},
			{"other_agent", "other-name", "realm2", "token.other"},
		} {
			agent, err := rt.st.CreateAgent(ctx, account.AccountID, v[role.realm], v[role.name])
			tenantSeedCheck(t, err, "create "+role.key)
			v[role.key] = agent.ID
			tok, tokID, _, err := rt.st.CreateAgentToken(ctx, account.AccountID, account.OperatorID, agent.ID)
			tenantSeedCheck(t, err, "mint "+role.key+" token")
			v[role.token], v["seed."+role.token+".id"] = tok, tokID
			rt.secrets = append(rt.secrets, tok)
			if role.key == "agent" {
				rt.creds[side][credAgent], v["token"] = tok, tokID
			}
			// EnsureAgentEmailMailbox is the startup integration test's mailbox
			// seed. Its account scope matches the child's explicit receive cohort.
			mailbox, err := rt.st.EnsureAgentEmailMailbox(ctx, store.AgentEmailReceiveScope{Enabled: true, Mode: store.AgentEmailReceiveModeProduction, Domain: "witmail.net", Audience: "tenant-matrix", AccountIDs: map[string]bool{account.AccountID: true}}, account.AccountID, v[role.realm], agent.ID, "")
			tenantSeedCheck(t, err, "seed "+role.key+" mailbox")
			v["seed."+role.key+".email"] = mailbox.Address
		}
		for _, profile := range []struct {
			cred           tenantCred
			value, profile string
		}{
			{credCuratorPreview, "token.curator-preview", store.AccessProfileCuratorPreview},
			{credCuratorApply, "token.curator-apply", store.AccessProfileCuratorApply},
		} {
			v["seed."+profile.value+".name"] = string(side) + "-" + profile.profile + "-" + tenantSeedCanary(side, profile.value+".name")
			tok, tokID, _, _, err := rt.st.CreateCuratorToken(ctx, account.AccountID, account.OperatorID, v["agent"], profile.profile, v["seed."+profile.value+".name"], time.Hour)
			tenantSeedCheck(t, err, "mint curator token")
			v[profile.value], v["seed."+profile.value+".id"] = tok, tokID
			rt.creds[side][profile.cred] = tok
			rt.secrets = append(rt.secrets, tok)
		}
		for _, key := range []string{
			"agent.delete", "agent.avatar.activate", "agent.avatar.policy", "agent.avatar.propose", "agent.avatar.quota", "agent.avatar.reject", "agent.avatar.reset", "agent.avatar.rollback",
			"agent.self.read", "agent.self.activate", "agent.self.failure", "agent.self.propose", "agent.self.propose.foreign", "agent.self.reset", "agent.self.rollback", "agent.subject.upsert",
		} {
			name := string(side) + "-" + strings.ReplaceAll(key, ".", "-") + "-" + tenantSeedCanary(side, key+".name")
			agent, err := rt.st.CreateAgent(ctx, account.AccountID, v["realm"], name)
			tenantSeedCheck(t, err, "create "+key)
			v[key], v["seed."+key+".name"] = agent.ID, name
			if strings.HasPrefix(key, "agent.self.") || key == "agent.subject.upsert" {
				tok, tokID, _, err := rt.st.CreateAgentToken(ctx, account.AccountID, account.OperatorID, agent.ID)
				tenantSeedCheck(t, err, "mint "+key+" token")
				v["seed."+key+".token"], v["seed."+key+".token-id"] = tok, tokID
				rt.freshCreds[side][key] = tok
				rt.secrets = append(rt.secrets, tok)
			}
		}
		// Revoke and operator deletion controls never destroy a credential used
		// by later rows. Sources: CreateOperator and CreateOperatorToken.
		deleteName := string(side) + " delete " + tenantSeedCanary(side, "operator.delete.name")
		v["seed.operator.delete.token-name"] = string(side) + "-delete-token-" + tenantSeedCanary(side, "operator.delete.token-name")
		deleteOperator, deleteToken, _, err := rt.st.CreateOperator(ctx, account.AccountID, account.OperatorID, deleteName, v["seed.operator.delete.token-name"], nil)
		tenantSeedCheck(t, err, "create disposable operator")
		v["operator.delete"], v["seed.operator.delete.name"], v["seed.operator.delete.token"] = deleteOperator.ID, deleteName, deleteToken
		for i, meta := range deleteOperator.Tokens {
			v[fmt.Sprintf("seed.operator.delete.token-id.%d", i)] = meta.ID
		}
		rt.secrets = append(rt.secrets, deleteToken)
		v["seed.token.revoke.name"] = string(side) + "-revoke-token-" + tenantSeedCanary(side, "token.revoke.name")
		revokeToken, revokeID, _, err := rt.st.CreateOperatorToken(ctx, account.AccountID, account.OperatorID, v["seed.token.revoke.name"], nil)
		tenantSeedCheck(t, err, "create disposable token")
		v["token.revoke"], v["seed.token.revoke.plaintext"] = revokeID, revokeToken
		rt.secrets = append(rt.secrets, revokeToken)
	}
	for _, key := range []string{"S-close", "S-suspend", "S-resume", "S-rename", "S-placement", "S-optoken"} {
		account, token := tenantSeedAccount(t, rt, strings.ToLower(key)+"-matrix@example.invalid", key+" tenant matrix", plan)
		rt.scratchCreds[key] = token
		if key == "S-resume" {
			tenantSeedCheck(t, rt.st.SuspendAccountOwner(ctx, account.AccountID, account.OperatorID, "matrix-resume-control"), "suspend resume scratch account")
		}
	}
	for key, value := range rt.f.bySide[sideA] {
		rt.f.ownValues[key] = value
	}
	for scope, values := range rt.f.ownProbeValues {
		if !strings.Contains(scope, "/v1/self/avatar") {
			continue
		}
		values["avatar.style.id"] = rt.f.bySide[sideA]["avatar.style.id"]
		values["avatar.style.version"], values["avatar.profile.revision"] = "1", "1"
	}
}

// tenantSeedAccount uses the public store lifecycle in startup integration
// tests, and SetAccountPlan's canonical fenced snapshot rather than SQL writes.
func tenantSeedAccount(t *testing.T, rt *tenantRuntime, email, display string, plan plans.Plan) (store.ProvisionedAccount, string) {
	t.Helper()
	ctx := context.Background()
	a, err := rt.st.ProvisionAccount(ctx, email, display, time.Hour)
	tenantSeedCheck(t, err, "provision account")
	active, err := rt.st.ActivateAccount(ctx, a.AccountID)
	tenantSeedCheck(t, err, "activate account")
	if !active {
		t.Fatal("seed account was not activated")
	}
	hash, err := plans.SnapshotHash(plan.ID, plan.Limits, plan.Policies, plan.Features)
	tenantSeedCheck(t, err, "hash plan")
	_, err = rt.st.SetAccountPlan(ctx, a.AccountID, 1, hash, plan.ID, plan.Limits, plan.Policies, plan.Features)
	tenantSeedCheck(t, err, "set account plan")
	token, _, err := rt.st.ExchangeBootstrap(ctx, a.BootstrapToken)
	tenantSeedCheck(t, err, "exchange account bootstrap")
	rt.secrets = append(rt.secrets, token, a.BootstrapToken)
	return a, token
}

// tenantSeedPrincipal derives exactly the token's identity for store seed calls.
func tenantSeedPrincipal(rt *tenantRuntime, side tenantSide, cred tenantCred, fresh string) store.Principal {
	v := rt.f.bySide[side]
	p := store.Principal{AccountID: v["account"], AccountStatus: "active", AccessProfile: store.AccessProfileFull}
	if cred == credOperator {
		p.Kind, p.ID = store.PrincipalOperator, v["operator"]
		return p
	}
	p.Kind, p.ID, p.RealmID, p.AgentName, p.RealmName = store.PrincipalAgent, v["agent"], v["realm"], v["agent-name"], v["realm-name"]
	if fresh != "" {
		p.ID, p.AgentName = v[fresh], v["seed."+fresh+".name"]
	}
	return p
}

// tenantSeedAvatars uses the valid HTTP proposal shape from
// internal/server/avatar_test.go:TestOperatorAvatarHTTPRoutesAreAccountScoped,
// and the propose/activate/rollback lifecycle from store/avatar_integration_test.go.
// All writes run through the real child and finish before the first fingerprint.
func tenantSeedAvatars(t *testing.T, rt *tenantRuntime) {
	t.Helper()
	for _, side := range []tenantSide{sideA, sideB} {
		v := rt.f.bySide[side]
		for _, key := range []string{"agent", "agent.avatar.activate", "agent.avatar.reject", "agent.avatar.reset", "agent.avatar.rollback", "agent.self.reset"} {
			descriptionKey := "seed." + key + ".avatar-description.1"
			v[descriptionKey] = tenantSeedCanary(side, descriptionKey)
			proposal := server.ProposeAvatarRequest{ExpectedProfileRevision: 1, StylePackID: v["avatar.style.id"], StylePackVersion: 1, SubjectForm: avatar.SubjectHuman, Description: v[descriptionKey], VisualSpec: []byte(`{"identity":{"expression":"calm"}}`), SVG: avatar.BuiltInFlatVectorStylePack().References[0].SVG}
			result := tenantSeedAvatarHTTP(t, rt, side, key, "proposals", proposal)
			if result.Avatar.Proposed != nil {
				v["seed."+key+".avatar-version-id.1"] = result.Avatar.Proposed.ID
			}
			if key == "agent" || key == "agent.avatar.rollback" {
				result = tenantSeedAvatarHTTP(t, rt, side, key, ":activate", server.ActivateAvatarRequest{Version: 1, ExpectedProfileRevision: result.Avatar.Profile.ProfileRevision})
			}
			if key == "agent.avatar.rollback" {
				descriptionKey = "seed." + key + ".avatar-description.2"
				v[descriptionKey] = tenantSeedCanary(side, descriptionKey)
				proposal.Description = v[descriptionKey]
				proposal.ExpectedProfileRevision, proposal.ParentVersion = result.Avatar.Profile.ProfileRevision, 1
				result = tenantSeedAvatarHTTP(t, rt, side, key, "proposals", proposal)
				if result.Avatar.Proposed != nil {
					v["seed."+key+".avatar-version-id.2"] = result.Avatar.Proposed.ID
				}
				result = tenantSeedAvatarHTTP(t, rt, side, key, ":activate", server.ActivateAvatarRequest{Version: 2, ExpectedProfileRevision: result.Avatar.Profile.ProfileRevision})
			}
			if key == "agent" && result.Avatar.Active != nil {
				v["avatar-version-id"] = result.Avatar.Active.ID
			}
			for _, row := range tenantRows() {
				for _, probe := range row.Probes {
					if probe.Fresh != key {
						continue
					}
					scope := row.Pattern + "/" + probe.Name
					rt.probeValues[side][scope] = map[string]string{"avatar.profile.revision": strconv.FormatInt(result.Avatar.Profile.ProfileRevision, 10), "avatar.version": "1"}
					if side == sideA && probe.Kind == own {
						rt.f.ownProbeValues[scope]["avatar.profile.revision"] = strconv.FormatInt(result.Avatar.Profile.ProfileRevision, 10)
					}
				}
			}
		}
	}
	for key, value := range rt.f.bySide[sideA] {
		rt.f.ownValues[key] = value
	}
}

func tenantSeedAvatarHTTP(t *testing.T, rt *tenantRuntime, side tenantSide, key, action string, body any) server.AvatarMutationResult {
	t.Helper()
	routePath := "/v1/agents/" + rt.f.bySide[side][key] + "/avatar"
	pattern := "POST /v1/agents/{agent}/avatar"
	if strings.HasPrefix(action, ":") {
		routePath += action
		pattern += action
	} else {
		routePath += "/" + action
		pattern += "/" + action
	}
	encoded, err := json.Marshal(body)
	tenantSeedCheck(t, err, "encode avatar seed")
	response := rt.send(t, tenantRow{Pattern: pattern}, tenantProbe{Cred: credOperator}, tenantRequest{Method: "POST", Path: routePath, Body: encoded, Headers: http.Header{"Content-Type": {"application/json"}}}, rt.creds[side][credOperator])
	if response.Status < 200 || response.Status >= 300 {
		t.Fatalf("avatar seed %s %s returned status %d", key, action, response.Status)
	}
	var result server.AvatarMutationResult
	tenantSeedCheck(t, json.Unmarshal(response.Body, &result), "decode avatar seed")
	if result.Avatar.Profile.ProfileRevision < 1 {
		t.Fatal("avatar seed response has no profile revision")
	}
	return result
}

func tenantSeedCanary(side tenantSide, key string) string {
	digest := sha256.Sum256([]byte("tenant-runtime:" + string(side) + ":" + key))
	return "tc" + string(side) + hex.EncodeToString(digest[:8])
}

func tenantSeedCheck(t *testing.T, err error, step string) {
	t.Helper()
	if err != nil {
		t.Fatalf("tenant seed %s failed (%T)", step, err)
	}
}
