package transcriptcapture

import (
	"os"
	"strings"
	"testing"
)

func TestReleaseManagedUploaderRequiresExactExecutable(t *testing.T) {
	for _, runtimeName := range []string{RuntimeCodex, RuntimeClaudeCode} {
		t.Run(runtimeName, func(t *testing.T) {
			opts := managedHooksTestOptions(t, runtimeName, ModeRaw)
			ownership, _, err := InstallManagedHooksOwned(opts, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifyManagedHookExecutable(runtimeName, ownership, opts.Executable); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(ownership.RunnerPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifyManagedHookExecutable(runtimeName, ownership, opts.Executable+"-new"); err == nil || !strings.Contains(err.Error(), "persisted uploader executable") {
				t.Fatalf("different registered executable accepted: %v", err)
			}
			after, err := os.ReadFile(ownership.RunnerPath)
			if err != nil || string(after) != string(before) {
				t.Fatalf("executable check changed managed runner: %v", err)
			}
		})
	}
}
