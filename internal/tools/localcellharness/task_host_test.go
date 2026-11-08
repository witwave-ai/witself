package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

func TestTaskHostArgumentsRequireExactLocalInvocation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "owner's café 日本語")
	valid := []string{"--runtime=local", "--root", root, "--generation", "generation-1"}
	parsed, err := parseTaskHostFlags(valid)
	if err != nil || parsed.root != root || parsed.generation != "generation-1" {
		t.Fatal("task host lost exact argument boundaries")
	}
	for _, args := range [][]string{
		{"--root", root, "--generation", "generation-1"},
		{"--runtime=hosted", "--root", root, "--generation", "generation-1"},
		{"--runtime=LOCAL", "--root", root, "--generation", "generation-1"},
		{"--runtime=local", "--root", "relative", "--generation", "generation-1"},
		{"--runtime=local", "--root", root},
		{"--runtime=local", "--root", root, "--generation", "bad\ngeneration"},
		append(append([]string{}, valid...), "--pgbin", root),
		append(append([]string{}, valid...), "unexpected"),
	} {
		if _, err := parseTaskHostFlags(args); err == nil {
			t.Fatal("task host accepted an inexact invocation")
		}
	}
}

func TestTaskHostDispatchUsesValidatedEntry(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal("cannot read task host dispatch contract")
	}
	normalized := strings.ReplaceAll(string(source), "\r\n", "\n")
	if !strings.Contains(normalized, "if len(args) != 0 && args[0] == \"task-host\" {\n\t\treturn runTaskHost(ctx, args[1:])\n\t}") {
		t.Fatal("task host dispatch bypassed the validated entry")
	}
}

func TestTaskHostMissingInstallationNeverPreparesOrSelectsRuntime(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	for _, selected := range []string{"", "hosted"} {
		t.Run(selected, func(t *testing.T) {
			t.Setenv("WITSELF_RUNTIME", selected)
			parent := privateHarnessRoot(t)
			root := filepath.Join(parent, "missing")
			if err := run(t.Context(), []string{"task-host", "--runtime=local", "--root", root, "--generation", "generation-1"}, io.Discard); err == nil {
				t.Fatal("task host adopted a missing installation")
			}
			if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("task host created missing installation state")
			}
			if os.Getenv("WITSELF_RUNTIME") != selected {
				t.Fatal("task host selected runtime before installation validation")
			}
		})
	}
}

func TestTaskHostEntryErrorsAreValueFree(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	root := filepath.Join(privateHarnessRoot(t), "private-task-host-root")
	err := runTaskHost(t.Context(), []string{"--runtime=local", "--root", root, "--generation", "private-generation"})
	const fixed = "service_task_host_failed: step=task_host:entry code=0 reason=entry_failed"
	if err == nil || err.Error() != fixed || strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "private-generation") {
		t.Fatal("task host entry exposed private failure details")
	}
	cause := &os.PathError{Op: "open", Path: root, Err: os.ErrPermission}
	wrapped := &taskHostCommandError{cause: cause}
	if wrapped.Error() != fixed || !errors.Is(wrapped, os.ErrPermission) {
		t.Fatal("task host error lost classification or exposed its path")
	}
}

func TestTaskHostValidatesBeforeSelectingRuntimeAndHost(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	for _, selected := range []string{"", "hosted"} {
		t.Run(selected, func(t *testing.T) {
			t.Setenv("WITSELF_RUNTIME", selected)
			root := privateHarnessRoot(t)
			values := serviceFlags{root: root, pgbin: root, server: filepath.Join(root, "server"), worker: filepath.Join(root, "worker"), timeout: time.Second}
			validated, hosted := false, false
			err := runTaskHostWith(t.Context(), []string{"--runtime=local", "--root", root, "--generation", "generation-1"},
				func(_ *localcell.Cell, generation string) (localcell.ServiceDescriptor, error) {
					if os.Getenv("WITSELF_RUNTIME") != selected || generation != "generation-1" {
						t.Fatal("task host selected runtime before validating exact generation")
					}
					validated = true
					return localcell.ServiceDescriptor{Root: root, Payload: localcell.ServicePayload{Args: values.hostArgs()}}, nil
				}, func(_ *localcell.Cell) error {
					if !validated || os.Getenv("WITSELF_RUNTIME") != "local" {
						t.Fatal("task host ran before validation and local selection")
					}
					hosted = true
					return nil
				})
			if err != nil || !hosted {
				t.Fatal("validated task host did not run the existing host")
			}
		})
	}
}

func TestTaskHostRefusesInvalidPersistedPayloadBeforeRuntime(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("WITSELF_RUNTIME", "hosted")
	root := privateHarnessRoot(t)
	valid := serviceFlags{root: root, pgbin: root, server: filepath.Join(root, "server"), worker: filepath.Join(root, "worker"), timeout: time.Second}.hostArgs()
	for _, payload := range [][]string{nil, {"install"}, {"run", "--root", root}, append(append([]string{}, valid...), "--adapter=windows-logon-task")} {
		if err := runTaskHostWith(t.Context(), []string{"--runtime=local", "--root", root, "--generation", "generation-1"},
			func(*localcell.Cell, string) (localcell.ServiceDescriptor, error) {
				return localcell.ServiceDescriptor{Root: root, Payload: localcell.ServicePayload{Args: payload}}, nil
			}, func(*localcell.Cell) error {
				t.Fatal("task host ran an invalid persisted payload")
				return nil
			}); err == nil {
			t.Fatal("task host accepted an invalid persisted payload")
		}
		if os.Getenv("WITSELF_RUNTIME") != "hosted" {
			t.Fatal("task host selected runtime before payload validation")
		}
	}
}

func TestTaskHostValidationRefusalCannotSelectRuntimeOrRunHost(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("WITSELF_RUNTIME", "hosted")
	root := privateHarnessRoot(t)
	refused := errors.New("fixture identity refusal")
	err := runTaskHostWith(t.Context(), []string{"--runtime=local", "--root", root, "--generation", "generation-1"},
		func(*localcell.Cell, string) (localcell.ServiceDescriptor, error) {
			return localcell.ServiceDescriptor{}, refused
		},
		func(*localcell.Cell) error {
			t.Fatal("task host ran after identity validation failed")
			return nil
		})
	if !errors.Is(err, refused) || os.Getenv("WITSELF_RUNTIME") != "hosted" {
		t.Fatal("task host did not preserve the identity refusal before runtime selection")
	}
}

func TestWindowsTaskAdapterRequiresExplicitPlatformSelection(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("WITSELF_RUNTIME", "local")
	root := privateHarnessRoot(t)
	for _, test := range []struct{ goos, action, adapter string }{
		{"linux", "install", "windows-logon-task"},
		{"darwin", "install", "windows-logon-task"},
		{"windows", "install", "systemd-user"},
		{"windows", "run", "windows-logon-task"},
		{"windows", "status", "windows-logon-task"},
	} {
		args := []string{"service", test.action, "--root", root, "--adapter", test.adapter}
		if test.action == "run" {
			args = args[1:]
		}
		if err := runServiceCommandForGOOS(t.Context(), args, io.Discard, test.goos); err == nil {
			t.Fatal("task adapter accepted an unintended platform or operation")
		}
	}
	err := runServiceCommandForGOOS(t.Context(), []string{"service", "install", "--root", root, "--adapter=windows-logon-task"}, io.Discard, "windows")
	if err == nil || !strings.Contains(err.Error(), "explicit component paths are required") {
		t.Fatal("explicit Windows selection did not reach preparation validation")
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatal("refused task adapter selection modified installation state")
	}
}
