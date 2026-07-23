package cpserver

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/witwave-ai/witself/internal/billing/lifecycle"
	"github.com/witwave-ai/witself/internal/client"
)

const defaultAccountPageSize = 100
const defaultAccountPagesPerRun = 1
const maxPlanLifecycleTickAccounts = 100
const planLifecycleAccountConcurrency = 8
const planLifecycleTickTimeout = 210 * time.Second

var planLifecycleAccountIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// AccountPage is one bounded page from the active account directory.
type AccountPage struct {
	AccountIDs []string
	NextCursor string
}

// AccountLister pages over directory-authoritative active accounts. Cursors
// are opaque and scoped to a single scan.
type AccountLister interface {
	ListActiveAccounts(
		ctx context.Context,
		cursor string,
		limit int,
	) (AccountPage, error)
}

type bridgeAccountLister struct {
	url   string
	token string
}

// NewBridgeAccountLister returns the production directory lister.
func NewBridgeAccountLister(bridgeURL, bridgeToken string) AccountLister {
	return bridgeAccountLister{url: bridgeURL, token: bridgeToken}
}

func (l bridgeAccountLister) ListActiveAccounts(
	ctx context.Context,
	cursor string,
	limit int,
) (AccountPage, error) {
	page, err := client.ListActiveAccountsViaBridge(
		ctx, l.url, l.token, cursor, limit)
	if err != nil {
		return AccountPage{}, err
	}
	return AccountPage{
		AccountIDs: page.AccountIDs,
		NextCursor: page.NextCursor,
	}, nil
}

// PlanLifecycleStatus is value-free fleet observability. It intentionally
// exposes no account ids, cursors, endpoints, provider/customer identifiers,
// or raw errors.
type PlanLifecycleStatus struct {
	Enabled          bool       `json:"enabled"`
	BillingAvailable bool       `json:"billing_available"`
	Running          bool       `json:"running"`
	Runs             uint64     `json:"runs"`
	LastStartedAt    *time.Time `json:"last_started_at,omitempty"`
	LastCompletedAt  *time.Time `json:"last_completed_at,omitempty"`
	LastSucceeded    bool       `json:"last_succeeded"`
	LastScanned      int        `json:"last_scanned"`
	LastSeeded       int        `json:"last_seeded"`
	LastApplyPending int        `json:"last_apply_pending"`
	LastFailed       int        `json:"last_failed"`
}

// PlanLifecycleObserver owns the concurrency-safe status projection.
type PlanLifecycleObserver struct {
	mu     sync.Mutex
	status PlanLifecycleStatus
}

// NewPlanLifecycleObserver returns an enabled status projection.
func NewPlanLifecycleObserver(billingAvailable bool) *PlanLifecycleObserver {
	return &PlanLifecycleObserver{status: PlanLifecycleStatus{
		Enabled:          true,
		BillingAvailable: billingAvailable,
	}}
}

func (o *PlanLifecycleObserver) begin(now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now = now.UTC()
	o.status.Running = true
	o.status.Runs++
	o.status.LastStartedAt = &now
}

func (o *PlanLifecycleObserver) complete(now time.Time, summary PlanLifecycleSummary, succeeded bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now = now.UTC()
	o.status.Running = false
	o.status.LastCompletedAt = &now
	o.status.LastSucceeded = succeeded
	o.status.LastScanned = summary.Scanned
	o.status.LastSeeded = summary.Seeded
	o.status.LastApplyPending = summary.ApplyPending
	o.status.LastFailed = summary.Failed
}

// Snapshot returns a value copy safe for JSON encoding.
func (o *PlanLifecycleObserver) Snapshot() PlanLifecycleStatus {
	if o == nil {
		return PlanLifecycleStatus{}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	out := o.status
	if out.LastStartedAt != nil {
		t := *out.LastStartedAt
		out.LastStartedAt = &t
	}
	if out.LastCompletedAt != nil {
		t := *out.LastCompletedAt
		out.LastCompletedAt = &t
	}
	return out
}

// PlanLifecycleSummary contains aggregate results from one directory scan.
type PlanLifecycleSummary struct {
	Scanned      int
	Seeded       int
	ApplyPending int
	Failed       int
}

// ReconcileAccountIDs applies one already-directory-authorized bounded account
// page. It is the stateless unit driven by the hosted Worker's cron tick; the
// Worker owns the opaque directory cursor so container sleep and process
// restarts cannot reset fleet progress.
//
// Work is concurrent only across accounts. lifecycle.Manager still serializes
// each individual account's apply fence, and the bounded worker count prevents a
// page from turning into an unbounded R2/cell request burst.
func ReconcileAccountIDs(
	ctx context.Context,
	manager *lifecycle.Manager,
	accountIDs []string,
	maxAccounts int,
) (PlanLifecycleSummary, error) {
	if manager == nil {
		return PlanLifecycleSummary{}, errors.New("plan lifecycle manager is required")
	}
	if err := validatePlanLifecycleAccountIDs(accountIDs, maxAccounts); err != nil {
		return PlanLifecycleSummary{}, err
	}
	if len(accountIDs) == 0 {
		return PlanLifecycleSummary{}, nil
	}

	workerCount := min(planLifecycleAccountConcurrency, len(accountIDs))
	jobs := make(chan string, len(accountIDs))
	for _, accountID := range accountIDs {
		jobs <- accountID
	}
	close(jobs)

	var (
		summary PlanLifecycleSummary
		mu      sync.Mutex
		wg      sync.WaitGroup
	)
	wg.Add(workerCount)
	for range workerCount {
		go func() {
			defer wg.Done()
			for {
				var accountID string
				var ok bool
				select {
				case <-ctx.Done():
					return
				case accountID, ok = <-jobs:
					if !ok {
						return
					}
				}
				created, pending, err := manager.EnsureAccount(ctx, accountID)
				mu.Lock()
				summary.Scanned++
				if created {
					summary.Seeded++
				}
				if pending {
					summary.ApplyPending++
				}
				if err != nil {
					summary.Failed++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return summary, errors.New("plan lifecycle page deadline exceeded")
	}
	if summary.Failed > 0 {
		return summary, fmt.Errorf("%d account lifecycle operations failed", summary.Failed)
	}
	return summary, nil
}

func validatePlanLifecycleAccountIDs(accountIDs []string, maxAccounts int) error {
	if maxAccounts < 1 || maxAccounts > 500 {
		return errors.New("plan lifecycle account bound must be 1-500")
	}
	if len(accountIDs) > maxAccounts {
		return errors.New("plan lifecycle account page exceeds its bound")
	}
	seen := make(map[string]struct{}, len(accountIDs))
	for _, accountID := range accountIDs {
		if !planLifecycleAccountIDPattern.MatchString(accountID) {
			return errors.New("plan lifecycle account page contains an invalid account id")
		}
		if _, duplicate := seen[accountID]; duplicate {
			return errors.New("plan lifecycle account page contains a duplicate account id")
		}
		seen[accountID] = struct{}{}
	}
	return nil
}

// ReconcileActiveAccounts processes at most maxPages from cursor, stores a
// Personal/free baseline for every unseen active account, and runs the fenced
// snapshot apply for both new and existing accounts. nextCursor must be fed
// into the next run; empty means this cycle reached the end and the next run
// starts from the beginning. Individual account failures do not stop the
// bounded scan; they remain pending and are retried on the next directory
// cycle.
func ReconcileActiveAccounts(
	ctx context.Context,
	manager *lifecycle.Manager,
	lister AccountLister,
	cursor string,
	pageSize int,
	maxPages int,
) (PlanLifecycleSummary, string, error) {
	if manager == nil || lister == nil {
		return PlanLifecycleSummary{}, cursor, errors.New("plan lifecycle manager and account lister are required")
	}
	if pageSize == 0 {
		pageSize = defaultAccountPageSize
	}
	if pageSize < 1 || pageSize > 500 {
		return PlanLifecycleSummary{}, cursor, errors.New("plan lifecycle page size must be 1-500")
	}
	if maxPages == 0 {
		maxPages = defaultAccountPagesPerRun
	}
	if maxPages < 1 || maxPages > 10 {
		return PlanLifecycleSummary{}, cursor, errors.New("plan lifecycle pages per run must be 1-10")
	}

	var summary PlanLifecycleSummary
	current := cursor
	for pageNumber := 0; pageNumber < maxPages; pageNumber++ {
		page, err := lister.ListActiveAccounts(ctx, current, pageSize)
		if err != nil {
			return summary, current, fmt.Errorf("list active accounts: %w", err)
		}
		if len(page.AccountIDs) > pageSize {
			return summary, current, errors.New("account directory returned an oversized page")
		}
		pageSummary, pageErr := ReconcileAccountIDs(
			ctx, manager, page.AccountIDs, pageSize)
		summary.Scanned += pageSummary.Scanned
		summary.Seeded += pageSummary.Seeded
		summary.ApplyPending += pageSummary.ApplyPending
		summary.Failed += pageSummary.Failed
		if pageErr != nil && pageSummary.Failed == 0 {
			return summary, current, pageErr
		}
		if page.NextCursor == "" {
			current = ""
			if summary.Failed > 0 {
				return summary, current, fmt.Errorf("%d account lifecycle operations failed", summary.Failed)
			}
			return summary, current, nil
		}
		if page.NextCursor == current {
			return summary, current, errors.New("account directory returned a repeated cursor")
		}
		current = page.NextCursor
	}
	if summary.Failed > 0 {
		return summary, current, fmt.Errorf("%d account lifecycle operations failed", summary.Failed)
	}
	return summary, current, nil
}

// RunPlanLifecycleReconciler immediately backfills the active directory, then
// repeats discovery and reconciliation on interval so newly activated
// accounts receive the plan baseline without a signup-path coupling.
func RunPlanLifecycleReconciler(
	ctx context.Context,
	manager *lifecycle.Manager,
	lister AccountLister,
	interval time.Duration,
	pageSize int,
	observer *PlanLifecycleObserver,
	logf func(string, ...any),
) {
	if interval <= 0 {
		interval = time.Minute
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	cursor := ""
	run := func() {
		if observer != nil {
			observer.begin(time.Now())
		}
		summary, nextCursor, seedErr := ReconcileActiveAccounts(
			ctx, manager, lister, cursor, pageSize, defaultAccountPagesPerRun)
		cursor = nextCursor
		succeeded := seedErr == nil
		if observer != nil {
			observer.complete(time.Now(), summary, succeeded)
		}
		// Aggregate, value-free log line: account ids and underlying provider,
		// transport, or storage errors deliberately stay out of logs.
		logf("cpserver: plan lifecycle scanned=%d seeded=%d apply_pending=%d failed=%d succeeded=%t",
			summary.Scanned, summary.Seeded, summary.ApplyPending,
			summary.Failed, succeeded)
	}

	run()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}
