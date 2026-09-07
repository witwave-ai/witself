package dashboard

import (
	"os/exec"
	"testing"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestDashboardMessageBodyUI(t *testing.T) {
	node := testenv.RequireNode(t)
	cmd := exec.Command(node, "--test", "testdata/message_body_test.cjs")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard message body JavaScript test: %v\n%s", err, output)
	}
}
