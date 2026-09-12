package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func zeroVaultLifecycleSamples() map[string]string {
	want := map[string]string{}
	for flow, operations := range map[string][]string{
		"registration": {"register"},
		"enrollment":   {"create", "approve", "receive", "consume", "cancel"},
		"rotation":     {"start", "stage", "commit", "cancel"},
	} {
		for _, operation := range operations {
			for _, result := range []string{"success", "conflict", "forbidden", "not_found", "invalid", "error"} {
				want[fmt.Sprintf(`witself_vault_lifecycle_operations_total{flow=%q,operation=%q,result=%q}`, flow, operation, result)] = "0"
			}
		}
	}
	return want
}

func TestSealedPlaneCountersPresentBeforeFirstObservation(t *testing.T) {
	response := httptest.NewRecorder()
	metricsMuxFor(newRuntimeMetrics(), nil, nil, nil, nil, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("metrics status=%d", response.Code)
	}
	want := map[string]string{}
	for _, kind := range []string{"password", "api_key", "token", "totp", "other"} {
		want[fmt.Sprintf(`witself_secret_material_deliveries_total{field_kind=%q,result="success"}`, kind)] = "0"
	}
	for _, result := range []string{"conflict", "forbidden", "not_found", "invalid", "error"} {
		want[fmt.Sprintf(`witself_secret_material_deliveries_total{field_kind="unknown",result=%q}`, result)] = "0"
	}
	assertMetricSamples(t, response.Body.String(), "witself_secret_material_deliveries_total", want)
	assertMetricSamples(t, response.Body.String(), "witself_vault_lifecycle_operations_total", zeroVaultLifecycleSamples())
}

func TestVaultRotationFirstConflictBurstAlerts(t *testing.T) {
	metrics := newRuntimeMetrics()
	rotation := VaultKeyRotation{ID: testRotationID, LifecycleState: "open", RowVersion: 2}
	calls := 0
	api := metrics.instrument(apiMux(metrics.instrumentConfig(Config{
		AuthenticatePrincipal: secretTestAuth,
		CancelVaultKeyRotation: func(_ context.Context, p DomainPrincipal, id string, in CancelVaultKeyRotationRequest) (VaultKeyRotationMutationResult, error) {
			assertSecretTestPrincipal(t, p)
			calls++
			if id != rotation.ID || in.ExpectedRotationRowVersion != 1 || in.IdempotencyKey != fmt.Sprintf("first-conflict-%d", calls) {
				t.Fatalf("unexpected cancel: id=%q input=%+v", id, in)
			}
			if in.ExpectedRotationRowVersion != rotation.RowVersion {
				return VaultKeyRotationMutationResult{}, ErrConflict
			}
			t.Fatal("expected a stale rotation fence")
			return VaultKeyRotationMutationResult{}, nil
		},
	})))
	const series = `witself_vault_lifecycle_operations_total{flow="rotation",operation="cancel",result="conflict"}`
	scrape := func() string {
		t.Helper()
		response := httptest.NewRecorder()
		metricsMuxFor(metrics, nil, nil, nil, nil, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("metrics status=%d", response.Code)
		}
		for line := range strings.SplitSeq(response.Body.String(), "\n") {
			if value, ok := strings.CutPrefix(line, series+" "); ok {
				return value
			}
		}
		return "_" // Promtool's missing-sample marker preserves an absent baseline.
	}
	// All six requests arrive before Prometheus observes this process.
	for i := 1; i <= 6; i++ {
		request := httptest.NewRequest(http.MethodPost, "/v1/vault/rotations/"+rotation.ID+":cancel", strings.NewReader(`{"expected_rotation_row_version":1}`))
		request.Header.Set("Authorization", "Bearer agent-token")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", fmt.Sprintf("first-conflict-%d", i))
		response := httptest.NewRecorder()
		api.ServeHTTP(response, request)
		if response.Code != http.StatusConflict {
			t.Fatalf("cancel %d status=%d body=%s", i, response.Code, response.Body.String())
		}
	}
	after := scrape()
	if calls != 6 || after != "6" || scrape() != after {
		t.Fatalf("conflict calls=%d sample=%q, want six followed by an unchanged scrape", calls, after)
	}

	t.Run("Prometheus alert", func(t *testing.T) {
		promtool, err := exec.LookPath("promtool")
		if err != nil {
			t.Skip("promtool unavailable; HTTP scrape and counter assertions ran")
		}
		rules, err := filepath.Abs("../../.gitops/charts/platform/files/founder-open-plane.rules.yaml")
		if err != nil {
			t.Fatal(err)
		}
		// Use the actual first HTTP scrape value, with no observed zero baseline,
		// to evaluate the production rule through its pending/firing/resolved states.
		fixture := fmt.Sprintf(`rule_files: [%q]
evaluation_interval: 1m
tests:
  - interval: 1m
    input_series:
      - series: '%s'
        values: '%s+0x20'
    alert_rule_test:
      - eval_time: 0m
        alertname: WitselfVaultRotationFenceConflicts
        exp_alerts: []
      - eval_time: 4m
        alertname: WitselfVaultRotationFenceConflicts
        exp_alerts: []
      - eval_time: 6m
        alertname: WitselfVaultRotationFenceConflicts
        exp_alerts:
          - exp_labels:
              severity: warning
              service: sealed-plane
              witself_alert: "true"
            exp_annotations:
              summary: Vault key rotation conflicts exceed the provisional fence-conflict budget.
              runbook: docs/runbooks.md#sealed-plane-alerts
      - eval_time: 15m
        alertname: WitselfVaultRotationFenceConflicts
        exp_alerts: []
`, rules, series, after)
		path := filepath.Join(t.TempDir(), "first-conflict-burst.test.yaml")
		if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
			t.Fatal(err)
		}
		if output, err := exec.Command(promtool, "test", "rules", path).CombinedOutput(); err != nil {
			t.Fatalf("first conflict burst alert: %v\n%s", err, output)
		}
	})
}
