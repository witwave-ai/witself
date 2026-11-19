package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// checkpointWitnessBytes is a witness as writeAtomic publishes it.
func checkpointWitnessBytes(t *testing.T, witness checkpointWitness) []byte {
	t.Helper()
	data, err := json.MarshalIndent(witness, "", "  ")
	if err != nil {
		t.Fatal("cannot encode the witness fixture")
	}
	return append(data, '\n')
}

// A witness read in full within readJSON's 1 MiB bound is complete or, when it
// is not this format-1 witness, incomplete without an error; a failed read is
// errCheckpointWitnessUnread, even after a complete valid object.
func TestCheckpointWitnessRead(t *testing.T) {
	const id = "ckpt_abcdefghijklmnop"
	want := checkpointWitness{Format: 1, CheckpointID: id, InstallationID: "lc_test", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Bytes: 26}
	valid := checkpointWitnessBytes(t, want)
	edited := func(edit func(*checkpointWitness)) []byte {
		witness := want
		edit(&witness)
		return checkpointWitnessBytes(t, witness)
	}
	spaces := func(n int) []byte { return bytes.Repeat([]byte(" "), n) }
	join := func(parts ...[]byte) io.Reader { return bytes.NewReader(bytes.Join(parts, nil)) }
	failing := func(parts ...[]byte) io.Reader {
		return io.MultiReader(join(parts...), iotest.ErrReader(errors.New("synthetic witness read failure")))
	}
	for _, row := range []struct {
		name     string
		reader   io.Reader
		complete bool
		err      error
	}{
		{"valid", join(valid), true, nil},
		{"object_within_bound", join([]byte("{"), spaces(8<<10), valid[1:]), true, nil},
		{"padded_past_bound", join(valid, spaces(1<<20)), true, nil},
		{"empty", join(), false, nil},
		{"truncated", join(valid[:len(valid)/2]), false, nil},
		{"not_json", join([]byte("{not json}")), false, nil},
		{"other_checkpoint", join(edited(func(w *checkpointWitness) { w.CheckpointID = "ckpt_bcdefghijklmnopq" })), false, nil},
		{"format_2", join(edited(func(w *checkpointWitness) { w.Format = 2 })), false, nil},
		{"unknown_field", join([]byte(`{"unknown":1,`), valid[1:]), false, nil},
		{"trailing_value", join(valid, []byte("{}")), false, nil},
		{"trailing_garbage", join(valid, []byte("x")), false, nil},
		{"object_past_bound", join([]byte("{"), spaces(1<<20), valid[1:]), false, nil},
		{"read_fails_at_once", failing(), false, errCheckpointWitnessUnread},
		{"read_fails_midway", failing(valid[:len(valid)/2]), false, errCheckpointWitnessUnread},
		{"read_fails_after_witness", failing(valid), false, errCheckpointWitnessUnread},
	} {
		t.Run(row.name, func(t *testing.T) {
			got, complete, err := checkpointWitnessRead(row.reader, id)
			if complete != row.complete || !errors.Is(err, row.err) {
				t.Fatal("witness read reported another completeness or error")
			}
			if complete && got != want {
				t.Fatal("witness read returned other fields")
			}
		})
	}
}

// Absence is proven below a directory and an irregular object is examined:
// both are incomplete without an error. A regular file where the checkpoint
// directory is expected is not absence on any OS (Windows reports it not
// found, a not-exist error): it is never read as incomplete.
func TestCheckpointWitnessOpen(t *testing.T) {
	const id, other = "ckpt_abcdefghijklmnop", "ckpt_bcdefghijklmnopq"
	for _, row := range []struct {
		name  string
		plant func(t *testing.T, c *Cell)
		err   error
	}{
		{"absent", func(t *testing.T, c *Cell) { checkpointPlant(t, c, id, "") }, nil},
		{"directory", func(t *testing.T, c *Cell) {
			if privateDir(filepath.Join(checkpointPlant(t, c, id, ""), checkpointWitnessName), true) != nil {
				t.Fatal("cannot plant a directory at the witness name")
			}
		}, nil},
		{"missing_directory", func(t *testing.T, c *Cell) { checkpointPlant(t, c, other, other) }, nil},
		{"not_a_directory", func(t *testing.T, c *Cell) {
			checkpointPlant(t, c, other, other)
			checkpointWrite(t, c, "recovery/"+id, "synthetic checkpoint bytes")
		}, errCheckpointWitnessUnread},
	} {
		t.Run(row.name, func(t *testing.T) {
			f, _ := checkpointFixture(t, "legacy", false)
			row.plant(t, f.cell)
			if _, complete, err := checkpointComplete(context.Background(), f.cell.path("recovery", id), id); complete || !errors.Is(err, row.err) {
				t.Fatal("witness open reported another completeness or error")
			}
		})
	}
}

// A handle whose own stat fails examined nothing ((*os.File).Stat of a closed
// handle fails on every OS): openRegularBy returns the reader's inspect error,
// never irregular, and the handle left open is examined and returned. With
// checkpointComplete's three errors (witness_open) the failure over a witness
// its real open reads complete is errCheckpointWitnessUnread, the error it
// returns (witness_proven), never absence or irregular: never incomplete.
func TestCheckpointWitnessHandleStat(t *testing.T) {
	const id = "ckpt_abcdefghijklmnop"
	fixture, _ := checkpointFixture(t, "legacy", false)
	dir := checkpointPlant(t, fixture.cell, id, id)
	if _, complete, err := checkpointComplete(context.Background(), dir, id); !complete || err != nil {
		t.Fatal("the planted witness does not read complete through the real open")
	}
	inspect, irregular, open := errors.New("synthetic inspect"), errors.New("synthetic irregular"), errors.New("synthetic open")
	for _, row := range []struct {
		name                           string
		closed                         bool
		inspect, irregular, open, want error
	}{
		{"closed", true, inspect, irregular, open, inspect},
		{"examined", false, inspect, irregular, open, nil},
		{"checkpoint", true, errCheckpointWitnessUnread, errCheckpointWitnessIrregular, errCheckpointWitnessUnread, errCheckpointWitnessUnread},
	} {
		t.Run(row.name, func(t *testing.T) {
			f, err := openRegularBy(context.Background(), func(_ context.Context, path string) (*os.File, error) {
				handle, err := os.Open(path)
				if err == nil && row.closed {
					_ = handle.Close()
				}
				return handle, err
			}, nil, filepath.Join(dir, checkpointWitnessName), row.inspect, row.irregular, row.open)
			if f != nil {
				_ = f.Close()
			}
			incomplete := errors.Is(err, os.ErrNotExist) || errors.Is(err, errCheckpointWitnessIrregular)
			if err != row.want || (f == nil) == (err == nil) || incomplete || err != nil && ClassifyCheckpointFailure(err) != (CheckpointFailure{Class: "local_checkpoint_operation_failed"}) {
				t.Fatal("a handle whose stat failed was read as irregular or absent, or an examined one refused")
			}
		})
	}
}

// checkpointWitnessStatement is a statement's outermost expressions printed by
// types.ExprString in source order, each followed by a semicolon.
func checkpointWitnessStatement(statement ast.Stmt) string {
	var text strings.Builder
	ast.Inspect(statement, func(node ast.Node) bool {
		if expression, ok := node.(ast.Expr); ok {
			text.WriteString(types.ExprString(expression) + ";")
			return false
		}
		return true
	})
	return text.String()
}

// checkpointWitnessContract pins the rule of checkpointComplete: its only
// absence is openRegular's, unread is returned, the read keeps the 1 MiB
// bound, and listCheckpoints returns what was not read.
func checkpointWitnessContract(source string) []string {
	file, err := parser.ParseFile(token.NewFileSet(), "checkpoint.go", strings.ReplaceAll(source, "\r\n", "\n"), 0)
	if err != nil {
		return []string{"parse:checkpoint.go"}
	}
	bodies, declared := map[string][]ast.Stmt{}, map[string]int{}
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Body != nil {
			bodies[function.Name.Name] = function.Body.List
			declared[function.Name.Name]++
		}
	}
	statement := func(function string, index int) string {
		if body := bodies[function]; declared[function] == 1 && index < len(body) {
			return checkpointWitnessStatement(body[index])
		}
		return ""
	}
	var failures []string
	for key, ok := range map[string]bool{
		"witness_open": len(bodies["checkpointComplete"]) == 4 &&
			statement("checkpointComplete", 0) == "f;err;openRegular(ctx, filepath.Join(dir, checkpointWitnessName), errCheckpointWitnessUnread, errCheckpointWitnessIrregular, errCheckpointWitnessUnread);",
		"witness_proven": statement("checkpointComplete", 1) == "errors.Is(err, os.ErrNotExist) || errors.Is(err, errCheckpointWitnessIrregular);checkpointWitness{};false;nil;err != nil;checkpointWitness{};false;err;",
		"witness_bound":  statement("checkpointWitnessRead", 0) == "data;err;io.ReadAll(io.LimitReader(r, 1 << 20));",
		"witness_unread": statement("checkpointWitnessRead", 1) == "err != nil;checkpointWitness{};false;errCheckpointWitnessUnread;",
	} {
		if !ok {
			failures = append(failures, key)
		}
	}
	// The one call in listCheckpoints is followed by the return of its error.
	calls, returned := 0, 0
	if declared["listCheckpoints"] == 1 {
		ast.Inspect(&ast.BlockStmt{List: bodies["listCheckpoints"]}, func(node ast.Node) bool {
			if block, ok := node.(*ast.BlockStmt); ok {
				for index, current := range block.List {
					if checkpointWitnessStatement(current) != "witness;complete;err;checkpointComplete(context.Background(), dir, entry.Name());" {
						continue
					}
					if calls++; index+1 < len(block.List) && checkpointWitnessStatement(block.List[index+1]) == "err != nil;nil;0;err;" {
						returned++
					}
				}
			}
			return true
		})
	}
	if calls != 1 || returned != 1 {
		failures = append(failures, "witness_listed")
	}
	slices.Sort(failures)
	return failures
}

func TestCheckpointWitnessSourceContract(t *testing.T) {
	source := strings.ReplaceAll(admissionReadSources(t)["checkpoint.go"], "\r\n", "\n")
	for _, ending := range []string{"\n", "\r\n"} {
		if failures := checkpointWitnessContract(strings.ReplaceAll(source, "\n", ending)); len(failures) != 0 {
			t.Fatalf("checkpoint witness contract (%q endings): %v", ending, failures)
		}
	}
	for _, row := range []struct{ name, before, after, want string }{
		{"absence_unproven", "errors.Is(err, os.ErrNotExist) || errors.Is(err, errCheckpointWitnessIrregular)", "err != nil", "witness_proven"},
		{"open_error_incomplete", "\t\treturn checkpointWitness{}, false, err\n", "\t\treturn checkpointWitness{}, false, nil\n", "witness_proven"},
		{"read_error_incomplete", "\t\treturn checkpointWitness{}, false, errCheckpointWitnessUnread\n", "\t\treturn checkpointWitness{}, false, nil\n", "witness_unread"},
		{"bound_changed", "io.LimitReader(r, 1<<20)", "io.LimitReader(r, 1<<10)", "witness_bound"},
		{"waiting_open", "openRegular(ctx, filepath.Join(dir, checkpointWitnessName),", "openRegularBy(ctx, openPrivateFile, nil, filepath.Join(dir, checkpointWitnessName),", "witness_open"},
		{"unread_listed", "\t\t\treturn nil, 0, err\n", "\t\t\tcontinue\n", "witness_listed"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if strings.Count(source, row.before) != 1 {
				t.Fatal("checkpoint witness contract anchor is not unique")
			}
			for _, ending := range []string{"\n", "\r\n"} {
				if failures := checkpointWitnessContract(strings.ReplaceAll(strings.Replace(source, row.before, row.after, 1), "\n", ending)); !slices.Contains(failures, row.want) {
					t.Fatalf("checkpoint witness contract reported %v, want %s", failures, row.want)
				}
			}
		})
	}
}
