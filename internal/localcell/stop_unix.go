//go:build !windows

package localcell

import (
	"context"
	"errors"
	"strconv"
)

func (*Cell) terminateChild(_ context.Context, _ string, state ProcessState) error {
	if err := signalTerminate(state.PID); err != nil {
		return errors.New("cannot terminate local cell component")
	}
	return nil
}

func (*Cell) terminatePendingPostgres(context.Context, ProcessState) (bool, error) {
	return false, nil
}

func (c *Cell) stopPostgres(ctx context.Context, _ ProcessState) error {
	return c.pgCommand(ctx, "pg_ctl", "stop", "-D", c.path("pg"), "-m", "fast", "-w", "-t", strconv.Itoa(max(1, int(c.cfg.Timeout.Seconds()))))
}
