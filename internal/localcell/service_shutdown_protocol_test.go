package localcell

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

func TestServiceShutdownProtocolACKBeforeCancellation(t *testing.T) {
	d := shutdownDescriptorFixture(t)
	calls := 0
	p := newShutdownProtocol(d, func() { calls++ })
	if !errors.Is(p.RuntimeReady(), errLocalShutdownRefused) {
		t.Fatal("runtime readiness bypassed the release barrier")
	}
	requestID, err := shutdownRandom(16)
	if err != nil {
		t.Fatal("request fixture unavailable")
	}
	release := d.request("release", requestID)
	if err := p.handle(release, func(response serviceShutdownResponse) error {
		if !response.Accepted || response.Phase != shutdownPhaseStarting {
			t.Fatal("release did not advance the startup phase")
		}
		return nil
	}); err != nil || p.WaitRelease(t.Context()) != nil || p.RuntimeReady() != nil {
		t.Fatal("valid release did not admit runtime startup")
	}
	requestID, err = shutdownRandom(16)
	if err != nil {
		t.Fatal("request fixture unavailable")
	}
	request := d.request("cancel", requestID)
	var first serviceShutdownResponse
	err = p.handle(request, func(response serviceShutdownResponse) error {
		first = response
		if calls != 0 || !response.Accepted || response.Phase != shutdownPhaseReady {
			t.Fatal("cancellation ran before the accepted ACK write")
		}
		return io.ErrClosedPipe
	})
	if !errors.Is(err, io.ErrClosedPipe) || calls != 1 {
		t.Fatal("failed ACK write suppressed cancellation")
	}
	var wire bytes.Buffer
	if err := p.handle(request, func(response serviceShutdownResponse) error {
		return json.NewEncoder(&wire).Encode(response)
	}); err != nil {
		t.Fatal("identical retry failed")
	}
	var repeated serviceShutdownResponse
	if shutdownReadFrame(&wire, &repeated) != nil || repeated != first || calls != 1 {
		t.Fatal("retry changed its ACK or repeated cancellation")
	}
	if !errors.Is(p.RuntimeReady(), errLocalShutdownRefused) {
		t.Fatal("cancelled protocol reentered runtime readiness")
	}
	request.Action = "release"
	if err := p.handle(request, func(response serviceShutdownResponse) error {
		if response.Accepted {
			t.Fatal("request identifier reuse bypassed action binding")
		}
		return nil
	}); err != nil {
		t.Fatal("refusal response failed")
	}
}
