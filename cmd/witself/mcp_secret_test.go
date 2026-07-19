package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/sealed"
	"github.com/witwave-ai/witself/internal/secretclient"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const (
	testMCPSecretID       = "sec_aaaaaaaaaaaaaaaa"
	testMCPUsernameField  = "fld_bbbbbbbbbbbbbbbb"
	testMCPPasswordField  = "fld_cccccccccccccccc"
	testMCPTOTPField      = "fld_dddddddddddddddd"
	testMCPBinaryField    = "fld_eeeeeeeeeeeeeeee"
	testMCPTOTPSeedBase32 = "JBSWY3DPEHPK3PXP"
)

type fakeSecretMCPBackend struct {
	*fakeMCPBackend

	searchPage    client.SecretPage
	shownSecret   client.Secret
	revealed      map[string][]byte
	lastSearch    client.SecretListOptions
	lastShowID    string
	lastCreate    secretclient.CreateInput
	createdValues map[string]string
	lastReveal    struct {
		secretID       string
		fieldID        string
		idempotencyKey string
	}
	revealCalls int
}

func newFakeSecretMCPBackend() *fakeSecretMCPBackend {
	publicUsername := "octavia"
	forbiddenSensitiveProjection := "must-never-be-returned"
	secret := client.Secret{
		ID: testMCPSecretID, AccountID: "acc_aaaaaaaaaaaaaaaa",
		RealmID: "realm_bbbbbbbbbbbbbbbb", OwnerAgentID: "agent_cccccccccccccccc",
		Name: "GitHub account", Template: "login", Tags: []string{"github"},
		Lifecycle: "active", RowVersion: 1, SensitiveCount: 3,
		Fields: []client.SecretField{
			{ID: testMCPUsernameField, Name: "username", Kind: "username", Encoding: sealed.ValueEncodingUTF8, PublicValue: &publicUsername},
			{ID: testMCPPasswordField, Name: "password", Kind: "password", Encoding: sealed.ValueEncodingUTF8, Sensitive: true, PublicValue: &forbiddenSensitiveProjection},
			{ID: testMCPTOTPField, Name: "totp", Kind: "totp", Encoding: sealed.ValueEncodingJSON, Sensitive: true, PublicValue: &forbiddenSensitiveProjection},
			{ID: testMCPBinaryField, Name: "private_key", Kind: "private_key", Encoding: sealed.ValueEncodingBinary, Sensitive: true, PublicValue: &forbiddenSensitiveProjection},
		},
	}
	return &fakeSecretMCPBackend{
		fakeMCPBackend: &fakeMCPBackend{},
		searchPage:     client.SecretPage{Items: []client.Secret{secret}, NextCursor: "next-secret-page"},
		shownSecret:    secret,
		revealed: map[string][]byte{
			testMCPPasswordField: []byte("correct horse battery staple"),
			testMCPBinaryField:   {0, 1, 2, 255},
		},
		createdValues: map[string]string{},
	}
}

func (b *fakeSecretMCPBackend) SearchSecrets(_ context.Context, options client.SecretListOptions) (*client.SecretPage, error) {
	b.lastSearch = options
	page := b.searchPage
	page.Items = append([]client.Secret(nil), page.Items...)
	return &page, nil
}

func (b *fakeSecretMCPBackend) ShowSecret(_ context.Context, secretID string) (*client.Secret, error) {
	b.lastShowID = secretID
	value := b.shownSecret
	value.Fields = append([]client.SecretField(nil), value.Fields...)
	return &value, nil
}

func (b *fakeSecretMCPBackend) CreateSealedSecret(_ context.Context, input secretclient.CreateInput) (*client.SecretMutationResult, error) {
	b.lastCreate = input
	b.lastCreate.Fields = append([]secretclient.FieldInput(nil), input.Fields...)
	for _, field := range input.Fields {
		b.createdValues[field.Name] = string(append([]byte(nil), field.Value...))
	}
	secret := b.shownSecret
	secret.Name = input.Name
	secret.Description = input.Description
	secret.Template = input.Template
	secret.Tags = append([]string(nil), input.Tags...)
	for index := range secret.Fields {
		if secret.Fields[index].Sensitive {
			leak := b.createdValues[secret.Fields[index].Name]
			secret.Fields[index].PublicValue = &leak
			secret.Fields[index].Redacted = false
		}
	}
	return &client.SecretMutationResult{
		Secret: secret,
		Receipt: client.SecretMutationReceipt{
			Operation: "secret_create", TargetKind: "secret", TargetID: testMCPSecretID,
			RequestHash: "public-request-hash", ResultRevision: 1,
		},
	}, nil
}

func (b *fakeSecretMCPBackend) RevealSealedSecretField(_ context.Context, secretID, fieldID, idempotencyKey string) ([]byte, error) {
	b.revealCalls++
	b.lastReveal.secretID = secretID
	b.lastReveal.fieldID = fieldID
	b.lastReveal.idempotencyKey = idempotencyKey
	return append([]byte(nil), b.revealed[fieldID]...), nil
}

func TestMCPSecretToolsRespectProfilesNamesAndAnnotations(t *testing.T) {
	backend := newFakeSecretMCPBackend()
	full := listSecretMCPTools(t, newWitselfMCPServerForRuntime(backend, transcriptcapture.RuntimeCursor))
	wants := map[string]struct {
		readOnly   bool
		idempotent bool
		phrase     string
	}{
		"witself.secret.search":     {readOnly: true, idempotent: true, phrase: "redacted inventory"},
		"witself.secret.show":       {readOnly: true, idempotent: true, phrase: "redacted"},
		"witself.secret.create":     {idempotent: true, phrase: "result is always redacted"},
		"witself.secret.reveal":     {idempotent: true, phrase: "Explicit value-returning"},
		"witself.password.generate": {phrase: "Explicit value-returning local"},
		"witself.totp.code":         {phrase: "Explicit value-returning"},
	}
	for name, want := range wants {
		tool := full[name]
		assertMCPToolAnnotations(t, tool, name, want.readOnly, false, want.idempotent)
		if !strings.Contains(tool.Description, want.phrase) {
			t.Errorf("%s description does not contain %q: %q", name, want.phrase, tool.Description)
		}
	}

	readOnly := listSecretMCPTools(t, newWitselfMCPServerForRuntimeOptions(
		backend, transcriptcapture.RuntimeCursor, mcpServerOptions{Profile: mcpProfileReadOnly},
	))
	for _, name := range []string{"witself.secret.search", "witself.secret.show"} {
		if readOnly[name] == nil {
			t.Errorf("read-only MCP omitted %s", name)
		}
	}
	for _, name := range []string{
		"witself.secret.create", "witself.secret.reveal", "witself.password.generate", "witself.totp.code",
	} {
		if readOnly[name] != nil {
			t.Errorf("read-only MCP advertised %s", name)
		}
		found := false
		for _, registered := range mcpMutatingToolNames(transcriptcapture.RuntimeCursor) {
			found = found || registered == name
		}
		if !found {
			t.Errorf("read-only mutation registry omitted %s", name)
		}
	}

	for _, profile := range []string{mcpProfileCuratorPreview, mcpProfileCuratorApply} {
		tools := listSecretMCPTools(t, newWitselfMCPServerForRuntimeOptions(
			backend, transcriptcapture.RuntimeCursor, mcpServerOptions{Profile: profile},
		))
		for name := range tools {
			if strings.HasPrefix(name, "witself.secret.") || name == "witself.password.generate" || name == "witself.totp.code" {
				t.Errorf("%s profile advertised secret tool %s", profile, name)
			}
		}
	}

	grok := listSecretMCPTools(t, newWitselfMCPServerForRuntime(backend, transcriptcapture.RuntimeGrokBuild))
	for dotted := range wants {
		portable := strings.ReplaceAll(dotted, ".", "_")
		if grok[portable] == nil {
			t.Errorf("Grok MCP omitted portable secret tool %s", portable)
		} else if strings.Contains(grok[portable].Description, "witself.") {
			t.Errorf("Grok MCP secret description contains a dotted tool name: %q", grok[portable].Description)
		}
		if grok[dotted] != nil {
			t.Errorf("Grok MCP advertised dotted secret tool %s", dotted)
		}
	}
}

func TestMCPSecretSearchShowAndCreateStayRedacted(t *testing.T) {
	backend := newFakeSecretMCPBackend()
	session := connectSecretMCPTest(t, newWitselfMCPServer(backend))

	search := callSecretMCPTool(t, session, "witself.secret.search", map[string]any{
		"query": "github", "template": "login", "tags": []string{"github"},
		"limit": 7, "include_fields": true,
	})
	assertMCPResultExcludes(t, search, "must-never-be-returned")
	if backend.lastSearch.Query != "github" || backend.lastSearch.Lifecycle != "active" ||
		backend.lastSearch.Template != "login" || backend.lastSearch.Limit != 7 || !backend.lastSearch.IncludeFields {
		t.Fatalf("search options = %#v", backend.lastSearch)
	}

	show := callSecretMCPTool(t, session, "witself.secret.show", map[string]any{"secret_id": testMCPSecretID})
	assertMCPResultExcludes(t, show, "must-never-be-returned")
	if backend.lastShowID != testMCPSecretID {
		t.Fatalf("show secret id = %q", backend.lastShowID)
	}

	const explicitValue = "client-only-api-key-value"
	otpauthURI := "otpauth://totp/Witself:scott%40example.com?secret=" + testMCPTOTPSeedBase32 + "&issuer=Witself&algorithm=SHA1&digits=6&period=30"
	create := callSecretMCPTool(t, session, "witself.secret.create", map[string]any{
		"name": "GitHub account", "description": "agent login", "template": "login",
		"tags": []string{"github", "automation"}, "idempotency_key": "create-mcp-secret-1",
		"fields": []any{
			map[string]any{"name": "api_key", "kind": "api_key", "value": explicitValue},
			map[string]any{"name": "password", "kind": "password", "generate_password": true, "password_policy": map[string]any{"length": 24, "symbols": false}},
			map[string]any{"name": "totp", "kind": "totp", "otpauth_uri": otpauthURI},
		},
	})
	if backend.lastCreate.IdempotencyKey != "create-mcp-secret-1" || backend.createdValues["api_key"] != explicitValue {
		t.Fatalf("captured create = %#v / values %#v", backend.lastCreate, backend.createdValues)
	}
	generatedPassword := backend.createdValues["password"]
	if len(generatedPassword) != 24 {
		t.Fatalf("generated password length = %d, want 24", len(generatedPassword))
	}
	payload, err := sealed.ParseTOTPPayload([]byte(backend.createdValues["totp"]))
	if err != nil || payload.Metadata().Issuer != "Witself" || payload.Metadata().Account != "scott@example.com" {
		t.Fatalf("canonical TOTP payload = %#v / %v", payload, err)
	}
	assertMCPResultExcludes(t, create, explicitValue, generatedPassword, otpauthURI, testMCPTOTPSeedBase32, "must-never-be-returned", "seed_base32")
}

func TestMCPSecretValueToolsAreExactLocalAndTOTPSafe(t *testing.T) {
	backend := newFakeSecretMCPBackend()
	otpauthURI := "otpauth://totp/Witself:scott%40example.com?secret=" + testMCPTOTPSeedBase32 + "&issuer=Witself"
	payload, err := sealed.ParseOTPAuthTOTP(otpauthURI)
	if err != nil {
		t.Fatal(err)
	}
	encodedTOTP, err := sealed.EncodeTOTPPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(encodedTOTP)
	backend.revealed[testMCPTOTPField] = append([]byte(nil), encodedTOTP...)

	oldNow := mcpSecretNow
	mcpSecretNow = func() time.Time { return time.Unix(59, 0).UTC() }
	t.Cleanup(func() { mcpSecretNow = oldNow })
	session := connectSecretMCPTest(t, newWitselfMCPServer(backend))

	revealed := callSecretMCPTool(t, session, "witself.secret.reveal", map[string]any{
		"secret_id": testMCPSecretID, "field_id": testMCPPasswordField, "idempotency_key": "reveal-password-1",
	})
	rawReveal := marshalMCPSecretResult(t, revealed)
	if !strings.Contains(rawReveal, "correct horse battery staple") || backend.lastReveal.fieldID != testMCPPasswordField || backend.lastReveal.idempotencyKey != "reveal-password-1" {
		t.Fatalf("password reveal = %s / backend %#v", rawReveal, backend.lastReveal)
	}

	public := callSecretMCPTool(t, session, "witself.secret.reveal", map[string]any{
		"secret_id": testMCPSecretID, "field_id": testMCPUsernameField, "idempotency_key": "reveal-public-1",
	})
	if raw := marshalMCPSecretResult(t, public); !strings.Contains(raw, "octavia") {
		t.Fatalf("public exact-field reveal = %s", raw)
	}
	if backend.revealCalls != 1 {
		t.Fatalf("public reveal fetched encrypted material; calls = %d", backend.revealCalls)
	}

	binary := callSecretMCPTool(t, session, "witself.secret.reveal", map[string]any{
		"secret_id": testMCPSecretID, "field_id": testMCPBinaryField, "idempotency_key": "reveal-binary-1",
	})
	if raw := marshalMCPSecretResult(t, binary); !strings.Contains(raw, `"value_base64":"AAEC/w=="`) {
		t.Fatalf("binary exact-field reveal = %s", raw)
	}

	totpReveal, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "witself.secret.reveal", Arguments: map[string]any{
			"secret_id": testMCPSecretID, "field_id": testMCPTOTPField, "idempotency_key": "forbidden-totp-reveal",
		},
	})
	if err != nil || !totpReveal.IsError {
		t.Fatalf("raw TOTP reveal = %#v / %v, want tool error", totpReveal, err)
	}
	if backend.revealCalls != 2 {
		t.Fatalf("raw TOTP reveal reached encrypted material; calls = %d", backend.revealCalls)
	}
	assertMCPResultExcludes(t, totpReveal, testMCPTOTPSeedBase32, "seed_base32", otpauthURI)

	password := callSecretMCPTool(t, session, "witself.password.generate", map[string]any{
		"length": 20, "symbols": false, "exclude_ambiguous": true,
	})
	var generated mcpPasswordGenerateOutput
	decodeMCPSecretStructured(t, password, &generated)
	if len(generated.Password) != 20 || generated.Length != 20 {
		t.Fatalf("generated password = %#v", generated)
	}

	codeResult := callSecretMCPTool(t, session, "witself.totp.code", map[string]any{
		"secret_id": testMCPSecretID, "field_id": testMCPTOTPField, "idempotency_key": "totp-code-1",
	})
	var code mcpTOTPCodeOutput
	decodeMCPSecretStructured(t, codeResult, &code)
	if code.SecretID != testMCPSecretID || code.FieldID != testMCPTOTPField || len(code.Code) != 6 ||
		code.PeriodSeconds != 30 || code.ExpiresAt.Unix() != 60 || backend.lastReveal.idempotencyKey != "totp-code-1" {
		t.Fatalf("TOTP code result = %#v / backend %#v", code, backend.lastReveal)
	}
	assertMCPResultExcludes(t, codeResult, testMCPTOTPSeedBase32, "seed_base32", otpauthURI, string(encodedTOTP), "Witself", "scott@example.com")
}

func TestConfiguredMCPSecretBackendRejectsIdentityDriftBeforeSecretAccess(t *testing.T) {
	secretRequests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v1/self" {
			_ = json.NewEncoder(w).Encode(client.SelfDigest{Identity: client.SelfIdentity{
				AccountID: "acc_aaaaaaaaaaaaaaaa", RealmID: "realm_bbbbbbbbbbbbbbbb", RealmName: "default",
				AgentID: "agent_zzzzzzzzzzzzzzzz", AgentName: "scott",
			}})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/secrets") {
			secretRequests++
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	tokenPath := filepath.Join(t.TempDir(), "scott.token")
	if err := os.WriteFile(tokenPath, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := configuredMCPBackend{cfg: transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeCodex, Account: "default", AccountID: "acc_aaaaaaaaaaaaaaaa",
		Realm: "default", RealmID: "realm_bbbbbbbbbbbbbbbb", Agent: "scott",
		AgentID: "agent_cccccccccccccccc", AgentName: "scott", Endpoint: srv.URL, TokenFile: tokenPath,
	}}
	if _, err := backend.SearchSecrets(context.Background(), client.SecretListOptions{}); err == nil || !strings.Contains(err.Error(), "agent id") {
		t.Fatalf("identity-drift search error = %v", err)
	}
	if secretRequests != 0 {
		t.Fatalf("identity drift reached secret API %d time(s)", secretRequests)
	}
}

func listSecretMCPTools(t *testing.T, server *mcp.Server) map[string]*mcp.Tool {
	t.Helper()
	session := connectSecretMCPTest(t, server)
	page, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := make(map[string]*mcp.Tool, len(page.Tools))
	for _, tool := range page.Tools {
		tools[tool.Name] = tool
	}
	return tools
}

func connectSecretMCPTest(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "secret-test", Version: "1"}, nil).Connect(
		context.Background(), clientTransport, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

func callSecretMCPTool(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("%s returned tool error: %s", name, marshalMCPSecretResult(t, result))
	}
	return result
}

func marshalMCPSecretResult(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func decodeMCPSecretStructured(t *testing.T, result *mcp.CallToolResult, target any) {
	t.Helper()
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode structured result %s: %v", raw, err)
	}
}

func assertMCPResultExcludes(t *testing.T, result *mcp.CallToolResult, forbidden ...string) {
	t.Helper()
	raw := marshalMCPSecretResult(t, result)
	for _, value := range forbidden {
		if value != "" && strings.Contains(raw, value) {
			t.Errorf("MCP result leaked %q: %s", value, raw)
		}
	}
}
