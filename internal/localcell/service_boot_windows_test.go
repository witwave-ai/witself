//go:build windows

package localcell

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// NativeServiceBootDiagnostic exposes only the query's value-free status to the
// external-package native receipt, including successful prerequisite queries.
func NativeServiceBootDiagnostic() string { return windowsBootEnvironmentDiagnostic() }

func TestWindowsBootEnvironmentLayout(t *testing.T) {
	ctx := windowsSyntheticTestContext(t)
	// phnt ntexapi.h, SYSTEM_BOOT_ENVIRONMENT_INFORMATION (public, non-SDK),
	// revision 53fbbdc5b5d2, supplies the class-90 layout premise:
	// https://github.com/winsiderss/phnt/blob/53fbbdc5b5d2/ntexapi.h
	// The first Windows CI service_boot_identity line is the executable oracle;
	// this static assertion cannot replace the query/agreement witness.
	var v windowsBootEnvironmentInformation
	if windows.SystemBootEnvironmentInformation != 90 || unsafe.Sizeof(v) != windowsBootEnvironmentSize || unsafe.Offsetof(v.BootIdentifier) != 0 || unsafe.Sizeof(v.BootIdentifier) != 16 || unsafe.Offsetof(v.FirmwareType) != 16 || unsafe.Sizeof(v.FirmwareType) != 4 || unsafe.Offsetof(v.BootFlags) != 24 || unsafe.Sizeof(v.BootFlags) != 8 {
		t.Fatal("boot environment query layout changed")
	}
	if ctx.Err() != nil {
		t.Fatal("boot layout test exceeded synthetic budget")
	}
}

func TestWindowsBootIdentityQueryValidation(t *testing.T) {
	ctx := windowsSyntheticTestContext(t)
	original := windowsQueryBootEnvironment
	t.Cleanup(func() { windowsQueryBootEnvironment = original })
	valid := make([]byte, windowsBootEnvironmentSize)
	copy(valid, []byte{0x78, 0x56, 0x34, 0x12, 0xbc, 0x9a, 0xf0, 0xde, 0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0})
	for _, tc := range []struct {
		name   string
		status uint32
		length uint32
		data   []byte
		valid  bool
	}{
		{"complete", 0, 32, valid, true},
		{"guid_only", 0, 16, valid, true},
		{"failed_status", 0xc0000001, 32, valid, false},
		{"informational_status", 1, 32, valid, false},
		{"zero_length", 0, 0, valid, false},
		{"short_length", 0, 15, valid, false},
		{"large_length", 0, 33, valid, false},
		{"short_buffer", 0, 32, valid[:16], false},
		{"absent_buffer", 0, 16, nil, false},
		{"large_buffer", 0, 32, append(bytes.Clone(valid), 0), false},
		{"zero_guid", 0, 32, make([]byte, 32), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if ctx.Err() != nil {
				t.Fatal("boot query validation exceeded synthetic budget")
			}
			windowsQueryBootEnvironment = func() (uint32, uint32, []byte) { return tc.status, tc.length, tc.data }
			got, err := serviceBootID()
			if tc.valid {
				if err != nil || got != "windows:12345678-9abc-def0-1234-56789abcdef0" {
					t.Fatal("validated GUID encoding changed")
				}
			} else if err == nil || err.Error() != "service_boot_identity_unavailable" || got != "" {
				t.Fatal("invalid query exposed a partial boot identity")
			}
			diagnostic := windowsBootEnvironmentDiagnostic()
			if !strings.HasPrefix(diagnostic, "service_boot_identity ntstatus=0x") || !strings.Contains(diagnostic, " length=") || !strings.Contains(diagnostic, " guid_nonzero=") || strings.Contains(diagnostic, "12345678-9abc") {
				t.Fatal("boot diagnostic omitted query state or exposed GUID")
			}
		})
	}
}

func TestWindowsServiceControlCapabilityBootIdentity(t *testing.T) {
	ctx := windowsSyntheticTestContext(t)
	original := windowsQueryBootEnvironment
	t.Cleanup(func() { windowsQueryBootEnvironment = original })
	valid := make([]byte, windowsBootEnvironmentSize)
	valid[0] = 1
	for _, tc := range []struct {
		name   string
		status uint32
		length uint32
		data   []byte
		valid  bool
	}{
		{"valid", 0, windowsBootEnvironmentSize, valid, true},
		{"query_failed", 0xc0000001, windowsBootEnvironmentSize, valid, false},
		{"guid_unavailable", 0, windowsBootEnvironmentSize, make([]byte, windowsBootEnvironmentSize), false},
		{"invalid_length", 0, 15, valid, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if ctx.Err() != nil {
				t.Fatal("boot capability test exceeded synthetic budget")
			}
			windowsQueryBootEnvironment = func() (uint32, uint32, []byte) { return tc.status, tc.length, tc.data }
			err := serviceControlCapability()
			if tc.valid {
				if err != nil {
					t.Fatal("valid boot query refused service control capability")
				}
				return
			}
			if !errors.Is(err, ErrServiceManagerUnavailable) || err.Error() != ErrServiceManagerUnavailable.Error()+": service_boot_identity_unavailable" {
				t.Fatal("failed boot query did not refuse capability with exact fixed detail")
			}
		})
	}
}

func TestWindowsBootIdentityChild(t *testing.T) {
	ctx := windowsSyntheticTestContext(t)
	if len(os.Args) < 2 || os.Args[len(os.Args)-1] != "localcell-boot-identity-child" {
		return
	}
	type fixtureInput struct {
		value []byte
		err   error
	}
	input := make(chan fixtureInput, 1)
	go func() {
		value, err := io.ReadAll(io.LimitReader(os.Stdin, 128))
		input <- fixtureInput{value, err}
	}()
	var expected []byte
	select {
	case result := <-input:
		if result.err != nil {
			t.Fatal("boot fixture input unavailable")
		}
		expected = result.value
	case <-ctx.Done():
		_ = os.Stdin.Close()
		t.Fatal("boot fixture input exceeded synthetic budget")
	}
	first, err := serviceBootID()
	if err != nil || first != string(expected) {
		t.Fatal("child boot identity differs")
	}
	second, err := serviceBootID()
	if err != nil || second != first {
		t.Fatal("child repeated boot query differs")
	}
}

func TestWindowsBootIdentityNative(t *testing.T) {
	ctx := windowsSyntheticTestContext(t)
	t.Log(windowsBootEnvironmentDiagnostic())
	first, err := serviceBootID()
	if err != nil {
		t.Fatal("service_boot_identity_unavailable")
	}
	second, err := serviceBootID()
	if err != nil || first != second {
		t.Fatal("same-process boot identity differs")
	}
	c, _ := unitCell(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal("boot fixture executable unavailable")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("boot fixture requires a bounded context")
	}
	// Reserve pipe-copy cleanup inside this test's share of the aggregate budget.
	waitDelay := windowsSyntheticTestBudget / 300
	commandContext, cancel := context.WithDeadline(ctx, deadline.Add(-waitDelay))
	defer cancel()
	cmd := exec.CommandContext(commandContext, self, "-test.run=^TestWindowsBootIdentityChild$", "--", "localcell-boot-identity-child")
	cmd.WaitDelay = waitDelay
	cmd.Env, err = c.baseEnvironment()
	if err != nil {
		t.Fatal("boot fixture environment unavailable")
	}
	cmd.Stdin = strings.NewReader(first)
	if cmd.Run() != nil {
		t.Fatal("cross-process boot identity differs")
	}
	t.Log("boot_identity same_process_equal=true child_process_equal=true")
}
