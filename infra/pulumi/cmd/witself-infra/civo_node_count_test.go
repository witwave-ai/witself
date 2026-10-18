package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/witwave-ai/witself/infra/pulumi/internal/backend"
	"gopkg.in/yaml.v3"
)

const civoNodeCountFixture = `version: 1
defaults:
  profile: minimal
cells:
  civo-sandbox-use1-backup:
    cloud: civo
    account_alias: sandbox
    region: nyc1
    role: backup
    civo_node_size: g4s.kube.medium
  civo-sandbox-use1-serving:
    cloud: civo
    account_alias: sandbox
    region: nyc1
    role: serving
    profile: prod
    civo_node_size: g4s.kube.medium
  civo-prod-use1-serving:
    cloud: civo
    account_alias: prod
    region: nyc1
    role: serving
    profile: prod
    civo_node_size: g4s.kube.large
    civo_node_count: 3
  aws-sandbox-usw2-dev:
    cloud: aws
    account_alias: sandbox
    region: us-west-2
    role: dev
`

const civoNodeCountTarget = "civo-prod-use1-serving"
const civoNodeCountBackup = "civo-sandbox-use1-backup"
const civoNodeCountSandbox = "civo-sandbox-use1-serving"
const civoNodeCountAWS = "aws-sandbox-usw2-dev"
const civoNodeCountQuotaMessage = "civo node count 3 for cell civo-prod-use1-serving needs 12 cores (3 x g4s.kube.large at 4 each); the other Civo cells in the inventory plan %d, so the total of %d is above the Civo core quota of %d. Retire a cell (destroy, then config remove-cell), or record a larger defaults.civo_core_quota once Civo has raised the account's quota"
const civoNodeCountBareQuotaMessage = "civo node count 5 for cell civo-prod-use1-serving needs 20 cores (5 x g4s.kube.large at 4 each), above the Civo core quota of 16; without -cell no other cell is counted"

func civoNodeCountInt(value int) *int { return &value }

func civoNodeCountConfigPath(t *testing.T, edit func(*infraConfig)) string {
	t.Helper()
	var cfg infraConfig
	if err := yaml.Unmarshal([]byte(civoNodeCountFixture), &cfg); err != nil {
		t.Fatal("decode node-count fixture")
	}
	if edit != nil {
		edit(&cfg)
	}
	raw, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatal("encode node-count fixture")
	}
	return writeConfig(t, string(raw))
}

func civoNodeCountSetEntry(cfg *infraConfig, name string, edit func(*cellEntry)) {
	e := cfg.Cells[name]
	edit(&e)
	cfg.Cells[name] = e
}

func civoNodeCountPin(cfg *infraConfig, name, account string) {
	civoNodeCountSetEntry(cfg, name, func(e *cellEntry) {
		e.SecurityContext = &securityContext{Civo: &civoContext{ExpectedAccountID: strPtr(account)}}
	})
}

func TestCivoNodeCoresCoverEverySupportedSize(t *testing.T) {
	if len(civoNodeCores) != len(civoNodeSizes) {
		t.Fatal("core table and supported sizes have different key counts")
	}
	for size := range civoNodeSizes {
		if _, ok := civoNodeCores[size]; !ok {
			t.Errorf("supported size %q has no core count", size)
		}
	}
	for size := range civoNodeCores {
		if _, ok := civoNodeSizes[size]; !ok {
			t.Errorf("core table has unsupported size %q", size)
		}
	}
	if civoNodeCores["g4s.kube.medium"] != 2 || civoNodeCores["g4s.kube.large"] != 4 {
		t.Error("reviewed medium and large core counts changed")
	}
}

func TestCivoCoreQuotaError(t *testing.T) {
	for _, tc := range []struct {
		name, account, size string
		count, quota        int
		nilInventory        bool
		edit                func(*infraConfig)
		want                string
	}{
		{name: "a_planned_total", count: 3, quota: 16, want: fmt.Sprintf(civoNodeCountQuotaMessage, 6, 18, 16)},
		{name: "b_target_excluded", count: 3, quota: 16, edit: func(c *infraConfig) { delete(c.Cells, civoNodeCountSandbox) }},
		{name: "c_equal_passes", count: 3, quota: 16, edit: func(c *infraConfig) {
			civoNodeCountSetEntry(c, civoNodeCountSandbox, func(e *cellEntry) { e.Profile = strPtr("minimal") })
		}},
		{name: "c_above_refused", count: 3, quota: 15, want: fmt.Sprintf(civoNodeCountQuotaMessage, 4, 16, 15), edit: func(c *infraConfig) {
			civoNodeCountSetEntry(c, civoNodeCountSandbox, func(e *cellEntry) { e.Profile = strPtr("minimal") })
		}},
		{name: "d_default_inert", count: 2, quota: 1},
		{name: "d_zero_inert", count: 0, quota: 1},
		{name: "d_default_reads_nothing", size: "unknown", count: 2, quota: 1, edit: func(c *infraConfig) {
			civoNodeCountSetEntry(c, civoNodeCountBackup, func(e *cellEntry) { e.CivoNodeSize = strPtr("unknown") })
		}},
		{name: "e_other_recorded_count", count: 3, quota: 16, want: fmt.Sprintf(civoNodeCountQuotaMessage, 8, 20, 16), edit: func(c *infraConfig) {
			civoNodeCountSetEntry(c, civoNodeCountSandbox, func(e *cellEntry) { e.CivoNodeCount = civoNodeCountInt(3) })
		}},
		{name: "f1_other_accounts", account: "acct-a", count: 3, quota: 16, edit: func(c *infraConfig) {
			civoNodeCountPin(c, civoNodeCountBackup, "acct-b")
			civoNodeCountPin(c, civoNodeCountSandbox, "acct-b")
		}},
		{name: "f2_same_account", account: "acct-a", count: 3, quota: 13, want: fmt.Sprintf(civoNodeCountQuotaMessage, 2, 14, 13), edit: func(c *infraConfig) {
			civoNodeCountPin(c, civoNodeCountBackup, "acct-a")
			civoNodeCountPin(c, civoNodeCountSandbox, "acct-b")
		}},
		{name: "f2_padded_same_account", account: "acct-a", count: 3, quota: 13, want: fmt.Sprintf(civoNodeCountQuotaMessage, 2, 14, 13), edit: func(c *infraConfig) {
			civoNodeCountPin(c, civoNodeCountBackup, " \tacct-a\n")
			civoNodeCountPin(c, civoNodeCountSandbox, "acct-b")
		}},
		{name: "f2_padded_target_account", account: " \tacct-a\n", count: 3, quota: 13, want: fmt.Sprintf(civoNodeCountQuotaMessage, 2, 14, 13), edit: func(c *infraConfig) {
			civoNodeCountPin(c, civoNodeCountBackup, "acct-a")
			civoNodeCountPin(c, civoNodeCountSandbox, "acct-b")
		}},
		{name: "f3_unpinned_target", count: 3, quota: 16, want: fmt.Sprintf(civoNodeCountQuotaMessage, 6, 18, 16), edit: func(c *infraConfig) {
			civoNodeCountPin(c, civoNodeCountBackup, "acct-b")
			civoNodeCountPin(c, civoNodeCountSandbox, "acct-b")
		}},
		{name: "f_unpinned_others", account: "acct-a", count: 3, quota: 16, want: fmt.Sprintf(civoNodeCountQuotaMessage, 6, 18, 16)},
		{name: "g_inherited_profile", count: 3, quota: 16, want: fmt.Sprintf(civoNodeCountQuotaMessage, 8, 20, 16), edit: func(c *infraConfig) { c.Defaults.Profile = strPtr("prod") }},
		{name: "g_inherited_size", count: 3, quota: 16, want: fmt.Sprintf(civoNodeCountQuotaMessage, 8, 20, 16), edit: func(c *infraConfig) {
			c.Defaults.CivoNodeSize = strPtr("g4s.kube.large")
			civoNodeCountSetEntry(c, civoNodeCountBackup, func(e *cellEntry) { e.CivoNodeSize = nil })
		}},
		{name: "h_nil_inventory_refused", count: 5, quota: 16, nilInventory: true, want: civoNodeCountBareQuotaMessage},
		{name: "h_nil_inventory_equal", count: 4, quota: 16, nilInventory: true},
		{name: "h_nil_inventory_one_core_above", count: 4, quota: 15, nilInventory: true, want: "civo node count 4 for cell civo-prod-use1-serving needs 16 cores (4 x g4s.kube.large at 4 each), above the Civo core quota of 15; without -cell no other cell is counted"},
		{name: "i_unknown_inventory_size", count: 3, quota: 16, want: `cell "civo-sandbox-use1-backup" in the inventory has civo_node_size "g4s.kube.xlarge", whose core count is unknown; correct that record before raising a node count`, edit: func(c *infraConfig) {
			civoNodeCountSetEntry(c, civoNodeCountBackup, func(e *cellEntry) { e.CivoNodeSize = strPtr("g4s.kube.xlarge") })
		}},
		{name: "j_unknown_target_size", count: 3, quota: 16, size: "g4s.kube.xlarge", nilInventory: true, want: `civo node size "g4s.kube.xlarge" has no known core count, so the quota check cannot run`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, err := loadInfraConfig(writeConfig(t, civoNodeCountFixture))
			if err != nil {
				t.Fatal("load node-count fixture")
			}
			if tc.edit != nil {
				tc.edit(cfg)
			}
			if tc.nilInventory {
				cfg = nil
			}
			size := tc.size
			if size == "" {
				size = "g4s.kube.large"
			}
			err = civoCoreQuotaError(civoNodeCountTarget, tc.account, "prod", size, tc.count, tc.quota, cfg)
			assertCivoIngressError(t, err, tc.want)
		})
	}
}

type civoNodeCountWrite struct {
	method, key string
	value       auto.ConfigValue
}

type civoNodeCountWriter struct {
	calls []civoNodeCountWrite
	err   error
}

func (w *civoNodeCountWriter) SetConfig(_ context.Context, key string, value auto.ConfigValue) error {
	w.calls = append(w.calls, civoNodeCountWrite{method: "set", key: key, value: value})
	return w.err
}

func (w *civoNodeCountWriter) RemoveConfig(_ context.Context, key string) error {
	w.calls = append(w.calls, civoNodeCountWrite{method: "remove", key: key})
	return w.err
}

func TestApplyCivoNodeCount(t *testing.T) {
	errFake := errors.New("fake writer error")
	for _, tc := range []struct {
		name, cmd, prefix string
		count             int
		writerErr         error
		want              []civoNodeCountWrite
	}{
		{name: "up_default", cmd: "up", want: []civoNodeCountWrite{{method: "remove", key: "witself:civoNodeCount"}}},
		{name: "preview_default", cmd: "preview", want: []civoNodeCountWrite{{method: "remove", key: "witself:civoNodeCount"}}},
		{name: "preview_count", cmd: "preview", count: 3, want: []civoNodeCountWrite{{method: "set", key: "witself:civoNodeCount", value: auto.ConfigValue{Value: "3", Secret: false}}}},
		{name: "refresh", cmd: "refresh", count: 3},
		{name: "destroy", cmd: "destroy", count: 3},
		{name: "outputs", cmd: "outputs", count: 3},
		{name: "cell-health", cmd: "cell-health", count: 3},
		{name: "remove_error", cmd: "up", writerErr: errFake, prefix: "clear Civo config witself:civoNodeCount: ", want: []civoNodeCountWrite{{method: "remove", key: "witself:civoNodeCount"}}},
		{name: "set_error", cmd: "preview", count: 3, writerErr: errFake, prefix: "set config witself:civoNodeCount: ", want: []civoNodeCountWrite{{method: "set", key: "witself:civoNodeCount", value: auto.ConfigValue{Value: "3", Secret: false}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writer := &civoNodeCountWriter{err: tc.writerErr}
			err := applyCivoNodeCount(context.Background(), writer, tc.cmd, tc.count)
			if tc.writerErr == nil {
				assertCivoIngressError(t, err, "")
			} else if !errors.Is(err, errFake) || !strings.HasPrefix(err.Error(), tc.prefix) {
				t.Fatal("writer error lost its cause or exact prefix")
			}
			if !reflect.DeepEqual(writer.calls, tc.want) {
				t.Fatal("writer calls differ in method, key, value, secret bit, or count")
			}
		})
	}
}

type civoNodeCountPulumi struct {
	auto.PulumiCommand
	calls   [][]string
	stopErr error
}

func (f *civoNodeCountPulumi) Run(_ context.Context, _ string, _ io.Reader, _, _ []io.Writer, _ []string, args ...string) (string, string, int, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if len(args) > 1 {
		switch {
		case args[0] == "stack" && args[1] == "select":
			return "", "no stack named test found", 1, errors.New("fake missing stack")
		case args[0] == "stack" && args[1] == "init":
			return "", "", 0, nil
		case args[0] == "stack" && args[1] == "output":
			return "{}", "", 0, nil
		case args[0] == "config" && (args[1] == "set" || args[1] == "rm"):
			return "", "", 0, nil
		}
	}
	return "", "fake stop", 1, f.stopErr
}

func TestRunWritesCivoNodeCountStackSetting(t *testing.T) {
	for _, tc := range []struct {
		name, cmd, count string
		args             []string
	}{
		{name: "a_default_removes", cmd: "preview"},
		{name: "b_preview_sets", cmd: "preview", count: "2", args: []string{"-civo-node-count", "2"}},
		{name: "c_up_sets_count_not_quota", cmd: "up", count: "3", args: []string{"-civo-node-count", "3", "-civo-core-quota", "20"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r2TestHome(t)
			for name, value := range r2TestValues() {
				t.Setenv(name, value)
			}
			t.Setenv("CIVO_TOKEN", "test-civo-token-not-real")
			t.Setenv("TMPDIR", t.TempDir())
			r2StubCheck(t, func(context.Context, backend.R2Settings, backend.R2Secrets, backend.R2CheckOptions) (backend.R2CheckReport, error) {
				return backend.R2CheckReport{}, nil
			})
			stopErr := errors.New("fake bootstrap config stop")
			fake := &civoNodeCountPulumi{stopErr: stopErr}
			r2StubCLICommand(t, fake)
			args := []string{tc.cmd, "-cloud", "civo", "-account-alias", "spike", "-region", "nyc1", "-role", "dev", "-backend", "r2", "-r2-bucket", r2TestSettings.Bucket, "-r2-endpoint", r2TestEndpoint, "-civo-admin-cidr", "203.0.113.7/32"}
			if err := run(append(args, tc.args...)); !errors.Is(err, stopErr) {
				t.Fatal("run did not reach the fake bootstrap config read")
			}
			stack := ""
			for _, args := range fake.calls {
				if len(args) < 2 || args[0] != "stack" || args[1] != "select" {
					continue
				}
				for i := 2; i+1 < len(args); i++ {
					if args[i] == "--stack" {
						stack = args[i+1]
					}
				}
			}
			if stack == "" {
				t.Fatal("missing stack select with stack argument")
			}
			wantLast := []string{"config", "--show-secrets", "--json", "--stack", stack}
			if !reflect.DeepEqual(fake.calls[len(fake.calls)-1], wantLast) {
				t.Fatal("last command was not the exact bootstrap config read")
			}
			want := []string{"config", "rm", "witself:civoNodeCount", "--stack", stack}
			if tc.count != "" {
				want = []string{"config", "set", "--stack", stack, "witself:civoNodeCount", "--plaintext", "--", tc.count}
			}
			count := 0
			for i, args := range fake.calls {
				for _, arg := range args {
					if arg != "witself:civoNodeCount" {
						continue
					}
					count++
					if !reflect.DeepEqual(args, want) {
						t.Fatal("node-count command differs from exact expected argv")
					}
					if i == 0 || !reflect.DeepEqual(fake.calls[i-1], []string{"config", "rm", "witself:cloudflareDNS", "--stack", stack}) {
						t.Fatal("node-count command did not immediately follow the last clear key")
					}
				}
			}
			if count != 1 {
				t.Fatalf("node-count commands = %d, want exactly 1", count)
			}
		})
	}
}

func TestCivoNodeCountRunRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, cmd, cloud, want string
		args                   []string
	}{
		{name: "prod_below_default", args: []string{"-profile", "prod", "-civo-node-count", "1"}, want: "-civo-node-count 1 must be from 2 to 5 for -profile prod (0 keeps the profile's default of 2)"},
		{name: "prod_above_max", args: []string{"-profile", "prod", "-civo-node-count", "6"}, want: "-civo-node-count 6 must be from 2 to 5 for -profile prod (0 keeps the profile's default of 2)"},
		{name: "negative_count", args: []string{"-civo-node-count=-1"}, want: "-civo-node-count -1 must be from 1 to 5 for -profile minimal (0 keeps the profile's default of 1)"},
		{name: "zero_count", args: []string{"-civo-node-count", "0"}, want: "-civo-admin-cidr is required with -cloud civo"},
		{name: "five_medium", args: []string{"-civo-node-count", "5"}, want: "-civo-admin-cidr is required with -cloud civo"},
		{name: "five_large", args: []string{"-civo-node-size", "g4s.kube.large", "-civo-node-count", "5"}, want: "civo node count 5 for cell civo-sandbox-use1-1 needs 20 cores (5 x g4s.kube.large at 4 each), above the Civo core quota of 16; without -cell no other cell is counted"},
		{name: "up_five_large", cmd: "up", args: []string{"-civo-node-size", "g4s.kube.large", "-civo-node-count", "5"}, want: "civo node count 5 for cell civo-sandbox-use1-1 needs 20 cores (5 x g4s.kube.large at 4 each), above the Civo core quota of 16; without -cell no other cell is counted"},
		{name: "five_large_raised_quota", args: []string{"-civo-node-size", "g4s.kube.large", "-civo-node-count", "5", "-civo-core-quota", "20"}, want: "-civo-admin-cidr is required with -cloud civo"},
		{name: "zero_quota", args: []string{"-civo-core-quota", "0"}, want: "-civo-core-quota 0 must be from 1 to 1024"},
		{name: "quota_above_max", args: []string{"-civo-core-quota", "1025"}, want: "-civo-core-quota 1025 must be from 1 to 1024"},
		{name: "aws_count", cloud: "aws", args: []string{"-restore-archives", "-civo-node-count", "3"}, want: "-civo-node-count and -civo-core-quota apply only to -cloud civo"},
		{name: "aws_quota", cloud: "aws", args: []string{"-restore-archives", "-civo-core-quota", "20"}, want: "-civo-node-count and -civo-core-quota apply only to -cloud civo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			civoIngressTestHome(t)
			t.Setenv("DSH_HOME", t.TempDir())
			state := filepath.Join(t.TempDir(), "state")
			cmd := tc.cmd
			if cmd == "" {
				cmd = "preview"
			}
			args := []string{cmd, "-cloud", "civo", "-region", "nyc1", "-backend", "local", "-state-dir", state}
			if tc.cloud == "aws" {
				args = []string{cmd, "-cloud", "aws", "-state-dir", state}
			}
			assertCivoIngressError(t, run(append(args, tc.args...)), tc.want)
			assertCivoIngressPathAbsent(t, state)
		})
	}
}

func TestCivoNodeCountInventoryQuotaGuard(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		edit       func(*infraConfig)
		aws        bool
	}{
		{name: "a_planned_total", want: fmt.Sprintf(civoNodeCountQuotaMessage, 6, 18, 16)},
		{name: "b_equal_passes", want: "-civo-admin-cidr is required with -cloud civo", edit: func(c *infraConfig) { c.Defaults.CivoCoreQuota = civoNodeCountInt(18) }},
		{name: "c_default_inert", want: "-civo-admin-cidr is required with -cloud civo", edit: func(c *infraConfig) {
			c.Defaults.CivoCoreQuota = civoNodeCountInt(1)
			civoNodeCountSetEntry(c, civoNodeCountTarget, func(e *cellEntry) { e.CivoNodeCount = nil })
		}},
		{name: "d_account_pin_forwarded", want: "-civo-admin-cidr is required with -cloud civo", edit: func(c *infraConfig) {
			civoNodeCountPin(c, civoNodeCountTarget, "account-123")
			civoNodeCountPin(c, civoNodeCountBackup, "acct-b")
			civoNodeCountPin(c, civoNodeCountSandbox, "acct-b")
		}},
		{name: "e_defaults_do_not_reject_aws", aws: true, want: "-restore-archives requires -control-plane", edit: func(c *infraConfig) { c.Defaults.CivoCoreQuota = civoNodeCountInt(20) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			civoIngressTestHome(t)
			t.Setenv("DSH_HOME", t.TempDir())
			oldBase := civoAPIBaseURL
			t.Cleanup(func() { civoAPIBaseURL = oldBase })
			var mu sync.Mutex
			var requests []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests = append(requests, r.Method+" "+r.URL.Path)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"account_id":"account-123"}`)
			}))
			t.Cleanup(server.Close)
			civoAPIBaseURL = server.URL
			var states []string
			path := civoNodeCountConfigPath(t, func(c *infraConfig) {
				for _, name := range []string{civoNodeCountBackup, civoNodeCountSandbox, civoNodeCountTarget} {
					state := filepath.Join(t.TempDir(), "state")
					states = append(states, state)
					civoNodeCountSetEntry(c, name, func(e *cellEntry) { e.Backend, e.StateDir = strPtr("local"), strPtr(state) })
				}
				if tc.edit != nil {
					tc.edit(c)
				}
			})
			args := []string{"preview", "-cell", civoNodeCountTarget, "-config", path}
			if tc.aws {
				args = []string{"outputs", "-cell", civoNodeCountAWS, "-config", path, "-restore-archives"}
			}
			assertCivoIngressError(t, run(args), tc.want)
			for _, state := range states {
				assertCivoIngressPathAbsent(t, state)
			}
			mu.Lock()
			defer mu.Unlock()
			var wantRequests []string
			if !tc.aws {
				wantRequests = []string{"POST /v2/auth/exchange"}
			}
			if !reflect.DeepEqual(requests, wantRequests) {
				t.Fatal("identity request method/path or count differs")
			}
		})
	}
}

func TestLoadInfraConfigValidatesCivoNodeCount(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		edit       func(*infraConfig)
	}{
		{name: "count_in_defaults", want: "defaults must not set civo_node_count — a node count is a per-cell spend decision", edit: func(c *infraConfig) { c.Defaults.CivoNodeCount = civoNodeCountInt(3) }},
		{name: "count_on_aws", want: `cell "aws-sandbox-usw2-dev": civo_node_count applies only to cloud civo`, edit: func(c *infraConfig) {
			civoNodeCountSetEntry(c, civoNodeCountAWS, func(e *cellEntry) { e.CivoNodeCount = civoNodeCountInt(3) })
		}},
		{name: "quota_on_aws", want: `cell "aws-sandbox-usw2-dev": civo_core_quota is an account setting; record it under defaults, not on a cell`, edit: func(c *infraConfig) {
			civoNodeCountSetEntry(c, civoNodeCountAWS, func(e *cellEntry) { e.CivoCoreQuota = civoNodeCountInt(20) })
		}},
		{name: "quota_on_civo", want: `cell "civo-sandbox-use1-backup": civo_core_quota is an account setting; record it under defaults, not on a cell`, edit: func(c *infraConfig) {
			civoNodeCountSetEntry(c, civoNodeCountBackup, func(e *cellEntry) { e.CivoCoreQuota = civoNodeCountInt(20) })
		}},
		{name: "prod_count_too_low", want: `cell "civo-prod-use1-serving": civo_node_count 1 must be from 2 to 5 for profile prod`, edit: func(c *infraConfig) {
			civoNodeCountSetEntry(c, civoNodeCountTarget, func(e *cellEntry) { e.CivoNodeCount = civoNodeCountInt(1) })
		}},
		{name: "prod_count_too_high", want: `cell "civo-prod-use1-serving": civo_node_count 6 must be from 2 to 5 for profile prod`, edit: func(c *infraConfig) {
			civoNodeCountSetEntry(c, civoNodeCountTarget, func(e *cellEntry) { e.CivoNodeCount = civoNodeCountInt(6) })
		}},
		{name: "zero_not_an_inventory_default", want: `cell "civo-sandbox-use1-backup": civo_node_count 0 must be from 1 to 5 for profile minimal`, edit: func(c *infraConfig) {
			civoNodeCountSetEntry(c, civoNodeCountBackup, func(e *cellEntry) { e.CivoNodeCount = civoNodeCountInt(0) })
		}},
		{name: "inherited_profile", want: `cell "civo-sandbox-use1-backup": civo_node_count 1 must be from 2 to 5 for profile prod`, edit: func(c *infraConfig) {
			c.Defaults.Profile = strPtr("prod")
			civoNodeCountSetEntry(c, civoNodeCountBackup, func(e *cellEntry) { e.CivoNodeCount = civoNodeCountInt(1) })
		}},
		{name: "quota_too_low", want: "defaults: civo_core_quota 0 must be from 1 to 1024", edit: func(c *infraConfig) { c.Defaults.CivoCoreQuota = civoNodeCountInt(0) }},
		{name: "quota_too_high", want: "defaults: civo_core_quota 1025 must be from 1 to 1024", edit: func(c *infraConfig) { c.Defaults.CivoCoreQuota = civoNodeCountInt(1025) }},
		{name: "valid", edit: func(c *infraConfig) { c.Defaults.CivoCoreQuota = civoNodeCountInt(32) }},
		{name: "sorted_first_error_and_quota_before_cloud", want: `cell "aws-sandbox-usw2-dev": civo_core_quota is an account setting; record it under defaults, not on a cell`, edit: func(c *infraConfig) {
			civoNodeCountSetEntry(c, civoNodeCountAWS, func(e *cellEntry) { e.CivoCoreQuota, e.CivoNodeCount = civoNodeCountInt(20), civoNodeCountInt(3) })
			civoNodeCountSetEntry(c, civoNodeCountTarget, func(e *cellEntry) { e.CivoNodeCount = civoNodeCountInt(1) })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := civoNodeCountConfigPath(t, tc.edit)
			_, _, err := loadInfraConfig(path)
			want := tc.want
			if want != "" {
				want = path + ": " + want
			}
			assertCivoIngressError(t, err, want)
		})
	}
	t.Run("non_integer", func(t *testing.T) {
		path := writeConfig(t, strings.Replace(civoNodeCountFixture, "civo_node_count: 3", "civo_node_count: three", 1))
		_, _, err := loadInfraConfig(path)
		if err == nil || !strings.HasPrefix(err.Error(), "parse ") {
			t.Fatal("non-integer node count did not return a parse error")
		}
	})
	for _, key := range []string{"civo_node_count", "civo_core_quota"} {
		for _, value := range []struct{ name, yaml string }{
			{"fractional_count", "3.5"},
			{"fractional_quota", "16.9"},
			{"whole_float", "3.0"},
			{"quoted_integer", `"3"`},
			{"boolean", "true"},
			{"null", "null"},
			{"sequence", "[3]"},
			{"mapping", "{count: 3}"},
		} {
			for _, location := range []string{"cell", "defaults"} {
				t.Run(key+"/"+value.name+"/"+location, func(t *testing.T) {
					fixture := civoNodeCountFixture
					context := `cell "civo-prod-use1-serving"`
					if location == "defaults" {
						fixture = strings.Replace(fixture, "defaults:\n", "defaults:\n  "+key+": "+value.yaml+"\n", 1)
						context = "defaults"
					} else {
						fixture = strings.Replace(fixture, "civo_node_count: 3", key+": "+value.yaml, 1)
					}
					path := writeConfig(t, fixture)
					_, _, err := loadInfraConfig(path)
					assertCivoIngressError(t, err, "parse "+path+": "+context+": "+key+" must be an integer")
				})
			}
		}
	}
}

func TestCivoNodeCountConfigRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name, target, wantCount, wantQuota string
		args                               []string
		withoutKeys                        bool
	}{
		{name: "recorded_count_inherited_quota", target: civoNodeCountTarget, wantCount: "3", wantQuota: "20"},
		{name: "explicit_count_wins", target: civoNodeCountTarget, args: []string{"-civo-node-count", "4"}, wantCount: "4", wantQuota: "20"},
		{name: "explicit_zero_wins", target: civoNodeCountTarget, args: []string{"-civo-node-count", "0"}, wantCount: "0", wantQuota: "20"},
		{name: "unset_count_inherited_quota", target: civoNodeCountBackup, wantCount: "0", wantQuota: "20"},
		{name: "no_keys", target: civoNodeCountTarget, withoutKeys: true, wantCount: "0", wantQuota: "16"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := civoNodeCountConfigPath(t, func(c *infraConfig) {
				if tc.withoutKeys {
					civoNodeCountSetEntry(c, civoNodeCountTarget, func(e *cellEntry) { e.CivoNodeCount = nil })
				} else {
					c.Defaults.CivoCoreQuota = civoNodeCountInt(20)
				}
			})
			fs := newTestFlagSet()
			fs.Int("civo-node-count", 0, "")
			fs.Int("civo-core-quota", 16, "")
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal("parse test flags")
			}
			if err := applyCellConfig(fs, tc.target, path); err != nil {
				t.Fatal("apply node-count inventory")
			}
			if fs.Lookup("civo-node-count").Value.String() != tc.wantCount || fs.Lookup("civo-core-quota").Value.String() != tc.wantQuota {
				t.Fatal("node-count or quota precedence differs")
			}
		})
	}
}

func TestConfigAddCellRefusesNodeCountFlags(t *testing.T) {
	const want = "-civo-node-count and -civo-core-quota are not recorded by config add-cell; a new cell starts at its profile's node count. Record civo_node_count on the cell's entry, or civo_core_quota under defaults, by hand (see the README)"
	for _, args := range [][]string{{"-civo-node-count", "3"}, {"-civo-core-quota", "20"}} {
		t.Run(args[0], func(t *testing.T) {
			civoIngressTestHome(t)
			t.Setenv("DSH_HOME", t.TempDir())
			dir := t.TempDir()
			path := filepath.Join(dir, "infra.yaml")
			tokenPath := filepath.Join(dir, "civo.token")
			if err := os.WriteFile(tokenPath, []byte("test-token\n"), 0o600); err != nil {
				t.Fatal("write fake token fixture")
			}
			fs := newTestFlagSet()
			fs.Int("civo-node-count", 0, "")
			fs.Int("civo-core-quota", 16, "")
			base := []string{"-cloud", "civo", "-account-alias", "fixture", "-region", "nyc1", "-role", "dev", "-backend", "local", "-civo-admin-cidr", "203.0.113.7/32", "-civo-node-size", "g4s.kube.medium", "-civo-token-file", tokenPath, "-civo-expected-account-id", "account-123", "-control-plane", "https://self.example.com", "-argocd", "-backup-validation-target"}
			if err := fs.Parse(append(base, args...)); err != nil {
				t.Fatal("parse add-cell fixture flags")
			}
			assertCivoIngressError(t, configAddCell(fs, path), want)
			assertCivoIngressPathAbsent(t, path)
		})
	}
}

func TestCivoNodeCountSettingsRow(t *testing.T) {
	entry := cellEntry{Cloud: strPtr("civo"), Profile: strPtr("prod")}
	base := effectiveSettings(entry, nil)
	profileIndex, sizeIndex := -1, -1
	for i, row := range base {
		switch row.key {
		case "profile":
			profileIndex = i
			if row.value != "prod (2 nodes)" {
				t.Fatal("unconfigured prod label changed")
			}
		case "node size":
			sizeIndex = i
		case "node count":
			t.Fatal("unset count produced a node-count row")
		}
	}
	if profileIndex < 0 || sizeIndex < 0 {
		t.Fatal("missing profile or node size row")
	}
	for _, tc := range []struct {
		profile, label string
		count          int
	}{{"prod", "prod", 3}, {"minimal", "minimal development", 2}} {
		t.Run(tc.profile, func(t *testing.T) {
			entry.Profile, entry.CivoNodeCount = strPtr(tc.profile), civoNodeCountInt(tc.count)
			want := append([]settingRow(nil), base...)
			want[profileIndex].value = tc.label
			want = append(want[:sizeIndex+1], append([]settingRow{{key: "node count", value: strconv.Itoa(tc.count), fromEntry: true}}, want[sizeIndex+1:]...)...)
			if !reflect.DeepEqual(effectiveSettings(entry, nil), want) {
				t.Fatal("settings rows differ beyond the profile label and adjacent recorded count")
			}
		})
	}
}

func TestCivoNodeCountIsDocumented(t *testing.T) {
	usageLines := strings.Split(usage, "\n")
	for _, want := range []string{
		"  -civo-node-count Civo node count, up to 5; 0 keeps the profile's default",
		"                  (1 for minimal, 2 for prod). Above the default adds monthly",
		"                  spend. With -cell an explicit 0 overrides civo_node_count",
		"  -civo-core-quota Civo account CPU core quota for the node-count check",
		"                  (default 16; inventory key defaults.civo_core_quota)",
	} {
		found := false
		for _, line := range usageLines {
			if line == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("usage missing exact whole line %q", want)
		}
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal("read infra README")
	}
	lines := strings.Split(string(readme), "\n")
	hasLine := func(want string, prefix bool) bool {
		for _, line := range lines {
			if line == want || (prefix && strings.HasPrefix(line, want)) {
				return true
			}
		}
		return false
	}
	if !hasLine("### Civo node count and core quota (opt-in)", false) {
		t.Error("README missing node-count section")
	}
	for _, prefix := range []string{"| `civo_node_count` |", "| `civo_core_quota` |"} {
		if !hasLine(prefix, true) {
			t.Errorf("README missing key-table row %q", prefix)
		}
	}
	for size, cores := range civoNodeCores {
		if !hasLine(fmt.Sprintf("| `%s` | %d |", size, cores), true) {
			t.Errorf("README missing correct core-table row for %q", size)
		}
	}
	for _, obsolete := range []string{"The node count still comes from the", "fixed two-node", "fixed node counts"} {
		if strings.Contains(string(readme), obsolete) {
			t.Errorf("README retains obsolete wording %q", obsolete)
		}
	}
	runbooks, err := os.ReadFile("../../../../docs/runbooks.md")
	if err != nil {
		t.Fatal("read runbook")
	}
	countIndex, emailIndex := -1, -1
	for i, line := range strings.Split(string(runbooks), "\n") {
		if line == "## Change a Civo cell's node count" {
			countIndex = i
		}
		if strings.HasPrefix(line, "## Move and stage the `witmail.net` ") {
			emailIndex = i
		}
	}
	if countIndex < 0 || emailIndex < 0 || countIndex >= emailIndex {
		t.Error("node-count runbook missing or after the managed-email section")
	}
}

func TestCivoNodeCountFlagsError(t *testing.T) {
	errFake := errors.New("fake inventory error")
	for _, cmd := range []string{"refresh", "destroy", "outputs", "cell-health", "bootstrap", "state-check"} {
		t.Run("inert_"+cmd, func(t *testing.T) {
			calls := 0
			err := civoNodeCountFlagsError(cmd, civoNodeCountTarget, "", "prod", "g4s.kube.large", 9, 0, func() (*infraConfig, error) {
				calls++
				return nil, errFake
			})
			assertCivoIngressError(t, err, "")
			if calls != 0 {
				t.Fatal("non-provisioning command read inventory")
			}
		})
	}
	for _, tc := range []struct {
		name, cmd, want string
		count, quota    int
		loaderKind      string
		wantCalls       int
		wantCause       bool
	}{
		{name: "count_range_before_inventory", cmd: "preview", count: 1, quota: 16, loaderKind: "error", want: "-civo-node-count 1 must be from 2 to 5 for -profile prod (0 keeps the profile's default of 2)"},
		{name: "quota_range_before_inventory", cmd: "preview", count: 3, quota: 0, loaderKind: "error", want: "-civo-core-quota 0 must be from 1 to 1024"},
		{name: "default_no_inventory", cmd: "up", count: 2, quota: 1, loaderKind: "error"},
		{name: "zero_no_inventory", cmd: "up", count: 0, quota: 1, loaderKind: "error"},
		{name: "loader_error_unchanged", cmd: "preview", count: 3, quota: 16, loaderKind: "error", wantCalls: 1, wantCause: true},
		{name: "loaded_inventory_guard", cmd: "preview", count: 3, quota: 16, loaderKind: "fixture", wantCalls: 1, want: fmt.Sprintf(civoNodeCountQuotaMessage, 6, 18, 16)},
		{name: "nil_inventory_guard", cmd: "preview", count: 5, quota: 16, want: civoNodeCountBareQuotaMessage},
		{name: "up_loaded_inventory_guard", cmd: "up", count: 3, quota: 16, loaderKind: "fixture", wantCalls: 1, want: fmt.Sprintf(civoNodeCountQuotaMessage, 6, 18, 16)},
		{name: "up_nil_inventory_guard", cmd: "up", count: 5, quota: 16, want: civoNodeCountBareQuotaMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var loader func() (*infraConfig, error)
			if tc.loaderKind != "" {
				loader = func() (*infraConfig, error) {
					calls++
					if tc.loaderKind == "error" {
						return nil, errFake
					}
					cfg, _, err := loadInfraConfig(writeConfig(t, civoNodeCountFixture))
					return cfg, err
				}
			}
			err := civoNodeCountFlagsError(tc.cmd, civoNodeCountTarget, "", "prod", "g4s.kube.large", tc.count, tc.quota, loader)
			if tc.wantCause {
				if !errors.Is(err, errFake) || err != errFake {
					t.Fatal("inventory loader error was not returned unchanged")
				}
			} else {
				assertCivoIngressError(t, err, tc.want)
			}
			if calls != tc.wantCalls {
				t.Fatalf("inventory loader calls = %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}
