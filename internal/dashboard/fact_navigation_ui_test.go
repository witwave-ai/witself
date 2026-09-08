package dashboard

import (
	"os/exec"
	"testing"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestDashboardFactNavigationUI(t *testing.T) {
	node := testenv.RequireNode(t)
	cmd := exec.Command(node, "--test", "testdata/fact_navigation_test.cjs")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard fact navigation JavaScript test: %v\n%s", err, output)
	}
}
