package main

// Slice 4: operations from the dashboard. The TUI never runs Pulumi
// in-process — it spawns witself-infra as a subprocess of ITSELF via
// os.Executable, matching the witself-admin pattern. Three wins:
//
//   1. The dashboard exercises the exact substrate a scripted or
//      AI-driven operator would drive — zero version skew.
//   2. The child's stdout/stderr are pipes, so Pulumi renders
//      plain non-interactive lines (no \r-spinner garbage) and every
//      progress line flows straight into a ring buffer.
//   3. A panic in the provider graph kills one op, not the dashboard.
//
// Safety rules encoded here: preview runs unconfirmed (read-only), up
// requires a confirm modal AFTER a preview succeeded, destroy
// additionally requires typing the cell name verbatim. One mutating
// op at a time (a global lock — bubbletea models are single-threaded
// but the subprocess isn't, and one op writing state at a time is the
// simplest correct thing).

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
)

// spawnCommand builds the exec.Cmd for a child provisioning op. It
// picks the right entry point based on how the DASHBOARD itself was
// started:
//
//   - Regular install (brew, curl, `go build`): re-invoke this same
//     compiled binary at its resolved path (os.Executable).
//
//   - `go run ./cmd/witself-infra` from source: os.Executable resolves
//     to /tmp/go-build.../witself-infra, which the Go tool cleans up
//     between builds and could vanish mid-op. Detect that and switch
//     to `go run <main pkg>` instead, launched from the source tree
//     the parent came from. Ops then use the current source, matching
//     what the operator is testing — and the child gets its own fresh
//     compile that outlives the parent's /tmp path.
//
// The source path is derived from runtime.Caller: this file lives at
// infra/pulumi/cmd/witself-infra/tui_ops.go, so filepath.Dir(file)
// gives us the same directory `go run` would want.
func spawnCommand(ctx context.Context, args []string) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if runningFromSource(self) {
		if src, ok := currentSourceDir(); ok {
			// `go run` needs the package path OR a directory of files.
			// We give it the directory, then append the app args after.
			goRunArgs := append([]string{"run", src}, args...)
			return exec.CommandContext(ctx, "go", goRunArgs...), nil
		}
	}
	return exec.CommandContext(ctx, self, args...), nil
}

// runningFromSource is a conservative heuristic: `go run` writes the
// binary under either os.TempDir()/go-build* or ~/Library/Caches/
// go-build* (macOS variant). We check for a "go-build" component in
// the executable path — no false positives from real installs.
func runningFromSource(exePath string) bool {
	return strings.Contains(exePath, string(filepath.Separator)+"go-build") ||
		strings.HasSuffix(filepath.Dir(exePath), "go-build")
}

// currentSourceDir returns the directory of THIS source file, which
// is where `go run` should be pointed. Empty when runtime.Caller
// couldn't identify our own path (should never happen in practice).
func currentSourceDir() (string, bool) {
	_, file, _, ok := runtime.Caller(0)
	if !ok || file == "" {
		return "", false
	}
	return filepath.Dir(file), true
}

// opKind names one runnable subprocess verb.
type opKind int

const (
	opPreview opKind = iota
	opUp
	opDestroy
)

func (k opKind) verb() string {
	switch k {
	case opPreview:
		return "preview"
	case opUp:
		return "up"
	case opDestroy:
		return "destroy"
	}
	return "?"
}

// opRun tracks one in-flight subprocess. Completion and its error
// travel exclusively via opDoneMsg — the model never polls the struct.
type opRun struct {
	kind    opKind
	cell    string
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	lines   []string // ring buffer, newest last
	mu      sync.Mutex
	program *tea.Program // pushed lines land as opLineMsg
}

const opLineCap = 2000

func (o *opRun) appendLine(line string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.lines = append(o.lines, line)
	if len(o.lines) > opLineCap {
		o.lines = o.lines[len(o.lines)-opLineCap:]
	}
}

// snapshot copies the tail for the view — never share the underlying
// slice, the writer goroutines keep mutating it.
func (o *opRun) snapshot(n int) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.lines) <= n {
		out := make([]string, len(o.lines))
		copy(out, o.lines)
		return out
	}
	out := make([]string, n)
	copy(out, o.lines[len(o.lines)-n:])
	return out
}

// startOp spawns witself-infra as a subprocess for the named cell/op.
// The child's process group is SEPARATE (Setpgid) so a ctrl+c reaching
// the dashboard doesn't cascade into the middle of `up`.
//
// When the dashboard runs from a `go run` build, os.Executable resolves
// to the ephemeral /tmp/go-build.../witself-infra path — usable, but
// fragile (a background tmp sweep during a 20-minute up would kill it).
// spawnCommand switches to `go run` in that case so child provisions
// use the same source tree the parent was launched from, matching the
// operator's mental model ("I'm running from code, so my ops run from
// code too").
func startOp(program *tea.Program, kind opKind, cell string, configPath string) (*opRun, error) {
	args := []string{kind.verb(), "-cell", cell}
	if configPath != "" {
		args = append(args, "-config", configPath)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd, err := spawnCommand(ctx, args)
	if err != nil {
		cancel()
		return nil, err
	}
	// Own process group: signals to the dashboard don't reach the child.
	// The child's cancel() (via context) still sends SIGKILL — that's
	// what the "cancel operation" modal choice does.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	op := &opRun{kind: kind, cell: cell, cmd: cmd, cancel: cancel, program: program}
	go op.pump(stdout)
	go op.pump(stderr)
	go op.wait()
	return op, nil
}

func (o *opRun) pump(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		o.appendLine(sc.Text())
		if o.program != nil {
			o.program.Send(opLineMsg{})
		}
	}
	// An over-long line (>1MB) stops the scanner; surface it in the
	// ring buffer rather than silently truncating the tail.
	if err := sc.Err(); err != nil {
		o.appendLine("[output truncated: " + err.Error() + "]")
	}
}

func (o *opRun) wait() {
	err := o.cmd.Wait()
	if o.program != nil {
		o.program.Send(opDoneMsg{cell: o.cell, err: err})
	}
}

// detach would let the dashboard exit while the op keeps running.
// REFUSED for now, honestly: the child's stdout/stderr are OUR pipes,
// and a parent exit closes their read ends — pulumi takes SIGPIPE on
// its next progress write within milliseconds, killing the very op a
// "detach" claims to preserve. A real detach needs a re-parenting
// helper (fork+setsid+dup2 onto a log file) at SPAWN time, not
// post-hoc; until that lands the interrupt modal offers keep/cancel.
func (o *opRun) detach() error {
	return fmt.Errorf("detach not implemented — pick [k] keep running or [c] cancel; a running op cannot survive the dashboard exiting")
}

// killOp SIGKILLs the running child's process group so the whole
// tree dies together — provider plugins are grandchildren.
func (o *opRun) killOp() {
	if o.cmd == nil || o.cmd.Process == nil {
		return
	}
	// Negative pid = process group (matches Setpgid on start).
	_ = syscall.Kill(-o.cmd.Process.Pid, syscall.SIGKILL)
	if o.cancel != nil {
		o.cancel()
	}
}

// opLineMsg is a re-render tick: the ring buffer already holds the
// line; the message just wakes the event loop so the view repaints.
type opLineMsg struct{}
type opDoneMsg struct {
	cell string
	err  error
}

// confirmDialog names what the operator is being asked to confirm.
type confirmDialog struct {
	kind        opKind
	cell        string
	previewSeen bool   // for up: a successful preview must precede
	typed       string // for destroy: the operator must type the cell name
	err         string // shown on partial typed match
}

// startConfirm decides what confirmation an op needs.
func startConfirm(kind opKind, cell string, previewSeen bool) *confirmDialog {
	switch kind {
	case opPreview:
		return nil // read-only
	case opUp:
		return &confirmDialog{kind: kind, cell: cell, previewSeen: previewSeen}
	case opDestroy:
		return &confirmDialog{kind: kind, cell: cell}
	}
	return nil
}

// canConfirm reports whether the confirm state permits the y-key to fire.
func (c *confirmDialog) canConfirm() bool {
	switch c.kind {
	case opUp:
		return c.previewSeen
	case opDestroy:
		return c.typed == c.cell
	}
	return true
}

// renderDialog is a compact view of the pending confirmation.
func (c *confirmDialog) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n\n", strings.ToUpper(c.kind.verb()), c.cell)
	switch c.kind {
	case opUp:
		if !c.previewSeen {
			b.WriteString("run `preview` first — up needs a preview to have succeeded so an operator has seen the diff. press esc to close.\n")
		} else {
			b.WriteString("preview passed. press y to apply, esc to cancel.\n")
		}
	case opDestroy:
		b.WriteString("destroy will DRAIN the cell, EVACUATE every account to R2, then DELETE the fleet entry and tear down every cloud resource.\n\n")
		b.WriteString("type the cell name exactly to confirm:\n")
		b.WriteString("  " + c.typed + "▏\n")
		if c.err != "" {
			b.WriteString("\n" + c.err + "\n")
		}
	}
	return b.String()
}
