package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"gopkg.in/yaml.v3"
)

const removeCellFleetToken = "test-fleet-token-not-real"
const removeCellStateSecret = "test-state-secret-not-real"
const removeCellLocalPassphrase = "test-local-passphrase-not-real"
const removeCellAliasCell = "civo-sandbox-use1-serving"
const removeCellAliasRegistry = "civo-sandbox-usw2-dev"

// removeCellFakePulumi answers the two Pulumi commands remove-cell may run:
// stack select and stack export. Any other command fails the test.
type removeCellFakePulumi struct {
	auto.PulumiCommand
	t           *testing.T
	missing     bool
	export      string
	exportFails bool
	onExport    func()
	calls       [][]string
	env         []string
}

func (f *removeCellFakePulumi) Run(_ context.Context, _ string, _ io.Reader, _, _ []io.Writer, env []string, args ...string) (string, string, int, error) {
	f.calls = append(f.calls, append([]string{}, args...))
	f.env = append([]string{}, env...)
	switch {
	case len(args) > 1 && args[0] == "stack" && args[1] == "select":
		if f.missing {
			return "", "error: no stack named '" + args[len(args)-1] + "' found", 255, errors.New("fake select failed")
		}
		return "", "", 0, nil
	case len(args) > 1 && args[0] == "stack" && args[1] == "export":
		if f.onExport != nil {
			f.onExport()
		}
		if f.exportFails {
			return `{"outputs":{"token":"` + removeCellStateSecret + `"}}`, "fake export failed", 1, errors.New("fake export failed")
		}
		return f.export, "", 0, nil
	default:
		f.t.Error("unexpected fake Pulumi command")
		return "", "", 1, errors.New("unexpected command")
	}
}

const removeCellEmptyExport = `{"version":3,"deployment":{"manifest":{"time":"2026-10-01T00:00:00Z"}}}`
const removeCellFullExport = `{"version":3,"deployment":{"resources":[{"urn":"urn:pulumi:x::witself-infra::pulumi:pulumi:Stack::witself-infra-x","type":"pulumi:pulumi:Stack","outputs":{"token":"` + removeCellStateSecret + `"}}],"pending_operations":[{"resource":{"urn":"urn:pulumi:x::witself-infra::civo:index/kubernetesCluster:KubernetesCluster::cell"},"type":"creating"}]}}`

type removeCellEnv struct {
	home, path, tokenPath, controlPlane string
	original                            []byte
	requests                            *int
	registry                            *[]string
	fake                                *removeCellFakePulumi
}

// removeCellFixture writes an inventory with a leading comment, an R2 cell and
// a local cell with a fleet alias, starts a fake control plane that lists
// civo-other-use1-dev and the given registry names, and installs fake as the
// Pulumi CLI. A row may replace the listed names through env.registry; nil
// makes the control plane answer {}.
func removeCellFixture(t *testing.T, fake *removeCellFakePulumi, registered ...string) removeCellEnv {
	t.Helper()
	home := r2TestHome(t)
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("WITSELF_FLEET_TOKEN", "")
	fake.t = t
	r2StubCLICommand(t, fake)
	requests := 0
	registry := append([]string{"civo-other-use1-dev"}, registered...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/cells" || r.Header.Get("Authorization") != "Bearer "+removeCellFleetToken {
			http.Error(w, `{"error":"unexpected request"}`, http.StatusBadRequest)
			return
		}
		if registry == nil {
			_, _ = io.WriteString(w, "{}")
			return
		}
		cells := []map[string]string{}
		for _, name := range registry {
			cells = append(cells, map[string]string{"name": name, "endpoint": "https://" + name + ".example.com"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "cells": cells})
	}))
	t.Cleanup(srv.Close)
	tokenPath := filepath.Join(home, "fleet.token")
	if err := os.WriteFile(tokenPath, []byte(removeCellFleetToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := "# operator note: kept only in the backup\nversion: 1\ndefaults:\n" +
		"  control_plane: " + srv.URL + "\n  fleet_token_file: " + tokenPath + "\ncells:\n" +
		"  " + r2TestCell + ":\n    cloud: civo\n    account_alias: spike\n    region: nyc1\n    role: dev\n" +
		"    backend: r2\n    r2_bucket: " + r2TestSettings.Bucket + "\n    r2_endpoint: " + r2TestEndpoint + "\n" +
		"  " + removeCellAliasCell + ":\n    registry_name: " + removeCellAliasRegistry + "\n    cloud: civo\n" +
		"    account_alias: sandbox\n    region: nyc1\n    role: serving\n    backend: local\n" +
		"    state_dir: " + filepath.Join(home, "state") + "\n"
	path := filepath.Join(home, "infra.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return removeCellEnv{home: home, path: path, tokenPath: tokenPath, controlPlane: srv.URL, original: []byte(body), requests: &requests, registry: &registry, fake: fake}
}

func setR2TestValues(t *testing.T) {
	t.Helper()
	for n, v := range r2TestValues() {
		t.Setenv(n, v)
	}
}

// runRemoveCell runs `config remove-cell` with args through run() and
// returns what it printed on stdout and stderr.
func runRemoveCell(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	dir := t.TempDir()
	outFile, outErr := os.Create(filepath.Join(dir, "stdout"))
	errFile, errErr := os.Create(filepath.Join(dir, "stderr"))
	if outErr != nil || errErr != nil {
		t.Fatal("create output capture failed")
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outFile, errFile
	err = run(append([]string{"config", "remove-cell"}, args...))
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = outFile.Close()
	_ = errFile.Close()
	printed, readOut := os.ReadFile(outFile.Name())
	warned, readErr := os.ReadFile(errFile.Name())
	if readOut != nil || readErr != nil {
		t.Fatal("read output capture failed")
	}
	return string(printed), string(warned), err
}

var removeCellHex64 = regexp.MustCompile(`[a-fA-F0-9]{64}`)

// requireNoSecretOutput fails when any text holds a fake credential value,
// the fake state secret, the fake local passphrase, a credential shape or a
// 64-hex value.
func requireNoSecretOutput(t *testing.T, texts ...string) {
	t.Helper()
	values := []string{removeCellFleetToken, removeCellStateSecret, removeCellLocalPassphrase}
	for _, v := range r2TestValues() {
		values = append(values, v)
	}
	for _, text := range texts {
		for _, v := range values {
			if strings.Contains(text, v) {
				t.Error("output disclosed a fake credential or state value")
			}
		}
		if secretShapes.MatchString(text) || removeCellHex64.MatchString(text) {
			t.Error("output holds a credential shape or a 64-hex value")
		}
	}
}

// requireUnchanged fails unless the inventory still holds its original bytes
// and no backup file exists.
func requireUnchanged(t *testing.T, env removeCellEnv) {
	t.Helper()
	got, err := os.ReadFile(env.path)
	if err != nil || !bytes.Equal(got, env.original) {
		t.Error("inventory changed")
	}
	if backups, _ := filepath.Glob(env.path + ".bak-*"); len(backups) != 0 {
		t.Error("backup written")
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestConfigRemoveCell(t *testing.T) {
	backupName := regexp.MustCompile(`^infra\.yaml\.bak-[0-9]{8}T[0-9]{6}Z$`)
	selectExport := [][]string{{"stack", "select", "--stack", r2TestCell}, {"stack", "export", "--show-secrets", "--stack", r2TestCell}}

	t.Run("unknown cell", func(t *testing.T) {
		for _, force := range []bool{false, true} {
			env := removeCellFixture(t, &removeCellFakePulumi{}, removeCellAliasRegistry)
			args := []string{"-config", env.path, "-cell", "civo-nope-use1-dev"}
			if force {
				args = append(args, "-force")
			}
			stdout, stderr, err := runRemoveCell(t, args...)
			want := `cell "civo-nope-use1-dev" not in ` + env.path + " (have: " + removeCellAliasCell + ", " + r2TestCell + ")"
			if errText(err) != want || stdout != "" || stderr != "" {
				t.Error("unknown cell refusal differs")
			}
			if *env.requests != 0 || len(env.fake.calls) != 0 {
				t.Error("unknown cell reached the registry or Pulumi")
			}
			requireUnchanged(t, env)
		}
	})

	t.Run("missing cell flag", func(t *testing.T) {
		env := removeCellFixture(t, &removeCellFakePulumi{})
		_, _, err := runRemoveCell(t, "-config", env.path)
		if errText(err) != "config remove-cell requires -cell NAME" {
			t.Error("missing -cell refusal differs")
		}
		requireUnchanged(t, env)
	})

	t.Run("other flags refused", func(t *testing.T) {
		env := removeCellFixture(t, &removeCellFakePulumi{})
		_, _, err := runRemoveCell(t, "-config", env.path, "-cell", r2TestCell, "-fleet-token-file", env.tokenPath, "-control-plane", "https://other.example.com")
		want := "config remove-cell takes only -cell, -config, -force and -dry-run (got -control-plane, -fleet-token-file); the control plane, the fleet token and the state backend come from the inventory"
		if errText(err) != want || *env.requests != 0 {
			t.Error("flag refusal differs")
		}
		requireUnchanged(t, env)
	})

	for _, registered := range []string{removeCellAliasRegistry, removeCellAliasCell} {
		t.Run("registered as "+registered, func(t *testing.T) {
			env := removeCellFixture(t, &removeCellFakePulumi{}, registered)
			stdout, _, err := runRemoveCell(t, "-config", env.path, "-cell", removeCellAliasCell)
			want := "remove-cell: control plane " + env.controlPlane + ` lists fleet registry entry "` + registered + `" for cell "` + removeCellAliasCell + `"; refusing to remove the entry (destroy the cell first; a cell that destroy removed moments ago can stay listed briefly)`
			if errText(err) != want || stdout != "" {
				t.Error("registered refusal differs")
			}
			if *env.requests != 1 || len(env.fake.calls) != 0 {
				t.Error("registered refusal request counts differ")
			}
			requireUnchanged(t, env)
		})
	}

	t.Run("registry unreadable", func(t *testing.T) {
		env := removeCellFixture(t, &removeCellFakePulumi{})
		if err := os.Remove(env.tokenPath); err != nil {
			t.Fatal(err)
		}
		_, _, err := runRemoveCell(t, "-config", env.path, "-cell", r2TestCell)
		prefix := "remove-cell: cannot read the fleet registry of control plane " + env.controlPlane + ` for cell "` + r2TestCell + `": `
		if err == nil || !strings.HasPrefix(err.Error(), prefix) || !strings.HasSuffix(err.Error(), "; refusing to remove the entry") {
			t.Error("unreadable registry refusal differs")
		}
		if *env.requests != 0 || len(env.fake.calls) != 0 {
			t.Error("unreadable registry reached the control plane or Pulumi")
		}
		requireUnchanged(t, env)
	})

	t.Run("registry lists no cells", func(t *testing.T) {
		for _, answer := range [][]string{nil, {""}} {
			env := removeCellFixture(t, &removeCellFakePulumi{})
			*env.registry = answer
			_, _, err := runRemoveCell(t, "-config", env.path, "-cell", r2TestCell)
			want := "remove-cell: cannot read the fleet registry of control plane " + env.controlPlane + ` for cell "` + r2TestCell + `": the answer lists no cells or a cell without a name; refusing to remove the entry`
			if errText(err) != want {
				t.Error("registry without cells refusal differs")
			}
			if *env.requests != 1 || len(env.fake.calls) != 0 {
				t.Error("registry without cells request counts differ")
			}
			requireUnchanged(t, env)
		}
	})

	t.Run("r2 credentials missing", func(t *testing.T) {
		env := removeCellFixture(t, &removeCellFakePulumi{export: removeCellEmptyExport})
		stdout, _, err := runRemoveCell(t, "-config", env.path, "-cell", r2TestCell)
		want := `remove-cell: cannot check the stack of cell "` + r2TestCell + `": state backend r2: missing environment variable(s): WITSELF_INFRA_R2_ACCESS_KEY_ID, WITSELF_INFRA_R2_SECRET_ACCESS_KEY, WITSELF_INFRA_STATE_PASSPHRASE; export them in the shell that runs witself-infra. They are never read from infra.yaml, a flag or a file, and the passphrase is never generated; refusing to remove the entry`
		if errText(err) != want {
			t.Error("missing credentials refusal differs")
		}
		if stdout != "registry: "+env.controlPlane+` lists no entry named "`+r2TestCell+`"`+"\n" {
			t.Error("registry line differs")
		}
		if *env.requests != 1 || len(env.fake.calls) != 0 {
			t.Error("missing credentials reached Pulumi")
		}
		requireUnchanged(t, env)
	})

	t.Run("r2 stack holds resources", func(t *testing.T) {
		env := removeCellFixture(t, &removeCellFakePulumi{export: removeCellFullExport})
		setR2TestValues(t)
		stdout, stderr, err := runRemoveCell(t, "-config", env.path, "-cell", r2TestCell)
		want := `remove-cell: the stack of cell "` + r2TestCell + `" holds 1 resource(s) and 1 pending operation(s); refusing to remove the entry (destroy the cell first)`
		if errText(err) != want {
			t.Error("resource refusal differs")
		}
		if !reflect.DeepEqual(env.fake.calls, selectExport) {
			t.Error("Pulumi command sequence differs")
		}
		requireNoSecretOutput(t, stdout, stderr, errText(err))
		requireUnchanged(t, env)
	})

	t.Run("r2 export fails", func(t *testing.T) {
		env := removeCellFixture(t, &removeCellFakePulumi{exportFails: true})
		setR2TestValues(t)
		stdout, stderr, err := runRemoveCell(t, "-config", env.path, "-cell", r2TestCell)
		want := `remove-cell: cannot read the stack of cell "` + r2TestCell + `": stack export failed; refusing to remove the entry`
		if errText(err) != want {
			t.Error("export failure refusal differs")
		}
		requireNoSecretOutput(t, stdout, stderr, errText(err), fatalMessage(errors.New(errText(err)), os.Getenv))
		requireUnchanged(t, env)
	})

	t.Run("r2 stack absent", func(t *testing.T) {
		env := removeCellFixture(t, &removeCellFakePulumi{missing: true})
		setR2TestValues(t)
		stdout, _, err := runRemoveCell(t, "-config", env.path, "-cell", r2TestCell)
		if err != nil {
			t.Fatal("absent stack refused")
		}
		lines := strings.Split(stdout, "\n")
		if len(lines) < 2 || lines[1] != `stack: cell "`+r2TestCell+`" has no stack in its r2 backend` {
			t.Error("absent stack line differs")
		}
		if !reflect.DeepEqual(env.fake.calls, selectExport[:1]) {
			t.Error("absent stack command sequence differs")
		}
		cfg, _, err := loadInfraConfig(env.path)
		if err != nil || len(cfg.Cells) != 1 {
			t.Error("entry not removed")
		}
	})

	t.Run("r2 stack empty", func(t *testing.T) {
		env := removeCellFixture(t, &removeCellFakePulumi{export: removeCellEmptyExport})
		setR2TestValues(t)
		before, _, err := loadInfraConfig(env.path)
		if err != nil {
			t.Fatal(err)
		}
		stdout, stderr, err := runRemoveCell(t, "-config", env.path, "-cell", r2TestCell)
		if err != nil || stderr != "" {
			t.Fatal("empty stack refused")
		}
		backups, _ := filepath.Glob(env.path + ".bak-*")
		if len(backups) != 1 || !backupName.MatchString(filepath.Base(backups[0])) {
			t.Fatal("backup name differs")
		}
		want := "registry: " + env.controlPlane + ` lists no entry named "` + r2TestCell + `"` + "\n" +
			`stack: the stack of cell "` + r2TestCell + `" holds no resources and no pending operations` + "\n" +
			"removed cell " + r2TestCell + " from " + env.path + "\n" +
			"backup: " + backups[0] + "\n" +
			"note: remove-cell rewrites the file — YAML comments are not preserved; the backup holds the original bytes\n"
		if stdout != want {
			t.Error("remove output differs")
		}
		info, err := os.Stat(backups[0])
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Error("backup mode differs")
		}
		saved, err := os.ReadFile(backups[0])
		if err != nil || !bytes.Equal(saved, env.original) {
			t.Error("backup bytes differ")
		}
		after, err := os.ReadFile(env.path)
		if err != nil || bytes.Contains(after, []byte("# operator note")) {
			t.Error("rewritten inventory differs")
		}
		cfg, _, err := loadInfraConfig(env.path)
		if err != nil || len(cfg.Cells) != 1 || !reflect.DeepEqual(cfg.Cells[removeCellAliasCell], before.Cells[removeCellAliasCell]) || !reflect.DeepEqual(cfg.Defaults, before.Defaults) {
			t.Error("remaining inventory differs")
		}
		requireNoSecretOutput(t, stdout, stderr)
	})

	t.Run("dry run", func(t *testing.T) {
		env := removeCellFixture(t, &removeCellFakePulumi{export: removeCellEmptyExport})
		setR2TestValues(t)
		stdout, _, err := runRemoveCell(t, "-config", env.path, "-cell", r2TestCell, "-dry-run")
		if err != nil {
			t.Fatal("dry run refused")
		}
		lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
		if len(lines) < 5 || lines[2] != "dry run: would remove cell "+r2TestCell+" from "+env.path+"; nothing was written" ||
			lines[len(lines)-1] != "note: without -dry-run, remove-cell copies the file to a backup and rewrites it — YAML comments are not preserved" {
			t.Error("dry run output differs")
		}
		var printed map[string]cellEntry
		if err := yaml.Unmarshal([]byte(strings.Join(lines[3:len(lines)-1], "\n")), &printed); err != nil || len(printed) != 1 || printed[r2TestCell].R2Bucket == nil {
			t.Error("dry run entry differs")
		}
		if !reflect.DeepEqual(env.fake.calls, selectExport) {
			t.Error("dry run skipped the stack check")
		}
		requireUnchanged(t, env)
	})

	t.Run("inventory changed during checks", func(t *testing.T) {
		appended := "# appended while remove-cell was checking\n"
		fake := &removeCellFakePulumi{export: removeCellEmptyExport}
		env := removeCellFixture(t, fake)
		fake.onExport = func() {
			f, err := os.OpenFile(env.path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Error("append to the inventory failed")
				return
			}
			_, _ = f.WriteString(appended)
			_ = f.Close()
		}
		setR2TestValues(t)
		_, _, err := runRemoveCell(t, "-config", env.path, "-cell", r2TestCell)
		if errText(err) != "remove-cell: "+env.path+" changed while remove-cell was checking; nothing was written; run it again" {
			t.Error("changed inventory refusal differs")
		}
		got, readErr := os.ReadFile(env.path)
		if readErr != nil || string(got) != string(env.original)+appended {
			t.Error("changed inventory lost the appended line")
		}
		if backups, _ := filepath.Glob(env.path + ".bak-*"); len(backups) != 0 {
			t.Error("backup written for a changed inventory")
		}
	})

	t.Run("force", func(t *testing.T) {
		env := removeCellFixture(t, &removeCellFakePulumi{}, removeCellAliasRegistry, removeCellAliasCell)
		stdout, stderr, err := runRemoveCell(t, "-config", env.path, "-cell", removeCellAliasCell, "-force")
		if err != nil {
			t.Fatal("forced removal refused")
		}
		if stderr != `warning: -force skipped the fleet registry and stack checks for cell "`+removeCellAliasCell+`"`+"\n" {
			t.Error("force warning differs")
		}
		if *env.requests != 0 || len(env.fake.calls) != 0 {
			t.Error("forced removal checked the registry or the stack")
		}
		if !strings.HasPrefix(stdout, "removed cell "+removeCellAliasCell+" from "+env.path+"\n") {
			t.Error("forced removal output differs")
		}
		cfg, _, err := loadInfraConfig(env.path)
		if err != nil {
			t.Fatal("forced removal left an unloadable inventory")
		}
		if _, kept := cfg.Cells[removeCellAliasCell]; kept {
			t.Error("forced removal kept the entry")
		}
	})

	t.Run("local state directory absent", func(t *testing.T) {
		env := removeCellFixture(t, &removeCellFakePulumi{})
		stdout, _, err := runRemoveCell(t, "-config", env.path, "-cell", removeCellAliasCell)
		if err != nil {
			t.Fatal("absent local state refused")
		}
		want := "registry: " + env.controlPlane + ` lists no entry named "` + removeCellAliasRegistry + `" or "` + removeCellAliasCell + `"` + "\n" +
			`stack: cell "` + removeCellAliasCell + `" has no stack in its local backend` + "\n"
		if !strings.HasPrefix(stdout, want) {
			t.Error("local absent output differs")
		}
		if len(env.fake.calls) != 0 {
			t.Error("absent local state reached Pulumi")
		}
		if _, err := os.Stat(filepath.Join(env.home, "state")); !errors.Is(err, os.ErrNotExist) {
			t.Error("remove-cell created a state directory")
		}
	})

	t.Run("local stack holds resources", func(t *testing.T) {
		env := removeCellFixture(t, &removeCellFakePulumi{export: removeCellFullExport})
		stateDir := filepath.Join(env.home, "state")
		if err := os.MkdirAll(filepath.Join(stateDir, ".pulumi"), 0o700); err != nil {
			t.Fatal(err)
		}
		_, _, err := runRemoveCell(t, "-config", env.path, "-cell", removeCellAliasCell)
		want := `remove-cell: the stack of cell "` + removeCellAliasCell + `" holds 1 resource(s) and 1 pending operation(s); refusing to remove the entry (destroy the cell first)`
		if errText(err) != want {
			t.Error("local resource refusal differs")
		}
		if !slices.Contains(env.fake.env, "PULUMI_BACKEND_URL=file://"+stateDir) {
			t.Error("local backend URL differs")
		}
		if _, err := os.Stat(filepath.Join(stateDir, "passphrase")); !errors.Is(err, os.ErrNotExist) {
			t.Error("remove-cell created a passphrase")
		}
		requireUnchanged(t, env)
	})

	t.Run("local stack empty", func(t *testing.T) {
		env := removeCellFixture(t, &removeCellFakePulumi{export: removeCellEmptyExport})
		stateDir := filepath.Join(env.home, "state")
		if err := os.MkdirAll(filepath.Join(stateDir, ".pulumi"), 0o700); err != nil {
			t.Fatal(err)
		}
		passphrasePath := filepath.Join(stateDir, "passphrase")
		passphraseBytes := []byte(removeCellLocalPassphrase + "\n")
		if err := os.WriteFile(passphrasePath, passphraseBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, err := runRemoveCell(t, "-config", env.path, "-cell", removeCellAliasCell)
		if err != nil {
			t.Fatal("empty local stack refused")
		}
		want := "registry: " + env.controlPlane + ` lists no entry named "` + removeCellAliasRegistry + `" or "` + removeCellAliasCell + `"` + "\n" +
			`stack: the stack of cell "` + removeCellAliasCell + `" holds no resources and no pending operations` + "\n" +
			"removed cell " + removeCellAliasCell + " from " + env.path + "\n"
		if !strings.HasPrefix(stdout, want) {
			t.Error("empty local stack output differs")
		}
		if !slices.Contains(env.fake.env, "PULUMI_CONFIG_PASSPHRASE="+removeCellLocalPassphrase) {
			t.Error("local passphrase not passed to Pulumi")
		}
		if got, err := os.ReadFile(passphrasePath); err != nil || !bytes.Equal(got, passphraseBytes) {
			t.Error("passphrase file changed")
		}
		cfg, _, err := loadInfraConfig(env.path)
		if err != nil {
			t.Fatal("inventory does not load after the removal")
		}
		if _, kept := cfg.Cells[removeCellAliasCell]; kept || len(cfg.Cells) != 1 {
			t.Error("empty local stack kept the entry")
		}
		requireNoSecretOutput(t, stdout, stderr)
	})

	t.Run("rewrite credential guard", func(t *testing.T) {
		home := r2TestHome(t)
		fake := &removeCellFakePulumi{t: t}
		r2StubCLICommand(t, fake)
		// The escaped line break keeps every raw line clear of the paste
		// guard; the decoded value is rewritten on one line.
		body := "version: 1\ndefaults:\n  fleet_token_file: \"/tokens/gh" + "p_AAAAAAAAAA\\\n    BBBBBBBBBBBBBBBBBBBB\"\ncells:\n" +
			"  cell-a:\n    backend: local\n    state_dir: " + filepath.Join(home, "state") + "\n  cell-b:\n    backend: local\n"
		path := filepath.Join(home, "infra.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		stdout, _, err := runRemoveCell(t, "-config", path, "-cell", "cell-a")
		want := "refusing to write " + path + ": line 3 looks like a credential — pass a token file PATH, never a token value"
		if errText(err) != want {
			t.Error("rewrite guard refusal differs")
		}
		if !strings.HasPrefix(stdout, `registry: no control plane is configured for cell "cell-a"; not checked`+"\n") {
			t.Error("no control plane line differs")
		}
		got, readErr := os.ReadFile(path)
		backups, _ := filepath.Glob(path + ".bak-*")
		if readErr != nil || string(got) != body || len(backups) != 0 {
			t.Error("guarded rewrite changed the inventory or wrote a backup")
		}
	})
}

func TestForceOnlyWithRemoveCell(t *testing.T) {
	for _, args := range [][]string{
		{"config", "show", "-force"},
		{"config", "add-cell", "-cloud", "aws", "-force"},
		{"health", "-force"},
		{"up", "-cloud", "civo", "-region", "nyc1", "-backend", "local", "-civo-admin-cidr", "203.0.113.7/32", "-force"},
	} {
		t.Run(strings.Join(args[:2], " "), func(t *testing.T) {
			home := r2TestHome(t)
			args = append(args, "-config", filepath.Join(home, "absent.yaml"))
			if _, err := runCapturingStdout(t, args); errText(err) != "-force is only valid with `config remove-cell`" {
				t.Error("force refusal differs")
			}
			if entries, _ := os.ReadDir(home); len(entries) != 0 {
				t.Error("refused command wrote a file")
			}
		})
	}
}
