//go:build darwin || linux

package localcell

import (
	"errors"
	"os"
	"testing"
)

func TestServiceControlStopRemovesEndpointUnix(t *testing.T) {
	output := serviceControlJoinCapture(t)
	cell := shortServiceTestCell(t)
	stop, err := cell.serveServiceControl(make(chan serviceCommand))
	if err != nil {
		t.Fatal("cannot start control endpoint removal fixture")
	}
	if err := cell.validateServiceControlEndpoint(); err != nil {
		stop()
		t.Fatal("control endpoint was not present before shutdown")
	}
	stop()
	if _, err := os.Lstat(cell.path("service", "control.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("control stop left its endpoint behind")
	}
	if output() != "" {
		t.Fatal("normal control endpoint removal emitted diagnostic output")
	}
}
