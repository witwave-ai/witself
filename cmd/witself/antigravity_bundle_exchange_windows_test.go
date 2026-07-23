//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsManagedInstructionAntigravityBundleLifecycleAcrossDiscoveryBoundary(t *testing.T) {
	configRoot := t.TempDir()
	live := filepath.Join(configRoot, "plugins", "witself-managed-test")
	current := antigravityPluginBundle{files: map[string][]byte{
		"plugin.json":      []byte("{\"name\":\"witself-managed-test\"}\n"),
		"rules/witself.md": []byte("# Current policy\n"),
	}}
	desired := antigravityPluginBundle{files: map[string][]byte{
		"plugin.json":      []byte("{\"name\":\"witself-managed-test\"}\n"),
		"rules/witself.md": []byte("# Desired policy\n"),
	}}

	if err := installAntigravityBundleDirectory(live, current, nil); err != nil {
		t.Fatalf("first install across plugin discovery boundary: %v", err)
	}
	if err := installAntigravityBundleDirectory(live, desired, &current); err != nil {
		t.Fatalf("replace bundle across plugin discovery boundary: %v", err)
	}
	if err := verifyAntigravityBundleDirectory(live, desired); err != nil {
		t.Fatalf("verify replacement bundle: %v", err)
	}
	for _, scratch := range []string{
		antigravityBundleSwapPath(live, current),
		antigravityBundleSwapPath(live, desired),
		antigravityBundleRemovalPath(live, current),
		antigravityBundleRemovalPath(live, desired),
	} {
		if _, err := os.Lstat(scratch); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("bundle transaction scratch remains at %s: %v", scratch, err)
		}
	}
	if err := removeExactAntigravityBundleDirectory(live, desired); err != nil {
		t.Fatalf("remove bundle across plugin discovery boundary: %v", err)
	}
	if _, err := os.Lstat(live); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed bundle remains after removal: %v", err)
	}
}

func TestWindowsManagedInstructionAntigravityPublishFailureRestoresPriorBundle(t *testing.T) {
	configRoot := t.TempDir()
	live := filepath.Join(configRoot, "plugins", "witself-managed-test")
	current := antigravityPluginBundle{files: map[string][]byte{
		"plugin.json":      []byte("{\"name\":\"witself-managed-test\"}\n"),
		"rules/witself.md": []byte("# Current policy\n"),
	}}
	desired := antigravityPluginBundle{files: map[string][]byte{
		"plugin.json":      []byte("{\"name\":\"witself-managed-test\"}\n"),
		"rules/witself.md": []byte("# Desired policy\n"),
	}}
	if err := installAntigravityBundleDirectory(live, current, nil); err != nil {
		t.Fatal(err)
	}
	staged := antigravityBundleSwapPath(live, desired)
	if err := os.Mkdir(staged, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := populateAntigravityBundleDirectory(staged, desired); err != nil {
		t.Fatal(err)
	}
	if err := syncAntigravityBundleDirectory(staged, desired); err != nil {
		t.Fatal(err)
	}

	var hookErr error
	originalHook := antigravityWindowsAfterBundleQuarantineForTest
	antigravityWindowsAfterBundleQuarantineForTest = func(_, staged, _ string) {
		hookErr = os.RemoveAll(staged)
	}
	t.Cleanup(func() { antigravityWindowsAfterBundleQuarantineForTest = originalHook })

	if _, err := exchangeAntigravityBundleDirectories(live, staged, current); err == nil {
		t.Fatal("bundle exchange accepted a staged publish failure")
	}
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err := verifyAntigravityBundleDirectory(live, current); err != nil {
		t.Fatalf("prior bundle was not restored: %v", err)
	}
	quarantine := antigravityBundleRemovalPath(live, current)
	if _, err := os.Lstat(quarantine); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restored exchange left quarantine at %s: %v", quarantine, err)
	}
}
