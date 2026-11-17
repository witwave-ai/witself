package localcell_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

// checkpointCommandNotice is the released receipt's notice, byte for byte.
const checkpointCommandNotice = "Checkpoint journal history may remain, including after purged-root delete; another purge does not prune it. Older readers may refuse that history; downgrade compatibility is unsupported."

// checkpointCommandReceipt is the released receipt, decoded with unknown
// fields refused (untagged names match their keys) and never printed.
type checkpointCommandReceipt struct {
	Operation, Outcome, Target, Presence, Verification, Notice string
	Failure                                                    *checkpointCommandFailure
	Before, After                                              *checkpointCommandSnapshot
}
type checkpointCommandFailure struct {
	Class, Reason     string
	CleanupIncomplete bool `json:"cleanup_incomplete"`
}
type checkpointCommandSnapshot struct {
	Status         string
	Failure        *checkpointCommandFailure
	InstallationID string `json:"installation_id"`
	Count          *int
	UnknownCount   *int `json:"unknown_count"`
	Entries        []struct {
		ID, Verification string
		InstallationID   string `json:"installation_id"`
		Complete         bool
	}
}

// listed maps a listed snapshot's unverified entries to completeness, or is nil.
func (s *checkpointCommandSnapshot) listed(installation string) map[string]bool {
	if s == nil || s.Status != "listed" || s.Failure != nil || s.Count == nil || *s.Count != len(s.Entries) || s.UnknownCount == nil || *s.UnknownCount != 0 {
		return nil
	}
	entries := map[string]bool{}
	for _, entry := range s.Entries {
		if entry.Verification != "not_performed" || (entry.Complete && entry.InstallationID != installation) {
			return nil
		}
		entries[entry.ID] = entry.Complete
	}
	return entries
}

// checkpointCommandKeys reads one JSON value and refuses a repeated key or one
// with a byte outside a-z and _, which encoding/json would accept or fold.
func checkpointCommandKeys(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil || (token != json.Delim('{') && token != json.Delim('[')) {
		return err == nil
	}
	seen := map[string]bool{}
	for decoder.More() {
		if token == json.Delim('{') {
			key, err := decoder.Token()
			name, ok := key.(string)
			if err != nil || !ok || seen[name] || strings.Trim(name, "abcdefghijklmnopqrstuvwxyz_") != "" {
				return false
			}
			seen[name] = true
		}
		if !checkpointCommandKeys(decoder) {
			return false
		}
	}
	_, err = decoder.Token()
	return err == nil
}

// testCheckpointCommandNative proves the released command on a real PostgreSQL
// 16 cell (legacy layout): a CLI built once from this checkout runs with the
// parent environment and fresh homes that stay empty. New identifiers come from
// a receipt's own before and after observations, never from attribution.
func testCheckpointCommandNative(t *testing.T, binaries localcell.Binaries) {
	ctx := t.Context()
	source, err := filepath.Abs(filepath.Join("..", ".."))
	cli := filepath.Join(t.TempDir(), lifecycleExecutableName("witself"))
	build := offlineBuildCommand(ctx, "build", "-o", cli, "./cmd/witself")
	build.Dir, build.Stdout, build.Stderr = source, io.Discard, io.Discard
	if err != nil || build.Run() != nil {
		t.Fatal("cannot build the released checkpoint command")
	}
	t.Setenv("WITSELF_CHECKPOINT_MODE", "cold-v1") // never selects the mode; set for every call
	var homes []string
	for _, name := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "WITSELF_HOME", "DSH_HOME"} {
		homes = append(homes, name+"="+t.TempDir())
	}
	var captured []byte
	command := func(want int, args ...string) (receipt checkpointCommandReceipt) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		bounded, cancel := context.WithTimeout(ctx, 2*time.Minute) // a hung call fails, so cleanup still runs
		defer cancel()
		cmd := exec.CommandContext(bounded, cli, args...)
		cmd.Env, cmd.Stdout, cmd.Stderr = append(os.Environ(), homes...), &stdout, &stderr
		code, err := 0, cmd.Run()
		captured = append(append(captured, stdout.Bytes()...), stderr.Bytes()...)
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal("cannot run the released checkpoint command")
		}
		if code != want {
			t.Fatal("the released checkpoint command exit code differs")
		}
		if len(args) < 2 || args[1] != "checkpoint" {
			if stdout.Len() != 0 || stderr.String() != "usage: witself local checkpoint create|inspect|delete --checkpoint-mode=cold-v1 --root=ABSOLUTE_PATH\n" {
				t.Fatal("the released namespace usage line differs")
			}
			return receipt
		}
		decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
		decoder.DisallowUnknownFields()
		if stderr.Len() != 0 || bytes.Count(stdout.Bytes(), []byte("\n")) != 1 || !bytes.HasSuffix(stdout.Bytes(), []byte("\n")) || !checkpointCommandKeys(json.NewDecoder(bytes.NewReader(stdout.Bytes()))) ||
			decoder.Decode(&receipt) != nil || decoder.Decode(new(any)) != io.EOF || receipt.Verification != "not_performed" || receipt.Notice != checkpointCommandNotice || receipt.Operation != args[2] {
			t.Fatal("a released checkpoint receipt is not one framed unverified line of its operation with its notice")
		}
		return receipt
	}
	cell, root := disposableCell(t, binaries, nil)
	if cell.Install(ctx, localcell.InstallOptions{}) != nil || startLifecycle(t, cell, root) != nil {
		t.Fatal("cannot install and start the command proof cell")
	}
	manifest := assertReady(t, cell)
	journal, password := filepath.Join(root, "journal", "operations.jsonl"), checkpointNativeRead(t, filepath.Join(root, "run", "postgres.password"))
	selected := []string{"--checkpoint-mode=cold-v1", "--root=" + root}
	create := append([]string{"local", "checkpoint", "create", "--pgbin=" + binaries.PostgresDir, "--installation-id=" + manifest.InstallationID}, selected...)
	// Step 1: a selected create on the live cell is refused before any write.
	prefix := checkpointNativeRead(t, journal)
	r := command(1, create...)
	if _, err := os.Lstat(filepath.Join(root, "recovery")); r.Outcome != "failed" || r.Failure == nil || r.Failure.Class != "local_checkpoint_state_unsupported" ||
		!errors.Is(err, fs.ErrNotExist) || !bytes.Equal(prefix, checkpointNativeRead(t, journal)) {
		t.Fatal("a selected create on the live cell was not refused unchanged")
	}
	if cell.Stop(ctx) != nil {
		t.Fatal("cannot stop the command proof cell")
	}
	// Step 2: without the selector each verb, and the namespace alone, change
	// nothing; the real main runs each call (LC-S7D2F4B2R G-1).
	tree := checkpointNativeTree(t, root, ".")
	for _, args := range [][]string{create[:5], {"local", "checkpoint", "inspect"}, {"local", "checkpoint", "delete", "--checkpoint-id=ckpt_aaaaaaaaaaaaaaaa"}} {
		if r := command(2, append(slices.Clip(args), "--root="+root)...); r.Outcome != "refused" || r.Failure == nil ||
			r.Failure.Class != "local_checkpoint_command_disabled" || r.Before != nil || r.After != nil || r.Target != "" {
			t.Fatal("an unselected create, inspect or delete was not refused as disabled")
		}
	}
	command(2, "local")
	command(2, "local", "bogus")
	if !reflect.DeepEqual(tree, checkpointNativeTree(t, root, ".")) {
		t.Fatal("an unselected call or the bare namespace changed the cell root")
	}
	// Step 3: a selected inspect lists none, with the manifest's identity.
	if r := command(0, append([]string{"local", "checkpoint", "inspect"}, selected...)...); r.Outcome != "succeeded" ||
		len(r.After.listed(manifest.InstallationID)) != 0 || r.After.listed(manifest.InstallationID) == nil || r.After.InstallationID != manifest.InstallationID {
		t.Fatal("a selected inspect did not list an empty recovery with the installation identity")
	}
	// Step 4: two creates through --pgbin; observations equal Go Inspect's.
	var ids []string
	for range 2 {
		before := checkpointNativeListed(t, cell)
		r := command(0, create...)
		after := checkpointNativeListed(t, cell)
		observed, created := r.Before.listed(manifest.InstallationID), r.After.listed(manifest.InstallationID)
		if r.Outcome != "succeeded" || r.Failure != nil || r.Target != "" || observed == nil || created == nil || !maps.Equal(observed, before) || !maps.Equal(created, after) {
			t.Fatal("a selected create did not succeed with observations equal to Go Inspect")
		}
		ids = append(ids, checkpointNativeNew(t, observed, created))
	}
	// Step 5: a third create is refused at the limit; listing and journal stay.
	before, prefix := checkpointNativeListed(t, cell), checkpointNativeRead(t, journal)
	if r := command(1, create...); r.Outcome != "failed" || r.Failure == nil || r.Failure.Class != "local_checkpoint_limit_reached" ||
		!maps.Equal(before, checkpointNativeListed(t, cell)) || !bytes.Equal(prefix, checkpointNativeRead(t, journal)) {
		t.Fatal("a third create was not refused at the limit unchanged")
	}
	// Step 6: delete the identifier Inspect lists last; the first is untouched.
	inspection, err := cell.Inspect(ctx)
	if err != nil || len(inspection.Checkpoints) != 2 || !maps.Equal(before, map[string]bool{ids[0]: true, ids[1]: true}) {
		t.Fatal("cannot order the two created checkpoints")
	}
	first, last := inspection.Checkpoints[0].ID, inspection.Checkpoints[1].ID
	kept := filepath.Join(root, "recovery", first)
	tree, inventory, witness := checkpointNativeTree(t, kept, "."), checkpointNativeRead(t, filepath.Join(kept, "inventory.json")), checkpointNativeRead(t, filepath.Join(kept, "witness.json"))
	r = command(0, append([]string{"local", "checkpoint", "delete", "--checkpoint-id=" + last}, selected...)...)
	if listed := checkpointNativeListed(t, cell); r.Outcome != "succeeded" || r.Target != last || r.Presence != "absent" || len(listed) != 1 || !listed[first] ||
		!reflect.DeepEqual(tree, checkpointNativeTree(t, kept, ".")) || !bytes.Equal(inventory, checkpointNativeRead(t, filepath.Join(kept, "inventory.json"))) ||
		!bytes.Equal(witness, checkpointNativeRead(t, filepath.Join(kept, "witness.json"))) {
		t.Fatal("deleting the last-listed checkpoint differs or changed the other one")
	}
	// Step 7: after Go removes and purges, a purged-root delete keeps history (O5).
	for _, remove := range []localcell.RemoveOptions{{}, {Purge: true}} {
		if cell.Remove(ctx, remove) != nil {
			t.Fatal("cannot remove and purge the command proof cell")
		}
	}
	r = command(0, append([]string{"local", "checkpoint", "delete", "--checkpoint-id=" + first}, selected...)...)
	entries, err := os.ReadDir(filepath.Join(root, "recovery"))
	if _, journalErr := os.Lstat(journal); r.Outcome != "succeeded" || r.Target != first || r.Presence != "absent" || journalErr != nil || err != nil || len(entries) != 0 {
		t.Fatal("the purged-root delete did not keep journal history and empty recovery")
	}
	// Step 8: no captured byte names a path (also escaped), recovery, the password,
	// a control field or a 64-hex run; identifiers are exempt (R3).
	if len(bytes.TrimSpace(password)) < 24 || regexp.MustCompile(`[0-9A-Fa-f]{64}`).Match(captured) {
		t.Fatal("captured command output holds a 64-hex run or the password is unreadable")
	}
	for _, value := range []string{root, filepath.Base(root), strconv.Quote(root), binaries.PostgresDir, strconv.Quote(binaries.PostgresDir), "recovery",
		string(bytes.TrimSpace(password)), "pg_control version number", "Catalog version number", "Database system identifier", "Database cluster state", "Latest checkpoint location"} {
		if bytes.Contains(captured, []byte(strings.Trim(value, `"`))) {
			t.Fatal("captured command output names a path, recovery, the password or control data")
		}
	}
	for _, home := range homes {
		if listing, err := os.ReadDir(home[strings.IndexByte(home, '=')+1:]); err != nil || len(listing) != 0 {
			t.Fatal("the released command wrote into a fresh home")
		}
	}
}
