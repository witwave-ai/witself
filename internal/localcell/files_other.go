//go:build !windows

package localcell

import "os"

// Preserve the existing Unix filesystem operations and permission checks.
func makePrivateDir(path string) error { return os.Mkdir(path, 0o700) }

func openPrivateFile(path string, flags int, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flags, mode)
}

func readPrivateFile(path string) ([]byte, error) { return os.ReadFile(path) }

func createPrivateTemp(dir string) (*os.File, error) { return os.CreateTemp(dir, ".pending-*") }

func privatePassword(_ string, info os.FileInfo) bool { return info.Mode().Perm() == 0o600 }

func validateRootPath(string) error    { return nil }
func validatePrivateTree(string) error { return nil }
func validateWriteTarget(string) error { return nil }

func publishPrivateFile(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }
func removePrivateFile(path string) error              { return os.Remove(path) }
