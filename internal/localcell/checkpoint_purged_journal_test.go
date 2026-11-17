package localcell

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// purgedCheckpointFixture mirrors the twoComplete closure, then purges custody.
func purgedCheckpointFixture(t *testing.T) (*Cell, []string) {
	t.Helper()
	f, _ := checkpointFixture(t, "legacy", false)
	c := f.cell
	if errors.Join(c.Checkpoint(t.Context(), checkpointOptions(f)), c.Checkpoint(t.Context(), checkpointOptions(f))) != nil {
		t.Fatal("cannot prepare two checkpoints")
	}
	checkpointWrite(t, c, "pg/sentinel", "live sentinel")
	ids := checkpointIDs(t, c)
	if len(ids) != 2 || c.Remove(t.Context(), RemoveOptions{Purge: true}) != nil {
		t.Fatal("cannot purge checkpoint fixture")
	}
	tree := checkpointTreeState(t, c)
	_, manifest := tree["manifest.json"]
	_, journal := tree["journal"]
	if manifest || journal || !slices.Equal(checkpointIDs(t, c), ids) ||
		!checkpointSummaryOf(t, c, ids[0]).Complete || !checkpointSummaryOf(t, c, ids[1]).Complete {
		t.Fatal("purged fixture preconditions differ")
	}
	return c, ids
}

func TestPurgedCheckpointJournal(t *testing.T) {
	crashRecords := map[string][]string{
		"unseal": {"checkpoint_delete/operation/started", "checkpoint_delete/unseal/started"},
		"remove": {"checkpoint_delete/operation/started", "checkpoint_delete/unseal/started",
			"checkpoint_delete/unseal/done", "checkpoint_delete/remove/started"},
	}
	repairRecords := map[string][]string{
		"unseal": {"checkpoint_delete/operation/started", "checkpoint_delete/unseal/started",
			"checkpoint_delete/revert_stop/started", "checkpoint_delete/revert_stop/done", "checkpoint_delete/operation/done"},
		"remove": {"checkpoint_delete/operation/started", "checkpoint_delete/unseal/started",
			"checkpoint_delete/unseal/done", "checkpoint_delete/remove/started", "checkpoint_delete/revert_stop/started",
			"checkpoint_delete/revert_stop/done", "checkpoint_delete/operation/done"},
	}
	// Every complete line is parsed independently; an empty or torn line fails.
	triples := func(t *testing.T, data []byte) []string {
		t.Helper()
		if len(data) == 0 || data[len(data)-1] != '\n' {
			t.Fatal("journal has no complete final line")
		}
		var result []string
		for _, line := range bytes.Split(data[:len(data)-1], []byte("\n")) {
			var entry journalEntry
			if json.Unmarshal(line, &entry) != nil {
				t.Fatal("journal line is unreadable")
			}
			result = append(result, entry.Operation+"/"+entry.Step+"/"+entry.State)
		}
		return result
	}
	journal := func(t *testing.T, c *Cell, want []string, pending bool) []byte {
		t.Helper()
		data := retainedRead(t, c, "journal/operations.jsonl")
		if !slices.Equal(triples(t, data), want) {
			t.Fatal("journal sequence differs")
		}
		progress, err := c.readJournal()
		if err != nil || progress.tornTail || (progress.pending != nil) != pending ||
			(pending && progress.pending.Operation != "checkpoint_delete") {
			t.Fatal("journal pending state differs")
		}
		return data
	}
	subtree := func(tree map[string]retainedEntry, id string) map[string]retainedEntry {
		result := maps.Clone(tree)
		maps.DeleteFunc(result, func(name string, _ retainedEntry) bool {
			return name != "recovery/"+id && !strings.HasPrefix(name, "recovery/"+id+"/")
		})
		return result
	}
	preserved := func(t *testing.T, c *Cell, ids []string, before map[string]retainedEntry) {
		t.Helper()
		after := checkpointTreeState(t, c)
		if _, exists := after["manifest.json"]; exists {
			t.Fatal("manifest was recreated")
		}
		if !reflect.DeepEqual(subtree(before, ids[1]), subtree(after, ids[1])) {
			t.Fatal("sibling checkpoint changed")
		}
	}
	deleted := func(t *testing.T) (*Cell, []string) {
		t.Helper()
		c, ids := purgedCheckpointFixture(t)
		before := checkpointTreeState(t, c)
		if c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: ids[0]}) != nil {
			t.Fatal("purged checkpoint deletion failed")
		}
		journal(t, c, checkpointDeleteRecords, false)
		if !slices.Equal(checkpointIDs(t, c), ids[1:]) {
			t.Fatal("deletion listing differs")
		}
		after := checkpointTreeState(t, c)
		changed := reinstallChanged(checkpointWithout(maps.Clone(before), ids[0]), checkpointWithout(maps.Clone(after), ids[0]))
		slices.Sort(changed)
		// checkpointWithout excludes the target and the separately checked journal file.
		if !slices.Equal(changed, []string{"journal", "run", "run/operation.lock"}) {
			t.Fatal("deletion changed unexpected tree entries")
		}
		// Closed directory union from validateRoot and layout; manifest is separate.
		for _, name := range []string{"pg", "spool", "run", "journal", "logs", "tmp", "home", "recovery"} {
			_, existed := before[name]
			_, exists := after[name]
			if !existed && exists && name != "journal" && name != "run" {
				t.Fatal("deletion recreated unexpected custody")
			}
		}
		preserved(t, c, ids, before)
		return c, ids
	}
	crashed := func(t *testing.T, cut string) (*Cell, []string) {
		t.Helper()
		c, ids := purgedCheckpointFixture(t)
		before := checkpointTreeState(t, c)
		c.cfg.Failpoint = checkpointFailpoint("checkpoint_delete", cut)
		err := c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: ids[0]})
		c.cfg.Failpoint = nil
		if !errors.Is(err, errInterrupted) {
			t.Fatal("deletion crash was not injected")
		}
		want := crashRecords[cut]
		remaining := subtree(before, ids[0])
		delete(remaining, "recovery/"+ids[0]+"/"+checkpointWitnessName)
		if cut == "remove" {
			remaining = map[string]retainedEntry{}
		}
		journal(t, c, want, true)
		if len(want) >= len(checkpointDeleteRecords) || !slices.Equal(want, checkpointDeleteRecords[:len(want)]) {
			t.Fatal("crash journal is not a strict deletion prefix")
		}
		if !reflect.DeepEqual(remaining, subtree(checkpointTreeState(t, c), ids[0])) {
			t.Fatal("crash checkpoint state differs")
		}
		preserved(t, c, ids, before)
		held := checkpointTreeState(t, c)
		if !errors.Is(c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: "ckpt_zzzzzzzzzzzzzzzz"}), errCheckpointIdentity) {
			t.Fatal("pending deletion did not refuse an absent identifier")
		}
		if !reflect.DeepEqual(held, checkpointTreeState(t, c)) {
			t.Fatal("pending deletion refusal changed the tree")
		}
		return c, ids
	}
	repaired := func(t *testing.T, cut string) (*Cell, []string) {
		t.Helper()
		c, ids := crashed(t, cut)
		before := checkpointTreeState(t, c)
		prefix := retainedRead(t, c, "journal/operations.jsonl")
		if c.Repair(t.Context()) != nil {
			t.Fatal("repair of purged checkpoint deletion failed")
		}
		after := retainedRead(t, c, "journal/operations.jsonl")
		if !bytes.HasPrefix(after, prefix) {
			t.Fatal("repair rewrote journal history")
		}
		appended := []string{"checkpoint_delete/revert_stop/started", "checkpoint_delete/revert_stop/done", "checkpoint_delete/operation/done"}
		if !slices.Equal(triples(t, after[len(prefix):]), appended) {
			t.Fatal("repair appended unexpected records")
		}
		journal(t, c, repairRecords[cut], false)
		preserved(t, c, ids, before)
		if cut == "unseal" {
			if !slices.Equal(checkpointIDs(t, c), ids) || checkpointSummaryOf(t, c, ids[0]).Complete {
				t.Fatal("repair changed the incomplete checkpoint listing")
			}
			if c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: ids[0]}) != nil {
				t.Fatal("second exact deletion failed")
			}
			final := retainedRead(t, c, "journal/operations.jsonl")
			if !bytes.HasPrefix(final, after) {
				t.Fatal("second deletion rewrote journal history")
			}
			if !slices.Equal(triples(t, final[len(after):]), checkpointDeleteRecords) {
				t.Fatal("second deletion appended unexpected records")
			}
			journal(t, c, append(slices.Clone(repairRecords[cut]), checkpointDeleteRecords...), false)
		} else {
			if !slices.Equal(checkpointIDs(t, c), ids[1:]) {
				t.Fatal("remove crash repair listing differs")
			}
			held := checkpointTreeState(t, c)
			if !errors.Is(c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: ids[0]}), errCheckpointIdentity) {
				t.Fatal("absent checkpoint deletion was not refused")
			}
			if !bytes.Equal(after, retainedRead(t, c, "journal/operations.jsonl")) {
				t.Fatal("absent checkpoint refusal changed journal")
			}
			// Existing lock bookkeeping is identical too: the allowed change set is empty.
			if !slices.Equal(reinstallChanged(held, checkpointTreeState(t, c)), []string{}) {
				t.Fatal("absent checkpoint refusal changed the tree")
			}
		}
		if !slices.Equal(checkpointIDs(t, c), ids[1:]) {
			t.Fatal("final repaired deletion listing differs")
		}
		preserved(t, c, ids, before)
		return c, ids
	}
	t.Run("delete", func(t *testing.T) { deleted(t) })
	t.Run("unseal-crash", func(t *testing.T) { crashed(t, "unseal") })
	t.Run("remove-crash", func(t *testing.T) { crashed(t, "remove") })
	t.Run("repair", func(t *testing.T) {
		for _, cut := range []string{"unseal", "remove"} {
			t.Run(cut, func(t *testing.T) { repaired(t, cut) })
		}
	})
	t.Run("second-purge", func(t *testing.T) {
		for _, state := range []string{"deleted", "unseal-crash"} {
			t.Run(state, func(t *testing.T) {
				var c *Cell
				var ids []string
				if state == "deleted" {
					c, ids = deleted(t)
				} else {
					c, ids = crashed(t, "unseal")
				}
				before := checkpointTreeState(t, c)
				prefix := retainedRead(t, c, "journal/operations.jsonl")
				if c.Remove(t.Context(), RemoveOptions{Purge: true}) != nil {
					t.Fatal("second purge failed")
				}
				if !bytes.Equal(prefix, retainedRead(t, c, "journal/operations.jsonl")) {
					t.Fatal("second purge changed journal")
				}
				wantRecords := checkpointDeleteRecords
				if state == "unseal-crash" {
					wantRecords = crashRecords["unseal"]
				}
				journal(t, c, wantRecords, state == "unseal-crash")
				// No entries may change, including the already present lock bookkeeping.
				if !slices.Equal(reinstallChanged(before, checkpointTreeState(t, c)), []string{}) {
					t.Fatal("second purge changed the tree")
				}
				want := ids[1:]
				if state == "unseal-crash" {
					want = ids
				}
				if !slices.Equal(checkpointIDs(t, c), want) {
					t.Fatal("second purge changed checkpoint listing")
				}
				preserved(t, c, ids, before)
			})
		}
	})
	t.Run("predecessor", func(t *testing.T) {
		for _, state := range []string{"deleted", "unseal", "remove"} {
			t.Run(state, func(t *testing.T) {
				var c *Cell
				if state == "deleted" {
					c, _ = deleted(t)
				} else {
					c, _ = repaired(t, state)
				}
				data := retainedRead(t, c, "journal/operations.jsonl")
				if _, err := c.readJournal(); err != nil || len(triples(t, data)) < len(checkpointDeleteRecords) {
					t.Fatal("current reader refused final deletion journal")
				}
				for _, line := range bytes.Split(data[:len(data)-1], []byte("\n")) {
					var entry journalEntry
					if json.Unmarshal(line, &entry) != nil || checkpointPredecessorJournalStep(entry.Operation, entry.Step) ||
						!validJournalStep(entry.Operation, entry.Step) {
						t.Fatal("predecessor vocabulary distinction failed")
					}
				}
			})
		}
	})
}
