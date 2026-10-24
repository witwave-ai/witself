//go:build !darwin && !linux && !windows

package localcell

import (
	"errors"
	"os"
	"os/exec"
	"time"
)

func processIdentity(int) (string, bool, error) {
	return "", false, errors.New("localcell: process identity is unsupported on this platform")
}

func signalTerminate(int) error {
	return errors.New("localcell: process termination is unsupported on this platform")
}

func configureProcessCommand(*exec.Cmd) {}

func privateOwnedDir(string, os.FileInfo) bool { return false }

func processStartedAt(int) (time.Time, error) {
	return time.Time{}, errors.New("localcell: process creation time is unsupported on this platform")
}
