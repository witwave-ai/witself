package worker

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// JobResult is the bounded result label for a supervised job-loop exit.
type JobResult string

const (
	// JobResultCompleted means a job loop returned nil without cancellation.
	JobResultCompleted JobResult = "completed"
	// JobResultError means a job loop returned an unexpected error.
	JobResultError JobResult = "error"
	// JobResultCanceled means a job loop stopped after runtime cancellation.
	JobResultCanceled JobResult = "canceled"
)

// RetentionResult is the bounded result label for a retention or cleanup batch.
type RetentionResult string

const (
	// RetentionResultSuccess means a bounded batch found and processed work.
	RetentionResultSuccess RetentionResult = "success"
	// RetentionResultNoWork means a bounded batch completed with no due work.
	RetentionResultNoWork RetentionResult = "no_work"
	// RetentionResultError means a bounded batch returned an error.
	RetentionResultError RetentionResult = "error"
)

// RetentionCounts contains value-free counts from one transcript-retention
// attempt. It intentionally has no account, realm, agent, or transcript fields.
type RetentionCounts struct {
	Scanned                int64
	SkippedLocked          int64
	Eligible               int64
	Deleted                int64
	DeferredEvidence       int64
	DeferredCuration       int64
	ReleasedCurationInputs int64
	DeletedCurationCursors int64
	ScanCapped             bool
	EligibleScanCapped     bool
	DeferredScanCapped     bool
}

// MessageRetentionCounts contains value-free whole-thread cleanup counts.
type MessageRetentionCounts struct {
	Scanned          int64
	SkippedLocked    int64
	EligibleThreads  int64
	DeletedThreads   int64
	DeletedMessages  int64
	DeferredEvidence int64
	DeferredActive   int64
	DeferredLocked   int64
	DeferredOversize int64
	DeferredBudget   int64
	RepairedActivity int64
	ScanCapped       bool
}

// AgentEmailRetentionCounts contains value-free agent-email cleanup counts.
type AgentEmailRetentionCounts struct {
	Scanned               int64
	SkippedLocked         int64
	Eligible              int64
	Deleted               int64
	DeletedRawBytes       int64
	DeferredActive        int64
	DeferredLocked        int64
	DeferredOversize      int64
	DeferredBudget        int64
	ClearedDuplicateLinks int64
	DeletedCanaryProofs   int64
	ScannedOutbound       int64
	EligibleOutbound      int64
	DeletedOutbound       int64
	DeletedOutboundBytes  int64
	ScannedProviderEvents int64
	DeletedProviderEvents int64
	ScannedSuppressions   int64
	EligibleSuppressions  int64
	DeletedSuppressions   int64
	ScanCapped            bool
}

// AccountPurgeCounts contains value-free closed-account purge counts. Deleted
// rows are aggregated across tables so table or tenant data cannot create
// metric labels.
type AccountPurgeCounts struct {
	Scanned                     int64
	SkippedLocked               int64
	Eligible                    int64
	PurgedAccounts              int64
	DeletedRows                 int64
	DeferredVaultLifecycle      int64
	AttachmentInvariantFailures int64
	ProvisionReceiptScrubs      int64
}

// AgentEmailOutboundCounts contains value-free send-worker outcomes. It has
// no account, realm, agent, recipient, message, or provider identifiers.
type AgentEmailOutboundCounts struct {
	Claimed           int64
	Accepted          int64
	Delivered         int64
	Retried           int64
	Bounced           int64
	Rejected          int64
	Failed            int64
	Ambiguous         int64
	Canceled          int64
	ExpiredReconciled int64
}

type jobMetric struct {
	Running  bool
	Failures uint64
	Exits    map[JobResult]uint64
}

type retentionBatchLabels struct {
	Mode   string
	Result RetentionResult
}

type retentionItemLabels struct {
	Mode string
	Kind string
}

// Metrics is a dependency-free, privacy-bounded process registry.
type Metrics struct {
	mu sync.Mutex

	jobs                                   map[string]*jobMetric
	retentionBatches                       map[retentionBatchLabels]uint64
	retentionItems                         map[retentionItemLabels]uint64
	retentionLastSuccess                   map[string]float64
	messageRetentionBatches                map[retentionBatchLabels]uint64
	messageRetentionItems                  map[retentionItemLabels]uint64
	messageRetentionLastSuccess            map[string]float64
	emailRetentionBatches                  map[retentionBatchLabels]uint64
	emailRetentionItems                    map[retentionItemLabels]uint64
	emailRetentionLastSuccess              map[string]float64
	accountPurgeBatches                    map[retentionBatchLabels]uint64
	accountPurgeItems                      map[retentionItemLabels]uint64
	accountPurgeLastSuccess                map[string]float64
	messageRateBucketCleanupBatches        map[RetentionResult]uint64
	messageRateBucketCleanupDeletedRows    uint64
	messageRateBucketCleanupLastSuccess    float64
	agentEmailRateBucketCleanupBatches     map[RetentionResult]uint64
	agentEmailRateBucketCleanupDeletedRows uint64
	agentEmailRateBucketCleanupLastSuccess float64
	agentEmailOutboundBatches              map[RetentionResult]uint64
	agentEmailOutboundItems                map[string]uint64
	agentEmailOutboundLastSuccess          float64
	now                                    func() time.Time
}

func newMetrics() *Metrics {
	return &Metrics{
		jobs:                        make(map[string]*jobMetric),
		retentionBatches:            make(map[retentionBatchLabels]uint64),
		retentionItems:              make(map[retentionItemLabels]uint64),
		retentionLastSuccess:        make(map[string]float64),
		messageRetentionBatches:     make(map[retentionBatchLabels]uint64),
		messageRetentionItems:       make(map[retentionItemLabels]uint64),
		messageRetentionLastSuccess: make(map[string]float64),
		emailRetentionBatches:       make(map[retentionBatchLabels]uint64),
		emailRetentionItems:         make(map[retentionItemLabels]uint64),
		emailRetentionLastSuccess:   make(map[string]float64),
		accountPurgeBatches:         make(map[retentionBatchLabels]uint64),
		accountPurgeItems:           make(map[retentionItemLabels]uint64),
		accountPurgeLastSuccess:     make(map[string]float64),
		messageRateBucketCleanupBatches: map[RetentionResult]uint64{
			RetentionResultSuccess: 0,
			RetentionResultNoWork:  0,
			RetentionResultError:   0,
		},
		agentEmailRateBucketCleanupBatches: map[RetentionResult]uint64{
			RetentionResultSuccess: 0,
			RetentionResultNoWork:  0,
			RetentionResultError:   0,
		},
		agentEmailOutboundBatches: map[RetentionResult]uint64{
			RetentionResultSuccess: 0,
			RetentionResultNoWork:  0,
			RetentionResultError:   0,
		},
		agentEmailOutboundItems: make(map[string]uint64),
		now:                     time.Now,
	}
}

func (m *Metrics) registerJob(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.jobs[name]; !exists {
		m.jobs[name] = &jobMetric{Exits: make(map[JobResult]uint64)}
	}
}

func (m *Metrics) setJobRunning(name string, running bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if metric := m.jobs[name]; metric != nil {
		metric.Running = running
	}
}

func (m *Metrics) observeJobExit(name string, result JobResult) {
	if result != JobResultCompleted && result != JobResultError && result != JobResultCanceled {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if metric := m.jobs[name]; metric != nil {
		metric.Running = false
		metric.Exits[result]++
	}
}

// RecordJobFailure records a recoverable job-loop error reported through a
// callback. Unknown names are ignored so runtime data cannot create labels.
func (m *Metrics) RecordJobFailure(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if metric := m.jobs[name]; metric != nil {
		metric.Failures++
	}
}

// ObserveRetentionBatch records one value-free transcript-retention attempt.
// Mode and result are closed enums; invalid values are ignored.
func (m *Metrics) ObserveRetentionBatch(mode string, result RetentionResult, counts RetentionCounts) {
	if mode != "preview" && mode != "enforce" {
		return
	}
	if result != RetentionResultSuccess && result != RetentionResultNoWork && result != RetentionResultError {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.retentionBatches[retentionBatchLabels{Mode: mode, Result: result}]++
	if result == RetentionResultSuccess || result == RetentionResultNoWork {
		m.retentionLastSuccess[mode] = float64(m.now().Unix())
	}
	for kind, value := range map[string]int64{
		"scanned":                  counts.Scanned,
		"skipped_locked":           counts.SkippedLocked,
		"eligible":                 counts.Eligible,
		"deleted":                  counts.Deleted,
		"deferred_evidence":        counts.DeferredEvidence,
		"deferred_curation":        counts.DeferredCuration,
		"released_curation_inputs": counts.ReleasedCurationInputs,
		"deleted_curation_cursors": counts.DeletedCurationCursors,
	} {
		if value > 0 {
			m.retentionItems[retentionItemLabels{Mode: mode, Kind: kind}] += uint64(value)
		}
	}
	for kind, capped := range map[string]bool{
		"scan_capped_batches":          counts.ScanCapped,
		"eligible_scan_capped_batches": counts.EligibleScanCapped,
		"deferred_scan_capped_batches": counts.DeferredScanCapped,
	} {
		if capped {
			m.retentionItems[retentionItemLabels{Mode: mode, Kind: kind}]++
		}
	}
}

// ObserveMessageRetentionBatch records one value-free whole-thread retention
// attempt. Mode, result, and item kinds are process-owned bounded labels.
func (m *Metrics) ObserveMessageRetentionBatch(
	mode string,
	result RetentionResult,
	counts MessageRetentionCounts,
) {
	if mode != "preview" && mode != "enforce" {
		return
	}
	if result != RetentionResultSuccess &&
		result != RetentionResultNoWork &&
		result != RetentionResultError {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.messageRetentionBatches[retentionBatchLabels{Mode: mode, Result: result}]++
	if result == RetentionResultSuccess || result == RetentionResultNoWork {
		m.messageRetentionLastSuccess[mode] = float64(m.now().Unix())
	}
	for kind, value := range map[string]int64{
		"scanned":           counts.Scanned,
		"skipped_locked":    counts.SkippedLocked,
		"eligible_threads":  counts.EligibleThreads,
		"deleted_threads":   counts.DeletedThreads,
		"deleted_messages":  counts.DeletedMessages,
		"deferred_evidence": counts.DeferredEvidence,
		"deferred_active":   counts.DeferredActive,
		"deferred_locked":   counts.DeferredLocked,
		"deferred_oversize": counts.DeferredOversize,
		"deferred_budget":   counts.DeferredBudget,
		"repaired_activity": counts.RepairedActivity,
	} {
		if value > 0 {
			m.messageRetentionItems[retentionItemLabels{Mode: mode, Kind: kind}] += uint64(value)
		}
	}
	if counts.ScanCapped {
		m.messageRetentionItems[retentionItemLabels{
			Mode: mode,
			Kind: "scan_capped_batches",
		}]++
	}
}

// ObserveAgentEmailRetentionBatch records one value-free agent-email
// retention attempt. Mode, result, and item kinds are closed process enums.
func (m *Metrics) ObserveAgentEmailRetentionBatch(
	mode string,
	result RetentionResult,
	counts AgentEmailRetentionCounts,
) {
	if mode != "preview" && mode != "enforce" {
		return
	}
	if result != RetentionResultSuccess &&
		result != RetentionResultNoWork &&
		result != RetentionResultError {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.emailRetentionBatches[retentionBatchLabels{Mode: mode, Result: result}]++
	if result == RetentionResultSuccess || result == RetentionResultNoWork {
		m.emailRetentionLastSuccess[mode] = float64(m.now().Unix())
	}
	for kind, value := range map[string]int64{
		"scanned":                 counts.Scanned,
		"skipped_locked":          counts.SkippedLocked,
		"eligible":                counts.Eligible,
		"deleted":                 counts.Deleted,
		"deleted_raw_bytes":       counts.DeletedRawBytes,
		"deferred_active":         counts.DeferredActive,
		"deferred_locked":         counts.DeferredLocked,
		"deferred_oversize":       counts.DeferredOversize,
		"deferred_budget":         counts.DeferredBudget,
		"cleared_duplicate_links": counts.ClearedDuplicateLinks,
		"deleted_canary_proofs":   counts.DeletedCanaryProofs,
		"scanned_outbound":        counts.ScannedOutbound,
		"eligible_outbound":       counts.EligibleOutbound,
		"deleted_outbound":        counts.DeletedOutbound,
		"deleted_outbound_bytes":  counts.DeletedOutboundBytes,
		"scanned_provider_events": counts.ScannedProviderEvents,
		"deleted_provider_events": counts.DeletedProviderEvents,
		"scanned_suppressions":    counts.ScannedSuppressions,
		"eligible_suppressions":   counts.EligibleSuppressions,
		"deleted_suppressions":    counts.DeletedSuppressions,
	} {
		if value > 0 {
			m.emailRetentionItems[retentionItemLabels{
				Mode: mode,
				Kind: kind,
			}] += uint64(value)
		}
	}
	if counts.ScanCapped {
		m.emailRetentionItems[retentionItemLabels{
			Mode: mode,
			Kind: "scan_capped_batches",
		}]++
	}
}

// ObserveAccountPurgeBatch records one value-free closed-account purge
// attempt. Mode, result, and item kinds are process-owned bounded labels.
func (m *Metrics) ObserveAccountPurgeBatch(
	mode string,
	result RetentionResult,
	counts AccountPurgeCounts,
) {
	if mode != "preview" && mode != "enforce" {
		return
	}
	if result != RetentionResultSuccess &&
		result != RetentionResultNoWork &&
		result != RetentionResultError {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.accountPurgeBatches[retentionBatchLabels{Mode: mode, Result: result}]++
	if result == RetentionResultSuccess || result == RetentionResultNoWork {
		m.accountPurgeLastSuccess[mode] = float64(m.now().Unix())
	}
	for kind, value := range map[string]int64{
		"scanned":                       counts.Scanned,
		"skipped_locked":                counts.SkippedLocked,
		"eligible":                      counts.Eligible,
		"purged_accounts":               counts.PurgedAccounts,
		"deleted_rows":                  counts.DeletedRows,
		"deferred_vault_lifecycle":      counts.DeferredVaultLifecycle,
		"attachment_invariant_failures": counts.AttachmentInvariantFailures,
		"provision_receipt_scrubs":      counts.ProvisionReceiptScrubs,
	} {
		if value > 0 {
			m.accountPurgeItems[retentionItemLabels{Mode: mode, Kind: kind}] += uint64(value)
		}
	}
}

// ObserveMessageRateBucketCleanupBatch records one value-free rate-bucket
// cleanup attempt. Result is a closed process enum and deleted is a count only.
func (m *Metrics) ObserveMessageRateBucketCleanupBatch(
	result RetentionResult,
	deleted int64,
) {
	if result != RetentionResultSuccess &&
		result != RetentionResultNoWork &&
		result != RetentionResultError {
		return
	}
	if deleted < 0 ||
		(result == RetentionResultSuccess && deleted == 0) ||
		(result != RetentionResultSuccess && deleted != 0) {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.messageRateBucketCleanupBatches[result]++
	if result == RetentionResultSuccess || result == RetentionResultNoWork {
		m.messageRateBucketCleanupLastSuccess = float64(m.now().Unix())
	}
	if deleted > 0 {
		m.messageRateBucketCleanupDeletedRows += uint64(deleted)
	}
}

// ObserveAgentEmailRateBucketCleanupBatch records one value-free email
// rate-bucket cleanup attempt. Result is a closed process enum and deleted is
// a count only.
func (m *Metrics) ObserveAgentEmailRateBucketCleanupBatch(
	result RetentionResult,
	deleted int64,
) {
	if result != RetentionResultSuccess &&
		result != RetentionResultNoWork &&
		result != RetentionResultError {
		return
	}
	if deleted < 0 ||
		(result == RetentionResultSuccess && deleted == 0) ||
		(result != RetentionResultSuccess && deleted != 0) {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.agentEmailRateBucketCleanupBatches[result]++
	if result == RetentionResultSuccess || result == RetentionResultNoWork {
		m.agentEmailRateBucketCleanupLastSuccess = float64(m.now().Unix())
	}
	if deleted > 0 {
		m.agentEmailRateBucketCleanupDeletedRows += uint64(deleted)
	}
}

// ObserveAgentEmailOutboundBatch records one bounded, value-free send-worker
// attempt. Invalid result/count combinations are ignored rather than creating
// data-controlled metric labels.
func (m *Metrics) ObserveAgentEmailOutboundBatch(
	result RetentionResult,
	counts AgentEmailOutboundCounts,
) {
	if result != RetentionResultSuccess &&
		result != RetentionResultNoWork &&
		result != RetentionResultError {
		return
	}
	values := map[string]int64{
		"claimed":            counts.Claimed,
		"accepted":           counts.Accepted,
		"delivered":          counts.Delivered,
		"retried":            counts.Retried,
		"bounced":            counts.Bounced,
		"rejected":           counts.Rejected,
		"failed":             counts.Failed,
		"ambiguous":          counts.Ambiguous,
		"canceled":           counts.Canceled,
		"expired_reconciled": counts.ExpiredReconciled,
	}
	hasItems := false
	for _, value := range values {
		if value < 0 {
			return
		}
		hasItems = hasItems || value > 0
	}
	if result == RetentionResultSuccess && !hasItems ||
		result != RetentionResultSuccess && hasItems {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.agentEmailOutboundBatches[result]++
	if result == RetentionResultSuccess || result == RetentionResultNoWork {
		m.agentEmailOutboundLastSuccess = float64(m.now().Unix())
	}
	for kind, value := range values {
		if value > 0 {
			m.agentEmailOutboundItems[kind] += uint64(value)
		}
	}
}

func (m *Metrics) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		m.writePrometheus(w)
	})
	return mux
}

func (m *Metrics) writePrometheus(w io.Writer) {
	m.mu.Lock()
	jobs := make(map[string]jobMetric, len(m.jobs))
	for name, metric := range m.jobs {
		exits := make(map[JobResult]uint64, len(metric.Exits))
		for result, value := range metric.Exits {
			exits[result] = value
		}
		jobs[name] = jobMetric{
			Running:  metric.Running,
			Failures: metric.Failures,
			Exits:    exits,
		}
	}
	batches := make(map[retentionBatchLabels]uint64, len(m.retentionBatches))
	for labels, value := range m.retentionBatches {
		batches[labels] = value
	}
	items := make(map[retentionItemLabels]uint64, len(m.retentionItems))
	for labels, value := range m.retentionItems {
		items[labels] = value
	}
	lastSuccess := make(map[string]float64, len(m.retentionLastSuccess))
	for mode, value := range m.retentionLastSuccess {
		lastSuccess[mode] = value
	}
	messageBatches := make(map[retentionBatchLabels]uint64, len(m.messageRetentionBatches))
	for labels, value := range m.messageRetentionBatches {
		messageBatches[labels] = value
	}
	messageItems := make(map[retentionItemLabels]uint64, len(m.messageRetentionItems))
	for labels, value := range m.messageRetentionItems {
		messageItems[labels] = value
	}
	messageLastSuccess := make(map[string]float64, len(m.messageRetentionLastSuccess))
	for mode, value := range m.messageRetentionLastSuccess {
		messageLastSuccess[mode] = value
	}
	emailBatches := make(map[retentionBatchLabels]uint64, len(m.emailRetentionBatches))
	for labels, value := range m.emailRetentionBatches {
		emailBatches[labels] = value
	}
	emailItems := make(map[retentionItemLabels]uint64, len(m.emailRetentionItems))
	for labels, value := range m.emailRetentionItems {
		emailItems[labels] = value
	}
	emailLastSuccess := make(map[string]float64, len(m.emailRetentionLastSuccess))
	for mode, value := range m.emailRetentionLastSuccess {
		emailLastSuccess[mode] = value
	}
	accountPurgeBatches := make(map[retentionBatchLabels]uint64, len(m.accountPurgeBatches))
	for labels, value := range m.accountPurgeBatches {
		accountPurgeBatches[labels] = value
	}
	accountPurgeItems := make(map[retentionItemLabels]uint64, len(m.accountPurgeItems))
	for labels, value := range m.accountPurgeItems {
		accountPurgeItems[labels] = value
	}
	accountPurgeLastSuccess := make(map[string]float64, len(m.accountPurgeLastSuccess))
	for mode, value := range m.accountPurgeLastSuccess {
		accountPurgeLastSuccess[mode] = value
	}
	messageRateBucketCleanupBatches := make(
		map[RetentionResult]uint64,
		len(m.messageRateBucketCleanupBatches),
	)
	for result, value := range m.messageRateBucketCleanupBatches {
		messageRateBucketCleanupBatches[result] = value
	}
	messageRateBucketCleanupDeletedRows := m.messageRateBucketCleanupDeletedRows
	messageRateBucketCleanupLastSuccess := m.messageRateBucketCleanupLastSuccess
	agentEmailRateBucketCleanupBatches := make(
		map[RetentionResult]uint64,
		len(m.agentEmailRateBucketCleanupBatches),
	)
	for result, value := range m.agentEmailRateBucketCleanupBatches {
		agentEmailRateBucketCleanupBatches[result] = value
	}
	agentEmailRateBucketCleanupDeletedRows := m.agentEmailRateBucketCleanupDeletedRows
	agentEmailRateBucketCleanupLastSuccess := m.agentEmailRateBucketCleanupLastSuccess
	agentEmailOutboundBatches := make(
		map[RetentionResult]uint64,
		len(m.agentEmailOutboundBatches),
	)
	for result, value := range m.agentEmailOutboundBatches {
		agentEmailOutboundBatches[result] = value
	}
	agentEmailOutboundItems := make(map[string]uint64, len(m.agentEmailOutboundItems))
	for kind, value := range m.agentEmailOutboundItems {
		agentEmailOutboundItems[kind] = value
	}
	agentEmailOutboundLastSuccess := m.agentEmailOutboundLastSuccess
	m.mu.Unlock()

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_up 1 if the witself-worker process is up.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_up gauge")
	_, _ = fmt.Fprintln(w, "witself_worker_up 1")

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_job_running 1 while a registered job loop is running.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_job_running gauge")
	jobNames := make([]string, 0, len(jobs))
	for name := range jobs {
		jobNames = append(jobNames, name)
	}
	sort.Strings(jobNames)
	for _, name := range jobNames {
		running := 0
		if jobs[name].Running {
			running = 1
		}
		_, _ = fmt.Fprintf(w, "witself_worker_job_running{job=%q} %d\n", name, running)
	}

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_job_failures_total Recoverable errors reported by a registered job loop.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_job_failures_total counter")
	for _, name := range jobNames {
		_, _ = fmt.Fprintf(w, "witself_worker_job_failures_total{job=%q} %d\n", name, jobs[name].Failures)
	}

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_job_exits_total Registered job-loop exits by bounded result.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_job_exits_total counter")
	for _, name := range jobNames {
		results := make([]string, 0, len(jobs[name].Exits))
		for result := range jobs[name].Exits {
			results = append(results, string(result))
		}
		sort.Strings(results)
		for _, result := range results {
			_, _ = fmt.Fprintf(w,
				"witself_worker_job_exits_total{job=%q,result=%q} %d\n",
				name, result, jobs[name].Exits[JobResult(result)])
		}
	}

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_retention_batches_total Transcript-retention batches by bounded mode and result.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_retention_batches_total counter")
	batchLabels := make([]retentionBatchLabels, 0, len(batches))
	for labels := range batches {
		batchLabels = append(batchLabels, labels)
	}
	sort.Slice(batchLabels, func(i, j int) bool {
		if batchLabels[i].Mode != batchLabels[j].Mode {
			return batchLabels[i].Mode < batchLabels[j].Mode
		}
		return batchLabels[i].Result < batchLabels[j].Result
	})
	for _, labels := range batchLabels {
		_, _ = fmt.Fprintf(w,
			"witself_worker_retention_batches_total{mode=%q,result=%q} %d\n",
			labels.Mode, labels.Result, batches[labels])
	}

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_retention_items_total Value-free transcript-retention counts by bounded kind.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_retention_items_total counter")
	itemLabels := make([]retentionItemLabels, 0, len(items))
	for labels := range items {
		itemLabels = append(itemLabels, labels)
	}
	sort.Slice(itemLabels, func(i, j int) bool {
		if itemLabels[i].Mode != itemLabels[j].Mode {
			return itemLabels[i].Mode < itemLabels[j].Mode
		}
		return itemLabels[i].Kind < itemLabels[j].Kind
	})
	for _, labels := range itemLabels {
		_, _ = fmt.Fprintf(w,
			"witself_worker_retention_items_total{mode=%q,kind=%q} %d\n",
			labels.Mode, labels.Kind, items[labels])
	}

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_retention_last_success_timestamp_seconds Unix timestamp of the last successful or no-work retention batch.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_retention_last_success_timestamp_seconds gauge")
	modes := make([]string, 0, len(lastSuccess))
	for mode := range lastSuccess {
		modes = append(modes, mode)
	}
	sort.Strings(modes)
	for _, mode := range modes {
		_, _ = fmt.Fprintf(w,
			"witself_worker_retention_last_success_timestamp_seconds{mode=%q} %.0f\n",
			mode, lastSuccess[mode])
	}

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_message_retention_batches_total Message-retention batches by bounded mode and result.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_message_retention_batches_total counter")
	messageBatchLabels := sortedRetentionBatchLabels(messageBatches)
	for _, labels := range messageBatchLabels {
		_, _ = fmt.Fprintf(w,
			"witself_worker_message_retention_batches_total{mode=%q,result=%q} %d\n",
			labels.Mode, labels.Result, messageBatches[labels])
	}

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_message_retention_items_total Value-free message-retention counts by bounded kind.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_message_retention_items_total counter")
	messageItemLabels := sortedRetentionItemLabels(messageItems)
	for _, labels := range messageItemLabels {
		_, _ = fmt.Fprintf(w,
			"witself_worker_message_retention_items_total{mode=%q,kind=%q} %d\n",
			labels.Mode, labels.Kind, messageItems[labels])
	}

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_message_retention_last_success_timestamp_seconds Unix timestamp of the last successful or no-work message-retention batch.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_message_retention_last_success_timestamp_seconds gauge")
	messageModes := make([]string, 0, len(messageLastSuccess))
	for mode := range messageLastSuccess {
		messageModes = append(messageModes, mode)
	}
	sort.Strings(messageModes)
	for _, mode := range messageModes {
		_, _ = fmt.Fprintf(w,
			"witself_worker_message_retention_last_success_timestamp_seconds{mode=%q} %.0f\n",
			mode, messageLastSuccess[mode])
	}

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_agent_email_retention_batches_total Agent-email-retention batches by bounded mode and result.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_agent_email_retention_batches_total counter")
	emailBatchLabels := sortedRetentionBatchLabels(emailBatches)
	for _, labels := range emailBatchLabels {
		_, _ = fmt.Fprintf(w,
			"witself_worker_agent_email_retention_batches_total{mode=%q,result=%q} %d\n",
			labels.Mode, labels.Result, emailBatches[labels])
	}

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_agent_email_retention_items_total Value-free agent-email-retention counts by bounded kind.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_agent_email_retention_items_total counter")
	emailItemLabels := sortedRetentionItemLabels(emailItems)
	for _, labels := range emailItemLabels {
		_, _ = fmt.Fprintf(w,
			"witself_worker_agent_email_retention_items_total{mode=%q,kind=%q} %d\n",
			labels.Mode, labels.Kind, emailItems[labels])
	}

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_agent_email_retention_last_success_timestamp_seconds Unix timestamp of the last successful or no-work agent-email-retention batch.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_agent_email_retention_last_success_timestamp_seconds gauge")
	emailModes := make([]string, 0, len(emailLastSuccess))
	for mode := range emailLastSuccess {
		emailModes = append(emailModes, mode)
	}
	sort.Strings(emailModes)
	for _, mode := range emailModes {
		_, _ = fmt.Fprintf(w,
			"witself_worker_agent_email_retention_last_success_timestamp_seconds{mode=%q} %.0f\n",
			mode, emailLastSuccess[mode])
	}

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_account_purge_batches_total Closed-account purge batches by bounded mode and result.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_account_purge_batches_total counter")
	accountPurgeBatchLabels := sortedRetentionBatchLabels(accountPurgeBatches)
	for _, labels := range accountPurgeBatchLabels {
		_, _ = fmt.Fprintf(w,
			"witself_worker_account_purge_batches_total{mode=%q,result=%q} %d\n",
			labels.Mode, labels.Result, accountPurgeBatches[labels])
	}

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_account_purge_items_total Value-free closed-account purge counts by bounded kind.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_account_purge_items_total counter")
	accountPurgeItemLabels := sortedRetentionItemLabels(accountPurgeItems)
	for _, labels := range accountPurgeItemLabels {
		_, _ = fmt.Fprintf(w,
			"witself_worker_account_purge_items_total{mode=%q,kind=%q} %d\n",
			labels.Mode, labels.Kind, accountPurgeItems[labels])
	}

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_account_purge_last_success_timestamp_seconds Unix timestamp of the last successful or no-work closed-account purge batch.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_account_purge_last_success_timestamp_seconds gauge")
	accountPurgeModes := make([]string, 0, len(accountPurgeLastSuccess))
	for mode := range accountPurgeLastSuccess {
		accountPurgeModes = append(accountPurgeModes, mode)
	}
	sort.Strings(accountPurgeModes)
	for _, mode := range accountPurgeModes {
		_, _ = fmt.Fprintf(w,
			"witself_worker_account_purge_last_success_timestamp_seconds{mode=%q} %.0f\n",
			mode, accountPurgeLastSuccess[mode])
	}

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_agent_email_outbound_batches_total Outbound agent-email worker batches by bounded result.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_agent_email_outbound_batches_total counter")
	outboundResults := make([]string, 0, len(agentEmailOutboundBatches))
	for result := range agentEmailOutboundBatches {
		outboundResults = append(outboundResults, string(result))
	}
	sort.Strings(outboundResults)
	for _, result := range outboundResults {
		_, _ = fmt.Fprintf(
			w,
			"witself_worker_agent_email_outbound_batches_total{result=%q} %d\n",
			result,
			agentEmailOutboundBatches[RetentionResult(result)],
		)
	}

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_agent_email_outbound_items_total Value-free outbound agent-email outcomes by bounded kind.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_agent_email_outbound_items_total counter")
	outboundKinds := make([]string, 0, len(agentEmailOutboundItems))
	for kind := range agentEmailOutboundItems {
		outboundKinds = append(outboundKinds, kind)
	}
	sort.Strings(outboundKinds)
	for _, kind := range outboundKinds {
		_, _ = fmt.Fprintf(
			w,
			"witself_worker_agent_email_outbound_items_total{kind=%q} %d\n",
			kind,
			agentEmailOutboundItems[kind],
		)
	}

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_agent_email_outbound_last_success_timestamp_seconds Unix timestamp of the last successful or no-work outbound agent-email batch.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_agent_email_outbound_last_success_timestamp_seconds gauge")
	_, _ = fmt.Fprintf(
		w,
		"witself_worker_agent_email_outbound_last_success_timestamp_seconds %.0f\n",
		agentEmailOutboundLastSuccess,
	)

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_message_rate_bucket_cleanup_batches_total Message-rate-bucket cleanup batches by bounded result.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_message_rate_bucket_cleanup_batches_total counter")
	cleanupResults := make([]string, 0, len(messageRateBucketCleanupBatches))
	for result := range messageRateBucketCleanupBatches {
		cleanupResults = append(cleanupResults, string(result))
	}
	sort.Strings(cleanupResults)
	for _, result := range cleanupResults {
		_, _ = fmt.Fprintf(
			w,
			"witself_worker_message_rate_bucket_cleanup_batches_total{result=%q} %d\n",
			result,
			messageRateBucketCleanupBatches[RetentionResult(result)],
		)
	}

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_message_rate_bucket_cleanup_deleted_rows_total Stale message-rate bucket rows deleted.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_message_rate_bucket_cleanup_deleted_rows_total counter")
	_, _ = fmt.Fprintf(
		w,
		"witself_worker_message_rate_bucket_cleanup_deleted_rows_total %d\n",
		messageRateBucketCleanupDeletedRows,
	)

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_message_rate_bucket_cleanup_last_success_timestamp_seconds Unix timestamp of the last successful or no-work message-rate-bucket cleanup batch.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_message_rate_bucket_cleanup_last_success_timestamp_seconds gauge")
	_, _ = fmt.Fprintf(
		w,
		"witself_worker_message_rate_bucket_cleanup_last_success_timestamp_seconds %.0f\n",
		messageRateBucketCleanupLastSuccess,
	)

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_agent_email_rate_bucket_cleanup_batches_total Agent-email-rate-bucket cleanup batches by bounded result.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_agent_email_rate_bucket_cleanup_batches_total counter")
	agentEmailCleanupResults := make([]string, 0, len(agentEmailRateBucketCleanupBatches))
	for result := range agentEmailRateBucketCleanupBatches {
		agentEmailCleanupResults = append(agentEmailCleanupResults, string(result))
	}
	sort.Strings(agentEmailCleanupResults)
	for _, result := range agentEmailCleanupResults {
		_, _ = fmt.Fprintf(
			w,
			"witself_worker_agent_email_rate_bucket_cleanup_batches_total{result=%q} %d\n",
			result,
			agentEmailRateBucketCleanupBatches[RetentionResult(result)],
		)
	}

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_agent_email_rate_bucket_cleanup_deleted_rows_total Stale agent-email rate-bucket rows deleted.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_agent_email_rate_bucket_cleanup_deleted_rows_total counter")
	_, _ = fmt.Fprintf(
		w,
		"witself_worker_agent_email_rate_bucket_cleanup_deleted_rows_total %d\n",
		agentEmailRateBucketCleanupDeletedRows,
	)

	_, _ = fmt.Fprintln(w, "# HELP witself_worker_agent_email_rate_bucket_cleanup_last_success_timestamp_seconds Unix timestamp of the last successful or no-work agent-email-rate-bucket cleanup batch.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_agent_email_rate_bucket_cleanup_last_success_timestamp_seconds gauge")
	_, _ = fmt.Fprintf(
		w,
		"witself_worker_agent_email_rate_bucket_cleanup_last_success_timestamp_seconds %.0f\n",
		agentEmailRateBucketCleanupLastSuccess,
	)
}

func sortedRetentionBatchLabels(values map[retentionBatchLabels]uint64) []retentionBatchLabels {
	labels := make([]retentionBatchLabels, 0, len(values))
	for value := range values {
		labels = append(labels, value)
	}
	sort.Slice(labels, func(i, j int) bool {
		if labels[i].Mode != labels[j].Mode {
			return labels[i].Mode < labels[j].Mode
		}
		return labels[i].Result < labels[j].Result
	})
	return labels
}

func sortedRetentionItemLabels(values map[retentionItemLabels]uint64) []retentionItemLabels {
	labels := make([]retentionItemLabels, 0, len(values))
	for value := range values {
		labels = append(labels, value)
	}
	sort.Slice(labels, func(i, j int) bool {
		if labels[i].Mode != labels[j].Mode {
			return labels[i].Mode < labels[j].Mode
		}
		return labels[i].Kind < labels[j].Kind
	})
	return labels
}
