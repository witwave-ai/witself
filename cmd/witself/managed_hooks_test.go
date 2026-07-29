package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestInspectLegacyManagedHooksPreservesWitselfHomeIdentity(t *testing.T) {
	for _, runtimeName := range []string{
		transcriptcapture.RuntimeClaudeCode,
		transcriptcapture.RuntimeCodex,
	} {
		for _, tc := range []struct {
			name string
			pin  bool
		}{
			{name: "legacy-unpinned"},
			{name: "explicitly-pinned", pin: true},
		} {
			t.Run(runtimeName+"/"+tc.name, func(t *testing.T) {
				root := t.TempDir()
				t.Setenv(managedHooksTestRootEnv, root)
				witselfHome := ""
				if tc.pin {
					var err error
					witselfHome, err = cleanCopilotAbsolutePath(
						"WITSELF_HOME",
						filepath.Join(t.TempDir(), ".witself"),
					)
					if err != nil {
						t.Fatal(err)
					}
				}

				executable := filepath.Join(root, "bin", "witself")
				if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
					t.Fatal(err)
				}

				opts, err := managedHooksOptions(
					runtimeName,
					transcriptcapture.ModeRaw,
					executable,
					"default",
					"default",
					"scott",
					"home",
				)
				if err != nil {
					t.Fatal(err)
				}
				opts.WitselfHome = witselfHome
				if _, err := transcriptcapture.InstallManagedHooks(opts); err != nil {
					t.Fatal(err)
				}
				if _, err := transcriptcapture.ReconstructLegacyManagedHookOwnership(opts); err != nil {
					t.Fatalf("direct reconstruction failed: %v", err)
				}

				got, err := inspectLegacyManagedRuntimeHooksAtHome(
					runtimeName,
					transcriptcapture.ModeRaw,
					executable,
					"default",
					"default",
					"scott",
					"home",
					witselfHome,
				)
				if err != nil {
					t.Fatal(err)
				}
				if got.Missing {
					t.Fatal("legacy managed hooks unexpectedly reported missing")
				}
				if got.Ownership.PolicyPath != opts.PolicyPath() {
					t.Fatalf("policy path = %q, want %q", got.Ownership.PolicyPath, opts.PolicyPath())
				}
				if got.Ownership.RunnerPath == "" ||
					got.Ownership.RunnerDigest == "" ||
					got.Ownership.PolicyDigest == "" {
					t.Fatalf("incomplete reconstructed ownership: %#v", got.Ownership)
				}
			})
		}
	}
}
