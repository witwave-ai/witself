package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestProgressSinkEmitsNDJSON pins the wire contract for -progress-json.
// The dashboard's future phase-checklist consumer and any scripted
// automation route on the JSON keys, so drift here silently breaks
// them.
func TestProgressSinkEmitsNDJSON(t *testing.T) {
	var buf strings.Builder
	sink := &progressSink{w: &buf}
	sink.start("aws-sandbox-usw2-dev", "pulumi.up", "")
	sink.errPhase("aws-sandbox-usw2-dev", "pulumi.up", errFakeCfg("boom"))
	sink.end("aws-sandbox-usw2-dev", "pulumi.up", "")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 NDJSON lines, got %d: %q", len(lines), buf.String())
	}
	var e progressEvent
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatalf("line 0 not valid JSON: %v", err)
	}
	if e.Phase != "pulumi.up" || e.State != "start" || e.Cell != "aws-sandbox-usw2-dev" {
		t.Errorf("start event = %+v", e)
	}
	// nil sink is a no-op.
	var nilSink *progressSink
	nilSink.start("x", "y", "z") // must not panic
}

// TestProgressSinkDisabled pins the off-by-default behavior.
func TestProgressSinkDisabled(t *testing.T) {
	if s := newProgressSink(false); s != nil {
		t.Fatal("newProgressSink(false) must return nil")
	}
}

type errFakeCfg string

func (e errFakeCfg) Error() string { return string(e) }
