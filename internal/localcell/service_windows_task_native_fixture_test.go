package localcell

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Execute the actual Windows entry bodies against fake token APIs and a
// recording runner on every host. Extraction preserves the entire body, so an
// extra /Create in the public capability entry is observed, not hidden behind
// a different injected capability implementation. No native manager runs.
func TestWindowsTaskNativeCapabilityAndOwnerFixtures(t *testing.T) {
	sources := windowsTaskNativeContractSources(t)
	native := sources["service_windows_task_native_windows.go"]
	var production strings.Builder
	for _, name := range []string{"WindowsLogonTaskCapability", "validateTaskHostOwner"} {
		function := windowsTaskContractFunction(native, name, false)
		if function == nil {
			t.Fatal("cannot extract native task capability and owner bodies")
		}
		production.WriteString(windowsTaskContractText(function))
		production.WriteByte('\n')
	}
	identity, err := os.ReadFile("service_windows_task_identity.go")
	if err != nil {
		t.Fatal("cannot read task owner policy")
	}
	policy := windowsTaskContractFunction(string(identity), "validWindowsTaskOwner", false)
	if policy == nil {
		t.Fatal("cannot extract native task owner policy")
	}
	production.WriteString(windowsTaskContractText(policy))
	directory := t.TempDir()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	for name, data := range map[string]string{
		"go.mod":         "module nativefixture\n\ngo 1.27.0\n",
		"native_test.go": windowsTaskNativeFixture + production.String(),
	} {
		if os.WriteFile(filepath.Join(directory, name), []byte(data), 0o600) != nil {
			t.Fatal("cannot write native task capability fixture")
		}
	}
	command := exec.CommandContext(t.Context(), "go", "test", "-count=1", "-v", ".")
	command.Dir = directory
	// The extracted fixture has no VCS or dependencies; the sandbox protects a
	// synthetic /tmp/.git, which must not be interpreted as its repository.
	command.Env = append(os.Environ(), "GOFLAGS=-mod=readonly -buildvcs=false", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off")
	output, runErr := command.CombinedOutput()
	if runErr != nil {
		for _, failure := range []string{"capability probe mutated task manager", "non-elevated owner equality bypassed"} {
			if strings.Contains(string(output), failure) {
				t.Fatal(failure)
			}
		}
		t.Fatal("native capability and owner fixture failed without private output")
	}
	for _, name := range []string{"TestCapabilityReadOnly", "TestNonElevatedOwnerEquality"} {
		if !strings.Contains(string(output), "--- PASS: "+name+" ") {
			t.Fatal("native capability and owner fixture omitted an assertion")
		}
	}
}

const windowsTaskNativeFixture = `package nativefixture
import (
 "context"
 "errors"
 "os"
 "strings"
 "testing"
)
var _ = os.Args
var ErrServiceManagerUnavailable = errors.New("manager unavailable")
var fixtureOwner = "S-" + "1-5-21-101-202-303-1001"
var tokenOwner = fixtureOwner
var tokenElevation uint32
var managerCalls int
type fakeSID string
func (s *fakeSID) String() string { return string(*s) }
type fakeToken struct{}
type fakeTokenUser struct { User struct { Sid *fakeSID } }
func (fakeToken) GetTokenUser() (*fakeTokenUser, error) {
 value := fakeSID(tokenOwner)
 user := &fakeTokenUser{}
 user.User.Sid = &value
 return user, nil
}
var windows = struct {
 GetCurrentProcessToken func() fakeToken
 TokenElevation uint32
}{func() fakeToken { return fakeToken{} }, 1}
func windowsTaskTokenUint32(fakeToken, uint32) (uint32, error) { return tokenElevation, nil }
func serviceOwner() (string, error) { return fixtureOwner, nil }
func windowsTaskInteractiveSession() error { return nil }
func windowsTaskSystemCommand() (string, string, error) { return "", "", nil }
func windowsTaskError(_ error, _ string, _ uint32, reason string) error { return errors.New(reason) }
type windowsTaskCommandResult struct{}
func runWindowsTaskCommand(context.Context, ...string) (windowsTaskCommandResult, error) {
 managerCalls++
 return windowsTaskCommandResult{}, nil
}
func TestCapabilityReadOnly(t *testing.T) {
 managerCalls, tokenElevation, tokenOwner = 0, 0, fixtureOwner
 err := WindowsLogonTaskCapability(t.Context())
 if managerCalls != 0 { t.Fatal("capability probe mutated task manager") }
 if err == nil || err.Error() != "query_result_untyped" { t.Fatal("capability probe lost fixed untyped refusal") }
}
func TestNonElevatedOwnerEquality(t *testing.T) {
 tokenElevation, tokenOwner = 0, fixtureOwner
 if validateTaskHostOwner(fixtureOwner) != nil { t.Fatal("matching non-elevated owner refused") }
 if validateTaskHostOwner(fixtureOwner+"-2") == nil { t.Fatal("non-elevated owner equality bypassed") }
 tokenElevation = 1
 if validateTaskHostOwner(fixtureOwner) == nil { t.Fatal("elevated owner accepted") }
}
`
