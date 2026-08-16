package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemail"
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

func TestAgentEmailCellStorageMetricsAreValueFreeAndBounded(t *testing.T) {
	metrics := newRuntimeMetrics()
	reads := 0
	handler := metricsMuxFor(metrics, func(ctx context.Context) (AgentEmailCellStorageMetrics, error) {
		reads++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > agentEmailCellStorageMetricsTimeout {
			return AgentEmailCellStorageMetrics{}, errors.New("collector deadline missing")
		}
		return AgentEmailCellStorageMetrics{
			RetainedBytes: 1234, RootRows: 12, CountedRows: 34,
			AdmissionBytes: 3221225472, AdmissionRootRows: 25000,
			HardBytes: 4294967296, HardCountedRows: 100000,
		}, nil
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK || reads != 1 {
		t.Fatalf("metrics response=%d reads=%d", response.Code, reads)
	}
	text := response.Body.String()
	for _, want := range []string{
		"witself_agent_email_cell_storage_metrics_up 1",
		"witself_agent_email_cell_storage_retained_bytes 1234",
		"witself_agent_email_cell_storage_admission_bytes 3221225472",
		"witself_agent_email_cell_storage_hard_bytes 4294967296",
		"witself_agent_email_cell_storage_root_rows 12",
		"witself_agent_email_cell_storage_admission_root_rows 25000",
		"witself_agent_email_cell_storage_counted_rows 34",
		"witself_agent_email_cell_storage_hard_counted_rows 100000",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{"account_private", "realm_private", "agent_private"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("cell storage metrics exposed %q:\n%s", forbidden, text)
		}
	}
}

func TestAgentEmailCellStorageMetricsFailClosedWithoutErrorText(t *testing.T) {
	for _, test := range []struct {
		name string
		read func(context.Context) (AgentEmailCellStorageMetrics, error)
	}{
		{
			name: "query failure",
			read: func(context.Context) (AgentEmailCellStorageMetrics, error) {
				return AgentEmailCellStorageMetrics{}, errors.New("database_private_host account_private_identifier")
			},
		},
		{
			name: "invalid projection",
			read: func(context.Context) (AgentEmailCellStorageMetrics, error) {
				return AgentEmailCellStorageMetrics{RetainedBytes: -1}, nil
			},
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			metricsMuxFor(newRuntimeMetrics(), test.read).ServeHTTP(
				response,
				httptest.NewRequest(http.MethodGet, "/metrics", nil),
			)
			text := response.Body.String()
			if !strings.Contains(text, "witself_agent_email_cell_storage_metrics_up 0") {
				t.Fatalf("metrics did not fail closed:\n%s", text)
			}
			if strings.Contains(text, "witself_agent_email_cell_storage_retained_bytes") ||
				strings.Contains(text, "database_private_host") ||
				strings.Contains(text, "account_private_identifier") {
				t.Fatalf("failed collector exposed values or error text:\n%s", text)
			}
		})
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

func TestRuntimeMetricsObserveBoundedAgentEmailIngestOutcomes(t *testing.T) {
	metrics := newRuntimeMetrics()
	outcomes := []error{
		nil,
		errors.Join(
			ErrAgentEmailAttachmentOmitted,
			errors.New("account_private_identifier attachment_private_identifier"),
		),
		errors.Join(
			ErrAgentEmailRawSizeExceeded,
			errors.New("account_private_identifier plan_private_name"),
		),
		ErrAgentEmailFeatureDisabled,
		ErrAgentEmailDatabaseCapacity,
		ErrAgentEmailReceiveDisabled,
		errors.Join(
			&AgentEmailRateLimitError{
				Dimension:  "email_received_bytes",
				Scope:      "sender",
				Source:     "platform",
				RetryAfter: time.Second,
			},
			errors.New("sender_private_identifier mailbox_private_identifier"),
		),
		&AgentEmailRateLimitError{
			Dimension:  "email_received",
			Scope:      "account",
			Source:     "platform",
			RetryAfter: time.Second,
		},
		ErrAgentEmailUnknownRecipient,
		errors.Join(ErrNotFound, errors.New("emsg_private_identifier")),
		ErrAgentEmailRetryCanaryTemporary,
		ErrAgentEmailRetryCanaryPermanent,
		errors.New("database_private_host tenant_private_identifier"),
	}
	nextOutcome := 0
	cfg := metrics.instrumentConfig(Config{
		IngestAgentEmailPilot: func(
			context.Context,
			agentemail.RelayMetadata,
			[]byte,
		) error {
			err := outcomes[nextOutcome]
			nextOutcome++
			return err
		},
	})
	metadata := agentemail.RelayMetadata{
		KeyID:             "key_private_identifier",
		Audience:          "cell_private_identifier",
		EnvelopeSender:    "sender-private@example.test",
		EnvelopeRecipient: "agent-private@example.test",
	}
	for range outcomes {
		_ = cfg.IngestAgentEmailPilot(
			context.Background(),
			metadata,
			[]byte("raw private message content"),
		)
	}

	expected := map[string]uint64{
		"retained":               1,
		"omitted_capacity":       1,
		"over_size":              1,
		"feature_disabled":       1,
		"storage_full":           1,
		"receive_disabled":       1,
		"rate_limited":           2,
		"unknown_recipient":      2,
		"retry_canary_temporary": 1,
		"retry_canary_rejected":  1,
		"error":                  1,
	}
	if len(metrics.agentEmailIngests) != len(expected) {
		t.Fatalf("agent-email metric outcomes = %#v", metrics.agentEmailIngests)
	}
	for outcome, count := range expected {
		if metrics.agentEmailIngests[outcome] != count {
			t.Errorf("agent-email outcome %q = %d, want %d", outcome, metrics.agentEmailIngests[outcome], count)
		}
	}

	var output bytes.Buffer
	metrics.writePrometheus(&output)
	text := output.String()
	metricLines := 0
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, `witself_agent_email_ingests_total{`) {
			continue
		}
		metricLines++
		matched := false
		for outcome, count := range expected {
			want := `witself_agent_email_ingests_total{outcome="` + outcome + `"} ` +
				strconv.FormatUint(count, 10)
			if line == want {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("unexpected agent-email metric series %q", line)
		}
	}
	if metricLines != len(expected) {
		t.Fatalf("agent-email metric series = %d, want %d:\n%s", metricLines, len(expected), text)
	}
	wantRate := `witself_agent_email_rate_limit_rejections_total{limit_dimension="email_received_bytes",scope="sender",source="platform"} 1`
	if !strings.Contains(text, wantRate) {
		t.Fatalf("missing bounded agent-email rate metric %q:\n%s", wantRate, text)
	}
	wantAccountRate := `witself_agent_email_rate_limit_rejections_total{limit_dimension="email_received",scope="account",source="platform"} 1`
	if !strings.Contains(text, wantAccountRate) {
		t.Fatalf("missing bounded account agent-email rate metric %q:\n%s", wantAccountRate, text)
	}
	for _, forbidden := range []string{
		"account_private_identifier",
		"attachment_private_identifier",
		"plan_private_name",
		"emsg_private_identifier",
		"database_private_host",
		"tenant_private_identifier",
		"key_private_identifier",
		"sender_private_identifier",
		"mailbox_private_identifier",
		"cell_private_identifier",
		"sender-private@example.test",
		"agent-private@example.test",
		"raw private message content",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("agent-email metrics exposed %q:\n%s", forbidden, text)
		}
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
		CreateAgent: func(_ context.Context, _, _ string, in CreateAgentRequest) (Agent, error) {
			dimension := "agents_per_realm"
			if in.Name == "legacy_agent_private_name" {
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
		CreateAgentRequest{Name: "agent_private_name"},
	)
	_, _ = cfg.CreateAgent(
		context.Background(),
		"account_private_identifier",
		"realm_private_identifier",
		CreateAgentRequest{Name: "legacy_agent_private_name"},
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

func TestRuntimeMetricsObserveMessageRateLimitRejectionsWithBoundedLabels(t *testing.T) {
	metrics := newRuntimeMetrics()
	rateErr := func(dimension, scope string, retryable bool) error {
		retryAfter := time.Duration(0)
		if retryable {
			retryAfter = time.Second
		}
		return &MessageRateLimitError{
			Dimension: dimension, Scope: scope, Limit: 999, Used: 998, Attempted: 2,
			WindowSeconds: 60, RetryAfter: retryAfter, Source: "private_source_value",
			Retryable: retryable,
		}
	}
	cfg := metrics.instrumentConfig(Config{
		SendMessage: func(context.Context, DomainPrincipal, SendMessageRequest) (Message, error) {
			return Message{}, rateErr("message_sent", "agent", true)
		},
		ReplyMessage: func(context.Context, DomainPrincipal, string, ReplyMessageRequest) (Message, error) {
			return Message{}, rateErr("message_delivered", "realm", false)
		},
		CompleteMessage: func(context.Context, DomainPrincipal, string, CompleteMessageRequest) (CompleteMessageResult, error) {
			return CompleteMessageResult{}, rateErr("message_delivered", "recipient", true)
		},
		CreateMessageRequest: func(context.Context, DomainPrincipal, CreateMessageRequestRequest) (CreateMessageRequestResult, error) {
			return CreateMessageRequestResult{}, rateErr("message_sent", "agent", false)
		},
		OfferMessageRequest: func(context.Context, DomainPrincipal, string, OfferMessageRequestRequest) (OfferMessageRequestResult, error) {
			return OfferMessageRequestResult{}, rateErr("message_delivered", "realm", true)
		},
		CompleteMessageRequest: func(context.Context, DomainPrincipal, string, CompleteMessageRequestRequest) (CompleteMessageRequestResult, error) {
			return CompleteMessageRequestResult{}, rateErr("message_delivered", "recipient", false)
		},
	})

	principal := DomainPrincipal{
		Kind: PrincipalKindAgent, ID: "agent_private_identifier", AccountID: "account_private_identifier",
		RealmID: "realm_private_identifier",
	}
	_, _ = cfg.SendMessage(context.Background(), principal, SendMessageRequest{Body: "private message body"})
	_, _ = cfg.ReplyMessage(context.Background(), principal, "message_private_identifier", ReplyMessageRequest{Body: "private reply body"})
	_, _ = cfg.CompleteMessage(context.Background(), principal, "message_private_identifier", CompleteMessageRequest{Body: "private result body"})
	_, _ = cfg.CreateMessageRequest(context.Background(), principal, CreateMessageRequestRequest{Body: "private request body"})
	_, _ = cfg.OfferMessageRequest(context.Background(), principal, "request_private_identifier", OfferMessageRequestRequest{Body: "private offer body"})
	_, _ = cfg.CompleteMessageRequest(context.Background(), principal, "request_private_identifier", CompleteMessageRequestRequest{Body: "private completion body"})
	metrics.observeMessageRateRejection(&MessageRateLimitError{
		Dimension: "private_dimension", Scope: "private_scope",
	}, "private_operation")

	var output bytes.Buffer
	metrics.writePrometheus(&output)
	text := output.String()
	for _, want := range []string{
		`witself_message_rate_limit_rejections_total{limit_dimension="message_sent",scope="agent",operation="send"} 1`,
		`witself_message_rate_limit_rejections_total{limit_dimension="message_delivered",scope="realm",operation="reply"} 1`,
		`witself_message_rate_limit_rejections_total{limit_dimension="message_delivered",scope="recipient",operation="complete"} 1`,
		`witself_message_rate_limit_rejections_total{limit_dimension="message_sent",scope="agent",operation="request_open"} 1`,
		`witself_message_rate_limit_rejections_total{limit_dimension="message_delivered",scope="realm",operation="request_offer"} 1`,
		`witself_message_rate_limit_rejections_total{limit_dimension="message_delivered",scope="recipient",operation="request_complete"} 1`,
		`witself_message_rate_limit_rejections_total{limit_dimension="unknown",scope="unknown",operation="unknown"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("message-rate counter missing %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{
		"agent_private_identifier", "account_private_identifier", "realm_private_identifier",
		"message_private_identifier", "request_private_identifier", "private message body",
		"private reply body", "private result body", "private request body", "private offer body",
		"private completion body", "private_source_value", "private_dimension", "private_scope",
		"private_operation", "999", "998",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("metrics exposed %q:\n%s", forbidden, text)
		}
	}
}
