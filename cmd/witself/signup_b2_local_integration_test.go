//go:build signup_b2_acceptance

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/jsonstrict"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/signuplegal"
)

// This opt-in acceptance driver consumes a separately built real CLI. Neither
// this driver nor the ordinary Go/Node suites build an alternate executable.
const (
	b2CLIStreamLimit   = 1 << 20
	b2CLISnapshotLimit = 512 << 10
)

type b2CLIInputs struct {
	binary, binaryHash, node, nodeHash, evidence, sourceHash, buildHash string
	commit, tree, adapter, adapterHash                                  string
}

type b2CLIBuildStream struct {
	Path string `json:"path"`
	Hash string `json:"sha256"`
	Size int64  `json:"size"`
}

type b2CLIBuildReceipt struct {
	Status             string `json:"status"`
	SourceInventorySHA string `json:"source_inventory_sha256"`
	Before             struct {
		Commit string `json:"source_commit"`
		Tree   string `json:"source_tree"`
		Clean  bool   `json:"source_clean_exact_branch"`
	} `json:"before"`
	After struct {
		Commit string `json:"source_commit"`
		Tree   string `json:"source_tree"`
		Clean  bool   `json:"source_clean_exact_branch"`
	} `json:"after"`
	Binary struct {
		Path string `json:"path"`
		Hash string `json:"sha256"`
	} `json:"binary"`
	Commands []struct {
		Exit     *int             `json:"actual_exit"`
		TimedOut *bool            `json:"timed_out"`
		Gone     bool             `json:"process_group_absent"`
		Stdout   b2CLIBuildStream `json:"stdout"`
		Stderr   b2CLIBuildStream `json:"stderr"`
	} `json:"commands"`
}

type b2CLIPair struct {
	Terms   string `json:"consent_terms_version"`
	Privacy string `json:"consent_privacy_version"`
}

type b2CLISnapshot struct {
	Counts map[string]int `json:"counts"`
	States map[string]*struct {
		Phase       string               `json:"phase"`
		Fingerprint string               `json:"request_fingerprint"`
		Refusal     *signuplegal.Refusal `json:"legal_refusal"`
		Successor   *struct {
			Registered bool                  `json:"registered"`
			Transition string                `json:"transition_id"`
			Candidate  signuplegal.Candidate `json:"candidate"`
		} `json:"legal_successor"`
		Reservation *struct {
			Refusal    signuplegal.Refusal   `json:"refusal"`
			Transition string                `json:"transition_id"`
			Candidate  signuplegal.Candidate `json:"candidate"`
		} `json:"legal_reservation"`
	} `json:"states"`
	RegistrationHashes  []string         `json:"registrationHashes"`
	CounterCounts       map[string][]int `json:"counterCounts"`
	InviteUses          int              `json:"inviteUses"`
	LogicalAccounts     int              `json:"logicalAccounts"`
	VerificationEntries int              `json:"verificationEntries"`
	ReceiptPairs        []struct {
		Role string `json:"role"`
		b2CLIPair
	} `json:"receiptPairs"`
	CLI struct {
		Original   string    `json:"originalID"`
		Candidate  string    `json:"candidateID"`
		Transition string    `json:"transitionID"`
		Manifest   int       `json:"manifest"`
		Bootstrap  int       `json:"bootstrap"`
		Current    b2CLIPair `json:"currentPair"`
	} `json:"cli"`
}

func b2CLIRequire(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" || strings.TrimSpace(value) != value {
		t.Fatalf("required explicit B2 artifact input is absent or invalid: %s", name)
	}
	return value
}

func b2CLIHashFile(t *testing.T, path string, maximum int64) string {
	t.Helper()
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		t.Fatal("B2 artifact path must be absolute and canonical")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maximum {
		t.Fatal("B2 artifact must be a bounded regular file, not a symlink")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal("open B2 artifact failed")
	}
	opened, statErr := file.Stat()
	h := sha256.New()
	_, copyErr := io.Copy(h, io.LimitReader(file, maximum+1))
	closeErr := file.Close()
	after, afterErr := os.Lstat(path)
	if statErr != nil || copyErr != nil || closeErr != nil || afterErr != nil ||
		!os.SameFile(info, opened) || !os.SameFile(info, after) || after.Size() != info.Size() {
		t.Fatal("B2 artifact identity changed during inspection")
	}
	return hex.EncodeToString(h.Sum(nil))
}

func b2CLIReadJSON(t *testing.T, raw []byte, target any) {
	t.Helper()
	if len(raw) > b2CLIStreamLimit {
		t.Fatal("B2 private JSON exceeds its bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if jsonstrict.ConsumeUniqueValue(decoder) != nil || jsonstrict.RequireEOF(decoder) != nil ||
		json.Unmarshal(raw, target) != nil {
		t.Fatal("B2 private JSON is malformed or duplicated")
	}
}

func b2CLILoadInputs(t *testing.T) b2CLIInputs {
	t.Helper()
	input := b2CLIInputs{
		binary:      b2CLIRequire(t, "WITSELF_B2_CLI_BINARY"),
		binaryHash:  b2CLIRequire(t, "WITSELF_B2_CLI_SHA256"),
		node:        b2CLIRequire(t, "WITSELF_B2_NODE_BINARY"),
		nodeHash:    b2CLIRequire(t, "WITSELF_B2_NODE_SHA256"),
		evidence:    b2CLIRequire(t, "WITSELF_B2_EVIDENCE_DIR"),
		sourceHash:  b2CLIRequire(t, "WITSELF_B2_SOURCE_INVENTORY_SHA256"),
		buildHash:   b2CLIRequire(t, "WITSELF_B2_BUILD_RECEIPT_SHA256"),
		adapterHash: b2CLIRequire(t, "WITSELF_B2_ADAPTER_SHA256"),
	}
	source := b2CLIRequire(t, "WITSELF_B2_SOURCE_INVENTORY")
	build := b2CLIRequire(t, "WITSELF_B2_BUILD_RECEIPT")
	for _, digest := range []string{input.binaryHash, input.nodeHash, input.sourceHash, input.buildHash, input.adapterHash} {
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(digest) {
			t.Fatal("B2 artifact input requires an exact SHA256")
		}
	}
	if b2CLIHashFile(t, input.binary, 128<<20) != input.binaryHash ||
		b2CLIHashFile(t, input.node, 256<<20) != input.nodeHash ||
		b2CLIHashFile(t, source, 16<<20) != input.sourceHash ||
		b2CLIHashFile(t, build, b2CLIStreamLimit) != input.buildHash {
		t.Fatal("B2 artifact hash does not match its explicit pin")
	}
	raw, err := os.ReadFile(build)
	if err != nil {
		t.Fatal("read B2 build receipt failed")
	}
	var receipt b2CLIBuildReceipt
	b2CLIReadJSON(t, raw, &receipt)
	commitPattern := regexp.MustCompile(`^[0-9a-f]{40}$`)
	if receipt.Status != "PASS" || receipt.SourceInventorySHA != input.sourceHash ||
		receipt.Binary.Path != input.binary || receipt.Binary.Hash != input.binaryHash ||
		!receipt.Before.Clean || !receipt.After.Clean || receipt.Before.Commit != receipt.After.Commit ||
		receipt.Before.Tree != receipt.After.Tree || !commitPattern.MatchString(receipt.Before.Commit) ||
		!commitPattern.MatchString(receipt.Before.Tree) || len(receipt.Commands) == 0 || len(receipt.Commands) > 8 {
		t.Fatal("B2 binary lacks its reviewed clean-source build receipt")
	}
	for _, command := range receipt.Commands {
		if command.Exit == nil || *command.Exit != 0 || command.TimedOut == nil || *command.TimedOut || !command.Gone {
			t.Fatal("B2 build receipt contains an incomplete or failed child")
		}
		for _, stream := range []b2CLIBuildStream{command.Stdout, command.Stderr} {
			if stream.Size < 0 || stream.Size > 16<<20 ||
				b2CLIHashFile(t, stream.Path, 16<<20) != stream.Hash {
				t.Fatal("B2 build stream differs from its retained receipt")
			}
			info, statErr := os.Lstat(stream.Path)
			if statErr != nil || info.Size() != stream.Size {
				t.Fatal("B2 build stream size differs from its retained receipt")
			}
		}
	}
	input.commit, input.tree = receipt.Before.Commit, receipt.Before.Tree
	info, err := buildinfo.ReadFile(input.binary)
	if err != nil || info.Path != "github.com/witwave-ai/witself/cmd/witself" || info.GoVersion != "go1.26.6" {
		t.Fatal("B2 artifact is not the pinned Go Witself CLI")
	}
	settings := map[string]string{}
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["GOOS"] != runtime.GOOS || settings["GOARCH"] != runtime.GOARCH ||
		settings["vcs.modified"] == "true" ||
		(settings["vcs.revision"] != "" && settings["vcs.revision"] != input.commit) {
		t.Fatal("B2 binary build metadata contradicts its source or host")
	}
	if !filepath.IsAbs(input.evidence) || filepath.Clean(input.evidence) != input.evidence {
		t.Fatal("B2 evidence root must be explicit and absolute")
	}
	evidenceInfo, err := os.Lstat(input.evidence)
	if err != nil || !evidenceInfo.IsDir() || evidenceInfo.Mode()&os.ModeSymlink != 0 ||
		(runtime.GOOS != "windows" && evidenceInfo.Mode().Perm()&0o077 != 0) {
		t.Fatal("B2 evidence root must be pre-created privately by the execution owner")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve B2 source location failed")
	}
	input.adapter = filepath.Join(filepath.Dir(thisFile), "..", "..", "infra", "cloudflare",
		"control-plane", "test", "fixtures", "signup-b2-cli-loopback.mjs")
	input.adapter, err = filepath.Abs(input.adapter)
	if err != nil {
		t.Fatal("resolve B2 adapter failed")
	}
	if b2CLIHashFile(t, input.adapter, 128<<10) != input.adapterHash {
		t.Fatal("B2 adapter source differs from its explicit pin")
	}
	return input
}

type b2CLIBuffer struct {
	raw      bytes.Buffer
	overflow bool
}

func (b *b2CLIBuffer) Write(value []byte) (int, error) {
	remaining := b2CLIStreamLimit - b.raw.Len()
	if len(value) > remaining {
		b.overflow = true
		_, _ = b.raw.Write(value[:remaining])
	} else {
		_, _ = b.raw.Write(value)
	}
	return len(value), nil
}

func b2CLIKeepPrivate(t *testing.T, input b2CLIInputs, suffix string, raw []byte) {
	t.Helper()
	path := filepath.Join(input.evidence, t.Name()+"."+suffix)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("B2 evidence path already exists or cannot be inspected")
	}
	if err := writeAccountCreatePrivateTestFile(path, raw); err != nil {
		t.Fatal("write private B2 evidence failed")
	}
	assertAccountCreatePrivateTestFile(t, path)
}

func b2CLIEnvironment(t *testing.T, input b2CLIInputs) (string, []string) {
	t.Helper()
	parent := newAccountCreateTestHome(t)
	home := filepath.Join(parent, "witself")
	profile := filepath.Join(parent, "profile")
	temp := filepath.Join(parent, "tmp")
	for _, path := range []string{profile, temp, filepath.Join(profile, "config"), filepath.Join(profile, "local")} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal("create isolated B2 profile directory failed")
		}
	}
	t.Setenv("WITSELF_HOME", home) // Only the test's real local reader uses this.
	env := []string{
		"HOME=" + profile, "USERPROFILE=" + profile, "WITSELF_HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(profile, "config"), "APPDATA=" + filepath.Join(profile, "config"),
		"LOCALAPPDATA=" + filepath.Join(profile, "local"), "TMPDIR=" + temp, "TMP=" + temp, "TEMP=" + temp,
		"LANG=C", "LC_ALL=C", "TZ=UTC", "NO_COLOR=1", "CI=true", "GOMAXPROCS=2",
	}
	path := []string{filepath.Dir(input.binary), filepath.Dir(input.node)}
	if runtime.GOOS == "windows" {
		for _, key := range []string{"SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT"} {
			value := os.Getenv(key)
			if value != "" {
				env = append(env, key+"="+value)
			}
		}
		systemRoot := os.Getenv("SYSTEMROOT")
		if systemRoot == "" {
			t.Fatal("Windows B2 child requires SYSTEMROOT")
		}
		path = append(path, filepath.Join(systemRoot, "System32"))
	} else {
		path = append(path, "/usr/bin", "/bin")
	}
	env = append(env, "PATH="+strings.Join(path, string(os.PathListSeparator)))
	return home, env
}

func b2CLIRun(t *testing.T, input b2CLIInputs, env []string, home, label string, args []string, want int) int {
	t.Helper()
	if b2CLIHashFile(t, input.binary, 128<<20) != input.binaryHash {
		t.Fatal("B2 CLI changed before execution")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, input.binary, args...)
	command.Env, command.Dir, command.WaitDelay = env, filepath.Dir(home), 2*time.Second
	var stdout, stderr b2CLIBuffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Start(); err != nil {
		t.Fatal("start actual B2 CLI failed")
	}
	pid := command.Process.Pid
	err := command.Wait()
	b2CLIKeepPrivate(t, input, label+".stdout", stdout.raw.Bytes())
	b2CLIKeepPrivate(t, input, label+".stderr", stderr.raw.Bytes())
	actual := command.ProcessState.ExitCode()
	waitError := ""
	if err != nil {
		waitError = err.Error()
	}
	receipt, receiptErr := json.Marshal(map[string]any{
		"pid": pid, "exit": actual, "wait_complete": true, "wait_error": waitError,
	})
	if receiptErr != nil {
		t.Fatal("encode actual B2 CLI exit receipt failed")
	}
	b2CLIKeepPrivate(t, input, label+".exit.json", append(receipt, '\n'))
	var exitError *exec.ExitError
	expectedWait := (want == 0 && err == nil) ||
		(want != 0 && errors.As(err, &exitError) && exitError.ExitCode() == want)
	if ctx.Err() != nil || stdout.overflow || stderr.overflow || actual != want ||
		!expectedWait || b2CLIHashFile(t, input.binary, 128<<20) != input.binaryHash {
		t.Fatalf("actual B2 CLI %s did not complete with expected exit %d; private streams retained", label, want)
	}
	return pid
}

type b2CLILines struct {
	mu      sync.Mutex
	raw     b2CLIBuffer
	pending []byte
	lines   chan []byte
}

func (b *b2CLILines) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, _ = b.raw.Write(value)
	if b.raw.overflow {
		return 0, errors.New("B2 protocol output limit")
	}
	b.pending = append(b.pending, value...)
	for {
		index := bytes.IndexByte(b.pending, '\n')
		if index < 0 {
			if len(b.pending) > b2CLISnapshotLimit {
				return 0, errors.New("B2 protocol line limit")
			}
			break
		}
		if index > b2CLISnapshotLimit {
			return 0, errors.New("B2 protocol line limit")
		}
		line := append([]byte(nil), b.pending[:index]...)
		b.pending = b.pending[index+1:]
		select {
		case b.lines <- line:
		default:
			return 0, errors.New("B2 protocol queue limit")
		}
	}
	return len(value), nil
}

type b2CLINode struct {
	input   b2CLIInputs
	command *exec.Cmd
	cancel  context.CancelFunc
	stdin   io.WriteCloser
	stdout  b2CLILines
	stderr  b2CLIBuffer
	done    chan struct{}
	waitErr error
	origin  string
	nextID  int
	closed  bool
}

func (n *b2CLINode) reply(t *testing.T, kind string, id int) json.RawMessage {
	t.Helper()
	var line []byte
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	select {
	case line = <-n.stdout.lines:
	case <-n.done:
		select {
		case line = <-n.stdout.lines:
		default:
			t.Fatal("B2 Node exited before its expected private reply")
		}
	case <-timer.C:
		t.Fatal("B2 private reply timed out")
	}
	var reply struct {
		Kind   string          `json:"kind"`
		ID     int             `json:"id"`
		Origin string          `json:"origin"`
		PID    int             `json:"pid"`
		Value  json.RawMessage `json:"value"`
	}
	b2CLIReadJSON(t, line, &reply)
	var keys map[string]json.RawMessage
	b2CLIReadJSON(t, line, &keys)
	wantKeys := map[string][]string{
		"ready": {"kind", "origin", "pid"}, "snapshot": {"kind", "id", "value"}, "closed": {"kind", "id"},
	}[kind]
	if len(keys) != len(wantKeys) {
		t.Fatal("B2 private protocol reply has unexpected fields")
	}
	for _, key := range wantKeys {
		if _, ok := keys[key]; !ok {
			t.Fatal("B2 private protocol reply is missing a required field")
		}
	}
	if reply.Kind != kind || reply.ID != id {
		t.Fatal("unexpected B2 private protocol reply")
	}
	if kind == "ready" {
		parsed, err := url.Parse(reply.Origin)
		port := 0
		if err == nil {
			port, err = strconv.Atoi(parsed.Port())
		}
		if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || port < 1 ||
			port > 65535 || parsed.Host != "127.0.0.1:"+strconv.Itoa(port) || parsed.Path != "" ||
			parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Opaque != "" || parsed.User != nil ||
			parsed.Fragment != "" || parsed.String() != reply.Origin || reply.PID != n.command.Process.Pid {
			t.Fatal("B2 Node did not bind its exact numeric loopback authority")
		}
		n.origin = reply.Origin
	}
	return reply.Value
}

func b2CLIStartNode(t *testing.T, input b2CLIInputs, env []string, home, scenario string) *b2CLINode {
	t.Helper()
	if b2CLIHashFile(t, input.node, 256<<20) != input.nodeHash ||
		b2CLIHashFile(t, input.adapter, 128<<10) != input.adapterHash {
		t.Fatal("B2 Node or adapter changed before execution")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	n := &b2CLINode{input: input, cancel: cancel, done: make(chan struct{})}
	n.stdout.lines = make(chan []byte, 16)
	n.command = exec.CommandContext(ctx, input.node, input.adapter, scenario)
	n.command.Env, n.command.Dir, n.command.WaitDelay = env, filepath.Dir(home), 2*time.Second
	n.command.Stdout, n.command.Stderr = &n.stdout, &n.stderr
	var err error
	n.stdin, err = n.command.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal("create B2 private input pipe failed")
	}
	if err := n.command.Start(); err != nil {
		_ = n.stdin.Close()
		cancel()
		t.Fatal("start pinned B2 Node failed")
	}
	go func() {
		n.waitErr = n.command.Wait()
		close(n.done)
	}()
	t.Cleanup(func() {
		if !n.closed {
			n.cancel()
			_ = n.stdin.Close()
			select {
			case <-n.done:
			case <-time.After(5 * time.Second):
				t.Error("B2 Node cleanup did not observe process closure")
				return
			}
			n.keep(t)
		}
	})
	n.reply(t, "ready", 0)
	return n
}

func (n *b2CLINode) keep(t *testing.T) {
	t.Helper()
	n.stdout.mu.Lock()
	raw := append([]byte(nil), n.stdout.raw.raw.Bytes()...)
	pending := len(n.stdout.pending)
	queued := len(n.stdout.lines)
	overflow := n.stdout.raw.overflow || n.stderr.overflow
	n.stdout.mu.Unlock()
	b2CLIKeepPrivate(t, n.input, "node.stdout", raw)
	b2CLIKeepPrivate(t, n.input, "node.stderr", n.stderr.raw.Bytes())
	waitError := ""
	if n.waitErr != nil {
		waitError = n.waitErr.Error()
	}
	receipt, receiptErr := json.Marshal(map[string]any{
		"pid": n.command.Process.Pid, "exit": n.command.ProcessState.ExitCode(), "wait_complete": true,
		"wait_error": waitError, "output_complete": pending == 0 && queued == 0 && !overflow,
		"unread_protocol_replies": queued,
	})
	if receiptErr != nil {
		t.Fatal("encode B2 Node exit receipt failed")
	}
	b2CLIKeepPrivate(t, n.input, "node.exit.json", append(receipt, '\n'))
	n.closed = true
	if pending != 0 || queued != 0 || overflow {
		t.Error("B2 Node protocol capture was incomplete")
	}
}

func (n *b2CLINode) snapshot(t *testing.T) b2CLISnapshot {
	t.Helper()
	n.nextID++
	if _, err := fmt.Fprintf(n.stdin, "{\"op\":\"snapshot\",\"id\":%d}\n", n.nextID); err != nil {
		t.Fatal("write B2 snapshot request failed")
	}
	var snapshot b2CLISnapshot
	b2CLIReadJSON(t, n.reply(t, "snapshot", n.nextID), &snapshot)
	return snapshot
}

func (n *b2CLINode) close(t *testing.T) {
	t.Helper()
	n.nextID++
	if _, err := fmt.Fprintf(n.stdin, "{\"op\":\"close\",\"id\":%d}\n", n.nextID); err != nil {
		t.Fatal("write B2 close request failed")
	}
	n.reply(t, "closed", n.nextID)
	// Wait closes StdinPipe too, and may finish immediately after the reply.
	if err := n.stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Fatal("close B2 private input failed")
	}
	select {
	case <-n.done:
	case <-time.After(5 * time.Second):
		t.Fatal("B2 Node did not exit after closing its listener")
	}
	n.keep(t)
	n.cancel()
	if n.waitErr != nil || n.command.ProcessState.ExitCode() != 0 ||
		b2CLIHashFile(t, n.input.node, 256<<20) != n.input.nodeHash ||
		b2CLIHashFile(t, n.input.adapter, 128<<10) != n.input.adapterHash {
		t.Fatal("B2 Node exited unsuccessfully or its inputs changed during execution")
	}
}

type b2CLIWaitBeforeCloseStdin struct {
	io.WriteCloser
	done     <-chan struct{}
	closeErr error
}

func (s *b2CLIWaitBeforeCloseStdin) Close() error {
	// Force command.Wait to close the real StdinPipe before the driver does.
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		return errors.New("B2 shutdown regression child did not exit")
	}
	s.closeErr = s.WriteCloser.Close()
	return s.closeErr
}

func TestSignupB2NodeCloseAfterWait(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	adapter := filepath.Join(t.TempDir(), "adapter.mjs")
	if err := os.WriteFile(adapter, []byte("// B2 shutdown regression fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n := &b2CLINode{
		input: b2CLIInputs{
			node: executable, nodeHash: b2CLIHashFile(t, executable, 256<<20),
			adapter: adapter, adapterHash: b2CLIHashFile(t, adapter, 128<<10),
			evidence: t.TempDir(),
		},
		command: exec.CommandContext(ctx, executable, "-test.run=^TestSignupB2NodeCloseHelperProcess$"),
		cancel:  cancel, done: make(chan struct{}), nextID: 1,
	}
	n.stdout.lines = make(chan []byte, 16)
	n.command.Env = append(os.Environ(), "WITSELF_B2_CLOSE_HELPER=1")
	n.command.Stdout, n.command.Stderr = &n.stdout, &n.stderr
	n.command.WaitDelay = 2 * time.Second
	pipe, err := n.command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pipe.Close() }()
	stdin := &b2CLIWaitBeforeCloseStdin{WriteCloser: pipe, done: n.done}
	n.stdin = stdin
	if err := n.command.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		n.waitErr = n.command.Wait()
		close(n.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-n.done:
		case <-time.After(5 * time.Second):
			t.Error("B2 shutdown regression child did not finish cleanup")
		}
	})
	n.close(t)
	if !errors.Is(stdin.closeErr, os.ErrClosed) || !n.closed {
		t.Fatal("B2 shutdown did not complete after Wait closed stdin")
	}
}

func TestSignupB2NodeCloseHelperProcess(t *testing.T) {
	if os.Getenv("WITSELF_B2_CLOSE_HELPER") != "1" {
		return
	}
	var command struct {
		Op string `json:"op"`
		ID int    `json:"id"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&command); err != nil || command.Op != "close" || command.ID != 2 {
		os.Exit(2)
	}
	if _, err := fmt.Fprintln(os.Stdout, `{"kind":"closed","id":2}`); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func b2CLIArguments(origin string, accept bool) []string {
	args := []string{"account", "create", "--email", "owner@example.test", "--invite", "b2-invite",
		"--display-name", "B2 Fixture", "--name", "b2", "--endpoint", origin, "--challenge", "b2-fixture-challenge"}
	if accept {
		args = append(args, "--accept-terms")
	}
	return args
}

func b2CLIJournal(t *testing.T, home string) local.AccountProvisionJournal {
	t.Helper()
	record, err := local.ReadAccountProvisionJournal("b2")
	if err != nil {
		t.Fatal("actual CLI did not leave a valid readable journal")
	}
	path, err := local.AccountProvisionJournalPath("b2")
	if err != nil || !strings.HasPrefix(path, home+string(os.PathSeparator)) {
		t.Fatal("B2 journal escaped its owned home")
	}
	assertAccountCreatePrivateTestFile(t, path)
	return record
}

func b2CLICounts(t *testing.T, snapshot b2CLISnapshot, signup, reconsent, canonical, reserve, dropped, complete int) {
	t.Helper()
	want := map[string]int{
		"publicSignup": signup, "publicReconsent": reconsent, "publicLimiter": signup + reconsent,
		"signupLimiter": signup + reconsent, "canonical": canonical, "turnstile": 1, "counters": 2,
		"invite": complete, "placement": complete, "protocol": complete, "provision": complete, "reserve": reserve,
		"email": complete, "event": complete, "targetBegin": complete, "targetAttach": complete,
		"targetPromote": complete, "droppedOuterAck": dropped,
	}
	if !reflect.DeepEqual(snapshot.Counts, want) ||
		!reflect.DeepEqual(snapshot.CounterCounts, map[string][]int{"ip": {1}, "global": {1}}) ||
		snapshot.InviteUses != complete || snapshot.LogicalAccounts != complete ||
		snapshot.VerificationEntries != complete {
		t.Fatal("B2 operation relationships or inherited abuse counts differ")
	}
}

func b2CLISaved(t *testing.T, home string, snapshot b2CLISnapshot, role string) {
	t.Helper()
	name, account, tokenValue, err := local.Resolve("b2")
	if err != nil || name != "b2" || account.ID != accountCreateTestAccountID ||
		account.Email != "owner@example.test" || tokenValue != accountCreateTestOperatorToken {
		t.Fatal("actual CLI durable local account/token does not match the fixture")
	}
	for _, path := range []string{filepath.Join(home, "config.json"),
		filepath.Join(home, "tokens", "accounts", "b2", "owner.token")} {
		assertAccountCreatePrivateTestFile(t, path)
	}
	if _, err := local.ReadAccountProvisionJournal("b2"); !errors.Is(err, local.ErrAccountProvisionJournalUnavailable) {
		t.Fatal("actual CLI did not remove its completed journal")
	}
	if snapshot.CLI.Bootstrap != 1 || len(snapshot.ReceiptPairs) != 1 ||
		snapshot.ReceiptPairs[0].Role != role || snapshot.ReceiptPairs[0].b2CLIPair != snapshot.CLI.Current ||
		snapshot.States[role] == nil || snapshot.States[role].Phase != "completed" {
		t.Fatal("B2 completed receipt, legal pair or one-use bootstrap differs")
	}
}

func b2CLIProvenance(t *testing.T, input b2CLIInputs, snapshot b2CLISnapshot) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"status": "PASS", "cli_sha256": input.binaryHash, "node_sha256": input.nodeHash,
		"adapter_sha256":       input.adapterHash,
		"build_receipt_sha256": input.buildHash, "source_inventory_sha256": input.sourceHash,
		"source_commit": input.commit, "source_tree": input.tree,
		"counts": snapshot.Counts, "manifest_reads": snapshot.CLI.Manifest, "bootstrap_count": snapshot.CLI.Bootstrap,
		"actual_cli_processes": true, "node_maps_survive_children": true, "provider_acceptance": false,
		"os_network_sandbox": false,
	})
	if err != nil {
		t.Fatal("encode B2 relationship evidence failed")
	}
	b2CLIKeepPrivate(t, input, "relationships.json", append(raw, '\n'))
	t.Log("actual pinned CLI completed; private source/exit/relationship evidence retained")
}

func TestSignupB2ActualCLICurrent(t *testing.T) {
	input := b2CLILoadInputs(t)
	home, env := b2CLIEnvironment(t, input)
	b2CLIRun(t, input, env, home, "version", []string{"version"}, 0)
	node := b2CLIStartNode(t, input, env, home, "current")
	b2CLIRun(t, input, env, home, "cli-1", b2CLIArguments(node.origin, true), 0)
	snapshot := node.snapshot(t)
	b2CLICounts(t, snapshot, 1, 0, 1, 0, 0, 1)
	if snapshot.CLI.Original == "" || snapshot.CLI.Candidate != "" || snapshot.CLI.Transition != "" ||
		snapshot.CLI.Manifest != 1 || len(snapshot.RegistrationHashes) != 0 {
		t.Fatal("current CLI flow selected an unexpected transition")
	}
	b2CLISaved(t, home, snapshot, "original")
	node.close(t)
	b2CLIProvenance(t, input, snapshot)
}

func TestSignupB2ActualCLILostReconsentAck(t *testing.T) {
	input := b2CLILoadInputs(t)
	home, env := b2CLIEnvironment(t, input)
	b2CLIRun(t, input, env, home, "version", []string{"version"}, 0)
	node := b2CLIStartNode(t, input, env, home, "lost-reconsent-ack")
	pids := map[int]bool{}
	run := func(label string, accept bool, want int) {
		pid := b2CLIRun(t, input, env, home, label, b2CLIArguments(node.origin, accept), want)
		if pids[pid] {
			t.Fatal("CLI restart did not use a distinct process")
		}
		pids[pid] = true
	}
	run("cli-1-refused", true, 1)
	refused := b2CLIJournal(t, home)
	first := node.snapshot(t)
	b2CLICounts(t, first, 1, 0, 1, 0, 0, 0)
	if refused.SchemaVersion != "witself.account-provision-journal.v2" || refused.LegalRefusal == nil ||
		refused.Reconsent != nil || first.CLI.Original != refused.ProvisionID || first.CLI.Candidate != "" ||
		first.CLI.Transition != "" || first.CLI.Manifest != 1 || first.CLI.Bootstrap != 0 ||
		first.States["original"] == nil || first.States["original"].Phase != "legal_rejected" ||
		!reflect.DeepEqual(first.States["original"].Refusal, refused.LegalRefusal) {
		t.Fatal("actual CLI refusal is not bound to the exact private predecessor")
	}
	run("cli-2-no-acceptance", false, 1)
	unchanged := b2CLIJournal(t, home)
	if !reflect.DeepEqual(unchanged, refused) || !reflect.DeepEqual(node.snapshot(t), first) {
		t.Fatal("CLI selected a successor or performed HTTP without acceptance")
	}
	run("cli-3-lost-ack", true, 1)
	pending := b2CLIJournal(t, home)
	lost := node.snapshot(t)
	b2CLICounts(t, lost, 1, 1, 1, 1, 1, 0)
	if pending.Reconsent == nil || pending.LegalRefusal == nil || *pending.LegalRefusal != *refused.LegalRefusal ||
		pending.ProvisionID != refused.ProvisionID || pending.RequestFingerprint != refused.RequestFingerprint ||
		lost.CLI.Original != pending.ProvisionID || lost.CLI.Candidate != pending.Reconsent.Candidate.ProvisionID ||
		lost.CLI.Candidate == lost.CLI.Original || lost.CLI.Transition != pending.Reconsent.TransitionID ||
		lost.CLI.Manifest != 2 || lost.CLI.Bootstrap != 0 || len(lost.RegistrationHashes) != 1 ||
		lost.States["original"] == nil || lost.States["original"].Successor == nil ||
		!lost.States["original"].Successor.Registered || lost.States["candidate"] == nil ||
		lost.States["candidate"].Phase != "legal_reserved" {
		t.Fatal("lost outer acknowledgement did not leave the exact durable CLI pending choice")
	}
	successor := lost.States["original"].Successor
	if successor.Candidate != pending.Reconsent.Candidate || successor.Transition != pending.Reconsent.TransitionID {
		t.Fatal("CP selected successor differs from the CLI pending journal")
	}
	localHash, err := client.AccountCreateRequestFingerprint(node.origin, "b2", "owner@example.test",
		"b2-invite", "B2 Fixture", pending.Reconsent.Candidate.ConsentTermsVersion, pending.Reconsent.Candidate.ConsentPrivacyVersion)
	if err != nil || localHash != pending.Reconsent.RequestFingerprint {
		t.Fatal("CLI pending choice has an unexpected local request fingerprint")
	}
	run("cli-4-restart", false, 0)
	final := node.snapshot(t)
	b2CLICounts(t, final, 2, 2, 2, 1, 1, 1)
	if len(pids) != 4 || final.CLI.Original != lost.CLI.Original || final.CLI.Candidate != lost.CLI.Candidate ||
		final.CLI.Transition != lost.CLI.Transition || final.CLI.Manifest != 2 ||
		len(final.RegistrationHashes) != 2 || final.RegistrationHashes[0] != lost.RegistrationHashes[0] ||
		final.RegistrationHashes[1] != lost.RegistrationHashes[0] {
		t.Fatal("actual CLI restart did not replay the one byte-exact registration")
	}
	b2CLISaved(t, home, final, "candidate")
	node.close(t)
	b2CLIProvenance(t, input, final)
}
