package runtimeconfig

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func captureProofValues(t *testing.T) map[string]string {
	t.Helper()
	values := localValues(t)
	values["WITSELF_LOCAL_SYNC_SCHEMA"] = "1"
	values["WITSELF_LOCAL_CAPTURE_PROOF"] = "1"
	values["WITSELF_LOCAL_CAPTURE_PROOF_TOKEN"] = "private-proof-fixture"
	values["WITSELF_LOCAL_CAPTURE_PROOF_STUB_EMAIL"] = "http://127.0.0.1:1234/v1/dispatch"
	return values
}

func TestCaptureProofSelection(t *testing.T) {
	for _, role := range []string{"server", "worker"} {
		for _, tc := range []struct{ name, key, value, cause string }{
			{"selected", "", "", ""},
			{"hosted", "WITSELF_RUNTIME", "", "capture_proof_requires_local_sync"},
			{"runtime alias", "WITSELF_RUNTIME", "LOCAL", "capture_proof_requires_local_sync"},
			{"without sync", "WITSELF_LOCAL_SYNC_SCHEMA", "", "capture_proof_requires_local_sync"},
			{"sync alias", "WITSELF_LOCAL_SYNC_SCHEMA", "true", "capture_proof_requires_local_sync"},
			{"empty selector", "WITSELF_LOCAL_CAPTURE_PROOF", "", "capture_proof_requires_local_sync"},
			{"disabled selector", "WITSELF_LOCAL_CAPTURE_PROOF", "0", "capture_proof_requires_local_sync"},
			{"value alias", "WITSELF_LOCAL_CAPTURE_PROOF", "true", "capture_proof_requires_local_sync"},
			{"padded selector", "WITSELF_LOCAL_CAPTURE_PROOF", " 1", "capture_proof_requires_local_sync"},
			{"missing token", "WITSELF_LOCAL_CAPTURE_PROOF_TOKEN", "", "capture_proof_database_unidentified"},
		} {
			t.Run(role+"/"+tc.name, func(t *testing.T) {
				values := captureProofValues(t)
				if tc.key != "" {
					values[tc.key] = tc.value
				}
				cfg, err := FromEnv(role, testLookup(values), nil)
				if tc.cause != "" {
					if err == nil || err.Error() != tc.cause {
						t.Fatal("capture profile selection refusal changed")
					}
				} else if err != nil || !cfg.Local || !cfg.SyncSchema || !cfg.CaptureProof || cfg.CaptureProofToken != values["WITSELF_LOCAL_CAPTURE_PROOF_TOKEN"] {
					t.Fatal("capture profile explicit selection failed")
				}
			})
		}
	}
}

func TestCaptureProofEnvironmentBoundary(t *testing.T) {
	for _, name := range []string{"WITSELF_LOCAL_CAPTURE_PROOF", "WITSELF_LOCAL_CAPTURE_PROOF_TOKEN", "WITSELF_LOCAL_CAPTURE_PROOF_STUB_EMAIL", "witself_local_capture_proof", "WITSELF_LOCAL_CAPTURE_PROOF_STUB_UNKNOWN", "witself_local_capture_proof_stub_email"} {
		for _, selected := range []bool{false, true} {
			t.Run(name+"/"+map[bool]string{false: "unselected", true: "selected"}[selected], func(t *testing.T) {
				values := localValues(t)
				if selected {
					values = captureProofValues(t)
				}
				values[name] = "private-proof-marker"
				_, err := FromEnv("worker", testLookup(values), []string{name + "=private-proof-marker"})
				if selected && name == "WITSELF_LOCAL_CAPTURE_PROOF_TOKEN" {
					if err != nil {
						t.Fatal("capture profile exact token name refused")
					}
					return
				}
				if selected && name == "WITSELF_LOCAL_CAPTURE_PROOF_STUB_EMAIL" {
					if err == nil || err.Error() != "capture_proof_stub_invalid" {
						t.Fatal("capture profile stub value validation skipped")
					}
					return
				}
				if err == nil || err.Error() != "capture_proof_requires_local_sync" {
					t.Fatal("capture profile ambient alias or unselected setting accepted")
				}
			})
		}
	}
	for _, forbidden := range []string{"WITSELF_PROVISION_TOKEN", "WITSELF_BACKUP_TOKEN", "WITSELF_BACKUP_VALIDATION_ENABLED", "WITSELF_BILLING_ENDPOINT", "WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED", "WITSELF_AGENT_EMAIL_RECEIVE_PRODUCTION_ENABLED", "WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_ENDPOINT", "WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_PRIVATE_KEY", "WITSELF_AGENT_EMAIL_PROVIDER_EVENT_TOKEN"} {
		values := captureProofValues(t)
		values[forbidden] = "private-proof-marker"
		_, err := FromEnv("worker", testLookup(values), []string{forbidden + "=private-proof-marker"})
		if err == nil || strings.Contains(err.Error(), "private-proof-marker") {
			t.Fatal("capture profile ambient provider configuration accepted or disclosed")
		}
	}
}

func TestCaptureProofLoopbackStub(t *testing.T) {
	for _, endpoint := range []string{"http://127.0.0.1:00/v1/dispatch", "http://127.0.0.1:1234/v1/dispatch#"} {
		if CaptureProofLoopbackEndpoint(endpoint) {
			t.Fatal("capture profile ambiguous loopback endpoint accepted")
		}
	}
	for _, endpoint := range []string{"", "http://localhost:12/v1/dispatch", "https://127.0.0.1:12/v1/dispatch", "http://192.0.2.1:12/v1/dispatch", "http://127.0.0.1:0/v1/dispatch", "http://127.0.0.1:12/other", "http://127.0.0.1:12/v1/dispatch?x", "http://127.0.0.1:12/v1/dispatch?", "http://127.0.0.1:12/v1/dispatch#x", "http://name@127.0.0.1:12/v1/dispatch", "http://127.0.0.1:12/v1/%64ispatch"} {
		values := captureProofValues(t)
		values["WITSELF_LOCAL_CAPTURE_PROOF_STUB_EMAIL"] = endpoint
		_, err := FromEnv("worker", testLookup(values), nil)
		if err == nil || err.Error() != "capture_proof_stub_invalid" {
			t.Fatal("capture profile nonloopback or ambiguous endpoint accepted")
		}
	}
	for _, endpoint := range []string{"http://127.0.0.1:1234/v1/dispatch", "http://[::1]:1234/v1/dispatch"} {
		if !CaptureProofLoopbackEndpoint(endpoint) {
			t.Fatal("capture profile literal loopback endpoint refused")
		}
	}
}

type captureProofReadinessStore struct {
	markerError error
	core, sync  int
	account     string
	calls       []string
}

func (s *captureProofReadinessStore) Ping(context.Context) error {
	s.calls = append(s.calls, "ping")
	return nil
}
func (s *captureProofReadinessStore) AppliedSchemaVersion(context.Context) (int, error) {
	s.calls = append(s.calls, "core")
	return s.core, nil
}
func (s *captureProofReadinessStore) LocalBootstrapAccount(context.Context) (string, error) {
	s.calls = append(s.calls, "account")
	return s.account, nil
}
func (s *captureProofReadinessStore) AppliedSyncSchemaVersion(context.Context) (int, error) {
	s.calls = append(s.calls, "sync")
	return s.sync, nil
}
func (s *captureProofReadinessStore) VerifySyncCaptureProofDatabase(context.Context, string) error {
	s.calls = append(s.calls, "marker")
	return s.markerError
}

func TestCaptureProofDatabaseIdentification(t *testing.T) {
	for _, reason := range []string{"missing marker", "expired marker", "wrong token", "wrong database", "multiple markers"} {
		t.Run(reason, func(t *testing.T) {
			st := &captureProofReadinessStore{markerError: errors.New("private-marker-detail")}
			cfg := Config{Local: true, SyncSchema: true, CaptureProof: true, CaptureProofToken: "private-token"}
			err := CheckCaptureProofReady(context.Background(), st, cfg, 123, 3)
			if err == nil || err.Error() != "capture_proof_database_unidentified" || !reflect.DeepEqual(st.calls, []string{"marker"}) {
				t.Fatal("capture profile unidentified database reached another startup dependency")
			}
		})
	}
	for _, version := range []int{0, 1, 2, 3, 4} {
		st := &captureProofReadinessStore{core: 123, sync: version, account: "fixture"}
		cfg := Config{Local: true, SyncSchema: true, CaptureProof: true, CaptureProofToken: "private-token", AccountID: "fixture"}
		err := CheckCaptureProofReady(context.Background(), st, cfg, 123, 3)
		if (err == nil) != (version >= 1 && version <= 3) {
			t.Fatal("capture profile schema readiness bound changed")
		}
	}
}
