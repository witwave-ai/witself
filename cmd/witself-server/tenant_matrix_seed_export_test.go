package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/store"
)

// S-export cannot share A's deliberately open vault prerequisites. Seed a full
// portable account through production store lifecycles before child startup and
// the first fingerprint. Its display name contains no canary, so an otherwise
// empty export can never satisfy the own control.
func tenantSeedExportAccount(t *testing.T, rt *tenantRuntime, account store.ProvisionedAccount) {
	t.Helper()
	ctx := context.Background()
	v := tenantSyntheticValues(sideA, "export")
	v["account"], v["operator"] = account.AccountID, account.OperatorID
	canary := func(kind string) string {
		return tenantDomainCanary(rt.f.bySide[sideA], sideA, "export."+kind)
	}
	v["realm-name"], v["agent-name"], v["peer-name"] = canary("realm"), canary("agent"), canary("peer")
	realm, err := rt.st.CreateRealm(ctx, account.AccountID, v["realm-name"])
	tenantSeedObjectError(t, "export CreateRealm", err)
	v["realm"] = realm.ID
	agent, err := rt.st.CreateAgent(ctx, account.AccountID, realm.ID, v["agent-name"])
	tenantSeedObjectError(t, "export CreateAgent", err)
	peer, err := rt.st.CreateAgent(ctx, account.AccountID, realm.ID, v["peer-name"])
	tenantSeedObjectError(t, "export CreateAgent peer", err)
	v["agent"], v["peer_agent"] = agent.ID, peer.ID
	p := store.Principal{Kind: store.PrincipalAgent, AccountID: account.AccountID, AccountStatus: "active", ID: agent.ID, RealmID: realm.ID, AgentName: agent.Name, RealmName: realm.Name, AccessProfile: store.AccessProfileFull}
	peerPrincipal := p
	peerPrincipal.ID, peerPrincipal.AgentName = peer.ID, peer.Name

	transcript, err := rt.st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID, store.CreateTranscriptInput{Title: canary("transcript")})
	tenantSeedObjectError(t, "export CreateTranscript", err)
	entry, err := rt.st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID, transcript.ID, store.AppendTranscriptEntryInput{Role: store.TranscriptRoleUser, Body: canary("entry")})
	tenantSeedObjectError(t, "export AppendTranscriptEntry", err)
	v["transcript"], v["transcript.entry"] = transcript.ID, entry.ID
	factValue, err := json.Marshal(canary("fact"))
	tenantSeedObjectError(t, "export fact JSON", err)
	fact, err := rt.st.SetFact(ctx, p, store.SetFactInput{Predicate: "matrix/export", ValueType: "string", Value: factValue, SourceKind: "agent", IdempotencyKey: "matrix-export-fact"})
	tenantSeedObjectError(t, "export SetFact", err)
	v["fact"] = fact.ID
	ticket, message, err := rt.st.OpenTicket(ctx, store.OpenTicketInput{AccountID: p.AccountID, OperatorID: account.OperatorID, Subject: canary("ticket"), Category: store.TicketCategoryOther, Priority: store.TicketPriorityNormal, Body: canary("ticket.message")})
	tenantSeedObjectError(t, "export OpenTicket", err)
	_, err = rt.st.ChangeTicketState(ctx, store.ChangeTicketStateInput{AccountID: p.AccountID, OperatorID: account.OperatorID, TicketID: ticket.ID, NewState: store.TicketStateClosed})
	tenantSeedObjectError(t, "export close ticket", err)
	v["ticket"], v["ticket.message"] = ticket.ID, message.ID

	// Keep both a real transcript reference and a resolved imported evidence
	// locator, whose separate canary proves the evidence content is exported.
	evidence := append(tenantMemoryTranscriptEvidence(transcript.ID), store.MemoryEvidenceInput{Type: "conversation", ResolutionState: store.MemoryEvidenceResolved, ResolvedKind: "import_artifact", SourceImportLocator: canary("evidence")})
	memory, err := rt.st.CaptureMemory(ctx, p, store.CaptureMemoryInput{Content: canary("memory"), Kind: "observation", CaptureReason: "user_request", Evidence: evidence, IdempotencyKey: "matrix-export-memory"})
	tenantSeedObjectError(t, "export CaptureMemory", err)
	v["memory"] = memory.Memory.ID
	profile, err := rt.st.CreateMemoryVectorProfile(ctx, p, store.CreateMemoryVectorProfileInput{Provider: canary("vector"), Model: "matrix-export", Recipe: "matrix-export", RecipeVersion: "1", Dimensions: 2, DistanceMetric: store.MemoryVectorMetricCosine, Normalization: store.MemoryVectorNormalizationNone})
	tenantSeedObjectError(t, "export CreateMemoryVectorProfile", err)
	v["vector_profile"] = profile.ID
	_, err = rt.st.PutMemoryVector(ctx, p, store.PutMemoryVectorInput{ProfileID: profile.ID, MemoryID: memory.Memory.ID, MemoryVersion: memory.Memory.Version, ContentHash: memory.Memory.ContentHash, Vector: []float64{1, 0}})
	tenantSeedObjectError(t, "export PutMemoryVector", err)

	v["secret_name"], v["secret_description"] = canary("secret"), canary("secret.description")
	tenantSeedVaultKey(t, rt, p, v)
	tenantSeedVaultSecret(t, rt, p, v, "secret")
	tenantSeedVaultEnrollment(t, rt, p, v, "enrollment", false)
	_, err = rt.st.CancelVaultKeyEnrollment(ctx, p, v["enrollment"], store.CancelVaultKeyEnrollmentInput{ExpectedRowVersion: 1, IdempotencyKey: "matrix-export-cancel-enrollment"})
	tenantSeedObjectError(t, "export cancel enrollment", err)
	tenantSeedVaultRotation(t, rt, p, v, "rotation", false)
	_, _, err = rt.st.CancelVaultKeyRotation(ctx, p, v["rotation"], store.CancelVaultKeyRotationInput{ExpectedRotationRowVersion: 1, IdempotencyKey: "matrix-export-cancel-rotation"})
	tenantSeedObjectError(t, "export cancel rotation", err)

	for _, direction := range []struct {
		key      string
		from, to store.Principal
	}{
		{"message.outbound", p, peerPrincipal},
		{"message.inbound", peerPrincipal, p},
	} {
		msg, sendErr := rt.st.SendMessage(ctx, direction.from, store.SendMessageInput{ToAgent: direction.to.ID, Subject: canary(direction.key + ".subject"), Body: canary(direction.key), IdempotencyKey: "matrix-export-" + direction.key})
		tenantSeedObjectError(t, "export SendMessage "+direction.key, sendErr)
		_, err = rt.st.AckMessage(ctx, direction.to, msg.ID)
		tenantSeedObjectError(t, "export AckMessage "+direction.key, err)
		v[direction.key] = msg.ID
	}
	scope := store.AgentEmailReceiveScope{Enabled: true, Mode: store.AgentEmailReceiveModeProduction, Domain: "witmail.net", Audience: "tenant-matrix", AccountIDs: map[string]bool{p.AccountID: true}}
	mailbox, err := rt.st.EnsureAgentEmailMailbox(ctx, scope, p.AccountID, p.RealmID, p.ID, "")
	tenantSeedObjectError(t, "export EnsureAgentEmailMailbox", err)
	v["seed.agent.email"] = mailbox.Address
	// The existing MIME seed helper only needs this account's isolated values.
	exportRuntime := *rt
	exportRuntime.f = &tenantFixture{bySide: map[tenantSide]map[string]string{sideA: v}}
	email := tenantSeedInboundEmail(t, &exportRuntime, sideA, scope, "export.email")
	_, err = rt.st.AckAgentEmail(ctx, scope, p, email.ID)
	tenantSeedObjectError(t, "export AckAgentEmail", err)
	rt.f.bySide[sideA]["canary.seed.export.email"] = v["canary.seed.export.email.body"]
	v["inbound_email"] = email.ID
	for key, value := range v {
		rt.f.bySide[sideA]["seed.export."+key] = value
	}
}

// All these independent object canaries must survive gzip expansion. This
// deliberately excludes the account display name and cannot pass on a header,
// manifest or empty account archive alone.
var tenantExportCanaryKinds = []struct{ kind, table string }{
	{"realm", "realms"}, {"agent", "agents"}, {"peer", "agents"},
	{"transcript", "transcript_conversations"}, {"entry", "transcript_entries"},
	{"fact", "fact_assertions"}, {"ticket", "support_tickets"}, {"ticket.message", "support_ticket_messages"},
	{"memory", "memory_versions"}, {"evidence", "memory_evidence"}, {"vector", "memory_vector_profiles"},
	{"secret", "secrets"}, {"message.outbound", "agent_messages"}, {"message.inbound", "agent_messages"},
	{"email", "agent_email_messages"},
}

func tenantExportOwnError(f *tenantFixture, response *capturedResponse) error {
	tables := map[string][]byte{}
	archive := tar.NewReader(bytes.NewReader(response.Body))
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("export own read is not an expanded account archive")
		}
		table, _, ok := strings.Cut(header.Name, "/")
		if !ok || !strings.HasSuffix(header.Name, ".ndjson") {
			continue
		}
		chunk, err := io.ReadAll(archive)
		if err != nil {
			return fmt.Errorf("export own read has an unreadable table chunk")
		}
		tables[table] = append(tables[table], chunk...)
	}
	for _, item := range tenantExportCanaryKinds {
		canary := f.bySide[sideA]["canary.seed.export."+item.kind]
		if canary == "" || !bytes.Contains(tables[item.table], []byte(canary)) {
			return fmt.Errorf("export own read lacks seeded %s object canary in %s", item.kind, item.table)
		}
	}
	// Vectors carry numerical components and owner references, not prose. Pin
	// the actual child row to the canary-bearing memory and vector profile.
	for _, line := range bytes.Split(tables["memory_vectors"], []byte("\n")) {
		var vector struct {
			MemoryID  string    `json:"memory_id"`
			ProfileID string    `json:"profile_id"`
			Vector    []float64 `json:"vector"`
		}
		if json.Unmarshal(line, &vector) == nil && vector.MemoryID != "" && vector.ProfileID != "" &&
			vector.MemoryID == f.bySide[sideA]["seed.export.memory"] &&
			vector.ProfileID == f.bySide[sideA]["seed.export.vector_profile"] &&
			reflect.DeepEqual(vector.Vector, []float64{1, 0}) {
			return nil
		}
	}
	return fmt.Errorf("export own read lacks the seeded memory vector child row")
}

func TestTenantMatrixExportRequiresPopulatedOwnAccount(t *testing.T) {
	f := &tenantFixture{bySide: map[tenantSide]map[string]string{sideA: {}}}
	for _, item := range tenantExportCanaryKinds {
		tenantDomainCanary(f.bySide[sideA], sideA, "export."+item.kind)
	}
	f.bySide[sideA]["seed.export.memory"] = "mem_export_test"
	f.bySide[sideA]["seed.export.vector_profile"] = "mvp_export_test"
	body := func(omit string) []byte {
		chunks := map[string][]byte{}
		for _, item := range tenantExportCanaryKinds {
			if item.kind != omit {
				data, err := json.Marshal(map[string]string{"canary": f.bySide[sideA]["canary.seed.export."+item.kind]})
				tenantSeedObjectError(t, "marshal export checker fixture", err)
				chunks[item.table] = append(chunks[item.table], append(data, '\n')...)
			}
		}
		if omit != "vector-row" {
			chunks["memory_vectors"] = []byte(`{"memory_id":"mem_export_test","profile_id":"mvp_export_test","vector":[1,0]}` + "\n")
		}
		return tenantExportTestArchive(t, chunks)
	}
	if err := tenantExportOwnError(f, &capturedResponse{Body: body("")}); err != nil {
		t.Fatal(err)
	}
	display := tenantDomainCanary(f.bySide[sideA], sideA, "export.account")
	if tenantExportOwnError(f, &capturedResponse{Body: tenantExportTestArchive(t, map[string][]byte{"accounts": []byte(display)})}) == nil {
		t.Fatal("export own control accepted only the account display canary")
	}
	for _, item := range tenantExportCanaryKinds {
		t.Run(item.kind, func(t *testing.T) {
			if tenantExportOwnError(f, &capturedResponse{Body: body(item.kind)}) == nil {
				t.Fatal("export own control accepted a missing object canary")
			}
		})
	}
	if tenantExportOwnError(f, &capturedResponse{Body: body("vector-row")}) == nil {
		t.Fatal("export own control accepted a profile without its memory vector")
	}
	row := tenantF1Export()
	if len(row.Probes) == 0 || !row.Probes[0].ShowsOwn || row.Probes[0].Scratch != "S-export" {
		t.Fatal("export must retain its populated scratch account own control")
	}
}

func tenantExportTestArchive(t *testing.T, chunks map[string][]byte) []byte {
	t.Helper()
	var raw bytes.Buffer
	w := tar.NewWriter(&raw)
	var tables []string
	for table := range chunks {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		data := chunks[table]
		tenantSeedObjectError(t, "write export checker header", w.WriteHeader(&tar.Header{Name: table + "/000001.ndjson", Mode: 0o600, Size: int64(len(data))}))
		_, err := w.Write(data)
		tenantSeedObjectError(t, "write export checker chunk", err)
	}
	tenantSeedObjectError(t, "close export checker archive", w.Close())
	return raw.Bytes()
}
