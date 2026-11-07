//go:build windows

package capturemix

import (
	"errors"
	"os"
)

func pauseChild(*os.Process) error  { return errors.New("capturemix process pause unavailable") }
func resumeChild(*os.Process) error { return errors.New("capturemix process pause unavailable") }
