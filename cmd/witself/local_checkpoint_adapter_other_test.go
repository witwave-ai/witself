//go:build !windows

package main

import "os"

// lcaPrivateDir and lcaPrivateFile create objects the local cell accepts as private.
func lcaPrivateDir(path string) error { return os.Mkdir(path, 0o700) }

func lcaPrivateFile(path string, data []byte) error { return os.WriteFile(path, data, 0o600) }
