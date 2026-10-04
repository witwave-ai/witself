package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/witwave-ai/witself/infra/pulumi/internal/backend"
)

const r2TestCell = "civo-spike-use1-dev"
const r2TestEndpoint = "https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com"

var r2TestSettings = backend.R2Settings{Bucket: "witself-state-test", Endpoint: r2TestEndpoint}

func r2TestValues() map[string]string {
	return map[string]string{backend.R2AccessKeyIDEnv: "test-access-key-id-not-real", backend.R2SecretAccessKeyEnv: "test-secret-access-key-not-real", backend.R2StatePassphraseEnv: "test-passphrase-not-real-0001"}
}
func r2TestSecrets(t *testing.T) backend.R2Secrets {
	t.Helper()
	v := r2TestValues()
	s, err := backend.R2SecretsFromEnv(func(k string) string { return v[k] })
	if err != nil {
		t.Fatal("fake secret validation failed")
	}
	return s
}
func r2TestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", home)
	t.Setenv("DSH_HOME", home)
	t.Setenv("CIVO_TOKEN", "")
	t.Setenv("CLOUDFLARE_API_TOKEN", "")
	t.Setenv("PULUMI_CONFIG_PASSPHRASE", "")
	for n := range r2TestValues() {
		t.Setenv(n, "")
	}
	return home
}
func r2TestError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected refusal")
	}
	for _, v := range r2TestValues() {
		if strings.Contains(err.Error(), v) {
			t.Fatal("error disclosed fake secret")
		}
	}
	if err.Error() != want {
		t.Fatalf("message mismatch; expected %q", want)
	}
}
func r2EmptyHome(t *testing.T, home string) {
	t.Helper()
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatal("unexpected local file or directory")
	}
}
func r2StubCLI(t *testing.T, minor, patch uint64, seamErr error) {
	t.Helper()
	old := r2PulumiCLI
	t.Cleanup(func() { r2PulumiCLI = old })
	r2PulumiCLI = func() (auto.PulumiCommand, uint64, uint64, uint64, error) { return nil, 3, minor, patch, seamErr }
}
func r2StubCLICommand(t *testing.T, command auto.PulumiCommand) {
	t.Helper()
	old := r2PulumiCLI
	t.Cleanup(func() { r2PulumiCLI = old })
	r2PulumiCLI = func() (auto.PulumiCommand, uint64, uint64, uint64, error) { return command, 3, 265, 0, nil }
}
func r2StubCheck(t *testing.T, fn func(context.Context, backend.R2Settings, backend.R2Secrets, backend.R2CheckOptions) (backend.R2CheckReport, error)) {
	t.Helper()
	old := r2CheckState
	t.Cleanup(func() { r2CheckState = old })
	r2CheckState = fn
}

func TestCivoBackendRefusals(t *testing.T) {
	for _, kind := range []string{"s3", "gcs", "azblob"} {
		t.Run(kind, func(t *testing.T) {
			r2TestHome(t)
			r2TestError(t, run([]string{"preview", "-cloud", "civo", "-region", "nyc1", "-backend", kind}), "-cloud civo requires -backend local or r2")
		})
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"wrong cloud", []string{"preview", "-cloud", "aws", "-backend", "r2", "-r2-bucket", r2TestSettings.Bucket, "-r2-endpoint", r2TestEndpoint}, "-backend r2 is only implemented for -cloud civo"},
		{"wrong backend", []string{"preview", "-cloud", "civo", "-region", "nyc1", "-backend", "local", "-r2-bucket", "x"}, "-r2-bucket and -r2-endpoint apply only to -backend r2"},
		{"state directory", []string{"preview", "-cloud", "civo", "-region", "nyc1", "-backend", "r2", "-r2-bucket", r2TestSettings.Bucket, "-r2-endpoint", r2TestEndpoint, "-state-dir", t.TempDir()}, "-state-dir does not apply to -backend r2 (inventory key state_dir)"},
		{"wrong check backend", []string{"state-check", "-cloud", "civo", "-region", "nyc1", "-backend", "local"}, "state-check is only implemented for -backend r2"},
	} {
		t.Run(tc.name, func(t *testing.T) { r2TestHome(t); r2TestError(t, run(tc.args), tc.want) })
	}
}
func TestR2RunRefusesMissingOrMalformedInput(t *testing.T) {
	valid := []string{"-r2-bucket", r2TestSettings.Bucket, "-r2-endpoint", r2TestEndpoint}
	missing := func(names string) string {
		return "state backend r2: missing environment variable(s): " + names + "; export them in the shell that runs witself-infra. They are never read from infra.yaml, a flag or a file, and the passphrase is never generated"
	}
	for _, tc := range []struct {
		name                  string
		flags                 []string
		values                bool
		variable, value, want string
	}{
		{name: "a", want: "-backend r2 requires -r2-bucket (inventory key r2_bucket)"},
		{name: "b", flags: valid[:2], want: "-backend r2 requires -r2-endpoint (inventory key r2_endpoint)"},
		{name: "c", flags: []string{"-r2-bucket", "Witself-State", "-r2-endpoint", r2TestEndpoint}, want: "-r2-bucket must be 3-63 characters of lowercase letters, digits and hyphens, and must not begin or end with a hyphen"},
		{name: "d", flags: []string{"-r2-bucket", r2TestSettings.Bucket, "-r2-endpoint", strings.Replace(r2TestEndpoint, "https:", "http:", 1)}, want: "-r2-endpoint must be https://<account-id>.r2.cloudflarestorage.com, or the eu, us or fedramp form of that host, with no path, query, port or credentials"},
		{name: "e", flags: valid, want: missing(backend.R2AccessKeyIDEnv + ", " + backend.R2SecretAccessKeyEnv + ", " + backend.R2StatePassphraseEnv)},
		{name: "f", flags: valid, values: true, variable: backend.R2StatePassphraseEnv, want: missing(backend.R2StatePassphraseEnv)},
		{name: "g", flags: valid, values: true, variable: backend.R2AccessKeyIDEnv, value: r2TestValues()[backend.R2AccessKeyIDEnv] + " ", want: "state backend r2: environment variable WITSELF_INFRA_R2_ACCESS_KEY_ID has leading or trailing whitespace; export the exact value"},
		{name: "h", flags: valid, values: true, variable: backend.R2SecretAccessKeyEnv, value: strings.Repeat("x", 15), want: "state backend r2: environment variable WITSELF_INFRA_R2_SECRET_ACCESS_KEY is shorter than 16 characters; it does not look like an R2 key"},
		{name: "i", flags: valid, values: true, variable: backend.R2StatePassphraseEnv, value: strings.Repeat("x", 19), want: "state backend r2: WITSELF_INFRA_STATE_PASSPHRASE is shorter than 20 characters"},
		{name: "j", flags: []string{"-r2-bucket", r2TestValues()[backend.R2AccessKeyIDEnv], "-r2-endpoint", r2TestEndpoint}, values: true, want: "state backend r2: -r2-bucket or -r2-endpoint contains the value of WITSELF_INFRA_R2_ACCESS_KEY_ID; the inventory and the flags hold names and endpoints, never credential values"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := r2TestHome(t)
			if tc.values {
				for n, v := range r2TestValues() {
					t.Setenv(n, v)
				}
			}
			if tc.variable != "" {
				t.Setenv(tc.variable, tc.value)
			}
			args := append([]string{"preview", "-cloud", "civo", "-region", "nyc1", "-backend", "r2"}, tc.flags...)
			r2TestError(t, run(args), tc.want)
			r2EmptyHome(t, home)
		})
	}
}
func TestLocalCellsIgnoreR2Variables(t *testing.T) {
	home := r2TestHome(t)
	for n, v := range r2TestValues() {
		t.Setenv(n, v)
	}
	dir := filepath.Join(home, "state")
	if err := run([]string{"bootstrap", "-cloud", "civo", "-region", "nyc1", "-backend", "local", "-state-dir", dir}); err != nil {
		t.Fatal("local bootstrap failed")
	}
	path := filepath.Join(dir, "passphrase")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("passphrase mode differs")
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(data)) == r2TestValues()[backend.R2StatePassphraseEnv] {
		t.Fatal("R2 passphrase reached local backend")
	}
}
func TestResolveR2BackendIsSilentForOtherBackends(t *testing.T) {
	for _, kind := range []string{"s3", "gcs", "azblob", "local"} {
		s, secrets, err := resolveR2Backend(kind, "aws", "", "", "", func(string) string { t.Fatal("unexpected environment read"); return "" })
		if err != nil || s != (backend.R2Settings{}) || !reflect.DeepEqual(secrets, backend.R2Secrets{}) {
			t.Fatal("non-R2 resolution changed")
		}
	}
}
func TestCheckR2PulumiVersion(t *testing.T) {
	for _, v := range [][3]uint64{{3, 262, 9}, {0, 0, 0}, {3, 263, 0}, {3, 265, 0}, {4, 0, 0}} {
		err := checkR2PulumiVersion(v[0], v[1], v[2])
		if v[0] == 0 || v[1] == 262 {
			r2TestError(t, err, fmt.Sprintf("state backend r2 needs Pulumi CLI 3.263.0 or newer on PATH; found %d.%d.%d", v[0], v[1], v[2]))
		} else if err != nil {
			t.Fatal("supported version refused")
		}
	}
}
func TestR2PulumiVersionWrapsSeamError(t *testing.T) {
	cause := errors.New("fake CLI unavailable")
	r2StubCLI(t, 265, 0, cause)
	_, _, _, _, err := r2PulumiVersion()
	r2TestError(t, err, "state backend r2: cannot run the Pulumi CLI: fake CLI unavailable")
	if !errors.Is(err, cause) {
		t.Fatal("seam error chain lost")
	}
}
func TestPrepareR2Workspace(t *testing.T) {
	for _, cmd := range []string{"up", "preview", "refresh", "destroy", "outputs", "cell-health"} {
		t.Run(cmd, func(t *testing.T) {
			r2StubCLI(t, 265, 0, nil)
			calls := 0
			r2StubCheck(t, func(_ context.Context, s backend.R2Settings, secrets backend.R2Secrets, o backend.R2CheckOptions) (backend.R2CheckReport, error) {
				calls++
				if s != r2TestSettings || !reflect.DeepEqual(secrets, r2TestSecrets(t)) || o != (backend.R2CheckOptions{Project: projectName, Stack: r2TestCell}) {
					t.Fatal("check inputs differ")
				}
				return backend.R2CheckReport{}, nil
			})
			var out bytes.Buffer
			env, _, err := prepareR2Workspace(context.Background(), cmd, r2TestSettings, r2TestSecrets(t), r2TestCell, &out)
			if err != nil || !reflect.DeepEqual(env, backend.R2WorkspaceEnv(r2TestSettings, r2TestSecrets(t))) {
				t.Fatal("workspace differs")
			}
			wantCalls := 1
			want := "state backend r2: bucket witself-state-test verified (list, write, read, delete)\n"
			if cmd == "outputs" || cmd == "cell-health" {
				wantCalls = 0
				want = ""
			}
			if calls != wantCalls || out.String() != want {
				t.Fatal("probe routing or success line differs")
			}
		})
	}
	for _, kind := range []string{"old", "cli error", "check error"} {
		t.Run(kind, func(t *testing.T) {
			minor := uint64(265)
			var cliErr error
			if kind == "old" {
				minor = 262
			}
			if kind == "cli error" {
				cliErr = errors.New("fake CLI unavailable")
			}
			r2StubCLI(t, minor, 0, cliErr)
			calls := 0
			r2StubCheck(t, func(context.Context, backend.R2Settings, backend.R2Secrets, backend.R2CheckOptions) (backend.R2CheckReport, error) {
				calls++
				return backend.R2CheckReport{}, errors.New("fake check failed")
			})
			var out bytes.Buffer
			env, _, err := prepareR2Workspace(context.Background(), "up", r2TestSettings, r2TestSecrets(t), r2TestCell, &out)
			if err == nil || env != nil || out.Len() != 0 {
				t.Fatal("failure returned environment or success output")
			}
			if (kind == "check error") != (calls == 1) {
				t.Fatal("version failure reached check")
			}
		})
	}
}

const r2CheckHeaders = "cell:              civo-spike-use1-dev\nbackend:           r2\nbucket:            witself-state-test\nendpoint:          " + r2TestEndpoint + "\nvariables:         3 of 3 present\npulumi cli:        3.265.0\n"
const r2CheckResults = "list:              ok\nwrite probe:       ok\nread probe:        ok\nlist after write:  ok\ndelete probe:      ok\nlist after delete: ok\nstack file:        absent\nstack locks:       0\nhistory objects:   0\nbackup objects:    0\nprobe leftovers:   0\nok\n"

func TestRunStateCheckOutput(t *testing.T) {
	for _, kind := range []string{"empty", "lock", "check error", "old", "cli error"} {
		t.Run(kind, func(t *testing.T) {
			minor, patch := uint64(265), uint64(0)
			var cliErr error
			if kind == "old" {
				// A non-zero patch proves the refusal prints the real version.
				minor, patch = 262, 9
			}
			if kind == "cli error" {
				cliErr = errors.New("fake CLI unavailable")
			}
			r2StubCLI(t, minor, patch, cliErr)
			calls := 0
			r2StubCheck(t, func(_ context.Context, _ backend.R2Settings, _ backend.R2Secrets, o backend.R2CheckOptions) (backend.R2CheckReport, error) {
				calls++
				if o != (backend.R2CheckOptions{Project: projectName, Stack: r2TestCell, Inventory: true}) {
					t.Fatal("inventory options differ")
				}
				if kind == "check error" {
					return backend.R2CheckReport{}, errors.New("fake check failed")
				}
				r := backend.R2CheckReport{StackLocks: "0", HistoryObjects: "0", BackupObjects: "0", ProbeLeftovers: "0"}
				if kind == "lock" {
					r.StackLocks = "1"
					r.StackFile = true
				}
				return r, nil
			})
			var out bytes.Buffer
			err := runStateCheck(context.Background(), &out, r2TestSettings, r2TestSecrets(t), r2TestCell)
			want := r2CheckHeaders + r2CheckResults
			switch kind {
			case "lock":
				want = strings.Replace(want, "stack locks:       0", "stack locks:       1 (an operation is running, or a crashed run left a lock; see the README)", 1)
				want = strings.Replace(want, "stack file:        absent", "stack file:        present", 1)
			case "check error":
				want = r2CheckHeaders
				r2TestError(t, err, "fake check failed")
			case "old":
				want = strings.Replace(r2CheckHeaders, "3.265.0", "3.262.9", 1)
				r2TestError(t, err, "state backend r2 needs Pulumi CLI 3.263.0 or newer on PATH; found 3.262.9")
			case "cli error":
				want = ""
				r2TestError(t, err, "state backend r2: cannot run the Pulumi CLI: fake CLI unavailable")
			}
			if out.String() != want {
				t.Fatalf("output mismatch; expected %q", want)
			}
			if (kind == "old" || kind == "cli error") && calls != 0 {
				t.Fatal("version failure reached check")
			}
			if (kind == "empty" || kind == "lock") && err != nil {
				t.Fatal("successful check returned error")
			}
		})
	}
}
func TestRunR2BootstrapWritesNoLocalFile(t *testing.T) {
	home := r2TestHome(t)
	calls := 0
	r2StubCheck(t, func(_ context.Context, _ backend.R2Settings, _ backend.R2Secrets, o backend.R2CheckOptions) (backend.R2CheckReport, error) {
		calls++
		if o != (backend.R2CheckOptions{Project: projectName, Stack: r2TestCell}) {
			t.Fatal("bootstrap options differ")
		}
		return backend.R2CheckReport{}, nil
	})
	var out bytes.Buffer
	if err := runR2Bootstrap(context.Background(), &out, r2TestSettings, r2TestSecrets(t), r2TestCell); err != nil {
		t.Fatal("bootstrap failed")
	}
	want := "Civo R2 state backend ready: bucket witself-state-test at " + r2TestEndpoint + " (the owner creates the bucket; witself-infra wrote and deleted one probe object)\n"
	if out.String() != want || calls != 1 {
		t.Fatal("bootstrap output or calls differ")
	}
	r2EmptyHome(t, home)
}

type r2FakePulumi struct {
	auto.PulumiCommand
	t                      *testing.T
	missing, outputFailure bool
	outputStderr           string
	outputJSON             string
	stopErr                error
	selectErr              error
	calls                  [][]string
	env                    []string
}

func (f *r2FakePulumi) Run(_ context.Context, _ string, _ io.Reader, _, _ []io.Writer, env []string, args ...string) (string, string, int, error) {
	f.calls = append(f.calls, append([]string{}, args...))
	f.env = append([]string{}, env...)
	switch {
	case len(args) > 1 && args[0] == "stack" && args[1] == "select":
		if f.selectErr != nil {
			return "", "fake selection failure", 1, f.selectErr
		}
		if f.missing {
			return "", "no stack named test found", 1, errors.New("fake command failed")
		}
		return "", "", 0, nil
	case len(args) > 1 && args[0] == "stack" && args[1] == "init":
		return "", "", 0, nil
	case len(args) > 1 && args[0] == "stack" && args[1] == "output" && f.outputFailure:
		var values []string
		for _, v := range r2TestValues() {
			values = append(values, v)
		}
		if f.outputStderr != "" {
			values = append(values, f.outputStderr)
		}
		return "", strings.Join(values, " "), 1, errors.New("fake output failed")
	case len(args) > 1 && args[0] == "stack" && args[1] == "output" && f.outputJSON != "":
		return f.outputJSON, "", 0, nil
	case len(args) > 0 && args[0] == "config" && f.stopErr != nil:
		return "", "fake stop", 1, f.stopErr
	default:
		f.t.Fatal("unexpected fake Pulumi command")
		return "", "", 1, errors.New("unexpected command")
	}
}
func TestOpenCellStackSelectOnlyOnR2(t *testing.T) {
	for _, tc := range []struct {
		kind, cmd string
		creates   bool
	}{{"r2", "destroy", false}, {"r2", "refresh", false}, {"r2", "outputs", false}, {"r2", "cell-health", false}, {"r2", "up", true}, {"r2", "preview", true}, {"local", "outputs", true}} {
		t.Run(tc.kind+tc.cmd, func(t *testing.T) {
			f := &r2FakePulumi{t: t, missing: true}
			_, err := openCellStack(context.Background(), tc.kind, tc.cmd, r2TestCell, r2TestSettings.Bucket, auto.WorkDir(t.TempDir()), auto.Pulumi(f), auto.EnvVars(backend.R2WorkspaceEnv(r2TestSettings, r2TestSecrets(t))))
			want := [][]string{{"stack", "select", "--stack", r2TestCell}}
			if tc.creates {
				want = append(want, []string{"stack", "init", r2TestCell})
				if err != nil {
					t.Fatal("stack creation failed")
				}
			} else {
				r2TestError(t, err, fmt.Sprintf("state backend r2: cell %q has no stack in bucket %q; %s never creates one. Check r2_bucket and r2_endpoint, or run preview first for a new cell", r2TestCell, r2TestSettings.Bucket, tc.cmd))
			}
			if !reflect.DeepEqual(f.calls, want) {
				t.Fatal("stack command sequence differs")
			}
			for _, entry := range []string{"PULUMI_BACKEND_URL=" + backend.R2StateURL(r2TestSettings), "PULUMI_DIY_BACKEND_LEGACY_LAYOUT=false", "AWS_SESSION_TOKEN="} {
				found := false
				for _, v := range f.env {
					if v == entry {
						found = true
					}
				}
				if !found {
					t.Fatal("required workspace environment entry missing")
				}
			}
		})
	}
}
func TestR2InventoryRoundTrip(t *testing.T) {
	home := r2TestHome(t)
	path := filepath.Join(home, "inventory.yaml")
	capture, captureErr := os.CreateTemp(t.TempDir(), "stdout")
	if captureErr != nil {
		t.Fatal(captureErr)
	}
	defer func() { _ = capture.Close() }()
	oldOut := os.Stdout
	os.Stdout = capture
	defer func() { os.Stdout = oldOut }()
	err := run([]string{"config", "add-cell", "-config", path, "-cloud", "civo", "-account-alias", "spike", "-region", "nyc1", "-role", "dev", "-backend", "r2", "-r2-bucket", r2TestSettings.Bucket, "-r2-endpoint", r2TestEndpoint, "-civo-admin-cidr", "203.0.113.7/32"})
	os.Stdout = oldOut
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capture.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	printed, err := io.ReadAll(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(printed), "try: witself-infra preview -cell "+r2TestCell+"\nfirst: witself-infra state-check -cell "+r2TestCell+"\n") {
		t.Fatal("recorded-cell next steps differ")
	}
	cfg, _, err := loadInfraConfig(path)
	if err != nil || cfg.Cells[r2TestCell].R2Bucket == nil {
		t.Fatal("round trip load failed")
	}
	fs := newTestFlagSet()
	if err := applyCellConfig(fs, r2TestCell, path); err != nil {
		t.Fatal(err)
	}
	if fs.Lookup("r2-bucket").Value.String() != r2TestSettings.Bucket || fs.Lookup("r2-endpoint").Value.String() != r2TestEndpoint {
		t.Fatal("R2 flags failed to resolve")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"backend: r2", "r2_bucket:", "r2_endpoint:"} {
		if !strings.Contains(string(raw), key) {
			t.Fatal("inventory field missing")
		}
	}
	if strings.Contains(string(raw), "state_dir:") {
		t.Fatal("local state recorded")
	}
}
func TestR2InventoryLoaderRefusals(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"default bucket", "defaults:\n  r2_bucket: abc\ncells: {}\n", "defaults must not set r2_bucket/r2_endpoint — stack addressing is per-cell only"},
		{"default endpoint", "defaults:\n  r2_endpoint: endpoint\ncells: {}\n", "defaults must not set r2_bucket/r2_endpoint — stack addressing is per-cell only"},
		{"cloud", "cells:\n  test:\n    backend: r2\n    cloud: aws\n", `cell "test": backend r2 is only implemented for cloud civo`},
		{"directory", "cells:\n  test:\n    backend: r2\n    cloud: civo\n    state_dir: local\n", `cell "test": state_dir does not apply to backend r2`},
		{"missing", "cells:\n  test:\n    backend: r2\n    cloud: civo\n", `cell "test": backend r2 requires r2_bucket and r2_endpoint`},
		{"invalid", "cells:\n  test:\n    backend: r2\n    cloud: civo\n    r2_bucket: X\n    r2_endpoint: endpoint\n", `cell "test": -r2-bucket must be 3-63 characters of lowercase letters, digits and hyphens, and must not begin or end with a hyphen`},
		{"other backend", "cells:\n  test:\n    backend: local\n    r2_bucket: abc\n", `cell "test": r2_bucket/r2_endpoint apply only to backend r2`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, "version: 1\n"+tc.body)
			_, _, err := loadInfraConfig(path)
			r2TestError(t, err, path+": "+tc.want)
		})
	}
}
func TestR2SettingsRows(t *testing.T) {
	for _, kind := range []string{"r2", "local"} {
		for _, set := range []bool{false, true} {
			e := cellEntry{Backend: &kind}
			if set {
				e.R2Bucket = &r2TestSettings.Bucket
				e.R2Endpoint = &r2TestSettings.Endpoint
			}
			var got []settingRow
			for _, r := range effectiveSettings(e, nil) {
				if strings.HasPrefix(r.key, "state ") && r.key != "state dir" {
					got = append(got, r)
				}
			}
			if kind == "local" {
				if len(got) != 0 {
					t.Fatal("local rows changed")
				}
				continue
			}
			bucket, endpoint := "required", "required"
			if set {
				bucket = r2TestSettings.Bucket
				endpoint = r2TestEndpoint
			}
			want := []settingRow{{key: "state bucket", value: bucket, fromEntry: set}, {key: "state endpoint", value: endpoint, fromEntry: set}, {key: "state secrets", value: "environment (WITSELF_INFRA_R2_*, WITSELF_INFRA_STATE_PASSPHRASE)", fromEntry: false}}
			if !reflect.DeepEqual(got, want) {
				t.Fatal("R2 settings rows differ")
			}
		}
	}
}
func TestFatalMessageAndProgressRedact(t *testing.T) {
	for _, set := range []bool{false, true} {
		t.Run(fmt.Sprint(set), func(t *testing.T) {
			r2TestHome(t)
			text, want := "", ""
			for _, n := range []string{backend.R2AccessKeyIDEnv, backend.R2SecretAccessKeyEnv, backend.R2StatePassphraseEnv} {
				v := r2TestValues()[n]
				text += v + " "
				if set {
					t.Setenv(n, v)
					want += "[redacted " + n + "] "
				} else {
					want += v + " "
				}
			}
			if fatalMessage(errors.New(text), os.Getenv) != "witself-infra: "+want {
				t.Fatal("fatal redaction differs")
			}
			var out bytes.Buffer
			p := progressSink{w: &out}
			p.errPhase(r2TestCell, "test", errors.New(text))
			var event progressEvent
			if json.Unmarshal(out.Bytes(), &event) != nil || event.Note != want {
				t.Fatal("progress redaction differs")
			}
		})
	}
}
func TestCellHealthReportRedactsR2Values(t *testing.T) {
	r2TestHome(t)
	for n, v := range r2TestValues() {
		t.Setenv(n, v)
	}
	f := &r2FakePulumi{t: t, outputFailure: true}
	stack, err := openCellStack(context.Background(), "r2", "cell-health", r2TestCell, r2TestSettings.Bucket, auto.WorkDir(t.TempDir()), auto.Pulumi(f))
	if err != nil {
		t.Fatal("fake stack select failed")
	}
	capture, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = capture.Close() }()
	old := os.Stdout
	os.Stdout = capture
	defer func() { os.Stdout = old }()
	_ = printCellHealth(context.Background(), stack, "civo", "nyc1", "", false, time.Second)
	os.Stdout = old
	if _, err := capture.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Kubernetes struct {
			Detail string `json:"detail"`
		} `json:"kubernetes"`
	}
	if json.NewDecoder(capture).Decode(&report) != nil {
		t.Fatal("health JSON invalid")
	}
	detail := report.Kubernetes.Detail
	if !strings.HasPrefix(detail, "read stack outputs: ") {
		t.Fatal("missing output error prefix")
	}
	for n, v := range r2TestValues() {
		if strings.Contains(detail, v) || !strings.Contains(detail, "[redacted "+n+"]") {
			t.Fatal("health diagnostic redaction differs")
		}
	}
}

func TestR2ConfigAddCellRefusals(t *testing.T) {
	for _, tc := range []struct {
		name       string
		flags      []string
		want       string
		setSecrets bool
	}{
		{"other backend", []string{"-backend", "local", "-r2-bucket", ""}, "-r2-bucket and -r2-endpoint apply only to -backend r2", false},
		{"other cloud", []string{"-cloud", "aws", "-region", "us-west-2", "-backend", "r2"}, "-backend r2 is only implemented for -cloud civo", false},
		{"civo backend", []string{"-backend", "s3"}, "-cloud civo requires -backend local or r2", false},
		{"missing bucket", []string{"-backend", "r2"}, "-backend r2 requires -r2-bucket (inventory key r2_bucket)", false},
		{"directory", []string{"-backend", "r2", "-r2-bucket", r2TestSettings.Bucket, "-r2-endpoint", r2TestEndpoint, "-state-dir", ""}, "-state-dir does not apply to -backend r2 (inventory key state_dir)", false},
		{"secret paste", []string{"-backend", "r2", "-r2-bucket", r2TestValues()[backend.R2AccessKeyIDEnv], "-r2-endpoint", r2TestEndpoint}, "state backend r2: -r2-bucket or -r2-endpoint contains the value of WITSELF_INFRA_R2_ACCESS_KEY_ID; the inventory and the flags hold names and endpoints, never credential values", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := r2TestHome(t)
			if tc.setSecrets {
				for n, v := range r2TestValues() {
					t.Setenv(n, v)
				}
			}
			args := []string{"config", "add-cell", "-config", filepath.Join(home, "inventory.yaml"), "-cloud", "civo", "-region", "nyc1", "-civo-admin-cidr", "203.0.113.7/32"}
			args = append(args, tc.flags...)
			r2TestError(t, run(args), tc.want)
			r2EmptyHome(t, home)
		})
	}
}

func TestR2RunCheckAndBootstrap(t *testing.T) {
	for _, cmd := range []string{"state-check", "bootstrap"} {
		t.Run(cmd, func(t *testing.T) {
			home := r2TestHome(t)
			for n, v := range r2TestValues() {
				t.Setenv(n, v)
			}
			r2StubCLI(t, 265, 0, nil)
			calls := 0
			r2StubCheck(t, func(_ context.Context, _ backend.R2Settings, _ backend.R2Secrets, o backend.R2CheckOptions) (backend.R2CheckReport, error) {
				calls++
				if o.Project != projectName || o.Stack != r2TestCell || o.Inventory != (cmd == "state-check") {
					t.Fatal("run check options differ")
				}
				return backend.R2CheckReport{StackLocks: "0", HistoryObjects: "0", BackupObjects: "0", ProbeLeftovers: "0"}, nil
			})
			capture, err := os.CreateTemp(t.TempDir(), "stdout")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = capture.Close() }()
			old := os.Stdout
			os.Stdout = capture
			defer func() { os.Stdout = old }()
			err = run([]string{cmd, "-cloud", "civo", "-account-alias", "spike", "-region", "nyc1", "-role", "dev", "-backend", "r2", "-r2-bucket", r2TestSettings.Bucket, "-r2-endpoint", r2TestEndpoint, "-bootstrap"})
			os.Stdout = old
			if err != nil || calls != 1 {
				t.Fatal("run failed or probe count differs")
			}
			if _, err := capture.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			out, err := io.ReadAll(capture)
			if err != nil {
				t.Fatal(err)
			}
			want := r2CheckHeaders + r2CheckResults
			if cmd == "bootstrap" {
				want = "Civo R2 state backend ready: bucket witself-state-test at " + r2TestEndpoint + " (the owner creates the bucket; witself-infra wrote and deleted one probe object)\n"
			}
			if string(out) != want {
				t.Fatal("run output differs")
			}
			r2EmptyHome(t, home)
		})
	}
}

func TestR2UnknownBackendMessage(t *testing.T) {
	r2TestHome(t)
	r2TestError(t, run([]string{"outputs", "-cloud", "aws", "-backend", "unknown"}), `unknown -backend "unknown" (want local|s3|gcs|azblob|r2)`)
}

func TestOpenCellStackWrapsOtherErrors(t *testing.T) {
	f := &r2FakePulumi{t: t, selectErr: errors.New("fake selection failure")}
	_, err := openCellStack(context.Background(), "r2", "outputs", r2TestCell, r2TestSettings.Bucket, auto.WorkDir(t.TempDir()), auto.Pulumi(f))
	if err == nil || !strings.HasPrefix(err.Error(), fmt.Sprintf("create/select cell %q: ", r2TestCell)) || !strings.Contains(err.Error(), "fake selection failure") {
		t.Fatal("generic select error wrapper differs")
	}
	if len(f.calls) != 1 {
		t.Fatal("generic select failure created stack")
	}
}

const cloudflareTestToken = "test-cloudflare-token-not-real"

// cloudflareShapeFake builds a fake prefixed Cloudflare credential at runtime,
// so that no literal in the source has the shape of a real one.
func cloudflareShapeFake(kind string) string {
	return "cf" + kind + "_" + strings.Repeat("TestOnly", 5) + "c0ffee"
}

var cloudflareShapeTestToken = cloudflareShapeFake("ut")

func TestRedactDiagnostic(t *testing.T) {
	r2AndToken := r2TestValues()
	r2AndToken["CLOUDFLARE_API_TOKEN"] = cloudflareTestToken
	allValues := r2TestValues()
	allValues["CLOUDFLARE_API_TOKEN"] = cloudflareTestToken
	allValues["CLOUDFLARE_API_KEY"] = "test-cloudflare-key-not-real"
	allValues["CLOUDFLARE_API_USER_SERVICE_KEY"] = "test-cloudflare-service-key-not-real"
	for _, tc := range []struct {
		name string
		env  map[string]string
		text string
		want string
	}{
		{
			name: "unset",
			text: "x " + cloudflareTestToken + " y",
			want: "x " + cloudflareTestToken + " y",
		},
		{
			name: "value",
			env:  map[string]string{"CLOUDFLARE_API_TOKEN": cloudflareTestToken},
			text: cloudflareTestToken + " and " + cloudflareTestToken,
			want: "[redacted CLOUDFLARE_API_TOKEN] and [redacted CLOUDFLARE_API_TOKEN]",
		},
		{
			name: "bearer",
			env:  map[string]string{"CLOUDFLARE_API_TOKEN": cloudflareTestToken},
			text: "Authorization: Bearer " + cloudflareTestToken,
			want: "Authorization: Bearer [redacted CLOUDFLARE_API_TOKEN]",
		},
		{
			name: "padded export, bare echo",
			env:  map[string]string{"CLOUDFLARE_API_TOKEN": " " + cloudflareTestToken + "\n"},
			text: "token " + cloudflareTestToken + " rejected",
			want: "token [redacted CLOUDFLARE_API_TOKEN] rejected",
		},
		{
			name: "padded export, padded echo",
			env:  map[string]string{"CLOUDFLARE_API_TOKEN": " " + cloudflareTestToken + "\n"},
			text: "<" + " " + cloudflareTestToken + "\n" + ">",
			want: "<[redacted CLOUDFLARE_API_TOKEN]>",
		},
		{
			name: "short value",
			env:  map[string]string{"CLOUDFLARE_API_TOKEN": "abc1234"},
			text: "abc1234 stays",
			want: "abc1234 stays",
		},
		{
			name: "empty value",
			env:  map[string]string{"CLOUDFLARE_API_TOKEN": ""},
			text: "plain text",
			want: "plain text",
		},
		{
			name: "global key value",
			env:  map[string]string{"CLOUDFLARE_API_KEY": "test-cloudflare-key-not-real"},
			text: "key test-cloudflare-key-not-real",
			want: "key [redacted CLOUDFLARE_API_KEY]",
		},
		{
			name: "service key value",
			env:  map[string]string{"CLOUDFLARE_API_USER_SERVICE_KEY": "test-cloudflare-service-key-not-real"},
			text: "svc test-cloudflare-service-key-not-real",
			want: "svc [redacted CLOUDFLARE_API_USER_SERVICE_KEY]",
		},
		{
			name: "user token shape",
			text: "token=" + cloudflareShapeTestToken + ";",
			want: "token=[redacted Cloudflare token];",
		},
		{
			name: "account token shape",
			text: cloudflareShapeFake("at"),
			want: "[redacted Cloudflare token]",
		},
		{
			name: "global key shape",
			text: cloudflareShapeFake("k"),
			want: "[redacted Cloudflare token]",
		},
		{
			name: "short shape",
			text: "cf" + "ut_short",
			want: "cf" + "ut_short",
		},
		{
			name: "value before shape",
			env:  map[string]string{"CLOUDFLARE_API_TOKEN": cloudflareShapeTestToken},
			text: "t " + cloudflareShapeTestToken,
			want: "t [redacted CLOUDFLARE_API_TOKEN]",
		},
		{
			name: "R2 kept",
			env:  r2AndToken,
			text: r2AndToken["WITSELF_INFRA_R2_ACCESS_KEY_ID"] + " " + r2AndToken["WITSELF_INFRA_R2_SECRET_ACCESS_KEY"] + " " + r2AndToken["WITSELF_INFRA_STATE_PASSPHRASE"] + " " + cloudflareTestToken,
			want: "[redacted WITSELF_INFRA_R2_ACCESS_KEY_ID] [redacted WITSELF_INFRA_R2_SECRET_ACCESS_KEY] [redacted WITSELF_INFRA_STATE_PASSPHRASE] [redacted CLOUDFLARE_API_TOKEN]",
		},
		{
			name: "names never redacted",
			env:  allValues,
			text: civoMissingCloudflareMessage,
			want: civoMissingCloudflareMessage,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if redactDiagnostic(tc.text, func(name string) string { return tc.env[name] }) != tc.want {
				t.Fatal(tc.name)
			}
		})
	}
}

func TestExitsRedactCloudflareCredentials(t *testing.T) {
	for _, set := range []bool{false, true} {
		t.Run(fmt.Sprint(set), func(t *testing.T) {
			r2TestHome(t)
			want := "Authorization: Bearer " + cloudflareTestToken + " [redacted Cloudflare token]"
			if set {
				t.Setenv("CLOUDFLARE_API_TOKEN", cloudflareTestToken)
				want = "Authorization: Bearer [redacted CLOUDFLARE_API_TOKEN] [redacted Cloudflare token]"
			}
			text := "Authorization: Bearer " + cloudflareTestToken + " " + cloudflareShapeTestToken
			if fatalMessage(errors.New(text), os.Getenv) != "witself-infra: "+want {
				t.Fatal("fatal")
			}
			var buf bytes.Buffer
			p := progressSink{w: &buf}
			p.errPhase(r2TestCell, "test", errors.New(text))
			var event progressEvent
			if bytes.Count(buf.Bytes(), []byte("\n")) != 1 || json.Unmarshal(buf.Bytes(), &event) != nil || event.Note != want {
				t.Fatal("progress")
			}
		})
	}
}

func TestCellHealthReportRedactsCloudflareCredentials(t *testing.T) {
	r2TestHome(t)
	for n, v := range r2TestValues() {
		t.Setenv(n, v)
	}
	t.Setenv("CLOUDFLARE_API_TOKEN", cloudflareTestToken)
	f := &r2FakePulumi{t: t, outputFailure: true, outputStderr: "Authorization: Bearer " + cloudflareTestToken + " " + cloudflareShapeTestToken}
	stack, err := openCellStack(context.Background(), "r2", "cell-health", r2TestCell, r2TestSettings.Bucket, auto.WorkDir(t.TempDir()), auto.Pulumi(f))
	if err != nil {
		t.Fatal("fake stack select failed")
	}
	capture, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal("capture creation failed")
	}
	defer func() { _ = capture.Close() }()
	old := os.Stdout
	os.Stdout = capture
	defer func() { os.Stdout = old }()
	_ = printCellHealth(context.Background(), stack, "civo", "nyc1", "", false, time.Second)
	os.Stdout = old
	if _, err := capture.Seek(0, 0); err != nil {
		t.Fatal("capture seek failed")
	}
	var report struct {
		Kubernetes struct {
			Detail string `json:"detail"`
		} `json:"kubernetes"`
	}
	if json.NewDecoder(capture).Decode(&report) != nil {
		t.Fatal("health JSON invalid")
	}
	detail := report.Kubernetes.Detail
	if !strings.HasPrefix(detail, "read stack outputs: ") {
		t.Fatal("missing output error prefix")
	}
	for n, v := range r2TestValues() {
		if strings.Contains(detail, v) || !strings.Contains(detail, "[redacted "+n+"]") {
			t.Fatal("R2")
		}
	}
	if strings.Contains(detail, cloudflareTestToken) || strings.Contains(detail, cloudflareShapeTestToken) || !strings.Contains(detail, "[redacted CLOUDFLARE_API_TOKEN]") || !strings.Contains(detail, "[redacted Cloudflare token]") {
		t.Fatal("Cloudflare")
	}
}

func TestDiagnosticRedactionEntryPoints(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal("source glob failed")
	}
	backendCalls := 0
	fatalPrints := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("source read failed")
		}
		source := string(data)
		calls := strings.Count(source, "backend.RedactR2Secrets(")
		backendCalls += calls
		if calls != 0 && path != "state_r2.go" {
			t.Fatal("backend redaction location")
		}
		if strings.Contains(source, "redactR2(") {
			t.Fatal("legacy redaction")
		}
		if path == "main.go" {
			fatalPrints = strings.Count(source, "fmt.Fprintln(os.Stderr, fatalMessage(err, os.Getenv))")
		}
	}
	if backendCalls != 1 {
		t.Fatal("backend redaction count")
	}
	if fatalPrints != 1 {
		t.Fatal("fatal print count")
	}
}

const civoTestToken = "test-civo-token-value-not-real"
const civoFileTestToken = "test-civo-from-disk-not-real"
const localPassphraseTestValue = "test-local-state-passphrase-not-real"

func isolateResolvedLocalPassphrases(t *testing.T) {
	t.Helper()
	resolvedLocalPassphrases.Lock()
	saved := resolvedLocalPassphrases.values
	resolvedLocalPassphrases.values = nil
	resolvedLocalPassphrases.Unlock()
	t.Cleanup(func() {
		resolvedLocalPassphrases.Lock()
		resolvedLocalPassphrases.values = saved
		resolvedLocalPassphrases.Unlock()
	})
}

func TestRedactDiagnosticLocalPassphrase(t *testing.T) {
	allValues := r2TestValues()
	allValues[backend.R2StatePassphraseEnv] = "test-r2-state-passphrase-not-real"
	allValues["CLOUDFLARE_API_TOKEN"] = cloudflareTestToken
	allValues["CIVO_TOKEN"] = civoTestToken
	allValues["PULUMI_CONFIG_PASSPHRASE"] = localPassphraseTestValue
	for _, tc := range []struct {
		name     string
		env      map[string]string
		recorded []string
		text     string
		want     string
	}{
		{
			name: "exported value",
			env:  map[string]string{"PULUMI_CONFIG_PASSPHRASE": localPassphraseTestValue},
			text: "x " + localPassphraseTestValue,
			want: "x [redacted PULUMI_CONFIG_PASSPHRASE]",
		},
		{
			name: "padded export",
			env:  map[string]string{"PULUMI_CONFIG_PASSPHRASE": " " + localPassphraseTestValue + "\n"},
			text: "x " + localPassphraseTestValue,
			want: "x [redacted PULUMI_CONFIG_PASSPHRASE]",
		},
		{
			name: "short export",
			env:  map[string]string{"PULUMI_CONFIG_PASSPHRASE": "short77"},
			text: "short77 stays",
			want: "short77 stays",
		},
		{
			name:     "recorded value",
			recorded: []string{localPassphraseTestValue},
			text:     "x " + localPassphraseTestValue,
			want:     "x [redacted PULUMI_CONFIG_PASSPHRASE]",
		},
		{
			name: "unrecorded value",
			text: "x " + localPassphraseTestValue,
			want: "x " + localPassphraseTestValue,
		},
		{
			name:     "short recorded value",
			recorded: []string{"short77"},
			text:     "short77 stays",
			want:     "short77 stays",
		},
		{
			name: "all kinds",
			env:  allValues,
			text: strings.Join([]string{
				allValues[backend.R2AccessKeyIDEnv],
				allValues[backend.R2SecretAccessKeyEnv],
				allValues[backend.R2StatePassphraseEnv],
				cloudflareTestToken,
				civoTestToken,
				localPassphraseTestValue,
			}, " "),
			want: "[redacted WITSELF_INFRA_R2_ACCESS_KEY_ID] [redacted WITSELF_INFRA_R2_SECRET_ACCESS_KEY] [redacted WITSELF_INFRA_STATE_PASSPHRASE] [redacted CLOUDFLARE_API_TOKEN] [redacted CIVO_TOKEN] [redacted PULUMI_CONFIG_PASSPHRASE]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r2TestHome(t)
			isolateResolvedCivoTokens(t)
			isolateResolvedLocalPassphrases(t)
			for _, value := range tc.recorded {
				rememberLocalPassphrase(value)
			}
			got := redactDiagnostic(tc.text, func(name string) string { return tc.env[name] })
			if got != tc.want {
				t.Fatal("passphrase diagnostic mismatch")
			}
			requireNoSecretOutput(t, got)
			for _, value := range allValues {
				if !strings.Contains(tc.want, value) && strings.Contains(got, value) {
					t.Fatal("diagnostic disclosed a fixture value")
				}
			}
		})
	}
}

func TestRememberLocalPassphrase(t *testing.T) {
	r2TestHome(t)
	isolateResolvedLocalPassphrases(t)
	rememberLocalPassphrase("")
	rememberLocalPassphrase(localPassphraseTestValue)
	rememberLocalPassphrase(localPassphraseTestValue)
	resolvedLocalPassphrases.Lock()
	defer resolvedLocalPassphrases.Unlock()
	if len(resolvedLocalPassphrases.values) != 1 || resolvedLocalPassphrases.values[0] != localPassphraseTestValue {
		t.Fatal("passphrase record must omit empty values and duplicates")
	}
}

func TestEnsurePassphraseRecordsSources(t *testing.T) {
	for _, source := range []string{"environment", "file", "generated"} {
		t.Run(source, func(t *testing.T) {
			r2TestHome(t)
			isolateResolvedLocalPassphrases(t)
			stateDir := t.TempDir()
			path := filepath.Join(stateDir, "passphrase")
			switch source {
			case "environment":
				t.Setenv("PULUMI_CONFIG_PASSPHRASE", localPassphraseTestValue)
			case "file":
				if err := os.WriteFile(path, []byte(" "+localPassphraseTestValue+"\n"), 0o600); err != nil {
					t.Fatal("write fixture passphrase failed")
				}
			}
			value, err := ensurePassphrase(stateDir)
			if err != nil {
				t.Fatal("resolve local passphrase failed")
			}
			if source != "generated" && value != localPassphraseTestValue {
				t.Fatal("resolved passphrase did not match fixture")
			}
			if source == "generated" {
				data, readErr := os.ReadFile(path)
				info, statErr := os.Stat(path)
				if readErr != nil || statErr != nil || string(data) != value+"\n" || len(value) != 43 || info.Mode().Perm() != 0o600 {
					t.Fatal("generated passphrase persistence or mode mismatch")
				}
			}
			t.Setenv("PULUMI_CONFIG_PASSPHRASE", "")
			got := redactDiagnostic("x "+value, os.Getenv)
			if got != "x [redacted PULUMI_CONFIG_PASSPHRASE]" || strings.Contains(got, value) {
				t.Fatal("resolved passphrase was not recorded for redaction")
			}
			requireNoSecretOutput(t, got)
		})
	}
}

// isolateResolvedCivoTokens empties the process-wide record of resolved Civo
// tokens for one test and restores it afterwards.
func isolateResolvedCivoTokens(t *testing.T) {
	t.Helper()
	resolvedCivoTokens.Lock()
	saved := resolvedCivoTokens.values
	resolvedCivoTokens.values = nil
	resolvedCivoTokens.Unlock()
	t.Cleanup(func() {
		resolvedCivoTokens.Lock()
		resolvedCivoTokens.values = saved
		resolvedCivoTokens.Unlock()
	})
}

func TestRedactDiagnosticCivoToken(t *testing.T) {
	allValues := r2TestValues()
	allValues["CLOUDFLARE_API_TOKEN"] = cloudflareTestToken
	allValues["CIVO_TOKEN"] = civoTestToken
	for _, tc := range []struct {
		name     string
		env      map[string]string
		recorded []string
		text     string
		want     string
	}{
		{
			name: "env value",
			env:  map[string]string{"CIVO_TOKEN": civoTestToken},
			text: "Authorization: Bearer " + civoTestToken,
			want: "Authorization: Bearer [redacted CIVO_TOKEN]",
		},
		{
			name: "padded export",
			env:  map[string]string{"CIVO_TOKEN": " " + civoTestToken + "\n"},
			text: "token " + civoTestToken + " rejected",
			want: "token [redacted CIVO_TOKEN] rejected",
		},
		{
			name: "short value",
			env:  map[string]string{"CIVO_TOKEN": "abc1234"},
			text: "abc1234 stays",
			want: "abc1234 stays",
		},
		{
			name:     "resolved from a file",
			recorded: []string{civoFileTestToken},
			text:     "x " + civoFileTestToken + " y",
			want:     "x [redacted CIVO_TOKEN] y",
		},
		{
			name: "not resolved",
			text: "x " + civoFileTestToken + " y",
			want: "x " + civoFileTestToken + " y",
		},
		{
			name:     "short resolved",
			recorded: []string{"abc1234"},
			text:     "abc1234 stays",
			want:     "abc1234 stays",
		},
		{
			name:     "all kinds",
			env:      allValues,
			recorded: []string{civoFileTestToken},
			text: strings.Join([]string{
				allValues[backend.R2AccessKeyIDEnv],
				allValues[backend.R2SecretAccessKeyEnv],
				allValues[backend.R2StatePassphraseEnv],
				cloudflareTestToken,
				civoTestToken,
				civoFileTestToken,
				cloudflareShapeTestToken,
			}, " "),
			want: "[redacted WITSELF_INFRA_R2_ACCESS_KEY_ID] [redacted WITSELF_INFRA_R2_SECRET_ACCESS_KEY] [redacted WITSELF_INFRA_STATE_PASSPHRASE] [redacted CLOUDFLARE_API_TOKEN] [redacted CIVO_TOKEN] [redacted CIVO_TOKEN] [redacted Cloudflare token]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateResolvedCivoTokens(t)
			for _, value := range tc.recorded {
				rememberCivoToken(value)
			}
			if redactDiagnostic(tc.text, func(name string) string { return tc.env[name] }) != tc.want {
				t.Fatal(tc.name)
			}
		})
	}
}

func TestExitsRedactCivoTokenFromFile(t *testing.T) {
	r2TestHome(t)
	isolateResolvedCivoTokens(t)
	dir := t.TempDir()
	text := "Authorization: Bearer " + civoFileTestToken
	broad := filepath.Join(dir, "broad.token")
	if err := os.WriteFile(broad, []byte(civoFileTestToken+"\n"), 0o600); err != nil {
		t.Fatal("broad file creation failed")
	}
	if err := os.Chmod(broad, 0o644); err != nil {
		t.Fatal("broad file permissions failed")
	}
	if _, err := resolveCivoToken(broad); err == nil {
		t.Fatal("broad file was accepted")
	}
	if fatalMessage(errors.New(text), os.Getenv) != "witself-infra: "+text {
		t.Fatal("failed resolution recorded a token")
	}
	secure := filepath.Join(dir, "civo.token")
	if err := os.WriteFile(secure, []byte(civoFileTestToken+"\n"), 0o600); err != nil {
		t.Fatal("secure file creation failed")
	}
	for range 2 {
		token, err := resolveCivoToken(secure)
		if err != nil || token != civoFileTestToken {
			t.Fatal("secure file resolution failed")
		}
	}
	resolvedCivoTokens.Lock()
	count := len(resolvedCivoTokens.values)
	resolvedCivoTokens.Unlock()
	if count != 1 {
		t.Fatal("resolved token record count differs")
	}
	if fatalMessage(errors.New(text), os.Getenv) != "witself-infra: Authorization: Bearer [redacted CIVO_TOKEN]" {
		t.Fatal("fatal")
	}
	var buf bytes.Buffer
	p := progressSink{w: &buf}
	p.errPhase(r2TestCell, "test", errors.New(text))
	var event progressEvent
	if bytes.Count(buf.Bytes(), []byte("\n")) != 1 || json.Unmarshal(buf.Bytes(), &event) != nil || event.Note != "Authorization: Bearer [redacted CIVO_TOKEN]" {
		t.Fatal("progress")
	}
}
