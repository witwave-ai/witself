package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

func TestSameInstallationRoot(t *testing.T) {
	parent := t.TempDir()
	first, second := filepath.Join(parent, "first"), filepath.Join(parent, "second")
	for _, root := range []string{first, second} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal("cannot create installation root fixture")
		}
	}
	canonical, err := localcell.CanonicalRoot(first)
	if err != nil {
		t.Fatal("cannot canonicalize installation root fixture")
	}
	missing := filepath.Join(parent, "missing", "cell")
	for _, test := range []struct {
		name, persisted, recorded string
		want                      bool
	}{
		{"identical", canonical, canonical, true},
		{"different_siblings", first, second, false},
		{"missing_persisted_parent", missing, canonical, false},
		{"missing_recorded_parent", canonical, missing, false},
		{"both_parents_missing", missing, missing, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := sameInstallationRoot(test.persisted, test.recorded); got != test.want {
				t.Fatalf("installation root identity = %t, want %t", got, test.want)
			}
		})
	}
}

func TestServicePayloadRootSourceContract(t *testing.T) {
	for _, name := range []string{"service.go", "task_host.go"} {
		t.Run(name, func(t *testing.T) {
			source, err := os.ReadFile(name)
			if err != nil {
				t.Fatal("cannot read service payload root contract")
			}
			normalized := strings.ReplaceAll(string(source), "\r\n", "\n")
			if strings.Count(normalized, "!sameInstallationRoot(persisted.root, descriptor.Root)") != 1 {
				t.Fatal("service payload must compare installation root identity exactly once")
			}
			if strings.Contains(normalized, "persisted.root != descriptor.Root") {
				t.Fatal("service payload compared root spelling instead of installation identity")
			}
		})
	}
}

func assertTaskHostRootIdentity(t *testing.T, aliasRoot, realRoot, otherRoot string) {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	for _, test := range []struct {
		name, persisted, recorded string
		wantHosted                bool
	}{
		{"persisted_alias", aliasRoot, realRoot, true},
		{"recorded_alias", realRoot, aliasRoot, true},
		{"different_directory", otherRoot, realRoot, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("WITSELF_RUNTIME", "hosted")
			values := serviceFlags{
				root: test.persisted, pgbin: realRoot,
				server: filepath.Join(realRoot, "server"), worker: filepath.Join(realRoot, "worker"),
				timeout: time.Second,
			}
			validated, hosted := false, false
			err := runTaskHostWith(t.Context(), []string{"--runtime=local", "--root", realRoot, "--generation", "generation-1"},
				func(_ *localcell.Cell, generation string) (localcell.ServiceDescriptor, error) {
					if os.Getenv("WITSELF_RUNTIME") != "hosted" || generation != "generation-1" {
						t.Fatal("task host selected runtime before validating exact generation")
					}
					validated = true
					return localcell.ServiceDescriptor{Root: test.recorded, Payload: localcell.ServicePayload{Args: values.hostArgs()}}, nil
				}, func(_ *localcell.Cell) error {
					if !validated || os.Getenv("WITSELF_RUNTIME") != "local" {
						t.Fatal("task host ran before validation and local runtime selection")
					}
					hosted = true
					return nil
				})
			if test.wantHosted {
				if err != nil || !hosted {
					t.Fatal("task host refused an alternate spelling of its installation root")
				}
				return
			}
			if err == nil || err.Error() != "localcellharness: invalid task host payload" || hosted {
				t.Fatal("task host did not refuse a different installation root")
			}
			if os.Getenv("WITSELF_RUNTIME") != "hosted" {
				t.Fatal("task host selected runtime for a different installation root")
			}
			// runTaskHost applies this existing public error boundary to the
			// refusal returned by the injected runTaskHostWith seam.
			wrapped := &taskHostCommandError{cause: err}
			if wrapped.Error() != "service_task_host_failed: step=task_host:entry code=0 reason=entry_failed" {
				t.Fatal("task host root refusal lost its fixed public error")
			}
		})
	}
}
