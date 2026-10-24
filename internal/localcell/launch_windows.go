package localcell

import (
	"errors"
	"os/exec"
)

func childCommand(_, _, _ string) (*exec.Cmd, error) {
	// Go os/exec can append ambient SystemRoot even to a supplied environment;
	// Windows also needs suspended creation to publish identity before resume.
	// Do not weaken either contract while the native platform adapter is pending.
	return nil, errors.New("native Windows local cell launch requires the deferred platform adapter")
}
