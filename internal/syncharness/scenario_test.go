package syncharness

import (
	"errors"
	"testing"
)

type recordingAdapter struct{ deliveries []Delivery }

func (a *recordingAdapter) Deliver(delivery Delivery) error {
	a.deliveries = append(a.deliveries, delivery)
	return nil
}

func TestTransportDisconnectedAndMissingAdapter(t *testing.T) {
	nodes := []NodeID{L1, L2, R, D, W, C}
	transport := NewTransport(nodes...)
	if transport.HasAdapter() {
		t.Fatal("default transport installed an adapter")
	}
	for _, from := range nodes {
		for _, to := range nodes {
			if transport.EdgeEnabled(from, to) {
				t.Fatal("default edge enabled")
			}
		}
	}
	delivery := Delivery{From: L1, To: R, Operation: "sentinel"}
	if err := transport.Deliver(delivery); !errors.Is(err, ErrCapabilityMissing) {
		t.Fatal("missing adapter became domain refusal/success")
	}
	adapter := &recordingAdapter{}
	transport.InstallAdapter(adapter)
	if err := transport.Deliver(delivery); !errors.Is(err, ErrPartitioned) {
		t.Fatal("disabled edge delivered")
	}
	if len(adapter.deliveries) != 0 {
		t.Fatal("disabled delivery reached adapter")
	}
	if err := transport.SetEdge(L1, R, true); err != nil {
		t.Fatal(err)
	}
	if err := transport.Deliver(delivery); err != nil {
		t.Fatal(err)
	}
	if len(adapter.deliveries) != 1 || transport.EdgeEnabled(R, L1) {
		t.Fatal("directed edge did not remain directed")
	}
	if err := transport.SetEdge(L1, L1, true); !errors.Is(err, ErrInvalidEdge) {
		t.Fatal("self edge accepted")
	}
}

func TestMissingCapabilityCannotPassM4(t *testing.T) {
	result := RequireCapabilities("M4", []string{"capture", "delivery", "delivery"}, []string{"capture"})
	if result.Status != ScaffoldOnly || result.Passed() || len(result.Missing) != 1 || result.Missing[0] != "delivery" {
		t.Fatal("missing adapter falsely passed")
	}
	result = RequireCapabilities("M4", []string{"capture"}, []string{"capture"})
	if result.Status != NotRun || result.Passed() {
		t.Fatal("capability presence manufactured proof")
	}
}
