package localcell

import (
	"os"
	"strings"
	"testing"
)

// The repair and its pre-append barrier share one platform gate. Windows
// flushes only the file; first publication retains its own platform rule.
func TestCheckpointJournalBarrierPlatform(t *testing.T) {
	const barrier = "c.syncCheckpointJournalPrefix(ctx, progress)"
	const fileSync = "if syncFile(f) != nil {"
	for _, row := range []struct {
		file string
		want []string
	}{
		{"checkpoint_journal_repair.go", []string{
			"\nconst checkpointJournalSettlementSupported = runtime.GOOS == \"linux\" || runtime.GOOS == \"darwin\" || runtime.GOOS == \"windows\"\n", // the whole line
			"func (c *Cell) RepairCheckpointJournal(ctx context.Context) (CheckpointJournalStatus, error) {\n\tif !checkpointJournalSettlementSupported {\n\t\treturn \"\", errCheckpointState\n\t}",
		}},
		{"checkpoint.go", []string{
			barrier,
		}},
		{"lifecycle.go", []string{
			"\tif !checkpointJournalSettlementSupported {\n\t\treturn nil\n\t}\n\terr := " + barrier + "\n",
		}},
		{"files.go", []string{ // first publication syncs the root only off Windows
			"\t\tif runtime.GOOS != \"windows\" {\n\t\t\tif err := syncJournalDirectory(c.cfg.Root); err != nil {\n",
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
			"//go:build !linux && !darwin && !windows\n",
			"func (c *Cell) syncCheckpointJournalPrefix(context.Context, journalProgress) error { return nil }",
		}},
		{"checkpoint_journal_binding_windows.go", []string{
			"//go:build windows\n", "var runWindowsJournalBarrier = (*Cell).syncWindowsJournalPrefix", "return runWindowsJournalBarrier(c, ctx, progress)",
			"runtimeconfig.WindowsPrivateOpenFlush(dir, name)", fileSync,
			"runtimeconfig.WindowsPrivateHold(dirHold, name, false)",
		}},
		{"checkpoint_root.go", []string{
			"\nvar syncCheckpointRoot = syncRootDirectory\n",
			"func syncRootDirectory(held *os.Root) error {\n\tif runtime.GOOS == \"windows\" {\n\t\treturn nil\n\t}\n",
		}},
		{"../runtimeconfig/windows_private_hold_windows.go", []string{ // the exclusive hold; neither it nor the hold by path creates
			"access, share, options := uint32(windows.GENERIC_READ|windows.GENERIC_WRITE), uint32(windows.FILE_SHARE_READ), uint32(0)\n",
			"windows.FILE_OPEN, options, nil)",
			`"private flush", windows.FILE_APPEND_DATA, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_NON_DIRECTORY_FILE)`,
			"windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, nil)",
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
			if row.file == "checkpoint_journal_binding_windows.go" {
				_, after, found := strings.Cut(source, "func (c *Cell) syncWindowsJournalPrefix(")
				body, _, closed := strings.Cut(after, "\n}")
				if !found || !closed || !strings.Contains(body, fileSync) {
					t.Fatal("Windows prefix sync lost its journal file flush")
				}
				for _, strict := range []string{"windowsJournalBound(", "WindowsPrivateValidate", "GetFinalPathNameByHandle", ".Links", ".DeletePending"} {
					if strings.Contains(body, strict) {
						t.Fatal("Windows prefix sync acquired a strict repair proof")
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
			// Allow the flush-only opener and the one file sync identified above.
			scanned := strings.ReplaceAll(strings.ReplaceAll(source, "WindowsPrivateOpenFlush", ""), fileSync, "")
			for _, flush := range []string{"Sync(", "Flush", "syncFile(", "syncHeldCheckpointJournal", "syncCheckpointRoot", "syncDirectory", "syncRootDirectory", "CREATE", "_IF", "SUPERSEDE"} {
				if strings.Contains(row.file, "windows") && strings.Contains(scanned, flush) {
					t.Fatal("the Windows journal binding flushes a directory or can create")
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
