package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func curationMetricsHandler(m *runtimeMetrics, counters func() MemoryCurationCounters, queue func(context.Context) (MemoryCurationQueueMetrics, error)) http.Handler {
	return metricsMuxFor(m, nil, nil, nil, nil, nil, counters, queue, nil)
}

func scrapeCurationMetrics(t *testing.T, h http.Handler) string {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics?run=mrun_private_identifier", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("scrape status=%d", w.Code)
	}
	return w.Body.String()
}

func TestMemoryCurationMetricLabelsAndIdleBaselines(t *testing.T) {
	metrics := newRuntimeMetrics()
	output := scrapeCurationMetrics(t, curationMetricsHandler(metrics, func() MemoryCurationCounters {
		return MemoryCurationCounters{}
	}, func(context.Context) (MemoryCurationQueueMetrics, error) {
		return MemoryCurationQueueMetrics{}, nil
	}))
	transitions := map[string]string{}
	for _, edge := range [][2]string{
		{"none", "open"}, {"open", "planned"}, {"planned", "applied"}, {"applied", "rolled_back"},
		{"open", "abandoned"}, {"planned", "abandoned"}, {"open", "interrupted"},
		{"planned", "interrupted"}, {"planned", "conflict"},
	} {
		transitions[fmt.Sprintf(`witself_memory_curation_run_transitions_total{from=%q,to=%q}`, edge[0], edge[1])] = "0"
	}
	assertMetricSamples(t, output, "witself_memory_curation_run_transitions_total", transitions)
	leases := map[string]string{}
	for _, event := range []string{"start", "renew", "expire", "reconcile"} {
		leases[fmt.Sprintf(`witself_memory_curation_lease_events_total{event=%q}`, event)] = "0"
	}
	assertMetricSamples(t, output, "witself_memory_curation_lease_events_total", leases)
	operations := map[string]string{}
	for _, op := range []string{"start", "renew", "plan", "apply", "cancel", "abandon", "rollback"} {
		for _, result := range []string{"success", "error"} {
			operations[fmt.Sprintf(`witself_memory_curation_operations_total{operation=%q,result=%q}`, op, result)] = "0"
		}
	}
	assertMetricSamples(t, output, "witself_memory_curation_operations_total", operations)
	assertMetricSamples(t, output, "witself_memory_curation_requests_pending", map[string]string{"witself_memory_curation_requests_pending": "0"})
	assertMetricSamples(t, output, "witself_memory_curation_queue_metrics_up", map[string]string{"witself_memory_curation_queue_metrics_up": "1"})
	histogram := map[string]string{}
	for _, bound := range []string{"0", "30", "60", "300", "900", "1800", "3600", "21600", "86400", "+Inf"} {
		histogram[fmt.Sprintf(`witself_memory_curation_queue_age_seconds_bucket{le=%q}`, bound)] = "1"
	}
	histogram["witself_memory_curation_queue_age_seconds_count"] = "1"
	histogram["witself_memory_curation_queue_age_seconds_sum"] = "0"
	assertMetricSamples(t, output, "witself_memory_curation_queue_age_seconds", histogram)
}

func TestMemoryCurationMetricsRejectIdentifierLabels(t *testing.T) {
	metrics := newRuntimeMetrics()
	const private = "mrun_private_identifier"
	metrics.observeCurationOperation(private, errors.New(private))
	h := curationMetricsHandler(metrics, func() MemoryCurationCounters {
		return MemoryCurationCounters{
			Transitions: map[MemoryCurationTransition]uint64{
				{From: private, To: "open"}: 7, {From: "open", To: private}: 8,
				{From: "open", To: "planned"}: 2,
			},
			LeaseEvents: map[string]uint64{private: 9, "renew": 3},
		}
	}, func(ctx context.Context) (MemoryCurationQueueMetrics, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 2*time.Second {
			t.Error("queue read is missing its bounded deadline")
		}
		return MemoryCurationQueueMetrics{RequestsPending: 4, QueueAgeSeconds: 901}, nil
	})
	output := scrapeCurationMetrics(t, h)
	if strings.Contains(output, private) {
		t.Fatal("identifier reached metrics")
	}
	for _, sample := range []string{
		`witself_memory_curation_run_transitions_total{from="open",to="planned"} 2`,
		`witself_memory_curation_lease_events_total{event="renew"} 3`,
		`witself_memory_curation_queue_age_seconds_bucket{le="900"} 0`,
		`witself_memory_curation_queue_age_seconds_bucket{le="1800"} 1`,
		`witself_memory_curation_queue_age_seconds_sum 901`,
		`witself_memory_curation_requests_pending 4`,
	} {
		if !strings.Contains(output, sample+"\n") {
			t.Errorf("missing %s", sample)
		}
	}
}

func TestMemoryCurationCallMetricsKeepAllMutationPathsValueFree(t *testing.T) {
	metrics := newRuntimeMetrics()
	const private = "mrun_private_identifier"
	p := DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_private_identifier", AccountID: "acct_private_identifier", RealmID: "realm_private_identifier"}
	var failure error
	result := func() (any, error) { return map[string]string{"run_id": private, "state": private}, failure }
	cfg := metrics.instrumentConfig(Config{
		StartMemoryCuration: func(context.Context, DomainPrincipal, StartMemoryCurationRequest) (any, error) { return result() },
		RenewMemoryCuration: func(context.Context, DomainPrincipal, string, RenewMemoryCurationRequest) (any, error) {
			return result()
		},
		PlanMemoryCuration: func(context.Context, DomainPrincipal, string, PlanMemoryCurationRequest) (any, error) {
			return result()
		},
		ApplyMemoryCuration: func(context.Context, DomainPrincipal, string, ApplyMemoryCurationRequest) (any, error) {
			return result()
		},
		CancelMemoryCuration: func(context.Context, DomainPrincipal, string, FinishMemoryCurationRequest) (any, error) {
			return result()
		},
		AbandonMemoryCuration: func(context.Context, DomainPrincipal, string, FinishMemoryCurationRequest) (any, error) {
			return result()
		},
		RollbackMemoryCuration: func(context.Context, DomainPrincipal, string, RollbackMemoryCurationRequest) (any, error) {
			return result()
		},
	})
	calls := map[string]func() (any, error){
		"start": func() (any, error) {
			return cfg.StartMemoryCuration(context.Background(), p, StartMemoryCurationRequest{RequestID: "mcrq_private_identifier", IdempotencyKey: private})
		},
		"renew": func() (any, error) {
			return cfg.RenewMemoryCuration(context.Background(), p, private, RenewMemoryCurationRequest{IdempotencyKey: private})
		},
		"plan": func() (any, error) {
			return cfg.PlanMemoryCuration(context.Background(), p, private, PlanMemoryCurationRequest{IdempotencyKey: private})
		},
		"apply": func() (any, error) {
			return cfg.ApplyMemoryCuration(context.Background(), p, private, ApplyMemoryCurationRequest{IdempotencyKey: private})
		},
		"cancel": func() (any, error) {
			return cfg.CancelMemoryCuration(context.Background(), p, private, FinishMemoryCurationRequest{IdempotencyKey: private, Reason: private})
		},
		"abandon": func() (any, error) {
			return cfg.AbandonMemoryCuration(context.Background(), p, private, FinishMemoryCurationRequest{IdempotencyKey: private, Reason: private})
		},
		"rollback": func() (any, error) {
			return cfg.RollbackMemoryCuration(context.Background(), p, private, RollbackMemoryCurationRequest{IdempotencyKey: private})
		},
	}
	for _, err := range []error{nil, errors.New(private)} {
		failure = err
		for name, call := range calls {
			got, gotErr := call()
			if gotErr != err || got.(map[string]string)["run_id"] != private {
				t.Fatalf("%s changed domain return", name)
			}
		}
	}
	output := scrapeCurationMetrics(t, curationMetricsHandler(metrics, nil, nil))
	for _, forbidden := range []string{private, p.ID, p.AccountID, p.RealmID, "mcrq_private_identifier"} {
		if strings.Contains(output, forbidden) {
			t.Fatal("mutation identifier reached metrics")
		}
	}
	want := map[string]string{}
	for name := range calls {
		for _, result := range []string{"success", "error"} {
			want[fmt.Sprintf(`witself_memory_curation_operations_total{operation=%q,result=%q}`, name, result)] = "1"
		}
	}
	assertMetricSamples(t, output, "witself_memory_curation_operations_total", want)
}

func TestMemoryCurationQueueReadFailureDoesNotReportHealthyOrStaleQueue(t *testing.T) {
	for _, test := range []struct {
		name  string
		value MemoryCurationQueueMetrics
		err   error
	}{
		{"error", MemoryCurationQueueMetrics{}, errors.New("acct_private_identifier")},
		{"negative count", MemoryCurationQueueMetrics{RequestsPending: -1}, nil},
		{"negative age", MemoryCurationQueueMetrics{RequestsPending: 1, QueueAgeSeconds: -1}, nil},
		{"nan", MemoryCurationQueueMetrics{RequestsPending: 1, QueueAgeSeconds: math.NaN()}, nil},
		{"infinity", MemoryCurationQueueMetrics{RequestsPending: 1, QueueAgeSeconds: math.Inf(1)}, nil},
		{"empty with age", MemoryCurationQueueMetrics{QueueAgeSeconds: 10}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newRuntimeMetrics()
			bad := false
			h := curationMetricsHandler(m, func() MemoryCurationCounters {
				return MemoryCurationCounters{LeaseEvents: map[string]uint64{"start": 2}}
			}, func(context.Context) (MemoryCurationQueueMetrics, error) {
				if bad {
					return test.value, test.err
				}
				return MemoryCurationQueueMetrics{RequestsPending: 1, QueueAgeSeconds: 1000}, nil
			})
			scrapeCurationMetrics(t, h)
			bad = true
			output := scrapeCurationMetrics(t, h)
			for _, forbidden := range []string{"acct_private_identifier", "witself_memory_curation_queue_age_seconds", "witself_memory_curation_requests_pending"} {
				if strings.Contains(output, forbidden) {
					t.Errorf("failed projection exposed %s", forbidden)
				}
			}
			assertMetricSamples(t, output, "witself_memory_curation_queue_metrics_up", map[string]string{"witself_memory_curation_queue_metrics_up": "0"})
			if !strings.Contains(output, `witself_memory_curation_lease_events_total{event="start"} 2`) {
				t.Fatal("DB failure lost process counters")
			}
			bad = false
			output = scrapeCurationMetrics(t, h)
			if !strings.Contains(output, "witself_memory_curation_queue_age_seconds_count 2\n") {
				t.Fatal("failed scrape incremented histogram")
			}
		})
	}
}

func TestMemoryCurationQueueConcurrentScrapes(t *testing.T) {
	m := newRuntimeMetrics()
	h := curationMetricsHandler(m, nil, func(context.Context) (MemoryCurationQueueMetrics, error) {
		return MemoryCurationQueueMetrics{RequestsPending: 1, QueueAgeSeconds: 900}, nil
	})
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() { scrapeCurationMetrics(t, h) })
	}
	wg.Wait()
	output := scrapeCurationMetrics(t, h)
	if !strings.Contains(output, "witself_memory_curation_queue_age_seconds_count 33\n") ||
		!strings.Contains(output, "witself_memory_curation_queue_age_seconds_sum 29700\n") {
		t.Fatal("concurrent scrapes lost queue observations")
	}
}

func TestMemoryCurationQueueReadOverlapsExistingCollectors(t *testing.T) {
	queueStarted := make(chan struct{})
	supportStarted := make(chan struct{})
	h := metricsMuxFor(newRuntimeMetrics(), nil, func(ctx context.Context) (SupportSLOMetrics, error) {
		close(supportStarted)
		select {
		case <-queueStarted:
			return SupportSLOMetrics{}, nil
		case <-ctx.Done():
			t.Error("curation read waited for the prior collector's deadline")
			return SupportSLOMetrics{}, ctx.Err()
		}
	}, nil, nil, nil, nil, func(ctx context.Context) (MemoryCurationQueueMetrics, error) {
		close(queueStarted)
		select {
		case <-supportStarted:
			return MemoryCurationQueueMetrics{}, nil
		case <-ctx.Done():
			t.Error("existing collectors waited for the curation deadline")
			return MemoryCurationQueueMetrics{}, ctx.Err()
		}
	}, nil)
	output := scrapeCurationMetrics(t, h)
	for _, sample := range []string{"witself_support_slo_metrics_up 1", "witself_memory_curation_queue_metrics_up 1"} {
		if !strings.Contains(output, sample+"\n") {
			t.Errorf("overlapping read omitted %s", sample)
		}
	}
}
