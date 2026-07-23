//go:build !windows

package transcriptcapture

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCapturePlatformRejectsGroupWritableUnixFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o620); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o620); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if trustedPathIdentity(path, info) {
		t.Fatal("group-writable file was trusted")
	}
}
