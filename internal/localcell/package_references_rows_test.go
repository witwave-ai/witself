package localcell

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// Anchors the rows share; each matches its file exactly once.
const (
	referencesAtRecover  = "func (c *Cell) recoverPostgresRecord("
	referencesAtProgress = "// journalProgress contains only"
	referencesAtPending  = "func (c *Cell) sweepPostgresPending() {"
	referencesAtImports  = "import (\n"
	referencesAtVersion  = "\tif version, err := readRegular(openPostgresReader, func(info os.FileInfo) bool { return info.Size() <= 64 }, c.path(\"pg\", \"PG_VERSION\"), errCheckpointSource"
	referencesAtJournal  = "\tf, err := openRegularWriter(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600, irregular, errors.New(\"cannot open local cell journal\"))\n\tif err != nil {\n\t\treturn err\n\t}\n"
	referencesAtTail     = "\tf, err := openRegularWriter(c.path(\"journal\", \"operations.jsonl\"), os.O_WRONLY, 0, torn, torn)\n"
)

// packageReferencesRows edit the sources in memory: each edit is a file, a
// mode, an anchor and a text. Modes: before, after or replace the anchor; "+"
// adds the file, "=" copies the anchor's file, "-" drops it; "guard" adds a
// guarded name and "dir" a directory entry. A row names the keys it reports.
var packageReferencesRows = []struct {
	name  string
	edits [][4]string
	keys  []string
}{
	{"same_file_wrapper", [][4]string{{"files.go", "after", referencesAtJournal, "\t_ = f.Close()\n\tif f, err = reopenJournal(path); err != nil {\n\t\treturn err\n\t}\n"},
		{"files.go", "before", referencesAtProgress, "func reopenJournal(path string) (*os.File, error) {\n\treturn os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)\n}\n\n"}}, []string{"ref:files.go:reopenJournal:os.OpenFile"}},
	{"package_alias", [][4]string{{"files.go", "before", referencesAtProgress, "var journalOpener = openPrivateFile\n\n"},
		{"files.go", "after", referencesAtJournal, "\t_ = f.Close()\n\tif f, err = journalOpener(path, os.O_WRONLY|os.O_APPEND, 0); err != nil {\n\t\treturn err\n\t}\n"}}, []string{"ref:files.go:(*Cell).journalCause:openPrivateFile", "ref:files.go:journalOpener:openPrivateFile"}},
	{"function_value", [][4]string{{"files.go", "before", referencesAtTail, "\topen := os.OpenFile\n\t_, _ = open(c.path(\"journal\", \"operations.jsonl\"), os.O_WRONLY, 0)\n"}}, []string{"ref:files.go:(*Cell).discardJournalTail:os.OpenFile"}},
	{"other_file_site", [][4]string{{"reinstall.go", "after", "\tif err := c.reinstallBoundary(\"operation_manifest\"); err != nil {\n\t\treturn err\n\t}\n",
		"\tif f, err := openPrivateFile(c.path(\"journal\", \"operations.jsonl\"), os.O_WRONLY|os.O_APPEND, 0); err == nil {\n\t\t_ = f.Close()\n\t}\n"}}, []string{"ref:reinstall.go:(*Cell).finishReinstall:openPrivateFile"}},
	{"helper_existing_file", [][4]string{{"postgres_pending.go", "before", referencesAtPending, "func zzHelperOpen(path string) (*os.File, error) { return os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0) }\n\n"},
		{"commands.go", "before", "\treturn openRegularWriter(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600, irregular, errors.New(\"cannot open local cell log\"))\n", "\tif probe, err := zzHelperOpen(path); err == nil {\n\t\t_ = probe.Close()\n\t}\n"}}, []string{"ref:postgres_pending.go:zzHelperOpen:os.OpenFile"}},
	{"alias_existing_file", [][4]string{{"postgres_pending.go", "before", referencesAtPending, "var zzAliasOpen = openPrivateFile\n\n"},
		{"checkpoint.go", "before", referencesAtVersion, "\tif probe, err := zzAliasOpen(filepath.Join(c.cfg.Root, \"pg\", \"PG_VERSION\"), os.O_RDWR|os.O_APPEND, 0); err == nil {\n\t\t_ = probe.Close()\n\t}\n"}}, []string{"ref:postgres_pending.go:zzAliasOpen:openPrivateFile", "ref:checkpoint.go:(*Cell).admitCheckpoint:openPrivateFile"}},
	{"existing_seam_elsewhere", [][4]string{{"checkpoint.go", "before", referencesAtVersion, "\tif probe, err := openUpdateJournalFile(filepath.Join(c.cfg.Root, \"pg\", \"PG_VERSION\"), os.O_RDONLY, 0); err == nil {\n\t\t_ = probe.Close()\n\t}\n"}}, []string{"ref:checkpoint.go:(*Cell).admitCheckpoint:openPrivateFile"}},
	{"renamed_import", [][4]string{{"lifecycle.go", "after", referencesAtImports, "\tzzos \"os\"\n"},
		{"lifecycle.go", "before", referencesAtRecover, "func zzRenamed(path string) { _, _ = zzos.ReadFile(path) }\n\n"}}, []string{"ref:lifecycle.go:zzRenamed:os.ReadFile"}},
	{"dot_import", [][4]string{{"zz_dot.go", "+", "", "package localcell\n\nimport . \"io/ioutil\"\n\nfunc zzDot() { _, _ = ReadFile(\".\") }\n"}}, []string{"file:zz_dot.go", "ref:zz_dot.go:zzDot:io/ioutil.ReadFile"}},
	{"method_value", [][4]string{{"lifecycle.go", "before", referencesAtRecover, "func zzMethodValue(root *os.Root) {\n\topen := root.Open\n\t_, _ = open(\".\")\n}\n\n"}}, []string{"ref:lifecycle.go:zzMethodValue:(*os.Root).Open"}},
	{"interface_dispatch", [][4]string{{"lifecycle.go", "before", referencesAtRecover, "func zzIface(root *os.Root) {\n\tvar r interface{ Open(string) (*os.File, error) } = root\n\t_, _ = r.Open(\".\")\n}\n\n"}}, []string{"ref:lifecycle.go:zzIface:(*os.Root).Open"}},
	{"field_use", [][4]string{{"checkpoint.go", "after", "\tfail := func(cause error) error { _ = discard(); return cause }\n\tplanned := make(map[string]bool, len(plan.entries))\n", "\tif f, err := ops.open(dir); err == nil {\n\t\t_ = f.Close()\n\t}\n"}}, []string{"ref:checkpoint.go:(*Cell).inventoryCheckpoint:openPrivateReader"}},
	{"generic_value", [][4]string{{"lifecycle.go", "before", referencesAtRecover, "func zzGeneric[F ~func(string) (*os.File, error)](f F) { _, _ = f(\".\") }\n\nfunc zzGenericCall() { zzGeneric(os.Open) }\n\n"}}, []string{"ref:lifecycle.go:zzGenericCall:os.Open"}},
	{"other_spelling", [][4]string{{"checkpoint.go", "after", referencesAtImports, "\t\"io/ioutil\"\n"},
		{"checkpoint.go", "before", referencesAtVersion, "\tif _, err := ioutil.ReadFile(filepath.Join(c.cfg.Root, \"pg\", \"PG_VERSION\")); err != nil {\n\t\treturn nil, errCheckpointSource\n\t}\n"}}, []string{"ref:checkpoint.go:(*Cell).admitCheckpoint:io/ioutil.ReadFile"}},
	{"windows_file", [][4]string{{"files_windows.go", "after", "func readPrivateFile(path string) ([]byte, error) {\n", "\tif data, err := os.ReadFile(path); err == nil {\n\t\treturn data, nil\n\t}\n"}}, []string{"ref:files_windows.go:readPrivateFile:os.ReadFile"}},
	{"darwin_file", [][4]string{{"process_darwin.go", "after", "func processIdentity(pid int) (string, bool, error) {\n", "\tif f, err := unix.Open(\"/dev/null\", unix.O_RDONLY, 0); err == nil {\n\t\t_ = unix.Close(f)\n\t}\n"}}, []string{"ref:process_darwin.go:processIdentity:golang.org/x/sys/unix.Open"}},
	{"other_system_file", [][4]string{{"checkpoint_other.go", "before", "\treturn nil, nil, errCheckpointState\n", "\t_, _ = os.Open(\".\")\n"}}, []string{"ref:checkpoint_other.go:(*Cell).openCheckpointSource:os.Open"}},
	{"platform_twin", [][4]string{{"files_linux_twin.go", "=", "files_other.go", ""},
		{"files_linux_twin.go", "replace", "//go:build !windows\n", "//go:build linux\n"},
		{"files_linux_twin.go", "replace", "\treturn os.OpenFile(path, flags|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, mode)\n}\n\n// blockingWriter", "\treturn os.OpenFile(path, flags, mode)\n}\n\n// blockingWriter"},
		{"files_other.go", "replace", "//go:build !windows\n", "//go:build !windows && !linux\n"}}, []string{"file:files_linux_twin.go", "ref:files_linux_twin.go:openPrivateWriter:os.OpenFile"}},
	{"custom_tag_twin", [][4]string{{"zz_twin.go", "+", "", "//go:build witselfreviewtwin\n\npackage localcell\n\nimport \"os\"\n\nfunc zzTwin() { _, _ = os.Open(\".\") }\n"}}, []string{"file:zz_twin.go", "uncompiled:zz_twin.go"}},
	{"linkname", [][4]string{{"zz_link.go", "+", "", "package localcell\n\nimport (\n\t\"os\"\n\t_ \"unsafe\"\n)\n\n//go:linkname zzOpen os.Open\nfunc zzOpen(name string) (*os.File, error)\n"}}, []string{"linkname:zz_link.go", "bodyless:zz_link.go:zzOpen"}},
	{"reflective_call", [][4]string{{"lifecycle.go", "after", referencesAtImports, "\t\"reflect\"\n"},
		{"lifecycle.go", "before", referencesAtRecover, "func zzReflect(f *os.File) { _ = reflect.ValueOf(f).MethodByName(\"Sync\").Call(nil) }\n\n"}}, []string{"ref:lifecycle.go:zzReflect:(reflect.Value).MethodByName", "ref:lifecycle.go:zzReflect:(reflect.Value).Call"}},
	{"unsafe_pointer", [][4]string{{"lifecycle.go", "after", referencesAtImports, "\t\"unsafe\"\n"},
		{"lifecycle.go", "before", referencesAtRecover, "func zzUnsafe(p *int) uintptr { return uintptr(unsafe.Pointer(p)) }\n\n"}}, []string{"ref:lifecycle.go:zzUnsafe:unsafe.Pointer"}},
	{"raw_syscall", [][4]string{{"process_linux.go", "before", "func processIdentity(", "func zzRaw() { _, _, _ = syscall.Syscall(syscall.SYS_GETPID, 0, 0, 0) }\n\n"}}, []string{"ref:process_linux.go:zzRaw:syscall.Syscall"}},
	{"dll_procedure", [][4]string{{"stop_windows.go", "before", "var (\n", "var zzProc = windows.NewLazySystemDLL(\"kernel32.dll\").NewProc(\"CreateFileW\")\n\n"}}, []string{"ref:stop_windows.go:zzProc:golang.org/x/sys/windows.NewLazySystemDLL", "ref:stop_windows.go:zzProc:(*golang.org/x/sys/windows.LazyDLL).NewProc"}},
	{"reference_removed", [][4]string{{"files.go", "replace", "\td, err := os.Open(path + string(filepath.Separator) + \".\")\n", "\td, err := openPlainDirectory(path)\n"},
		{"files.go", "before", referencesAtProgress, "func openPlainDirectory(string) (*os.File, error) { return nil, errInterrupted }\n\n"}}, []string{"ref:files.go:syncDirectory:os.Open"}},
	{"file_removed", [][4]string{{"doc.go", "-", "", ""}}, []string{"file:doc.go"}},
	{"type_error", [][4]string{{"lifecycle.go", "before", referencesAtRecover, "func zzBroken() { _ = zzUndefined }\n\n"}}, []string{"types:linux:lifecycle.go"}},
	{"unknown_name", [][4]string{{"", "guard", "", "os.OpenFil"}}, []string{"unknown:os.OpenFil"}},
	{"foreign_file", [][4]string{{"zz_asm_amd64.s", "dir", "", ""}}, []string{"foreign:zz_asm_amd64.s"}},
	{"shadowing_local", [][4]string{{"lifecycle.go", "before", referencesAtRecover, "func zzShadow() {\n\topenPrivateFile := func(string) error { return nil }\n\t_ = openPrivateFile(\"x\")\n}\n\n"}}, nil},
}

// referencesEnding returns the sources with every line ended by ending.
func referencesEnding(sources map[string]string, ending string) map[string]string {
	ended := map[string]string{}
	for name, source := range sources {
		ended[name] = strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\n", ending)
	}
	return ended
}

// Every non-test file compiles on one of the five systems and references the
// guarded set exactly as the list says, by file, declaration and count, with
// LF and with CRLF; every guarded name resolves; no entry of the directory is
// non-Go source.
func TestPackageReferencesSourceContract(t *testing.T) {
	sources, names := admissionReadSources(t), []string{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal("cannot list the local cell package directory")
	}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	keys := packageReferencesForeign(names)
	for _, ending := range []string{"\n", "\r\n"} {
		keys = append(keys, packageReferencesContract(referencesEnding(sources, ending), packageReferencesGuarded, packageReferencesList)...)
	}
	for _, key := range slices.Compact(slices.Sorted(slices.Values(keys))) {
		t.Errorf("package references contract key %s", key)
	}
}

// Each row reports at least its keys, LF and CRLF, through the whole contract
// on all five systems; the control row reports none.
func TestPackageReferencesSourceContractMutations(t *testing.T) {
	sources := referencesEnding(admissionReadSources(t), "\n")
	for _, row := range packageReferencesRows {
		t.Run(row.name, func(t *testing.T) {
			edited, guarded, listing := maps.Clone(sources), slices.Clone(packageReferencesGuarded), []string{}
			for _, edit := range row.edits {
				file, mode, anchor, text := edit[0], edit[1], edit[2], edit[3]
				switch {
				case mode == "guard":
					guarded = append(guarded, text)
				case mode == "dir":
					listing = append(listing, file)
				case mode == "+" || mode == "=":
					edited[file] = text + edited[anchor]
				case mode == "-":
					delete(edited, file)
				case strings.Count(edited[file], anchor) != 1:
					t.Fatal("package references row anchor does not match once")
				case mode == "before":
					edited[file] = strings.Replace(edited[file], anchor, text+anchor, 1)
				case mode == "after":
					edited[file] = strings.Replace(edited[file], anchor, anchor+text, 1)
				default:
					edited[file] = strings.Replace(edited[file], anchor, text, 1)
				}
			}
			for _, ending := range []string{"\n", "\r\n"} {
				keys := append(packageReferencesContract(referencesEnding(edited, ending), guarded, packageReferencesList), packageReferencesForeign(listing)...)
				for _, key := range row.keys {
					if !slices.Contains(keys, key) {
						t.Errorf("package references row lacks key %s", key)
					}
				}
				for _, key := range keys {
					if len(row.keys) == 0 {
						t.Errorf("package references control row reports key %s", key)
					}
				}
			}
		})
	}
}
