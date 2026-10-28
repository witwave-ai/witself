//go:build !windows

package localcell

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceSystemdFilesystemCauseOmitsExternalPath(t *testing.T) {
	a, d, _ := systemdFixture(t)
	a.unitBase = filepath.Join(d.Root, "external-config")
	a.unitDir = filepath.Join(a.unitBase, "systemd", "user")
	a.inspectFile = func(path string) (os.FileInfo, error) {
		return nil, &os.PathError{Op: "lstat", Path: path, Err: os.ErrPermission}
	}
	for _, create := range []bool{false, true} {
		err := a.inspectDirectories(create)
		if !errors.Is(err, ErrServiceOwnershipConflict) || !strings.Contains(err.Error(), "unit_dir_walk:0:stat: permission denied") {
			t.Fatal("external directory refusal lost its sentinel or filesystem cause")
		}
		if strings.Contains(err.Error(), d.Root) || strings.Contains(err.Error(), "external-config") {
			t.Fatal("directory refusal exposed an external path before a descriptor was available")
		}
	}
}

func TestServiceSystemdCommandCauseOmitsForcedEnvironmentPaths(t *testing.T) {
	a, d, _ := systemdFixture(t)
	a.runtimeDir = filepath.Join(t.TempDir(), "runtime")
	config := filepath.Join(t.TempDir(), "config")
	a.unitDir = filepath.Join(config, "systemd", "user")
	stderr := "failed at " + a.runtimeDir + "/bus and " + config + "/systemd/user"
	err := a.commandError(ErrServiceManagerUnavailable, "bus:systemctl_show", &exec.ExitError{Stderr: []byte(stderr)}, d.Root)
	if !errors.Is(err, ErrServiceManagerUnavailable) || !strings.Contains(err.Error(), "failed at <path>/bus and <path>/systemd/user") {
		t.Fatal("command cause did not retain redacted session/config path evidence")
	}
	if strings.Contains(err.Error(), a.runtimeDir) || strings.Contains(err.Error(), config) {
		t.Fatal("command cause exposed forced environment paths outside home")
	}
}
