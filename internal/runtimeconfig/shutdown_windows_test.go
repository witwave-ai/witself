//go:build windows

package runtimeconfig

import (
	"errors"
	"strings"
	"testing"
)

func TestLocalShutdownWindowsPathRefusals(t *testing.T) {
	for _, path := range []string{
		`\\server\share\descriptor.json`, `\\.\pipe\descriptor`, `\\?\C:\private\descriptor.json`,
		`C:\private\CON.json`, `C:\private\descriptor.json:stream`, `C:\private\descriptor.json.`,
		`C:\private\descriptor.json `, `C:\private\` + strings.Repeat("x", 260) + ".json",
	} {
		values := localValues(t)
		values["WITSELF_LOCAL_SHUTDOWN_DESCRIPTOR"] = path
		if _, err := FromEnv("server", testLookup(values), nil); !errors.Is(err, ErrLocalShutdownConfigInvalid) {
			t.Fatal("unsafe Windows descriptor path reached descriptor startup")
		}
	}
}
