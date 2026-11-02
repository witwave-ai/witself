//go:build darwin || linux

package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/localcell"
)

// Both branches must select the transport fallback after reopening the persisted
// configuration. These actual harness refusals carry no more specific site.
func TestServiceFailureSiteHarnessControlTransportFallback(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("WITSELF_RUNTIME", "local")
	for _, action := range []string{"run", "health"} {
		t.Run(action, func(t *testing.T) {
			host, err := os.Executable()
			if err != nil {
				t.Fatal("cannot resolve fixture host")
			}
			if action == "run" {
				// Fail host ownership before acquiring a lease or starting children.
				host = filepath.Join(t.TempDir(), "missing-fixture-host")
			}
			root, _ := prepareServiceHarnessInstallation(t, host, "foreground", []byte(strings.Repeat("A", 43)))
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal("cannot capture harness site receipt")
			}
			previous := os.Stderr
			os.Stderr = writer
			defer func() {
				os.Stderr = previous
				_ = writer.Close()
				_ = reader.Close()
			}()
			args := []string{"service", action, "--root", root}
			if action == "run" {
				args = []string{"run", "--root", root}
			}
			failure := run(t.Context(), args, io.Discard)
			closeErr := writer.Close()
			os.Stderr = previous
			data, readErr := io.ReadAll(reader)
			if closeErr != nil || readErr != nil {
				t.Fatal("cannot read harness site receipt")
			}
			expected := localcell.ErrServiceControlRefused
			if action == "run" {
				expected = localcell.ErrServiceOwnershipConflict
			}
			if !errors.Is(failure, expected) {
				t.Fatal("harness fixture did not reach an unlocated transport refusal")
			}
			if string(data) != "native service host site=control_transport\n" {
				t.Fatal("harness transport fallback mislabeled an actual unlocated refusal")
			}
		})
	}
}
