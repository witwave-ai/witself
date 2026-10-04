package main

import "testing"

func TestCommandOnlyFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"up batch", []string{"up", "-batch", "2"}, "-batch is only valid with `rebalance`"},
		{"health limit", []string{"health", "-limit", "3"}, "-limit is only valid with `placement-status`"},
		{"config show run", []string{"config", "show", "-run"}, "-run is only valid with `placement-runner`"},
		{"whoami enable", []string{"whoami", "-enable"}, "-enable is only valid with `placement-runner`"},
		{"destroy disable", []string{"destroy", "-disable"}, "-disable is only valid with `placement-runner`"},
		{"config remove batch", []string{"config", "remove-cell", "-batch", "2"}, "-batch is only valid with `rebalance`"},
		{"rebalance limit", []string{"rebalance", "-limit", "3"}, "-limit is only valid with `placement-status`"},
		{"placement status run", []string{"placement-status", "-run"}, "-run is only valid with `placement-runner`"},
		{"rebalance owns batch", []string{"rebalance", "-batch", "2"}, "rebalance requires -control-plane"},
		{"placement runner owns flags", []string{"placement-runner", "-enable", "-disable", "-run"}, "placement-runner requires -control-plane"},
		{"placement status owns limit", []string{"placement-status", "-limit", "5"}, "placement-status requires -control-plane"},
		{"explicit false", []string{"up", "-run=false"}, "-run is only valid with `placement-runner`"},
		{"fixed flag order", []string{"up", "-limit", "3", "-run", "-disable", "-enable", "-batch", "2"}, "-batch is only valid with `rebalance`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r2TestHome(t)
			err := run(tc.args)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("command-only guard mismatch; want %q", tc.want)
			}
			requireNoSecretOutput(t, err.Error())
		})
	}
}
