//go:build linux || darwin

package localcell_test

import (
	"testing"

	"github.com/witwave-ai/witself/internal/localcell"
)

func TestLifecycleJournalBarrierInstall(t *testing.T) {
	localcell.CheckLifecycleInstallBarrierForTest(t, buildLifecycleFixture(t))
}
