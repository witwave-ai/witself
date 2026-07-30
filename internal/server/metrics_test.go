package server

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestRuntimeMetricsObserveMemoryLimitRejectionsWithBoundedLabels(t *testing.T) {
	limitErr := func() error {
		maximum, remaining := int64(1000), int64(0)
		return &MemoryLimitError{Status: MemoryLimitStatus{
			Used: 1000, Max: &maximum, Remaining: &remaining,
			NearLimit: true, AtLimit: true,
		}}
	}
	metrics := newRuntimeMetrics()
	cfg := metrics.instrumentConfig(Config{
		CaptureMemory: func(context.Context, DomainPrincipal, CaptureMemoryRequest) (MemoryMutationResult, error) {
			return MemoryMutationResult{}, limitErr()
		},
		SupersedeMemory: func(context.Context, DomainPrincipal, string, SupersedeMemoryRequest) (SupersedeMemoryResult, error) {
			return SupersedeMemoryResult{}, limitErr()
		},
		RestoreMemory: func(context.Context, DomainPrincipal, string, MemoryLifecycleRequest) (MemoryMutationResult, error) {
			return MemoryMutationResult{}, limitErr()
		},
		ReactivateMemory: func(context.Context, DomainPrincipal, string, MemoryLifecycleRequest) (MemoryMutationResult, error) {
			return MemoryMutationResult{}, limitErr()
		},
		ApplyMemoryCuration: func(context.Context, DomainPrincipal, string, ApplyMemoryCurationRequest) (any, error) {
			return nil, limitErr()
		},
	})
	principal := DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_private_identifier"}
	_, _ = cfg.CaptureMemory(context.Background(), principal, CaptureMemoryRequest{Content: "private memory content"})
	_, _ = cfg.SupersedeMemory(context.Background(), principal, "mem_private_identifier", SupersedeMemoryRequest{})
	_, _ = cfg.RestoreMemory(context.Background(), principal, "mem_private_identifier", MemoryLifecycleRequest{})
	_, _ = cfg.ReactivateMemory(context.Background(), principal, "mem_private_identifier", MemoryLifecycleRequest{})
	_, _ = cfg.ApplyMemoryCuration(context.Background(), principal, "mrun_private_identifier", ApplyMemoryCurationRequest{})

	var output bytes.Buffer
	metrics.writePrometheus(&output)
	text := output.String()
	for _, operation := range []string{"create", "supersede", "restore", "reactivate", "curation_apply"} {
		want := `witself_memory_limit_rejections_total{limit_dimension="stored_memory",operation="` + operation + `"} 1`
		if !strings.Contains(text, want) {
			t.Errorf("memory-limit counter missing %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{
		"agent_private_identifier",
		"mem_private_identifier",
		"mrun_private_identifier",
		"private memory content",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("metrics exposed %q:\n%s", forbidden, text)
		}
	}
}

func TestRuntimeMetricsObserveFactLimitRejectionsWithBoundedLabels(t *testing.T) {
	maximum, remaining := int64(1000), int64(0)
	limitErr := func() error {
		return &FactLimitError{Status: FactLimitStatus{
			Used: 1000, Max: &maximum, Remaining: &remaining,
			NearLimit: true, AtLimit: true,
		}}
	}
	if !errors.Is(limitErr(), ErrFactLimitReached) {
		t.Fatal("fact limit error does not unwrap to ErrFactLimitReached")
	}
	metrics := newRuntimeMetrics()
	cfg := metrics.instrumentConfig(Config{
		SetFact: func(context.Context, DomainPrincipal, SetFactRequest) (Fact, error) {
			return Fact{}, limitErr()
		},
		ConfirmFactCandidate: func(context.Context, DomainPrincipal, string, string) (Fact, error) {
			return Fact{}, limitErr()
		},
	})
	principal := DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_private_identifier"}
	_, _ = cfg.SetFact(context.Background(), principal, SetFactRequest{
		Subject: "person_private_subject", Predicate: "identity/private_predicate",
		Value: json.RawMessage(`"private value"`),
	})
	_, _ = cfg.ConfirmFactCandidate(
		context.Background(), principal, "fcand_private_identifier", "private-retry-key",
	)
	if len(metrics.factLimitRejects) != 2 {
		t.Fatalf("fact-limit metric entries = %#v", metrics.factLimitRejects)
	}

	var output bytes.Buffer
	metrics.writePrometheus(&output)
	text := output.String()
	for _, operation := range []string{"create", "confirm"} {
		want := `witself_fact_limit_rejections_total{limit_dimension="stored_fact",operation="` + operation + `"} 1`
		if !strings.Contains(text, want) {
			t.Errorf("fact-limit counter missing %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{
		"agent_private_identifier",
		"person_private_subject",
		"private_predicate",
		"private value",
		"fcand_private_identifier",
		"private-retry-key",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("metrics exposed %q:\n%s", forbidden, text)
		}
	}
}

func TestRuntimeMetricsObservePlanLimitRejectionsWithBoundedLabels(t *testing.T) {
	metrics := newRuntimeMetrics()
	cfg := metrics.instrumentConfig(Config{
		CreateRealm: func(context.Context, string, string) (Realm, error) {
			return Realm{}, &PlanLimitError{
				Dimension: "realms", Used: 1, Max: 1, Plan: "free",
			}
		},
		CreateAgent: func(_ context.Context, _, _, name string) (Agent, error) {
			dimension := "agents_per_realm"
			if name == "legacy_agent_private_name" {
				dimension = "agents"
			}
			return Agent{}, &PlanLimitError{
				Dimension: dimension, Used: 10, Max: 10, Plan: "free",
			}
		},
	})
	_, _ = cfg.CreateRealm(context.Background(), "account_private_identifier", "realm_private_name")
	_, _ = cfg.CreateAgent(
		context.Background(),
		"account_private_identifier",
		"realm_private_identifier",
		"agent_private_name",
	)
	_, _ = cfg.CreateAgent(
		context.Background(),
		"account_private_identifier",
		"realm_private_identifier",
		"legacy_agent_private_name",
	)

	var output bytes.Buffer
	metrics.writePrometheus(&output)
	text := output.String()
	for _, want := range []string{
		`witself_plan_limit_rejections_total{limit_dimension="realms",operation="create"} 1`,
		`witself_plan_limit_rejections_total{limit_dimension="agents",operation="create"} 1`,
		`witself_plan_limit_rejections_total{limit_dimension="agents_per_realm",operation="create"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("plan-limit counter missing %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{
		"account_private_identifier",
		"realm_private_identifier",
		"realm_private_name",
		"agent_private_name",
		"legacy_agent_private_name",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("metrics exposed %q:\n%s", forbidden, text)
		}
	}
}
