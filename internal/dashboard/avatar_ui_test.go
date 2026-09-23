package dashboard

import (
	"os/exec"
	"testing"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestDashboardAvatarUI(t *testing.T) {
	node := testenv.RequireNode(t)
	cmd := exec.Command(node, "--test", "testdata/avatar_test.cjs")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard avatar JavaScript test: %v\n%s", err, output)
	}
}
