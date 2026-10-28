//go:build !windows

package localcell_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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
