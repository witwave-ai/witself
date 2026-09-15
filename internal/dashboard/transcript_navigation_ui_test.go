package dashboard

import (
	"os/exec"
	"testing"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestDashboardTranscriptNavigationUI(t *testing.T) {
	node := testenv.RequireNode(t)
	cmd := exec.Command(node, "--test", "testdata/transcript_navigation_test.cjs")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard transcript navigation JavaScript test: %v\n%s", err, output)
	}
}
