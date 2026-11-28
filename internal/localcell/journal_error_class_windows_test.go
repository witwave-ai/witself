//go:build windows

package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"
)

func TestJournalRepairErrorClassAfterDiscard(t *testing.T) {
	c, _ := unitCell(t)
	path := c.path("journal", "operations.jsonl")
	prefix := journalRecord(t, "start", "operation", "started")
	if err := testWriteFile(path, append(prefix, '{'), 0o600); err != nil {
		t.Fatal("could not prepare torn journal for repair classification")
	}
	ctx, cancel := context.WithDeadline(t.Context(), time.Unix(1, 0))
	defer cancel()
	if class := journalRepairErrorClass(c.Repair(ctx)); class != "sentinel=other windows_errno=other path=other step=revert_stop" {
		t.Logf("journal repair error class: %s", class)
		t.Fatal("repair classifier lost the post-discard rollback failure")
	}
	// The failed rollback journals its own started and failed records. They
	// follow the validated prefix directly only when the discard removed the
	// torn byte first.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("could not read the journal after the failed rollback")
	}
	if !bytes.HasPrefix(got, prefix) {
		t.Fatal("failed rollback did not leave the validated journal prefix")
	}
	var records []string
	for _, line := range bytes.SplitAfter(got[len(prefix):], []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var entry journalEntry
		if !bytes.HasSuffix(line, []byte{'\n'}) || json.Unmarshal(line, &entry) != nil {
			t.Fatal("failed rollback left bytes after the prefix that are not complete records")
		}
		records = append(records, entry.Operation+" "+entry.Step+" "+entry.State)
	}
	if !slices.Equal(records, []string{"start revert_stop started", "start revert_stop failed"}) {
		t.Fatal("failed rollback did not journal exactly its own started and failed records")
	}
}
