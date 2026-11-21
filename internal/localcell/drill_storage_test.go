package localcell

import (
	"context"
	"errors"
	"go/ast"
	"go/build"
	"go/parser"
	"go/types"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The platform test files add their admission and volume rows here.
var drillStorageAdmissionRows, drillStorageVolumeRows []func(*testing.T)

// drillStorageCall is one admission; drillStorageAll is a fake volume with all space free.
type drillStorageCall struct {
	ctx       context.Context
	id        string
	allowance uint64
	volume    func(*os.File) (drillVolume, error)
}

var drillStorageAll = drillStorageFree(4096, math.MaxUint64)

// drillStorageSnapshot records each object below the cell root: name, type, mode, a file's size and bytes.
var drillStorageSnapshot = checkpointTreeState

// drillStorageReason requires exactly the closed, value-free refusal want.
func drillStorageReason(t *testing.T, err error, want, message string) {
	t.Helper()
	drillAssert(t, err == error(drillStorageError{reason: want}) && err.Error() == "drill storage refused", message)
}

// drillStorageSeam replaces the volume seam until the test ends.
func drillStorageSeam(t *testing.T, seam *func(*os.File) (drillVolume, error), fake func(*os.File) (drillVolume, error)) {
	t.Helper()
	prior := *seam
	*seam = fake
	t.Cleanup(func() { *seam = prior })
}
func drillStorageFree(unit, free uint64) func(*os.File) (drillVolume, error) {
	return func(*os.File) (drillVolume, error) { return drillVolume{unit: unit, free: free}, nil }
}

// drillStorageOpened identifies the volume of the directory at name, opened read-only.
func drillStorageOpened(t *testing.T, name string) (drillVolume, error) {
	f, err := os.Open(name)
	drillAssert(t, err == nil, "cannot open the drill storage volume fixture")
	defer func() { _ = f.Close() }()
	return drillPlatformVolume(f)
}

// drillStorageAdmit runs one admission on v's cell, whose lock v holds; the cell tree must not change.
func drillStorageAdmit(t *testing.T, v *checkpointVerifyCase, call drillStorageCall) (drillStorageEstimate, error) {
	t.Helper()
	drillStorageSeam(t, &drillStorageVolume, call.volume)
	before := drillStorageSnapshot(t, v.c)
	estimate, err := v.c.admitDrillStorageLocked(call.ctx, call.id, call.allowance)
	drillAssert(t, reflect.DeepEqual(before, drillStorageSnapshot(t, v.c)), "drill storage admission changed the cell tree")
	return estimate, err
}

// drillStorageNeed recomputes v's estimate without the production arithmetic: 1 MiB,
// 2U, 256 per entry, U per directory, each file and the allowance rounded up to U.
func drillStorageNeed(v *checkpointVerifyCase, unit, allowance uint64) drillStorageEstimate {
	clone := uint64(1<<20) + 2*unit
	for _, entry := range v.inventory.Entries {
		if clone += 256; entry.Type == "dir" {
			clone += unit
		} else {
			clone += (uint64(*entry.Size) + unit - 1) / unit * unit
		}
	}
	allowance = (allowance + unit - 1) / unit * unit
	return drillStorageEstimate{unit: unit, clone: clone, allowance: allowance, need: clone + allowance}
}

// drillStorageJournal appends line to the cell's journal.
func drillStorageJournal(v *checkpointVerifyCase, line string) {
	data := retainedRead(v.t, v.c, "journal/operations.jsonl")
	drillAssert(v.t, writeAtomicBytes(v.c.path("journal", "operations.jsonl"), append(data, line...), nil) == nil, "cannot prepare the drill storage journal")
}

func TestDrillStorageAdmission(t *testing.T) {
	t.Run("estimate", func(t *testing.T) {
		v := newCheckpointVerifyCase(t, false)
		for _, unit := range []uint64{4096, 65536} {
			want := drillStorageNeed(v, unit, 12345)
			want.free = math.MaxUint64
			got, err := drillStorageAdmit(t, v, drillStorageCall{t.Context(), v.id, 12345, drillStorageFree(unit, math.MaxUint64)})
			drillAssert(t, err == nil && got == want, "drill storage estimate differs from its recomputation")
		}
	})
	t.Run("estimate/malformed", func(t *testing.T) {
		link, file := []checkpointInventoryEntry{{Type: "link", Size: new(int64)}}, []checkpointInventoryEntry{{Type: "file"}}
		drillAssert(t, drillCloneBytes(link, checkpointBlock) == math.MaxUint64 && drillCloneBytes(file, checkpointBlock) == math.MaxUint64, "drill storage estimated a malformed entry")
	})
	t.Run("exact_boundary", func(t *testing.T) {
		v := newCheckpointVerifyCase(t, false)
		want := drillStorageNeed(v, 4096, 1)
		want.free = want.need + drillStorageFloor
		got, err := drillStorageAdmit(t, v, drillStorageCall{t.Context(), v.id, 1, drillStorageFree(4096, want.free)})
		drillAssert(t, err == nil && got == want, "drill storage refused exactly its need and floor")
		_, err = drillStorageAdmit(t, v, drillStorageCall{t.Context(), v.id, 1, drillStorageFree(4096, want.free-1)})
		drillStorageReason(t, err, "storage_insufficient", "drill storage admitted one byte short of its floor")
	})
	t.Run("exact_boundary/rounding", func(t *testing.T) {
		drillAssert(t, drillStorageRound(4096, 4096) == 4096 && drillStorageRound(4097, 4096) == 8192 && drillStorageRound(0, 4096) == 0 &&
			drillStorageRound(5, 0) == math.MaxUint64, "drill storage rounded a size wrongly")
	})
	t.Run("overflow", func(t *testing.T) {
		size := int64(math.MaxInt64)
		files := []checkpointInventoryEntry{{Path: "a", Type: "file", Size: &size}, {Path: "b", Type: "file", Size: &size}}
		drillAssert(t, drillStorageAdd(math.MaxUint64-1, 2) == math.MaxUint64 && drillStorageMul(math.MaxUint64, 2) == math.MaxUint64 &&
			drillStorageRound(math.MaxUint64, 4096) == math.MaxUint64 && drillCloneBytes(files, 4096) == math.MaxUint64 &&
			!drillStorageAdmits(math.MaxUint64, math.MaxUint64) && !drillStorageAdmits(math.MaxUint64, math.MaxUint64-drillStorageFloor), "drill storage arithmetic wrapped")
	})
	t.Run("overflow/resealed", func(t *testing.T) {
		v := newCheckpointVerifyCase(t, false)
		manifest := v.entry("manifest.json")
		size := math.MaxInt64 - (v.witness.Bytes - *manifest.Size)
		manifest.Size, v.witness.Bytes = &size, math.MaxInt64
		v.inventorySeal()
		_, err := drillStorageAdmit(t, v, drillStorageCall{t.Context(), v.id, math.MaxInt64, drillStorageAll})
		drillStorageReason(t, err, "storage_insufficient", "drill storage admitted a saturated need")
	})
	drill, torn := drillFixture().DrillID, `{"operation":"stop"`
	for _, row := range [][2]string{{"unsupported_fs", "unsupported_fs"}, {"boundary_refused/zero_unit", "boundary_refused"}, {"boundary_refused/absent", "boundary_refused"},
		{"boundary_refused/file", "boundary_refused"}, {"boundary_refused/not_private", "boundary_refused"}, {"boundary_refused/noncanonical", "boundary_refused"}, {"outstanding/declared", "drill_outstanding"}, {"outstanding/remainder", "drill_outstanding"},
		{"outstanding/foreign", "drill_outstanding"}, {"outstanding/unknown", "drill_outstanding"}, {"outstanding/file", "drill_outstanding"}, {"outstanding/alias", "drill_outstanding"},
		{"checkpoint_absent", "checkpoint_absent"}, {"checkpoint_unverified/digest", "checkpoint_unverified"}, {"checkpoint_unverified/identity", "checkpoint_unverified"},
		{"checkpoint_unverified/foreign", "checkpoint_unverified"}, {"checkpoint_unverified/count", "checkpoint_unverified"}, {"journal_pending/pending", "journal_pending"},
		{"journal_pending/torn", "journal_pending"}, {"allowance_invalid", "allowance_invalid"}, {"identity_invalid", "identity_invalid"}, {"cancelled", "cancelled"}} {
		t.Run(row[0], func(t *testing.T) {
			v := newCheckpointVerifyCase(t, false)
			call, then, recovery, ready := drillStorageCall{t.Context(), v.id, 1, drillStorageAll}, func() {}, v.c.recoveryDir(), true
			switch row[0] {
			case "unsupported_fs":
				call.volume = func(*os.File) (drillVolume, error) { return drillVolume{}, drillStorageError{reason: "unsupported_fs"} }
			case "boundary_refused/zero_unit":
				call.volume = drillStorageFree(0, math.MaxUint64)
			case "boundary_refused/absent":
				ready = os.RemoveAll(recovery) == nil
			case "boundary_refused/file": // Windows reads "not a directory" as not-exist.
				ready = os.RemoveAll(recovery) == nil && writeAtomicBytes(recovery, []byte("private file"), nil) == nil
			case "boundary_refused/not_private": // 0644 on Unix; a plain Windows create inherits the parent ACL.
				f, err := os.OpenFile(filepath.Join(recovery, "exposed"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
				ready = err == nil && errors.Join(f.Close(), os.Chmod(filepath.Join(recovery, "exposed"), 0o644)) == nil
			case "boundary_refused/noncanonical": // case-insensitive volumes resolve the other spelling; Linux does not
				ready = os.Rename(recovery, filepath.Join(v.c.cfg.Root, "Recovery")) == nil
			case "outstanding/declared", "outstanding/foreign", "outstanding/alias": // a declared record, a foreign one, an upper-cased name
				record, name := drillFixture(), map[bool]string{false: drill, true: strings.ToUpper(drill)}[row[0] == "outstanding/alias"]
				record.Binding.InstallationID = map[bool]string{false: v.witness.InstallationID, true: "another_installation"}[row[0] == "outstanding/foreign"]
				drillRootPlant(t, v.c, name, record)
			case "outstanding/remainder":
				ready = privateDir(filepath.Join(recovery, drill), true) == nil
			case "outstanding/unknown":
				ready = privateDir(filepath.Join(recovery, drill), true) == nil && writeAtomicBytes(filepath.Join(recovery, drill, "content"), []byte("unknown"), nil) == nil
			case "outstanding/file":
				ready = writeAtomicBytes(filepath.Join(recovery, drill), []byte("private file"), nil) == nil
			case "checkpoint_absent":
				call.id = "ckpt_aaaaaaaaaaaaaaaa"
			case "checkpoint_unverified/digest":
				v.witness.InventorySHA256 = strings.Repeat("0", 64)
				v.document(checkpointWitnessName, v.witness)
			case "checkpoint_unverified/identity": // the documents refuse with the whole inventory decoded
				v.inventory.CheckpointID = "ckpt_bbbbbbbbbbbbbbbb"
				v.inventorySeal()
			case "checkpoint_unverified/foreign":
				v.witness.InstallationID = "another_installation"
				v.document(checkpointWitnessName, v.witness)
			case "checkpoint_unverified/count":
				v.witness.Files++
				v.document(checkpointWitnessName, v.witness)
			case "journal_pending/pending":
				drillStorageJournal(v, `{"operation":"stop","step":"operation","state":"started","at":"2026-01-01T00:00:00Z"}`+"\n")
			case "journal_pending/torn":
				drillStorageJournal(v, torn)
			case "allowance_invalid": // the torn tail proves both come before any file system access
				drillStorageJournal(v, torn)
				call.allowance, then = 0, func() { call.allowance = math.MaxInt64 + 1 }
			case "identity_invalid":
				drillStorageJournal(v, torn)
				call.id, then = strings.ToUpper(v.id), func() { call.id = drill }
			case "cancelled": // before the call, then by the volume, before the documents are read
				ctx, cancel := context.WithCancel(call.ctx)
				cancel()
				call.ctx, then = ctx, func() {
					ctx, cancel := context.WithCancel(t.Context())
					call.ctx, call.volume = ctx, func(f *os.File) (drillVolume, error) { cancel(); return drillStorageAll(f) }
				}
			}
			drillAssert(t, ready, "cannot prepare the drill storage fixture")
			for _, change := range []func(){func() {}, then} {
				change()
				_, err := drillStorageAdmit(t, v, call)
				drillStorageReason(t, err, row[1], "drill storage admission gave another outcome")
			}
		})
	}
	for _, rows := range drillStorageAdmissionRows {
		rows(t)
	}
}

func TestDrillStorageVolume(t *testing.T) {
	t.Run("decide", func(t *testing.T) {
		judged := drillLinuxSupported(0xEF53, 4096, 4096) && !drillLinuxSupported(0xEF53, 0, 4096) && drillWindowsSupported("NTFS", false, false) &&
			!drillWindowsSupported("NTFS", true, false) && !drillWindowsSupported("NTFS", false, true)
		for _, magic := range []uint64{0x01021994, 0x9123683E, 0x58465342, 0x794C7630, 0x6969, 0x65735546} { // tmpfs, btrfs, xfs, overlay, nfs, fuse
			judged = judged && !drillLinuxSupported(magic, 4096, 4096)
		}
		for _, name := range []string{"ReFS", "FAT32", "exFAT", "ntfs"} {
			judged = judged && !drillWindowsSupported(name, false, false)
		}
		drillAssert(t, judged, "drill storage misjudged a volume")
	})
	for _, rows := range drillStorageVolumeRows {
		rows(t)
	}
}

// drillStorageCallees are the only functions, methods and function-typed variables or fields declared
// elsewhere that the drill storage files name, as built here; x/sys members do not resolve here, and
// the checkpoint contract pins them as system selectors and the ntdll procedure as drillStorageProc.
var drillStorageCallees = []string{"(context.Context).Err", "context.Background", "(*regexp.Regexp).MatchString", "errors.As", "strings.ToLower", "strings.EqualFold", "os.OpenRoot",
	"(*os.Root).Open", "(*os.Root).Close", "(*os.File).Close", "(*os.File).Fd", "localcell.readManifest", "localcell.drillRootNames", "localcell.drillValidID",
	"localcell.verifyCheckpointDocuments", "localcell.checkpointVerifyShape", "(*localcell.Cell).readJournal", "(*localcell.Cell).validateRecovery",
	"(*localcell.Cell).recoveryDir", "(*localcell.Cell).checkpointDir", "(*localcell.Cell).checkpointOps"}

// drillReserveCallees are drillStorageCallees' counterpart for the reservation files (G4B1: the Linux
// platform layer); x/sys and runtimeconfig members do not resolve here, and the checkpoint contract pins them.
// The sequence (G4B2) adds the rest; the volume seam resolves to drillPlatformVolume.
var drillReserveCallees = []string{"errors.As", "errors.Is", "(*os.Root).Open", "(*os.File).Close", "(*os.File).Fd", "(*os.File).Stat",
	"(io/fs.FileInfo).Size", "os.NewFile", "strings.Contains", "strings.Split", "localcell.drillStorageAdd", "path/filepath.FromSlash", "path/filepath.Join",
	"strings.Repeat", "localcell.checkpointPathFits", "os.SameFile", "(*os.Root).OpenRoot", "(*os.Root).Stat", "(*os.Root).Close",
	"(context.Context).Err", "context.Background", "encoding/json.Marshal", "errors.Join", "os.OpenRoot", "os.Lstat", "path.Join", "(*os.File).Sync", "(*os.File).Write",
	"(*os.Root).Lstat", "(*os.Root).Remove", "(*os.Root).Rename", "localcell.drillPlatformVolume", "(*localcell.Cell).admitDrillStorageLocked",
	"(*localcell.Cell).checkpointDir", "(*localcell.Cell).checkpointOps", "(*localcell.Cell).recoveryDir", "localcell.readManifest",
	"localcell.verifyCheckpointDocuments", "localcell.checkpointVerifyShape", "localcell.drillCloneBytes", "localcell.drillStorageAdmits",
	"localcell.drillValidOperationID", "localcell.drillValidID", "localcell.drillParseRecord", "(*localcell.Cell).drillRootStage", "localcell.drillRootSync",
	"(*localcell.Cell).deleteDrillLocked", "localcell.drillCloneShare", "localcell.drillStorageRound", "localcell.drillRootNames", "path.Dir", "io/fs.ErrExist"}

// drillReservePlatformCallees hold the platform files (drill_reserve_<system>.go) to G4B1's list and the two decisions of
// drill_reserve.go they call: a name only drill_reserve.go needs stays refused in them (drill_reserve__callee:<file>:<name>).
var drillReservePlatformCallees = []string{"errors.As", "errors.Is", "(*os.Root).Open", "(*os.File).Close", "(*os.File).Fd", "(*os.File).Stat",
	"(io/fs.FileInfo).Size", "os.NewFile", "strings.Contains", "strings.Split", "localcell.drillStorageAdd", "localcell.drillExtentsCover", "localcell.drillWindowsProved",
	"localcell.drillWindowsInherits"}

// drillStorageReferences returns drill_storage_callee:<file>:<name> per name outside drillStorageCallees.
func drillStorageReferences(sources map[string]string) []string {
	return drillCalleeReferences(sources, "drill_storage", drillStorageCallees)
}

// drillCalleeReferences type-checks the sources built here with their standard imports from source (as
// writerReferences) and returns <prefix>_callee:<file>:<name> per name outside allowed that a file whose
// name starts with prefix names: a call, a value, a renamed or dot import's member, or a package variable
// declared as one in any file.
func drillCalleeReferences(sources map[string]string, prefix string, allowed []string) (failures []string) {
	platform, files, own, admit := build.Default, []*ast.File(nil), map[string]*ast.File{}, map[string]bool{}
	lf := func(name string) string { return strings.ReplaceAll(sources[filepath.Base(name)], "\r\n", "\n") }
	platform.OpenFile = func(name string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(lf(name))), nil }
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		match, err := platform.MatchFile(".", name)
		file, parseErr := parser.ParseFile(readers4Fset, name, lf(name), 0)
		if err != nil || parseErr != nil || !match {
			continue
		}
		if files = append(files, file); strings.HasPrefix(name, prefix) {
			own[name] = file
		}
		for _, spec := range file.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			admit[path] = !strings.Contains(strings.Split(path, "/")[0], ".")
		}
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	config := types.Config{Error: func(error) {}, Importer: readers4Importer(func(path string) (*types.Package, error) {
		if !admit[path] {
			return nil, errors.New("import not admitted")
		}
		return readers4Std.Import(path)
	})}
	_, _ = config.Check("localcell", readers4Fset, files, info)
	aliases := map[types.Object]types.Object{}
	for _, initializer := range info.InitOrder {
		if selector, ok := ast.Unparen(initializer.Rhs).(*ast.SelectorExpr); ok {
			aliases[initializer.Lhs[0]] = info.Uses[selector.Sel]
		} else if identifier, ok := ast.Unparen(initializer.Rhs).(*ast.Ident); ok {
			aliases[initializer.Lhs[0]] = info.Uses[identifier]
		}
	}
	for name, file := range own {
		ast.Inspect(file, func(node ast.Node) bool {
			identifier, _ := node.(*ast.Ident)
			object, key := info.Uses[identifier], ""
			for depth := 0; depth < 8 && aliases[object] != nil; depth++ {
				object = aliases[object]
			}
			switch object := object.(type) {
			case *types.Func:
				key = object.FullName()
			case *types.Var: // a function-typed variable or field; from a reservation file any package variable (G13)
				if _, function := object.Type().Underlying().(*types.Signature); object.Pkg() != nil && (function || strings.HasPrefix(prefix, "drill_reserve") && object.Parent() == object.Pkg().Scope()) {
					key = object.Pkg().Path() + "." + object.Name()
				}
			}
			if key != "" && !slices.Contains(allowed, key) {
				if declared := readers4Fset.File(object.Pos()); declared != nil && own[declared.Name()] == nil {
					failures = append(failures, prefix+"_callee:"+name+":"+key)
				}
			}
			return true
		})
	}
	slices.Sort(failures)
	return slices.Compact(failures)
}

// The drill storage files name no writer, sync or lock by declaration: an alias declared elsewhere, a
// lock, a dot import, a renamed import's method value and a function-typed field each report, LF and CRLF.
func TestDrillStorageSourceContract(t *testing.T) {
	sources := admissionReadSources(t)
	for name, source := range sources {
		sources[name] = strings.ReplaceAll(source, "\r\n", "\n")
	}
	const tail, imports = "func drillStorageAdd(", "\t\"strings\"\n)"
	for _, row := range []struct {
		name, want string
		edits      [][3]string // file, anchor, text put before the anchor
	}{
		{"control", "", nil},
		{"alias_elsewhere", "localcell.syncDirectory", [][3]string{{"files.go", "func syncDirectory(", "var drillStorageProbeSync = syncDirectory\n\n"},
			{"drill_storage.go", tail, "func (c *Cell) drillStorageProbe() { _ = drillStorageProbeSync(c.recoveryDir()) }\n\n"}}},
		{"lock", "localcell.acquireLock", [][3]string{{"drill_storage.go", tail, "func (c *Cell) drillStorageProbe() { _, _ = acquireLock(c.cfg.Root, true) }\n\n"}}},
		{"dot_import", "io.WriteString", [][3]string{{"drill_storage.go", imports, "\t. \"io\"\n"}, {"drill_storage.go", tail, "func drillStorageProbe(w Writer) { _, _ = WriteString(w, \"x\") }\n\n"}}},
		{"renamed_import", "(*os.File).Write", [][3]string{{"drill_storage.go", imports, "\tfiles \"os\"\n"}, {"drill_storage.go", tail, "func drillStorageProbe(f *files.File) { write := f.Write; _, _ = write(nil) }\n\n"}}},
		{"field_value", "localcell.publish", [][3]string{{"drill_storage.go", tail, "func (c *Cell) drillStorageProbe() { _ = c.checkpointOps(context.Background()).publish }\n\n"}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			for _, ending := range []string{"\n", "\r\n"} {
				input := maps.Clone(sources)
				for _, edit := range row.edits {
					drillAssert(t, strings.Count(input[edit[0]], edit[1]) == 1, "drill storage contract anchor is not unique")
					input[edit[0]] = strings.Replace(input[edit[0]], edit[1], edit[2]+edit[1], 1)
				}
				for name, source := range input {
					input[name] = strings.ReplaceAll(source, "\n", ending)
				}
				failures := drillStorageReferences(input)
				if !slices.Equal(failures, []string{"drill_storage_callee:drill_storage.go:" + row.want}) && (row.want != "" || len(failures) != 0) {
					t.Fatalf("drill storage source contract reported %v", failures)
				}
			}
		})
	}
}

// The reservation files name no pathname writer, sync, remover, writer or lock by declaration: each
// probe in drill_reserve.go reports exactly its own key, LF and CRLF.
func TestDrillReserveSourceContract(t *testing.T) {
	sources := admissionReadSources(t)
	for name, source := range sources {
		sources[name] = strings.ReplaceAll(source, "\r\n", "\n")
	}
	const tail = "func drillExtentsCover("
	for _, row := range []struct{ name, want, probe string }{
		{"control", "", ""},
		{"pathname_writer", "localcell.makePrivateDir", "func (c *Cell) drillReserveProbe() { _ = makePrivateDir(c.recoveryDir()) }\n\n"},
		{"sync_elsewhere", "localcell.syncDirectory", "func (c *Cell) drillReserveProbe() { _ = syncDirectory(c.recoveryDir()) }\n\n"},
		{"remover", "localcell.removePrivateFile", "func drillReserveProbe() { _ = removePrivateFile(\"x\") }\n\n"},
		{"writer", "localcell.writeAtomicBytes", "func drillReserveProbe() { _ = writeAtomicBytes(\"x\", nil, nil) }\n\n"},
		{"lock", "localcell.acquireLock", "func (c *Cell) drillReserveProbe() { _, _ = acquireLock(c.cfg.Root, true) }\n\n"},
		{"remove_all", "(*os.Root).RemoveAll", "func drillReserveProbe(r *os.Root) { _ = r.RemoveAll(\"x\") }\n\n"},
		{"truncate", "(*os.File).Truncate", "func drillReserveProbe(f *os.File) { _ = f.Truncate(0) }\n\n"},
		{"package_variable", "localcell.checkpointIDPattern", "func drillReserveProbe() { checkpointIDPattern = nil }\n\n"}, // G13: any type
		{"variable_initializer", "localcell.checkpointIDPattern", "var drillReserveProbe = func() int { checkpointIDPattern = nil; return 0 }()\n\n"},
	} {
		t.Run(row.name, func(t *testing.T) {
			var want []string
			if row.want != "" {
				want = append(want, "drill_reserve_callee:drill_reserve.go:"+row.want)
			}
			// c.recoveryDir joins drillReserveCallees with the Windows creators; until then a probe that spells it
			// reports it too.
			if strings.Contains(row.probe, "c.recoveryDir()") && !slices.Contains(drillReserveCallees, "(*localcell.Cell).recoveryDir") {
				want = append(want, "drill_reserve_callee:drill_reserve.go:(*localcell.Cell).recoveryDir")
			}
			slices.Sort(want)
			for _, ending := range []string{"\n", "\r\n"} {
				input := maps.Clone(sources)
				drillAssert(t, strings.Count(input["drill_reserve.go"], tail) == 1, "drill reserve contract anchor is not unique")
				input["drill_reserve.go"] = strings.Replace(input["drill_reserve.go"], tail, row.probe+tail, 1)
				for name, source := range input {
					input[name] = strings.ReplaceAll(source, "\n", ending)
				}
				failures := append(drillCalleeReferences(input, "drill_reserve", drillReserveCallees), drillCalleeReferences(input, "drill_reserve_", drillReservePlatformCallees)...)
				if !slices.Equal(failures, want) {
					t.Fatalf("drill reserve source contract reported %v", failures)
				}
			}
		})
	}
	// The platform files keep G4B1's names: a name only drill_reserve.go needs is refused in the host's own.
	t.Run("platform_close", func(t *testing.T) {
		for _, ending := range []string{"\n", "\r\n"} {
			input := maps.Clone(sources)
			for _, name := range []string{"drill_reserve_linux.go", "drill_reserve_windows.go", "drill_reserve_other.go"} {
				drillAssert(t, strings.Count(input[name], "func drillPlatformAllocate(") == 1, "drill reserve contract anchor is not unique")
				input[name] = strings.ReplaceAll(strings.Replace(input[name], "func drillPlatformAllocate(", "func drillReserveProbe(r *os.Root) { _ = r.Close() }\n\nfunc drillPlatformAllocate(", 1), "\n", ending)
			}
			failures := drillCalleeReferences(input, "drill_reserve_", drillReservePlatformCallees)
			drillAssert(t, len(failures) == 1 && strings.HasPrefix(failures[0], "drill_reserve__callee:drill_reserve_") && strings.HasSuffix(failures[0], ".go:(*os.Root).Close"),
				"a name only the portable file needs was admitted in a platform file")
		}
	})
}
