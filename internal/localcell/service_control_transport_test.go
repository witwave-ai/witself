package localcell

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type serviceControlSyntheticTimeout struct{}

func (serviceControlSyntheticTimeout) Error() string   { return "private timeout detail" }
func (serviceControlSyntheticTimeout) Timeout() bool   { return true }
func (serviceControlSyntheticTimeout) Temporary() bool { return false }

// NativeServiceControlChildRefusal is exported only in the test build so the
// external child-diagnostic contract exercises Control's transport classifier.
func NativeServiceControlChildRefusal() error {
	return serviceControlTransportError(&url.Error{Op: "Post", URL: "http://localcell/stop", Err: &serviceControlRefusal{step: "pipe:io"}})
}

func TestServiceControlTransportError(t *testing.T) {
	pipeIO := &serviceControlRefusal{step: "pipe:io"}
	peer := &serviceControlRefusal{step: "peer:sid"}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal("control transport privacy fixture could not generate a token")
	}
	privateValues := []string{
		filepath.Join(t.TempDir(), "private-control"),
		base64.RawURLEncoding.EncodeToString(random[:]),
		`\\.\pipe\witself-cell-private-composite`,
	}
	composite := errors.New(strings.Join(privateValues, " "))
	wrap := func(err error) error {
		return &url.Error{Op: "Post", URL: "http://private.invalid/stop", Err: fmt.Errorf("private marker: %w", err)}
	}
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"dial", &serviceControlDialError{err: errors.New("private dial detail")}, "localcell: service_control_unavailable: transport:dial"},
		{"dial_eof", &serviceControlDialError{err: io.EOF}, "localcell: service_control_unavailable: transport:dial"},
		{"deadline", context.DeadlineExceeded, "localcell: service_control_unavailable: transport:deadline"},
		{"canceled", context.Canceled, "localcell: service_control_unavailable: transport:deadline"},
		{"net_timeout", &net.OpError{Op: "read", Net: "private", Err: os.ErrDeadlineExceeded}, "localcell: service_control_unavailable: transport:deadline"},
		{"client_timeout", serviceControlSyntheticTimeout{}, "localcell: service_control_unavailable: transport:deadline"},
		{"joined_timeout", errors.Join(errors.New("private wrapper"), serviceControlSyntheticTimeout{}), "localcell: service_control_unavailable: transport:deadline"},
		{"dial_timeout", &serviceControlDialError{err: context.DeadlineExceeded}, "localcell: service_control_unavailable: transport:deadline"},
		{"eof", io.EOF, "localcell: service_control_unavailable: transport:eof"},
		{"unexpected_eof", io.ErrUnexpectedEOF, "localcell: service_control_unavailable: transport:eof"},
		{"response", errors.New("private protocol detail"), "localcell: service_control_unavailable: transport:response"},
		{"io", pipeIO, "localcell: service_control_refused: pipe:io"},
		{"dial_refusal", &serviceControlDialError{err: peer}, "localcell: service_control_refused: peer:sid"},
		{"refusal_before_timeout", errors.Join(context.DeadlineExceeded, pipeIO), "localcell: service_control_refused: pipe:io"},
		{"composite_response", composite, "localcell: service_control_unavailable: transport:response"},
		{"composite_dial", &serviceControlDialError{err: composite}, "localcell: service_control_unavailable: transport:dial"},
		{"composite_deadline", errors.Join(composite, context.DeadlineExceeded), "localcell: service_control_unavailable: transport:deadline"},
		{"composite_eof", errors.Join(composite, io.EOF), "localcell: service_control_unavailable: transport:eof"},
		{"composite_refusal", errors.Join(composite, pipeIO), "localcell: service_control_refused: pipe:io"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := serviceControlTransportError(wrap(test.err))
			if got == nil {
				t.Fatal("control transport failure lost its fixed classification")
			}
			line := got.Error()
			for _, private := range privateValues {
				if strings.Contains(line, private) {
					t.Error("control transport failure exposed a private value")
				}
			}
			if line != test.want {
				t.Fatal("control transport failure lost its fixed classification")
			}
			var refusal *serviceControlRefusal
			if errors.As(test.err, &refusal) && got != refusal {
				t.Fatal("control transport failure replaced the original refusal")
			}
		})
	}
}
