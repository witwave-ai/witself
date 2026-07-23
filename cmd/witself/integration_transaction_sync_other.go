//go:build !windows

package main

import "os"

func openIntegrationTransactionFileForSync(path string) (*os.File, error) {
	return os.Open(path)
}

func syncIntegrationTransactionDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}
