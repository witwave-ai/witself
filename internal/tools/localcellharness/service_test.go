package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

func TestServiceAdapterKindByGOOS(t *testing.T) {
	for _, test := range []struct{ goos, adapter string }{
		{"linux", "systemd-user"}, {"darwin", "launchd-agent"},
		{"windows", ""}, {"freebsd", ""}, {"", ""},
	} {
		t.Run(test.goos, func(t *testing.T) {
			adapter, err := serviceAdapterKind(test.goos)
			if adapter != test.adapter {
				t.Fatal("service adapter selection differs from platform contract")
			}
			if test.adapter == "" {
				if !errors.Is(err, localcell.ErrServiceManagerUnavailable) || !strings.Contains(err.Error(), "adapter:platform: unsupported") || !strings.Contains(err.Error(), "localcellharness run") {
					t.Fatal("unsupported platform omitted its capability and foreground fallback")
				}
			} else if err != nil {
				t.Fatal("supported platform did not select an adapter")
			}
		})
	}
}

func TestServiceInstallUnsupportedGOOSDoesNotTouchDescriptor(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("WITSELF_RUNTIME", "local")
	for _, goos := range []string{"windows", "freebsd"} {
		t.Run(goos, func(t *testing.T) {
			root := privateHarnessRoot(t)
			serviceDir := filepath.Join(root, "service")
			if os.Mkdir(serviceDir, 0o700) != nil {
				t.Fatal("cannot prepare descriptor refusal fixture")
			}
			path := filepath.Join(serviceDir, "descriptor.json")
			before := []byte("invalid descriptor must not be read or replaced\n")
			if os.WriteFile(path, before, 0o600) != nil {
				t.Fatal("cannot prepare descriptor refusal fixture")
			}
			err := runServiceCommandForGOOS(t.Context(), []string{"service", "install", "--root", root}, io.Discard, goos)
			if !errors.Is(err, localcell.ErrServiceManagerUnavailable) {
				t.Fatal("unsupported install touched installation state before capability refusal")
			}
			after, err := os.ReadFile(path)
			entries, dirErr := os.ReadDir(root)
			serviceEntries, serviceErr := os.ReadDir(serviceDir)
			if err != nil || string(after) != string(before) || dirErr != nil || len(entries) != 1 || serviceErr != nil || len(serviceEntries) != 1 {
				t.Fatal("unsupported install mutated the descriptor or installation")
			}
		})
	}
}

func TestServiceCommandsRequireExplicitLocalSelection(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	for _, selector := range []string{"", "hosted", "LOCAL"} {
		t.Run(selector, func(t *testing.T) {
			t.Setenv("WITSELF_RUNTIME", selector)
			root := privateHarnessRoot(t)
			for _, action := range [][]string{{"run"}, {"service", "install"}, {"service", "start"}, {"service", "stop"}, {"service", "status"}, {"service", "observe"}, {"service", "remove"}, {"service", "repair"}} {
				if err := run(context.Background(), append(action, "--root", root), io.Discard); err == nil {
					t.Fatal("service command accepted an unselected local runtime")
				}
			}
			if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
				t.Fatal("unselected service command mutated the private root")
			}
		})
	}
}

func TestServiceArgumentsPreserveAbsolutePayloadPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "owner's café 日本語")
	values := serviceFlags{root: root, pgbin: filepath.Join(root, "Postgres bin"), server: filepath.Join(root, "server's executable"), worker: filepath.Join(root, "工作"), jobs: "message_rate_bucket_cleanup", timeout: 3 * time.Second}
	args := values.hostArgs()
	if args[0] != "run" {
		t.Fatal("service payload omitted foreground operation")
	}
	reopened, err := parseServiceFlags(args[1:])
	if err != nil || !reflect.DeepEqual(reopened, values) {
		t.Fatal("service payload changed argument boundaries or Unicode")
	}
}

func TestServiceArgumentsRefuseUnsupportedActionsAndFlags(t *testing.T) {
	t.Setenv("WITSELF_RUNTIME", "local")
	root := privateHarnessRoot(t)
	for _, args := range [][]string{
		{"service"}, {"service", "enable", "--root", root},
		{"run", "--root", "relative"}, {"run", "--root", root, "--timeout", "0s"},
		{"run", "--root", root, "--purge"}, {"service", "install", "--root", root, "--system"},
		{"run", "--root", root, "unexpected"},
	} {
		if err := run(t.Context(), args, io.Discard); err == nil {
			t.Fatal("invalid service command was accepted")
		}
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatal("invalid service command mutated the private root")
	}
}
