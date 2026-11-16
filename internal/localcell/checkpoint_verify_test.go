package localcell

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type checkpointVerifyCase struct {
	t         *testing.T
	c         *Cell
	id, dir   string
	lock      *operationLock
	inventory checkpointInventory
	witness   checkpointWitness
}

func newCheckpointVerifyCase(t *testing.T, selected bool) *checkpointVerifyCase {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	layout := "legacy"
	if selected {
		layout = "selected"
	}
	f, _ := checkpointFixture(t, layout, false)
	if f.cell.Checkpoint(t.Context(), checkpointOptions(f)) != nil {
		t.Fatal("cannot prepare sealed fixture")
	}
	ids := checkpointIDs(t, f.cell)
	if len(ids) != 1 {
		t.Fatal("fixture identity count refused")
	}
	return openCheckpointVerifyCase(t, f.cell, ids[0])
}

func openCheckpointVerifyCase(t *testing.T, c *Cell, id string) *checkpointVerifyCase {
	t.Helper()
	dir, err := c.checkpointDir(id)
	if err != nil {
		t.Fatal("fixture directory resolution failed")
	}
	lock, err := acquireLock(c.cfg.Root, false)
	if err != nil || lock == nil {
		t.Fatal("fixture lock acquisition failed")
	}
	t.Cleanup(func() { _ = lock.Close() })
	v := &checkpointVerifyCase{t: t, c: c, id: id, dir: dir, lock: lock}
	if json.Unmarshal(v.read(checkpointInventoryName), &v.inventory) != nil || json.Unmarshal(v.read(checkpointWitnessName), &v.witness) != nil {
		t.Fatal("fixture document decoding failed")
	}
	return v
}

// Each mutating row owns a private copy; the writer compatibility and preservation
// rows still exercise a freshly written checkpoint. Bases live only in this test.
func checkpointVerifyFactory(parent *testing.T) func(*testing.T, bool) *checkpointVerifyCase {
	bases := map[bool]*checkpointVerifyCase{}
	return func(t *testing.T, selected bool) *checkpointVerifyCase {
		t.Helper()
		switch filepath.Base(t.Name()) {
		case "complete", "complete_selected", "live_manifest_newer", "read_only":
			return newCheckpointVerifyCase(t, selected)
		}
		base := bases[selected]
		if base == nil {
			base = newCheckpointVerifyCase(parent, selected)
			_ = base.lock.Close()
			bases[selected] = base
		}
		t.Setenv("WITSELF_HOME", t.TempDir())
		t.Setenv("DSH_HOME", t.TempDir())
		cfg := base.c.cfg
		cfg.Root = filepath.Join(t.TempDir(), "cell")
		err := filepath.WalkDir(base.c.cfg.Root, func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(base.c.cfg.Root, name)
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			dest := filepath.Join(cfg.Root, rel)
			if entry.IsDir() {
				err = makePrivateDir(dest)
			} else if info.Mode().IsRegular() {
				var data []byte
				data, err = os.ReadFile(name)
				if err == nil {
					err = os.WriteFile(dest, data, info.Mode().Perm())
				}
			} else {
				return errors.New("fixture copy entry refused")
			}
			if err != nil {
				return err
			}
			return os.Chmod(dest, info.Mode().Perm())
		})
		if err != nil {
			t.Fatal("fixture private copy failed")
		}
		c, err := New(cfg)
		if err != nil {
			t.Fatal("fixture copy configuration failed")
		}
		c.checkpointTest = &checkpointOperations{}
		return openCheckpointVerifyCase(t, c, base.id)
	}
}

func (v *checkpointVerifyCase) read(name string) []byte {
	v.t.Helper()
	data, err := os.ReadFile(filepath.Join(v.dir, filepath.FromSlash(name)))
	if err != nil {
		v.t.Fatal("fixture document reading failed")
	}
	return data
}
func (v *checkpointVerifyCase) write(name string, data []byte) {
	v.t.Helper()
	if writeAtomicBytes(filepath.Join(v.dir, filepath.FromSlash(name)), data, nil) != nil {
		v.t.Fatal("fixture material writing failed")
	}
}
func (v *checkpointVerifyCase) document(name string, value any) {
	v.t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		v.t.Fatal("fixture document encoding failed")
	}
	v.write(name, data)
}
func (v *checkpointVerifyCase) seal() {
	digest := sha256.Sum256(v.read(checkpointInventoryName))
	v.witness.InventorySHA256 = hex.EncodeToString(digest[:])
	v.document(checkpointWitnessName, v.witness)
}
func (v *checkpointVerifyCase) inventorySeal() {
	v.document(checkpointInventoryName, v.inventory)
	v.seal()
}
func (v *checkpointVerifyCase) entry(name string) *checkpointInventoryEntry {
	v.t.Helper()
	for i := range v.inventory.Entries {
		if v.inventory.Entries[i].Path == name {
			return &v.inventory.Entries[i]
		}
	}
	v.t.Fatal("fixture inventory entry absent")
	return nil
}
func (v *checkpointVerifyCase) contentSeal(name string) {
	data := v.read(name)
	entry := v.entry(name)
	digest, size := sha256.Sum256(data), int64(len(data))
	v.witness.Bytes += size - *entry.Size
	entry.Size, entry.SHA256 = &size, hex.EncodeToString(digest[:])
	v.inventorySeal()
}
func (v *checkpointVerifyCase) check(ctx context.Context, want string) {
	v.t.Helper()
	got, err := v.c.verifyCheckpoint(ctx, v.lock, v.id)
	if want == "integrity_verified" {
		if err != nil || string(got) != want {
			v.t.Fatal("integrity positive outcome missing")
		}
		return
	}
	var refusal checkpointVerifyError
	if err == nil || !errors.As(err, &refusal) || string(refusal.reason) != want || string(got) != "" || err.Error() != "checkpoint integrity verification refused" {
		v.t.Fatalf("integrity refusal reason differs: %s", want)
	}
}

func TestCheckpointVerify(t *testing.T) {
	fixture := checkpointVerifyFactory(t)
	const target = "pg/base/1/1259"
	rows := []struct {
		name, want string
		change     func(*checkpointVerifyCase)
	}{
		{"complete", "integrity_verified", nil},
		{"complete_selected", "integrity_verified", nil},
		{"changed_byte", "file_mismatch", func(v *checkpointVerifyCase) { data := v.read(target); data[0] ^= 1; v.write(target, data) }},
		// Keep the original size while binding the shortened bytes' digest, isolating the size comparison.
		{"truncated", "file_mismatch", func(v *checkpointVerifyCase) {
			data := v.read(target)
			data = data[:len(data)-1]
			v.write(target, data)
			digest := sha256.Sum256(data)
			v.entry(target).SHA256 = hex.EncodeToString(digest[:])
			v.inventorySeal()
		}},
		{"extra_entry", "tree_mismatch", func(v *checkpointVerifyCase) { v.write("extra", []byte("fixture")) }},
		{"missing_file", "tree_mismatch", func(v *checkpointVerifyCase) {
			if os.Remove(filepath.Join(v.dir, filepath.FromSlash(target))) != nil {
				v.t.Fatal("fixture file removal failed")
			}
		}},
		{"type_swap", "tree_mismatch", func(v *checkpointVerifyCase) {
			name := filepath.Join(v.dir, filepath.FromSlash(target))
			if os.Remove(name) != nil || privateDir(name, true) != nil {
				v.t.Fatal("fixture type replacement failed")
			}
		}},
		{"duplicate_path", "inventory_malformed", func(v *checkpointVerifyCase) {
			v.inventory.Entries = append(v.inventory.Entries[:1], v.inventory.Entries...)
			v.inventorySeal()
		}},
		{"unsorted_path", "inventory_malformed", func(v *checkpointVerifyCase) { slices.Reverse(v.inventory.Entries); v.inventorySeal() }},
		{"witness_identity", "identity_mismatch", func(v *checkpointVerifyCase) {
			v.witness.CheckpointID = "ckpt_bbbbbbbbbbbbbbbb"
			v.document(checkpointWitnessName, v.witness)
		}},
		{"inventory_identity", "identity_mismatch", func(v *checkpointVerifyCase) { v.inventory.CheckpointID = "ckpt_bbbbbbbbbbbbbbbb"; v.inventorySeal() }},
		{"inventory_digest", "inventory_digest_mismatch", func(v *checkpointVerifyCase) {
			v.write(checkpointInventoryName, append(v.read(checkpointInventoryName), ' '))
		}},
		{"manifest_mismatch", "manifest_mismatch", func(v *checkpointVerifyCase) {
			var m Manifest
			if json.Unmarshal(v.read("manifest.json"), &m) != nil {
				v.t.Fatal("fixture manifest decoding failed")
			}
			m.InstallationID += "x"
			v.document("manifest.json", m)
			v.contentSeal("manifest.json")
		}},
		{"manifest_trailing_object", "manifest_mismatch", func(v *checkpointVerifyCase) {
			v.write("manifest.json", append(v.read("manifest.json"), []byte("{}")...))
			v.contentSeal("manifest.json")
		}},
		{"manifest_trailing_junk", "manifest_mismatch", func(v *checkpointVerifyCase) {
			v.write("manifest.json", append(v.read("manifest.json"), 'x'))
			v.contentSeal("manifest.json")
		}},
		{"live_manifest_newer", "integrity_verified", func(v *checkpointVerifyCase) {
			m, err := readManifest(v.c.cfg.Root)
			if err != nil {
				v.t.Fatal("fixture live manifest reading failed")
			}
			m.LastVerifiedSchemaVersion++
			if writeAtomic(v.c.path("manifest.json"), m, nil) != nil {
				v.t.Fatal("fixture live manifest writing failed")
			}
		}},
		{"complete_alone", "file_mismatch", func(v *checkpointVerifyCase) {
			for _, entry := range v.inventory.Entries {
				if entry.Type == "file" {
					data := v.read(entry.Path)
					for i := range data {
						data[i] ^= 1
					}
					v.write(entry.Path, data)
				}
			}
		}},
		{"empty_installation", "witness_malformed", func(v *checkpointVerifyCase) {
			v.witness.InstallationID = ""
			v.document(checkpointWitnessName, v.witness)
		}},
		{"negative_count", "witness_malformed", func(v *checkpointVerifyCase) { v.witness.Files = -1; v.document(checkpointWitnessName, v.witness) }},
		{"bad_witness_digest", "witness_malformed", func(v *checkpointVerifyCase) {
			v.witness.InventorySHA256 = strings.Repeat("A", 64)
			v.document(checkpointWitnessName, v.witness)
		}},
		{"large_witness", "witness_malformed", func(v *checkpointVerifyCase) {
			v.write(checkpointWitnessName, append(v.read(checkpointWitnessName), bytes.Repeat([]byte(" "), checkpointControlLimit)...))
		}},
		{"witness_exact_limit", "integrity_verified", func(v *checkpointVerifyCase) {
			data := v.read(checkpointWitnessName)
			v.write(checkpointWitnessName, append(data, bytes.Repeat([]byte(" "), checkpointControlLimit-len(data))...))
		}},
		{"witness_limit_plus_one", "witness_malformed", func(v *checkpointVerifyCase) {
			data := v.read(checkpointWitnessName)
			data = append(data, bytes.Repeat([]byte(" "), checkpointControlLimit-len(data))...)
			v.write(checkpointWitnessName, append(data, '\n'))
		}},
		{"file_size_negative", "inventory_malformed", func(v *checkpointVerifyCase) { size := int64(-1); v.entry(target).Size = &size; v.inventorySeal() }},
		{"file_size_missing", "inventory_malformed", func(v *checkpointVerifyCase) { v.entry(target).Size = nil; v.inventorySeal() }},
		{"file_digest_missing", "inventory_malformed", func(v *checkpointVerifyCase) { v.entry(target).SHA256 = ""; v.inventorySeal() }},
		{"file_digest_uppercase", "inventory_malformed", func(v *checkpointVerifyCase) { v.entry(target).SHA256 = strings.Repeat("A", 64); v.inventorySeal() }},
		{"entry_type_unknown", "inventory_malformed", func(v *checkpointVerifyCase) { v.entry(target).Type = "unknown"; v.inventorySeal() }},
		{"directory_size", "inventory_malformed", func(v *checkpointVerifyCase) { size := int64(0); v.entry("pg").Size = &size; v.inventorySeal() }},
		{"directory_digest", "inventory_malformed", func(v *checkpointVerifyCase) { v.entry("pg").SHA256 = strings.Repeat("a", 64); v.inventorySeal() }},
		{"directory_empty_digest", "inventory_malformed", func(v *checkpointVerifyCase) {
			before := []byte(`{"path":"pg","type":"dir"}`)
			data := v.read(checkpointInventoryName)
			if bytes.Count(data, before) != 1 {
				v.t.Fatal("directory field anchor not unique")
			}
			v.write(checkpointInventoryName, bytes.Replace(data, before, []byte(`{"path":"pg","type":"dir","sha256":""}`), 1))
			v.seal()
		}},
		{"postgres_major", "manifest_mismatch", func(v *checkpointVerifyCase) {
			v.witness.PostgresMajor = "17"
			v.document(checkpointWitnessName, v.witness)
		}},
		{"layout_version", "manifest_mismatch", func(v *checkpointVerifyCase) { v.witness.LayoutVersion++; v.document(checkpointWitnessName, v.witness) }},
		{"principal_layout_version", "manifest_mismatch", func(v *checkpointVerifyCase) {
			v.witness.PrincipalLayoutVersion++
			v.document(checkpointWitnessName, v.witness)
		}},
		{"schema_version", "manifest_mismatch", func(v *checkpointVerifyCase) { v.witness.SchemaVersion++; v.document(checkpointWitnessName, v.witness) }},
		{"manifest_malformed", "manifest_mismatch", func(v *checkpointVerifyCase) { v.write("manifest.json", []byte("{")); v.contentSeal("manifest.json") }},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			v := fixture(t, row.name == "complete_selected")
			if row.change != nil {
				row.change(v)
			}
			v.check(t.Context(), row.want)
		})
	}
	t.Run("read_only", func(t *testing.T) {
		v := fixture(t, false)
		before := checkpointTreeState(t, v.c)
		v.check(t.Context(), "integrity_verified")
		if !reflect.DeepEqual(before, checkpointTreeState(t, v.c)) {
			t.Fatal("integrity success changed fixture tree")
		}
		v.write(target, []byte("changed"))
		before = checkpointTreeState(t, v.c)
		v.check(t.Context(), "file_mismatch")
		if !reflect.DeepEqual(before, checkpointTreeState(t, v.c)) {
			t.Fatal("integrity refusal changed fixture tree")
		}
	})
	t.Run("lock_required", func(t *testing.T) {
		v := fixture(t, false)
		v.lock = nil
		opened := false
		v.c.checkpointTest.open = func(string) (*os.File, error) { opened = true; return nil, errors.New("injected refusal") }
		v.check(t.Context(), "lock_required")
		if opened {
			t.Fatal("unowned verification opened fixture")
		}
	})
	t.Run("boundary_refusal", func(t *testing.T) {
		v := fixture(t, false)
		if os.Rename(v.c.recoveryDir(), v.c.path("kept")) != nil || writeAtomicBytes(v.c.recoveryDir(), []byte("fixture"), nil) != nil {
			t.Fatal("boundary fixture replacement failed")
		}
		want := v.c.validateRecovery()
		_, got := v.c.verifyCheckpoint(t.Context(), v.lock, v.id)
		var expected, actual checkpointStateError
		if !errors.As(want, &expected) || !errors.As(got, &actual) || expected != actual {
			t.Fatal("boundary refusal identity changed")
		}
		v.lock = nil
		v.check(t.Context(), "lock_required")
	})
	for _, kind := range []string{"files", "directories", "bytes"} {
		t.Run("count_mismatch/"+kind, func(t *testing.T) {
			v := fixture(t, false)
			switch kind {
			case "files":
				v.witness.Files++
			case "directories":
				v.witness.Directories++
			case "bytes":
				v.witness.Bytes++
			}
			v.document(checkpointWitnessName, v.witness)
			v.check(t.Context(), "count_mismatch")
		})
	}
	for _, row := range []struct{ name, path string }{
		{"parent", "../outside"}, {"absolute", "/outside"}, {"backslash", `pg\outside`}, {"empty_segment", "pg//outside"},
		{"empty", ""}, {"dot", "./outside"}, {"nul", "pg/\x00"}, {"inventory", checkpointInventoryName}, {"witness", checkpointWitnessName},
	} {
		t.Run("escape/"+row.name, func(t *testing.T) {
			v := fixture(t, false)
			v.entry(target).Path = row.path
			slices.SortFunc(v.inventory.Entries, func(a, b checkpointInventoryEntry) int { return strings.Compare(a.Path, b.Path) })
			v.inventorySeal()
			v.check(t.Context(), "path_refused")
		})
	}
	checkpointVerifyDocumentCases(t, fixture)
	checkpointVerifyOpenCases(t, target, fixture)
	t.Run("offline_only", func(t *testing.T) {
		if string(checkpointIntegrityVerified) != "integrity_verified" || reflect.TypeFor[checkpointVerifyResult]().Kind() != reflect.String {
			t.Fatal("integrity assurance constant changed")
		}
		source, err := os.ReadFile("checkpoint_verify.go")
		if err != nil {
			t.Fatal("integrity source reading failed")
		}
		file, err := parser.ParseFile(token.NewFileSet(), "", source, 0)
		if err != nil {
			t.Fatal("integrity source parsing failed")
		}
		positive := 0
		ast.Inspect(file, func(node ast.Node) bool {
			if identifier, ok := node.(*ast.Ident); ok && identifier.Obj != nil {
				switch identifier.Obj.Decl.(type) {
				case *ast.ValueSpec, *ast.TypeSpec:
					for _, word := range []string{"restore", "recovery", "restorable", "usable"} {
						if strings.Contains(strings.ToLower(identifier.Name), word) {
							t.Fatal("integrity identifier assurance expanded")
						}
					}
				}
			}
			if decl, ok := node.(*ast.ValueSpec); ok {
				for _, value := range decl.Values {
					if literal, ok := value.(*ast.BasicLit); ok && literal.Kind == token.STRING {
						text, _ := strconv.Unquote(literal.Value)
						if text == "integrity_verified" {
							positive++
						}
						for _, word := range []string{"restore", "recovery", "restorable", "usable", "tested-by-start"} {
							if strings.Contains(strings.ToLower(text), word) {
								t.Fatal("integrity vocabulary assurance expanded")
							}
						}
					}
				}
			}
			return true
		})
		if positive != 1 {
			t.Fatal("integrity positive constant count changed")
		}
	})
}

func checkpointVerifyDocumentCases(t *testing.T, fixture func(*testing.T, bool) *checkpointVerifyCase) {
	t.Helper()
	rows := []struct{ name, before, after string }{
		{"duplicate_field", `"format":1`, `"format":1,"format":1`},
		{"unknown_field", `"format":1`, `"format":1,"unknown":0`},
		{"unknown_format", `"format":1`, `"format":2`},
		{"missing_field", `"format":1,`, ""},
		{"wrong_type", `"format":1`, `"format":"1"`},
		{"null_field", `"format":1`, `"format":null`},
		{"trailing_data", "", " {}"},
	}
	for _, row := range rows {
		for _, inventory := range []bool{false, true} {
			which := "witness"
			if inventory {
				which = "inventory"
			}
			t.Run(row.name+"/"+which, func(t *testing.T) {
				v := fixture(t, false)
				name, want := checkpointWitnessName, "witness_malformed"
				if inventory {
					name, want = checkpointInventoryName, "inventory_malformed"
				}
				data := v.read(name)
				var compact bytes.Buffer
				if json.Compact(&compact, data) != nil {
					t.Fatal("document mutation compacting failed")
				}
				data = compact.Bytes()
				if row.before == "" {
					data = append(data, row.after...)
				} else {
					if bytes.Count(data, []byte(row.before)) != 1 {
						t.Fatal("document mutation anchor not unique")
					}
					data = bytes.Replace(data, []byte(row.before), []byte(row.after), 1)
				}
				v.write(name, data)
				if inventory {
					v.seal()
				}
				if row.name == "unknown_format" {
					want = "format_unsupported"
				}
				v.check(t.Context(), want)
			})
		}
	}
	for _, row := range []struct{ name, document, before, after, want string }{
		{"duplicate_control", checkpointWitnessName, `"control":{`, `"control":{"state":"ignored",`, "witness_malformed"},
		{"unknown_control", checkpointWitnessName, `"control":{`, `"control":{"unknown":"ignored",`, "witness_malformed"},
		{"duplicate_entry", checkpointInventoryName, `"entries":[{`, `"entries":[{"path":"ignored",`, "inventory_malformed"},
		{"unknown_entry", checkpointInventoryName, `"entries":[{`, `"entries":[{"unknown":0,`, "inventory_malformed"},
	} {
		t.Run(row.name, func(t *testing.T) {
			v := fixture(t, false)
			data := v.read(row.document)
			var compact bytes.Buffer
			if json.Compact(&compact, data) != nil {
				t.Fatal("nested mutation compacting failed")
			}
			data = compact.Bytes()
			if bytes.Count(data, []byte(row.before)) != 1 {
				t.Fatal("nested mutation anchor not unique")
			}
			v.write(row.document, bytes.Replace(data, []byte(row.before), []byte(row.after), 1))
			if row.document == checkpointInventoryName {
				v.seal()
			}
			v.check(t.Context(), row.want)
		})
	}
	for _, row := range []struct{ name, document, field, value, want string }{
		{"control_not_object", checkpointWitnessName, "control", "[]", "witness_malformed"},
		{"null_control", checkpointWitnessName, "control", "null", "witness_malformed"},
		{"null_entries", checkpointInventoryName, "entries", "null", "inventory_malformed"},
		{"missing_entries", checkpointInventoryName, "entries", "", "inventory_malformed"},
	} {
		t.Run(row.name, func(t *testing.T) {
			v := fixture(t, false)
			var object map[string]json.RawMessage
			if json.Unmarshal(v.read(row.document), &object) != nil {
				t.Fatal("shape fixture decoding failed")
			}
			if row.value == "" {
				delete(object, row.field)
			} else {
				object[row.field] = json.RawMessage(row.value)
			}
			v.document(row.document, object)
			if row.document == checkpointInventoryName {
				v.seal()
			}
			v.check(t.Context(), row.want)
		})
	}
	t.Run("semantic_documents", func(t *testing.T) {
		v := fixture(t, false)
		for _, name := range []string{checkpointInventoryName, checkpointWitnessName} {
			var object map[string]json.RawMessage
			if json.Unmarshal(v.read(name), &object) != nil {
				t.Fatal("semantic fixture decoding failed")
			}
			data, err := json.MarshalIndent(object, "", "  ")
			if err != nil {
				t.Fatal("semantic fixture encoding failed")
			}
			v.write(name, append(data, '\n'))
		}
		// Rebinding preserves the differently ordered witness document.
		var object map[string]json.RawMessage
		if json.Unmarshal(v.read(checkpointWitnessName), &object) != nil {
			t.Fatal("semantic witness decoding failed")
		}
		digest := sha256.Sum256(v.read(checkpointInventoryName))
		value, err := json.Marshal(hex.EncodeToString(digest[:]))
		if err != nil {
			t.Fatal("semantic digest encoding failed")
		}
		object["inventory_sha256"] = value
		v.document(checkpointWitnessName, object)
		v.check(t.Context(), "integrity_verified")
	})
}

func checkpointVerifyOpenCases(t *testing.T, target string, fixture func(*testing.T, bool) *checkpointVerifyCase) {
	t.Helper()
	for _, row := range []struct {
		name, document, want string
		after                int
	}{
		{"unreadable_not_mismatch", target, "file_unreadable", 0}, {"read_unreadable", target, "file_unreadable", 0}, {"witness_unreadable", checkpointWitnessName, "witness_unreadable", 0},
		{"inventory_unreadable", checkpointInventoryName, "inventory_unreadable", 0}, {"manifest_unreadable", "manifest.json", "manifest_unreadable", 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			v := fixture(t, false)
			original := v.c.checkpointOps().open
			calls := 0
			v.c.checkpointTest.open = func(name string) (*os.File, error) {
				if name == filepath.Join(v.dir, filepath.FromSlash(row.document)) {
					calls++
					if calls > row.after {
						if row.name == "read_unreadable" {
							file, err := original(name)
							if err == nil {
								_ = file.Close()
							}
							return file, err
						}
						return nil, errors.New("injected refusal")
					}
				}
				return original(name)
			}
			v.check(t.Context(), row.want)
		})
	}
	for mode := range 3 {
		readFailure := mode == 1
		name := "transient_open"
		if readFailure {
			name = "transient_read"
		}
		if mode == 2 {
			name = "retry_budget"
		}
		t.Run(name, func(t *testing.T) {
			v := fixture(t, false)
			original := v.c.checkpointOps().open
			calls, sleeps := 0, 0
			now := time.Now()
			var slept time.Duration
			failure := errors.New("injected transient")
			var prefix []byte
			var partialChecks []func()
			if readFailure {
				prefix = bytes.Repeat([]byte("partial"), 1<<20)
				v.write(target, append(append([]byte(nil), prefix...), 'x'))
				v.contentSeal(target)
			}
			v.c.checkpointTest.open = func(name string) (*os.File, error) {
				if name == filepath.Join(v.dir, filepath.FromSlash(target)) {
					calls++
					if calls <= 2 || mode == 2 {
						if !readFailure {
							return nil, failure
						}
						file, writer, err := os.Pipe()
						if err != nil {
							return nil, err
						}
						// Completing this write exceeds platform pipe capacity, so
						// the failed read has consumed a real prefix before closure.
						done := make(chan struct{})
						var written bool
						t.Cleanup(func() { _ = file.Close(); _ = writer.Close(); <-done })
						go func() {
							n, err := writer.Write(prefix)
							written = err == nil && n == len(prefix)
							_ = file.Close()
							_ = writer.Close()
							close(done)
						}()
						partialChecks = append(partialChecks, func() {
							<-done
							if !written {
								t.Fatal("transient partial read injection failed")
							}
						})
						return file, nil
					}
				}
				return original(name)
			}
			v.c.checkpointTest.transient = func(err error) bool { return errors.Is(err, failure) || errors.Is(err, os.ErrClosed) }
			v.c.checkpointTest.now = func() time.Time { return now }
			v.c.checkpointTest.sleep = func(_ context.Context, delay time.Duration) {
				sleeps++
				slept += delay
				if mode != 2 {
					now = now.Add(delay)
				}
			}
			if mode == 2 {
				v.check(t.Context(), "file_unreadable")
				if slept != checkpointRetryFloor+time.Duration(v.witness.Files)*checkpointRetryPerFile {
					t.Fatal("bounded retry allowance differs")
				}
				return
			}
			v.check(t.Context(), "integrity_verified")
			for _, check := range partialChecks {
				check()
			}
			if calls != 3 || sleeps != 2 {
				t.Fatal("transient retry attempt count differs")
			}
		})
	}
	for _, row := range []struct {
		name, want    string
		retry         bool
		opens, sleeps int
	}{
		{"transient_inventory_second_pass", "integrity_verified", true, 2, 1},
		{"terminal_inventory_second_pass", "inventory_unreadable", false, 1, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			v := fixture(t, false)
			original := v.c.checkpointOps().open
			data := v.read(checkpointInventoryName)
			calls, seeks, sleeps := 0, 0, 0
			done := make(chan struct{})
			var written bool
			v.c.checkpointTest.open = func(name string) (*os.File, error) {
				if name == filepath.Join(v.dir, checkpointInventoryName) {
					calls++
					if calls == 1 {
						file, writer, err := os.Pipe()
						if err != nil {
							t.Fatal("inventory pipe creation failed")
						}
						t.Cleanup(func() { _ = file.Close(); _ = writer.Close(); <-done })
						go func() {
							n, err := writer.Write(data)
							written = err == nil && n == len(data)
							_ = writer.Close()
							close(done)
						}()
						return file, nil
					}
				}
				return original(name)
			}
			v.c.checkpointTest.transient = func(err error) bool {
				var pathError *fs.PathError
				if errors.As(err, &pathError) && pathError.Op == "seek" {
					seeks++
					return row.retry
				}
				return false
			}
			now := time.Time{}
			v.c.checkpointTest.now = func() time.Time { return now }
			v.c.checkpointTest.sleep = func(_ context.Context, delay time.Duration) { sleeps++; now = now.Add(delay) }
			v.check(t.Context(), row.want)
			<-done
			if !written {
				t.Fatal("inventory pipe write incomplete")
			}
			if seeks != 1 {
				t.Fatal("inventory seek refusal count differs")
			}
			if calls != row.opens {
				t.Fatal("inventory opener count differs")
			}
			if sleeps != row.sleeps {
				t.Fatal("inventory retry sleep count differs")
			}
		})
	}
	t.Run("transient_inventory_second_read", func(t *testing.T) {
		v := fixture(t, false)
		ops := v.c.checkpointOps()
		original := ops.open
		calls, sleeps := 0, 0
		failure := errors.New("injected transient")
		ops.open = func(name string) (*os.File, error) { calls++; return original(name) }
		ops.transient = func(err error) bool { return errors.Is(err, failure) }
		ops.sleep = func(context.Context, time.Duration) { sleeps++ }
		var buffer bytes.Buffer
		budget := checkpointRetryBudget{limit: checkpointRetryFloor}
		err := checkpointVerifyRead(t.Context(), &budget, ops, filepath.Join(v.dir, checkpointInventoryName), func(file *os.File) error {
			reader := checkpointVerifyCountingReader{reader: file}
			if calls == 1 {
				reader.readErr = failure
			}
			matched, err := checkpointVerifyInventory(&reader, v.witness.InventorySHA256, &buffer)
			if calls == 1 && (reader.secondBytes == 0 || !errors.Is(err, failure)) {
				t.Fatal("transient partial read injection failed")
			}
			if err == nil && !matched {
				t.Fatal("integrity positive outcome missing")
			}
			return err
		})
		if err != nil || calls != 2 || sleeps != 1 || !bytes.Equal(buffer.Bytes(), v.read(checkpointInventoryName)) {
			t.Fatal("transient retry attempt count differs")
		}
		v.check(t.Context(), "integrity_verified")
	})
	t.Run("cancelled", func(t *testing.T) {
		v := fixture(t, false)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		original := v.c.checkpointOps().open
		late := false
		v.c.checkpointTest.open = func(name string) (*os.File, error) {
			if ctx.Err() != nil {
				late = true
			}
			file, err := original(name)
			if filepath.Base(name) == checkpointInventoryName {
				cancel()
			}
			return file, err
		}
		v.check(ctx, "cancelled")
		if late {
			t.Fatal("cancelled verification opened another file")
		}
	})
}

type checkpointVerifyCountingReader struct {
	reader                         io.ReadSeeker
	after                          []byte
	seekErr, readErr               error
	seeks, firstBytes, secondBytes int
}

func (r *checkpointVerifyCountingReader) Read(p []byte) (int, error) {
	if r.seeks > 0 && r.readErr != nil {
		if r.secondBytes > 0 {
			return 0, r.readErr
		}
		p = p[:1]
	}
	n, err := r.reader.Read(p)
	if r.seeks == 0 {
		r.firstBytes += n
	} else {
		r.secondBytes += n
	}
	return n, err
}

func (r *checkpointVerifyCountingReader) Seek(offset int64, whence int) (int64, error) {
	r.seeks++
	if r.seekErr != nil {
		return 0, r.seekErr
	}
	if r.after != nil {
		r.reader = bytes.NewReader(r.after)
	}
	return r.reader.Seek(offset, whence)
}

func TestCheckpointVerifyInventory(t *testing.T) {
	data := []byte("inventory")
	digest := sha256.Sum256(data)
	expected := hex.EncodeToString(digest[:])
	failure := errors.New("injected refusal")
	for _, row := range []struct {
		name, expected string
		after          []byte
		seekErr        error
		matched        bool
		seeks          int
	}{
		{"inventory_mismatch_single_pass", "", nil, nil, false, 0},
		{"inventory_changed_between_passes", expected, []byte("InventorY"), nil, false, 1},
		{"inventory_grew_between_passes", expected, bytes.Repeat(data, 3), nil, false, 1},
		{"inventory_match", expected, nil, nil, true, 1},
		{"inventory_seek_failure", expected, nil, failure, false, 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			reader := checkpointVerifyCountingReader{reader: bytes.NewReader(data), after: row.after, seekErr: row.seekErr}
			buffer := *bytes.NewBufferString("previous attempt")
			matched, err := checkpointVerifyInventory(&reader, row.expected, &buffer)
			if matched != row.matched || !errors.Is(err, row.seekErr) || reader.seeks != row.seeks || reader.firstBytes != len(data) {
				t.Fatal("integrity inventory binding differs")
			}
			if buffer.Len() > len(data)+1 || reader.secondBytes > len(data)+1 || (row.seeks == 0 && buffer.Len() != 0) {
				t.Fatal("integrity inventory buffering differs")
			}
			if matched && !bytes.Equal(buffer.Bytes(), data) {
				t.Fatal("integrity positive outcome missing")
			}
		})
	}
}
