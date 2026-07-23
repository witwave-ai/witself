package main

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

type pendingTransactionTestFixture struct {
	runtime   string
	operation string
	persisted *transcriptcapture.Config
	paths     []string
}

type pendingTransactionPathState struct {
	exists bool
	mode   os.FileMode
	raw    []byte
}

type pendingTransactionTestCase struct {
	name  string
	setup func(*testing.T) pendingTransactionTestFixture
}

func TestPendingIntegrationTransactionIsReadOnlyAcrossProviderFamilies(t *testing.T) {
	tests := []pendingTransactionTestCase{
		{
			name: "generic",
			setup: func(t *testing.T) pendingTransactionTestFixture {
				fixture := newGenericProviderTestFixture(t, transcriptcapture.RuntimeCodex)
				fixture.seedNonTargetConfig(t)
				desired := genericTransactionConfig(fixture)
				preflight, err := prepareGenericMCPInstallSnapshot(fixture.cli, desired, nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := beginGenericProviderInstallTransaction(nil, nil, desired, desired, preflight); err != nil {
					t.Fatal(err)
				}
				path, err := genericProviderTransactionPath(desired.Runtime)
				if err != nil {
					t.Fatal(err)
				}
				return pendingTransactionTestFixture{
					runtime: desired.Runtime, operation: genericProviderTransactionInstall,
					persisted: &desired, paths: []string{path, desired.RuntimeMCPConfigPath},
				}
			},
		},
		{
			name: "openclaw",
			setup: func(t *testing.T) pendingTransactionTestFixture {
				fixture := setupOpenClawIntegrationFixture(t)
				desired := openClawTransactionTestConfig(t, fixture)
				if _, err := beginOpenClawTransaction(openClawTransactionInstall, nil, &desired); err != nil {
					t.Fatal(err)
				}
				return pendingTransactionTestFixture{
					runtime: desired.Runtime, operation: openClawTransactionInstall,
					persisted: &desired, paths: []string{openClawTransactionPath(fixture.stateDir), fixture.state},
				}
			},
		},
		{
			name: "copilot",
			setup: func(t *testing.T) pendingTransactionTestFixture {
				desired := configuredCopilotTestConfig(t)
				desired.CaptureMode = transcriptcapture.ModeRaw
				desired.HookMode = transcriptcapture.HookModeNone
				_ = installCopilotCLIFixture(t)
				if _, err := beginCopilotTransaction(copilotTransactionInstall, nil, &desired); err != nil {
					t.Fatal(err)
				}
				return pendingTransactionTestFixture{
					runtime: desired.Runtime, operation: copilotTransactionInstall,
					persisted: &desired,
					paths:     []string{copilotTransactionPath(desired.RuntimeConfigRoot), desired.RuntimeMCPConfigPath},
				}
			},
		},
	}
	tests = append(tests, platformPendingTransactionTestCases()...)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := test.setup(t)
			before := snapshotPendingTransactionPaths(t, fixture.paths)
			operation, pending, err := pendingIntegrationTransaction(fixture.runtime, fixture.persisted)
			if err != nil || !pending || operation != fixture.operation {
				t.Fatalf("pending operation=%q pending=%t err=%v, want %q", operation, pending, err, fixture.operation)
			}
			after := snapshotPendingTransactionPaths(t, fixture.paths)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("pending inspection changed provider state:\nbefore=%#v\nafter=%#v", before, after)
			}
		})
	}
}

func snapshotPendingTransactionPaths(t *testing.T, paths []string) map[string]pendingTransactionPathState {
	t.Helper()
	states := make(map[string]pendingTransactionPathState, len(paths))
	for _, path := range paths {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			states[path] = pendingTransactionPathState{}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		state := pendingTransactionPathState{exists: true, mode: info.Mode()}
		if info.Mode().IsRegular() {
			state.raw, err = os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
		}
		states[path] = state
	}
	return states
}
