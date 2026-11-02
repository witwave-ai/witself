//go:build !windows

package localcell_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNativeServiceDirectoryDiagnostics(t *testing.T) {
	home := t.TempDir()
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	wants := filepath.Join(unitDir, "default.target.wants")
	if err := os.MkdirAll(wants, 0o755); err != nil {
		t.Fatal("cannot create directory diagnostic fixture")
	}
	if err := os.Chmod(wants, 0o755); err != nil {
		t.Fatal("cannot set directory diagnostic fixture mode")
	}
	log := func() string {
		var output strings.Builder
		nativeServiceDirectoryDiagnostics(func(format string, values ...any) { fmt.Fprintf(&output, format+"\n", values...) }, home)
		return sanitizeNativeServiceDiagnostic(output.String(), "", home)
	}
	output := log()
	for _, suffix := range []string{"", "/.config", "/.config/systemd", "/.config/systemd/user", "/.config/systemd/user/default.target.wants"} {
		if !strings.Contains(output, "directory <home>"+suffix+" mode=") {
			t.Fatal("directory diagnostics omitted a required path")
		}
	}
	uid := strconv.Itoa(os.Getuid())
	mode := "0755"
	if !strings.Contains(output, "default.target.wants mode="+mode+" uid="+uid+" symlink=false") {
		t.Fatal("directory diagnostics omitted mode, owner UID, or symlink evidence")
	}
	if err := os.Remove(wants); err != nil {
		t.Fatal("cannot replace directory diagnostic fixture")
	}
	if err := os.Symlink(filepath.Join(home, "missing-target"), wants); err != nil {
		t.Fatal("cannot create symlink diagnostic fixture")
	}
	if output = log(); !strings.Contains(output, "symlink=true") || strings.Contains(output, "default.target.wants metadata error") {
		t.Fatal("directory diagnostics followed the symlink instead of reporting its own metadata")
	}
}

func TestNativeServiceLaunchdDirectoryDiagnostics(t *testing.T) {
	home := t.TempDir()
	directory := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal("cannot create LaunchAgents diagnostic fixture")
	}
	if err := os.WriteFile(filepath.Join(directory, "fixture.plist"), []byte("private plist contents"), 0o600); err != nil {
		t.Fatal("cannot create plist metadata fixture")
	}
	log := func() string {
		var output strings.Builder
		nativeServiceLaunchdDirectoryDiagnostics(func(format string, values ...any) { fmt.Fprintf(&output, format+"\n", values...) }, home)
		return sanitizeNativeServiceDiagnostic(output.String(), "", home)
	}
	output := log()
	for _, expected := range []string{"<home>/Library mode=0700 symlink=false", "<home>/Library/LaunchAgents mode=0700 symlink=false", "LaunchAgents entry fixture.plist mode=0600"} {
		if !strings.Contains(output, expected) {
			t.Error("LaunchAgents diagnostics omitted required names or modes")
		}
	}
	if strings.Contains(output, "private plist contents") || strings.Contains(output, " uid=") {
		t.Fatal("LaunchAgents diagnostics exceeded names and modes")
	}
	if err := os.Rename(directory, filepath.Join(home, "foreign")); err != nil {
		t.Fatal("cannot move directory diagnostic fixture")
	}
	if err := os.Symlink(filepath.Join(home, "foreign"), directory); err != nil {
		t.Fatal("cannot create LaunchAgents symlink fixture")
	}
	output = log()
	if !strings.Contains(output, "symlink=true") || strings.Contains(output, "entry fixture.plist") {
		t.Fatal("LaunchAgents diagnostics followed a directory symlink")
	}
}

func TestNativeServiceLaunchdStderrRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "service"), 0o700); err != nil {
		t.Fatal("cannot create log symlink fixture")
	}
	foreign := filepath.Join(t.TempDir(), "foreign")
	if err := os.WriteFile(foreign, []byte("foreign log contents"), 0o600); err != nil {
		t.Fatal("cannot write foreign log fixture")
	}
	if err := os.Symlink(foreign, filepath.Join(root, "service", "host-stderr.log")); err != nil {
		t.Fatal("cannot create log symlink fixture")
	}
	if output, err := nativeServiceLaunchdStderr(root); err == nil || len(output) != 0 {
		t.Fatal("launchd diagnostics followed a log symlink")
	}
	if err := os.Rename(filepath.Join(root, "service"), filepath.Join(root, "old-service")); err != nil {
		t.Fatal("cannot move service directory diagnostic fixture")
	}
	if err := os.Symlink(filepath.Join(root, "old-service"), filepath.Join(root, "service")); err != nil {
		t.Fatal("cannot create service directory symlink fixture")
	}
	if output, err := nativeServiceLaunchdStderr(root); err == nil || len(output) != 0 {
		t.Fatal("launchd diagnostics followed a service directory symlink")
	}
}

// The launchd failure-diagnostics contract runs through the GOOS seam against
// host temporary directories and darwin path separators; Windows separators
// would change the sanitized evidence shape, so it stays Unix-only like the
// other Unix-semantics hermetic tests (slice LC-S7A lesson).
func TestNativeServiceLaunchdFailureDiagnostics(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	for _, directory := range []string{filepath.Join(home, "Library", "LaunchAgents"), filepath.Join(root, "service")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal("cannot create launchd diagnostic fixture")
		}
	}
	hex, token := strings.Repeat("d3", 32), strings.Repeat("x", 48)
	standardToken := nativeServiceStandardBase64TokenFixture(t)
	const environmentValue = "private launchd environment value"
	var hostStderr strings.Builder
	for line := 0; line < 35; line++ {
		fmt.Fprintf(&hostStderr, "host-line-%02d %s %s %s %s LANG=%s\n", line, root, hex, token, standardToken, environmentValue)
	}
	if err := os.WriteFile(filepath.Join(root, "service", "host-stderr.log"), []byte(hostStderr.String()), 0o600); err != nil {
		t.Fatal("cannot create launchd host stderr fixture")
	}
	var logged strings.Builder
	var commands [][]string
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second || ctx.Err() != nil {
			t.Error("launchd diagnostic lacks an independent bounded context")
		}
		commands = append(commands, append([]string{name}, args...))
		if name == "child-capability" {
			return []byte("service_manager_unavailable: gui_domain: fixture unavailable"), errors.New("fixture child unavailable")
		}
		return []byte("gui/501/fixture = {\n\tpath = " + home + "/Library/LaunchAgents/fixture.plist\n\tprogram = /private/unlisted-host\n\targuments = {\n\t\t/private/unlisted-host\n\t\t" + root + "\n\t}\n\tstate = running\n\tenvironment = {\n\t\tUNRELATED_SECRET => " + environmentValue + "\n\t}\n\tunknown = " + hex + " " + token + " " + standardToken + "\n\tresource coalition = {\n\t\tID = 7\n\t}\n}\n"), nil
	}
	const label = "ai.witwave.witself.cell.fixture"
	nativeServiceFailureDiagnosticsForGOOS("darwin", 501, func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		if strings.Contains(line, "\n") {
			t.Error("launchd diagnostic would lose CI source prefixes")
		}
		fmt.Fprintln(&logged, line)
	}, root, home, label, errors.New("fixture failure "+root+" "+standardToken), nil, run)
	want := [][]string{{"launchctl", "print", "gui/501/" + label}, {"launchctl", "print", "gui/501"}, {"child-capability"}}
	if !reflect.DeepEqual(commands, want) {
		t.Fatal("launchd diagnostics did not query exactly the service, gui domain and fresh child")
	}
	output := logged.String()
	for _, private := range []string{root, home, hex, token, standardToken, environmentValue, "UNRELATED_SECRET", "/private/unlisted-host"} {
		if strings.Contains(output, private) {
			t.Fatal("launchd diagnostics exposed a planted private value")
		}
	}
	for _, expected := range []string{"host-line-05 <root> <hex> <token> <token> <environment>", "host-line-34", "path = <path>", "program = <path>", "arguments = {", "<arg:0>", "<arg:1>", "state = <value>", "\tresource coalition = {\n\t\tID = <value>\n\t}\n}\n", "service_manager_unavailable: gui_domain", "<home>/Library/LaunchAgents mode="} {
		if !strings.Contains(output, expected) {
			t.Error("launchd diagnostics omitted required value-free evidence")
		}
	}
	for line := 0; line < 5; line++ {
		if strings.Contains(output, fmt.Sprintf("host-line-%02d", line)) {
			t.Error("launchd host stderr exceeded its last-30-lines boundary")
		}
	}
}
