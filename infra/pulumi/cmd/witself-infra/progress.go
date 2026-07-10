package main

// Slice 5: structured NDJSON progress. `-progress json` on up/preview/
// destroy emits one JSON object per line to stderr for every phase
// transition (backend, pulumi, register, health-wait, argo-wait,
// evacuate, restore) alongside the existing plain-text Pulumi output
// on stdout. The dashboard consumes these to render phase-level
// checklists instead of scrolling raw text; scripts get an
// unambiguous machine-parseable timeline.
//
// The line contract is stable: {"ts":"<RFC3339Nano>","phase":"<name>",
// "state":"<start|end|error>","cell":"<name>","note":"<free-text>"}.
// Additive fields are OK; the base four are load-bearing.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// progressSink writes NDJSON events. Nil when -progress json isn't set.
type progressSink struct {
	w  io.Writer
	mu sync.Mutex
}

// newProgressSink returns a sink if the flag was set, else nil.
func newProgressSink(enabled bool) *progressSink {
	if !enabled {
		return nil
	}
	return &progressSink{w: os.Stderr}
}

// event schema — deliberately minimal so consumers can rely on it.
type progressEvent struct {
	TS    string `json:"ts"`
	Phase string `json:"phase"`
	State string `json:"state"`
	Cell  string `json:"cell,omitempty"`
	Note  string `json:"note,omitempty"`
}

// emit writes one event line. Silently drops on a nil sink so call
// sites don't have to guard.
func (p *progressSink) emit(cell, phase, state, note string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := progressEvent{
		TS:    time.Now().UTC().Format(time.RFC3339Nano),
		Phase: phase, State: state, Cell: cell, Note: note,
	}
	// One line per event — NDJSON. Errors ignored: stderr writes
	// blocking would defeat the purpose.
	raw, _ := json.Marshal(e)
	_, _ = fmt.Fprintln(p.w, string(raw))
}

// start / end / errPhase are the three transitions.
func (p *progressSink) start(cell, phase, note string) {
	p.emit(cell, phase, "start", note)
}
func (p *progressSink) end(cell, phase, note string) {
	p.emit(cell, phase, "end", note)
}
func (p *progressSink) errPhase(cell, phase string, err error) {
	if err == nil {
		return
	}
	p.emit(cell, phase, "error", err.Error())
}
