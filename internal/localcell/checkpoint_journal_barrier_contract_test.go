package localcell

import (
	"os"
	"strings"
	"testing"
)

// Pin the single platform gate on both paths. Android and iOS can select a
// Unix binding by build tags even though their runtime.GOOS differs.
func TestCheckpointJournalBarrierPlatform(t *testing.T) {
	const barrier = "c.syncCheckpointJournalPrefix(ctx, progress)"
	for _, row := range []struct {
		file string
		want []string
	}{
		{"checkpoint_journal_repair.go", []string{
			"\nconst checkpointJournalSettlementSupported = runtime.GOOS == \"linux\" || runtime.GOOS == \"darwin\"\n", // the whole line
			"func (c *Cell) RepairCheckpointJournal(ctx context.Context) (CheckpointJournalStatus, error) {\n\tif !checkpointJournalSettlementSupported {\n\t\treturn \"\", errCheckpointState\n\t}",
		}},
		{"checkpoint.go", []string{
			barrier,
		}},
		{"lifecycle.go", []string{
			"if checkpointJournalSettlementSupported && " + barrier + " != nil {",
		}},
		{"checkpoint_journal_binding_unix.go", []string{
			"//go:build linux || darwin\n",
			"func (c *Cell) syncCheckpointJournalPrefix(",
			"openCheckpointJournalFile(ctx, dir, name, os.O_RDONLY)",
			"openCheckpointJournalFile(ctx, dir, name, os.O_RDWR)",
			"if !journalNamesBound([]journalBinding{",
			"return syncHeldCheckpointJournal(root, dir, f)",
		}},
		{"checkpoint_journal_binding_other.go", []string{
			"//go:build !linux && !darwin\n",
			"func (c *Cell) syncCheckpointJournalPrefix(context.Context, journalProgress) error { return nil }",
		}},
	} {
		t.Run(row.file, func(t *testing.T) {
			data, err := os.ReadFile(row.file)
			if err != nil {
				t.Fatal("cannot read checkpoint journal platform source")
			}
			source := strings.ReplaceAll(string(data), "\r\n", "\n")
			if row.file == "checkpoint_journal_binding_unix.go" {
				for _, signature := range []string{"func (c *Cell) syncCheckpointJournalPrefix(", "func journalNamesBound("} {
					_, after, found := strings.Cut(source, signature)
					body, _, closed := strings.Cut(after, "\n}")
					if !found || !closed || strings.Contains(body, ".Dev") || strings.Contains(body, "journalBindings(") {
						t.Fatal("barrier acquired the repair device restriction")
					}
				}
			}
			if row.file == "checkpoint.go" {
				_, supported, found := strings.Cut(source, "\n\tif checkpointJournalSettlementSupported {\n")
				body, _, closed := strings.Cut(supported, "\n\t}")
				if !found || !closed || !strings.Contains(body, barrier) {
					t.Fatal("prefix barrier bypassed the journal repair platform gate")
				}
			}
			for _, want := range row.want {
				if strings.Count(source, want) != 1 {
					t.Fatal("journal repair and prefix barrier platform selection changed")
				}
			}
		})
	}
}
