package runtimeconfig

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLocalShutdownConfiguration(t *testing.T) {
	for _, role := range []string{"server", "worker"} {
		t.Run(role, func(t *testing.T) {
			values := localValues(t)
			path := filepath.Join(t.TempDir(), "descriptor.json")
			values["WITSELF_LOCAL_SHUTDOWN_DESCRIPTOR"] = path
			for _, explicit := range []bool{false, true} {
				if explicit {
					values["WITSELF_LOCAL_SHUTDOWN_TRANSPORT"] = LocalShutdownNamedPipe
				}
				cfg, err := FromEnv(role, testLookup(values), nil)
				if err != nil || cfg.LocalShutdown != (LocalShutdown{Transport: LocalShutdownNamedPipe, DescriptorPath: path}) {
					t.Fatalf("valid opt-in refused: %v", err)
				}
			}
		})
	}
}

func TestLocalShutdownConfigurationRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, key, value, role, runtime string
		omitDescriptor                  bool
	}{
		{name: "case transport", key: "WITSELF_LOCAL_SHUTDOWN_TRANSPORT", value: "NAMED_PIPE"},
		{name: "unknown transport", key: "WITSELF_LOCAL_SHUTDOWN_TRANSPORT", value: "private-value"},
		{name: "empty transport", key: "WITSELF_LOCAL_SHUTDOWN_TRANSPORT"},
		{name: "transport alone", key: "WITSELF_LOCAL_SHUTDOWN_TRANSPORT", value: LocalShutdownNamedPipe, omitDescriptor: true},
		{name: "relative path", key: "WITSELF_LOCAL_SHUTDOWN_DESCRIPTOR", value: "private/descriptor.json"},
		{name: "empty path", key: "WITSELF_LOCAL_SHUTDOWN_DESCRIPTOR"},
		{name: "nul path", key: "WITSELF_LOCAL_SHUTDOWN_DESCRIPTOR", value: "\x00private-value"},
		{name: "descriptor name alias", key: "witself_local_shutdown_descriptor", value: "private-value"},
		{name: "transport name alias", key: "Witself_Local_Shutdown_Transport", value: LocalShutdownNamedPipe},
		{name: "unknown selector", key: "WITSELF_LOCAL_SHUTDOWN_ROLE", value: "server"},
		{name: "invalid role", role: "postgres"},
		{name: "missing local", runtime: "unset"},
		{name: "empty local", runtime: "empty"},
		{name: "case local", runtime: "LOCAL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := localValues(t)
			values["WITSELF_LOCAL_SHUTDOWN_DESCRIPTOR"] = filepath.Join(t.TempDir(), "descriptor.json")
			if tc.omitDescriptor {
				delete(values, "WITSELF_LOCAL_SHUTDOWN_DESCRIPTOR")
			}
			if tc.key != "" {
				values[tc.key] = tc.value
			}
			switch tc.runtime {
			case "unset":
				delete(values, "WITSELF_RUNTIME")
			case "empty":
				values["WITSELF_RUNTIME"] = ""
			case "":
			default:
				values["WITSELF_RUNTIME"] = tc.runtime
			}
			role := tc.role
			if role == "" {
				role = "server"
			}
			var names []string
			for name, value := range values {
				names = append(names, name+"="+value)
			}
			cfg, err := FromEnv(role, testLookup(values), names)
			if !errors.Is(err, ErrLocalShutdownConfigInvalid) || err.Error() != "local_shutdown_config_invalid: shutdown:config" || !reflect.DeepEqual(cfg, Config{}) {
				t.Fatalf("expected exact value-free configuration refusal, got %v", err)
			}
		})
	}
}

func TestLocalShutdownHostedAbsence(t *testing.T) {
	for _, selected := range []string{"", "unset"} {
		values := map[string]string{}
		if selected != "unset" {
			values["WITSELF_RUNTIME"] = selected
		}
		cfg, err := FromEnv("server", testLookup(values), nil)
		if err != nil || !reflect.DeepEqual(cfg, Config{}) {
			t.Fatal("hosted absence changed configuration")
		}
	}
	// Selector values never enter a diagnostic, including multiline payloads.
	values := localValues(t)
	values["WITSELF_LOCAL_SHUTDOWN_DESCRIPTOR"] = "private\nmarker"
	_, err := FromEnv("server", testLookup(values), nil)
	if err == nil || strings.Contains(err.Error(), "marker") {
		t.Fatal("shutdown refusal disclosed a supplied value")
	}
}
