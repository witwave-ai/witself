package localcell

import (
	"context"
	"time"
)

type serviceHostExitPolicy struct {
	admission   time.Duration
	transaction time.Duration
}

// Admission covers a queued health operation and command acceptance. It must
// not consume the existing child-tree drain allowance.
func serviceHostExitBudgets(health time.Duration) serviceHostExitPolicy {
	const maximum = time.Duration(1<<63 - 1)
	admission := serviceControlBudget(max(health, 0), 2)
	transaction := maximum
	if admission <= maximum-ServiceShutdownTimeout {
		transaction = admission + ServiceShutdownTimeout
	}
	return serviceHostExitPolicy{admission: admission, transaction: transaction}
}

// ServiceHostExitBudget includes admission and the unchanged child-tree drain
// allowance. Callers still bound the whole operation with their own deadline.
func ServiceHostExitBudget(health time.Duration) time.Duration {
	return serviceHostExitBudgets(health).transaction
}

func serviceHostExitPhaseBudget(ctx context.Context, limit time.Duration) time.Duration {
	if ctx.Err() != nil {
		return 0
	}
	if deadline, ok := ctx.Deadline(); ok {
		return max(0, min(limit, time.Until(deadline)))
	}
	return max(0, limit)
}
