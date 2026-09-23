package dashboard

import (
	"os/exec"
	"testing"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestDashboardReadabilityUI(t *testing.T) {
	node := testenv.RequireNode(t)
	cmd := exec.Command(node, "--test", "testdata/readability_test.cjs")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard readability JavaScript test: %v\n%s", err, output)
	}
}
