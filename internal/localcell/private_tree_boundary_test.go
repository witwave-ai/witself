package localcell

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// privateTreeVisit is one validator call made by a recorded whole-root walk.
type privateTreeVisit struct {
	path      string // slash-separated and relative to the walked root
	directory bool
	rule      string // "strict" or "postgres"
}

// recordPrivateTreeWalk drives the real boundary decision through the snapshot
// seam with the real filepath.WalkDir, wired exactly like the Windows walk but
// with recording validators. refuse, when set, supplies each validator result.
func recordPrivateTreeWalk(t *testing.T, root string, refuse func(path string) error) ([]privateTreeVisit, error) {
	t.Helper()
	var visits []privateTreeVisit
	record := func(path string, directory bool, rule string) error {
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			t.Fatal("private tree walk reported a path outside its root")
		}
		visits = append(visits, privateTreeVisit{filepath.ToSlash(relative), directory, rule})
		if refuse == nil {
			return nil
		}
		return refuse(path)
	}
	err := validatePrivateTreeSnapshot(root, filepath.WalkDir, func(path string, directory bool) error {
		return privateTreeEntry(root, path, directory, func() error {
			return record(path, directory, "strict")
		}, func(pg string) error {
			if pg != filepath.Join(root, "pg") {
				t.Fatal("PGDATA rule received another boundary")
			}
			return record(path, directory, "postgres")
		})
	}, os.Lstat)
	return visits, err
}

// privateTreeFixture creates slash-separated entries below root: a trailing
// slash makes a directory, anything else an empty file. Privacy is irrelevant
// because the recording validators accept every object they are asked about.
func privateTreeFixture(t *testing.T, root string, entries ...string) {
	t.Helper()
	for _, entry := range entries {
		path := filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(entry, "/")))
		if strings.HasSuffix(entry, "/") {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal("cannot create private tree fixture directory")
			}
			continue
		}
		if os.MkdirAll(filepath.Dir(path), 0o700) != nil || os.WriteFile(path, nil, 0o600) != nil {
			t.Fatal("cannot create private tree fixture file")
		}
	}
}

// privateTreeExpected lists every fixture path and its ancestors with the rule
// the walk must apply, minus everything strictly below <root>/recovery.
func privateTreeExpected(entries ...string) []privateTreeVisit {
	seen := map[string]bool{".": true}
	expected := []privateTreeVisit{{".", true, "strict"}}
	for _, entry := range entries {
		elements := strings.Split(strings.TrimSuffix(entry, "/"), "/")
		for i := range elements {
			path := strings.Join(elements[:i+1], "/")
			if seen[path] || strings.HasPrefix(path, "recovery/") {
				continue
			}
			seen[path] = true
			rule := "strict"
			if elements[0] == "pg" {
				rule = "postgres"
			}
			expected = append(expected, privateTreeVisit{path, i < len(elements)-1 || strings.HasSuffix(entry, "/"), rule})
		}
	}
	return expected
}

func sortedPrivateTreeVisits(visits []privateTreeVisit) []privateTreeVisit {
	sorted := append([]privateTreeVisit(nil), visits...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].path < sorted[j].path })
	return sorted
}

func TestPrivateTreeRecoveryBoundary(t *testing.T) {
	siblings := []privateTreeVisit{
		{"run", true, "strict"}, {"run/y", false, "strict"},
		{"spool", true, "strict"}, {"spool/z", false, "strict"},
		{"tmp", true, "strict"}, {"tmp/w", false, "strict"},
	}
	before := []privateTreeVisit{{".", true, "strict"}, {"pg", true, "postgres"}, {"pg/x", false, "postgres"}}

	t.Run("a_populated_boundary_is_validated_once_and_not_descended", func(t *testing.T) {
		root := t.TempDir()
		privateTreeFixture(t, root, "recovery/a/b/f", "pg/x", "run/y", "spool/z", "tmp/w")
		visits, err := recordPrivateTreeWalk(t, root, nil)
		want := append(append(append([]privateTreeVisit(nil), before...), privateTreeVisit{"recovery", true, "strict"}), siblings...)
		if err != nil || !reflect.DeepEqual(visits, want) {
			t.Fatalf("boundary walk visited %v (err %v), want %v", visits, err, want)
		}
	})

	t.Run("b_boundary_refusal_is_returned_unchanged", func(t *testing.T) {
		root := t.TempDir()
		privateTreeFixture(t, root, "recovery/a/b/f", "pg/x", "run/y", "spool/z", "tmp/w")
		refused := errors.New("synthetic boundary refusal")
		visits, err := recordPrivateTreeWalk(t, root, func(path string) error {
			if path == filepath.Join(root, "recovery") {
				return refused
			}
			return nil
		})
		want := append(append([]privateTreeVisit(nil), before...), privateTreeVisit{"recovery", true, "strict"})
		if err != refused || !reflect.DeepEqual(visits, want) {
			t.Fatalf("refused boundary walk visited %v and returned %v, want %v and the refusal itself", visits, err, want)
		}
	})

	t.Run("c_file_boundary_is_validated_as_a_file_and_siblings_after_it_still_walked", func(t *testing.T) {
		root := t.TempDir()
		privateTreeFixture(t, root, "recovery", "pg/x", "run/y", "spool/z", "tmp/w")
		visits, err := recordPrivateTreeWalk(t, root, nil)
		want := append(append(append([]privateTreeVisit(nil), before...), privateTreeVisit{"recovery", false, "strict"}), siblings...)
		if err != nil || !reflect.DeepEqual(visits, want) {
			t.Fatalf("file boundary walk visited %v (err %v), want %v", visits, err, want)
		}
	})

	t.Run("c_link_boundary_is_validated_as_a_non_directory_and_siblings_after_it_still_walked", func(t *testing.T) {
		root := t.TempDir()
		privateTreeFixture(t, root, "pg/x", "run/y", "spool/z", "tmp/w")
		// The link points at a sibling directory, so following it would
		// validate spool/z twice; WalkDir must report it as a non-directory.
		// Without the Windows symbolic link privilege the link is a junction.
		recoveryBoundaryLink(t, filepath.Join(root, "recovery"), filepath.Join(root, "spool"))
		visits, err := recordPrivateTreeWalk(t, root, nil)
		want := append(append(append([]privateTreeVisit(nil), before...), privateTreeVisit{"recovery", false, "strict"}), siblings...)
		if err != nil || !reflect.DeepEqual(visits, want) {
			t.Fatalf("link boundary walk visited %v (err %v), want %v", visits, err, want)
		}
	})

	t.Run("d_look_alikes_are_walked_in_full", func(t *testing.T) {
		root := t.TempDir()
		entries := []string{
			"recovery/inside", "recovery2/deep/f", "recovery.old/deep/f",
			"spool/recovery/deep/f", "pg/recovery/deep/f", "run/y",
		}
		privateTreeFixture(t, root, entries...)
		// Direct-child case aliases now have stronger refusal coverage in
		// TestPrivateTreeRecoveryAlias; these other look-alikes still walk fully.
		visits, err := recordPrivateTreeWalk(t, root, nil)
		got, want := sortedPrivateTreeVisits(visits), sortedPrivateTreeVisits(privateTreeExpected(entries...))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("look-alike walk visited %v (err %v), want %v", got, err, want)
		}
	})

	t.Run("e_only_the_exact_child_of_the_root_is_the_boundary", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "cell")
		boundary := filepath.Join(root, "recovery")
		named := filepath.Join(base, "recovery") // a root whose own base name is recovery
		separator := string(filepath.Separator)
		// No normalisation: a forward-slash spelling is the boundary only where
		// it is already the walk's own spelling.
		slashed := privateTreeStrict
		if root+"/recovery" == boundary {
			slashed = privateTreeBoundary
		}
		for _, test := range []struct {
			root, path string
			want       privateTreeClass
		}{
			{root, boundary, privateTreeBoundary},
			{named, filepath.Join(named, "recovery"), privateTreeBoundary},
			{root, root + "/recovery", slashed},
			{root, filepath.Join(root, "Recovery"), privateTreeAlias},
			{root, filepath.Join(root, "RECOVERY"), privateTreeAlias},
			{root, filepath.Join(root, "recoverY"), privateTreeAlias},
			{root, filepath.Join(root, "recoverx"), privateTreeStrict},
			{root, boundary + separator, privateTreeStrict},
			{root, boundary + separator + "anything", privateTreeStrict},
			{root, filepath.Join(boundary, "recovery"), privateTreeStrict},
			{root, boundary + "2", privateTreeStrict},
			{root, boundary + ".old", privateTreeStrict},
			{root, root, privateTreeStrict},
			{named, named, privateTreeStrict},
			{root, filepath.Join(root, "spool", "recovery"), privateTreeStrict},
			{root, filepath.Join(root, "spool", "Recovery"), privateTreeStrict},
			{root, filepath.Join(root, "pg"), privateTreePostgres},
			{root, filepath.Join(root, "pg", "recovery"), privateTreePostgres},
			{root, filepath.Join(root, "pg", "Recovery"), privateTreePostgres},
			{root, filepath.Join(root, "pgx"), privateTreeStrict},
		} {
			if got := classifyPrivateTreePath(test.root, test.path); got != test.want {
				t.Errorf("class of %q under %q = %d, want %d", test.path, test.root, got, test.want)
			}
		}
	})
}

func TestValidateRootRecoveryBoundary(t *testing.T) {
	const notRealDirectory = "local cell directory is missing or not a real directory"
	const notPrivate = "local cell directory must be private and owned by the current user"
	for _, test := range []struct {
		name    string
		build   func(t *testing.T, path string)
		refusal string // the existing fixed text off Windows; empty accepts
	}{
		{name: "absent", build: func(*testing.T, string) {}},
		{name: "private_directory_with_arbitrary_content", build: func(t *testing.T, path string) {
			if err := testMkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			recoveryBoundaryLink(t, filepath.Join(path, "link"), testPrivateRoot(t))
			world := filepath.Join(path, "world-readable")
			if err := testWriteFile(world, []byte("synthetic checkpoint bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			recoveryBoundaryNonPrivate(t, world, false)
			open := filepath.Join(path, "open")
			if err := testMkdir(open, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := testWriteFile(filepath.Join(open, "file"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			recoveryBoundaryNonPrivate(t, open, true)
		}},
		{name: "symlink", refusal: notRealDirectory, build: func(t *testing.T, path string) {
			recoveryBoundaryLink(t, path, testPrivateRoot(t))
		}},
		{name: "regular_file", refusal: notRealDirectory, build: func(t *testing.T, path string) {
			if err := testWriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "non_private_directory", refusal: notPrivate, build: func(t *testing.T, path string) {
			if err := testMkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			recoveryBoundaryNonPrivate(t, path, true)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, _ := unitCell(t)
			path := c.path("recovery")
			test.build(t, path)
			err := c.validateRoot(false)
			lock, ownErr := c.own(false)
			if lock != nil {
				if closeErr := lock.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
			}
			if test.refusal == "" {
				if err != nil || ownErr != nil || c.validateRoot(true) != nil {
					t.Fatalf("recovery boundary %s was refused: %v / %v", test.name, err, ownErr)
				}
				if test.name == "absent" {
					if layoutErr := c.layout(); layoutErr != nil {
						t.Fatal(layoutErr)
					}
					if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
						t.Fatal("validation, ownership or layout created the recovery boundary")
					}
				}
				return
			}
			if err == nil || ownErr == nil || errors.Is(ownErr, ErrLocked) {
				t.Fatalf("recovery boundary %s was accepted: %v / %v", test.name, err, ownErr)
			}
			for _, refusal := range []error{err, ownErr} {
				if strings.Contains(refusal.Error(), c.cfg.Root) {
					t.Fatal("recovery boundary refusal exposed a path")
				}
				// Windows refuses with its private-object reasons, often from the
				// walk; elsewhere the existing validateRoot text must be unchanged.
				if runtime.GOOS != "windows" && refusal.Error() != test.refusal {
					t.Fatalf("recovery boundary %s refusal = %q, want %q", test.name, refusal.Error(), test.refusal)
				}
			}
		})
	}
}
