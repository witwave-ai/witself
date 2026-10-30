package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

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
