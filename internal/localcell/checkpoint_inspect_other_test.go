//go:build !windows

package localcell

import (
	"os"
	"testing"
)

func checkpointInspectPoison(t *testing.T, name string) {
	t.Helper()
	if os.WriteFile(name, nil, 0o600) != nil || os.Chmod(name, 0o644) != nil {
		t.Fatal("cannot plant a validator-refused entry")
	}
}
