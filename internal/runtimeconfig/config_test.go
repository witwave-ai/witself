package runtimeconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func testLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) { value, ok := values[key]; return value, ok }
}

func privateDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("LC-S2-PW1: Windows private-root ownership/DACL semantics are pending")
	}
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func localValues(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{"WITSELF_RUNTIME": "local", "WITSELF_DATABASE_URL": "postgres://synthetic.invalid/local", "WITSELF_LOCAL_SPOOL_DIR": privateDir(t)}
}

func TestRuntimeSelector(t *testing.T) {
	for _, role := range []string{"server", "worker"} {
		for _, selector := range []string{"unset", "", "local", "hosted-secret", " local", "LOCAL"} {
			t.Run(role+"/"+selector, func(t *testing.T) {
				// Only local selection needs a validated private spool. Hosted and
				// invalid selectors must remain covered on every platform.
				values := map[string]string{
					"WITSELF_RUNTIME":         "local",
					"WITSELF_DATABASE_URL":    "postgres://synthetic.invalid/local",
					"WITSELF_LOCAL_SPOOL_DIR": "unused-spool",
				}
				if selector == "local" {
					values = localValues(t)
				}
				if selector == "unset" {
					delete(values, "WITSELF_RUNTIME")
				} else {
					values["WITSELF_RUNTIME"] = selector
				}
				cfg, err := FromEnv(role, testLookup(values), nil)
				switch selector {
				case "unset", "":
					if err != nil || !reflect.DeepEqual(cfg, Config{}) {
						t.Fatalf("hosted config changed: %+v %v", cfg, err)
					}
				case "local":
					if err != nil || !cfg.Local {
						t.Fatalf("local not selected: %v", err)
					}
				default:
					if err == nil || !strings.Contains(err.Error(), "WITSELF_RUNTIME") || strings.Contains(err.Error(), selector) {
						t.Fatalf("unsafe selector diagnostic: %v", err)
					}
				}
			})
		}
	}
}

func TestLocalListenerIsolation(t *testing.T) {
	for _, role := range []string{"server", "worker"} {
		for _, name := range []string{"WITSELF_API_ADDR", "WITSELF_HEALTH_ADDR", "WITSELF_METRICS_ADDR"} {
			if role == "worker" && name == "WITSELF_API_ADDR" {
				continue
			}
			for _, address := range []string{":8080", "0.0.0.0:8080", "[::]:8080", "192.0.2.4:8080", "example.invalid:8080", "localhost:8080", "127.0.0.1:65536", "127.0.0.1:http", "127.0.0.1:0", "127.2.3.4:1234", "[::1]:8080", "[::ffff:127.0.0.1]:8080"} {
				t.Run(role+"/"+name+"/"+address, func(t *testing.T) {
					values := localValues(t)
					values[name] = address
					_, err := FromEnv(role, testLookup(values), nil)
					allowed := address == "127.0.0.1:0" || address == "127.2.3.4:1234" || address == "[::1]:8080" || address == "[::ffff:127.0.0.1]:8080"
					if allowed && err != nil {
						t.Fatalf("loopback refused: %v", err)
					}
					if !allowed && (err == nil || !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), address)) {
						t.Fatalf("unsafe listener accepted or disclosed: %v", err)
					}
				})
			}
		}
	}
	values := localValues(t)
	server, err := FromEnv("server", testLookup(values), nil)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := FromEnv("worker", testLookup(values), nil)
	if err != nil {
		t.Fatal(err)
	}
	if server.APIAddr != "127.0.0.1:8080" || server.HealthAddr != "127.0.0.1:8081" || server.MetricsAddr != "127.0.0.1:9090" || worker.HealthAddr != "127.0.0.1:8082" || worker.MetricsAddr != "127.0.0.1:9091" {
		t.Fatal("local listener defaults changed")
	}
}

func TestLocalDangerousEnvironment(t *testing.T) {
	// Deliberately independent of the production inventory: dropping a row must fail.
	for _, name := range []string{
		"WITSELF_PROVISION_TOKEN", "WITSELF_BACKUP_TOKEN", "WITSELF_BACKUP_VALIDATION_ENABLED", "WITSELF_BILLING_ENDPOINT",
		"WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED", "WITSELF_AGENT_EMAIL_RECEIVE_PRODUCTION_ENABLED",
		"WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_ENDPOINT", "WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_AUDIENCE", "WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_KEY_ID", "WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_PRIVATE_KEY", "WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_FUTURE_SETTING",
		"WITSELF_AGENT_EMAIL_PROVIDER_EVENT_TOKEN",
		"witself_provision_token", "Witself_Agent_Email_Outbound_Dispatch_X",
	} {
		for _, role := range []string{"server", "worker"} {
			for _, value := range []string{"", "false", "private-fixture-marker"} {
				t.Run(role+"/"+name+"/"+value, func(t *testing.T) {
					values := localValues(t)
					if _, err := FromEnv(role, testLookup(values), nil); err != nil {
						t.Fatal(err)
					}
					values[name] = value
					_, err := FromEnv(role, testLookup(values), []string{name + "=" + value})
					if err == nil || !strings.Contains(err.Error(), name) || (value != "" && strings.Contains(err.Error(), value)) {
						t.Fatalf("forbidden variable accepted or value disclosed: %v", err)
					}
					delete(values, "WITSELF_RUNTIME")
					if cfg, err := FromEnv(role, testLookup(values), []string{name + "=" + value}); err != nil || !reflect.DeepEqual(cfg, Config{}) {
						t.Fatal("hosted configuration changed")
					}
				})
			}
		}
	}
}

func TestLocalRequiredConfiguration(t *testing.T) {
	for _, name := range []string{"WITSELF_DATABASE_URL", "WITSELF_LOCAL_SPOOL_DIR", "WITSELF_LOCAL_ACCOUNT_ID"} {
		for _, value := range []string{"", "  "} {
			t.Run(name+"/"+value, func(t *testing.T) {
				values := localValues(t)
				values[name] = value
				values["DATABASE_URL"] = "postgres://ambient.invalid/database"
				_, err := FromEnv("server", testLookup(values), nil)
				if err == nil || !strings.Contains(err.Error(), name) {
					t.Fatalf("missing refusal: %v", err)
				}
			})
		}
	}
	values := localValues(t)
	values["WITSELF_LOCAL_ACCOUNT_ID"] = " account-fixture "
	cfg, err := FromEnv("server", testLookup(values), nil)
	if err != nil || cfg.AccountID != " account-fixture " {
		t.Fatal("account identity must not be normalized")
	}
}

func TestLocalJobEligibility(t *testing.T) {
	allowed := []string{"avatar_style_rollout", "message_rate_bucket_cleanup", "agent_email_rate_bucket_cleanup", "transcript_storage_reconcile"}
	for _, raw := range []string{"", " ", strings.Join(allowed, ","), " avatar_style_rollout , avatar_style_rollout "} {
		jobs, err := ParseJobs(raw)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(raw) == "" && len(jobs) != 0 {
			t.Fatal("empty allowlist selected jobs")
		}
		if raw == strings.Join(allowed, ",") && len(jobs) != len(allowed) {
			t.Fatal("allowed jobs missing")
		}
	}
	for _, name := range []string{"agent_email_outbound", "account_purge", "transcript_retention", "message_retention", "agent_email_retention", "support_ticket_age_out", "unknown-private-job", ",", "avatar_style_rollout,"} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseJobs(name)
			if err == nil || !strings.Contains(err.Error(), "WITSELF_LOCAL_JOBS") || strings.Contains(err.Error(), name) {
				t.Fatalf("job accepted or disclosed: %v", err)
			}
		})
	}
}

func TestLocalSpoolDirectory(t *testing.T) {
	private := privateDir(t)
	file := filepath.Join(private, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(private, "temp-alias")
	if err := os.Symlink(os.TempDir(), alias); err != nil {
		t.Logf("symlink case unavailable: %v", err)
	} else {
		if ValidateSpoolDir(alias) == nil {
			t.Fatal("temporary directory alias accepted")
		}
	}
	for _, dir := range []string{"", os.TempDir(), file, filepath.Join(private, "missing")} {
		if err := ValidateSpoolDir(dir); err == nil || !strings.Contains(err.Error(), "WITSELF_LOCAL_SPOOL_DIR") {
			t.Fatal("unsafe spool accepted")
		}
	}
	if err := ValidateSpoolDir(private); err != nil {
		t.Fatal(err)
	}
}
