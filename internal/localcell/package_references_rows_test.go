package localcell

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
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
	referencesAtVars     = "var (\n"
	referencesAtIdentity = "func processIdentity("
	referencesAtVerify   = "\tvar data []byte\n\tvar oversized bool\n"
)

// packageReferencesRows edit the sources in memory: each edit is a file, a
// mode, an anchor and a text. Modes: before, after or replace the anchor;
// "append" ends the file with the text; "+" adds the file, "=" copies the
// anchor's file, "-" drops it; "guard" adds a guarded name and "dir" a
// directory entry. A row names every key it reports.
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
	{"dot_import", [][4]string{{"zz_dot.go", "+", "", "package localcell\n\nimport . \"io/ioutil\"\n\nfunc zzDot() { _, _ = ReadFile(\".\") }\n"}}, []string{"file:zz_dot.go", "import:zz_dot.go:io/ioutil", "ref:zz_dot.go:zzDot:io/ioutil.ReadFile"}},
	{"method_value", [][4]string{{"lifecycle.go", "before", referencesAtRecover, "func zzMethodValue(root *os.Root) {\n\topen := root.Open\n\t_, _ = open(\".\")\n}\n\n"}}, []string{"ref:lifecycle.go:zzMethodValue:(*os.Root).Open"}},
	{"interface_dispatch", [][4]string{{"lifecycle.go", "before", referencesAtRecover, "func zzIface(root *os.Root) {\n\tvar r interface{ Open(string) (*os.File, error) } = root\n\t_, _ = r.Open(\".\")\n}\n\n"}}, []string{"ref:lifecycle.go:zzIface:(*os.Root).Open"}},
	{"field_use", [][4]string{{"checkpoint.go", "after", "\tfail := func(cause error) error { _ = discard(); return cause }\n\tplanned := make(map[string]bool, len(plan.entries))\n", "\tif f, err := ops.open(dir); err == nil {\n\t\t_ = f.Close()\n\t}\n"}}, []string{"ref:checkpoint.go:(*Cell).inventoryCheckpoint:openPrivateReader"}},
	{"generic_value", [][4]string{{"lifecycle.go", "before", referencesAtRecover, "func zzGeneric[F ~func(string) (*os.File, error)](f F) { _, _ = f(\".\") }\n\nfunc zzGenericCall() { zzGeneric(os.Open) }\n\n"}}, []string{"ref:lifecycle.go:zzGenericCall:os.Open"}},
	{"other_spelling", [][4]string{{"checkpoint.go", "after", referencesAtImports, "\t\"io/ioutil\"\n"},
		{"checkpoint.go", "before", referencesAtVersion, "\tif _, err := ioutil.ReadFile(filepath.Join(c.cfg.Root, \"pg\", \"PG_VERSION\")); err != nil {\n\t\treturn nil, errCheckpointSource\n\t}\n"}}, []string{"import:checkpoint.go:io/ioutil", "ref:checkpoint.go:(*Cell).admitCheckpoint:io/ioutil.ReadFile"}},
	{"windows_file", [][4]string{{"files_windows.go", "after", "func readPrivateFile(path string) ([]byte, error) {\n", "\tif data, err := os.ReadFile(path); err == nil {\n\t\treturn data, nil\n\t}\n"}}, []string{"ref:files_windows.go:readPrivateFile:os.ReadFile"}},
	{"darwin_file", [][4]string{{"process_darwin.go", "after", "func processIdentity(pid int) (string, bool, error) {\n", "\tif f, err := unix.Open(\"/dev/null\", unix.O_RDONLY, 0); err == nil {\n\t\t_ = unix.Close(f)\n\t}\n"}}, []string{"ref:process_darwin.go:processIdentity:golang.org/x/sys/unix.Open"}},
	{"other_system_file", [][4]string{{"checkpoint_other.go", "before", "\treturn nil, nil, errCheckpointState\n", "\t_, _ = os.Open(\".\")\n"}}, []string{"ref:checkpoint_other.go:(*Cell).openCheckpointSource:os.Open"}},
	{"platform_twin", [][4]string{{"files_linux_twin.go", "=", "files_other.go", ""},
		{"files_linux_twin.go", "replace", "//go:build !windows\n", "//go:build linux\n"},
		{"files_linux_twin.go", "replace", "\treturn os.OpenFile(path, flags|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, mode)\n}\n\n// blockingWriter", "\treturn os.OpenFile(path, flags, mode)\n}\n\n// blockingWriter"},
		{"files_other.go", "replace", "//go:build !windows\n", "//go:build !windows && !linux\n"}}, []string{"file:files_linux_twin.go", "import:files_linux_twin.go:os", "import:files_linux_twin.go:syscall",
		"ref:files_linux_twin.go:createPrivateTemp:os.CreateTemp", "ref:files_linux_twin.go:openPlainReader:openPrivateReader", "ref:files_linux_twin.go:openPostgresReader:openPrivateReader", "ref:files_linux_twin.go:openPrivateFile:os.OpenFile",
		"ref:files_linux_twin.go:openPrivateReader:os.OpenFile", "ref:files_linux_twin.go:openPrivateWriter:os.OpenFile", "ref:files_linux_twin.go:readPostgresFile:readPrivateFile", "ref:files_linux_twin.go:readPrivateFile:os.ReadFile"}},
	{"custom_tag_twin", [][4]string{{"zz_twin.go", "+", "", "//go:build witselfreviewtwin\n\npackage localcell\n\nimport \"os\"\n\nfunc zzTwin() { _, _ = os.Open(\".\") }\n"}}, []string{"file:zz_twin.go", "uncompiled:zz_twin.go"}},
	{"linkname", [][4]string{{"zz_link.go", "+", "", "package localcell\n\nimport (\n\t\"os\"\n\t_ \"unsafe\"\n)\n\n//go:linkname zzOpen os.Open\nfunc zzOpen(name string) (*os.File, error)\n"}}, []string{"bodyless:zz_link.go:zzOpen", "file:zz_link.go", "import:zz_link.go:os", "import:zz_link.go:unsafe", "linkname:zz_link.go"}},
	{"reflective_call", [][4]string{{"lifecycle.go", "after", referencesAtImports, "\t\"reflect\"\n"},
		{"lifecycle.go", "before", referencesAtRecover, "func zzReflect(f *os.File) { _ = reflect.ValueOf(f).MethodByName(\"Sync\").Call(nil) }\n\n"}}, []string{"import:lifecycle.go:reflect", "ref:lifecycle.go:zzReflect:(reflect.Value).Call", "ref:lifecycle.go:zzReflect:(reflect.Value).MethodByName"}},
	{"unsafe_pointer", [][4]string{{"lifecycle.go", "after", referencesAtImports, "\t\"unsafe\"\n"},
		{"lifecycle.go", "before", referencesAtRecover, "func zzUnsafe(p *int) uintptr { return uintptr(unsafe.Pointer(p)) }\n\n"}}, []string{"import:lifecycle.go:unsafe", "ref:lifecycle.go:zzUnsafe:unsafe.Pointer"}},
	{"raw_syscall", [][4]string{{"process_linux.go", "before", "func processIdentity(", "func zzRaw() { _, _, _ = syscall.Syscall(syscall.SYS_GETPID, 0, 0, 0) }\n\n"}}, []string{"ref:process_linux.go:zzRaw:syscall.Syscall"}},
	{"dll_procedure", [][4]string{{"stop_windows.go", "before", "var (\n", "var zzProc = windows.NewLazySystemDLL(\"kernel32.dll\").NewProc(\"CreateFileW\")\n\n"}}, []string{"ref:stop_windows.go:zzProc:golang.org/x/sys/windows.NewLazySystemDLL", "ref:stop_windows.go:zzProc:(*golang.org/x/sys/windows.LazyDLL).NewProc"}},
	{"reference_removed", [][4]string{{"files.go", "replace", "\td, err := os.Open(path + string(filepath.Separator) + \".\")\n", "\td, err := openPlainDirectory(path)\n"},
		{"files.go", "before", referencesAtProgress, "func openPlainDirectory(string) (*os.File, error) { return nil, errInterrupted }\n\n"}}, []string{"ref:files.go:syncDirectory:os.Open"}},
	{"file_removed", [][4]string{{"doc.go", "-", "", ""}}, []string{"file:doc.go"}},
	{"type_error", [][4]string{{"lifecycle.go", "before", referencesAtRecover, "func zzBroken() { _ = zzUndefined }\n\n"}}, []string{"types:darwin:lifecycle.go", "types:freebsd:lifecycle.go", "types:linux:lifecycle.go", "types:solaris:lifecycle.go", "types:windows:lifecycle.go"}},
	{"unknown_name", [][4]string{{"", "guard", "", "os.OpenFil"}}, []string{"unknown:os.OpenFil"}},
	{"foreign_file", [][4]string{{"zz_asm_amd64.s", "dir", "", ""}}, []string{"foreign:zz_asm_amd64.s"}},
	{"shadowing_local", [][4]string{{"lifecycle.go", "before", referencesAtRecover, "func zzShadow() {\n\topenPrivateFile := func(string) error { return nil }\n\t_ = openPrivateFile(\"x\")\n}\n\n"}}, nil},
	{"line_directive", [][4]string{{"service_lease_publish.go", "replace", "package localcell\n", "//line service_lease_publish_gen.go:1\npackage localcell\n"},
		{"service_lease_publish.go", "append", "", "\nfunc zzHiddenOpen(path string) {\n\tif f, err := os.OpenFile(path, os.O_RDWR, 0); err == nil {\n\t\t_ = f.Close()\n\t}\n}\n"}}, []string{"line:service_lease_publish.go", "ref:service_lease_publish.go:zzHiddenOpen:os.OpenFile"}},
	{"struct_conversion", [][4]string{{"checkpoint_verify.go", "after", referencesAtVerify, "\ttype zzOps struct {\n\t\tcontrol     func(context.Context) (pgControlData, error)\n\t\tfree        func(string) (uint64, uint64, error)\n\t\tafterCopy   func(string)\n\t\tbeforeClose func()\n\t\tnewID       func() (string, error)\n\t\tpublish     func(string, string) error\n\t\topen        func(string) (*os.File, error)\n\t\ttransient   func(error) bool\n\t\tsleep       func(context.Context, time.Duration)\n\t\tnow         func() time.Time\n\t\tbeforeStep  func(step, path string)\n\t}\n\tif f, err := zzOps(ops).open(filepath.Join(dir, \"..\", \"zz\")); err == nil {\n\t\t_ = f.Close()\n\t}\n"}}, []string{"ref:checkpoint_verify.go:verifyCheckpointDocuments:openPrivateReader"}},
	{"type_method_interface", [][4]string{{"lifecycle.go", "after", referencesAtImports, "\t\"reflect\"\n"},
		{"lifecycle.go", "before", referencesAtRecover, "func zzTypeMethod(root *os.Root) {\n\tvar typ interface {\n\t\tMethodByName(string) (reflect.Method, bool)\n\t} = reflect.TypeOf(root)\n\tif method, ok := typ.MethodByName(\"Open\"); ok {\n\t\tif open, ok := method.Func.Interface().(func(*os.Root, string) (*os.File, error)); ok {\n\t\t\t_, _ = open(root, \".\")\n\t\t}\n\t}\n}\n\n"}}, []string{"import:lifecycle.go:reflect", "ref:lifecycle.go:zzTypeMethod:(reflect.Type).MethodByName"}},
	{"template_execute", [][4]string{{"lifecycle.go", "after", referencesAtImports, "\t\"io\"\n\t\"text/template\"\n"},
		{"lifecycle.go", "before", referencesAtRecover, "func zzTemplate(root *os.Root) {\n\t_ = template.Must(template.New(\"zz\").Parse(`{{.ReadFile \"zz\"}}`)).Execute(io.Discard, root)\n}\n\n"}}, []string{"import:lifecycle.go:io", "import:lifecycle.go:text/template"}},
	{"zip_reader", [][4]string{{"lifecycle.go", "after", referencesAtImports, "\t\"archive/zip\"\n"},
		{"lifecycle.go", "before", referencesAtRecover, "func zzZip(path string) {\n\tif r, err := zip.OpenReader(path); err == nil {\n\t\t_ = r.Close()\n\t}\n}\n\n"}}, []string{"import:lifecycle.go:archive/zip"}},
	{"tls_key_pair", [][4]string{{"lifecycle.go", "after", referencesAtImports, "\t\"crypto/tls\"\n"},
		{"lifecycle.go", "before", referencesAtRecover, "func zzKeyPair(cert, key string) { _, _ = tls.LoadX509KeyPair(cert, key) }\n\n"}}, []string{"import:lifecycle.go:crypto/tls"}},
	{"unix_dial", [][4]string{{"lifecycle.go", "after", referencesAtImports, "\t\"net\"\n"},
		{"lifecycle.go", "before", referencesAtRecover, "func zzDial(path string) {\n\tif conn, err := net.Dial(\"unix\", path); err == nil {\n\t\t_ = conn.Close()\n\t}\n}\n\n"}}, []string{"import:lifecycle.go:net", "ref:lifecycle.go:zzDial:net.Dial"}},
	{"nt_create_file", [][4]string{{"stop_windows.go", "before", referencesAtVars, "func zzNtCreate(oa *windows.OBJECT_ATTRIBUTES) {\n\tvar handle windows.Handle\n\tvar status windows.IO_STATUS_BLOCK\n\t_ = windows.NtCreateFile(&handle, windows.GENERIC_READ, oa, &status, nil, 0, 0, windows.FILE_OPEN, 0, 0, 0)\n}\n\n"}}, []string{"ref:stop_windows.go:zzNtCreate:golang.org/x/sys/windows.NtCreateFile"}},
	{"find_first_file", [][4]string{{"stop_windows.go", "before", referencesAtVars, "func zzFindFirst(pattern *uint16) {\n\tvar data windows.Win32finddata\n\tif handle, err := windows.FindFirstFile(pattern, &data); err == nil {\n\t\t_ = windows.FindClose(handle)\n\t}\n}\n\n"}}, []string{"ref:stop_windows.go:zzFindFirst:golang.org/x/sys/windows.FindFirstFile"}},
	{"syscall_dll", [][4]string{{"stop_windows.go", "before", referencesAtVars, "func zzStdDLL() { _, _, _ = syscall.NewLazyDLL(\"kernel32.dll\").NewProc(\"GetTickCount\").Call() }\n\n"}},
		[]string{"ref:stop_windows.go:zzStdDLL:(*syscall.LazyDLL).NewProc", "ref:stop_windows.go:zzStdDLL:(*syscall.LazyProc).Call", "ref:stop_windows.go:zzStdDLL:syscall.NewLazyDLL"}},
	{"syscall18", [][4]string{{"stop_windows.go", "before", referencesAtVars, "func zzSyscall18(proc uintptr) { _, _, _ = syscall.Syscall18(proc, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0) }\n\n"}}, []string{"ref:stop_windows.go:zzSyscall18:syscall.Syscall18"}},
	{"all_threads_syscall", [][4]string{{"process_linux.go", "before", referencesAtIdentity, "func zzAllThreads() { _, _, _ = syscall.AllThreadsSyscall(syscall.SYS_GETPID, 0, 0, 0) }\n\n"}}, []string{"ref:process_linux.go:zzAllThreads:syscall.AllThreadsSyscall"}},
	{"darwin_syscall9", [][4]string{{"process_darwin.go", "before", referencesAtIdentity, "func zzSyscall9() { _, _, _ = unix.Syscall9(unix.SYS_GETPID, 0, 0, 0, 0, 0, 0, 0, 0, 0) }\n\n"}}, []string{"ref:process_darwin.go:zzSyscall9:golang.org/x/sys/unix.Syscall9"}},
	{"forged_field", [][4]string{{"checkpoint_verify.go", "after", referencesAtVerify, "\tif open, ok := reflect.NewAt(reflect.TypeFor[func(string) (*os.File, error)](), reflect.ValueOf(&ops).Elem().FieldByName(\"open\").Addr().UnsafePointer()).Elem().Interface().(func(string) (*os.File, error)); ok {\n\t\tif f, err := open(filepath.Join(dir, \"..\", \"zz\")); err == nil {\n\t\t\t_ = f.Close()\n\t\t}\n\t}\n"}},
		[]string{"ref:checkpoint_verify.go:verifyCheckpointDocuments:(reflect.Value).UnsafePointer", "ref:checkpoint_verify.go:verifyCheckpointDocuments:reflect.NewAt"}},
	{"freebsd_file", [][4]string{{"zz_freebsd.go", "+", "", "package localcell\n\nimport \"os\"\n\nfunc zzFreeBSD() { _, _ = os.Open(\".\") }\n"}}, []string{"file:zz_freebsd.go", "import:zz_freebsd.go:os", "ref:zz_freebsd.go:zzFreeBSD:os.Open"}},
	{"solaris_file", [][4]string{{"lock_unsupported.go", "after", "func openLockFile(string, bool) (*os.File, error) {\n", "\tif f, err := os.Open(\".\"); err == nil {\n\t\t_ = f.Close()\n\t}\n"}}, []string{"ref:lock_unsupported.go:openLockFile:os.Open"}},
	{"generic_field", [][4]string{{"lifecycle.go", "before", referencesAtRecover, "type zzBox[T any] struct{ zzOpen func(T) (*os.File, error) }\n\nvar zzBoxed = zzBox[string]{zzOpen: os.Open}\n\nfunc zzBoxUse() { _, _ = zzBoxed.zzOpen(\".\") }\n\n"}}, []string{"ref:lifecycle.go:zzBoxUse:os.Open", "ref:lifecycle.go:zzBoxed:os.Open"}},
	{"interface_other_signature", [][4]string{{"lifecycle.go", "before", referencesAtRecover, "func zzOpenNoArgs(v interface{ Open() error }) { _ = v.Open() }\n\n"}}, nil},
	{"receiver_spelling", [][4]string{{"", "guard", "", "(os.Root).Open"}}, []string{"unknown:(os.Root).Open"}},
	{"darwin_only_count", [][4]string{{"service_launchd_darwin.go", "before", "func NewServiceAdapter() ServiceAdapter {", "var zzPlatformReadDir = os.ReadDir\n\n"},
		{"zz_platform.go", "+", "", "//go:build !darwin\n\npackage localcell\n\nfunc zzPlatformReadDir(string) (any, error) { return nil, nil }\n"},
		{"lifecycle.go", "before", "\tlock, err := c.own(true)\n", "\t_, _ = zzPlatformReadDir(\".\")\n"}}, []string{"file:zz_platform.go", "ref:lifecycle.go:(*Cell).Install:os.ReadDir", "ref:service_launchd_darwin.go:zzPlatformReadDir:os.ReadDir"}},
	{"alias_chain", [][4]string{{"lifecycle.go", "before", referencesAtRecover, "type zzCopy struct{ open func(string) (*os.File, error) }\n\nvar zzChained = zzCopy{}.open\n\nfunc zzChainUse() { _, _ = zzChained(\".\") }\n\n"}},
		[]string{"ref:lifecycle.go:zzChainUse:openPrivateReader", "ref:lifecycle.go:zzChained:openPrivateReader"}},
	{"generic_interface", [][4]string{{"lifecycle.go", "before", referencesAtRecover, "type zzGenOpener[T any] interface{ Open(T) (*os.File, error) }\n\nfunc zzGenIface(root *os.Root) {\n\tvar r zzGenOpener[string] = root\n\t_, _ = r.Open(\".\")\n}\n\n"}},
		[]string{"ref:lifecycle.go:zzGenIface:(*os.Root).Open"}},
	{"generic_constraint", [][4]string{{"lifecycle.go", "before", referencesAtRecover, "func zzGenOpenT[T any, R interface{ Open(T) (*os.File, error) }](r R, name T) { _, _ = r.Open(name) }\n\nfunc zzGenRootT(root *os.Root) { zzGenOpenT(root, \".\") }\n\n"}},
		[]string{"ref:lifecycle.go:zzGenRootT:(*os.Root).Open"}},
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
	keys, details := packageReferencesForeign(names), map[string]string{}
	for _, ending := range []string{"\n", "\r\n"} {
		found, counts := packageReferencesContract(referencesEnding(sources, ending), packageReferencesGuarded, packageReferencesList)
		keys = append(keys, found...)
		maps.Copy(details, counts)
	}
	for _, key := range slices.Compact(slices.Sorted(slices.Values(keys))) {
		t.Errorf("package references contract key %s%s", key, details[key])
	}
}

// Each row reports exactly its keys, LF and CRLF, through the whole contract
// on all five systems; a control row reports none.
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
				case mode == "append":
					edited[file] += text
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
				keys, _ := packageReferencesContract(referencesEnding(edited, ending), guarded, packageReferencesList)
				keys = slices.Sorted(slices.Values(append(keys, packageReferencesForeign(listing)...)))
				if want := slices.Sorted(slices.Values(row.keys)); !slices.Equal(keys, want) {
					for _, key := range keys {
						if !slices.Contains(want, key) {
							t.Errorf("package references row reports unlisted key %s", key)
						}
					}
					for _, key := range want {
						if !slices.Contains(keys, key) {
							t.Errorf("package references row lacks key %s", key)
						}
					}
				}
			}
		})
	}
}

// The world rows. A listing the go command failed, one cut short, one without
// runtime and an empty one each fail their system with world:, as a missing go
// command does; a dependency that does not type-check is refused, so the file
// importing it fails types:; and a load writes nothing to the build cache.
func TestPackageReferencesSourceContractWorld(t *testing.T) {
	dir, listing, sources := t.TempDir(), `{"ImportPath":"runtime"}{"ImportPath":"zz"}`, map[string]string{"zz.go": "package zz\n\nimport \"zz/broken\"\n\nvar _ = broken.Broken\n"}
	if os.WriteFile(filepath.Join(dir, "broken.go"), []byte("package broken\n\nvar Broken int = \"zz\"\n"), 0o600) != nil {
		t.Fatal("cannot write the package references broken dependency")
	}
	expect := func(view referencesView, key string) {
		maps.DeleteFunc(view.keys, func(_ string, bad bool) bool { return !bad })
		if keys := slices.Sorted(maps.Keys(view.keys)); !slices.Equal(keys, []string{key}) {
			t.Errorf("package references world row reports keys %s", strings.Join(keys, " "))
		}
	}
	for _, row := range []struct {
		output string
		err    error
		key    string
	}{{listing, nil, "types:linux:zz.go"}, {listing, errors.New("exit status 1"), "world:linux"}, {listing + "{", nil, "world:linux"},
		{`{"ImportPath":"zz"}`, nil, "world:linux"}, {"", nil, "world:linux"}} {
		var world referencesWorld
		world.admit([]byte(row.output), row.err)
		world.listed["zz/broken"] = &referencesListed{ImportPath: "zz/broken", Dir: dir, GoFiles: []string{"broken.go"}}
		expect(world.check("linux", sources, nil), row.key)
	}
	cache := t.TempDir()
	t.Setenv("GOCACHE", cache)
	var loaded, missing referencesWorld
	if loaded.load("linux"); loaded.failed {
		t.Error("package references world cannot list the package")
	}
	if entries, err := os.ReadDir(cache); err != nil || len(entries) != 0 {
		t.Error("package references world load writes to the build cache")
	}
	t.Setenv("PATH", "")
	expect(missing.check("linux", sources, nil), "world:linux")
}
