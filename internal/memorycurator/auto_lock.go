package memorycurator

import (
	"errors"
	"fmt"
	"os"
)

var errAutoLockFileHandle = errors.New("curator automation lock handle is unavailable")

func validateAutoLockFileIdentity(path string, file *os.File) (os.FileInfo, error) {
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	linked, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || opened.Mode()&os.ModeSymlink != 0 ||
		!linked.Mode().IsRegular() || linked.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("curator automation lock %s is not a regular file", path)
	}
	if !os.SameFile(opened, linked) {
		return nil, fmt.Errorf("curator automation lock %s changed while opening", path)
	}
	return opened, nil
}
