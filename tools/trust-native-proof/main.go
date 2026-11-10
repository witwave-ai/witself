// Command trust-native-proof retains bounded, sanitized Windows owner evidence.
// It never changes the checkout: mutations exist only in compiler overlays.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	packageName = "github.com/witwave-ai/witself/internal/localcell/trust"
	adapterPath = "internal/localcell/trust/private_generation_windows.go"
	planVersion = "trust-native-proof-v1"
	maxLogBytes = 32 << 20
)

var sourcePaths = []string{
	"internal/localcell/trust/private_generation.go",
	"internal/localcell/trust/private_generation_test.go",
	adapterPath,
	"internal/localcell/trust/private_generation_windows_test.go",
}

var groups = []string{
	"TestWindowsTrustPrivateGenerationCreateAndReopen",
	"TestWindowsTrustPrivateGenerationPrivacyRefusals",
	"TestWindowsTrustPrivateGenerationReparseAndReplacement",
	"TestWindowsTrustPrivateGenerationFailureCleanupAndHandles",
	"TestWindowsTrustPrivateRecordCreateAndReopen",
	"TestWindowsTrustPrivateRecordPrivacyRefusals",
	"TestWindowsTrustPrivateRecordReparseAndReplacement",
	"TestWindowsTrustPrivateRecordFenceAndPublication",
	"TestWindowsTrustPublicPersistenceStillRefuses",
}

// These are the complete cases of the frozen D0A and B1 test groups. New cases
// require deliberate review of the plan, rather than silently broadening proof.
var groupCases = [][]string{
	nil,
	{"null-dacl", "unprotected-dacl", "permissive-dacl", "wrong-owner", "initial-owned-file-privacy", "unsupported-parent-type-and-path", "existing-root"},
	{"native-file-replacement", "native-nested-junction", "observed-reparse", "observed-type", "observed-identity"},
	{"write", "sync", "reopen", "remove", "close", "closed", "close-parent", "two-creators"},
	{"directory-creation-failure-cleanup"},
	{"wrong-owner", "extra-trustee", "unprotected-dacl", "inherited-ace", "missing-system", "invalid-rights", "unsafe-directory"},
	{"reparse-leaf", "reparse-ancestor", "parent-replaced", "physical-parent-replaced", "leaf-replaced", "target-replaced-before-publication", "synthetic-additional-reparse-tags"},
	{"contending-writer", "abandoned-fence", "reader-and-bounded-replacement", "creation-validation-failure", "creation-validation-failure/temporary-created", "creation-validation-failure/fence-created", "created-object-confinement-failure", "prepublication-failure", "postpublication-failure"},
	nil,
}

type witness struct {
	mechanism        string
	test             string
	line             int
	assertion        string
	category         string
	prerequisiteLine int
	prerequisite     string
}

var witnesses = []witness{
	{"M1", groups[1] + "/initial-owned-file-privacy", 332, "initial native privacy refusal did not precede first payload write", "initial_privacy_before_write", 329, "native fixture prerequisite failed: initial privacy observation"},
	{"M2", groups[2] + "/observed-reparse", 504, "observed native mismatch reached a payload effect", "observed_mismatch_before_effect", 501, "native fixture prerequisite failed: mismatch observation"},
	{"M2", groups[2] + "/observed-type", 504, "observed native mismatch reached a payload effect", "observed_mismatch_before_effect", 501, "native fixture prerequisite failed: mismatch observation"},
}

func historicalSkips() map[string][]string {
	privacy, reparse := groups[5]+"/", groups[6]+"/"
	return map[string][]string{
		privacy + "inherited-ace":    {"NOT RUN — inherited ACE not persistable"},
		privacy + "wrong-owner":      {"NOT RUN — distinct privileged owner prerequisite unavailable", "NOT RUN — privileged wrong-owner fixture unavailable"},
		privacy + "missing-system":   {"NOT RUN — separate system principal unavailable"},
		reparse + "reparse-leaf":     {"NOT RUN — native junction handle prerequisite unavailable", "NOT RUN — native junction creation prerequisite unavailable"},
		reparse + "reparse-ancestor": {"NOT RUN — native junction handle prerequisite unavailable", "NOT RUN — native junction creation prerequisite unavailable"},
	}
}

type inventory struct {
	Test     string `json:"test"`
	Run      bool   `json:"run"`
	Terminal string `json:"terminal"`
	Category string `json:"category,omitempty"`
	Reason   string `json:"capability_reason,omitempty"`
	NotRun   bool   `json:"not_run_observed,omitempty"`
}

type phase struct {
	Name          string      `json:"name"`
	Status        string      `json:"status"`
	Category      string      `json:"category,omitempty"`
	Assertion     string      `json:"assertion_category,omitempty"`
	Exit          int         `json:"exit"`
	Inventory     []inventory `json:"inventory"`
	UnknownEvents int         `json:"unknown_test_events"`
	UnknownSkips  int         `json:"unknown_test_skips"`
}

type sourceIdentity struct {
	Path           string `json:"path"`
	RawSHA256      string `json:"raw_sha256"`
	BlobSHA256     string `json:"blob_sha256"`
	Representation string `json:"representation"`
}

type provenance struct {
	Head         string           `json:"head"`
	Tree         string           `json:"tree"`
	IndexSHA256  string           `json:"index_sha256"`
	StatusSHA256 string           `json:"tracked_status_sha256"`
	Sources      []sourceIdentity `json:"sources"`
}

type receipt struct {
	Version      string     `json:"command_plan_version"`
	OS           string     `json:"os"`
	Arch         string     `json:"arch"`
	Status       string     `json:"status"`
	Category     string     `json:"category"`
	Provenance   provenance `json:"provenance"`
	Phases       []phase    `json:"phases"`
	Preserved    bool       `json:"source_and_index_preserved"`
	Cleanup      bool       `json:"scratch_removed"`
	B1Coverage   string     `json:"b1_coverage"`
	NativeAccept bool       `json:"owner_acceptance"`
}

func requiredCases(w *witness) []string {
	if w != nil {
		return []string{strings.Split(w.test, "/")[0], w.test}
	}
	var names []string
	for i, group := range groups {
		names = append(names, group)
		for _, child := range groupCases[i] {
			names = append(names, group+"/"+child)
		}
	}
	return names
}

func newPhase(name string, w *witness) phase {
	p := phase{Name: name, Status: "NOT_RUN", Exit: -1}
	if w != nil {
		p.Assertion = w.category
	}
	for _, test := range requiredCases(w) {
		p.Inventory = append(p.Inventory, inventory{Test: test, Terminal: "NOT_RUN"})
	}
	return p
}

func newReceipt(goos, arch string) receipt {
	return receipt{Version: planVersion, OS: goos, Arch: arch, Status: "INCOMPLETE", Category: "not_started", B1Coverage: "NOT_RUN", Phases: []phase{
		newPhase("control_initial", nil), newPhase("M1", &witnesses[0]), newPhase("control_restored_M1", nil),
		newPhase("M2_reparse", &witnesses[1]), newPhase("M2_type", &witnesses[2]), newPhase("control_restored_M2", nil),
	}}
}

// No caller-provided error, command output, environment, or path is projected.
func printSummary(out io.Writer, r receipt) {
	_, _ = fmt.Fprintf(out, "proof status=%s category=%s\n", r.Status, r.Category)
	for _, p := range r.Phases {
		_, _ = fmt.Fprintf(out, "phase=%s status=%s category=%s\n", p.Name, p.Status, p.Category)
		for _, item := range p.Inventory {
			if item.Terminal == "skip" || item.Terminal == "NOT_RUN" || item.NotRun {
				_, _ = fmt.Fprintf(out, "test=%s status=%s category=%s\n", item.Test, item.Terminal, item.Category)
			}
		}
	}
}

func childEnvironment(input []string, goos, arch string) []string {
	m := map[string]string{}
	removed := map[string]bool{"WITSELF_TEST_DATABASE_URL": true, "WITSELF_DATABASE_URL": true, "WITSELF_LOCALCELL_PGBIN": true, "WITSELF_TEST_REQUIRE_DATABASE": true}
	for _, item := range input {
		key, value, ok := strings.Cut(item, "=")
		if ok && !removed[strings.ToUpper(key)] {
			m[strings.ToUpper(key)] = value
		}
	}
	for key, value := range map[string]string{"GIT_OPTIONAL_LOCKS": "0", "GOFLAGS": "-mod=readonly", "GOPROXY": "off", "GOTOOLCHAIN": "local", "GOWORK": "off", "GOOS": goos, "GOARCH": arch} {
		m[key] = value
	}
	var result []string
	for key, value := range m {
		result = append(result, key+"="+value)
	}
	sort.Strings(result)
	return result
}

type commandResult struct {
	stdout, stderr []byte
	exit           int
	category       string
}

type commandRunner func(context.Context, string, []string, []string, string, string) commandResult

// Each stream has an independent bounded private log, closed before readback.
type boundedLog struct {
	f        *os.File
	n        int
	overflow bool
}

func (b *boundedLog) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > maxLogBytes-b.n {
		b.overflow = true
		p = p[:maxLogBytes-b.n]
	}
	written, err := b.f.Write(p)
	b.n += written
	if err != nil {
		return written, err
	}
	return n, nil
}

func execute(ctx context.Context, program string, args, environ []string, cwd, scratch string) commandResult {
	result := commandResult{exit: -1}
	out, err := os.CreateTemp(scratch, "stdout-")
	if err != nil {
		result.category = "log_io"
		return result
	}
	errout, err := os.CreateTemp(scratch, "stderr-")
	if err != nil {
		_ = out.Close()
		result.category = "log_io"
		return result
	}
	stdout, stderr := &boundedLog{f: out}, &boundedLog{f: errout}
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = cwd, environ, stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run()
	closeOut, closeErr := out.Close(), errout.Close()
	if cmd.ProcessState != nil {
		result.exit = cmd.ProcessState.ExitCode()
	}
	switch {
	case ctx.Err() != nil:
		result.category = "command_timeout"
	case stdout.overflow || stderr.overflow:
		result.category = "log_limit"
	case closeOut != nil || closeErr != nil:
		result.category = "log_io"
	case err != nil && result.exit < 0:
		result.category = "tool_unavailable"
	}
	result.stdout, err = os.ReadFile(out.Name())
	if err != nil {
		result.category = "log_io"
	}
	result.stderr, err = os.ReadFile(errout.Name())
	if err != nil {
		result.category = "log_io"
	}
	return result
}

type snapshot struct {
	identity provenance
	root     string
	index    string
	indexRaw []byte
	status   []byte
	raw      map[string][]byte
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func gitRead(run commandRunner, environ []string, cwd, scratch string, args ...string) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r := run(ctx, "git", args, environ, cwd, scratch)
	return r.stdout, r.exit == 0 && r.category == "" && len(r.stderr) == 0
}

var gitIdentity = regexp.MustCompile(`^[a-f0-9]{40}(?:[a-f0-9]{24})?$`)

func takeSnapshot(run commandRunner, environ []string, scratch string) (snapshot, string) {
	s := snapshot{raw: map[string][]byte{}}
	root, ok := gitRead(run, environ, "", scratch, "rev-parse", "--show-toplevel")
	if !ok {
		return s, "checkout_unavailable"
	}
	s.root = strings.TrimSpace(string(root))
	if relative, err := filepath.Rel(s.root, scratch); err != nil || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return s, "scratch_inside_checkout"
	}
	for _, item := range []struct {
		ref string
		dst *string
	}{{"HEAD", &s.identity.Head}, {"HEAD^{tree}", &s.identity.Tree}} {
		value, valid := gitRead(run, environ, s.root, scratch, "rev-parse", item.ref)
		*item.dst = strings.TrimSpace(string(value))
		if !valid || !gitIdentity.MatchString(*item.dst) {
			return s, "identity_unavailable"
		}
	}
	index, ok := gitRead(run, environ, s.root, scratch, "rev-parse", "--git-path", "index")
	if !ok {
		return s, "index_unavailable"
	}
	s.index = strings.TrimSpace(string(index))
	if !filepath.IsAbs(s.index) {
		s.index = filepath.Join(s.root, s.index)
	}
	var err error
	s.indexRaw, err = os.ReadFile(s.index)
	if err != nil {
		return s, "index_unavailable"
	}
	s.identity.IndexSHA256 = digest(s.indexRaw)
	s.status, ok = gitRead(run, environ, s.root, scratch, "status", "--porcelain=v1", "--untracked-files=no")
	if !ok {
		return s, "status_unavailable"
	}
	s.identity.StatusSHA256 = digest(s.status)
	indexArgs := append([]string{"diff", "--no-ext-diff", "--no-textconv", "--cached", "--quiet", "HEAD", "--"}, sourcePaths...)
	if _, valid := gitRead(run, environ, s.root, scratch, indexArgs...); !valid {
		return s, "source_index_altered"
	}
	modeArgs := append([]string{"diff", "--no-ext-diff", "--no-textconv", "--summary", "--"}, sourcePaths...)
	if modes, valid := gitRead(run, environ, s.root, scratch, modeArgs...); !valid || len(modes) != 0 {
		return s, "source_mode_altered"
	}
	for _, path := range sourcePaths {
		blob, valid := gitRead(run, environ, s.root, scratch, "show", "HEAD:"+path)
		staged, stagedOK := gitRead(run, environ, s.root, scratch, "show", ":"+path)
		actualPath := filepath.Join(s.root, filepath.FromSlash(path))
		info, statErr := os.Lstat(actualPath)
		if statErr != nil || !info.Mode().IsRegular() {
			return s, "source_type_altered"
		}
		raw, readErr := os.ReadFile(actualPath)
		repr := representation(blob, raw)
		if !valid || !stagedOK || readErr != nil || !bytes.Equal(blob, staged) || repr == "" {
			return s, "source_altered"
		}
		s.raw[path] = raw
		s.identity.Sources = append(s.identity.Sources, sourceIdentity{path, digest(raw), digest(blob), repr})
	}
	return s, ""
}

func representation(blob, raw []byte) string {
	if bytes.Equal(blob, raw) {
		return "EXACT"
	}
	if !bytes.Contains(blob, []byte("\r")) && bytes.Equal(bytes.ReplaceAll(blob, []byte("\n"), []byte("\r\n")), raw) {
		return "CRLF"
	}
	return ""
}

func preserved(s snapshot, run commandRunner, environ []string, scratch string) bool {
	now, category := takeSnapshot(run, environ, scratch)
	if category != "" || now.root != s.root || now.index != s.index || now.identity.Head != s.identity.Head || now.identity.Tree != s.identity.Tree || !bytes.Equal(now.indexRaw, s.indexRaw) || !bytes.Equal(now.status, s.status) {
		return false
	}
	for path, raw := range s.raw {
		if !bytes.Equal(raw, now.raw[path]) {
			return false
		}
	}
	return true
}

func replaceExactly(source []byte, old, replacement string) ([]byte, string) {
	if old == replacement {
		return nil, "mutation_noop"
	}
	if bytes.Count(source, []byte(old)) != 1 {
		return nil, "mutation_anchor"
	}
	return bytes.Replace(source, []byte(old), []byte(replacement), 1), ""
}

func mutate(source []byte, mechanism string) ([]byte, string) {
	normalized := bytes.ReplaceAll(source, []byte("\r\n"), []byte("\n"))
	var changed []byte
	var category string
	switch mechanism {
	case "M1":
		changed, category = replaceExactly(normalized, "if privacy && checkPrivateRecordPolicy(p) != nil {", "if privacy && stage != \"created\" && checkPrivateRecordPolicy(p) != nil {")
	case "M2":
		anchor := "\tif o.hooks != nil && o.hooks.observe != nil {\n\t\to.hooks.observe(stage, f, &p)\n\t}\n"
		changed, category = replaceExactly(normalized, anchor, anchor+"\tp.reparse = false\n\tp.directory = f.directory\n")
	default:
		return nil, "mutation_unknown"
	}
	if category != "" {
		return nil, category
	}
	if bytes.Contains(source, []byte("\r\n")) {
		changed = bytes.ReplaceAll(changed, []byte("\n"), []byte("\r\n"))
	}
	return changed, ""
}

func writeOverlay(root, scratch string, source []byte, mechanism string) (string, string) {
	changed, category := mutate(source, mechanism)
	if category != "" {
		return "", category
	}
	copyPath := filepath.Join(scratch, mechanism+"-adapter.go")
	overlayPath := filepath.Join(scratch, mechanism+"-overlay.json")
	if err := os.WriteFile(copyPath, changed, 0o600); err != nil {
		return "", "overlay_io"
	}
	data, err := json.Marshal(struct{ Replace map[string]string }{map[string]string{filepath.Join(root, filepath.FromSlash(adapterPath)): copyPath}})
	if err != nil || os.WriteFile(overlayPath, data, 0o600) != nil {
		return "", "overlay_io"
	}
	return overlayPath, ""
}

type testEvent struct {
	Action      string
	Package     string
	Test        string
	Output      string
	FailedBuild string
}

type eventState struct {
	run, done bool
	terminal  string
	output    strings.Builder
}

var diagnostic = regexp.MustCompile(`^\s+([^\s:]+\.go):([0-9]+): (.*)$`)

func parseEvents(result commandResult, w *witness, p *phase, pkg string) string {
	p.Exit = result.exit
	defer observeInventory(result.stdout, p, pkg)
	if result.category != "" {
		return result.category
	}
	states := map[string]*eventState{}
	for _, item := range p.Inventory {
		states[item.Test] = &eventState{}
	}
	if len(result.stdout) == 0 || result.stdout[len(result.stdout)-1] != '\n' {
		return "truncated_events"
	}
	scanner := bufio.NewScanner(bytes.NewReader(result.stdout))
	scanner.Buffer(make([]byte, 4096), maxLogBytes)
	started, finished, packageTerminal := false, false, ""
	for scanner.Scan() {
		var event testEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Action == "" {
			return "malformed_events"
		}
		if event.FailedBuild != "" || event.Action == "build-fail" || event.Action == "build-output" {
			return "build_failure"
		}
		if event.Package != pkg || finished {
			return "unexpected_events"
		}
		if strings.Contains(event.Output, "panic:") || strings.Contains(event.Output, "fatal error:") || strings.Contains(event.Output, "test timed out") {
			return "panic_or_timeout"
		}
		if event.Test == "" {
			switch event.Action {
			case "start":
				if started {
					return "duplicate_events"
				}
				started = true
			case "pass", "fail", "skip":
				if !started {
					return "missing_events"
				}
				finished, packageTerminal = true, event.Action
			case "output":
				if !started || strings.Contains(event.Output, "NOT RUN") || strings.Contains(event.Output, "[build failed]") {
					return "unexpected_output"
				}
			default:
				return "unexpected_events"
			}
			continue
		}
		s, known := states[event.Test]
		if !known || !started || s.done {
			return "unexpected_events"
		}
		switch event.Action {
		case "run":
			if s.run {
				return "duplicate_events"
			}
			if separator := strings.LastIndex(event.Test, "/"); separator >= 0 {
				parent := states[event.Test[:separator]]
				if parent == nil || !parent.run || parent.done {
					return "missing_events"
				}
			}
			s.run = true
		case "pass", "fail", "skip":
			if !s.run {
				return "missing_events"
			}
			for name, child := range states {
				if strings.HasPrefix(name, event.Test+"/") && child.run && !child.done {
					return "conflicting_events"
				}
			}
			s.done, s.terminal = true, event.Action
		case "output":
			if !s.run {
				return "missing_events"
			}
			s.output.WriteString(event.Output)
		case "pause", "cont":
			if !s.run {
				return "missing_events"
			}
		default:
			return "unexpected_events"
		}
	}
	if scanner.Err() != nil || !finished {
		return "truncated_events"
	}
	if len(result.stderr) != 0 {
		return "non_json_stderr"
	}
	for i := range p.Inventory {
		item := &p.Inventory[i]
		s := states[item.Test]
		if !s.run || !s.done {
			return "missing_events"
		}
		if s.terminal == "skip" {
			if w != nil || !acceptHistoricalSkip(item.Test, s.output.String()) {
				return "unqualified_skip"
			}
			item.Category = "B1_CAPABILITY_NOT_RUN"
			continue
		}
		if strings.Contains(s.output.String(), "NOT RUN") {
			return "unqualified_not_run"
		}
		want := "pass"
		if w != nil {
			want = "fail"
		}
		if s.terminal != want {
			if w != nil && s.terminal == "pass" && result.exit == 0 {
				return "mutation_survived"
			}
			return "unexpected_result"
		}
	}
	if w == nil {
		if result.exit != 0 || packageTerminal != "pass" {
			return "control_failure"
		}
		return ""
	}
	if result.exit != 1 || packageTerminal != "fail" {
		return "mutation_exit"
	}
	// Only the behavioral fatal assertion's exact source location can establish
	// the intended failure. Its preceding fixture guard cannot establish a kill.
	assertions, prerequisites := 0, 0
	for test, s := range states {
		for _, line := range strings.Split(s.output.String(), "\n") {
			match := diagnostic.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			if test != w.test {
				return "wrong_assertion"
			}
			if prerequisiteDiagnostic(match[1], match[2], match[3]) {
				prerequisites++
				continue
			}
			if match[1] != "private_generation_windows_test.go" || match[2] != strconv.Itoa(w.line) || match[3] != w.assertion {
				return "wrong_assertion"
			}
			assertions++
		}
	}
	if prerequisites != 0 {
		if prerequisites != 1 || assertions != 0 {
			return "wrong_assertion"
		}
		return "prerequisite_failed"
	}
	if assertions != 1 {
		return "missing_assertion"
	}
	return ""
}

func prerequisiteDiagnostic(file, line, message string) bool {
	for _, w := range witnesses {
		if message == w.prerequisite || (file == "private_generation_windows_test.go" && line == strconv.Itoa(w.prerequisiteLine)) {
			return true
		}
	}
	return false
}

// Inventory is a separate, complete scan: even a disqualifying early event
// must not hide a later skip. Unknown names are counted, never echoed as data.
func observeInventory(data []byte, p *phase, pkg string) {
	items := map[string]*inventory{}
	outputs := map[string]string{}
	for i := range p.Inventory {
		items[p.Inventory[i].Test] = &p.Inventory[i]
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), maxLogBytes)
	for scanner.Scan() {
		var e testEvent
		if json.Unmarshal(scanner.Bytes(), &e) != nil || e.Package != pkg || e.Test == "" {
			continue
		}
		item := items[e.Test]
		if item == nil {
			p.UnknownEvents++
			if e.Action == "skip" {
				p.UnknownSkips++
			}
			continue
		}
		switch e.Action {
		case "run":
			item.Run = true
		case "pass", "fail", "skip":
			if item.Terminal != "NOT_RUN" {
				item.Terminal = "CONFLICT"
			} else {
				item.Terminal = e.Action
			}
		case "output":
			outputs[e.Test] += e.Output
		}
	}
	for test, item := range items {
		output := outputs[test]
		item.NotRun = strings.Contains(output, "NOT RUN")
		if item.NotRun {
			item.Category = "UNQUALIFIED_NOT_RUN"
		}
		if item.Terminal == "skip" {
			item.Category = "UNQUALIFIED_SKIP"
			if acceptHistoricalSkip(test, output) {
				item.Category = "B1_CAPABILITY_NOT_RUN"
				for _, reason := range historicalSkips()[test] {
					if strings.Contains(output, reason) {
						item.Reason = reason
					}
				}
			}
		}
	}
}

func acceptHistoricalSkip(test, output string) bool {
	count := 0
	for _, line := range strings.Split(output, "\n") {
		match := diagnostic.FindStringSubmatch(line)
		if match == nil && !strings.Contains(line, "NOT RUN") {
			continue
		}
		if match == nil || match[1] != "private_record_windows_test.go" {
			return false
		}
		accepted := false
		for _, reason := range historicalSkips()[test] {
			if match[3] == reason {
				accepted = true
			}
		}
		if !accepted {
			return false
		}
		count++
	}
	return count == 1
}

func testArguments(w *witness, overlay string) []string {
	selection, timeout := "^("+strings.Join(groups, "|")+")$", "10m"
	if w != nil {
		parts := strings.Split(w.test, "/")
		selection, timeout = "^"+parts[0]+"$/^"+parts[1]+"$", "2m"
	}
	args := []string{"test", "-json", "-count=1", "-timeout=" + timeout, "-run", selection}
	if overlay != "" {
		args = append(args, "-overlay", overlay)
	}
	return append(args, "./internal/localcell/trust")
}

func runPhase(p *phase, w *witness, overlay string, s snapshot, scratch string, environ []string, run commandRunner) {
	bound := 11 * time.Minute
	if w != nil {
		bound = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	r := run(ctx, "go", testArguments(w, overlay), environ, s.root, scratch)
	p.Category = parseEvents(r, w, p, packageName)
	p.Status = "INCOMPLETE"
	switch p.Category {
	case "mutation_survived", "wrong_assertion", "missing_assertion", "unexpected_result", "control_failure":
		p.Status = "FAIL"
	}
	if p.Category == "" {
		p.Status, p.Category = "PASS", "control_pass"
		if w != nil {
			p.Category = "mutation_killed"
		}
	}
}

func discardOverlay(scratch, mechanism string) bool {
	ok := true
	for _, suffix := range []string{"-adapter.go", "-overlay.json"} {
		if err := os.Remove(filepath.Join(scratch, mechanism+suffix)); err != nil && !os.IsNotExist(err) {
			ok = false
		}
	}
	return ok
}

func proof(r *receipt, scratch string, environ []string, run commandRunner) {
	defer func() { r.B1Coverage = b1Coverage(r.Phases) }()
	s, category := takeSnapshot(run, environ, scratch)
	r.Provenance = s.identity
	if category != "" {
		r.Category = category
		return
	}
	defer func() {
		r.Preserved = preserved(s, run, environ, scratch)
		if !r.Preserved {
			r.Status, r.Category = "INCOMPLETE", "preservation_failure"
		}
	}()
	runPhase(&r.Phases[0], nil, "", s, scratch, environ, run)
	if r.Phases[0].Status != "PASS" {
		r.Status = r.Phases[0].Status
		r.Category = "initial_control_incomplete"
		return
	}
	for _, batch := range []struct {
		mechanism string
		phases    []int
		witnesses []int
		restored  int
	}{{"M1", []int{1}, []int{0}, 2}, {"M2", []int{3, 4}, []int{1, 2}, 5}} {
		if !preserved(s, run, environ, scratch) {
			r.Category = "preservation_failure"
			return
		}
		overlay, failure := writeOverlay(s.root, scratch, s.raw[adapterPath], batch.mechanism)
		if failure != "" {
			r.Category = failure
			return
		}
		for i, phaseIndex := range batch.phases {
			runPhase(&r.Phases[phaseIndex], &witnesses[batch.witnesses[i]], overlay, s, scratch, environ, run)
		}
		if !discardOverlay(scratch, batch.mechanism) {
			r.Category = "cleanup_uncertain"
			return
		}
		if !preserved(s, run, environ, scratch) {
			r.Category = "preservation_failure"
			return
		}
		runPhase(&r.Phases[batch.restored], nil, "", s, scratch, environ, run)
	}
	complete, failed := true, false
	for _, p := range r.Phases {
		complete = complete && p.Status == "PASS"
		failed = failed || p.Status == "FAIL"
	}
	if !complete {
		r.Category = "phase_incomplete"
		if failed {
			r.Status, r.Category = "FAIL", "proof_failure"
		}
		return
	}
	r.Status, r.Category = "PASS", "proof_complete_owner_review_pending"
}

func b1Coverage(phases []phase) string {
	seen, complete, qualified := false, true, false
	for _, index := range []int{0, 2, 5} {
		for _, item := range phases[index].Inventory {
			if strings.HasPrefix(item.Test, "TestWindowsTrustPrivateGeneration") {
				continue
			}
			seen = seen || item.Run
			skip := item.Terminal == "skip" && item.Category == "B1_CAPABILITY_NOT_RUN"
			qualified = qualified || skip
			complete = complete && item.Run && (item.Terminal == "pass" || skip)
		}
	}
	if !seen {
		return "NOT_RUN"
	}
	if !complete {
		if qualified {
			return "PARTIAL_CAPABILITY_NOT_RUN"
		}
		return "PARTIAL"
	}
	if qualified {
		return "QUALIFIED_CAPABILITY_NOT_RUN"
	}
	return "EXECUTED"
}

func runCLI(args []string, stdout io.Writer, goos, arch string, environ []string, run commandRunner) int {
	flags := flag.NewFlagSet("trust-native-proof", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	output := flags.String("output", "", "new receipt path")
	if flags.Parse(args) != nil || *output == "" || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stdout, "proof status=INCOMPLETE category=arguments")
		return 2
	}
	f, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_, _ = fmt.Fprintln(stdout, "proof status=INCOMPLETE category=output_unavailable")
		return 2
	}
	r := newReceipt(goos, arch)
	if goos != "windows" || arch != "amd64" {
		r.Category = "unsupported_platform"
	} else {
		scratch, scratchErr := os.MkdirTemp("", "trust-native-proof-")
		if scratchErr != nil {
			r.Category = "scratch_unavailable"
		} else {
			proof(&r, scratch, childEnvironment(environ, goos, arch), run)
			r.Cleanup = os.RemoveAll(scratch) == nil
			if !r.Cleanup {
				r.Status, r.Category = "INCOMPLETE", "cleanup_uncertain"
			}
		}
	}
	encodeErr := json.NewEncoder(f).Encode(r)
	syncErr, closeErr := f.Sync(), f.Close()
	if encodeErr != nil || syncErr != nil || closeErr != nil {
		r.Status, r.Category = "INCOMPLETE", "receipt_io"
	}
	printSummary(stdout, r)
	if r.Status != "PASS" {
		return 1
	}
	return 0
}

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, runtime.GOOS, runtime.GOARCH, os.Environ(), execute))
}
