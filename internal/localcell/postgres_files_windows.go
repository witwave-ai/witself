//go:build windows

package localcell

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

func (c *Cell) preparePostgresDirectory() error {
	return runtimeconfig.WindowsPrivatePostgresMkdir(c.path("pg"))
}

func (c *Cell) validatePostgresDirectory() error {
	return runtimeconfig.WindowsPrivatePostgresValidate(c.path("pg"), c.path("pg"), true)
}

func readPostgresFile(path string) ([]byte, error) {
	return runtimeconfig.WindowsPrivatePostgresRead(filepath.Dir(path), path)
}

func (c *Cell) writePostgresConfig(data []byte) error {
	root, path := c.path("pg"), c.path("pg", "postgresql.conf")
	if err := runtimeconfig.WindowsPrivatePostgresValidate(root, root, true); err != nil {
		return err
	}
	if err := runtimeconfig.WindowsPrivatePostgresValidate(root, path, false); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := runtimeconfig.WindowsPrivateCreateTemp(root)
	if err != nil {
		return errors.New("cannot create local cell PostgreSQL configuration")
	}
	defer func() { _ = runtimeconfig.WindowsPrivateRemove(file.Name()) }()
	if _, err = file.Write(data); err == nil {
		err = syncFile(file)
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return errors.New("cannot persist local cell PostgreSQL configuration")
	}
	return runtimeconfig.WindowsPrivatePostgresRename(root, file.Name(), path)
}
