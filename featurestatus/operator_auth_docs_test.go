package featurestatus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The reconciled onboarding contract must not promise recovery of a credential
// that never reached the journal after the server consumed its bootstrap.
func TestOperatorAuthDocumentsBootstrapRecoveryBoundary(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "operator-auth.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, onboarding, ok := strings.Cut(string(raw), "### Managed account creation and adoption")
	if !ok {
		t.Fatal("operator auth documentation is missing managed onboarding")
	}
	onboarding, _, _ = strings.Cut(onboarding, "### Self-hosted bootstrap")
	text := strings.Join(strings.Fields(onboarding), " ")
	for _, required := range []string{
		"retry the same signup while the account remains pending and its bootstrap remains unconsumed",
		"resume local saving after the operator credential is durably journaled",
		"If the bootstrap exchange commits but its successful response is lost",
		"rerunning `account create` cannot recover the credential: provision replay refuses the consumed bootstrap",
		"For a pending account, complete activation through the verification email before requesting recovery",
		"witself account recover --id acc_ID",
		"witself account recover --id acc_ID --code CODE --name work",
		"runbooks.md#recover-a-lost-owner-token",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("managed onboarding must document %q", required)
		}
	}
}
