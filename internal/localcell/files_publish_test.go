package localcell

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Off Windows every publication failure, and on every platform every failure
// that is not a private Windows sharing or access refusal, is one attempt that
// returns its unchanged error. A successful publication writes exactly once.
// Each row is a complete publication: a fresh pending source, its sync, the
// caller's pre-rename check and the rename.
func TestAtomicPublicationRetriesOnlyTransient(t *testing.T) {
	refusal := errors.New("synthetic pre-rename refusal")
	for _, row := range []struct {
		name    string
		publish func(path string, beforeRename func() error) error
		want    []byte
	}{
		{"record", func(path string, beforeRename func() error) error {
			return writeAtomic(path, map[string]string{"fixture": "synthetic"}, beforeRename)
		}, []byte("{\n  \"fixture\": \"synthetic\"\n}\n")},
		{"bytes", func(path string, beforeRename func() error) error {
			return writeAtomicBytes(path, []byte("synthetic bytes\n"), beforeRename)
		}, []byte("synthetic bytes\n")},
	} {
		for _, outcome := range []string{"published_once", "rename_refusal", "persist_refusal"} {
			t.Run(row.name+"/"+outcome, func(t *testing.T) {
				root := testPrivateRoot(t)
				path := filepath.Join(root, "record.json")
				syncs, checks := 0, 0
				original := syncFile
				t.Cleanup(func() { syncFile = original })
				syncFile = func(f *os.File) error {
					if filepath.Dir(f.Name()) == root && strings.HasPrefix(filepath.Base(f.Name()), ".pending-") {
						syncs++
						// A fuse keeps a broken retry finite; it can only add attempts.
						if outcome == "persist_refusal" && syncs <= 16 {
							return errors.New("synthetic persist refusal")
						}
					}
					return original(f)
				}
				started := time.Now()
				err := row.publish(path, func() error {
					checks++
					if outcome == "rename_refusal" && checks <= 16 {
						return refusal
					}
					return nil
				})
				if syncs != 1 || time.Since(started) >= serviceLeasePublishBudget {
					t.Fatalf("publication attempts=%d", syncs)
				}
				switch outcome {
				case "published_once":
					data, readErr := os.ReadFile(path)
					if err != nil || checks != 1 || readErr != nil || !bytes.Equal(data, row.want) {
						t.Fatal("successful publication did not write its exact record once")
					}
				case "rename_refusal":
					// The caller's own error value returns unchanged.
					if err != refusal || checks != 1 {
						t.Fatal("pre-rename refusal was retried or changed")
					}
				case "persist_refusal":
					if err == nil || err.Error() != "cannot persist local cell record" || checks != 0 {
						t.Fatal("persist refusal was retried or changed")
					}
				}
				if outcome != "published_once" {
					if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
						t.Fatal("refused publication exposed a record")
					}
				}
				if pending, globErr := filepath.Glob(filepath.Join(root, ".pending-*")); globErr != nil || len(pending) != 0 {
					t.Fatal("publication left a pending source")
				}
			})
		}
	}
}

// The startup Stop of a stopped foreground host publishes manifest.json through
// finish. Drive that real Stop: its manifest publication is one attempt when it
// succeeds, and one attempt that leaves the manifest and a pending stop for
// Repair when a refusal is not a private Windows sharing or access refusal.
func TestStopManifestPublicationSingleAttemptRefusal(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "published_once"
		if fail {
			name = "refusal_single_attempt"
		}
		t.Run(name, func(t *testing.T) {
			c, m := unitCell(t)
			path := c.path("manifest.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal("cannot read manifest fixture")
			}
			attempts := 0
			original := syncFile
			t.Cleanup(func() { syncFile = original })
			syncFile = func(f *os.File) error {
				if filepath.Dir(f.Name()) == filepath.Dir(path) && strings.HasPrefix(filepath.Base(f.Name()), ".pending-") {
					attempts++
					// A fuse keeps a broken retry finite; it can only add attempts.
					if fail && attempts <= 16 {
						return errors.New("synthetic manifest publication refusal")
					}
				}
				return original(f)
			}
			err = c.stopServiceChildren(t.Context())
			if attempts != 1 {
				t.Fatalf("Stop manifest publication attempts=%d", attempts)
			}
			progress, journalErr := c.readJournal()
			if fail {
				data, readErr := os.ReadFile(path)
				if err == nil || err.Error() != "cannot persist local cell record" || readErr != nil || !bytes.Equal(data, before) {
					t.Fatal("refused Stop manifest publication changed the manifest or its error")
				}
				if journalErr != nil || progress.pending == nil || progress.pending.Operation != "stop" {
					t.Fatal("refused Stop manifest publication left no pending stop for Repair")
				}
				return
			}
			published, manifestErr := readManifest(c.cfg.Root)
			if err != nil || manifestErr != nil || published.LastOperation != "stop" || published.InstallationID != m.InstallationID {
				t.Fatal("Stop did not publish its stopped manifest")
			}
			if journalErr != nil || progress.pending != nil || progress.tornTail {
				t.Fatal("Stop did not settle its journal operation")
			}
		})
	}
}

// writeAtomic and writeAtomicBytes are every private atomic publication. Pin
// their one shared bounded retry, and that no other production code retries a
// publication again (nesting) or publishes with a single attempt (bypass). The
// Windows reproductions are the behavioral oracle and cannot run on hosts.
func TestAtomicPublicationSharedRetrySourceContract(t *testing.T) {
	sources := atomicPublicationSources(t)
	if err := atomicPublicationContract(sources); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct{ name, file, before, after string }{
		{"shared_retry_removed", "files.go", "_, err := publishAtomic(func() error { return writeAtomicBytesOnce(path, data, beforeRename) }, leasePublishHooks{})\n\treturn err", "return writeAtomicBytesOnce(path, data, beforeRename)"},
		{"record_retry_removed", "files.go", "_, err := publishAtomic(func() error { return writeAtomicOnce(path, value, beforeRename) }, leasePublishHooks{})\n\treturn err", "return writeAtomicOnce(path, value, beforeRename)"},
		{"predicate_any_error", "files.go", "func(err error) bool { return runtimeconfig.WindowsPrivateTransient(err) }", "func(error) bool { return true }"},
		{"predicate_off_windows", "files.go", "func(err error) bool { return runtimeconfig.WindowsPrivateTransient(err) }", `func(err error) bool { return runtime.GOOS != "windows" || runtimeconfig.WindowsPrivateTransient(err) }`},
		{"test_hooks", "files.go", "writeAtomicBytesOnce(path, data, beforeRename) }, leasePublishHooks{})", "writeAtomicBytesOnce(path, data, beforeRename) }, serviceLeasePublishTestHooks)"},
		{"boot_nested_retry", "service_boot.go", `return writeAtomic(c.path("service", "boot.json"), serviceBootRecord{d.InstallationID, boot}, nil)`, "path, record := c.path(\"service\", \"boot.json\"), serviceBootRecord{d.InstallationID, boot}\n\t_, err = retryPublish(func() error { return writeAtomic(path, record, nil) }, func(err error) bool { return runtimeconfig.WindowsPrivateTransient(err) }, leasePublishHooks{})\n\treturn err"},
		{"lease_nested_attempt", "service_lease_publish.go", "h.write = writeAtomicOnce", "h.write = writeAtomic"},
		{"stop_bypasses_retry", "lifecycle.go", "m.LastOperation = operation\n\tif err := writeAtomic(", "m.LastOperation = operation\n\tif err := writeAtomicOnce("},
		{"caller_nested_retry", "service_supervisor.go", `return serviceFailureAt(writeAtomic(s.cell.path("service", "supervisor.json"), s.record, nil), "supervisor_save")`, "_, err := publishAtomic(func() error { return writeAtomic(s.cell.path(\"service\", \"supervisor.json\"), s.record, nil) }, leasePublishHooks{})\n\treturn serviceFailureAt(err, \"supervisor_save\")"},
		{"caller_engine_retry", "service_control_endpoint.go", `if err := writeAtomic(c.path("service", "control-endpoint.json"), record, nil); err != nil {`, `if _, err := retryPublish(func() error { return writeAtomic(c.path("service", "control-endpoint.json"), record, nil) }, func(error) bool { return true }, leasePublishHooks{}); err != nil {`},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			changed := make(map[string]string, len(sources))
			for name, source := range sources {
				changed[name] = source
			}
			if strings.Count(changed[mutation.file], mutation.before) != 1 {
				t.Fatal("shared publication mutation anchor is not unique")
			}
			changed[mutation.file] = strings.Replace(changed[mutation.file], mutation.before, mutation.after, 1)
			if atomicPublicationContract(changed) == nil {
				t.Fatal("shared publication contract accepted a mutation")
			}
		})
	}
}

// Every non-test source of this package, for every platform, with LF endings.
func atomicPublicationSources(t *testing.T) map[string]string {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	sources := make(map[string]string)
	for _, path := range paths {
		if !strings.HasSuffix(path, "_test.go") {
			sources[path] = leasePublishReadSource(t, path)
		}
	}
	return sources
}

func atomicPublicationContract(sources map[string]string) error {
	fail := errors.New("shared atomic publication retry contract changed")
	const expected = `package localcell
func writeAtomic(path string, value any, beforeRename func() error) error {
 _, err := publishAtomic(func() error { return writeAtomicOnce(path, value, beforeRename) }, leasePublishHooks{})
 return err
}
func writeAtomicOnce(path string, value any, beforeRename func() error) error {
 data, err := json.MarshalIndent(value, "", "  ")
 if err != nil { return errors.New("cannot encode local cell record") }
 return writeAtomicBytesOnce(path, append(data, '\n'), beforeRename)
}
func writeAtomicBytes(path string, data []byte, beforeRename func() error) error {
 _, err := publishAtomic(func() error { return writeAtomicBytesOnce(path, data, beforeRename) }, leasePublishHooks{})
 return err
}
func publishAtomic(attempt func() error, h leasePublishHooks) (int, error) {
 return retryPublish(attempt, func(err error) bool { return runtimeconfig.WindowsPrivateTransient(err) }, h)
}`
	wanted := leasePublishDeclarations(expected)
	actual := leasePublishDeclarations(sources["files.go"])
	if len(wanted) != 4 {
		return fail
	}
	for name, declaration := range wanted {
		if actual[name] != declaration {
			return fail
		}
	}
	// Exactly these declarations may name the engine, the shared retry or a
	// single attempt: the shared layer itself, the lease publisher (which
	// passes its single-attempt hook to the shared retry) and that hook's
	// production default. A package-level value counts under "".
	want := map[string]map[string]int{
		"retryPublish":         {"publishAtomic": 1},
		"publishAtomic":        {"writeAtomic": 1, "writeAtomicBytes": 1, "publishServiceLease": 1},
		"writeAtomicOnce":      {"writeAtomic": 1, "defaults": 1},
		"writeAtomicBytesOnce": {"writeAtomicOnce": 1, "writeAtomicBytes": 1},
	}
	got := make(map[string]map[string]int)
	for name, source := range sources {
		file, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
		if err != nil {
			return fail
		}
		for _, declaration := range file.Decls {
			owner, node := "", ast.Node(declaration)
			if function, ok := declaration.(*ast.FuncDecl); ok {
				if function.Body == nil {
					continue
				}
				owner, node = function.Name.Name, function.Body
			}
			retries, publishes := false, false
			ast.Inspect(node, func(child ast.Node) bool {
				identifier, ok := child.(*ast.Ident)
				if !ok {
					return true
				}
				switch identifier.Name {
				case "retryPublish", "publishAtomic":
					retries = true
				case "writeAtomic", "writeAtomicBytes":
					publishes = true
				}
				if want[identifier.Name] != nil {
					if got[identifier.Name] == nil {
						got[identifier.Name] = make(map[string]int)
					}
					got[identifier.Name][owner]++
				}
				return true
			})
			// A retry wrapped around a retrying publication would nest.
			if retries && publishes {
				return fail
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		return fail
	}
	return nil
}

// The Windows manifest reproduction must hold exactly the validator's guard: it
// opens no handle of its own and holds holdWindowsBootValidatorGuard, which
// TestWindowsBootValidatorGuardMatchesPrivateGuard pins to windowsPrivateGuard.
// It must also drive the real Stop that publishes the manifest.
func TestWindowsStopManifestReproductionReusesValidatorGuard(t *testing.T) {
	source := leasePublishReadSource(t, "files_publish_windows_test.go")
	if !windowsManifestReproductionContract(source) {
		t.Fatal("manifest publication reproduction no longer holds the private validator guard during Stop")
	}
	for _, mutation := range []struct{ name, before, after string }{
		{"own_guard", "guard := holdWindowsBootValidatorGuard(t, path)\n\t\tattempts := observeWindowsManifestPublications(t, c, func(int, string) {})", "guard := holdWindowsBootValidatorGuard(t, path)\n\t\tguard.release()\n\t\tpointer, _ := windows.UTF16PtrFromString(path)\n\t\tforeign, _ := windows.CreateFile(pointer, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)\n\t\tdefer func() { _ = windows.CloseHandle(foreign) }()\n\t\tattempts := observeWindowsManifestPublications(t, c, func(int, string) {})"},
		{"publication_only", "err := c.stopServiceChildren(t.Context())\n\t\telapsed := time.Since(started)", "err := writeAtomic(path, struct{}{}, nil)\n\t\telapsed := time.Since(started)"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			if strings.Count(source, mutation.before) != 1 {
				t.Fatal("manifest reproduction mutation anchor is not unique")
			}
			if windowsManifestReproductionContract(strings.Replace(source, mutation.before, mutation.after, 1)) {
				t.Fatal("manifest reproduction contract accepted a mutation")
			}
		})
	}
}

func windowsManifestReproductionContract(source string) bool {
	file, err := parser.ParseFile(token.NewFileSet(), "files_publish_windows_test.go", source, 0)
	if err != nil {
		return false
	}
	guards, stops, handles := 0, 0, 0
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch callee := call.Fun.(type) {
		case *ast.Ident:
			if callee.Name == "holdWindowsBootValidatorGuard" {
				guards++
			}
		case *ast.SelectorExpr:
			if receiver, ok := callee.X.(*ast.Ident); ok && receiver.Name == "windows" && callee.Sel.Name == "CreateFile" {
				handles++
			}
			if callee.Sel.Name == "stopServiceChildren" {
				stops++
			}
		}
		return true
	})
	// Five subtests hold the guard; four drive Stop's own publication.
	return handles == 0 && guards == 5 && stops == 4
}
