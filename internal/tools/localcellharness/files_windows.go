//go:build windows

package main

import (
	"errors"
	"os"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

func harnessPrivateDirectory(path string) error {
	if err := runtimeconfig.WindowsPrivateMkdir(path); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return runtimeconfig.WindowsPrivateValidate(path, true)
}
func harnessCreateFile(path string) (*os.File, error) {
	return runtimeconfig.WindowsPrivateOpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
}
