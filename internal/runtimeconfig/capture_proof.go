package runtimeconfig

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

const captureProofPrefix = "WITSELF_LOCAL_CAPTURE_PROOF"

// CaptureProofStore is deliberately read-only. The profile never installs its
// own identification marker or advances a migration on a runtime connection.
type CaptureProofStore interface {
	BootstrapStore
	SyncSchemaStore
	VerifySyncCaptureProofDatabase(context.Context, string) error
}

func captureProofFromEnv(role, runtime string, lookup func(string) (string, bool), environ []string) (Config, error) {
	selected, present := lookup(captureProofPrefix)
	sync, _ := lookup("WITSELF_LOCAL_SYNC_SCHEMA")
	valid := present && selected == "1" && runtime == "local" && sync == "1"
	known := map[string]bool{captureProofPrefix: true, captureProofPrefix + "_TOKEN": true, captureProofPrefix + "_STUB_EMAIL": true}
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(name), captureProofPrefix) && (!valid || !known[name]) {
			return Config{}, errors.New("capture_proof_requires_local_sync")
		}
	}
	for name := range known {
		if _, exists := lookup(name); exists && !valid {
			return Config{}, errors.New("capture_proof_requires_local_sync")
		}
	}
	if !present {
		return Config{}, nil
	}
	if !valid {
		return Config{}, errors.New("capture_proof_requires_local_sync")
	}
	token, _ := lookup(captureProofPrefix + "_TOKEN")
	if token == "" {
		return Config{}, errors.New("capture_proof_database_unidentified")
	}
	endpoint, endpointPresent := lookup(captureProofPrefix + "_STUB_EMAIL")
	if (role == "worker" || endpointPresent) && !CaptureProofLoopbackEndpoint(endpoint) {
		return Config{}, errors.New("capture_proof_stub_invalid")
	}
	return Config{CaptureProof: true, CaptureProofToken: token, CaptureProofStubEmail: endpoint}, nil
}

// CaptureProofLoopbackEndpoint accepts only the exact HTTP dispatch path at a
// literal loopback address. DNS, credentials, fragments and queries are absent.
func CaptureProofLoopbackEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && port > 0 && u.Scheme == "http" && loopbackAddress(u.Host) && !strings.Contains(endpoint, "#") &&
		u.User == nil && u.Opaque == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" &&
		u.Path == "/v1/dispatch" && u.RawPath == ""
}

// CheckCaptureProofReady permits the reviewed pre-guard schema while its owner
// installs guards behind already-running processes. It never signals capture
// acceptance: core readiness and the disposable marker remain mandatory.
func CheckCaptureProofReady(ctx context.Context, st CaptureProofStore, cfg Config, coreVersion, syncVersion int) error {
	if !cfg.Local || !cfg.SyncSchema || !cfg.CaptureProof || st == nil || cfg.CaptureProofToken == "" {
		return errors.New("capture_proof_database_unidentified")
	}
	if err := st.VerifySyncCaptureProofDatabase(ctx, cfg.CaptureProofToken); err != nil {
		return errors.New("capture_proof_database_unidentified")
	}
	if err := CheckReady(ctx, st, coreVersion, cfg.AccountID); err != nil {
		return errors.New("capture_proof_database_unidentified")
	}
	version, err := st.AppliedSyncSchemaVersion(ctx)
	if err != nil || version < 1 || version > syncVersion {
		return errors.New("capture_proof_database_unidentified")
	}
	return nil
}
