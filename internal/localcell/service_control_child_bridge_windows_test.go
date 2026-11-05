//go:build windows

package localcell

import (
	"testing"
	"time"
)

// Export only a test bridge: the child is in localcell_test so it can invoke the
// exact native proof environment helper while the host uses synthetic hooks.
const NativeServiceChildSyntheticBudget = windowsSyntheticTestBudget / 15

func NativeServiceChildFixture(t *testing.T) (*Cell, string, SupervisorPolicy) {
	t.Helper()
	cell, _ := unitCell(t)
	prepareSupervisorTest(t, cell)
	hooks, _, _, _, _ := supervisorTestHooks()
	hooks.ticks = make(chan time.Time)
	return cell, cell.cfg.Root, SupervisorPolicy{hooks: &hooks, ShutdownTimeout: time.Second}
}
