//go:build windows

package transcriptcapture

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func codexWindowsPowerShellExecutable() (string, error) {
	systemDirectory, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	if systemDirectory == "" {
		return "", errors.New("Windows system directory is empty")
	}
	executable := filepath.Join(systemDirectory, "WindowsPowerShell", "v1.0", "powershell.exe")
	info, err := os.Lstat(executable)
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", executable, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("trusted Windows PowerShell path is not a regular file: %s", executable)
	}
	return executable, nil
}
