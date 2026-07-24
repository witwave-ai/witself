package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// runtimeMetrics is deliberately small and dependency-free. Labels are drawn
// only from bounded server-owned enums and route templates; tenant identifiers,
// request paths, memory content, database details, and error text never enter
// this collector.
type runtimeMetrics struct {
	mu sync.Mutex

	httpInFlight int64
	httpRequests map[httpMetricLabels]uint64
	httpLatency  map[httpDurationLabels]*metricHistogram

	memoryOperations   map[memoryOperationMetricLabels]uint64
	memoryRecalls      map[recallMetricLabels]uint64
	memoryRecallTime   map[recallDurationLabels]*metricHistogram
	memoryRecallHits   map[string]*metricHistogram
	vectorSearches     map[vectorSearchMetricLabels]uint64
	vectorFallbacks    map[vectorFallbackMetricLabels]uint64
	curationOperations map[operationMetricLabels]uint64
	planLimitRejects   map[limitMetricLabels]uint64
	secretLimitRejects map[limitMetricLabels]uint64
}

type httpMetricLabels struct {
	Method, Route, StatusClass, Result string
}

type httpDurationLabels struct {
	Method, Route string
}

type operationMetricLabels struct {
	Operation, Result string
}

type memoryOperationMetricLabels struct {
	Operation, PrincipalKind, Result string
}

type recallMetricLabels struct {
	Mode, PrincipalKind, Result string
}

type recallDurationLabels struct {
	Mode, PrincipalKind string
}

type vectorSearchMetricLabels struct {
	Coverage, Result string
}

type vectorFallbackMetricLabels struct {
	Reason string
}

type limitMetricLabels struct {
	LimitDimension, Operation string
}

type metricHistogram struct {
	Buckets []uint64
	Count   uint64
	Sum     float64
}

var latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
var hitBuckets = []float64{0, 1, 2, 5, 10, 25, 50, 100}

func newRuntimeMetrics() *runtimeMetrics {
	return &runtimeMetrics{
		httpRequests:       make(map[httpMetricLabels]uint64),
		httpLatency:        make(map[httpDurationLabels]*metricHistogram),
		memoryOperations:   make(map[memoryOperationMetricLabels]uint64),
		memoryRecalls:      make(map[recallMetricLabels]uint64),
		memoryRecallTime:   make(map[recallDurationLabels]*metricHistogram),
		memoryRecallHits:   make(map[string]*metricHistogram),
		vectorSearches:     make(map[vectorSearchMetricLabels]uint64),
		vectorFallbacks:    make(map[vectorFallbackMetricLabels]uint64),
		curationOperations: make(map[operationMetricLabels]uint64),
		planLimitRejects:   make(map[limitMetricLabels]uint64),
		secretLimitRejects: make(map[limitMetricLabels]uint64),
	}
}

func (m *runtimeMetrics) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		m.mu.Lock()
		m.httpInFlight++
		m.mu.Unlock()

		recorder := &metricsResponseWriter{ResponseWriter: w}
		completed := false
		defer func() {
			status := recorder.status
			if status == 0 && completed {
				status = http.StatusOK
			} else if status == 0 {
				status = http.StatusInternalServerError
			}
			m.observeHTTP(r.Method, r.Pattern, status, time.Since(started))
		}()
		next.ServeHTTP(recorder, r)
		completed = true
	})
}

func (m *runtimeMetrics) observeHTTP(method, pattern string, status int, elapsed time.Duration) {
	method = metricMethod(method)
	route := metricRoute(pattern)
	statusClass := strconv.Itoa(status/100) + "xx"
	result := metricResult(status < 400)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.httpInFlight > 0 {
		m.httpInFlight--
	}
	m.httpRequests[httpMetricLabels{Method: method, Route: route, StatusClass: statusClass, Result: result}]++
	observeHistogram(m.httpLatency, httpDurationLabels{Method: method, Route: route}, elapsed.Seconds(), latencyBuckets)
}

func (m *runtimeMetrics) instrumentConfig(cfg Config) Config {
	if operation := cfg.CreateRealm; operation != nil {
		cfg.CreateRealm = func(ctx context.Context, accountID, name string) (Realm, error) {
			result, err := operation(ctx, accountID, name)
			m.observePlanLimitRejection(err, "realms")
			return result, err
		}
	}
	if operation := cfg.CreateAgent; operation != nil {
		cfg.CreateAgent = func(ctx context.Context, accountID, realmID, name string) (Agent, error) {
			result, err := operation(ctx, accountID, realmID, name)
			m.observePlanLimitRejection(err, "agents_per_realm")
			return result, err
		}
	}
	if operation := cfg.CreateSecret; operation != nil {
		cfg.CreateSecret = func(ctx context.Context, p DomainPrincipal, in CreateSecretRequest) (SecretMutationResult, error) {
			result, err := operation(ctx, p, in)
			if errors.Is(err, ErrSecretLimitReached) {
				m.mu.Lock()
				m.secretLimitRejects[limitMetricLabels{
					LimitDimension: "stored_secret", Operation: "create",
				}]++
				m.mu.Unlock()
			}
			return result, err
		}
	}
	if operation := cfg.CaptureMemory; operation != nil {
		cfg.CaptureMemory = func(ctx context.Context, p DomainPrincipal, in CaptureMemoryRequest) (MemoryMutationResult, error) {
			result, err := operation(ctx, p, in)
			m.observeMemoryOperation("add", p.Kind, err)
			return result, err
		}
	}
	if operation := cfg.GetMemory; operation != nil {
		cfg.GetMemory = func(ctx context.Context, p DomainPrincipal, memoryID string) (Memory, error) {
			result, err := operation(ctx, p, memoryID)
			m.observeMemoryOperation("read", p.Kind, err)
			return result, err
		}
	}
	if operation := cfg.ListMemories; operation != nil {
		cfg.ListMemories = func(ctx context.Context, p DomainPrincipal, opts MemoryListOptions) (MemoryPage, error) {
			result, err := operation(ctx, p, opts)
			m.observeMemoryOperation("list", p.Kind, err)
			return result, err
		}
	}
	if operation := cfg.GetMemoryHistory; operation != nil {
		cfg.GetMemoryHistory = func(ctx context.Context, p DomainPrincipal, memoryID string, opts MemoryHistoryOptions) (MemoryHistoryPage, error) {
			result, err := operation(ctx, p, memoryID, opts)
			m.observeMemoryOperation("history", p.Kind, err)
			return result, err
		}
	}
	if operation := cfg.AdjustMemory; operation != nil {
		cfg.AdjustMemory = func(ctx context.Context, p DomainPrincipal, memoryID string, in AdjustMemoryRequest) (MemoryMutationResult, error) {
			result, err := operation(ctx, p, memoryID, in)
			m.observeMemoryOperation("adjust", p.Kind, err)
			return result, err
		}
	}
	if operation := cfg.SupersedeMemory; operation != nil {
		cfg.SupersedeMemory = func(ctx context.Context, p DomainPrincipal, memoryID string, in SupersedeMemoryRequest) (SupersedeMemoryResult, error) {
			result, err := operation(ctx, p, memoryID, in)
			m.observeMemoryOperation("supersede", p.Kind, err)
			return result, err
		}
	}
	if operation := cfg.ForgetMemory; operation != nil {
		cfg.ForgetMemory = func(ctx context.Context, p DomainPrincipal, memoryID string, in MemoryLifecycleRequest) (MemoryMutationResult, error) {
			result, err := operation(ctx, p, memoryID, in)
			m.observeMemoryOperation("forget", p.Kind, err)
			return result, err
		}
	}
	if operation := cfg.RestoreMemory; operation != nil {
		cfg.RestoreMemory = func(ctx context.Context, p DomainPrincipal, memoryID string, in MemoryLifecycleRequest) (MemoryMutationResult, error) {
			result, err := operation(ctx, p, memoryID, in)
			m.observeMemoryOperation("restore", p.Kind, err)
			return result, err
		}
	}
	if operation := cfg.ReactivateMemory; operation != nil {
		cfg.ReactivateMemory = func(ctx context.Context, p DomainPrincipal, memoryID string, in MemoryLifecycleRequest) (MemoryMutationResult, error) {
			result, err := operation(ctx, p, memoryID, in)
			m.observeMemoryOperation("reactivate", p.Kind, err)
			return result, err
		}
	}
	if operation := cfg.ResolveMemoryEvidence; operation != nil {
		cfg.ResolveMemoryEvidence = func(ctx context.Context, p DomainPrincipal, evidenceID string, in ResolveMemoryEvidenceRequest) (MemoryEvidence, error) {
			result, err := operation(ctx, p, evidenceID, in)
			m.observeMemoryOperation("evidence_resolve", p.Kind, err)
			return result, err
		}
	}
	if operation := cfg.DeleteMemory; operation != nil {
		cfg.DeleteMemory = func(ctx context.Context, p DomainPrincipal, in DeleteMemoryRequest) (MemoryDeletionReceipt, error) {
			result, err := operation(ctx, p, in)
			m.observeMemoryOperation("delete", p.Kind, err)
			return result, err
		}
	}
	if recall := cfg.RecallMemories; recall != nil {
		cfg.RecallMemories = func(ctx context.Context, p DomainPrincipal, in MemoryRecallRequest) (MemoryRecallPage, error) {
			started := time.Now()
			page, err := recall(ctx, p, in)
			m.observeRecall(p.Kind, in, page, err, time.Since(started))
			return page, err
		}
	}
	if operation := cfg.StartMemoryCuration; operation != nil {
		cfg.StartMemoryCuration = func(ctx context.Context, p DomainPrincipal, in StartMemoryCurationRequest) (any, error) {
			result, err := operation(ctx, p, in)
			m.observeCurationOperation("start", err)
			return result, err
		}
	}
	if operation := cfg.RenewMemoryCuration; operation != nil {
		cfg.RenewMemoryCuration = func(ctx context.Context, p DomainPrincipal, runID string, in RenewMemoryCurationRequest) (any, error) {
			result, err := operation(ctx, p, runID, in)
			m.observeCurationOperation("renew", err)
			return result, err
		}
	}
	if operation := cfg.PlanMemoryCuration; operation != nil {
		cfg.PlanMemoryCuration = func(ctx context.Context, p DomainPrincipal, runID string, in PlanMemoryCurationRequest) (any, error) {
			result, err := operation(ctx, p, runID, in)
			m.observeCurationOperation("plan", err)
			return result, err
		}
	}
	if operation := cfg.ApplyMemoryCuration; operation != nil {
		cfg.ApplyMemoryCuration = func(ctx context.Context, p DomainPrincipal, runID string, in ApplyMemoryCurationRequest) (any, error) {
			result, err := operation(ctx, p, runID, in)
			m.observeCurationOperation("apply", err)
			return result, err
		}
	}
	if operation := cfg.CancelMemoryCuration; operation != nil {
		cfg.CancelMemoryCuration = func(ctx context.Context, p DomainPrincipal, runID string, in FinishMemoryCurationRequest) (any, error) {
			result, err := operation(ctx, p, runID, in)
			m.observeCurationOperation("cancel", err)
			return result, err
		}
	}
	if operation := cfg.AbandonMemoryCuration; operation != nil {
		cfg.AbandonMemoryCuration = func(ctx context.Context, p DomainPrincipal, runID string, in FinishMemoryCurationRequest) (any, error) {
			result, err := operation(ctx, p, runID, in)
			m.observeCurationOperation("abandon", err)
			return result, err
		}
	}
	if operation := cfg.RollbackMemoryCuration; operation != nil {
		cfg.RollbackMemoryCuration = func(ctx context.Context, p DomainPrincipal, runID string, in RollbackMemoryCurationRequest) (any, error) {
			result, err := operation(ctx, p, runID, in)
			m.observeCurationOperation("rollback", err)
			return result, err
		}
	}
	return cfg
}

func (m *runtimeMetrics) observePlanLimitRejection(err error, fallbackDimension string) {
	if !errors.Is(err, ErrPlanLimit) {
		return
	}
	dimension := fallbackDimension
	var detail *PlanLimitError
	if errors.As(err, &detail) {
		dimension = detail.Dimension
	}
	switch dimension {
	case "realms", "agents", "agents_per_realm":
	default:
		dimension = "unknown"
	}
	m.mu.Lock()
	m.planLimitRejects[limitMetricLabels{
		LimitDimension: dimension,
		Operation:      "create",
	}]++
	m.mu.Unlock()
}

func (m *runtimeMetrics) observeMemoryOperation(operation, principalKind string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.memoryOperations[memoryOperationMetricLabels{
		Operation: operation, PrincipalKind: metricPrincipalKind(principalKind), Result: metricResult(err == nil),
	}]++
}

func (m *runtimeMetrics) observeCurationOperation(operation string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.curationOperations[operationMetricLabels{Operation: operation, Result: metricResult(err == nil)}]++
}

func (m *runtimeMetrics) observeRecall(principalKind string, request MemoryRecallRequest, page MemoryRecallPage, err error, elapsed time.Duration) {
	principalKind = metricPrincipalKind(principalKind)
	mode := metricRecallMode(page.RetrievalMode)
	result := metricResult(err == nil)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.memoryRecalls[recallMetricLabels{Mode: mode, PrincipalKind: principalKind, Result: result}]++
	observeHistogram(m.memoryRecallTime, recallDurationLabels{Mode: mode, PrincipalKind: principalKind}, elapsed.Seconds(), latencyBuckets)
	vectorRequested := request.VectorProfileID != "" && len(request.QueryVector) > 0
	if vectorRequested {
		coverage := "unknown"
		if err == nil {
			coverage = metricCoverage(page.VectorCoverage)
		}
		m.vectorSearches[vectorSearchMetricLabels{Coverage: coverage, Result: result}]++
	}
	if err != nil {
		return
	}
	observeHistogram(m.memoryRecallHits, mode, float64(len(page.Hits)), hitBuckets)
	if !vectorRequested {
		return
	}
	if page.Degraded && mode == "lexical" {
		reason := metricVectorFallbackReason(page.DegradedReason)
		m.vectorFallbacks[vectorFallbackMetricLabels{Reason: reason}]++
	}
}

func observeHistogram[K comparable](target map[K]*metricHistogram, key K, value float64, bounds []float64) {
	histogram := target[key]
	if histogram == nil {
		histogram = &metricHistogram{Buckets: make([]uint64, len(bounds))}
		target[key] = histogram
	}
	histogram.Count++
	histogram.Sum += value
	for i, bound := range bounds {
		if value <= bound {
			histogram.Buckets[i]++
		}
	}
}

func (m *runtimeMetrics) writePrometheus(w io.Writer) {
	snapshot := m.snapshot()
	snapshot.writePrometheusSnapshot(w)
}

func (m *runtimeMetrics) snapshot() *runtimeMetrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	return &runtimeMetrics{
		httpInFlight:       m.httpInFlight,
		httpRequests:       maps.Clone(m.httpRequests),
		httpLatency:        cloneHistogramMap(m.httpLatency),
		memoryOperations:   maps.Clone(m.memoryOperations),
		memoryRecalls:      maps.Clone(m.memoryRecalls),
		memoryRecallTime:   cloneHistogramMap(m.memoryRecallTime),
		memoryRecallHits:   cloneHistogramMap(m.memoryRecallHits),
		vectorSearches:     maps.Clone(m.vectorSearches),
		vectorFallbacks:    maps.Clone(m.vectorFallbacks),
		curationOperations: maps.Clone(m.curationOperations),
		planLimitRejects:   maps.Clone(m.planLimitRejects),
		secretLimitRejects: maps.Clone(m.secretLimitRejects),
	}
}

func cloneHistogramMap[K comparable](source map[K]*metricHistogram) map[K]*metricHistogram {
	cloned := make(map[K]*metricHistogram, len(source))
	for key, histogram := range source {
		if histogram == nil {
			continue
		}
		cloned[key] = &metricHistogram{
			Buckets: append([]uint64(nil), histogram.Buckets...),
			Count:   histogram.Count,
			Sum:     histogram.Sum,
		}
	}
	return cloned
}

func (m *runtimeMetrics) writePrometheusSnapshot(w io.Writer) {
	_, _ = fmt.Fprintln(w, "# HELP witself_up 1 if the witself-server process is up.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_up gauge")
	_, _ = fmt.Fprintln(w, "witself_up 1")
	_, _ = fmt.Fprintln(w, "# HELP witself_http_in_flight_requests Current API requests in flight.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_http_in_flight_requests gauge")
	_, _ = fmt.Fprintf(w, "witself_http_in_flight_requests %d\n", m.httpInFlight)

	writeCounterMap(w, "witself_http_requests_total", "API requests by bounded route template, method, status class, and result.", m.httpRequests, func(k httpMetricLabels) string {
		return labels("method", k.Method, "route", k.Route, "status_class", k.StatusClass, "result", k.Result)
	})
	writeHistogramMap(w, "witself_http_request_duration_seconds", "API request duration by bounded route template and method.", m.httpLatency, latencyBuckets, func(k httpDurationLabels) string {
		return labels("method", k.Method, "route", k.Route)
	})
	writeCounterMap(w, "witself_memory_operations_total", "Narrative-memory domain operations by operation, principal kind, and result.", m.memoryOperations, func(k memoryOperationMetricLabels) string {
		return labels("operation", k.Operation, "principal_kind", k.PrincipalKind, "result", k.Result)
	})
	writeCounterMap(w, "witself_memory_recalls_total", "Narrative-memory recall requests by mode, principal kind, and result.", m.memoryRecalls, func(k recallMetricLabels) string {
		return labels("mode", k.Mode, "principal_kind", k.PrincipalKind, "result", k.Result)
	})
	writeHistogramMap(w, "witself_memory_recall_duration_seconds", "Narrative-memory recall duration by mode and principal kind.", m.memoryRecallTime, latencyBuckets, func(k recallDurationLabels) string {
		return labels("mode", k.Mode, "principal_kind", k.PrincipalKind)
	})
	writeHistogramMap(w, "witself_memory_recall_hits", "Narrative-memory recall result count by mode.", m.memoryRecallHits, hitBuckets, func(mode string) string {
		return labels("mode", mode)
	})
	writeCounterMap(w, "witself_memory_vector_searches_total", "Client-vector searches by bounded coverage class and result.", m.vectorSearches, func(k vectorSearchMetricLabels) string {
		return labels("coverage", k.Coverage, "result", k.Result)
	})
	writeCounterMap(w, "witself_memory_vector_fallbacks_total", "Hybrid requests that fell back to lexical ranking.", m.vectorFallbacks, func(k vectorFallbackMetricLabels) string {
		return labels("reason", k.Reason)
	})
	writeCounterMap(w, "witself_memory_curation_operations_total", "Completed memory-curation domain calls by operation and result; idempotent replays are counted as calls.", m.curationOperations, func(k operationMetricLabels) string {
		return labels("operation", k.Operation, "result", k.Result)
	})
	writeCounterMap(w, "witself_plan_limit_rejections_total", "Realm and agent create refusals by bounded plan-limit dimension and operation.", m.planLimitRejects, func(key limitMetricLabels) string {
		return labels("limit_dimension", key.LimitDimension, "operation", key.Operation)
	})
	writeCounterMap(w, "witself_secret_limit_rejections_total", "Stored-secret create refusals by bounded limit dimension and operation.", m.secretLimitRejects, func(key limitMetricLabels) string {
		return labels("limit_dimension", key.LimitDimension, "operation", key.Operation)
	})
}

func writeCounterMap[K comparable](w io.Writer, name, help string, values map[K]uint64, labeler func(K) string) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	type row struct {
		labels string
		value  uint64
	}
	rows := make([]row, 0, len(values))
	for key, value := range values {
		rows = append(rows, row{labels: labeler(key), value: value})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].labels < rows[j].labels })
	for _, row := range rows {
		_, _ = fmt.Fprintf(w, "%s%s %d\n", name, row.labels, row.value)
	}
}

func writeHistogramMap[K comparable](w io.Writer, name, help string, values map[K]*metricHistogram, bounds []float64, labeler func(K) string) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	type row struct {
		labels string
		hist   *metricHistogram
	}
	rows := make([]row, 0, len(values))
	for key, histogram := range values {
		rows = append(rows, row{labels: labeler(key), hist: histogram})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].labels < rows[j].labels })
	for _, row := range rows {
		for i, bound := range bounds {
			_, _ = fmt.Fprintf(w, "%s_bucket%s %d\n", name, appendLabel(row.labels, "le", strconv.FormatFloat(bound, 'g', -1, 64)), row.hist.Buckets[i])
		}
		_, _ = fmt.Fprintf(w, "%s_bucket%s %d\n", name, appendLabel(row.labels, "le", "+Inf"), row.hist.Count)
		_, _ = fmt.Fprintf(w, "%s_sum%s %s\n", name, row.labels, strconv.FormatFloat(row.hist.Sum, 'g', -1, 64))
		_, _ = fmt.Fprintf(w, "%s_count%s %d\n", name, row.labels, row.hist.Count)
	}
}

func labels(pairs ...string) string {
	parts := make([]string, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, pairs[i]+"=\""+escapeMetricLabel(pairs[i+1])+"\"")
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func appendLabel(existing, key, value string) string {
	return strings.TrimSuffix(existing, "}") + "," + key + "=\"" + escapeMetricLabel(value) + "\"}"
}

func escapeMetricLabel(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\n", "\\n")
	return strings.ReplaceAll(value, "\"", "\\\"")
}

func metricMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		return method
	default:
		return "OTHER"
	}
}

func metricRoute(pattern string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "unmatched"
	}
	if fields := strings.Fields(pattern); len(fields) == 2 && strings.HasPrefix(fields[1], "/") {
		return fields[1]
	}
	if strings.HasPrefix(pattern, "/") {
		return pattern
	}
	return "unmatched"
}

func metricResult(ok bool) string {
	if ok {
		return "success"
	}
	return "error"
}

func metricPrincipalKind(kind string) string {
	switch kind {
	case PrincipalKindAgent, PrincipalKindOperator:
		return kind
	default:
		return "unknown"
	}
}

func metricVectorFallbackReason(reason string) string {
	switch reason {
	case "no_compatible_vectors", "candidate_budget_exceeded":
		return reason
	default:
		return "other"
	}
}

func metricRecallMode(mode string) string {
	switch mode {
	case "lexical", "hybrid":
		return mode
	default:
		return "unknown"
	}
}

func metricCoverage(coverage float64) string {
	switch {
	case coverage >= 1:
		return "full"
	case coverage > 0:
		return "partial"
	default:
		return "none"
	}
}

type metricsResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *metricsResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *metricsResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

// Unwrap preserves optional response-controller behavior without inspecting or
// retaining response bodies.
func (w *metricsResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
