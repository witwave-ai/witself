package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func events(spec phaseSpec, target string) []testEvent {
	out := []testEvent{{Action: "start", Package: packageName}}
	for _, test := range spec.tests {
		out = append(out, testEvent{Action: "run", Package: packageName, Test: test.Name})
		action := "pass"
		if target == "windows-x64" && test.Provider == "cursor" {
			action = "skip"
			out = append(out, testEvent{Action: "output", Package: packageName, Test: test.Name, Output: "    fixture_test.go:33: " + cursorSkip + "\n"})
		}
		out = append(out, testEvent{Action: action, Package: packageName, Test: test.Name})
	}
	return append(out, testEvent{Action: "pass", Package: packageName})
}
func eventBytes(input []testEvent) []byte {
	var out bytes.Buffer
	for _, e := range input {
		b, _ := json.Marshal(e)
		out.Write(b)
		out.WriteByte('\n')
	}
	return out.Bytes()
}
func TestExactEventOutcomes(t *testing.T) {
	count, passed, na := 0, 0, 0
	for _, target := range targets {
		for _, spec := range specs() {
			s := newEventStream(spec, target, nil)
			raw := eventBytes(events(spec, target))
			for _, b := range raw {
				_, _ = s.Write([]byte{b})
			}
			results, reason := s.finish(0)
			if reason != "none" {
				t.Fatalf("%s %s: %s", target, spec.name, reason)
			}
			for _, r := range results {
				count++
				switch r.Status {
				case "passed":
					passed++
				case "not_applicable":
					na++
				default:
					t.Fatal("bad status")
				}
			}
		}
	}
	if count != 75 || passed != 73 || na != 2 {
		t.Fatalf("wrong outcomes %d/%d/%d", count, passed, na)
	}
}
func TestEventRefusals(t *testing.T) {
	spec := specs()[0]
	cases := map[string]func([]testEvent) []testEvent{
		"zero selected":      func(in []testEvent) []testEvent { return []testEvent{in[0], in[len(in)-1]} },
		"missing test":       func(in []testEvent) []testEvent { return append(in[:1], in[3:]...) },
		"failed test":        func(in []testEvent) []testEvent { in[2].Action = "fail"; return in },
		"failed package":     func(in []testEvent) []testEvent { in[len(in)-1].Action = "fail"; return in },
		"skipped package":    func(in []testEvent) []testEvent { in[len(in)-1].Action = "skip"; return in },
		"duplicate terminal": func(in []testEvent) []testEvent { return append(in[:3], append([]testEvent{in[2]}, in[3:]...)...) },
		"conflicting terminal": func(in []testEvent) []testEvent {
			conflict := in[2]
			conflict.Action = "fail"
			return append(in[:3], append([]testEvent{conflict}, in[3:]...)...)
		},
		"missing run":              func(in []testEvent) []testEvent { return append(in[:1], in[2:]...) },
		"missing package terminal": func(in []testEvent) []testEvent { return in[:len(in)-1] },
		"unexpected skip":          func(in []testEvent) []testEvent { in[2].Action = "skip"; return in },
		"wrong package":            func(in []testEvent) []testEvent { in[0].Package = "private/package"; return in },
		"wrong phase test":         func(in []testEvent) []testEvent { in[1].Test = "TestProviderIntegrationContractClaude"; return in },
		"test after package":       func(in []testEvent) []testEvent { return append(in, in[1]) },
		"nested failure": func(in []testEvent) []testEvent {
			child := []testEvent{{Action: "run", Package: packageName, Test: spec.tests[0].Name + "/child"}, {Action: "fail", Package: packageName, Test: spec.tests[0].Name + "/child"}}
			return append(in[:2], append(child, in[2:]...)...)
		},
		"nested incomplete": func(in []testEvent) []testEvent {
			return append(in[:2], append([]testEvent{{Action: "run", Package: packageName, Test: spec.tests[0].Name + "/child"}}, in[2:]...)...)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := newEventStream(spec, "linux-x64", nil)
			_, _ = s.Write(eventBytes(change(events(spec, "linux-x64"))))
			if _, reason := s.finish(0); reason == "none" {
				t.Fatal("invalid event stream passed")
			}
		})
	}
	for _, raw := range [][]byte{[]byte("not JSON\n"), []byte(`{"Action":"start","Action":"pass","Package":"` + packageName + `"}` + "\n"), []byte(`{"Action":"start","Package":"` + packageName + `","Unknown":"PRIVATE_SENTINEL"}` + "\n"), []byte(`{"Action":"start","Package":"` + packageName + `"}`), bytes.Repeat([]byte("x"), maxEventBytes+1)} {
		s := newEventStream(spec, "linux-x64", nil)
		_, _ = s.Write(raw)
		if _, reason := s.finish(0); reason == "none" {
			t.Fatal("malformed/truncated/unbounded input passed")
		}
	}
	s := newEventStream(spec, "linux-x64", nil)
	_, _ = s.Write(eventBytes(events(spec, "linux-x64")))
	if _, reason := s.finish(7); reason != "process_failed" {
		t.Fatal("nonzero process passed")
	}
}
func TestWindowsCursorRequiresExactReason(t *testing.T) {
	spec := specs()[1]
	for _, kind := range []string{"wrong reason", "pass instead of skip", "missing skip"} {
		t.Run(kind, func(t *testing.T) {
			input := events(spec, "windows-x64")
			for n := range input {
				if input[n].Test == "TestProviderIntegrationContractCursor" {
					if input[n].Action == "output" && kind == "wrong reason" {
						input[n].Output = "private unrelated skip\n"
					}
					if input[n].Action == "skip" {
						if kind == "pass instead of skip" {
							input[n].Action = "pass"
						}
						if kind == "missing skip" {
							input = input[:n]
							break
						}
					}
				}
			}
			s := newEventStream(spec, "windows-x64", nil)
			_, _ = s.Write(eventBytes(input))
			if _, reason := s.finish(0); reason == "none" {
				t.Fatal("unproven Windows exception passed")
			}
		})
	}
}
func TestRawOutputDoesNotEscape(t *testing.T) {
	spec := specs()[0]
	in := events(spec, "linux-x64")
	in = append(in[:2], append([]testEvent{{Action: "output", Package: packageName, Test: spec.tests[0].Name, Output: "PRIVATE_SENTINEL /private/home/runtime token=SECRET_VALUE\n"}}, in[2:]...)...)
	s := newEventStream(spec, "linux-x64", nil)
	_, _ = s.Write(eventBytes(in))
	results, reason := s.finish(0)
	if reason != "none" {
		t.Fatal(reason)
	}
	b := marshal(t, results)
	for _, bad := range []string{"PRIVATE_SENTINEL", "/private/", "SECRET_VALUE", "Output"} {
		if bytes.Contains(b, []byte(bad)) {
			t.Fatal("raw output leaked")
		}
	}
}

// The subprocess fixture is this already-built test executable. It never
// invokes Go builds, the Witself client, providers, or a live backend.
func TestEvidenceChild(_ *testing.T) {
	index := -1
	for n, a := range os.Args {
		if a == "--evidence-child" {
			index = n
			break
		}
	}
	if index < 0 {
		return
	}
	mode, phaseNumber, target := os.Args[index+1], os.Args[index+2], os.Args[index+3]
	n, _ := strconv.Atoi(phaseNumber)
	spec := specs()[n]
	if mode == "flood" {
		_, _ = fmt.Fprintln(os.Stdout, "malformed PRIVATE_SENTINEL")
		block := bytes.Repeat([]byte("x"), 65536)
		for i := 0; i < 2048; i++ {
			_, _ = os.Stdout.Write(block)
		}
		os.Exit(0)
	}
	expected := os.Args[index+4]
	if envValue(os.Environ(), installedEnv) != expected {
		_, _ = fmt.Fprintln(os.Stderr, "PRIVATE_ENV_SENTINEL")
		os.Exit(23)
	}
	input := events(spec, target)
	if mode == "unsafe-go-env" && (strings.Contains(os.Getenv("GOFLAGS"), "overlay") || os.Getenv("GOWORK") != "off" || os.Getenv("GOENV") != "off" || os.Getenv("GOEXPERIMENT") != "") {
		os.Exit(29)
	}
	if mode == "missing-then-fail" && n == 0 {
		input = []testEvent{input[0], input[len(input)-1]}
	}
	_, _ = os.Stdout.Write(eventBytes(input))
	_, _ = fmt.Fprintln(os.Stderr, "PRIVATE_STDERR_SENTINEL")
	if (mode == "fail-first" && n == 0) || (mode == "missing-then-fail" && n == 1) {
		os.Exit(7)
	}
	os.Exit(0)
}
func helperFactory(t *testing.T, mode, target, binary string, calls *[]int) commandFactory {
	t.Helper()
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "go" || len(args) != 6 || args[0] != "test" || args[1] != "-json" || args[2] != "./cmd/witself" || args[3] != "-run" || args[5] != "-count=1" {
			t.Fatal("non-contract command")
		}
		n := -1
		for i, spec := range specs() {
			if args[4] == spec.selection {
				n = i
			}
		}
		if n < 0 {
			t.Fatal("unexpected test selection")
		}
		*calls = append(*calls, n)
		expected := ""
		if n == 2 {
			expected = binary
		}
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEvidenceChild$", "--", "--evidence-child", mode, strconv.Itoa(n), target, expected)
	}
}
func nativeFixture(t *testing.T) cell {
	t.Helper()
	for _, c := range fixture(t).Cells {
		if c.GOOS == runtime.GOOS && c.GOARCH == runtime.GOARCH {
			return c
		}
	}
	t.Fatal("unsupported test host")
	return cell{}
}
func runnerFixture(t *testing.T, mode string) (runOptions, runDependencies, *[]int) {
	t.Helper()
	c := nativeFixture(t)
	binary := filepath.Join(t.TempDir(), "synthetic-binary")
	o := runOptions{identity: c.Identity, target: c.Target, binary: binary, dist: t.TempDir()}
	calls := []int{}
	deps := runDependencies{command: helperFactory(t, mode, c.Target, binary, &calls), checkout: func(context.Context, string) error { return nil }, artifact: func(string, string, cell) (*artifact, error) { a := *c.Artifact; return &a, nil }, version: func(context.Context, string, *artifact, cell, commandFactory) error { return nil }}
	return o, deps, &calls
}
func TestRunnerContinuesAndPreservesExit(t *testing.T) {
	for _, mode := range []string{"pass", "fail-first", "missing-then-fail"} {
		t.Run(mode, func(t *testing.T) {
			o, deps, calls := runnerFixture(t, mode)
			t.Setenv(installedEnv, "PRIVATE_INHERITED_BINARY")
			c, exit := executeRun(context.Background(), o, deps)
			if fmt.Sprint(*calls) != "[0 1 2]" || len(c.Phases) != 3 {
				t.Fatal("did not execute every phase once")
			}
			want := 7
			if mode == "pass" {
				want = 0
			}
			if exit != want {
				t.Fatalf("exit=%d want %d", exit, want)
			}
			if mode == "pass" && validateCell(c, o.identity) != nil {
				t.Fatal("complete actual child evidence invalid")
			}
			raw := marshal(t, c)
			if bytes.Contains(raw, []byte("PRIVATE_")) {
				t.Fatal("raw output inherited/leaked")
			}
		})
	}
}
func TestRunnerReapsMalformedFlood(t *testing.T) {
	o, deps, calls := runnerFixture(t, "flood")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start := time.Now()
	c, code := executeRun(ctx, o, deps)
	if code == 0 || len(*calls) != 3 || c.Status == "passed" || ctx.Err() != nil || time.Since(start) > 7*time.Second {
		t.Fatal("malformed child not bounded/drained/reaped")
	}
}
func TestRunnerProvenanceFailures(t *testing.T) {
	for _, kind := range []string{"wrong native target", "wrong checkout", "checkout changed", "missing artifact", "artifact changed", "version refused"} {
		t.Run(kind, func(t *testing.T) {
			o, deps, calls := runnerFixture(t, "pass")
			switch kind {
			case "wrong native target":
				if o.target == "linux-x64" {
					o.target = "windows-x64"
				} else {
					o.target = "linux-x64"
				}
			case "wrong checkout":
				deps.checkout = func(context.Context, string) error { return errInvalidEvidence }
			case "checkout changed":
				n := 0
				deps.checkout = func(context.Context, string) error {
					n++
					if n == 2 {
						return errInvalidEvidence
					}
					return nil
				}
			case "missing artifact":
				deps.artifact = func(string, string, cell) (*artifact, error) { return nil, errInvalidEvidence }
			case "artifact changed":
				original := deps.artifact
				n := 0
				deps.artifact = func(d, b string, c cell) (*artifact, error) {
					n++
					a, e := original(d, b, c)
					if n == 2 {
						a.BinarySHA256 = strings.Repeat("b", 64)
					}
					return a, e
				}
			case "version refused":
				deps.version = func(context.Context, string, *artifact, cell, commandFactory) error { return errInvalidEvidence }
			}
			c, exit := executeRun(context.Background(), o, deps)
			if exit == 0 || c.Status == "passed" {
				t.Fatal("unproven cell passed")
			}
			if (kind == "wrong native target" || kind == "wrong checkout") && len(*calls) != 0 {
				t.Fatal("tests ran without source identity")
			}
			if (kind == "missing artifact" || kind == "version refused") && (len(*calls) != 2 || c.Phases[2].Status != "incomplete") {
				t.Fatal("unverified executable was used")
			}
		})
	}
}
func TestPhaseEnvironment(t *testing.T) {
	env := phaseEnvironment([]string{installedEnv + "=SECRET", "goos=windows", "GOARCH=386", "PATH=fixture"}, "")
	if envValue(env, installedEnv) != "" || envValue(env, "GOOS") != runtime.GOOS || envValue(env, "GOARCH") != runtime.GOARCH || envValue(env, "PATH") != "fixture" {
		t.Fatal("phase environment is ambiguous")
	}
}

var _ io.Writer = (*eventStream)(nil)

func TestEventAliasesCannotTurnFailureIntoPass(t *testing.T) {
	spec := specs()[0]
	raw := eventBytes(events(spec, "linux-x64"))
	raw = bytes.Replace(raw, []byte(`"Action":"pass"`), []byte(`"Action":"fail","action":"pass"`), 1)
	stream := newEventStream(spec, "linux-x64", nil)
	_, _ = stream.Write(raw)
	if _, reason := stream.finish(0); reason == "none" {
		t.Fatal("case-fold alias changed failure into a passing outcome")
	}
}
func TestRunnerDoesNotInheritSourceSubstitution(t *testing.T) {
	t.Setenv("GOFLAGS", "-overlay=/private/SUBSTITUTE_SOURCE.json -modfile=/private/other.mod -tags=forged")
	t.Setenv("GOWORK", "/private/other.go.work")
	t.Setenv("GOENV", "/private/other.go.env")
	t.Setenv("GOEXPERIMENT", "PRIVATE_EXPERIMENT")
	o, deps, _ := runnerFixture(t, "unsafe-go-env")
	_, code := executeRun(context.Background(), o, deps)
	if code != 0 {
		t.Fatalf("source-substituting Go environment reached selected test child: exit %d", code)
	}
}
func pinFixtureToolchain(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Resolve the installed toolchain through Go rather than the build-time
	// GOROOT. A newer global launcher must not inherit an earlier launcher's
	// private switch sentinel or select a different version for this fixture.
	env := []string{}
	for _, value := range phaseEnvironment(os.Environ(), "") {
		key, _, _ := strings.Cut(value, "=")
		switch strings.ToUpper(key) {
		case "GOROOT", "GOTOOLCHAIN", "GOTOOLCHAIN_INTERNAL_SWITCH_VERSION", "GOTOOLCHAIN_INTERNAL_SWITCH_COUNT":
			continue
		}
		env = append(env, value)
	}
	env = append(env, "GOTOOLCHAIN="+runtime.Version())
	cmd := exec.CommandContext(ctx, "go", "env", "-json", "GOROOT", "GOVERSION")
	cmd.Env = env
	cmd.WaitDelay = 2 * time.Second
	cmd.Stderr = io.Discard
	out := &cappedBuffer{limit: 4096}
	cmd.Stdout = out
	var discovered struct{ GOROOT, GOVERSION string }
	if cmd.Run() != nil || out.overflow || json.Unmarshal(out.Bytes(), &discovered) != nil || discovered.GOVERSION != runtime.Version() || !filepath.IsAbs(discovered.GOROOT) {
		t.Fatal("could not resolve the pinned fixture toolchain")
	}
	t.Setenv("PATH", filepath.Join(discovered.GOROOT, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOTOOLCHAIN", runtime.Version())
	for _, key := range []string{"GOROOT", "GOTOOLCHAIN_INTERNAL_SWITCH_VERSION", "GOTOOLCHAIN_INTERNAL_SWITCH_COUNT"} {
		t.Setenv(key, "")
	}
}
func TestRealGoOverlayCannotSubstituteProviderSource(t *testing.T) {
	// A stale launcher handoff must not control the discovered or selected Go.
	t.Setenv("GOTOOLCHAIN_INTERNAL_SWITCH_VERSION", "go0.0.0")
	t.Setenv("GOTOOLCHAIN_INTERNAL_SWITCH_COUNT", "100")
	pinFixtureToolchain(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sourceDir := filepath.Join(root, "cmd", "witself")
	if os.MkdirAll(sourceDir, 0o700) != nil {
		t.Fatal("mkdir")
	}
	if os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/witwave-ai/witself\n\ngo 1.26.0\n"), 0o600) != nil {
		t.Fatal("module")
	}
	source := []byte("package main\nimport \"testing\"\nfunc TestProviderIntegrationContractCodex(t *testing.T){t.Fatal(\"original source fixture\")}\nfunc TestProviderIntegrationContractCodexRollbackRestoresPriorInstall(t *testing.T){}\n")
	path := filepath.Join(sourceDir, "provider_test.go")
	if os.WriteFile(path, source, 0o600) != nil {
		t.Fatal("source")
	}
	replacement := filepath.Join(t.TempDir(), "replacement.go")
	if os.WriteFile(replacement, bytes.Replace(source, []byte("t.Fatal"), []byte("t.Log"), 1), 0o600) != nil {
		t.Fatal("overlay replacement")
	}
	overlay := filepath.Join(t.TempDir(), "overlay.json")
	if os.WriteFile(overlay, marshal(t, map[string]any{"Replace": map[string]string{path: replacement}}), 0o600) != nil {
		t.Fatal("overlay")
	}
	t.Setenv("GOFLAGS", "-p=2 -mod=readonly -overlay="+overlay)
	t.Setenv("GOWORK", "off")
	t.Setenv("GOENV", "off")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	spec := specs()[0]
	target := nativeFixture(t).Target
	// Reproduce the old inherited environment with real Go source substitution.
	baseline := newEventStream(spec, target, cancel)
	cmd := exec.CommandContext(ctx, "go", "test", "-json", "./cmd/witself", "-run", spec.selection, "-count=1")
	cmd.Dir = root
	cmd.Env = os.Environ()
	cmd.Stdout = baseline
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		t.Fatalf("overlay baseline process failed: %v, parser %s", err, baseline.failure)
	}
	if _, reason := baseline.finish(0); reason != "none" {
		t.Fatalf("overlay did not reproduce substituted pass: %s", reason)
	}
	t.Log("Unsafe inherited overlay executed external replacement and produced passing selected provider outcomes.")
	got := runPhase(ctx, spec, target, "", func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = root
		return cmd
	})
	if got.Status != "failed" || got.ProcessExit == nil || *got.ProcessExit != 1 || got.Tests[0].Status != "failed" {
		t.Fatal("controlled runner certified external replacement instead of original source")
	}
	after, e := os.ReadFile(path)
	if e != nil || !bytes.Equal(after, source) {
		t.Fatal("fixture source changed")
	}
	t.Log("Controlled runner executed unchanged original source, observed failing test and child exit 1; external overlay was not used.")
}
func TestReportWriteFailurePreservesChildExit(t *testing.T) {
	o, deps, _ := runnerFixture(t, "fail-first")
	var stderr bytes.Buffer
	args := []string{"run", "--output", filepath.Join(t.TempDir(), "missing", "cell.json"), "--target", o.target, "--expected-commit", o.identity.SourceCommit, "--repository", o.identity.Repository, "--workflow", o.identity.Workflow, "--run-id", o.identity.RunID, "--run-attempt", strconv.Itoa(o.identity.RunAttempt), "--source-ref", o.identity.SourceRef, "--release-tag", o.identity.ReleaseTag, "--binary", o.binary, "--dist", o.dist}
	if got := cliWithDependencies(args, &stderr, deps); got != 7 {
		t.Fatalf("evidence write failure replaced child exit: %d", got)
	}
	if stderr.String() != "provider-contract-evidence: invalid_evidence\n" {
		t.Fatal("unbounded failure output")
	}
}
