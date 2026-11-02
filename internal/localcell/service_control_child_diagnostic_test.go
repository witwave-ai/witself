package localcell_test

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/localcell"
)

func serviceControlChildDiagnostic(err error) string {
	if err != nil {
		if line := err.Error(); serviceControlChildDiagnosticLine.MatchString(line) {
			return line
		}
	}
	return "localcell: service_control_child_error_unclassified"
}

func TestServiceControlChildDiagnostic(t *testing.T) {
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal("child diagnostic privacy fixture could not generate synthetic input")
	}
	privatePath := "/private/synthetic/control-child"
	privateToken := base64.RawURLEncoding.EncodeToString(random[:])
	privatePipe := `\\.\pipe\witself-cell-` + privateToken
	privateText := privatePath + " " + privateToken + " " + privatePipe
	const unclassified = "localcell: service_control_child_error_unclassified"
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"refusal", localcell.NativeServiceControlChildRefusal(), "localcell: service_control_refused: pipe:io"},
		{"dial", errors.New("localcell: service_control_unavailable: transport:dial"), "localcell: service_control_unavailable: transport:dial"},
		{"deadline", errors.New("localcell: service_control_unavailable: transport:deadline"), "localcell: service_control_unavailable: transport:deadline"},
		{"eof", errors.New("localcell: service_control_unavailable: transport:eof"), "localcell: service_control_unavailable: transport:eof"},
		{"response", errors.New("localcell: service_control_unavailable: transport:response"), "localcell: service_control_unavailable: transport:response"},
		{"private_composite", errors.New(privateText), unclassified},
		{"private_suffix", errors.New("localcell: service_control_unavailable: transport:eof " + privateText), unclassified},
		{"nil", nil, unclassified},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := serviceControlChildDiagnostic(test.err)
			for _, private := range []string{privatePath, privateToken, privatePipe} {
				if strings.Contains(got, private) {
					t.Error("child diagnostic output exposed a private input")
				}
			}
			if got != test.want {
				t.Fatal("child diagnostic output escaped its exact vocabulary mapping")
			}
		})
	}
}
