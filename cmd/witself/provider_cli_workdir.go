package main

import (
	"fmt"
	"os"
	"os/exec"
)

// isolateProviderCLIWorkingDirectory prevents provider CLIs from discovering
// or creating project-local state in the directory from which Witself was
// invoked. Some provider CLIs create session or documentation artifacts even
// for read-only capability and version probes.
func isolateProviderCLIWorkingDirectory(cmd *exec.Cmd, provider string) (func(), error) {
	directory, err := os.MkdirTemp("", "witself-provider-cli-")
	if err != nil {
		return nil, fmt.Errorf("create isolated %s CLI working directory: %w", provider, err)
	}
	cmd.Dir = directory
	return func() { _ = os.RemoveAll(directory) }, nil
}
