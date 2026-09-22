package dashboard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

// Run the real browser normalizer against the same typed snapshots accepted by
// the TUI's shared validator, including malformed new-metric wire shapes.
func TestSummaryActivityConsumerAgreement(t *testing.T) {
	node := testenv.RequireNode(t)
	type sample struct {
		Summary  Summary  `json:"summary"`
		Expected []string `json:"expected"`
	}
	var cases []sample
	mutations := []func(*SummaryActivity){
		func(_ *SummaryActivity) {},
		func(a *SummaryActivity) { a.Coverage = nil },
		func(a *SummaryActivity) { a.Coverage.TrackingSince = nil },
		func(a *SummaryActivity) { a.Coverage.PartialFirstBucket = false },
		func(a *SummaryActivity) { at := summaryTestNow.Add(time.Hour); a.Coverage.TrackingSince = &at },
		func(a *SummaryActivity) { v := int64(0); a.Bins[0] = &v },
		func(a *SummaryActivity) { a.Bins[7] = nil },
		func(a *SummaryActivity) { a.Bins = a.Bins[:23] },
		func(a *SummaryActivity) { a.Total = nil },
		func(a *SummaryActivity) { *a.Total++ },
		func(a *SummaryActivity) { a.Dimension = "PRIVATE" },
		func(a *SummaryActivity) { a.Unit = "record" },
		func(a *SummaryActivity) { a.Breakdown = a.Breakdown[:1] },
		func(a *SummaryActivity) { a.Breakdown[0].Total = nil },
		func(a *SummaryActivity) { *a.Breakdown[0].Total = -1 },
		func(a *SummaryActivity) { *a.Breakdown[0].Total = summaryMaxInteger },
		func(a *SummaryActivity) { a.Breakdown[0].Unit = "PRIVATE" },
		func(a *SummaryActivity) { a.Breakdown[0] = a.Breakdown[1] },
	}
	for _, mutate := range mutations {
		s := projectActivityFixture(activityFixture())
		for _, i := range []int{0, 3} {
			mutate(&s.Categories[i].Activity)
		}
		expected := []string{}
		for _, i := range []int{0, 3} {
			a := s.Categories[i].Activity
			status := "unavailable"
			if ValidSummaryActivity(a, s.Categories[i].Key, s.Window) {
				status = a.Status
			}
			expected = append(expected, status)
		}
		cases = append(cases, sample{s, expected})
	}
	for _, status := range []string{"not_tracked", "server_update_needed", "unavailable", "disabled"} {
		s := emptySummary(summaryTestNow)
		for _, i := range []int{0, 3} {
			a := &s.Categories[i].Activity
			a.Status = status
			if status == "not_tracked" {
				a.Bins = make([]*int64, 24)
				a.Coverage = &SummaryCoverage{}
			}
			if !ValidSummaryActivity(*a, s.Categories[i].Key, s.Window) {
				t.Fatal("invalid status fixture")
			}
		}
		cases = append(cases, sample{s, []string{status, status}})
	}
	raw, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "summary.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "--test", "testdata/summary_consumer_test.cjs")
	cmd.Env = append(os.Environ(), "WITSELF_SUMMARY_FIXTURE="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("consumer disagreement: %v\n%s", err, output)
	}
}
