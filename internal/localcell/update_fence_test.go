package localcell

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func updateTestFence(t *testing.T, c *Cell) maintenanceFence {
	t.Helper()
	fence := maintenanceFence{Generation: 1, OperationID: "update-test", Since: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)}
	if err := writeAtomic(c.path("journal", "maintenance.json"), fence, nil); err != nil {
		t.Fatal("cannot prepare maintenance fence")
	}
	return fence
}

func TestUpdateFenceReadResults(t *testing.T) {
	t.Run("absent_leaf", func(t *testing.T) {
		c, _ := unitCell(t)
		fence, err := c.readMaintenanceFence()
		if err != nil || fence != nil || c.maintenanceFenceAllows(0) != nil {
			t.Fatal("absent fence refused ordinary predicate")
		}
		if !errors.Is(c.maintenanceFenceAllows(1), errMaintenanceFenceActive) {
			t.Fatal("absent fence authorized a transition")
		}
	})
	t.Run("absent_parent", func(t *testing.T) {
		c, _ := unitCell(t)
		if err := os.Remove(c.path("journal")); err != nil {
			t.Fatal("cannot prepare absent parent fixture")
		}
		fence, err := c.readMaintenanceFence()
		if err != nil || fence != nil {
			t.Fatal("missing fence parent was not absent")
		}
	})
	t.Run("valid", func(t *testing.T) {
		c, _ := unitCell(t)
		want := updateTestFence(t, c)
		before, err := os.ReadFile(c.path("journal", "maintenance.json"))
		if err != nil {
			t.Fatal("cannot read fixture fence")
		}
		got, err := c.readMaintenanceFence()
		if err != nil || got == nil || !reflect.DeepEqual(*got, want) {
			t.Fatal("valid fence was not returned")
		}
		for _, generation := range []int{-1, 0, want.Generation + 1} {
			if !errors.Is(c.maintenanceFenceAllows(generation), errMaintenanceFenceActive) {
				t.Fatal("unmatched maintenance generation was allowed")
			}
		}
		if err := c.maintenanceFenceAllows(want.Generation); err != nil {
			t.Fatal("matching maintenance generation was refused")
		}
		after, err := os.ReadFile(c.path("journal", "maintenance.json"))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("fence predicate changed existing bytes")
		}
	})
}

func TestUpdateFenceInvalidParent(t *testing.T) {
	check := func(t *testing.T, c *Cell) {
		t.Helper()
		want := privateDir(c.path("journal"), false)
		if want == nil {
			t.Fatal("fixture parent unexpectedly valid")
		}
		fence, err := c.readMaintenanceFence()
		if fence != nil || err == nil || errors.Is(err, errMaintenanceFenceActive) || err.Error() != want.Error() {
			t.Fatal("absent leaf lost its distinct parent validation error")
		}
		if err = c.maintenanceFenceAllows(0); err == nil || errors.Is(err, errMaintenanceFenceActive) || err.Error() != want.Error() {
			t.Fatal("predicate converted parent validation error to fence refusal or absence")
		}
	}
	t.Run("parent_file", func(t *testing.T) {
		c, _ := unitCell(t)
		if err := os.Remove(c.path("journal")); err != nil {
			t.Fatal("cannot remove empty fixture journal directory")
		}
		if err := testWriteFile(c.path("journal"), []byte("fixture parent"), 0o600); err != nil {
			t.Fatal("cannot prepare invalid fixture parent")
		}
		check(t, c)
	})
	// POSIX mode bits do not express Windows directory privacy. The common
	// parent-file case above still exercises the separate result on Windows.
	if runtime.GOOS != "windows" {
		t.Run("mode_0755_absent_leaf", func(t *testing.T) {
			c, _ := unitCell(t)
			if err := os.Chmod(c.path("journal"), 0o755); err != nil {
				t.Fatal("cannot prepare non-private journal directory")
			}
			check(t, c)
		})
	}
}

func TestUpdateFenceCorruptionFailsClosed(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		value any
	}{
		{"zero_generation", map[string]any{"generation": 0, "operation_id": "update-test", "since": time.Now().UTC()}},
		{"unsafe_operation_id", map[string]any{"generation": 1, "operation_id": "../foreign", "since": time.Now().UTC()}},
		{"unknown_field", map[string]any{"generation": 1, "operation_id": "update-test", "since": time.Now().UTC(), "unexpected": true}},
		{"missing_since", map[string]any{"generation": 1, "operation_id": "update-test"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			c, _ := unitCell(t)
			if err := writeAtomic(c.path("journal", "maintenance.json"), fixture.value, nil); err != nil {
				t.Fatal("cannot prepare invalid fence")
			}
			before, err := os.ReadFile(c.path("journal", "maintenance.json"))
			if err != nil {
				t.Fatal("cannot inspect fixture fence")
			}
			fence, err := c.readMaintenanceFence()
			if fence != nil || !errors.Is(err, errMaintenanceFenceActive) ||
				!errors.Is(c.maintenanceFenceAllows(0), errMaintenanceFenceActive) ||
				!errors.Is(c.maintenanceFenceAllows(1), errMaintenanceFenceActive) {
				t.Fatal("invalid present fence was not refused")
			}
			after, err := os.ReadFile(c.path("journal", "maintenance.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("refusal changed invalid fence evidence")
			}
		})
	}
	t.Run("malformed_json", func(t *testing.T) {
		c, _ := unitCell(t)
		if err := testWriteFile(c.path("journal", "maintenance.json"), []byte("{"), 0o600); err != nil {
			t.Fatal("cannot prepare malformed fixture")
		}
		fence, err := c.readMaintenanceFence()
		if fence != nil || !errors.Is(err, errMaintenanceFenceActive) {
			t.Fatal("malformed present fence was treated as absent")
		}
	})
	t.Run("unsafe_directory_leaf", func(t *testing.T) {
		c, _ := unitCell(t)
		if err := testMkdir(c.path("journal", "maintenance.json"), 0o700); err != nil {
			t.Fatal("cannot prepare unsafe fixture leaf")
		}
		fence, err := c.readMaintenanceFence()
		if fence != nil || !errors.Is(err, errMaintenanceFenceActive) {
			t.Fatal("unsafe present fence was not refused")
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		c, _ := unitCell(t)
		updateTestFence(t, c)
		fence, err := c.readMaintenanceFenceWith(func(string, any) error { return os.ErrPermission })
		if fence != nil || !errors.Is(err, errMaintenanceFenceActive) {
			t.Fatal("unreadable present fence was not refused")
		}
	})
	if runtime.GOOS != "windows" {
		t.Run("unsafe_parent_mode", func(t *testing.T) {
			c, _ := unitCell(t)
			updateTestFence(t, c)
			if err := os.Chmod(c.path("journal"), 0o755); err != nil {
				t.Fatal("cannot prepare unsafe fence parent")
			}
			fence, err := c.readMaintenanceFence()
			if fence != nil || !errors.Is(err, errMaintenanceFenceActive) {
				t.Fatal("unsafe parent of a present fence was not refused")
			}
		})
		t.Run("unsafe_mode", func(t *testing.T) {
			c, _ := unitCell(t)
			updateTestFence(t, c)
			if err := os.Chmod(c.path("journal", "maintenance.json"), 0o644); err != nil {
				t.Fatal("cannot prepare unsafe fence mode")
			}
			fence, err := c.readMaintenanceFence()
			if fence != nil || !errors.Is(err, errMaintenanceFenceActive) {
				t.Fatal("non-private present fence was not refused")
			}
		})
	}
}

func TestUpdateFenceObservedThenGone(t *testing.T) {
	c, _ := unitCell(t)
	updateTestFence(t, c)
	called := false
	fence, err := c.readMaintenanceFenceWith(func(path string, dst any) error {
		called = true
		if err := os.Remove(path); err != nil {
			t.Fatal("cannot simulate fixture fence disappearing after observation")
		}
		return readJSON(path, dst)
	})
	if !called || fence != nil || !errors.Is(err, errMaintenanceFenceActive) {
		t.Fatal("observed fence disappearing during read did not fail closed")
	}
}

func TestUpdateFenceSymlinkEvidence(t *testing.T) {
	// These POSIX fixtures need no symlink privilege or platform skip. Windows
	// keeps the nonregular and private-record cases in the common result suite.
	if runtime.GOOS != "windows" {
		t.Run("leaf", func(t *testing.T) {
			c, _ := unitCell(t)
			target := filepath.Join(testPrivateRoot(t), "fence.json")
			fence := maintenanceFence{Generation: 1, OperationID: "foreign-update", Since: time.Now().UTC()}
			if err := writeAtomic(target, fence, nil); err != nil {
				t.Fatal("cannot prepare symlink target fence")
			}
			before, err := os.ReadFile(target)
			if err != nil {
				t.Fatal("cannot read symlink target fence")
			}
			if err := os.Symlink(target, c.path("journal", "maintenance.json")); err != nil {
				t.Fatal("cannot prepare symlink fence fixture")
			}
			got, err := c.readMaintenanceFence()
			if got != nil || !errors.Is(err, errMaintenanceFenceActive) ||
				!errors.Is(c.maintenanceFenceAllows(fence.Generation), errMaintenanceFenceActive) {
				t.Fatal("symlink leaf granted target fence authority")
			}
			after, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("symlink fence refusal changed target bytes")
			}
		})
		for _, present := range []bool{false, true} {
			name := "parent_absent_leaf"
			if present {
				name = "parent_present_leaf"
			}
			t.Run(name, func(t *testing.T) {
				c, _ := unitCell(t)
				target := testPrivateRoot(t)
				var before []byte
				if present {
					fence := maintenanceFence{Generation: 1, OperationID: "foreign-update", Since: time.Now().UTC()}
					if err := writeAtomic(filepath.Join(target, "maintenance.json"), fence, nil); err != nil {
						t.Fatal("cannot prepare target directory fence")
					}
					var err error
					before, err = os.ReadFile(filepath.Join(target, "maintenance.json"))
					if err != nil {
						t.Fatal("cannot read target directory fence")
					}
				}
				if err := os.Remove(c.path("journal")); err != nil {
					t.Fatal("cannot remove empty fixture journal directory")
				}
				if err := os.Symlink(target, c.path("journal")); err != nil {
					t.Fatal("cannot prepare symlink parent fixture")
				}
				got, err := c.readMaintenanceFence()
				if present {
					if got != nil || !errors.Is(err, errMaintenanceFenceActive) {
						t.Fatal("present fence below symlink parent was not refused")
					}
					after, err := os.ReadFile(filepath.Join(target, "maintenance.json"))
					if err != nil || !bytes.Equal(before, after) {
						t.Fatal("symlink parent refusal changed target fence bytes")
					}
				} else {
					want := privateDir(c.path("journal"), false)
					if got != nil || err == nil || errors.Is(err, errMaintenanceFenceActive) || want == nil || err.Error() != want.Error() {
						t.Fatal("absent leaf below symlink parent lost parent validation error")
					}
					entries, err := os.ReadDir(target)
					if err != nil || len(entries) != 0 {
						t.Fatal("symlink parent refusal changed empty target directory")
					}
				}
				link, err := os.Readlink(c.path("journal"))
				if err != nil || link != target {
					t.Fatal("fence reader replaced symlink parent evidence")
				}
			})
		}
	}
}
