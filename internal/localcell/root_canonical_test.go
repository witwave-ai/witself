package localcell

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCanonicalRootIdempotent(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "cell")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := CanonicalRoot(root)
	if err != nil || first != root {
		t.Fatalf("canonical directory changed: root=%q err=%v", first, err)
	}
	second, err := CanonicalRoot(first)
	if err != nil || second != first {
		t.Fatalf("canonicalization is not idempotent: root=%q err=%v", second, err)
	}
}

func TestCanonicalRootRelative(t *testing.T) {
	parent := t.TempDir()
	t.Chdir(parent)
	got, err := CanonicalRoot("cell")
	if runtime.GOOS == "windows" {
		// Windows validates absolute path syntax before filepath.Abs; preserve
		// that existing New policy rather than relaxing its trust boundary.
		if err == nil || err.Error() != "private Windows filesystem object required" {
			t.Fatalf("relative Windows root did not retain its refusal: %v", err)
		}
		return
	}
	canonicalParent, parentErr := filepath.EvalSymlinks(parent)
	if parentErr != nil {
		t.Fatal(parentErr)
	}
	if err != nil || !filepath.IsAbs(got) || got != filepath.Join(canonicalParent, "cell") {
		t.Fatalf("relative root did not become canonical and absolute: root=%q err=%v", got, err)
	}
}

func TestCanonicalRootTrailingSeparator(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "cell")
	got, err := CanonicalRoot(root + string(os.PathSeparator))
	if err != nil || got != root {
		t.Fatalf("trailing separator was not cleaned: root=%q err=%v", got, err)
	}
}

func TestCanonicalRootInvalid(t *testing.T) {
	parent := t.TempDir()
	missingParentError := "local cell parent directory is unavailable"
	if runtime.GOOS == "windows" {
		// The Windows path guard requires every parent to exist and runs
		// before the parent EvalSymlinks check.
		missingParentError = "private Windows filesystem object required"
	}
	for _, test := range []struct {
		name string
		root string
		want string
	}{
		{"empty", "", "local cell root is required"},
		{"volume_root", filepath.VolumeName(parent) + string(os.PathSeparator), "invalid local cell root"},
		{"missing_parent", filepath.Join(parent, "missing", "cell"), missingParentError},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := CanonicalRoot(test.root)
			if err == nil || err.Error() != test.want || got != "" {
				t.Fatalf("invalid root result = (%q, %v), want empty root and %q", got, err, test.want)
			}
		})
	}
}

func TestNewRecordsCanonicalRoot(t *testing.T) {
	parent := t.TempDir()
	canonicalParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "cell") + string(os.PathSeparator)
	if runtime.GOOS != "windows" {
		t.Chdir(parent)
		root = "cell" + string(os.PathSeparator)
	}
	cell, err := New(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	// path is the existing behavior used to locate the installation's files.
	if got, want := cell.path("journal"), filepath.Join(canonicalParent, "cell", "journal"); got != want {
		t.Fatalf("New did not record the canonical root: path=%q want=%q", got, want)
	}
}
