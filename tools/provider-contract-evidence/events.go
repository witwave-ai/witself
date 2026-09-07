package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

func parseEventTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

const maxEventBytes = 1 << 20
const maxStreamBytes = 64 << 20
const cursorSkip = "Cursor's supported Windows contract is WSL-as-Linux, not native Windows"

type testEvent struct {
	Time        string  `json:"Time"`
	Action      string  `json:"Action"`
	Package     string  `json:"Package"`
	Test        string  `json:"Test"`
	Elapsed     float64 `json:"Elapsed"`
	Output      string  `json:"Output"`
	FailedBuild string  `json:"FailedBuild"`
}
type testState struct {
	ran, ended, cursorReason bool
	action                   string
}
type eventStream struct {
	spec                         phaseSpec
	target                       string
	buffer                       []byte
	total                        int
	failure                      string
	packageStarted, packageEnded bool
	packageAction                string
	tests                        map[string]*testState
	cancel                       func()
}

func newEventStream(spec phaseSpec, target string, cancel func()) *eventStream {
	return &eventStream{spec: spec, target: target, tests: map[string]*testState{}, cancel: cancel}
}
func (s *eventStream) reject(category string) {
	if s.failure == "" {
		s.failure = category
		s.buffer = nil
		if s.cancel != nil {
			s.cancel()
		}
	}
}

// Discard subsequent bytes after rejection and cancel the child. Returning a
// successful write permits os/exec to drain/reap without retaining raw output.
func (s *eventStream) Write(p []byte) (int, error) {
	n := len(p)
	s.total += n
	if s.total > maxStreamBytes {
		s.reject("stream_limit")
	}
	if s.failure != "" {
		return n, nil
	}
	for len(p) > 0 {
		index := bytes.IndexByte(p, '\n')
		if index < 0 {
			index = len(p)
		}
		if len(s.buffer)+index > maxEventBytes {
			s.reject("stream_limit")
			return n, nil
		}
		s.buffer = append(s.buffer, p[:index]...)
		if index == len(p) {
			return n, nil
		}
		s.consume(s.buffer)
		s.buffer = s.buffer[:0]
		p = p[index+1:]
		if s.failure != "" {
			return n, nil
		}
	}
	return n, nil
}
func (s *eventStream) consume(raw []byte) {
	var event testEvent
	// Events contain optional fields; reports use the stricter required-field
	// decoder. Both reject duplicate/unknown keys and trailing JSON values.
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if scanJSON(d, 0) != nil {
		s.reject("malformed_stream")
		return
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(raw, &keys) != nil {
		s.reject("malformed_stream")
		return
	}
	for key := range keys {
		switch key {
		case "Time", "Action", "Package", "Test", "Elapsed", "Output", "FailedBuild":
		default:
			s.reject("malformed_stream")
			return
		}
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&event) != nil || !json.Valid(raw) || event.Package != packageName || len(event.Test) > 512 || event.Elapsed < 0 {
		s.reject("malformed_stream")
		return
	}
	if event.Time != "" { // Go may use a local offset; only the runner's UTC times are published.
		if _, err := parseEventTime(event.Time); err != nil {
			s.reject("malformed_stream")
			return
		}
	}
	if event.Test == "" {
		switch event.Action {
		case "start":
			if s.packageStarted || s.packageEnded {
				s.reject("invalid_events")
				return
			}
			s.packageStarted = true
		case "output": // includes package build and test summary text; never retained
		case "pass", "fail", "skip":
			if !s.packageStarted || s.packageEnded {
				s.reject("invalid_events")
				return
			}
			s.packageEnded = true
			s.packageAction = event.Action
		default:
			s.reject("invalid_events")
		}
		return
	}
	if !s.packageStarted || s.packageEnded {
		s.reject("invalid_events")
		return
	}
	root := strings.SplitN(event.Test, "/", 2)[0]
	known := false
	for _, test := range s.spec.tests {
		if test.Name == root {
			known = true
			break
		}
	}
	if !known {
		s.reject("unexpected_test")
		return
	}
	state := s.tests[event.Test]
	if state == nil {
		if len(s.tests) >= 1024 {
			s.reject("stream_limit")
			return
		}
		state = &testState{}
		s.tests[event.Test] = state
	}
	switch event.Action {
	case "run":
		if state.ran || state.ended {
			s.reject("invalid_events")
			return
		}
		state.ran = true
	case "output":
		if s.target == "windows-x64" && event.Test == "TestProviderIntegrationContractCursor" && strings.HasSuffix(event.Output, ": "+cursorSkip+"\n") {
			state.cursorReason = true
		}
	case "pause", "cont":
		if !state.ran || state.ended {
			s.reject("invalid_events")
		}
	case "pass", "fail", "skip":
		if !state.ran || state.ended {
			s.reject("invalid_events")
			return
		}
		state.ended = true
		state.action = event.Action
	default:
		s.reject("invalid_events")
	}
}
func (s *eventStream) finish(exit int) ([]testResult, string) {
	if len(s.buffer) > 0 {
		s.reject("truncated_stream")
	}
	results := append([]testResult(nil), s.spec.tests...)
	failure := s.failure
	if failure == "" && (!s.packageStarted || !s.packageEnded) {
		failure = "incomplete_events"
	}
	if failure == "" && s.packageAction != "pass" {
		failure = "package_failed"
	}
	for name, state := range s.tests {
		if failure != "" {
			break
		}
		if !state.ran || !state.ended {
			failure = "incomplete_events"
		} else if state.action == "fail" {
			failure = "test_failed"
		} else if state.action == "skip" && (name != "TestProviderIntegrationContractCursor" || s.target != "windows-x64" || !state.cursorReason) {
			failure = "unexpected_skip"
		}
	}
	for index, test := range results {
		state := s.tests[test.Name]
		status, reason := "incomplete", "missing_test"
		if state != nil && state.ran && state.ended {
			switch state.action {
			case "pass":
				status, reason = "passed", "none"
			case "fail":
				status, reason = "failed", "test_failed"
			case "skip":
				status, reason = "failed", "unexpected_skip"
				if s.target == "windows-x64" && test.Provider == "cursor" && state.cursorReason {
					status, reason = "not_applicable", "cursor_native_windows_unsupported"
				}
			}
		}
		if failure == "" && status != "passed" && status != "not_applicable" {
			failure = reason
		}
		if s.target == "windows-x64" && test.Provider == "cursor" && status != "not_applicable" {
			status, reason = "failed", "cursor_expected_skip"
			if failure == "" {
				failure = reason
			}
		}
		results[index].Status = status
		results[index].FailureCategory = reason
	}
	if failure == "" && exit != 0 {
		failure = "process_failed"
	}
	if failure == "" {
		failure = "none"
	}
	return results, failure
}
