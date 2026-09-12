package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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

func TestSelfDigestMetricsAreValueFreeAndBounded(t *testing.T) {
	metrics := newRuntimeMetrics()
	handler := apiMux(metrics.instrumentConfig(Config{
		AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) {
			return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_private", AccountID: "account_private", RealmID: "realm_private", AccountStatus: "active"}, true, nil
		},
	}))
	for _, header := range []string{"session", "prompt", "", "session_hook", "SESSION", " session", "token_private", strings.Repeat("private_header", 1024)} {
		request := httptest.NewRequest(http.MethodGet, "/v1/self", nil)
		request.Header.Set("Authorization", "Bearer token_private")
		if header != "" {
			request.Header.Set("X-Witself-Hydration", header)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("self status = %d", response.Code)
		}
	}
	var output bytes.Buffer
	metrics.writePrometheus(&output)
	text := output.String()
	for surface, count := range map[string]int{"session_hook": 1, "prompt_hook": 1, "other": 6} {
		for _, want := range []string{
			fmt.Sprintf(`witself_self_digest_reads_total{surface="%s",elided="false",result="success"} %d`, surface, count),
			fmt.Sprintf(`witself_self_digest_read_duration_seconds_count{surface="%s"} %d`, surface, count),
			fmt.Sprintf(`witself_self_digest_elided_entries_count{surface="%s"} %d`, surface, count),
			fmt.Sprintf(`witself_self_digest_elided_entries_sum{surface="%s"} 0`, surface),
		} {
			if !strings.Contains(text, want+"\n") {
				t.Errorf("metrics missing %q", want)
			}
		}
		prefix := fmt.Sprintf(`witself_self_digest_read_duration_seconds_sum{surface="%s"} `, surface)
		if _, rest, ok := strings.Cut(text, prefix); ok {
			value, _, _ := strings.Cut(rest, "\n")
			elapsed, err := strconv.ParseFloat(value, 64)
			if err != nil || elapsed <= 0 {
				t.Errorf("invalid observed latency %q", value)
			}
		} else {
			t.Errorf("missing latency for %s", surface)
		}
	}
	for _, forbidden := range []string{"token_private", "account_private", "agent_private", "realm_private", "private_header", `bytes=`, `account=`, `agent_id=`} {
		if strings.Contains(text, forbidden) {
			t.Errorf("metrics exposed %q", forbidden)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "witself_self_digest_") {
			continue
		}
		_, suffix, ok := strings.Cut(line, "{")
		if !ok {
			t.Fatalf("missing labels: %s", line)
		}
		labelText, _, _ := strings.Cut(suffix, "}")
		for _, label := range strings.Split(labelText, ",") {
			key, value, _ := strings.Cut(label, "=")
			valid := false
			switch key {
			case "surface":
				valid = value == `"session_hook"` || value == `"prompt_hook"` || value == `"other"`
			case "elided":
				valid = value == `"true"` || value == `"false"`
			case "result":
				valid = value == `"success"` || value == `"error"`
			case "le":
				valid = strings.Contains(line, "_bucket{")
			}
			if !valid {
				t.Errorf("unexpected metric label %s", label)
			}
		}
	}
}

func TestSelfDigestLatencyHistogramHasAlertBoundary(t *testing.T) {
	metrics := newRuntimeMetrics()
	for _, elapsed := range []time.Duration{1100 * time.Millisecond, 1400 * time.Millisecond, 1500 * time.Millisecond, 1600 * time.Millisecond} {
		metrics.observeSelfDigest("prompt", false, 0, nil, elapsed)
		metrics.observeHTTP(http.MethodGet, "GET /v1/self", http.StatusOK, elapsed)
	}
	var output bytes.Buffer
	metrics.writePrometheus(&output)
	text := output.String()
	for _, want := range []string{
		`witself_self_digest_read_duration_seconds_bucket{surface="prompt_hook",le="1"} 0`,
		`witself_self_digest_read_duration_seconds_bucket{surface="prompt_hook",le="1.5"} 3`,
		`witself_self_digest_read_duration_seconds_bucket{surface="prompt_hook",le="2.5"} 4`,
		`witself_self_digest_read_duration_seconds_count{surface="prompt_hook"} 4`,
		`witself_http_request_duration_seconds_bucket{method="GET",route="/v1/self",le="1"} 0`,
		`witself_http_request_duration_seconds_bucket{method="GET",route="/v1/self",le="2.5"} 4`,
	} {
		if !strings.Contains(text, want+"\n") {
			t.Errorf("metrics missing %q", want)
		}
	}
	// The hydration alert boundary must not change unrelated histogram contracts.
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "witself_http_request_duration_seconds_bucket{") && strings.Contains(line, `le="1.5"`) {
			t.Errorf("self-digest boundary leaked into HTTP histogram: %s", line)
		}
	}
}

func TestAgentEmailCellStorageMetricsAreValueFreeAndBounded(t *testing.T) {
	metrics := newRuntimeMetrics()
	reads := 0
	handler := metricsMuxForCellStorageTest(metrics, func(ctx context.Context) (AgentEmailCellStorageMetrics, error) {
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
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			metricsMuxFor(newRuntimeMetrics(), test.read, nil, nil, nil, nil).ServeHTTP(
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
		CreateOperator: func(context.Context, string, string, string, string, *time.Duration) (Operator, string, *time.Time, error) {
			return Operator{}, "", nil, &PlanLimitError{
				Dimension: "operator_seats", Used: 10, Max: 10, Plan: "plan_private_name",
			}
		},
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
	_, _, _, _ = cfg.CreateOperator(context.Background(), "account_private_identifier", "operator_private_identifier", "operator_private_name", "token_private_name", nil)
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
		`witself_plan_limit_rejections_total{limit_dimension="operator_seats",operation="create"} 1`,
		`witself_plan_limit_rejections_total{limit_dimension="agents",operation="create"} 1`,
		`witself_plan_limit_rejections_total{limit_dimension="agents_per_realm",operation="create"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("plan-limit counter missing %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{
		"operator_private_identifier",
		"operator_private_name",
		"token_private_name",
		"plan_private_name",
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

// metricsMuxForCellStorageTest keeps the older two-argument shape for the
// cell-storage test above.
func metricsMuxForCellStorageTest(
	metrics *runtimeMetrics,
	read func(context.Context) (AgentEmailCellStorageMetrics, error),
) http.Handler {
	return metricsMuxFor(metrics, read, nil, nil, nil, nil)
}

// The SLO gauges must separate "nothing waiting" from "read failed": a broken
// read renders _up 0 with no gauge lines, never zeros that read healthy.
func TestSupportSLOMetricsRenderAndFailValueFree(t *testing.T) {
	ok := httptest.NewRecorder()
	metricsMuxFor(newRuntimeMetrics(), nil, func(context.Context) (SupportSLOMetrics, error) {
		return SupportSLOMetrics{UnansweredTickets: 2, OldestUnansweredSeconds: 90061}, nil
	}, nil, nil, nil).ServeHTTP(ok, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := ok.Body.String()
	for _, want := range []string{
		"witself_support_slo_metrics_up 1",
		"witself_support_unanswered_tickets 2",
		"witself_support_oldest_unanswered_seconds 90061",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}
	broken := httptest.NewRecorder()
	metricsMuxFor(newRuntimeMetrics(), nil, func(context.Context) (SupportSLOMetrics, error) {
		return SupportSLOMetrics{}, errors.New("db down with tenant detail")
	}, nil, nil, nil).ServeHTTP(broken, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	b := broken.Body.String()
	if !strings.Contains(b, "witself_support_slo_metrics_up 0") ||
		strings.Contains(b, "witself_support_unanswered_tickets") ||
		strings.Contains(b, "db down") {
		t.Fatalf("failed read leaked or rendered gauges:\n%s", b)
	}
}

var capacityMetricsForbidden = []string{
	"account_capacity_private", "realm_capacity_private", "agent_capacity_private",
	"operator_capacity_private", "plan_capacity_private", "plan_capacity_unlimited_private",
}

func assertCapacityMetricsValueFree(t *testing.T, output string) {
	t.Helper()
	for _, forbidden := range capacityMetricsForbidden {
		if strings.Contains(output, forbidden) {
			t.Fatalf("metrics exposed %q: %s", forbidden, output)
		}
	}
}

func assertCapacityMetricsDeadline(ctx context.Context, t *testing.T) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 2*time.Second {
		t.Fatal("collector must supply a live deadline no more than two seconds away")
	}
}

func capacityMetricsRequest() *http.Request {
	return httptest.NewRequest(http.MethodGet, "/metrics?account=account_capacity_private&realm=realm_capacity_private&agent=agent_capacity_private&operator=operator_capacity_private&plan=plan_capacity_private", nil)
}

func TestIdentityCapacityMetricsAreValueFreeAndBounded(t *testing.T) {
	reads := 0
	response := httptest.NewRecorder()
	metricsMuxFor(newRuntimeMetrics(), nil, nil, func(ctx context.Context) (IdentityCapacityMetrics, error) {
		reads++
		assertCapacityMetricsDeadline(ctx, t)
		return IdentityCapacityMetrics{
			Realms:         IdentityCapacityDimensionMetrics{AccountsMeasured: 3, AccountsNearLimit: 2, AccountsAtLimit: 1, AccountsUnlimited: 4, MinHeadroomRatio: 0},
			AgentsPerRealm: IdentityCapacityDimensionMetrics{AccountsMeasured: 5, AccountsNearLimit: 1, AccountsAtLimit: 0, AccountsUnlimited: 2, MinHeadroomRatio: 0.1},
			OperatorSeats:  IdentityCapacityDimensionMetrics{AccountsMeasured: 0, AccountsUnlimited: 7, MinHeadroomRatio: 1},
		}, nil
	}, nil, nil).ServeHTTP(response, capacityMetricsRequest())
	if response.Code != http.StatusOK || reads != 1 {
		t.Fatalf("metrics response=%d reads=%d", response.Code, reads)
	}
	output := response.Body.String()
	want := map[string]string{"witself_identity_capacity_metrics_up": "1"}
	for _, dimension := range []struct {
		name   string
		values []string
	}{
		{"realms", []string{"3", "2", "1", "4", "0"}},
		{"agents_per_realm", []string{"5", "1", "0", "2", "0.1"}},
		{"operator_seats", []string{"0", "0", "0", "7", "1"}},
	} {
		for i, metric := range []string{"accounts_measured", "accounts_near_limit", "accounts_at_limit", "accounts_unlimited", "min_headroom_ratio"} {
			want["witself_identity_capacity_"+metric+`{dimension="`+dimension.name+`"}`] = dimension.values[i]
		}
	}
	assertMetricSamples(t, output, "witself_identity_capacity_", want)
	assertCapacityMetricsValueFree(t, output)
}

func TestIdentityCapacityMetricsFailClosedWithoutErrorText(t *testing.T) {
	valid := IdentityCapacityDimensionMetrics{AccountsMeasured: 1, MinHeadroomRatio: 1}
	for _, test := range []struct {
		name   string
		status IdentityCapacityDimensionMetrics
		err    error
	}{
		{"read error", valid, errors.New("database_capacity_error_canary account_capacity_private")},
		{"negative count", IdentityCapacityDimensionMetrics{AccountsMeasured: -1, MinHeadroomRatio: 1}, nil},
		{"impossible counts", IdentityCapacityDimensionMetrics{AccountsMeasured: 1, AccountsNearLimit: 2, MinHeadroomRatio: 0.1}, nil},
		{"not a number", IdentityCapacityDimensionMetrics{AccountsMeasured: 1, MinHeadroomRatio: math.NaN()}, nil},
		{"out of range", IdentityCapacityDimensionMetrics{AccountsMeasured: 1, MinHeadroomRatio: 2}, nil},
		{"empty finite set", IdentityCapacityDimensionMetrics{MinHeadroomRatio: 0}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			metricsMuxFor(newRuntimeMetrics(), nil, nil, func(ctx context.Context) (IdentityCapacityMetrics, error) {
				assertCapacityMetricsDeadline(ctx, t)
				return IdentityCapacityMetrics{Realms: valid, AgentsPerRealm: valid, OperatorSeats: test.status}, test.err
			}, nil, nil).ServeHTTP(response, capacityMetricsRequest())
			assertCapacityMetricsFailClosed(t, response.Body.String(), "witself_identity_capacity_")
		})
	}
}

func TestAuditAppendMetricsAreValueFreeAndBounded(t *testing.T) {
	reads := 0
	response := httptest.NewRecorder()
	metricsMuxFor(newRuntimeMetrics(), nil, nil, nil, func(ctx context.Context) (AuditAppendMetrics, error) {
		reads++
		assertCapacityMetricsDeadline(ctx, t)
		return AuditAppendMetrics{TxFailures: 7}, nil
	}, nil).ServeHTTP(response, capacityMetricsRequest())
	if response.Code != http.StatusOK || reads != 1 {
		t.Fatalf("metrics response=%d reads=%d", response.Code, reads)
	}
	output := response.Body.String()
	assertMetricSamples(t, output, "witself_audit_append_", map[string]string{
		"witself_audit_append_metrics_up":                               "1",
		"witself_audit_append_tx_failures_total":                        "7",
		`witself_audit_append_total{result="success",reason="none"}`:    "0",
		`witself_audit_append_total{result="error",reason="not_found"}`: "0",
		`witself_audit_append_total{result="error",reason="bad_input"}`: "0",
		`witself_audit_append_total{result="error",reason="error"}`:     "0",
	})
	if !strings.Contains(output, "# TYPE witself_audit_append_tx_failures_total counter\n") {
		t.Fatal("audit failures must be a counter")
	}
	assertCapacityMetricsValueFree(t, output)
}

func TestAuditAppendMetricsFailClosedWithoutErrorText(t *testing.T) {
	response := httptest.NewRecorder()
	metricsMuxFor(newRuntimeMetrics(), nil, nil, nil, func(ctx context.Context) (AuditAppendMetrics, error) {
		assertCapacityMetricsDeadline(ctx, t)
		return AuditAppendMetrics{TxFailures: 7}, errors.New("database_capacity_error_canary account_capacity_private")
	}, nil).ServeHTTP(response, capacityMetricsRequest())
	assertCapacityMetricsFailClosed(t, response.Body.String(), "witself_audit_append_")
}

func assertMetricSamples(t *testing.T, output, prefix string, want map[string]string) {
	t.Helper()
	remaining := make(map[string]string, len(want))
	for k, v := range want {
		remaining[k] = v
	}
	for line := range strings.SplitSeq(output, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("malformed metric sample %q", line)
		}
		value, ok := remaining[fields[0]]
		if !ok || value != fields[1] {
			t.Fatalf("unexpected or duplicate metric sample %q", line)
		}
		delete(remaining, fields[0])
	}
	if len(remaining) != 0 {
		t.Fatalf("missing metric samples: %v\n%s", remaining, output)
	}
}

func assertCapacityMetricsFailClosed(t *testing.T, output, prefix string) {
	t.Helper()
	want := map[string]string{prefix + "metrics_up": "0"}
	if prefix == "witself_audit_append_" {
		for _, sample := range []string{
			`witself_audit_append_total{result="success",reason="none"}`,
			`witself_audit_append_total{result="error",reason="not_found"}`,
			`witself_audit_append_total{result="error",reason="bad_input"}`,
			`witself_audit_append_total{result="error",reason="error"}`,
		} {
			want[sample] = "0"
		}
	}
	assertMetricSamples(t, output, prefix, want)
	if strings.Contains(output, "database_capacity_error_canary") {
		t.Fatalf("collector exposed error text: %s", output)
	}
	assertCapacityMetricsValueFree(t, output)
}

func TestRuntimeMetricsAuditAppendReasonsAreBounded(t *testing.T) {
	metrics := newRuntimeMetrics()
	var nextErr error
	calls := 0
	cfg := metrics.instrumentConfig(Config{
		LogAccountEvent: func(_ context.Context, accountID, verb, actorKind string, metadata map[string]any) error {
			calls++
			if accountID != "account_capacity_private" || verb != "private_audit_verb" || actorKind != "private_actor_kind" || metadata["private_key"] != "private_value" {
				t.Fatal("audit wrapper changed arguments")
			}
			return nextErr
		},
	})
	for _, err := range []error{
		nil,
		fmt.Errorf("account_capacity_private: %w", ErrNotFound),
		fmt.Errorf("plan_capacity_private: %w", ErrBadInput),
		errors.New("database_capacity_error_canary"),
	} {
		nextErr = err
		if got := cfg.LogAccountEvent(context.Background(), "account_capacity_private", "private_audit_verb", "private_actor_kind", map[string]any{"private_key": "private_value"}); got != err {
			t.Fatalf("audit wrapper changed error: %v != %v", got, err)
		}
	}
	if calls != 4 {
		t.Fatalf("audit calls=%d, want 4", calls)
	}
	var output bytes.Buffer
	metrics.writePrometheus(&output)
	assertMetricSamples(t, output.String(), "witself_audit_append_", map[string]string{
		`witself_audit_append_total{result="success",reason="none"}`:    "1",
		`witself_audit_append_total{result="error",reason="not_found"}`: "1",
		`witself_audit_append_total{result="error",reason="bad_input"}`: "1",
		`witself_audit_append_total{result="error",reason="error"}`:     "1",
	})
	assertCapacityMetricsValueFree(t, output.String())
	for _, forbidden := range []string{"private_audit_verb", "private_actor_kind", "private_key", "private_value", "database_capacity_error_canary"} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("audit metrics exposed %q", forbidden)
		}
	}
}

func TestRuntimeMetricsObserveSecretMaterialDeliveriesWithBoundedLabels(t *testing.T) {
	metrics := newRuntimeMetrics()
	principal := DomainPrincipal{Kind: "agent", ID: "agent_sealed_private", AccountID: "account_sealed_private", RealmID: "realm_sealed_private", AgentName: "agent_name_sealed_private"}
	var nextKind string
	var nextErr error
	calls := 0
	cfg := metrics.instrumentConfig(Config{
		AccessSecretField: func(_ context.Context, p DomainPrincipal, secretID, fieldID string, in AccessSecretFieldRequest) (SecretMaterial, error) {
			calls++
			if p != principal || secretID != "secret_sealed_private" || fieldID != "field_sealed_private" || in.IdempotencyKey != "request_sealed_private" {
				t.Fatal("delivery wrapper changed operation arguments")
			}
			return SecretMaterial{SecretID: secretID, FieldID: fieldID, FieldName: "secret_name_sealed_private", FieldKind: nextKind, Ciphertext: []byte("ciphertext_sealed_private")}, nextErr
		},
	})
	for _, kind := range []string{"password", "api_key", "token", "totp", "kind_sealed_private"} {
		nextKind, nextErr = kind, nil
		result, err := cfg.AccessSecretField(context.Background(), principal, "secret_sealed_private", "field_sealed_private", AccessSecretFieldRequest{IdempotencyKey: "request_sealed_private"})
		if err != nil || result.FieldKind != kind || string(result.Ciphertext) != "ciphertext_sealed_private" {
			t.Fatalf("delivery wrapper changed result: kind=%q err=%v", result.FieldKind, err)
		}
	}
	for _, err := range []error{ErrNotFound, ErrForbidden, ErrConflict, ErrIdempotencyConflict, ErrSecretVaultKeyMismatch, ErrSecretVaultKeyUnavailable, ErrBadInput, errors.New("error_sealed_private")} {
		nextKind, nextErr = "password", fmt.Errorf("wrapped: %w", err)
		_, gotErr := cfg.AccessSecretField(context.Background(), principal, "secret_sealed_private", "field_sealed_private", AccessSecretFieldRequest{IdempotencyKey: "request_sealed_private"})
		if gotErr != nextErr {
			t.Fatal("delivery wrapper changed returned error")
		}
	}
	if calls != 13 {
		t.Fatalf("operation calls=%d want 13", calls)
	}
	var output bytes.Buffer
	metrics.writePrometheus(&output)
	assertMetricSamples(t, output.String(), "witself_secret_material_deliveries_total", map[string]string{
		`witself_secret_material_deliveries_total{field_kind="password",result="success"}`:  "1",
		`witself_secret_material_deliveries_total{field_kind="api_key",result="success"}`:   "1",
		`witself_secret_material_deliveries_total{field_kind="token",result="success"}`:     "1",
		`witself_secret_material_deliveries_total{field_kind="totp",result="success"}`:      "1",
		`witself_secret_material_deliveries_total{field_kind="other",result="success"}`:     "1",
		`witself_secret_material_deliveries_total{field_kind="unknown",result="not_found"}`: "1",
		`witself_secret_material_deliveries_total{field_kind="unknown",result="forbidden"}`: "1",
		`witself_secret_material_deliveries_total{field_kind="unknown",result="conflict"}`:  "4",
		`witself_secret_material_deliveries_total{field_kind="unknown",result="invalid"}`:   "1",
		`witself_secret_material_deliveries_total{field_kind="unknown",result="error"}`:     "1",
	})
	if strings.Contains(output.String(), "sealed_private") || strings.Contains(output.String(), "server_side_decrypt") {
		t.Fatalf("delivery metrics leaked private material or obsolete decryption label:\n%s", output.String())
	}
}

func TestRuntimeMetricsClassifyVaultLifecycleConflicts(t *testing.T) {
	principal := DomainPrincipal{Kind: "agent", ID: "agent_sealed_private"}
	for _, test := range []struct {
		flow, operation string
		configure       func(*Config, error) func(Config) error
	}{
		{"registration", "register", func(cfg *Config, nextErr error) func(Config) error {
			cfg.RegisterVaultKey = func(_ context.Context, p DomainPrincipal, _ RegisterVaultKeyRequest) (VaultKeyMutationResult, error) {
				if p != principal {
					t.Fatal("lifecycle wrapper changed arguments")
				}
				return VaultKeyMutationResult{}, nextErr
			}
			return func(cfg Config) error {
				_, err := cfg.RegisterVaultKey(context.Background(), principal, RegisterVaultKeyRequest{})
				return err
			}
		}},
		{"enrollment", "create", func(cfg *Config, nextErr error) func(Config) error {
			cfg.CreateVaultKeyEnrollment = func(_ context.Context, p DomainPrincipal, _ CreateVaultKeyEnrollmentRequest) (VaultKeyEnrollment, error) {
				if p != principal {
					t.Fatal("lifecycle wrapper changed arguments")
				}
				return VaultKeyEnrollment{}, nextErr
			}
			return func(cfg Config) error {
				_, err := cfg.CreateVaultKeyEnrollment(context.Background(), principal, CreateVaultKeyEnrollmentRequest{})
				return err
			}
		}},
		{"enrollment", "approve", func(cfg *Config, nextErr error) func(Config) error {
			cfg.ApproveVaultKeyEnrollment = func(_ context.Context, p DomainPrincipal, id string, _ ApproveVaultKeyEnrollmentRequest) (VaultKeyEnrollment, error) {
				if p != principal || id != "lifecycle_sealed_private" {
					t.Fatal("lifecycle wrapper changed arguments")
				}
				return VaultKeyEnrollment{}, nextErr
			}
			return func(cfg Config) error {
				_, err := cfg.ApproveVaultKeyEnrollment(context.Background(), principal, "lifecycle_sealed_private", ApproveVaultKeyEnrollmentRequest{})
				return err
			}
		}},
		{"enrollment", "receive", func(cfg *Config, nextErr error) func(Config) error {
			cfg.ReceiveVaultKeyEnrollment = func(_ context.Context, p DomainPrincipal, id string, targetLocationID string) (VaultKeyEnrollmentTransfer, error) {
				if p != principal || id != "lifecycle_sealed_private" || targetLocationID != "location_sealed_private" {
					t.Fatal("lifecycle wrapper changed arguments")
				}
				return VaultKeyEnrollmentTransfer{}, nextErr
			}
			return func(cfg Config) error {
				_, err := cfg.ReceiveVaultKeyEnrollment(context.Background(), principal, "lifecycle_sealed_private", "location_sealed_private")
				return err
			}
		}},
		{"enrollment", "consume", func(cfg *Config, nextErr error) func(Config) error {
			cfg.ConsumeVaultKeyEnrollment = func(_ context.Context, p DomainPrincipal, id string, _ ConsumeVaultKeyEnrollmentRequest) (VaultKeyEnrollment, error) {
				if p != principal || id != "lifecycle_sealed_private" {
					t.Fatal("lifecycle wrapper changed arguments")
				}
				return VaultKeyEnrollment{}, nextErr
			}
			return func(cfg Config) error {
				_, err := cfg.ConsumeVaultKeyEnrollment(context.Background(), principal, "lifecycle_sealed_private", ConsumeVaultKeyEnrollmentRequest{})
				return err
			}
		}},
		{"enrollment", "cancel", func(cfg *Config, nextErr error) func(Config) error {
			cfg.CancelVaultKeyEnrollment = func(_ context.Context, p DomainPrincipal, id string, _ CancelVaultKeyEnrollmentRequest) (VaultKeyEnrollment, error) {
				if p != principal || id != "lifecycle_sealed_private" {
					t.Fatal("lifecycle wrapper changed arguments")
				}
				return VaultKeyEnrollment{}, nextErr
			}
			return func(cfg Config) error {
				_, err := cfg.CancelVaultKeyEnrollment(context.Background(), principal, "lifecycle_sealed_private", CancelVaultKeyEnrollmentRequest{})
				return err
			}
		}},
		{"rotation", "start", func(cfg *Config, nextErr error) func(Config) error {
			cfg.StartVaultKeyRotation = func(_ context.Context, p DomainPrincipal, _ StartVaultKeyRotationRequest) (VaultKeyRotationMutationResult, error) {
				if p != principal {
					t.Fatal("lifecycle wrapper changed arguments")
				}
				return VaultKeyRotationMutationResult{}, nextErr
			}
			return func(cfg Config) error {
				_, err := cfg.StartVaultKeyRotation(context.Background(), principal, StartVaultKeyRotationRequest{})
				return err
			}
		}},
		{"rotation", "stage", func(cfg *Config, nextErr error) func(Config) error {
			cfg.StageVaultKeyRotation = func(_ context.Context, p DomainPrincipal, id string, _ StageVaultKeyRotationRequest) (VaultKeyRotationMutationResult, error) {
				if p != principal || id != "lifecycle_sealed_private" {
					t.Fatal("lifecycle wrapper changed arguments")
				}
				return VaultKeyRotationMutationResult{}, nextErr
			}
			return func(cfg Config) error {
				_, err := cfg.StageVaultKeyRotation(context.Background(), principal, "lifecycle_sealed_private", StageVaultKeyRotationRequest{})
				return err
			}
		}},
		{"rotation", "commit", func(cfg *Config, nextErr error) func(Config) error {
			cfg.CommitVaultKeyRotation = func(_ context.Context, p DomainPrincipal, id string, _ CommitVaultKeyRotationRequest) (VaultKeyRotationMutationResult, error) {
				if p != principal || id != "lifecycle_sealed_private" {
					t.Fatal("lifecycle wrapper changed arguments")
				}
				return VaultKeyRotationMutationResult{}, nextErr
			}
			return func(cfg Config) error {
				_, err := cfg.CommitVaultKeyRotation(context.Background(), principal, "lifecycle_sealed_private", CommitVaultKeyRotationRequest{})
				return err
			}
		}},
		{"rotation", "cancel", func(cfg *Config, nextErr error) func(Config) error {
			cfg.CancelVaultKeyRotation = func(_ context.Context, p DomainPrincipal, id string, _ CancelVaultKeyRotationRequest) (VaultKeyRotationMutationResult, error) {
				if p != principal || id != "lifecycle_sealed_private" {
					t.Fatal("lifecycle wrapper changed arguments")
				}
				return VaultKeyRotationMutationResult{}, nextErr
			}
			return func(cfg Config) error {
				_, err := cfg.CancelVaultKeyRotation(context.Background(), principal, "lifecycle_sealed_private", CancelVaultKeyRotationRequest{})
				return err
			}
		}},
	} {
		t.Run(test.flow+"/"+test.operation, func(t *testing.T) {
			metrics := newRuntimeMetrics()
			for _, nextErr := range []error{nil, fmt.Errorf("wrapped: %w", ErrConflict), errors.New("error_sealed_private")} {
				cfg := Config{}
				invoke := test.configure(&cfg, nextErr)
				if got := invoke(metrics.instrumentConfig(cfg)); got != nextErr {
					t.Fatal("lifecycle wrapper changed returned error")
				}
			}
			var output bytes.Buffer
			metrics.writePrometheus(&output)
			want := zeroVaultLifecycleSamples()
			for _, result := range []string{"success", "conflict", "error"} {
				want[`witself_vault_lifecycle_operations_total{flow="`+test.flow+`",operation="`+test.operation+`",result="`+result+`"}`] = "1"
			}
			assertMetricSamples(t, output.String(), "witself_vault_lifecycle_operations_total", want)
			if strings.Contains(output.String(), "sealed_private") {
				t.Fatalf("lifecycle metrics leaked private data:\n%s", output.String())
			}
		})
	}
}

func TestSealedPlanePostureMetricsRenderAndFailValueFree(t *testing.T) {
	healthy := SealedPlanePostureMetrics{OpenRotations: 2, OldestOpenRotationSeconds: 90061, PendingEnrollments: 3, OldestPendingEnrollmentSeconds: 61, MaxAgentDeliveries15m: 121}
	gaugeNames := []string{"witself_vault_open_rotations", "witself_vault_oldest_open_rotation_seconds", "witself_vault_pending_enrollments", "witself_vault_oldest_pending_enrollment_seconds", "witself_secret_material_max_agent_deliveries_15m"}
	for _, test := range []struct {
		name     string
		status   SealedPlanePostureMetrics
		err      error
		disabled bool
	}{
		{name: "healthy", status: healthy},
		{name: "zero"},
		{name: "read error", status: healthy, err: errors.New("error_sealed_private")},
		{name: "negative rotations", status: SealedPlanePostureMetrics{OpenRotations: -1}},
		{name: "negative rotation age", status: SealedPlanePostureMetrics{OldestOpenRotationSeconds: -1}},
		{name: "negative enrollments", status: SealedPlanePostureMetrics{PendingEnrollments: -1}},
		{name: "negative enrollment age", status: SealedPlanePostureMetrics{OldestPendingEnrollmentSeconds: -1}},
		{name: "negative volume", status: SealedPlanePostureMetrics{MaxAgentDeliveries15m: -1}},
		{name: "nil reader", disabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			reads := 0
			read := func(ctx context.Context) (SealedPlanePostureMetrics, error) {
				reads++
				assertCapacityMetricsDeadline(ctx, t)
				return test.status, test.err
			}
			if test.disabled {
				read = nil
			}
			response := httptest.NewRecorder()
			metricsMuxFor(newRuntimeMetrics(), nil, nil, nil, nil, read).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics?agent=agent_sealed_private", nil))
			if response.Code != http.StatusOK || (!test.disabled && reads != 1) || (test.disabled && reads != 0) {
				t.Fatalf("response=%d reads=%d", response.Code, reads)
			}
			body := response.Body.String()
			want := map[string]string{}
			if !test.disabled {
				want["witself_sealed_plane_posture_metrics_up"] = "0"
				if test.name == "healthy" || test.name == "zero" {
					want["witself_sealed_plane_posture_metrics_up"] = "1"
					for i, value := range []int64{test.status.OpenRotations, test.status.OldestOpenRotationSeconds, test.status.PendingEnrollments, test.status.OldestPendingEnrollmentSeconds, test.status.MaxAgentDeliveries15m} {
						want[gaugeNames[i]] = strconv.FormatInt(value, 10)
					}
				}
			}
			var samples strings.Builder
			for line := range strings.SplitSeq(body, "\n") {
				if strings.HasPrefix(line, "witself_sealed_plane_posture_") || (strings.HasPrefix(line, "witself_vault_") && !strings.HasPrefix(line, "witself_vault_lifecycle_")) || strings.HasPrefix(line, "witself_secret_material_max_agent_") {
					samples.WriteString(line + "\n")
				}
			}
			assertMetricSamples(t, samples.String(), "witself_", want)
			if strings.Contains(body, "sealed_private") {
				t.Fatalf("posture metrics leaked private data:\n%s", body)
			}
		})
	}
}
