package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRuntimeMetricsUseBoundedRouteTemplates(t *testing.T) {
	metrics := newRuntimeMetrics()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/memories/{memory}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/memories/mem_private_identifier", nil)
	response := httptest.NewRecorder()
	metrics.instrument(mux).ServeHTTP(response, req)

	var output bytes.Buffer
	metrics.writePrometheus(&output)
	text := output.String()
	for _, want := range []string{
		`witself_http_requests_total{method="GET",route="/v1/memories/{memory}",status_class="4xx",result="error"} 1`,
		`witself_http_in_flight_requests 0`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "mem_private_identifier") {
		t.Fatalf("metrics exposed a concrete resource id:\n%s", text)
	}
}

func TestRuntimeMetricsObserveDomainMemoryAndCurationOperations(t *testing.T) {
	metrics := newRuntimeMetrics()
	cfg := metrics.instrumentConfig(Config{
		GetMemory: func(context.Context, DomainPrincipal, string) (Memory, error) {
			return Memory{}, nil
		},
		StartMemoryCuration: func(context.Context, DomainPrincipal, StartMemoryCurationRequest) (any, error) {
			return map[string]any{"state": "started"}, nil
		},
	})
	if _, err := cfg.GetMemory(context.Background(), DomainPrincipal{Kind: PrincipalKindAgent}, "mem_not_a_label"); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.StartMemoryCuration(context.Background(), DomainPrincipal{Kind: PrincipalKindAgent}, StartMemoryCurationRequest{}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	metrics.writePrometheus(&output)
	text := output.String()
	for _, want := range []string{
		`witself_memory_operations_total{operation="read",principal_kind="agent",result="success"} 1`,
		`witself_memory_curation_operations_total{operation="start",result="success"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "mem_not_a_label") {
		t.Fatalf("metrics exposed a resource id:\n%s", text)
	}
}

func TestRuntimeMetricsObserveRecallAndVectorFallback(t *testing.T) {
	metrics := newRuntimeMetrics()
	cfg := metrics.instrumentConfig(Config{
		RecallMemories: func(context.Context, DomainPrincipal, MemoryRecallRequest) (MemoryRecallPage, error) {
			return MemoryRecallPage{
				Hits:           []MemoryRecallHit{{}, {}},
				RetrievalMode:  "lexical",
				VectorCoverage: 0,
				Degraded:       true,
				DegradedReason: "no_compatible_vectors",
			}, nil
		},
	})
	_, err := cfg.RecallMemories(context.Background(), DomainPrincipal{Kind: PrincipalKindAgent}, MemoryRecallRequest{
		VectorProfileID: "profile_not_exported_as_a_label",
		QueryVector:     []float64{0.1, 0.2},
	})
	if err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	metrics.writePrometheus(&output)
	text := output.String()
	for _, want := range []string{
		`witself_memory_recalls_total{mode="lexical",principal_kind="agent",result="success"} 1`,
		`witself_memory_recall_hits_bucket{mode="lexical",le="2"} 1`,
		`witself_memory_vector_searches_total{coverage="none",result="success"} 1`,
		`witself_memory_vector_fallbacks_total{reason="no_compatible_vectors"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "profile_not_exported_as_a_label") {
		t.Fatalf("metrics exposed a vector profile id:\n%s", text)
	}
}

func TestRuntimeMetricsDoNotMisclassifyHybridDegradationAsLexicalFallback(t *testing.T) {
	for _, reason := range []string{"candidate_budget_exceeded", "partial_vector_coverage"} {
		t.Run(reason, func(t *testing.T) {
			metrics := newRuntimeMetrics()
			cfg := metrics.instrumentConfig(Config{
				RecallMemories: func(context.Context, DomainPrincipal, MemoryRecallRequest) (MemoryRecallPage, error) {
					return MemoryRecallPage{
						RetrievalMode: "hybrid", VectorCoverage: 1,
						Degraded: true, DegradedReason: reason,
					}, nil
				},
			})
			_, err := cfg.RecallMemories(context.Background(), DomainPrincipal{Kind: PrincipalKindAgent}, MemoryRecallRequest{
				VectorProfileID: "profile", QueryVector: []float64{0.1, 0.2},
			})
			if err != nil {
				t.Fatal(err)
			}

			var output bytes.Buffer
			metrics.writePrometheus(&output)
			if strings.Contains(output.String(), "witself_memory_vector_fallbacks_total{") {
				t.Fatalf("hybrid degradation %q was counted as a lexical fallback:\n%s", reason, output.String())
			}
		})
	}
}

func TestRuntimeMetricsObserveRecallErrorWithoutErrorText(t *testing.T) {
	metrics := newRuntimeMetrics()
	cfg := metrics.instrumentConfig(Config{
		RecallMemories: func(context.Context, DomainPrincipal, MemoryRecallRequest) (MemoryRecallPage, error) {
			return MemoryRecallPage{}, errors.New("database host and private content must not escape")
		},
	})
	_, _ = cfg.RecallMemories(context.Background(), DomainPrincipal{Kind: "unexpected"}, MemoryRecallRequest{
		VectorProfileID: "private-profile", QueryVector: []float64{0.1},
	})

	var output bytes.Buffer
	metrics.writePrometheus(&output)
	text := output.String()
	if !strings.Contains(text, `witself_memory_recalls_total{mode="unknown",principal_kind="unknown",result="error"} 1`) {
		t.Fatalf("error recall counter missing:\n%s", text)
	}
	if !strings.Contains(text, `witself_memory_vector_searches_total{coverage="unknown",result="error"} 1`) {
		t.Fatalf("error vector-search counter missing:\n%s", text)
	}
	for _, forbidden := range []string{"database host", "private content", "private-profile"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("metrics exposed %q:\n%s", forbidden, text)
		}
	}
}

func TestRuntimeMetricsObserveSecretLimitRejectionWithBoundedLabels(t *testing.T) {
	metrics := newRuntimeMetrics()
	maximum, remaining := int64(1), int64(0)
	cfg := metrics.instrumentConfig(Config{
		CreateSecret: func(context.Context, DomainPrincipal, CreateSecretRequest) (SecretMutationResult, error) {
			return SecretMutationResult{}, &SecretLimitError{Status: SecretLimitStatus{
				Used: 1, Max: &maximum, Remaining: &remaining,
			}}
		},
	})
	_, _ = cfg.CreateSecret(context.Background(), DomainPrincipal{
		Kind: PrincipalKindAgent, ID: "agent_private_identifier",
	}, CreateSecretRequest{Name: "secret_private_name"})

	var output bytes.Buffer
	metrics.writePrometheus(&output)
	text := output.String()
	want := `witself_secret_limit_rejections_total{limit_dimension="stored_secret",operation="create"} 1`
	if !strings.Contains(text, want) {
		t.Fatalf("secret-limit counter missing %q:\n%s", want, text)
	}
	for _, forbidden := range []string{"agent_private_identifier", "secret_private_name"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("metrics exposed %q:\n%s", forbidden, text)
		}
	}
}
