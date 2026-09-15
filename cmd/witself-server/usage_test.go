package main

import (
	"testing"

	"github.com/witwave-ai/witself/internal/store"
)

func TestUsageReportPreservesTruncation(t *testing.T) {
	report := toServerUsageReport(store.UsageReport{Truncated: true})
	if !report.Truncated {
		t.Fatal("store truncation indicator was lost at the HTTP adapter")
	}
	if report.Points == nil || report.Totals == nil {
		t.Fatal("empty usage arrays must remain JSON arrays")
	}
}
