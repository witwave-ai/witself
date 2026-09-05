package stubcell

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/witwave-ai/witself/internal/client"
)

// SecretCanary is synthetic private fixture material, never a real credential.
// The standalone cell projects it out before any HTTP response is encoded.
const SecretCanary = "STUB_SECRET_CANARY"

// Config selects the test identity and optional bearer authentication. A blank
// bearer is useful only for existing in-process proxy tests.
type Config struct {
	BearerToken string
	Identity    client.SelfIdentity
	MinimalSelf bool
}

// Identity returns the established dashboard test identity with the supplied
// agent name. Acceptance uses "acceptance" and CLI regression tests use "dash".
func Identity(agentName string) client.SelfIdentity {
	return client.SelfIdentity{AccountID: "acc_1", AgentID: "agt_dash", AgentName: agentName, RealmID: "rlm_1", RealmName: "default"}
}

// SelfDigest is the minimal old-cell response used by the existing tests.
func SelfDigest(identity client.SelfIdentity) client.SelfDigest {
	return client.SelfDigest{SchemaVersion: "witself.v0", Identity: identity}
}

func acceptanceSelf(identity client.SelfIdentity) client.SelfDigest {
	digest := SelfDigest(identity)
	digest.Index.Counts = map[string]int{"facts": 2, "memories": 1, "transcripts": 1, "secrets": 1}
	digest.SalientMemories = []client.SelfMemory{{ID: "mem_1", Kind: "note", Snippet: "Acceptance fixture memory", Salience: 0.8}}
	digest.FactCapacity = &client.FactLimitStatus{Used: 2, Unlimited: true}
	digest.MemoryCapacity = &client.MemoryLimitStatus{Used: 1, Unlimited: true}
	digest.PlanEntitlements = &client.SelfAgentEntitlements{
		SchemaVersion: "witself.agent-entitlements.v1", State: "applied", Source: "cell_applied_snapshot", EnforcedPlanID: "standard",
		Features:      &client.SelfAgentEntitlementFeatures{Memory: true, Facts: true, Secrets: true, Messaging: true, Collaboration: true, AgentEmailReceive: true, AgentEmailSend: true},
		RetentionDays: &client.SelfAgentRetentionDays{},
	}
	return digest
}

// SecretMetadata projects the same deliberately leaky fixture used by the
// proxy tests into metadata for the standalone cell. No private field value,
// including the canary, is ever serialized on the standalone cell API.
func SecretMetadata() map[string]any {
	full := LeakySecret()
	metadata := make(map[string]any)
	for _, key := range []string{"id", "name", "template", "tags", "lifecycle", "sensitive_field_count", "created_at", "updated_at"} {
		metadata[key] = full[key]
	}
	fields := make([]map[string]any, 0, 2)
	for _, field := range full["fields"].([]map[string]any) {
		fields = append(fields, map[string]any{"id": field["id"], "name": field["name"], "kind": field["kind"], "sensitive": field["sensitive"]})
	}
	metadata["fields"] = fields
	return metadata
}

// New serves the shared read-only fixtures. Unknown and mutating routes fail
// closed, and the facts capability probe matches an observational-capable cell.
func New(cfg Config) http.Handler {
	if cfg.Identity.AgentID == "" {
		cfg.Identity = Identity("acceptance")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		write := func(status int, value any) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(value)
		}
		if cfg.BearerToken != "" && r.Header.Get("Authorization") != "Bearer "+cfg.BearerToken {
			write(http.StatusUnauthorized, map[string]string{"error": "invalid bearer token"})
			return
		}
		if r.Method != http.MethodGet {
			write(http.StatusMethodNotAllowed, map[string]string{"error": "read-only stub cell"})
			return
		}
		var response any
		switch r.URL.Path {
		case "/v1/self":
			if cfg.MinimalSelf {
				response = SelfDigest(cfg.Identity)
			} else {
				response = acceptanceSelf(cfg.Identity)
			}
		case "/v1/transcripts":
			response = Transcripts()
		case "/v1/self/dashboard-preferences":
			response = map[string]any{"schema_version": "witself.v0", "preferences": nil}
		case "/v1/self/avatar":
			response = Avatar()
		case "/v1/transcripts/tr_1":
			response = TranscriptDetail()
		case "/v1/memories":
			response = Memories()
		case "/v1/memories/mem_1":
			response = MemoryDetail()
		case "/v1/memories/mem_1/history":
			response = MemoryHistory()
		case "/v1/messages":
			response = Messages()
		case "/v1/email/address":
			response = EmailAddress()
		case "/v1/email:status":
			response = EmailStatus()
		case "/v1/email":
			response = Emails()
		case "/v1/email/sent":
			response = SentEmails()
		case "/v1/facts":
			if raw := r.URL.Query().Get("observational"); raw != "" {
				if _, err := strconv.ParseBool(raw); err != nil {
					write(http.StatusBadRequest, map[string]string{"error": "observational must be true or false"})
					return
				}
			}
			response = Facts()
		case "/v1/secrets":
			response = map[string]any{"items": []map[string]any{SecretMetadata()}}
		default:
			write(http.StatusNotFound, map[string]string{"error": "unknown stub cell route"})
			return
		}
		write(http.StatusOK, response)
	})
}
