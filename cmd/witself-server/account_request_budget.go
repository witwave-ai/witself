package main

import (
	"fmt"
	"io"

	"github.com/witwave-ai/witself/internal/server"
)

func configureAccountRequestBudget(cfg *server.Config, lookup func(string) (string, bool), w io.Writer) error {
	budget, err := server.ParseAccountRequestBudgetConfig(lookup)
	if err != nil {
		return err
	}
	cfg.AccountRequestBudget = budget
	if budget.Enabled {
		_, _ = fmt.Fprintf(w, "witself-server: account request budget enabled read_per_minute=%d read_burst=%d write_per_minute=%d write_burst=%d max_tracked_accounts=%d\n",
			budget.ReadPerMinute, budget.ReadBurst, budget.WritePerMinute, budget.WriteBurst, budget.MaxTrackedAccounts)
	}
	return nil
}
