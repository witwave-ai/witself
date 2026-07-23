package main

import (
	"os"
	"testing"
)

func TestIntegrationFileModeMatchesPlatform(t *testing.T) {
	if !integrationFileModeMatchesPlatform("windows", os.FileMode(0o666), 0o600) {
		t.Fatal("native Windows mode projection must not be compared as POSIX permissions")
	}
	if integrationFileModeMatchesPlatform("linux", os.FileMode(0o666), 0o600) {
		t.Fatal("Linux permission drift must be rejected")
	}
	if !integrationFileModeMatchesPlatform("darwin", os.FileMode(0o600), 0o600) {
		t.Fatal("matching macOS permissions must be accepted")
	}
}
