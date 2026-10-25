package runtimeconfig

import "os"

// ValidateSpoolDir refuses the process-wide temp directory, including aliases.
// It does not create, repair, or change ownership of any directory.
func ValidateSpoolDir(dir string) error {
	_, err := validatedSpoolDirectory(dir)
	return err
}

func validatedSpoolDirectory(dir string) (os.FileInfo, error) {
	if dir == "" || !privateSpoolPath(dir) {
		return nil, errSpool
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, errSpool
	}
	tempPath, err := privateSpoolTempPath()
	if err != nil {
		return nil, errSpool
	}
	tempInfo, err := os.Stat(tempPath)
	if err != nil {
		return nil, errSpool
	}
	if os.SameFile(info, tempInfo) {
		return nil, errSpoolTemporary
	}
	resolved, err := privateSpoolResolvedPath(dir)
	if err != nil || !privateSpoolOwner(resolved, info) {
		return nil, errSpool
	}
	return info, nil
}

// OpenSpoolRoot pins the validated directory for the lifetime of the process.
// Check the opened handle as well, so a replacement during validation cannot
// substitute a different directory before the handle is acquired.
func OpenSpoolRoot(dir string) (*os.Root, error) {
	return openSpoolRoot(dir, validatedSpoolDirectory)
}

// The validator is supplied explicitly so tests can deterministically replace
// the path between validation and opening without a process-global race hook.
func openSpoolRoot(dir string, validate func(string) (os.FileInfo, error)) (*os.Root, error) {
	before, err := validate(dir)
	if err != nil {
		return nil, privateSpoolFailure(err)
	}
	root, err := openPrivateSpoolRoot(dir)
	if err != nil {
		return nil, privateSpoolFailure(err)
	}
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		_ = root.Close()
		return nil, errSpoolIdentity
	}
	if !privateSpoolRoot(root) {
		_ = root.Close()
		return nil, errSpool
	}
	return root, nil
}
