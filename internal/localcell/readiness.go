package localcell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// These observations are scoped to children launched by this handle. Durable
// process records continue to own process lifetime; the observer never signals.
type childObservation struct {
	pid       int
	logOffset int64
	done      chan struct{}
	status    *os.ProcessState
}

func (c *Cell) beginChildObservation(component string, logOffset int64) {
	c.childrenMu.Lock()
	defer c.childrenMu.Unlock()
	if c.children == nil {
		c.children = make(map[string]*childObservation)
	}
	c.children[component] = &childObservation{logOffset: logOffset, done: make(chan struct{})}
}

func (c *Cell) observeChildExit(component string, pid int, wait func() (*os.ProcessState, error)) error {
	if wait == nil {
		// Native launchers use the standard library's process observer; no
		// platform-specific status decoding or launch policy is needed here.
		process, err := os.FindProcess(pid)
		if err != nil {
			return errors.New("cannot observe local cell component exit")
		}
		wait = process.Wait
	}
	c.childrenMu.Lock()
	observation := c.children[component]
	if observation != nil {
		observation.pid = pid
	}
	c.childrenMu.Unlock()
	go func() {
		state, _ := wait()
		if observation != nil {
			observation.status = state
			close(observation.done)
		}
	}()
	return nil
}

type readinessExitError struct {
	observation *readinessError
	component   string
	exitStatus  string
	diagnostic  string
}

func (e *readinessExitError) Error() string {
	return fmt.Sprintf("local cell %s exited before readiness (exit status %s): %s", e.component, e.exitStatus, e.diagnostic)
}

// Unwrap preserves PW3's pre-rollback process observation alongside the
// additional value-free exit detail of a child launched by this handle.
func (e *readinessExitError) Unwrap() error { return e.observation }

func (c *Cell) childReadinessExit(ctx context.Context, component string, pid int) error {
	failure := &readinessError{component: component, state: ProcessState{PID: pid}}
	exit := &readinessExitError{observation: failure, component: component, exitStatus: "unavailable", diagnostic: "value-free diagnostic unavailable"}
	c.childrenMu.Lock()
	observation := c.children[component]
	if observation != nil && observation.pid != pid {
		observation = nil
	}
	c.childrenMu.Unlock()
	if observation == nil {
		// No launch observer owns this identity. Keep PW3's fixed failure and
		// observation rather than attaching an unavailable child diagnostic.
		return failure
	}
	select {
	case <-observation.done:
	case <-ctx.Done():
	}
	// Completion wins if both channels are ready. Reading status only after
	// receiving done also retains the observer goroutine's publication fence.
	select {
	case <-observation.done:
		if observation.status != nil {
			exit.exitStatus = fmt.Sprint(observation.status.ExitCode())
		}
	default:
	}
	if diagnostic := c.lastValueFreeDiagnostic(component, observation.logOffset); diagnostic != "" {
		exit.diagnostic = diagnostic
	}
	return exit
}

const maxReadinessDiagnostic = 256

// Component output may contain arbitrary values. Only recognized, reconstructed
// value-free diagnostics may cross into caller errors or durable journals.
// Restrict reading to this launch and a bounded tail, never an entire log.
func (c *Cell) lastValueFreeDiagnostic(component string, offset int64) string {
	f, err := openPrivateFile(c.path("logs", component+".log"), os.O_RDONLY, 0)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	const maxTail = 4096
	if offset < info.Size()-maxTail {
		offset = info.Size() - maxTail
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(f, maxTail))
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if diagnostic := valueFreeDiagnostic(component, lines[i]); diagnostic != "" {
			if len(diagnostic) > maxReadinessDiagnostic {
				diagnostic = diagnostic[:maxReadinessDiagnostic]
			}
			return diagnostic
		}
	}
	return ""
}

func valueFreeDiagnostic(component, line string) string {
	// log's default timestamp is metadata, never part of the saved diagnostic.
	if len(line) > 20 && line[19] == ' ' {
		if _, err := time.Parse("2006/01/02 15:04:05", line[:19]); err == nil {
			line = line[20:]
		}
	}
	if component == "worker" {
		job, ok := strings.CutPrefix(line, "witself-worker: worker job restart limit exceeded: ")
		if ok {
			switch job {
			case "avatar_style_rollout", "message_rate_bucket_cleanup", "agent_email_rate_bucket_cleanup", "transcript_storage_reconcile":
				return "witself-worker: worker job restart limit exceeded: " + job
			}
		}
		return ""
	}
	if component != "server" {
		return ""
	}
	remainder, ok := strings.CutPrefix(line, "witself-server: migration wait exceeded ")
	if !ok {
		return ""
	}
	bound, phase, ok := strings.Cut(remainder, " during ")
	if !ok || (phase != "advisory lock" && phase != "preflight" && phase != "verify") {
		return ""
	}
	duration, err := time.ParseDuration(bound)
	if err != nil || duration <= 0 {
		return ""
	}
	return fmt.Sprintf("witself-server: migration wait exceeded %s during %s", duration, phase)
}
