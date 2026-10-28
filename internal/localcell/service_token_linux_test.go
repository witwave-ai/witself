//go:build linux

package localcell

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type serviceManagerEnvironmentObservation struct {
	Entries     int  `json:"entries"`
	Expected    bool `json:"expected"`
	TokenOffset int  `json:"token_offset"`
	TokenLength int  `json:"token_length"`
}

func TestServiceSystemdManagerEnvironmentExcludesControlToken(t *testing.T) {
	c, _ := unitCell(t)
	prepareSupervisorTest(t, c)
	token := installServiceFixtureControlToken(t, c)
	// A caller may already have a secret in its environment. The actual Linux
	// command closure must supply only its explicit session and config paths.
	t.Setenv("WITSELF_CONTROL_TOKEN", string(token))
	adapter, ok := NewServiceAdapter().(*systemdUserAdapter)
	if !ok {
		t.Fatal("Linux adapter constructor did not provide the command fixture seam")
	}
	adapter.runtimeDir = t.TempDir()
	adapter.home = filepath.Join(adapter.runtimeDir, "home")
	adapter.unitDir = filepath.Join(adapter.home, ".config", "systemd", "user")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal("cannot resolve synthetic manager executable")
	}
	// Invoke the unmodified production command closure, using this test binary
	// as the fake manager. No real systemctl or user manager is called.
	output, err := adapter.command(t.Context(), binary, "-test.run=^TestServiceSystemdManagerCommandEnvironmentFixture$", "--", "service-manager-environment-fixture", adapter.runtimeDir)
	if err != nil {
		t.Fatal("synthetic manager process failed")
	}
	assertServiceOutputExcludesToken(t, "manager process output", output, token)
	data, err := os.ReadFile(filepath.Join(adapter.runtimeDir, "manager-observation.json"))
	if err != nil {
		t.Fatal("synthetic manager did not capture its environment")
	}
	var observation serviceManagerEnvironmentObservation
	if json.Unmarshal(data, &observation) != nil {
		t.Fatal("invalid synthetic manager environment observation")
	}
	if observation.TokenOffset >= 0 {
		t.Errorf("control token present in manager environment (length=%d offset=%d)", observation.TokenLength, observation.TokenOffset)
	}
	if observation.Entries != 4 || !observation.Expected {
		t.Fatal("manager process environment differed from the four explicit session and config variables")
	}
}

func TestServiceSystemdManagerCommandEnvironmentFixture(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "service-manager-environment-fixture" {
		return
	}
	runtimeDir := os.Args[len(os.Args)-1]
	token := serviceFixtureControlToken()
	// Inspect child environment bytes in memory only. Persist only counts and
	// comparisons, so even a regression that inherits credentials cannot print
	// or write those values into a fixture observation or failure message.
	observed := os.Environ()
	expected := []string{"XDG_RUNTIME_DIR=" + runtimeDir, "DBUS_SESSION_BUS_ADDRESS=unix:path=" + runtimeDir + "/bus", "HOME=" + filepath.Join(runtimeDir, "home"), "XDG_CONFIG_HOME=" + filepath.Join(runtimeDir, "home", ".config")}
	observation := serviceManagerEnvironmentObservation{Entries: len(observed), Expected: reflect.DeepEqual(observed, expected), TokenOffset: -1, TokenLength: len(token)}
	for _, entry := range observed {
		if offset := bytes.Index([]byte(entry), token); offset >= 0 {
			observation.TokenOffset = offset
			break
		}
	}
	data, err := json.Marshal(observation)
	if err != nil || os.WriteFile(filepath.Join(runtimeDir, "manager-observation.json"), data, 0o600) != nil {
		t.Fatal("cannot persist value-free manager environment observation")
	}
}
