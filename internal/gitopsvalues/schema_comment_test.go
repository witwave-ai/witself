package gitopsvalues

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Source migrations establish that the historical hold is obsolete. They do
// not establish the schema currently running in a deployed cell, so generated
// values must not make a numbered claim about the live schema.
func TestCompatibilityCommentMatchesMigrationSet(t *testing.T) {
	root := repoRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, "internal/store/migrations"))
	if err != nil {
		t.Fatal(err)
	}
	latest := 0
	for _, entry := range entries {
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		if entry.IsDir() || !ok || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			t.Fatal(err)
		}
		latest = max(latest, version)
	}
	if latest <= 28 {
		t.Fatalf("migration set ends at %d; cannot claim the schema-28 hold is historical", latest)
	}
	generated, err := generateAll(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, cell := range []string{"civo-sandbox-use1-backup", "civo-sandbox-use1-serving"} {
		body := string(generated[cell])
		if !strings.Contains(body, "# Schema-28's compatibility hold expired long ago;") {
			t.Errorf("%s: missing historical compatibility explanation", cell)
		}
		if regexp.MustCompile(`cells run schema\s+\d+`).MatchString(body) {
			t.Errorf("%s: source migrations cannot establish the live cell schema", cell)
		}
	}
}
