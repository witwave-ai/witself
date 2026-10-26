//go:build !windows

package localcell

import "os/exec"

func configurePostgresCommand(_ *exec.Cmd) (func(), error) { return func() {}, nil }

func configurePostgresVersionCommand(_ *exec.Cmd, _ string) (func(), error) {
	return func() {}, nil
}
