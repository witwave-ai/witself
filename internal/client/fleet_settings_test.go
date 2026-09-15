package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fleetSettingsPointer[T any](v T) *T { return &v }

func fleetSettingsRunner() PlacementRunnerConfig {
	return PlacementRunnerConfig{Enabled: true, RestoreArchives: true, RestoreBatch: 4, RestoreAnyRegion: false, Rebalance: true, RebalanceBatch: 1}
}

type fleetSettingsOperation struct {
	name, method, route, key string
	body                     string
	response                 any
	call                     func(context.Context, string) (any, error)
}

func fleetSettingsOperations() []fleetSettingsOperation {
	runner := fleetSettingsRunner()
	return []fleetSettingsOperation{
		{"get-runner", "GET", "/v1/placement-runner", "placement_runner", "", runner, func(ctx context.Context, endpoint string) (any, error) {
			return GetPlacementRunner(ctx, endpoint, "fleet-test-token")
		}},
		{"set-runner", "POST", "/v1/placement-runner", "placement_runner", `{"enabled":true}`, runner, func(ctx context.Context, endpoint string) (any, error) {
			return SetPlacementRunner(ctx, endpoint, "fleet-test-token", PlacementRunnerPatch{Enabled: fleetSettingsPointer(true)})
		}},
		{"run-runner", "POST", "/v1/placement:run", "placement_runner", `{}`, runner, func(ctx context.Context, endpoint string) (any, error) {
			return RunPlacementRunner(ctx, endpoint, "fleet-test-token", PlacementRunnerPatch{})
		}},
		{"get-reaper", "GET", "/v1/reaper", "reaper", "", ReaperConfig{Enabled: true, TTLMinutes: 60}, func(ctx context.Context, endpoint string) (any, error) {
			return GetReaper(ctx, endpoint, "fleet-test-token")
		}},
		{"enable-reaper", "POST", "/v1/reaper", "reaper", `{"enabled":true,"ttl_minutes":60}`, ReaperConfig{Enabled: true, TTLMinutes: 60}, func(ctx context.Context, endpoint string) (any, error) {
			return SetReaper(ctx, endpoint, "fleet-test-token", ReaperConfig{Enabled: true, TTLMinutes: 60})
		}},
		{"disable-reaper", "POST", "/v1/reaper", "reaper", `{"enabled":false}`, ReaperConfig{}, func(ctx context.Context, endpoint string) (any, error) {
			return SetReaper(ctx, endpoint, "fleet-test-token", ReaperConfig{TTLMinutes: 60})
		}},
		{"get-placement", "GET", "/v1/placement", "placement", "", PlacementConfig{Strategy: "weighted"}, func(ctx context.Context, endpoint string) (any, error) {
			return GetPlacement(ctx, endpoint, "fleet-test-token")
		}},
		{"weighted-placement", "POST", "/v1/placement", "placement", `{"strategy":"weighted"}`, PlacementConfig{Strategy: "weighted"}, func(ctx context.Context, endpoint string) (any, error) {
			return SetPlacement(ctx, endpoint, "fleet-test-token", PlacementConfig{Strategy: "weighted"})
		}},
		{"pinned-placement", "POST", "/v1/placement", "placement", `{"strategy":"pinned","pinned_cell":"civo-usw2"}`, PlacementConfig{Strategy: "pinned", PinnedCell: "civo-usw2"}, func(ctx context.Context, endpoint string) (any, error) {
			return SetPlacement(ctx, endpoint, "fleet-test-token", PlacementConfig{Strategy: "pinned", PinnedCell: "civo-usw2"})
		}},
	}
}

func TestFleetSettingsRequestContracts(t *testing.T) {
	ops := fleetSettingsOperations()
	for _, field := range []string{"enabled", "restore_archives", "restore_batch", "restore_any_region", "rebalance", "rebalance_batch"} {
		patch := PlacementRunnerPatch{}
		runner := fleetSettingsRunner()
		var value any
		switch field {
		case "enabled":
			patch.Enabled = fleetSettingsPointer(false)
			runner.Enabled = false
			value = false
		case "restore_archives":
			patch.RestoreArchives = fleetSettingsPointer(false)
			runner.RestoreArchives = false
			value = false
		case "restore_batch":
			patch.RestoreBatch = fleetSettingsPointer(7)
			runner.RestoreBatch = 7
			value = 7
		case "restore_any_region":
			patch.RestoreAnyRegion = fleetSettingsPointer(true)
			runner.RestoreAnyRegion = true
			value = true
		case "rebalance":
			patch.Rebalance = fleetSettingsPointer(false)
			runner.Rebalance = false
			value = false
		case "rebalance_batch":
			patch.RebalanceBatch = fleetSettingsPointer(3)
			runner.RebalanceBatch = 3
			value = 3
		}
		body, _ := json.Marshal(map[string]any{field: value})
		ops = append(ops, fleetSettingsOperation{"partial-" + field, "POST", "/v1/placement-runner", "placement_runner", string(body), runner, func(ctx context.Context, endpoint string) (any, error) {
			return SetPlacementRunner(ctx, endpoint, "fleet-test-token", patch)
		}})
		if field != "enabled" {
			ops = append(ops, fleetSettingsOperation{"run-partial-" + field, "POST", "/v1/placement:run", "placement_runner", string(body), runner, func(ctx context.Context, endpoint string) (any, error) {
				return RunPlacementRunner(ctx, endpoint, "fleet-test-token", patch)
			}})
		}
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != op.method || r.URL.RequestURI() != op.route {
					t.Errorf("request = %s %s, want %s %s", r.Method, r.URL.RequestURI(), op.method, op.route)
				}
				if r.Header.Get("Authorization") != "Bearer fleet-test-token" {
					t.Error("missing fleet authorization")
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if op.body == "" {
					if len(body) != 0 {
						t.Errorf("GET body = %s", body)
					}
				} else {
					if r.Header.Get("Content-Type") != "application/json" {
						t.Error("missing JSON content type")
					}
					var got, want map[string]any
					if err := json.Unmarshal(body, &got); err != nil {
						t.Error(err)
					}
					if err := json.Unmarshal([]byte(op.body), &want); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, want) {
						t.Errorf("body = %s, want %s; only explicit fields may be sent", body, op.body)
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", op.key: op.response})
			}))
			defer server.Close()
			got, err := op.call(context.Background(), server.URL+"/")
			if err != nil {
				t.Fatal(err)
			}
			if run, ok := got.(PlacementRunnerResult); ok {
				got = run.PlacementRunner
			}
			if !reflect.DeepEqual(got, op.response) {
				t.Errorf("result = %#v, want authoritative %#v", got, op.response)
			}
			if calls != 1 {
				t.Errorf("requests = %d, want 1", calls)
			}
		})
	}
}

func TestFleetSettingsRejectInvalidInput(t *testing.T) {
	cases := []struct {
		name, want string
		call       func(string) error
	}{
		{"empty-runner-patch", "at least one", func(endpoint string) error {
			_, err := SetPlacementRunner(context.Background(), endpoint, "fleet-test-token", PlacementRunnerPatch{})
			return err
		}},
		{"runner-restore-low", "restore_batch", func(endpoint string) error {
			_, err := SetPlacementRunner(context.Background(), endpoint, "fleet-test-token", PlacementRunnerPatch{RestoreBatch: fleetSettingsPointer(0)})
			return err
		}},
		{"runner-restore-high", "restore_batch", func(endpoint string) error {
			_, err := RunPlacementRunner(context.Background(), endpoint, "fleet-test-token", PlacementRunnerPatch{RestoreBatch: fleetSettingsPointer(11)})
			return err
		}},
		{"runner-rebalance-low", "rebalance_batch", func(endpoint string) error {
			_, err := RunPlacementRunner(context.Background(), endpoint, "fleet-test-token", PlacementRunnerPatch{RebalanceBatch: fleetSettingsPointer(0)})
			return err
		}},
		{"runner-rebalance-high", "rebalance_batch", func(endpoint string) error {
			_, err := SetPlacementRunner(context.Background(), endpoint, "fleet-test-token", PlacementRunnerPatch{RebalanceBatch: fleetSettingsPointer(6)})
			return err
		}},
		{"reaper-zero-ttl", "ttl_minutes", func(endpoint string) error {
			_, err := SetReaper(context.Background(), endpoint, "fleet-test-token", ReaperConfig{Enabled: true})
			return err
		}},
		{"reaper-negative-ttl", "ttl_minutes", func(endpoint string) error {
			_, err := SetReaper(context.Background(), endpoint, "fleet-test-token", ReaperConfig{Enabled: true, TTLMinutes: -1})
			return err
		}},
		{"placement-unknown", "strategy", func(endpoint string) error {
			_, err := SetPlacement(context.Background(), endpoint, "fleet-test-token", PlacementConfig{Strategy: "geo"})
			return err
		}},
		{"placement-no-pin", "cell name", func(endpoint string) error {
			_, err := SetPlacement(context.Background(), endpoint, "fleet-test-token", PlacementConfig{Strategy: "pinned"})
			return err
		}},
		{"placement-invalid-pin", "cell name", func(endpoint string) error {
			_, err := SetPlacement(context.Background(), endpoint, "fleet-test-token", PlacementConfig{Strategy: "pinned", PinnedCell: "bad/../cell"})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; http.Error(w, "unexpected request", 500) }))
			defer server.Close()
			err := tc.call(server.URL)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if calls != 0 {
				t.Fatalf("invalid input made %d requests", calls)
			}
		})
	}
}

func TestFleetSettingsRejectInvalidSchemaAndMissingConfig(t *testing.T) {
	for _, op := range fleetSettingsOperations() {
		t.Run(op.name, func(t *testing.T) {
			for _, variant := range []string{"schema", "missing", "null"} {
				t.Run(variant, func(t *testing.T) {
					calls := 0
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						calls++
						out := map[string]any{"schema_version": "witself.v0", op.key: op.response}
						switch variant {
						case "schema":
							out["schema_version"] = "witself.v1"
						case "missing":
							delete(out, op.key)
						case "null":
							out[op.key] = nil
						}
						_ = json.NewEncoder(w).Encode(out)
					}))
					defer server.Close()
					_, err := op.call(context.Background(), server.URL)
					if err == nil || !strings.Contains(err.Error(), "control plane") {
						t.Fatalf("error = %v, want invalid control-plane response", err)
					}
					if calls != 1 {
						t.Fatalf("requests = %d, want 1", calls)
					}
				})
			}
		})
	}
}

func TestFleetSettingsRejectUnacknowledgedWrites(t *testing.T) {
	for _, op := range fleetSettingsOperations() {
		if op.method != "POST" {
			continue
		}
		for _, variant := range []string{"mismatch", "missing"} {
			t.Run(op.name+"/"+variant, func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					raw, _ := json.Marshal(op.response)
					var cfg map[string]any
					_ = json.Unmarshal(raw, &cfg)
					field := "enabled"
					if op.key == "placement" {
						field = "strategy"
					}
					if variant == "missing" {
						delete(cfg, field)
					} else if field == "strategy" {
						cfg[field] = "unknown"
					} else {
						cfg[field] = !cfg[field].(bool)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", op.key: cfg})
				}))
				defer server.Close()
				_, err := op.call(context.Background(), server.URL)
				if err == nil || !strings.Contains(err.Error(), "did not acknowledge") {
					t.Fatalf("error = %v, want did not acknowledge", err)
				}
				if calls != 1 {
					t.Fatalf("requests = %d, want 1", calls)
				}
			})
		}
	}
	// Every explicit patch field must be present and equal, including false values.
	patch := PlacementRunnerPatch{Enabled: fleetSettingsPointer(false), RestoreArchives: fleetSettingsPointer(false), RestoreBatch: fleetSettingsPointer(7), RestoreAnyRegion: fleetSettingsPointer(false), Rebalance: fleetSettingsPointer(false), RebalanceBatch: fleetSettingsPointer(3)}
	for _, field := range []string{"enabled", "restore_archives", "restore_batch", "restore_any_region", "rebalance", "rebalance_batch"} {
		for _, variant := range []string{"mismatch", "missing", "null"} {
			t.Run(field+"/"+variant, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					raw, _ := json.Marshal(patch)
					var cfg map[string]any
					_ = json.Unmarshal(raw, &cfg)
					switch variant {
					case "missing":
						delete(cfg, field)
					case "null":
						cfg[field] = nil
					case "mismatch":
						if v, ok := cfg[field].(bool); ok {
							cfg[field] = !v
						} else {
							cfg[field] = 1
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "placement_runner": cfg})
				}))
				defer server.Close()
				_, err := SetPlacementRunner(context.Background(), server.URL, "fleet-test-token", patch)
				if err == nil || !strings.Contains(err.Error(), "did not acknowledge") {
					t.Fatalf("error = %v, want did not acknowledge", err)
				}
			})
		}
	}
	for _, tc := range []struct {
		name, key, field string
		cfg              any
		call             func(context.Context, string) (any, error)
	}{
		{"reaper-ttl", "reaper", "ttl_minutes", ReaperConfig{Enabled: true, TTLMinutes: 60}, fleetSettingsOperations()[4].call},
		{"placement-pin", "placement", "pinned_cell", PlacementConfig{Strategy: "pinned", PinnedCell: "civo-usw2"}, fleetSettingsOperations()[8].call},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				raw, _ := json.Marshal(tc.cfg)
				var cfg map[string]any
				_ = json.Unmarshal(raw, &cfg)
				delete(cfg, tc.field)
				_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", tc.key: cfg})
			}))
			defer server.Close()
			_, err := tc.call(context.Background(), server.URL)
			if err == nil || !strings.Contains(err.Error(), "did not acknowledge") {
				t.Fatalf("error = %v, want did not acknowledge", err)
			}
		})
	}
}

func TestFleetSettingsUnauthorized(t *testing.T) {
	for _, op := range fleetSettingsOperations() {
		t.Run(op.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"fleet token refused"}`)
			}))
			defer server.Close()
			_, err := op.call(context.Background(), server.URL)
			if !errors.Is(err, ErrUnauthorized) || !strings.Contains(err.Error(), "fleet token refused") {
				t.Fatalf("error = %v, want ErrUnauthorized with server refusal", err)
			}
			if calls != 1 {
				t.Fatalf("requests = %d, want 1; unauthorized requests must not fall back", calls)
			}
		})
	}
}

func TestFleetSettingsEndpointsCannotRedirectRoutes(t *testing.T) {
	for _, op := range fleetSettingsOperations() {
		t.Run(op.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; http.Error(w, "unexpected request", 500) }))
			defer server.Close()
			for _, suffix := range []string{"/v1/placement:purge", "/v1/placement:purge#", "/v1/placement:restore?", "?", "#", "?route=/v1/placement:rebalance", "/%2e%2e", "//"} {
				_, err := op.call(context.Background(), server.URL+suffix)
				if err == nil || !strings.Contains(err.Error(), "origin") {
					t.Errorf("endpoint suffix %q: error = %v, want origin validation", suffix, err)
				}
			}
			if calls != 0 {
				t.Fatalf("unsafe endpoints made %d requests", calls)
			}
		})
	}
}

func TestFleetSettingsRunPreservesStepResults(t *testing.T) {
	for _, step := range []string{"restore", "rebalance"} {
		t.Run(step, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.RequestURI() != "/v1/placement:run" {
					t.Errorf("unsafe runner route %s %s", r.Method, r.URL.RequestURI())
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "placement_runner": fleetSettingsRunner(), "restore": json.RawMessage(`{"restored":[],"remaining":2}`), "rebalance": json.RawMessage(`{"rebalanced":[],"skipped":1}`), step + "_error": PlacementRunnerStepError{Status: 503, Body: json.RawMessage(`{"error":"cell unavailable"}`)}})
			}))
			defer server.Close()
			got, err := RunPlacementRunner(context.Background(), server.URL, "fleet-test-token", PlacementRunnerPatch{})
			if err != nil {
				t.Fatal(err)
			}
			if string(got.Restore) != `{"restored":[],"remaining":2}` || string(got.Rebalance) != `{"rebalanced":[],"skipped":1}` {
				t.Fatalf("lost step results: %#v", got)
			}
			failure, other := got.RestoreError, got.RebalanceError
			if step == "rebalance" {
				failure, other = other, failure
			}
			if failure == nil || failure.Status != 503 || string(failure.Body) != `{"error":"cell unavailable"}` || other != nil {
				t.Fatalf("lost step error: %#v", got)
			}
		})
	}
}

type fleetSettingsDeadlineTransport struct {
	base  http.RoundTripper
	t     *testing.T
	calls int
}

func (tr *fleetSettingsDeadlineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.calls++
	deadline, ok := req.Context().Deadline()
	if !ok {
		tr.t.Error("manual run has no timeout")
	} else if remaining := time.Until(deadline); remaining < 9*time.Minute || remaining > 10*time.Minute {
		tr.t.Errorf("manual run timeout = %s, want 10 minutes", remaining)
	}
	return tr.base.RoundTrip(req)
}

func TestFleetSettingsRunUsesTenMinuteTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "placement_runner": fleetSettingsRunner()})
	}))
	defer server.Close()
	original := http.DefaultTransport
	transport := &fleetSettingsDeadlineTransport{base: original, t: t}
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = original }()
	if _, err := RunPlacementRunner(context.Background(), server.URL, "fleet-test-token", PlacementRunnerPatch{}); err != nil {
		t.Fatal(err)
	}
	if transport.calls != 1 {
		t.Fatalf("requests = %d, want 1", transport.calls)
	}
}

func TestFleetSettingsReaperFractionalRead(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      float64
	}{
		{"whole", `1`, 1},
		{"fractional", `1.5`, 1.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != "/v1/reaper" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				_, _ = io.WriteString(w, `{"schema_version":"witself.v0","reaper":{"enabled":true,"ttl_minutes":`+tc.raw+`}}`)
			}))
			defer server.Close()
			got, err := GetReaper(context.Background(), server.URL, "fleet-test-token")
			if err != nil {
				t.Fatalf("valid reaper TTL could not be read: %v", err)
			}
			if !got.Enabled || float64(got.TTLMinutes) != tc.want || calls != 1 {
				t.Fatalf("reaper = %#v, requests = %d; want enabled TTL %v and one request", got, calls, tc.want)
			}
		})
	}
}

func TestFleetSettingsReaperFractionalAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		wantError   bool
	}{
		{"matching", `,"ttl_minutes":1.5`, false},
		{"fractional-mismatch", `,"ttl_minutes":1.75`, true},
		{"rounded", `,"ttl_minutes":1`, true},
		{"missing", ``, true},
		{"null", `,"ttl_minutes":null`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/v1/reaper" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if !reflect.DeepEqual(body, map[string]any{"enabled": true, "ttl_minutes": 1.5}) {
					t.Errorf("request changed fractional TTL: %#v", body)
				}
				_, _ = io.WriteString(w, `{"schema_version":"witself.v0","reaper":{"enabled":true`+tc.field+`}}`)
			}))
			defer server.Close()
			want := ReaperConfig{Enabled: true, TTLMinutes: 1.5}
			got, err := SetReaper(context.Background(), server.URL, "fleet-test-token", want)
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "did not acknowledge ttl_minutes") {
					t.Fatalf("error = %v, want fractional acknowledgement refusal", err)
				}
			} else if err != nil || got != want {
				t.Fatalf("reaper = %#v, error = %v; want %#v", got, err, want)
			}
			if calls != 1 {
				t.Fatalf("requests = %d, want 1", calls)
			}
		})
	}
}

func TestFleetSettingsReaperTTLValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		ttl  float64
	}{
		{"zero", 0}, {"negative", -1}, {"below-minimum", 0.5},
		{"nan", math.NaN()}, {"positive-infinity", math.Inf(1)}, {"negative-infinity", math.Inf(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				http.Error(w, "unexpected request", http.StatusInternalServerError)
			}))
			defer server.Close()
			_, err := SetReaper(context.Background(), server.URL, "fleet-test-token", ReaperConfig{Enabled: true, TTLMinutes: tc.ttl})
			if err == nil || !strings.Contains(err.Error(), "ttl_minutes") || calls != 0 {
				t.Fatalf("invalid TTL: error = %v, requests = %d; want refusal before request", err, calls)
			}
		})
	}
	for _, raw := range []string{`0`, `-1`, `0.5`, `null`, `1e999`} {
		t.Run("read/"+raw, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"schema_version":"witself.v0","reaper":{"enabled":true,"ttl_minutes":`+raw+`}}`)
			}))
			defer server.Close()
			if got, err := GetReaper(context.Background(), server.URL, "fleet-test-token"); err == nil {
				t.Fatalf("invalid reaper TTL accepted: %#v", got)
			}
		})
	}
	t.Run("disable-omits-ttl", func(t *testing.T) {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode disable: %v", err)
			}
			if !reflect.DeepEqual(body, map[string]any{"enabled": false}) {
				t.Errorf("disable request included TTL: %#v", body)
			}
			_, _ = io.WriteString(w, `{"schema_version":"witself.v0","reaper":{"enabled":false}}`)
		}))
		defer server.Close()
		for _, ttl := range []float64{1.5, math.NaN(), math.Inf(1), math.Inf(-1)} {
			got, err := SetReaper(context.Background(), server.URL, "fleet-test-token", ReaperConfig{TTLMinutes: ttl})
			if err != nil || got != (ReaperConfig{}) {
				t.Fatalf("disable = %#v, %v; want disabled with no TTL", got, err)
			}
		}
		if calls != 4 {
			t.Fatalf("requests = %d, want 4", calls)
		}
	})
}
