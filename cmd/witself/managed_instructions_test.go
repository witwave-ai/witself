package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testManagedInstructionsSpec(path string) managedInstructionsSpec {
	const (
		begin = "<!-- BEGIN TEST MANAGED BLOCK -->"
		end   = "<!-- END TEST MANAGED BLOCK -->"
	)
	return managedInstructionsSpec{
		path:        path,
		fileName:    "RUNTIME-RULES.md",
		tempPattern: ".runtime-rules.witself-*",
		beginMarker: begin,
		endMarker:   end,
		block:       []byte(begin + "\nmanaged policy\n" + end),
	}
}

func TestManagedInstructionsLifecyclePreservesArbitraryFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "RUNTIME-RULES.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("# Personal rules\r\n\r\nKeep these bytes exactly.\n")
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}
	spec := testManagedInstructionsSpec(path)

	installSnapshot, err := installManagedInstructions(spec)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantInstalled := append(append(append([]byte{}, spec.block...), '\n', '\n'), original...)
	if !bytes.Equal(installed, wantInstalled) {
		t.Fatalf("installed bytes = %q, want %q", installed, wantInstalled)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("installed mode: info=%v err=%v", info, err)
	}
	if matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".runtime-rules.witself-*")); err != nil || len(matches) != 0 {
		t.Fatalf("temporary files remain: matches=%v err=%v", matches, err)
	}

	if _, err := installManagedInstructions(spec); err != nil {
		t.Fatal(err)
	}
	reinstalled, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reinstalled, installed) {
		t.Fatal("idempotent install changed the file")
	}
	if err := installSnapshot.restore(); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rolledBack, original) {
		t.Fatal("install snapshot did not restore the original file")
	}

	if _, err := installManagedInstructions(spec); err != nil {
		t.Fatal(err)
	}

	removeSnapshot, err := removeManagedInstructions(spec)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(removed, original) {
		t.Fatalf("removed bytes = %q, want %q", removed, original)
	}
	if err := removeSnapshot.restore(); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, installed) {
		t.Fatal("remove snapshot did not restore the installed file")
	}
}

func TestManagedInstructionsRemoveEmptyAndSnapshotRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules", "witself.md")
	spec := testManagedInstructionsSpec(path)
	spec.removeEmpty = true

	createdSnapshot, err := installManagedInstructions(spec)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("created mode: info=%v err=%v", info, err)
	}
	if err := createdSnapshot.restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("creation rollback did not remove the file: %v", err)
	}

	if _, err := installManagedInstructions(spec); err != nil {
		t.Fatal(err)
	}

	removedSnapshot, err := removeManagedInstructions(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("dedicated managed-only file was not deleted: %v", err)
	}
	if err := removedSnapshot.restore(); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, installed) {
		t.Fatal("removal rollback did not restore the dedicated file")
	}
}

func TestManagedInstructionsRemoveEmptyPreservesSymlinkStructure(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.md")
	link := filepath.Join(root, "rules.md")
	spec := testManagedInstructionsSpec(link)
	spec.removeEmpty = true
	managedOnly := append(append([]byte{}, spec.block...), '\n')
	if err := os.WriteFile(target, managedOnly, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if _, err := removeManagedInstructions(spec); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("remove replaced symlink: info=%v err=%v", info, err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("symlink target retained %q", got)
	}
}

func TestManagedInstructionsRejectMalformedAndMultipleMarkersWithoutWriting(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  func(managedInstructionsSpec) []byte
		want string
	}{
		"incomplete": {
			raw:  func(spec managedInstructionsSpec) []byte { return []byte("personal\n" + spec.beginMarker + "\n") },
			want: "RUNTIME-RULES.md contains an incomplete",
		},
		"multiple": {
			raw: func(spec managedInstructionsSpec) []byte {
				return append(append(append([]byte{}, spec.block...), '\n'), spec.block...)
			},
			want: "RUNTIME-RULES.md contains multiple",
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "RUNTIME-RULES.md")
			spec := testManagedInstructionsSpec(path)
			original := tc.raw(spec)
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := installManagedInstructions(spec); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("install error = %v, want %q", err, tc.want)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, original) {
				t.Fatal("rejected install modified the file")
			}
		})
	}
}

func TestManagedInstructionsUsesConfiguredFilenameInTemporaryWriteErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "RUNTIME-RULES.md")
	spec := testManagedInstructionsSpec(path)
	spec.tempPattern = "invalid/pattern-*"
	if _, err := installManagedInstructions(spec); err == nil || !strings.Contains(err.Error(), "create temporary RUNTIME-RULES.md") {
		t.Fatalf("install error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed install created the destination: %v", err)
	}
}

func TestManagedInstructionsRollbackRefusesConcurrentEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "RUNTIME-RULES.md")
	original := []byte("# Original\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := installManagedInstructions(testManagedInstructionsSpec(path))
	if err != nil {
		t.Fatal(err)
	}
	concurrent := []byte("# User changed this after install\n")
	if err := os.WriteFile(path, concurrent, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.restore(); err == nil || !strings.Contains(err.Error(), "changed after Witself updated it") {
		t.Fatalf("rollback error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, concurrent) {
		t.Fatalf("rollback overwrote concurrent edit: %q", got)
	}
}

func TestManagedInstructionsRollbackRefusesRecreatedDedicatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "witself-memory-routing.md")
	spec := testManagedInstructionsSpec(path)
	spec.removeEmpty = true
	managedOnly := append(append([]byte{}, spec.block...), '\n')
	if err := os.WriteFile(path, managedOnly, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := removeManagedInstructions(spec)
	if err != nil {
		t.Fatal(err)
	}
	recreated := []byte("# Replacement rule\n")
	if err := os.WriteFile(path, recreated, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.restore(); err == nil || !strings.Contains(err.Error(), "recreated after Witself removed it") {
		t.Fatalf("rollback error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, recreated) {
		t.Fatalf("rollback overwrote recreated file: %q", got)
	}
}

func TestManagedInstructionsIdempotentSnapshotDoesNotRollbackLaterChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "RUNTIME-RULES.md")
	spec := testManagedInstructionsSpec(path)
	installed := append(append([]byte{}, spec.block...), '\n')
	if err := os.WriteFile(path, installed, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := installManagedInstructions(spec)
	if err != nil {
		t.Fatal(err)
	}
	later := append(installed, []byte("# Later rule\n")...)
	if err := os.WriteFile(path, later, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.restore(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, later) {
		t.Fatalf("idempotent rollback changed later content: %q", got)
	}
}

func TestManagedInstructionsRollbackRefusesSameBytesReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "RUNTIME-RULES.md")
	snapshot, err := installManagedInstructions(testManagedInstructionsSpec(path))
	if err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, installed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.restore(); err == nil || !strings.Contains(err.Error(), "changed after Witself updated it") {
		t.Fatalf("rollback error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, installed) {
		t.Fatalf("rollback changed replacement: %q", got)
	}
}

func TestManagedInstructionsRollbackRefusesSymlinkSubstitution(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "RUNTIME-RULES.md")
	snapshot, err := installManagedInstructions(testManagedInstructionsSpec(path))
	if err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "replacement.md")
	if err := os.WriteFile(target, installed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.restore(); err == nil || !strings.Contains(err.Error(), "changed after Witself updated it") {
		t.Fatalf("rollback error = %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("rollback replaced the substituted symlink")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, installed) {
		t.Fatalf("rollback changed symlink target: %q", got)
	}
}

func TestManagedInstructionsRollbackRefusesRecreatedSourceSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.md")
	link := filepath.Join(root, "rules.md")
	if err := os.WriteFile(target, []byte("# Personal rules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	snapshot, err := installManagedInstructions(testManagedInstructionsSpec(link))
	if err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.restore(); err == nil || !strings.Contains(err.Error(), "symlink changed after Witself updated it") {
		t.Fatalf("rollback error = %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, installed) {
		t.Fatalf("rollback changed recreated symlink target: %q", got)
	}
}

func TestManagedInstructionsWriteDetectsReplacementAtMutationBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "RUNTIME-RULES.md")
	original := []byte("# Original rules\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := installManagedInstructions(testManagedInstructionsSpec(path))
	if err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var replacementInfo os.FileInfo
	managedInstructionsBeforeMutationForTest = func() {
		managedInstructionsBeforeMutationForTest = nil
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, installed, 0o600); err != nil {
			t.Fatal(err)
		}
		replacementInfo, err = os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { managedInstructionsBeforeMutationForTest = nil })

	if err := snapshot.restore(); err == nil || !strings.Contains(err.Error(), "changed during update") {
		t.Fatalf("rollback error = %v", err)
	}
	gotInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if replacementInfo == nil || !os.SameFile(gotInfo, replacementInfo) {
		t.Fatal("rollback did not preserve the replacement inode")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, installed) {
		t.Fatalf("rollback changed the replacement bytes: %q", got)
	}
}

func TestManagedInstructionsDeleteDetectsReplacementAtMutationBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules", "witself.md")
	spec := testManagedInstructionsSpec(path)
	spec.removeEmpty = true
	snapshot, err := installManagedInstructions(spec)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var replacementInfo os.FileInfo
	managedInstructionsBeforeMutationForTest = func() {
		managedInstructionsBeforeMutationForTest = nil
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, installed, 0o600); err != nil {
			t.Fatal(err)
		}
		replacementInfo, err = os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { managedInstructionsBeforeMutationForTest = nil })

	if err := snapshot.restore(); err == nil || !strings.Contains(err.Error(), "changed during update") {
		t.Fatalf("rollback error = %v", err)
	}
	gotInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if replacementInfo == nil || !os.SameFile(gotInfo, replacementInfo) {
		t.Fatal("rollback delete did not preserve the replacement inode")
	}
}

func TestManagedInstructionsCreateDoesNotOverwriteBoundaryRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules", "witself.md")
	spec := testManagedInstructionsSpec(path)
	spec.removeEmpty = true
	managedOnly := append(append([]byte{}, spec.block...), '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, managedOnly, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := removeManagedInstructions(spec)
	if err != nil {
		t.Fatal(err)
	}
	replacement := []byte("# Created during rollback\n")
	managedInstructionsBeforeMutationForTest = func() {
		managedInstructionsBeforeMutationForTest = nil
		if err := os.WriteFile(path, replacement, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { managedInstructionsBeforeMutationForTest = nil })

	if err := snapshot.restore(); err == nil || !strings.Contains(err.Error(), "changed during update") {
		t.Fatalf("rollback error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatalf("rollback overwrote boundary-race file: %q", got)
	}
}

func TestManagedInstructionsDetectsModeChangeAtMutationBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "RUNTIME-RULES.md")
	if err := os.WriteFile(path, []byte("# Original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := installManagedInstructions(testManagedInstructionsSpec(path))
	if err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	managedInstructionsBeforeMutationForTest = func() {
		managedInstructionsBeforeMutationForTest = nil
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { managedInstructionsBeforeMutationForTest = nil })
	if err := snapshot.restore(); err == nil || !strings.Contains(err.Error(), "changed during update") {
		t.Fatalf("rollback error = %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, installed) {
		t.Fatalf("rollback changed boundary file: got=%q err=%v", got, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("rollback changed boundary mode: info=%v err=%v", info, err)
	}
}

func TestManagedInstructionsDetectsSourceSymlinkChangeAtMutationBoundary(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.md")
	replacementTarget := filepath.Join(root, "replacement.md")
	link := filepath.Join(root, "rules.md")
	if err := os.WriteFile(target, []byte("# Original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacementTarget, []byte("# Replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	snapshot, err := installManagedInstructions(testManagedInstructionsSpec(link))
	if err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	managedInstructionsBeforeMutationForTest = func() {
		managedInstructionsBeforeMutationForTest = nil
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(replacementTarget, link); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { managedInstructionsBeforeMutationForTest = nil })
	if err := snapshot.restore(); err == nil || !strings.Contains(err.Error(), "changed during update") {
		t.Fatalf("rollback error = %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, installed) {
		t.Fatalf("rollback changed original target: got=%q err=%v", got, err)
	}
	if got, err := os.Readlink(link); err != nil || got != replacementTarget {
		t.Fatalf("rollback changed replacement symlink: target=%q err=%v", got, err)
	}
}

func TestManagedInstructionsPreservesSubstitutedTemporaryEntries(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T, string, managedInstructionsSpec, managedInstructionsSnapshot, []byte)
	}{
		{
			name: "write temp",
			run: func(t *testing.T, path string, spec managedInstructionsSpec, snapshot managedInstructionsSnapshot, installed []byte) {
				substitute := []byte("# Substituted temp\n")
				managedInstructionsBeforeMutationForTest = func() {
					managedInstructionsBeforeMutationForTest = nil
					matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), spec.tempPattern))
					if err != nil || len(matches) != 1 {
						t.Fatalf("temporary entries: %v err=%v", matches, err)
					}
					if err := os.Remove(matches[0]); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(matches[0], substitute, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := snapshot.restore(); err == nil || !strings.Contains(err.Error(), "preserved state") {
					t.Fatalf("rollback error = %v", err)
				}
				assertManagedInstructionsFileAndPreservedBytes(t, path, installed, substitute)
			},
		},
		{
			name: "delete tomb",
			run: func(t *testing.T, path string, _ managedInstructionsSpec, snapshot managedInstructionsSnapshot, installed []byte) {
				substitute := []byte("# Substituted tomb\n")
				managedInstructionsBeforeDeleteMutationForTest = func(tombPath string) {
					managedInstructionsBeforeDeleteMutationForTest = nil
					if err := os.WriteFile(tombPath, substitute, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := snapshot.restore(); err == nil || !strings.Contains(err.Error(), "changed during update") {
					t.Fatalf("rollback error = %v", err)
				}
				assertManagedInstructionsFileAndPreservedBytes(t, path, installed, substitute)
			},
		},
		{
			name: "recovery capture",
			run: func(t *testing.T, path string, _ managedInstructionsSpec, snapshot managedInstructionsSnapshot, installed []byte) {
				substitute := []byte("# Substituted capture\n")
				managedInstructionsBeforeMutationForTest = func() {
					managedInstructionsBeforeMutationForTest = nil
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, installed, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				managedInstructionsBeforeRecoveryForTest = func() {
					managedInstructionsBeforeRecoveryForTest = nil
					entries, err := os.ReadDir(filepath.Dir(path))
					if err != nil {
						t.Fatal(err)
					}
					for _, entry := range entries {
						if strings.Contains(entry.Name(), "-recovery-") {
							capturePath := filepath.Join(filepath.Dir(path), entry.Name())
							if err := os.Remove(capturePath); err != nil {
								t.Fatal(err)
							}
							if err := os.WriteFile(capturePath, substitute, 0o600); err != nil {
								t.Fatal(err)
							}
							return
						}
					}
					t.Fatal("recovery capture was not found")
				}
				if err := snapshot.restore(); err == nil || !strings.Contains(err.Error(), "preserved state") {
					t.Fatalf("rollback error = %v", err)
				}
				assertManagedInstructionsFileAndPreservedBytes(t, path, installed, substitute)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rules", "witself.md")
			spec := testManagedInstructionsSpec(path)
			spec.removeEmpty = true
			if tc.name != "delete tomb" {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("# Original\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := installManagedInstructions(spec)
			if err != nil {
				t.Fatal(err)
			}
			installed, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				managedInstructionsBeforeMutationForTest = nil
				managedInstructionsBeforeRecoveryForTest = nil
				managedInstructionsBeforeDeleteMutationForTest = nil
			})
			tc.run(t, path, spec, snapshot, installed)
		})
	}
}

func TestManagedInstructionsSyncFailureRestoresPriorState(t *testing.T) {
	for _, name := range []string{"create", "replace", "delete"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rules", "witself.md")
			spec := testManagedInstructionsSpec(path)
			spec.removeEmpty = true
			var original []byte
			if name != "create" {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if name == "delete" {
					original = append(append([]byte{}, spec.block...), '\n')
				} else {
					original = []byte("# Original\n")
				}
				if err := os.WriteFile(path, original, 0o640); err != nil {
					t.Fatal(err)
				}
			}
			actualSync := managedInstructionsSyncDirectory
			calls := 0
			managedInstructionsSyncDirectory = func(path string) error {
				calls++
				if calls == 1 {
					return errors.New("injected directory sync failure")
				}
				return actualSync(path)
			}
			t.Cleanup(func() { managedInstructionsSyncDirectory = actualSync })
			var err error
			if name == "delete" {
				_, err = removeManagedInstructions(spec)
			} else {
				_, err = installManagedInstructions(spec)
			}
			if err == nil || !strings.Contains(err.Error(), "injected directory sync failure") {
				t.Fatalf("mutation error = %v", err)
			}
			if name == "create" {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed create left destination: %v", err)
				}
				return
			}
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, original) {
				t.Fatalf("sync failure did not restore original: got=%q err=%v", got, err)
			}
		})
	}
}

func assertManagedInstructionsFileAndPreservedBytes(t *testing.T, path string, wantPath, wantPreserved []byte) {
	t.Helper()
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, wantPath) {
		t.Fatalf("destination changed: got=%q err=%v", got, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		got, err := os.ReadFile(filepath.Join(filepath.Dir(path), entry.Name()))
		if err == nil && bytes.Equal(got, wantPreserved) {
			return
		}
	}
	t.Fatalf("substituted bytes were not preserved: %q", wantPreserved)
}
