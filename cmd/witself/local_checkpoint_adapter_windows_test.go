package main

import (
	"os"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

// lcaPrivateDir and lcaPrivateFile create objects with the protected DACL the
// local cell accepts as private; a runner's inherited ACLs are not private.
func lcaPrivateDir(path string) error { return runtimeconfig.WindowsPrivateMkdir(path) }

func lcaPrivateFile(path string, data []byte) error {
	file, err := runtimeconfig.WindowsPrivateOpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}
