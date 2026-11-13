//go:build windows

package localcell

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

// recoveryBoundaryLink creates a symbolic link fixture at path. Without the
// symbolic link privilege it creates a junction, the mandatory Windows link
// fixture, so the row never skips.
func recoveryBoundaryLink(t *testing.T, path, target string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		if !errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
			t.Fatal("cannot create recovery boundary link fixture")
		}
		t.Log("symbolic link privilege unavailable; the recovery boundary link fixture is a junction")
		createTestJunction(t, path, target)
	}
}

// recoveryBoundaryNonPrivate grants Everyone read access to an existing fixture
// object, which the private-object rules refuse.
func recoveryBoundaryNonPrivate(t *testing.T, path string, _ bool) {
	t.Helper()
	setTestDACL(t, path, "(A;;FR;;;WD)", false, false)
}

// countPrivateTreeValidations walks root through the snapshot seam with the
// real filepath.WalkDir and the real Windows validators, wired exactly as
// validatePrivateTree wires them, and counts every validated path.
func countPrivateTreeValidations(t *testing.T, root string) int {
	t.Helper()
	count := 0
	err := validatePrivateTreeSnapshot(root, filepath.WalkDir, func(path string, directory bool) error {
		return privateTreeEntry(root, path, directory, func() error {
			count++
			return validatePrivate(path, directory)
		}, func(pg string) error {
			count++
			return runtimeconfig.WindowsPrivatePostgresValidate(pg, path, directory)
		})
	}, os.Lstat)
	if err != nil {
		t.Fatalf("counted private tree walk refused the fixture tree: %v", err)
	}
	return count
}

func assertRecoveryOwnRefused(t *testing.T, c *Cell, path string) {
	t.Helper()
	lock, err := c.own(false)
	if lock != nil {
		_ = lock.Close()
	}
	assertPrivateRefusal(t, err, path)
	if errors.Is(err, ErrLocked) {
		t.Fatal("recovery boundary refusal was reported as a busy cell")
	}
}

func TestWindowsPrivateTreeRecoveryBoundary(t *testing.T) {
	t.Run("a_refused_object_below_the_boundary", func(t *testing.T) {
		for _, kind := range []string{"world_readable_file", "world_readable_directory", "junction"} {
			t.Run(kind, func(t *testing.T) {
				c, _ := unitCell(t)
				for _, directory := range []string{c.path("recovery"), c.path("recovery", "x")} {
					if err := testMkdir(directory, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				object, directory := c.path("recovery", "x", "object"), false
				switch kind {
				case "world_readable_file":
					if err := testWriteFile(object, []byte("synthetic checkpoint bytes"), 0o600); err != nil {
						t.Fatal(err)
					}
					setTestDACL(t, object, "(A;;FR;;;WD)", false, false)
				case "world_readable_directory":
					directory = true
					if err := testMkdir(object, 0o700); err != nil {
						t.Fatal(err)
					}
					setTestDACL(t, object, "(A;;FR;;;WD)", false, false)
				case "junction":
					createTestJunction(t, object, testPrivateRoot(t))
				}
				if validatePrivate(object, directory) == nil {
					t.Fatal("fixture object passes the strict rule")
				}
				if err := validatePrivateTree(c.cfg.Root); err != nil {
					t.Fatalf("an object below the recovery boundary refused the tree: %v", err)
				}
				lock, err := c.own(false)
				if err != nil {
					t.Fatalf("an object below the recovery boundary refused ownership: %v", err)
				}
				if err := lock.Close(); err != nil {
					t.Fatal(err)
				}
				// Control: the same object, moved where the walk reaches it.
				moved := c.path("spool", "object")
				if err := os.Rename(object, moved); err != nil {
					t.Fatal("cannot move the refused fixture object under spool")
				}
				assertPrivateRefusal(t, validatePrivateTree(c.cfg.Root), moved)
				assertRecoveryOwnRefused(t, c, moved)
			})
		}
	})

	t.Run("b_non_private_boundary", func(t *testing.T) {
		for _, test := range []struct{ name, extra string }{
			{"Everyone", "(A;;FR;;;WD)"},
			{"Users", "(A;;FR;;;BU)"},
		} {
			t.Run(test.name, func(t *testing.T) {
				c, _ := unitCell(t)
				boundary := c.path("recovery")
				if err := testMkdir(boundary, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := testWriteFile(filepath.Join(boundary, "inside"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				setTestDACL(t, boundary, test.extra, false, false)
				want := validatePrivate(boundary, true)
				if want == nil {
					t.Fatal("fixture boundary passes the strict rule")
				}
				got := validatePrivateTree(c.cfg.Root)
				if got == nil || got.Error() != want.Error() {
					t.Fatalf("non-private boundary walk returned %v, want the unchanged refusal %v", got, want)
				}
				assertPrivateRefusal(t, got, boundary)
				assertRecoveryOwnRefused(t, c, boundary)
			})
		}
	})

	t.Run("c_junction_boundary", func(t *testing.T) {
		c, _ := unitCell(t)
		outside := testPrivateRoot(t)
		if err := testWriteFile(filepath.Join(outside, "witness"), []byte("outside bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		boundary := c.path("recovery")
		createTestJunction(t, boundary, outside)
		before := windowsTreeSnapshot(t, outside)
		assertPrivateRefusal(t, validatePrivateTree(c.cfg.Root), boundary)
		assertPrivateRefusal(t, c.validateRoot(false), boundary)
		assertRecoveryOwnRefused(t, c, boundary)
		if !reflect.DeepEqual(before, windowsTreeSnapshot(t, outside)) {
			t.Fatal("junction boundary refusal changed its target")
		}
	})

	t.Run("d_validated_path_count", func(t *testing.T) {
		c, _ := unitCell(t)
		if err := testMkdir(c.path("recovery"), 0o700); err != nil {
			t.Fatal(err)
		}
		empty := countPrivateTreeValidations(t, c.cfg.Root)
		objects := 0
		for i := range 4 {
			directory := c.path("recovery", fmt.Sprintf("checkpoint-%d", i))
			if err := testMkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			objects++
			for j := range 5 {
				if err := testWriteFile(filepath.Join(directory, fmt.Sprintf("member-%d", j)), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				objects++
			}
		}
		if got := countPrivateTreeValidations(t, c.cfg.Root); got != empty {
			t.Fatalf("%d private objects below the boundary changed the validated path count from %d to %d", objects, empty, got)
		}
		// Objects the strict rule refuses would fail any walk that reached them.
		refused := c.path("recovery", "checkpoint-0", "world-readable")
		if err := testWriteFile(refused, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		setTestDACL(t, refused, "(A;;FR;;;WD)", false, false)
		createTestJunction(t, c.path("recovery", "checkpoint-1", "junction"), testPrivateRoot(t))
		if got := countPrivateTreeValidations(t, c.cfg.Root); got != empty {
			t.Fatalf("refused objects below the boundary changed the validated path count from %d to %d", empty, got)
		}
		if err := validatePrivateTree(c.cfg.Root); err != nil {
			t.Fatalf("objects below the boundary refused the real tree validation: %v", err)
		}
		// Control: the same counter sees every object outside the boundary.
		for i := range objects {
			if err := testWriteFile(c.path("spool", fmt.Sprintf("member-%d", i)), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if got := countPrivateTreeValidations(t, c.cfg.Root); got != empty+objects {
			t.Fatalf("%d objects under spool counted %d validated paths, want %d", objects, got, empty+objects)
		}
	})
}
