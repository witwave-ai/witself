package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal("fixture directory")
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal("fixture write")
	}
}

func adapterFixture() []byte {
	return []byte("package trust\nfunc snapshot() {\n\tif o.hooks != nil && o.hooks.observe != nil {\n\t\to.hooks.observe(stage, f, &p)\n\t}\n\tif privacy && checkPrivateRecordPolicy(p) != nil {\n\t\treturn ReasonStateIO\n\t}\n}\n")
}

func TestSourceOverlays(t *testing.T) {
	for _, newline := range []string{"LF", "CRLF"} {
		for _, mechanism := range []string{"M1", "M2"} {
			t.Run(newline+"/"+mechanism, func(t *testing.T) {
				root, scratch := t.TempDir(), t.TempDir()
				source := adapterFixture()
				if newline == "CRLF" {
					source = bytes.ReplaceAll(source, []byte("\n"), []byte("\r\n"))
				}
				path := filepath.Join(root, filepath.FromSlash(adapterPath))
				writeTestFile(t, path, source)
				overlay, category := writeOverlay(root, scratch, source, mechanism)
				if category != "" {
					t.Fatal("overlay refused")
				}
				data, err := os.ReadFile(overlay)
				var mapping struct{ Replace map[string]string }
				if err != nil || json.Unmarshal(data, &mapping) != nil || len(mapping.Replace) != 1 {
					t.Fatal("one-file mapping")
				}
				changed, err := os.ReadFile(mapping.Replace[path])
				if err != nil || bytes.Equal(changed, source) || filepath.Dir(mapping.Replace[path]) != scratch {
					t.Fatal("overlay ownership")
				}
				var want []byte
				if mechanism == "M1" {
					want = bytes.Replace(source, []byte("if privacy &&"), []byte("if privacy && stage != \"created\" &&"), 1)
				} else {
					end := "\n\t}\n\tif privacy"
					replacement := "\n\t}\n\tp.reparse = false\n\tp.directory = f.directory\n\tif privacy"
					if newline == "CRLF" {
						end, replacement = strings.ReplaceAll(end, "\n", "\r\n"), strings.ReplaceAll(replacement, "\n", "\r\n")
					}
					want = bytes.Replace(source, []byte(end), []byte(replacement), 1)
				}
				if !bytes.Equal(changed, want) {
					t.Fatal("mutation scope")
				}
				actual, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(actual, source) {
					t.Fatal("actual source changed")
				}
			})
		}
	}
	for _, mechanism := range []string{"M1", "M2"} {
		for _, bad := range [][]byte{[]byte("package trust\n"), append(adapterFixture(), adapterFixture()...)} {
			if _, category := mutate(bad, mechanism); category != "mutation_anchor" {
				t.Fatal("ambiguous anchor accepted")
			}
		}
	}
	if _, category := replaceExactly([]byte("anchor"), "anchor", "anchor"); category != "mutation_noop" {
		t.Fatal("no-op accepted")
	}
	if _, category := mutate(adapterFixture(), "unknown"); category != "mutation_unknown" {
		t.Fatal("unknown mechanism accepted")
	}
	if representation([]byte("a\nb\n"), []byte("a\r\nb\r\n")) != "CRLF" || representation([]byte("a\nb\n"), []byte("a\r\nb\n")) != "" || representation([]byte("a\n"), []byte("b\n")) != "" {
		t.Fatal("checkout representation")
	}
	// Replay actual immutable source anchors, without ever changing those files.
	actual, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(adapterPath)))
	if err != nil {
		t.Fatal("frozen adapter unavailable")
	}
	for _, mechanism := range []string{"M1", "M2"} {
		for _, raw := range [][]byte{bytes.ReplaceAll(actual, []byte("\r\n"), []byte("\n")), bytes.ReplaceAll(bytes.ReplaceAll(actual, []byte("\r\n"), []byte("\n")), []byte("\n"), []byte("\r\n"))} {
			if _, category := mutate(raw, mechanism); category != "" {
				t.Fatal("frozen anchor drift")
			}
		}
	}
}

func event(action, test, output string) testEvent {
	return testEvent{Action: action, Package: packageName, Test: test, Output: output}
}

func encodeEvents(events []testEvent, exit int) commandResult {
	var data bytes.Buffer
	for _, e := range events {
		_ = json.NewEncoder(&data).Encode(e)
	}
	return commandResult{stdout: data.Bytes(), exit: exit}
}

func mutantEvents(w witness) []testEvent {
	parent := strings.Split(w.test, "/")[0]
	return []testEvent{event("start", "", ""), event("run", parent, ""), event("run", w.test, ""),
		event("output", w.test, fmt.Sprintf("    private_generation_windows_test.go:%d: %s\n", w.line, w.assertion)),
		event("fail", w.test, ""), event("fail", parent, ""), event("fail", "", "")}
}

func controlEvents(skips map[string]string) []testEvent {
	events := []testEvent{event("start", "", "")}
	for i, group := range groups {
		events = append(events, event("run", group, ""))
		var open []string
		for _, child := range groupCases[i] {
			name := group + "/" + child
			for len(open) > 0 && !strings.HasPrefix(name, open[len(open)-1]+"/") {
				events = append(events, event("pass", open[len(open)-1], ""))
				open = open[:len(open)-1]
			}
			events = append(events, event("run", name, ""))
			if reason := skips[name]; reason != "" {
				events = append(events, event("output", name, "    private_record_windows_test.go:210: "+reason+"\n"), event("skip", name, ""))
			} else {
				open = append(open, name)
			}
		}
		for i := len(open) - 1; i >= 0; i-- {
			events = append(events, event("pass", open[i], ""))
		}
		events = append(events, event("pass", group, ""))
	}
	return append(events, event("pass", "", ""))
}

func TestNativeProofEvents(t *testing.T) {
	for _, w := range witnesses {
		p := newPhase(w.mechanism, &w)
		if category := parseEvents(encodeEvents(mutantEvents(w), 1), &w, &p, packageName); category != "" {
			t.Fatalf("valid witness: %s", category)
		}
	}
	w := witnesses[0]
	tests := []struct {
		name string
		edit func([]testEvent) []testEvent
	}{
		{"missing-run", func(e []testEvent) []testEvent { return append(e[:2], e[3:]...) }},
		{"duplicate-run", func(e []testEvent) []testEvent { return append(e[:3], append([]testEvent{e[2]}, e[3:]...)...) }},
		{"conflicting-terminal", func(e []testEvent) []testEvent {
			return append(e[:5], append([]testEvent{event("pass", w.test, "")}, e[5:]...)...)
		}},
		{"target-skip", func(e []testEvent) []testEvent { e[4].Action = "skip"; return e }},
		{"unrelated-failure", func(e []testEvent) []testEvent { e[4].Test += "-other"; return e }},
		{"wrong-package", func(e []testEvent) []testEvent { e[3].Package = "other"; return e }},
		{"build-failure", func(e []testEvent) []testEvent { e[0].FailedBuild = packageName; return e }},
		{"build-event", func(e []testEvent) []testEvent { e[0].Action = "build-fail"; return e }},
		{"timeout", func(e []testEvent) []testEvent { e[3].Output = "panic: test timed out\n"; return e }},
		{"panic", func(e []testEvent) []testEvent { e[3].Output = "panic: interrupted\n"; return e }},
		{"marker-without-case", func(e []testEvent) []testEvent { return []testEvent{e[0], event("output", "", w.assertion), e[6]} }},
		{"marker-from-log-line", func(e []testEvent) []testEvent {
			e[3].Output = strings.ReplaceAll(e[3].Output, fmt.Sprintf(":%d:", w.line), fmt.Sprintf(":%d:", w.line-1))
			return e
		}},
		{"logged-marker-other-assertion", func(e []testEvent) []testEvent {
			e[3].Output += "    private_generation_windows_test.go:331: unrelated assertion\n"
			return e
		}},
		{"marker-in-unlocated-output", func(e []testEvent) []testEvent { e[3].Output = w.assertion + "\n"; return e }},
		{"duplicate-assertion", func(e []testEvent) []testEvent { e[3].Output += e[3].Output; return e }},
		{"missing-parent-fail", func(e []testEvent) []testEvent { return append(e[:5], e[6:]...) }},
		{"parent-terminal-before-child", func(e []testEvent) []testEvent { e[4], e[5] = e[5], e[4]; return e }},
		{"surviving-mutant", func(e []testEvent) []testEvent { e[4].Action = "pass"; return e }},
		{"duplicate-package", func(e []testEvent) []testEvent { return append(e, e[len(e)-1]) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newPhase("M1", &w)
			if parseEvents(encodeEvents(tc.edit(mutantEvents(w)), 1), &w, &p, packageName) == "" {
				t.Fatal("invalid proof accepted")
			}
		})
	}
	for _, name := range []string{"stderr", "malformed", "truncated", "killed", "zero-exit", "wrong-exit", "empty"} {
		t.Run(name, func(t *testing.T) {
			r := encodeEvents(mutantEvents(w), 1)
			switch name {
			case "stderr":
				r.stderr = []byte("private diagnostic")
			case "malformed":
				r.stdout = append([]byte("invalid\n"), r.stdout...)
			case "truncated":
				r.stdout = r.stdout[:len(r.stdout)-1]
			case "killed":
				r.category = "command_timeout"
			case "zero-exit":
				r.exit = 0
			case "wrong-exit":
				r.exit = 2
			case "empty":
				r.stdout = nil
			}
			p := newPhase("M1", &w)
			if parseEvents(r, &w, &p, packageName) == "" {
				t.Fatal("invalid result accepted")
			}
		})
	}
	t.Run("all-control-cases", func(t *testing.T) {
		p := newPhase("control", nil)
		if len(p.Inventory) != 53 {
			t.Fatal("required inventory drift")
		}
		if category := parseEvents(encodeEvents(controlEvents(nil), 0), nil, &p, packageName); category != "" {
			t.Fatalf("control: %s", category)
		}
		for _, item := range p.Inventory {
			if !item.Run || item.Terminal != "pass" {
				t.Fatal("control inventory")
			}
		}
	})
	for test, reasons := range historicalSkips() {
		for i, reason := range reasons {
			t.Run(fmt.Sprintf("B1/%s/%d", test, i), func(t *testing.T) {
				p := newPhase("control", nil)
				events := controlEvents(map[string]string{test: reason})
				if category := parseEvents(encodeEvents(events, 0), nil, &p, packageName); category != "" {
					t.Fatalf("historical qualification: %s", category)
				}
				found := false
				for _, item := range p.Inventory {
					if item.Test == test && item.Terminal == "skip" && item.Category == "B1_CAPABILITY_NOT_RUN" {
						found = true
					}
				}
				if !found {
					t.Fatal("qualification absent")
				}
				for j := range events {
					if events[j].Test == test && events[j].Action == "skip" {
						events[j].Action = "pass"
					}
				}
				p = newPhase("control", nil)
				if parseEvents(encodeEvents(events, 0), nil, &p, packageName) == "" {
					t.Fatal("reason without skip accepted")
				}
				for _, wrong := range []string{witnesses[0].test, groups[5] + "/extra-trustee", groups[5]} {
					p = newPhase("control", nil)
					events = controlEvents(map[string]string{wrong: reason})
					if wrong == groups[5] {
						for j := range events {
							if events[j].Test == wrong && events[j].Action == "pass" {
								events[j].Action = "skip"
							}
						}
					}
					if parseEvents(encodeEvents(events, 0), nil, &p, packageName) == "" {
						t.Fatal("qualification on wrong case accepted")
					}
				}
			})
		}
	}
	for _, name := range []string{"unknown-reason", "missing-group", "missing-case", "new-skip"} {
		t.Run(name, func(t *testing.T) {
			events := controlEvents(nil)
			switch name {
			case "unknown-reason":
				events = controlEvents(map[string]string{groups[5] + "/inherited-ace": "NOT RUN — unknown capability"})
			case "missing-group", "missing-case":
				target := groups[0]
				if name == "missing-case" {
					target = witnesses[0].test
				}
				var kept []testEvent
				for _, e := range events {
					if e.Test != target {
						kept = append(kept, e)
					}
				}
				events = kept
			case "new-skip":
				for i := range events {
					if events[i].Test == witnesses[0].test && events[i].Action == "pass" {
						events[i].Action = "skip"
					}
				}
			}
			p := newPhase("control", nil)
			if parseEvents(encodeEvents(events, 0), nil, &p, packageName) == "" {
				t.Fatal("invalid control accepted")
			}
		})
	}
	t.Run("early-error-retains-later-qualification", func(t *testing.T) {
		target := groups[5] + "/inherited-ace"
		events := controlEvents(map[string]string{target: "NOT RUN — inherited ACE not persistable"})
		events[0].FailedBuild = packageName
		p := newPhase("control", nil)
		if parseEvents(encodeEvents(events, 1), nil, &p, packageName) == "" {
			t.Fatal("early failure accepted")
		}
		for _, item := range p.Inventory {
			if item.Test == target {
				if !item.Run || item.Terminal != "skip" || item.Reason != "NOT RUN — inherited ACE not persistable" {
					t.Fatal("later qualification hidden")
				}
			}
		}
	})
	t.Run("skip-marker-plus-different-skip", func(t *testing.T) {
		target := groups[5] + "/inherited-ace"
		events := controlEvents(map[string]string{target: "NOT RUN — inherited ACE not persistable"})
		for i := range events {
			if events[i].Test == target && events[i].Action == "output" {
				events[i].Output += "    private_record_windows_test.go:211: different skip\n"
			}
		}
		p := newPhase("control", nil)
		if parseEvents(encodeEvents(events, 0), nil, &p, packageName) == "" {
			t.Fatal("unrelated skip accepted")
		}
	})
}

func TestNativeProofPrerequisiteFailures(t *testing.T) {
	for _, w := range witnesses {
		t.Run(w.test, func(t *testing.T) {
			for _, mode := range []string{"exact-prerequisite", "text-only", "site-only", "both-fatals", "both-fatals-reversed", "duplicate-prerequisite"} {
				t.Run(mode, func(t *testing.T) {
					events := mutantEvents(w)
					behavior := events[3].Output
					line, message := w.prerequisiteLine, w.prerequisite
					if mode == "text-only" {
						line = w.line
					}
					if mode == "site-only" {
						message = "private fixture diagnostic"
					}
					prerequisite := fmt.Sprintf("    private_generation_windows_test.go:%d: %s\n", line, message)
					events[3].Output = prerequisite
					wantStatus, wantCategory := "INCOMPLETE", "prerequisite_failed"
					switch mode {
					case "both-fatals":
						events[3].Output += behavior
					case "both-fatals-reversed":
						events[3].Output = behavior + prerequisite
					case "duplicate-prerequisite":
						events[3].Output += prerequisite
					}
					if strings.HasPrefix(mode, "both-fatals") || mode == "duplicate-prerequisite" {
						wantStatus, wantCategory = "FAIL", "wrong_assertion"
					}
					p := newPhase(w.mechanism, &w)
					run := func(context.Context, string, []string, []string, string, string) commandResult {
						return encodeEvents(events, 1)
					}
					runPhase(&p, &w, "", snapshot{}, "", nil, run)
					if p.Status != wantStatus || p.Category != wantCategory || p.Exit != 1 {
						t.Fatalf("prerequisite classification: %s / %s", p.Status, p.Category)
					}
					for _, item := range p.Inventory {
						if !item.Run || item.Terminal != "fail" {
							t.Fatal("prerequisite execution inventory lost")
						}
					}
					data, err := json.Marshal(p)
					if err != nil || bytes.Contains(data, []byte(message)) {
						t.Fatal("prerequisite raw diagnostic projected")
					}
				})
			}
		})
	}
}

func TestPrerequisiteFailureReceipt(t *testing.T) {
	for witnessIndex, w := range witnesses {
		t.Run(w.test, func(t *testing.T) {
			f := fixture(t)
			phaseIndex := []int{1, 3, 4}[witnessIndex]
			f.goResult = func(n int) commandResult {
				if n == phaseIndex+1 {
					events := mutantEvents(w)
					events[3].Output = fmt.Sprintf("    private_generation_windows_test.go:%d: %s\n", w.prerequisiteLine, w.prerequisite)
					return encodeEvents(events, 1)
				}
				for i, index := range []int{1, 3, 4} {
					if n == index+1 {
						return encodeEvents(mutantEvents(witnesses[i]), 1)
					}
				}
				return encodeEvents(controlEvents(nil), 0)
			}
			path := filepath.Join(t.TempDir(), "receipt.json")
			var out bytes.Buffer
			if runCLI([]string{"--output", path}, &out, "windows", "amd64", nil, f.run) != 1 {
				t.Fatal("prerequisite failure did not exit nonzero")
			}
			data, err := os.ReadFile(path)
			var r receipt
			if err != nil || json.Unmarshal(data, &r) != nil || r.Status != "INCOMPLETE" || r.Category != "phase_incomplete" || !r.Cleanup || !r.Preserved || r.NativeAccept || f.goCalls != 6 {
				t.Fatal("prerequisite receipt or restored execution incomplete")
			}
			for i, p := range r.Phases {
				if i == phaseIndex {
					if p.Status != "INCOMPLETE" || p.Category != "prerequisite_failed" || p.Exit != 1 || p.Assertion != w.category {
						t.Fatal("prerequisite receipt classification lost")
					}
				} else if p.Status != "PASS" {
					t.Fatal("prerequisite failure displaced another phase")
				}
			}
			if bytes.Contains(data, []byte(w.prerequisite)) || strings.Contains(out.String(), w.prerequisite) {
				t.Fatal("prerequisite diagnostic escaped fixed categories")
			}
		})
	}
}

type proofFixture struct {
	root, scratch string
	raw           map[string][]byte
	goCalls       int
	mutateCall    func(int)
	goResult      func(int) commandResult
	plans         [][]string
}

func fixture(t *testing.T) *proofFixture {
	t.Helper()
	f := &proofFixture{root: t.TempDir(), scratch: t.TempDir(), raw: map[string][]byte{}}
	for _, path := range sourcePaths {
		f.raw[path] = []byte("package trust\n")
		if path == adapterPath {
			f.raw[path] = adapterFixture()
		}
		writeTestFile(t, filepath.Join(f.root, filepath.FromSlash(path)), f.raw[path])
	}
	writeTestFile(t, filepath.Join(f.root, "index"), []byte("index fixture"))
	return f
}

func (f *proofFixture) run(_ context.Context, program string, args, _ []string, _, _ string) commandResult {
	if program == "git" {
		value := ""
		switch strings.Join(args, " ") {
		case "rev-parse --show-toplevel":
			value = f.root + "\n"
		case "rev-parse HEAD", "rev-parse HEAD^{tree}":
			value = strings.Repeat("a", 40) + "\n"
		case "rev-parse --git-path index":
			value = "index\n"
		case "status --porcelain=v1 --untracked-files=no":
		default:
			if len(args) == 2 && args[0] == "show" {
				value = string(f.raw[strings.TrimPrefix(strings.TrimPrefix(args[1], "HEAD:"), ":")])
			} else if len(args) > 1 && args[0] == "diff" {
				return commandResult{}
			} else {
				return commandResult{exit: 1}
			}
		}
		return commandResult{stdout: []byte(value)}
	}
	f.goCalls++
	f.plans = append(f.plans, append([]string(nil), args...))
	if f.mutateCall != nil {
		f.mutateCall(f.goCalls)
	}
	if f.goResult != nil {
		return f.goResult(f.goCalls)
	}
	for _, w := range witnesses {
		if strings.Contains(strings.Join(args, " "), "^"+strings.Split(w.test, "/")[1]+"$") {
			return encodeEvents(mutantEvents(w), 1)
		}
	}
	return encodeEvents(controlEvents(nil), 0)
}

func TestProofPreservesInputs(t *testing.T) {
	t.Run("qualification-in-every-control", func(t *testing.T) {
		f := fixture(t)
		f.goResult = func(n int) commandResult {
			if n == 2 {
				return encodeEvents(mutantEvents(witnesses[0]), 1)
			}
			if n == 4 || n == 5 {
				return encodeEvents(mutantEvents(witnesses[n-3]), 1)
			}
			return encodeEvents(controlEvents(map[string]string{groups[5] + "/inherited-ace": "NOT RUN — inherited ACE not persistable"}), 0)
		}
		r := newReceipt("windows", "amd64")
		proof(&r, f.scratch, nil, f.run)
		if r.Status != "PASS" || r.B1Coverage != "QUALIFIED_CAPABILITY_NOT_RUN" {
			t.Fatal("qualified coverage")
		}
		for _, index := range []int{0, 2, 5} {
			found := false
			for _, item := range r.Phases[index].Inventory {
				if item.Reason == "NOT RUN — inherited ACE not persistable" && item.Terminal == "skip" {
					found = true
				}
			}
			if !found {
				t.Fatal("control qualification lost")
			}
		}
	})
	for _, category := range []string{"index-change", "mode-change", "nonregular-source"} {
		t.Run(category, func(t *testing.T) {
			f := fixture(t)
			if category == "nonregular-source" {
				path := filepath.Join(f.root, filepath.FromSlash(adapterPath))
				if os.Remove(path) != nil || os.Mkdir(path, 0o700) != nil {
					t.Fatal("nonregular fixture")
				}
			}
			run := func(ctx context.Context, program string, args, environ []string, cwd, scratch string) commandResult {
				plan := strings.Join(args, " ")
				if program == "git" && category == "index-change" && strings.Contains(plan, "--cached") {
					return commandResult{exit: 1}
				}
				if program == "git" && category == "mode-change" && strings.Contains(plan, "--summary") {
					return commandResult{stdout: []byte("mode change")}
				}
				return f.run(ctx, program, args, environ, cwd, scratch)
			}
			r := newReceipt("windows", "amd64")
			proof(&r, f.scratch, nil, run)
			if r.Status == "PASS" || f.goCalls != 0 {
				t.Fatal("preexisting metadata alteration accepted")
			}
		})
	}
	t.Run("receipt-and-owned-scratch", func(t *testing.T) {
		f := fixture(t)
		path := filepath.Join(t.TempDir(), "receipt.json")
		var out bytes.Buffer
		var runScratch string
		run := func(ctx context.Context, program string, args, environ []string, cwd, scratch string) commandResult {
			runScratch = scratch
			settings := strings.Join(environ, "\n")
			for _, required := range []string{"GIT_OPTIONAL_LOCKS=0", "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOOS=windows", "GOARCH=amd64"} {
				if !strings.Contains(settings, required) {
					t.Fatal("child mapping missing")
				}
			}
			return f.run(ctx, program, args, environ, cwd, scratch)
		}
		if runCLI([]string{"--output", path}, &out, "windows", "amd64", nil, run) != 0 {
			t.Fatal("portable injected CLI")
		}
		data, err := os.ReadFile(path)
		var r receipt
		if err != nil || json.Unmarshal(data, &r) != nil || r.Status != "PASS" || !r.Cleanup || !r.Preserved || r.NativeAccept || len(r.Provenance.Sources) != 4 {
			t.Fatal("sanitized receipt")
		}
		if _, err := os.Stat(runScratch); !os.IsNotExist(err) {
			t.Fatal("owned scratch retained")
		}
	})
	t.Run("crlf-checkout-preserved", func(t *testing.T) {
		f := fixture(t)
		for path, raw := range f.raw {
			writeTestFile(t, filepath.Join(f.root, filepath.FromSlash(path)), bytes.ReplaceAll(raw, []byte("\n"), []byte("\r\n")))
		}
		r := newReceipt("windows", "amd64")
		proof(&r, f.scratch, nil, f.run)
		if r.Status != "PASS" || !r.Preserved {
			t.Fatal("CRLF proof")
		}
		for _, source := range r.Provenance.Sources {
			if source.Representation != "CRLF" {
				t.Fatal("CRLF provenance")
			}
		}
	})
	t.Run("cleanup-uncertain", func(t *testing.T) {
		f := fixture(t)
		f.mutateCall = func(n int) {
			if n == 2 {
				path := filepath.Join(f.scratch, "M1-overlay.json")
				if os.Remove(path) != nil {
					t.Fatal("cleanup fixture")
				}
				writeTestFile(t, filepath.Join(path, "owned-child"), []byte("fixture"))
			}
		}
		r := newReceipt("windows", "amd64")
		proof(&r, f.scratch, nil, f.run)
		if r.Status == "PASS" || r.Category != "cleanup_uncertain" || !r.Preserved {
			t.Fatal("uncertain cleanup accepted")
		}
	})
	t.Run("complete-order", func(t *testing.T) {
		f := fixture(t)
		r := newReceipt("windows", "amd64")
		proof(&r, f.scratch, nil, f.run)
		if r.Status != "PASS" || !r.Preserved || f.goCalls != 6 || r.NativeAccept {
			t.Fatalf("proof state: %s", r.Category)
		}
		for i, args := range f.plans {
			joined := strings.Join(args, " ")
			mutant := i == 1 || i == 3 || i == 4
			if strings.Contains(joined, "-overlay") != mutant {
				t.Fatal("restored overlay state")
			}
			if !strings.Contains(joined, "-count=1") || !strings.Contains(joined, "-json") {
				t.Fatal("command plan")
			}
			if !mutant && !strings.Contains(joined, "-timeout=10m") {
				t.Fatal("control timeout")
			}
			if mutant && !strings.Contains(joined, "-timeout=2m") {
				t.Fatal("mutation timeout")
			}
		}
		for path, want := range f.raw {
			got, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(path)))
			if err != nil || !bytes.Equal(got, want) {
				t.Fatal("source preservation")
			}
		}
		for _, mechanism := range []string{"M1", "M2"} {
			if _, err := os.Stat(filepath.Join(f.scratch, mechanism+"-overlay.json")); !os.IsNotExist(err) {
				t.Fatal("overlay not discarded")
			}
		}
	})
	t.Run("earlier-control-cannot-be-replaced", func(t *testing.T) {
		f := fixture(t)
		r := newReceipt("windows", "amd64")
		f.goResult = func(n int) commandResult {
			if n == 2 {
				return encodeEvents(mutantEvents(witnesses[0]), 1)
			}
			if n == 3 {
				return commandResult{category: "tool_unavailable", exit: -1}
			}
			if n == 4 || n == 5 {
				return encodeEvents(mutantEvents(witnesses[n-3]), 1)
			}
			return encodeEvents(controlEvents(nil), 0)
		}
		proof(&r, f.scratch, nil, f.run)
		if r.Status == "PASS" || r.Phases[2].Status == "PASS" || r.Phases[5].Status != "PASS" || r.B1Coverage != "PARTIAL" {
			t.Fatal("missing earlier restoration masked")
		}
	})
	for _, name := range []string{"preexisting-source", "source-during-proof", "index-during-proof"} {
		t.Run(name, func(t *testing.T) {
			f := fixture(t)
			r := newReceipt("windows", "amd64")
			if name == "preexisting-source" {
				writeTestFile(t, filepath.Join(f.root, filepath.FromSlash(adapterPath)), []byte("altered"))
			} else {
				f.mutateCall = func(n int) {
					if n == 2 {
						path := filepath.Join(f.root, filepath.FromSlash(adapterPath))
						if name == "index-during-proof" {
							path = filepath.Join(f.root, "index")
						}
						writeTestFile(t, path, []byte("altered"))
					}
				}
			}
			proof(&r, f.scratch, nil, f.run)
			if r.Status == "PASS" || r.Preserved {
				t.Fatal("input alteration accepted")
			}
			if name == "preexisting-source" && f.goCalls != 0 {
				t.Fatal("altered input executed")
			}
		})
	}
	t.Run("unsupported-platform-and-existing-output", func(t *testing.T) {
		calls := 0
		run := func(context.Context, string, []string, []string, string, string) commandResult {
			calls++
			return commandResult{}
		}
		for _, platform := range [][2]string{{"linux", "amd64"}, {"windows", "arm64"}} {
			var out bytes.Buffer
			path := filepath.Join(t.TempDir(), "receipt.json")
			if runCLI([]string{"--output", path}, &out, platform[0], platform[1], nil, run) == 0 || calls != 0 {
				t.Fatal("platform spawned child")
			}
			before, _ := os.ReadFile(path)
			if runCLI([]string{"--output", path}, &out, "windows", "amd64", nil, run) == 0 || calls != 0 {
				t.Fatal("overwrite spawned child")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("preexisting output overwritten")
			}
		}
	})
	t.Run("environment", func(t *testing.T) {
		input := []string{"WITSELF_TEST_DATABASE_URL=private", "WITSELF_DATABASE_URL=private", "WITSELF_LOCALCELL_PGBIN=private", "WITSELF_TEST_REQUIRE_DATABASE=1", "GOSUMDB=existing", "GOFLAGS=wrong", "goproxy=wrong", "PATH=existing"}
		got := childEnvironment(input, "windows", "amd64")
		joined := strings.Join(got, "\n")
		if strings.Contains(joined, "private") || strings.Contains(joined, "WITSELF_") || strings.Contains(joined, "wrong") {
			t.Fatal("unsafe environment retained")
		}
		for _, setting := range []string{"GIT_OPTIONAL_LOCKS=0", "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOOS=windows", "GOARCH=amd64", "GOSUMDB=existing"} {
			if !strings.Contains(joined, setting) {
				t.Fatal("missing child setting")
			}
		}
		if input[0] != "WITSELF_TEST_DATABASE_URL=private" {
			t.Fatal("parent environment changed")
		}
	})
}

func TestProofRedaction(t *testing.T) {
	secret := "private-fixture-value-" + strings.Repeat("b", 64)
	r := newReceipt("windows", "amd64")
	raw := encodeEvents(mutantEvents(witnesses[0]), 1)
	raw.stderr = []byte(secret)
	r.Phases[1].Category = parseEvents(raw, &witnesses[0], &r.Phases[1], packageName)
	var out bytes.Buffer
	printSummary(&out, r)
	data, err := json.Marshal(r)
	if err != nil || bytes.Contains(data, []byte(secret)) || strings.Contains(out.String(), secret) {
		t.Fatal("private output projected")
	}
	r.Provenance.Head = strings.Repeat("a", 40)
	r.Provenance.IndexSHA256 = strings.Repeat("b", 64)
	out.Reset()
	printSummary(&out, r)
	if strings.Contains(out.String(), strings.Repeat("a", 40)) || strings.Contains(out.String(), strings.Repeat("b", 64)) {
		t.Fatal("identity printed")
	}
	logFile, err := os.CreateTemp(t.TempDir(), "log-")
	if err != nil {
		t.Fatal("log fixture")
	}
	b := boundedLog{f: logFile}
	block := bytes.Repeat([]byte("x"), maxLogBytes+1)
	if n, err := b.Write(block); err != nil || n != len(block) || !b.overflow || b.n != maxLogBytes {
		t.Fatal("log bound")
	}
	if err := logFile.Close(); err != nil {
		t.Fatal("log close")
	}
}

func TestGoOverlayExecution(t *testing.T) {
	// Real portable compiler evidence; this does not execute the Windows adapter.
	root, scratch := t.TempDir(), t.TempDir()
	// The go command resolves its working directory, so an overlay key under a
	// symlinked temporary directory (the macOS default) would never match.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	source := []byte("package overlayproof\nfunc value() int { return 1 }\n")
	writeTestFile(t, filepath.Join(root, "go.mod"), []byte("module overlayproof\n\ngo 1.24\n"))
	writeTestFile(t, filepath.Join(root, "value.go"), source)
	writeTestFile(t, filepath.Join(root, "value_test.go"), []byte(`package overlayproof
import ("os"; "strings"; "testing")
func TestOverlay(t *testing.T) {
 raw, err := os.ReadFile("value.go")
 if err != nil || !strings.Contains(string(raw), "return 1") { t.Fatal("runtime_original_missing") }
 t.Log("runtime_original_confirmed")
 if value() != 1 { t.Fatal("compiled_overlay_effect") }
}
`))
	changed := filepath.Join(scratch, "changed.go")
	writeTestFile(t, changed, bytes.Replace(source, []byte("return 1"), []byte("return 2"), 1))
	overlay := filepath.Join(scratch, "overlay.json")
	mapping, _ := json.Marshal(struct{ Replace map[string]string }{map[string]string{filepath.Join(root, "value.go"): changed}})
	writeTestFile(t, overlay, mapping)
	environ := childEnvironment(os.Environ(), runtime.GOOS, runtime.GOARCH)
	for i := range environ {
		if strings.HasPrefix(environ[i], "GOFLAGS=") {
			environ[i] = "GOFLAGS=-mod=readonly -buildvcs=false"
		}
	}
	for i := 0; i < 3; i++ {
		args := []string{"test", "-json", "-count=1", "-timeout=30s"}
		if i == 1 {
			args = append(args, "-overlay", overlay)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.WaitDelay = 5 * time.Second
		cmd.Dir = root
		cmd.Env = environ
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		cancel()
		if (err != nil) != (i == 1) || stderr.Len() != 0 {
			t.Fatal("portable overlay process result")
		}
		decoder := json.NewDecoder(&stdout)
		run, terminal, runtimeRead, assertion := false, "", false, false
		for decoder.More() {
			var e testEvent
			if decoder.Decode(&e) != nil {
				t.Fatal("portable events")
			}
			if e.Package != "overlayproof" {
				t.Fatal("portable package")
			}
			if e.Test == "TestOverlay" {
				if e.Action == "run" {
					run = true
				}
				if e.Action == "pass" || e.Action == "fail" {
					terminal = e.Action
				}
				if strings.Contains(e.Output, "value_test.go:6: runtime_original_confirmed") {
					runtimeRead = true
				}
				if strings.Contains(e.Output, "value_test.go:7: compiled_overlay_effect") {
					assertion = true
				}
			}
		}
		want := "pass"
		if i == 1 {
			want = "fail"
		}
		if !run || terminal != want || !runtimeRead || assertion != (i == 1) {
			t.Fatal("portable overlay witness")
		}
		actual, readErr := os.ReadFile(filepath.Join(root, "value.go"))
		if readErr != nil || !bytes.Equal(actual, source) {
			t.Fatal("portable actual source changed")
		}
	}
}

// These source pins make a failure at a behavioral fatal imply that the
// fixture prerequisites passed earlier in that same execution, on every host.
func TestNativeWitnessSourceContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "localcell", "trust", "private_generation_windows_test.go"))
	if err != nil {
		t.Fatal("native witness source unavailable")
	}
	lf := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	for _, newline := range []string{"LF", "CRLF"} {
		t.Run(newline, func(t *testing.T) {
			source := lf
			if newline == "CRLF" {
				source = bytes.ReplaceAll(lf, []byte("\n"), []byte("\r\n"))
			}
			fset := token.NewFileSet()
			file, parseErr := parser.ParseFile(fset, "private_generation_windows_test.go", source, 0)
			if parseErr != nil {
				t.Fatal("native witness source does not parse")
			}
			for _, w := range witnesses {
				t.Run(w.test, func(t *testing.T) {
					guardCondition := "!observed"
					behaviorCondition := "result != ReasonStateIO || writes != 0"
					writeExpression := `g.write(ctx, "blocked", 1, bytes.NewReader([]byte{1}))`
					prerequisiteText := "native fixture prerequisite failed: mismatch observation"
					if w.mechanism == "M1" {
						guardCondition = "!changed || !observed"
						behaviorCondition += " || g.completed"
						writeExpression = `g.write(ctx, "blocked.bin", 1, bytes.NewReader([]byte{1}))`
						prerequisiteText = "native fixture prerequisite failed: initial privacy observation"
					}
					matches := 0
					ast.Inspect(file, func(node ast.Node) bool {
						block, ok := node.(*ast.BlockStmt)
						if !ok {
							return true
						}
						for i, statement := range block.List {
							behavior, ok := statement.(*ast.IfStmt)
							if !ok {
								continue
							}
							fatal, text, ok := sourceContractFatal(behavior)
							if !ok || text != w.assertion {
								continue
							}
							matches++
							if fset.Position(fatal.Pos()).Line != w.line {
								t.Fatal("behavioral fatal line does not match tool pin")
							}
							if sourceContractText(t, fset, behavior.Cond) != behaviorCondition || sourceContractReferences(behavior.Cond, "changed", "observed") {
								t.Fatal("behavioral condition includes prerequisites or drifted")
							}
							if i < 2 {
								t.Fatal("behavioral assertion lacks preceding prerequisite and write")
							}
							guard, ok := block.List[i-1].(*ast.IfStmt)
							if !ok {
								t.Fatal("prerequisite guard is not immediately before behavioral assertion")
							}
							guardFatal, guardText, ok := sourceContractFatal(guard)
							if !ok || guardText != prerequisiteText || guardText != w.prerequisite || fset.Position(guardFatal.Pos()).Line != w.prerequisiteLine {
								t.Fatal("prerequisite fatal does not match source and tool pins")
							}
							assignment, ok := block.List[i-2].(*ast.AssignStmt)
							if !ok || assignment.Tok != token.DEFINE || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 {
								t.Fatal("write result is not captured immediately before guard")
							}
							result, ok := assignment.Lhs[0].(*ast.Ident)
							if !ok || result.Name != "result" || sourceContractText(t, fset, assignment.Rhs[0]) != writeExpression {
								t.Fatal("captured write result drifted")
							}
							if sourceContractText(t, fset, guard.Cond) != guardCondition || sourceContractReferences(guard.Cond, "writes", "completed", result.Name) {
								t.Fatal("prerequisite guard includes behavior or drifted")
							}
						}
						return true
					})
					if matches != 1 {
						t.Fatal("behavioral fatal is missing, duplicated, or structurally changed")
					}
				})
			}
		})
	}
}

func sourceContractFatal(statement *ast.IfStmt) (*ast.CallExpr, string, bool) {
	if statement.Init != nil || statement.Else != nil || len(statement.Body.List) != 1 {
		return nil, "", false
	}
	expression, ok := statement.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return nil, "", false
	}
	call, ok := expression.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 || call.Ellipsis.IsValid() {
		return nil, "", false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Fatal" {
		return nil, "", false
	}
	receiver, ok := selector.X.(*ast.Ident)
	if !ok || receiver.Name != "t" {
		return nil, "", false
	}
	literal, ok := call.Args[0].(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return nil, "", false
	}
	text, err := strconv.Unquote(literal.Value)
	return call, text, err == nil
}

func sourceContractText(t *testing.T, fset *token.FileSet, node ast.Node) string {
	t.Helper()
	var output bytes.Buffer
	if format.Node(&output, fset, node) != nil {
		t.Fatal("source contract expression could not be formatted")
	}
	return output.String()
}

func sourceContractReferences(node ast.Node, names ...string) bool {
	found := false
	ast.Inspect(node, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if ok {
			for _, name := range names {
				found = found || identifier.Name == name
			}
		}
		return true
	})
	return found
}
