package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemailoutbound"
	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/worker"
)

var captureProofJobSnapshot = []string{
	"avatar_style_rollout", "message_rate_bucket_cleanup", "agent_email_rate_bucket_cleanup", "transcript_storage_reconcile",
	"agent_email_outbound", "account_purge", "transcript_retention", "message_retention", "agent_email_retention", "support_ticket_age_out",
}

func captureProofWorkerValues(t *testing.T) map[string]string {
	t.Helper()
	values := localWorkerValues(t)
	values["WITSELF_LOCAL_SYNC_SCHEMA"] = "1"
	values["WITSELF_LOCAL_CAPTURE_PROOF"] = "1"
	values["WITSELF_LOCAL_CAPTURE_PROOF_TOKEN"] = "private-proof-fixture"
	values["WITSELF_LOCAL_CAPTURE_PROOF_STUB_EMAIL"] = "http://127.0.0.1:1234/v1/dispatch"
	return values
}

func assertCaptureProofRegistration(t *testing.T, registry *worker.Registry, expected map[string]bool) {
	t.Helper()
	// Inert cardinality inspection closes the unknown-extra-job gap in the
	// duplicate-registration probes without adding a product introspection API.
	if reflect.ValueOf(registry).Elem().FieldByName("jobs").Len() != len(expected) {
		t.Fatal("capture profile registered job count differs from snapshot")
	}
	for _, name := range captureProofJobSnapshot {
		err := registry.Register(worker.Job{Name: name, Run: func(context.Context) error { return nil }})
		if (err != nil) != expected[name] {
			t.Fatalf("capture profile job snapshot mismatch for %s", name)
		}
	}
}

func TestWorkerCaptureProofRegistrationSnapshot(t *testing.T) {
	for _, mode := range []string{"hosted", "ordinary local", "profile"} {
		t.Run(mode, func(t *testing.T) {
			values := map[string]string{}
			expected := map[string]bool{"avatar_style_rollout": true, "message_rate_bucket_cleanup": true, "agent_email_rate_bucket_cleanup": true}
			if mode == "ordinary local" {
				values = localWorkerValues(t)
				values["WITSELF_LOCAL_JOBS"] = "avatar_style_rollout,transcript_storage_reconcile"
				expected = map[string]bool{"avatar_style_rollout": true, "transcript_storage_reconcile": true}
			}
			if mode == "profile" {
				values = captureProofWorkerValues(t)
				for _, name := range captureProofJobSnapshot {
					expected[name] = true
				}
			}
			cfg, jobs, err := workerConfigFromEnv(mapLookup(values), nil)
			if err != nil {
				t.Fatal("capture profile job configuration failed")
			}
			captureWorkerStderr(t, func() {
				registry := worker.NewRegistry()
				if registerJobs(registry, nil, jobs, cfg.CaptureProof) != 0 {
					t.Fatal("capture profile job registration failed")
				}
				assertCaptureProofRegistration(t, registry, expected)
			})
			if cfg.CaptureProof {
				selected := captureProofJobs(jobs)
				if selected.retention.Mode != store.TranscriptRetentionModeEnforce || selected.messageRetention.Mode != store.MessageRetentionModeEnforce || selected.agentEmailRetention.Mode != store.AgentEmailRetentionModeEnforce || selected.accountPurge.Mode != store.AccountPurgeModeEnforce {
					t.Fatal("capture profile retained a preview-only writer")
				}
				// Registration alone is the opt-in boundary. Parsing the profile
				// must not mutate ordinary local job selection.
				if jobs.agentEmailOutboundEnabled || jobs.accountPurgeEnabled || jobs.retentionEnabled {
					t.Fatal("capture profile escaped explicit registration argument")
				}
			}
		})
	}
}

type captureProofWorkerStore struct {
	workerBootstrapStore
	markerError error
	markerCalls int
	syncVersion int
	migrations  int
}

func (s *captureProofWorkerStore) VerifySyncCaptureProofDatabase(context.Context, string) error {
	s.markerCalls++
	return s.markerError
}
func (s *captureProofWorkerStore) AppliedSyncSchemaVersion(context.Context) (int, error) {
	return s.syncVersion, nil
}
func (s *captureProofWorkerStore) Migrate() error                    { s.migrations++; return nil }
func (s *captureProofWorkerStore) MigrateSync(context.Context) error { s.migrations++; return nil }

func TestWorkerCaptureProofStartup(t *testing.T) {
	values := captureProofWorkerValues(t)
	cfg, jobs, err := workerConfigFromEnv(mapLookup(values), nil)
	if err != nil {
		t.Fatal("capture profile startup configuration failed")
	}
	for _, identified := range []bool{false, true} {
		st := &captureProofWorkerStore{workerBootstrapStore: workerBootstrapStore{version: store.SchemaVersion(), account: "fixture"}, syncVersion: 1}
		if !identified {
			st.markerError = errors.New("private-marker-diagnostic")
		}
		if err := migrateWorkerStore(context.Background(), st, cfg); (err == nil) != identified || st.migrations != 0 {
			t.Fatal("capture profile startup migrated or accepted an unidentified database")
		}
		ran := false
		output := captureWorkerStderr(t, func() {
			code := serveWorker(context.Background(), nil, st, cfg, jobs, mapLookup(nil), func(registry *worker.Registry, ctx context.Context, wc worker.Config) error {
				ran = true
				if !identified {
					t.Fatal("capture profile marker refusal reached registry")
				}
				expected := make(map[string]bool)
				for _, name := range captureProofJobSnapshot {
					expected[name] = true
				}
				assertCaptureProofRegistration(t, registry, expected)
				if wc.SyncSchemaReady == nil || wc.SyncSchemaReady(ctx) != nil {
					t.Fatal("capture profile pre-install readiness failed")
				}
				st.syncVersion = 3
				if wc.SyncSchemaReady(ctx) != nil {
					t.Fatal("capture profile post-install readiness failed")
				}
				st.markerError = errors.New("expired")
				if wc.SyncSchemaReady(ctx) == nil {
					t.Fatal("capture profile expired marker retained readiness")
				}
				return nil
			})
			if (code == 0) != identified || ran != identified {
				t.Fatal("capture profile startup registration ordering changed")
			}
		})
		if strings.Contains(output, "private-") {
			t.Fatal("capture profile startup disclosed marker detail")
		}
		if !identified && output != "witself-worker: capture_proof_database_unidentified\n" {
			t.Fatal("capture profile startup fixed refusal changed")
		}
	}
}

func TestWorkerCaptureProofDispatchIsolation(t *testing.T) {
	requests := 0
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != agentemailoutbound.DispatchPath {
			t.Error("capture profile provider request route changed")
		}
		var dispatch agentemailoutbound.Dispatch
		if json.NewDecoder(r.Body).Decode(&dispatch) != nil {
			t.Error("capture profile provider request malformed")
		}
		_ = json.NewEncoder(w).Encode(agentemailoutbound.Response{SchemaVersion: agentemailoutbound.ResponseSchemaVersion, SendID: dispatch.SendID, State: agentemailoutbound.StateAccepted, Provider: "managed", ProviderMessageID: "proof-fixture"})
	}))
	defer stub.Close()
	client, err := captureProofDispatchClient(stub.URL + agentemailoutbound.DispatchPath)
	if err != nil || client.Validate() != nil {
		t.Fatal("capture profile signed loopback dispatch client invalid")
	}
	client.HTTPClient.Timeout = time.Second
	dispatch := agentemailoutbound.Dispatch{SchemaVersion: agentemailoutbound.DispatchSchemaVersion, SendID: "esnd_fixture", AccountID: "acc_fixture", RealmID: "realm_fixture", AgentID: "agent_fixture", From: "fixture@send.witmail.net", ReplyTo: "fixture@witmail.net", To: "fixture@example.invalid", Text: "fixture"}
	if _, err := client.Send(context.Background(), dispatch); err != nil || requests != 1 {
		t.Fatal("capture profile signed request did not reach isolated stub")
	}
	for _, endpoint := range []string{"https://192.0.2.1:1234/v1/dispatch", "https://127.0.0.1:1/v1/dispatch"} {
		client.Endpoint = endpoint
		if _, err := client.Send(context.Background(), dispatch); err == nil || requests != 1 {
			t.Fatal("capture profile transport accepted a substituted origin")
		}
	}
}

func TestWorkerCaptureProofDispatchDoesNotFollowRedirects(t *testing.T) {
	forwarded := false
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded = true }))
	defer sink.Close()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+agentemailoutbound.DispatchPath, http.StatusTemporaryRedirect)
	}))
	defer stub.Close()
	client, err := captureProofDispatchClient(stub.URL + agentemailoutbound.DispatchPath)
	if err != nil {
		t.Fatal("capture profile redirect fixture failed")
	}
	client.HTTPClient.Timeout = time.Second
	dispatch := agentemailoutbound.Dispatch{SchemaVersion: agentemailoutbound.DispatchSchemaVersion, SendID: "esnd_fixture", AccountID: "acc_fixture", RealmID: "realm_fixture", AgentID: "agent_fixture", From: "fixture@send.witmail.net", ReplyTo: "fixture@witmail.net", To: "fixture@example.invalid", Text: "fixture"}
	if _, err := client.Send(context.Background(), dispatch); err == nil || forwarded {
		t.Fatal("capture profile provider redirect escaped isolated stub")
	}
}

func TestWorkerCaptureProofUnselectedConfiguration(t *testing.T) {
	values := localWorkerValues(t)
	beforeRuntime, beforeJobs, err := workerConfigFromEnv(mapLookup(values), nil)
	if err != nil {
		t.Fatal("capture profile unselected baseline failed")
	}
	values["WITSELF_LOCAL_SYNC_SCHEMA"] = "1"
	afterRuntime, afterJobs, err := workerConfigFromEnv(mapLookup(values), nil)
	if err != nil {
		t.Fatal("capture profile unselected sync configuration failed")
	}
	afterRuntime.SyncSchema = false
	if !reflect.DeepEqual(beforeRuntime, afterRuntime) || !reflect.DeepEqual(beforeJobs, afterJobs) || afterRuntime.CaptureProof {
		t.Fatal("capture profile unselected ordinary local bytes changed")
	}
}
