package listenerdiagnostic

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
)

func TestDiagnostic(t *testing.T) {
	wrapped := &net.OpError{Op: "listen", Net: "tcp", Err: &os.SyscallError{Syscall: "bind", Err: ErrAddressInUse}}
	for _, component := range []string{"server", "worker"} {
		for _, test := range []struct {
			name     string
			err      error
			category string
		}{
			{"native", ErrAddressInUse, "address in use"},
			{"wrapped", fmt.Errorf("private address: %w", wrapped), "address in use"},
			{"other", errors.New("private listen failure"), "other"},
			{"lookalike", errors.New("address already in use"), "other"},
		} {
			t.Run(component+"/"+test.name, func(t *testing.T) {
				var output bytes.Buffer
				Write(&output, component, test.err)
				want := "witself-" + component + ": listener bind failed: " + test.category + "\n"
				if output.String() != want {
					t.Fatal("listener diagnostic must contain only the exact fixed line")
				}
			})
		}
	}
	for _, test := range []struct {
		component string
		err       error
	}{{"server", nil}, {"unknown\nprivate", ErrAddressInUse}} {
		var output bytes.Buffer
		Write(&output, test.component, test.err)
		if output.Len() != 0 {
			t.Fatal("nil errors and unknown components must not emit a diagnostic")
		}
	}
}

func TestNativeListenAddressInUse(t *testing.T) {
	held, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal("cannot create held loopback listener")
	}
	defer func() { _ = held.Close() }()
	second, err := net.Listen("tcp4", held.Addr().String())
	if second != nil {
		_ = second.Close()
		t.Fatal("second listener unexpectedly acquired occupied address")
	}
	if !IsAddressInUse(err) {
		t.Fatal("native occupied listener error was not classified as address in use")
	}
}
