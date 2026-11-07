package worker

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"testing"
)

// The child exercises Registry.Run before any job or HTTP server is launched.
// Its error takes the same nonzero exit path used by the worker binary.
func TestWorkerListenerBindFailure(t *testing.T) {
	if len(os.Args) >= 4 && os.Args[len(os.Args)-3] == "listener-bind-fixture" {
		cfg := Config{HealthAddr: "127.0.0.1:0", MetricsAddr: "127.0.0.1:0"}
		switch os.Args[len(os.Args)-2] {
		case "health":
			cfg.HealthAddr = os.Args[len(os.Args)-1]
		case "metrics":
			cfg.MetricsAddr = os.Args[len(os.Args)-1]
		default:
			os.Exit(3)
		}
		if NewRegistry().Run(context.Background(), cfg) != nil {
			os.Exit(1)
		}
		os.Exit(2)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("cannot locate test executable")
	}
	for _, listener := range []string{"health", "metrics"} {
		for _, category := range []string{"address in use", "other"} {
			t.Run(listener+"/"+category, func(t *testing.T) {
				address := "invalid-listen-address"
				if category == "address in use" {
					held, err := net.Listen("tcp4", "127.0.0.1:0")
					if err != nil {
						t.Fatal("cannot hold loopback listener")
					}
					defer func() { _ = held.Close() }()
					address = held.Addr().String()
				}
				cmd := exec.Command(executable, "-test.run=^TestWorkerListenerBindFailure$", "listener-bind-fixture", listener, address)
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				err := cmd.Run()
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 {
					t.Fatal("listener failure must take the nonzero exit path")
				}
				if stderr.String() != "witself-worker: listener bind failed: "+category+"\n" {
					t.Fatal("listener failure stderr must be exactly one fixed diagnostic line")
				}
			})
		}
	}
}
