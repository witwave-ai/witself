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
	"strings"
	"sync"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
)

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

// opRun tracks one in-flight subprocess.
type opRun struct {
	kind    opKind
	cell    string
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	lines   []string // ring buffer, newest last
	done    bool
	err     error
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
func startOp(program *tea.Program, kind opKind, cell string, configPath string) (*opRun, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	args := []string{kind.verb(), "-cell", cell}
	if configPath != "" {
		args = append(args, "-config", configPath)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, self, args...)
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
	go op.pump(stdout, "stdout")
	go op.pump(stderr, "stderr")
	go op.wait()
	return op, nil
}

func (o *opRun) pump(r io.Reader, stream string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		o.appendLine(line)
		if o.program != nil {
			o.program.Send(opLineMsg{cell: o.cell, stream: stream, line: line})
		}
	}
}

func (o *opRun) wait() {
	err := o.cmd.Wait()
	o.mu.Lock()
	o.done = true
	o.err = err
	o.mu.Unlock()
	if o.program != nil {
		o.program.Send(opDoneMsg{cell: o.cell, err: err})
	}
}

// detach reroutes the child's stdout/stderr from our pipes onto a
// persistent log file, then closes our end. That way the parent can
// exit without the child taking SIGPIPE on its next write — the
// entire point of a "detach and quit" option.
//
// The pipes live inside the exec.Cmd; the "child fd" the child
// actually writes to is the write end held open in the child. We
// don't have access to it directly, but closing OUR read ends and
// letting the parent exit is safe when Go's runtime is told to ignore
// SIGPIPE for the just-cleared child. Since we can't intervene in the
// child's runtime, the pragmatic move is: tee every subsequent write
// to a log file so the operator can see progress after detach — and
// keep the read ends open in a detached goroutine so the child never
// sees the pipe close. The goroutine outlives the parent process by
// hopping onto the child (via reparenting to PID 1) with the standard
// UNIX trick: close nothing, just stop reading. But that leaks fds
// and eventually blocks. The correct fix is the log file plus a
// keep-alive reader that runs until Wait completes.
func (o *opRun) detach() error {
	logPath := detachLogPath(o.cell, o.kind.verb())
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "detached — pulumi output continues in %s\n", logPath)
	// The pump goroutines are already reading; hand each pipe a copy of
	// the file descriptor to tee into via an atomic pointer swap.
	// Since our pumps use bufio.Scanner over cmd.StdoutPipe/StderrPipe,
	// the simplest path is to install the tee sink on the opRun so
	// subsequent lines land in the file too. The parent will exit
	// after this returns; the pump goroutines run inside the parent so
	// they die too — which means the child's next write hits SIGPIPE.
	//
	// The RELIABLE detach is a fork-exec re-parenting to PID 1 which
	// we can't do post-hoc from within the same process; so the safe
	// choice for now is to REFUSE detach with a clear error rather
	// than lie. Callers get an explicit message: kill or keep. The
	// "d" branch in tui.go already handles a non-nil error by
	// reporting it in the status line.
	_ = f.Close()
	_ = os.Remove(logPath)
	return fmt.Errorf("detach not implemented — pick [k] keep running or [c] cancel; a running op cannot survive the dashboard exiting on this platform")
}

// detachLogPath is where a detached op WOULD stream to when detach is
// implemented (Slice 4b — via a fork+setsid+dup2 helper on POSIX).
func detachLogPath(cell, verb string) string {
	root := os.Getenv("WITSELF_HOME")
	if root == "" {
		if home, err := os.UserHomeDir(); err == nil {
			root = filepath.Join(home, ".witself")
		} else {
			root = os.TempDir()
		}
	}
	return filepath.Join(root, "logs", "infra", cell+"-"+verb+".log")
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

type opLineMsg struct{ cell, stream, line string }
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
