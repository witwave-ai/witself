//go:build !windows

package localcell

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

func (c *Cell) startPostgres(ctx context.Context, _ *Manifest) error {
	if err := c.verifyAll(); err != nil {
		return err
	}
	state, err := c.processState("postgres")
	if err != nil || state.Alive {
		return err
	}
	if err := c.pgCommand(ctx, "pg_ctl", "start", "-D", c.path("pg"), "-l", c.path("logs", "postgres.log"), "-w", "-t", strconv.Itoa(max(1, int(c.cfg.Timeout.Seconds())))); err != nil {
		return err
	}
	pid, err := c.postmaster()
	if err != nil || pid == 0 {
		return fixedCause("PostgreSQL did not publish process ownership")
	}
	identity, alive, err := processIdentity(pid)
	if err != nil || !alive {
		return errors.New("cannot verify started PostgreSQL identity")
	}
	return writeAtomic(c.path("run", "postgres.json"), ProcessRecord{PID: pid, StartTime: identity, Root: c.cfg.Root, Component: "postgres"}, nil)
}

func (c *Cell) pendingPostgresWithoutPID(_ ProcessState, _ int) bool { return false }

func postmasterLines(data []byte) []string { return strings.Split(string(data), "\n") }

func (*Cell) pendingPostmasterPID([]byte) (int, bool, error) { return 0, false, nil }
