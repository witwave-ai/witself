//go:build windows

package localcell_test

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

func shortLifecycleRoot(t *testing.T) string {
	t.Helper()
	// A flat random child avoids testing.TempDir's long test-name prefix. PW1
	// creates its protected DACL atomically rather than inheriting runner ACLs.
	for range 10 {
		root := filepath.Join(os.TempDir(), fmt.Sprintf("lc-%x", randomLifecycleSuffix(t)))
		if err := runtimeconfig.WindowsPrivateMkdir(root); os.IsExist(err) {
			continue
		} else if err != nil {
			t.Fatal("cannot allocate private disposable cell")
		}
		return root
	}
	t.Fatal("cannot allocate unique disposable cell")
	return ""
}

func randomLifecycleSuffix(t *testing.T) []byte {
	t.Helper()
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal("cannot allocate disposable cell identity")
	}
	return suffix
}

func lifecycleWriteFile(path string, data []byte) error {
	file, err := runtimeconfig.WindowsPrivateOpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func TestWindowsLifecycleFixtureDispatchAndStop(t *testing.T) {
	binaries := buildLifecycleFixture(t)
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	// LifecycleOrder checks postgres.exe version dispatch, initdb/createdb/psql
	// dispatch, ordered component readiness, CTRL_BREAK handling, and restart.
	testLifecycleOrderAndRemoval(t, binaries)
}
