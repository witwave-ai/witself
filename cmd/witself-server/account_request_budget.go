package main

import (
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/witwave-ai/witself/internal/server"
)

func configureAccountRequestBudget(cfg *server.Config, lookup func(string) (string, bool), w io.Writer) error {
	budget, err := server.ParseAccountRequestBudgetConfig(lookup)
	if err != nil {
		return err
	}
	if budget.Enabled {
		budget.Plans, err = server.ParseAccountRequestBudgetPlans(lookup)
		if err != nil {
			return err
		}
	}
	cfg.AccountRequestBudget = budget
	if budget.Enabled {
		_, _ = fmt.Fprintf(w, "witself-server: account request budget enabled read_per_minute=%d read_burst=%d write_per_minute=%d write_burst=%d max_tracked_accounts=%d\n",
			budget.ReadPerMinute, budget.ReadBurst, budget.WritePerMinute, budget.WriteBurst, budget.MaxTrackedAccounts)
		for _, id := range slices.Sorted(maps.Keys(budget.Plans)) {
			tier := budget.Plans[id]
			_, _ = fmt.Fprintf(w, "witself-server: account request budget plan=%s read_per_minute=%d read_burst=%d write_per_minute=%d write_burst=%d\n",
				id, tier.ReadPerMinute, tier.ReadBurst, tier.WritePerMinute, tier.WriteBurst)
		}
	}
	return nil
}
