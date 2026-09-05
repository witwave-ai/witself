package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func setupTranscriptReleaseCLI(t *testing.T) transcriptcapture.Event {
	t.Helper()
	prompt := setupTranscriptFenceCLI(t, false)
	installReleaseUploaderTestHooks(t, compatibleReleaseUploaderTestExecutable(t))
	if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeCodex, []byte(`{"hook_event_name":"PostToolUse","session_id":"delegated","transcript_path":"/tmp/codex-delegated.jsonl","tool_name":"ordinary_tool","tool_use_id":"tool-1","tool_output":{"value":"release-cli-result-canary"}}`)); err != nil {
		t.Fatal(err)
	}
	return prompt
}

func ageTranscriptReleaseSession(t *testing.T, sessionID string, age time.Duration) (time.Time, time.Time) {
	t.Helper()
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	var first, last time.Time
	for i, item := range pending {
		if item.Event.SessionID != sessionID {
			continue
		}
		item.Event.OccurredAt = time.Now().UTC().Add(-age).Add(time.Duration(i) * time.Millisecond)
		if first.IsZero() {
			first = item.Event.OccurredAt
		}
		last = item.Event.OccurredAt
		raw, err := json.Marshal(item.Event)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(item.Path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return first, last
}

func transcriptReleaseFiles(t *testing.T) map[string]string {
	t.Helper()
	files := make(map[string]string)
	err := filepath.WalkDir(os.Getenv("WITSELF_HOME"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		files[path] = string(raw)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestTranscriptReleaseDryRunListsResidueAndChangesNothing(t *testing.T) {
	for _, args := range [][]string{
		{"--runtime", "codex"},
		{"--runtime", "codex", "--all"},
		{"--runtime", "codex", "--session", "delegated", "--dry-run"},
		{"--runtime", "codex", "--all", "--dry-run", "--yes", "--force"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			setupTranscriptReleaseCLI(t)
			first, last := ageTranscriptReleaseSession(t, "delegated", 48*time.Hour)
			before := transcriptReleaseFiles(t)
			stdout, stderr, code := captureIntegrationsCLI(t, func() int { return transcriptCmd(append([]string{"release"}, args...)) })
			if code != 0 || stderr != "" {
				t.Fatalf("release dry-run = %d, stderr %q", code, stderr)
			}
			for _, want := range []string{"SESSION", "QUEUED EVENTS", `"delegated"`, "true", first.Format(time.RFC3339Nano), last.Format(time.RFC3339Nano)} {
				if !strings.Contains(stdout, want) {
					t.Fatalf("dry-run output missing %q: %s", want, stdout)
				}
			}
			fields := strings.Fields(strings.Split(strings.TrimSpace(stdout), "\n")[1])
			if len(fields) != 5 || fields[1] != "3" {
				t.Fatalf("residue row = %q, want three queued events", fields)
			}
			if strings.Contains(stdout, "canary") {
				t.Fatal("dry-run exposed event content")
			}
			if !reflect.DeepEqual(before, transcriptReleaseFiles(t)) {
				t.Fatal("dry-run changed capture files")
			}
		})
	}
}

func TestTranscriptReleaseOlderThanRefusesFreshAndForceOverrides(t *testing.T) {
	setupTranscriptReleaseCLI(t)
	before, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--runtime", "codex", "--session", "delegated", "--yes"}
	_, stderr, code := captureIntegrationsCLI(t, func() int { return transcriptRelease(args) })
	if code != 1 || !strings.Contains(stderr, transcriptReleasePrivacyCaveat) {
		t.Fatalf("fresh release = %d, stderr %q", code, stderr)
	}
	after, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("fresh turn changed: error %v", err)
	}
	stdout, stderr, code := captureIntegrationsCLI(t, func() int { return transcriptRelease(append(args, "--force")) })
	if code != 0 || !strings.Contains(stdout, "released 1 turn(s)") || !strings.Contains(stderr, transcriptReleasePrivacyCaveat) {
		t.Fatalf("forced release = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	assertTranscriptReleaseCLIReady(t, "delegated")
}

func TestTranscriptReleaseAllSkipsFreshAndFencedTurns(t *testing.T) {
	setupTranscriptReleaseCLI(t)
	ageTranscriptReleaseSession(t, "delegated", 48*time.Hour)
	for _, sessionID := range []string{"fresh", "fenced"} {
		raw, err := json.Marshal(map[string]string{
			"hook_event_name": "UserPromptSubmit", "session_id": sessionID,
			"transcript_path": "/tmp/codex-" + sessionID + ".jsonl", "prompt": "other session prompt",
		})
		if err != nil {
			t.Fatal(err)
		}
		prompt, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeCodex, raw)
		if err != nil {
			t.Fatal(err)
		}
		if sessionID == "fenced" {
			if _, _, err := transcriptcapture.EnqueueFence(transcriptcapture.RuntimeCodex, sessionID, prompt.RunID, prompt.TurnID, "job-completed"); err != nil {
				t.Fatal(err)
			}
		}
	}
	before, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := captureIntegrationsCLI(t, func() int {
		return transcriptRelease([]string{"--runtime", "codex", "--all", "--older-than", "24h", "--yes"})
	})
	if code != 0 || !strings.Contains(stdout, "released 1 turn(s)") || strings.Contains(stdout, `"fenced"`) || !strings.Contains(stdout, `"fresh"`) || !strings.Contains(stderr, transcriptReleasePrivacyCaveat) {
		t.Fatalf("all release = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	for _, item := range before {
		if item.Event.SessionID == "delegated" {
			continue
		}
		raw, err := os.ReadFile(item.Path)
		if err != nil {
			t.Fatal(err)
		}
		var event transcriptcapture.Event
		if err := json.Unmarshal(raw, &event); err != nil || !reflect.DeepEqual(item.Event, event) {
			t.Fatalf("release touched session %s: %v", item.Event.SessionID, err)
		}
	}
	assertTranscriptReleaseCLIReady(t, "delegated")
}

func TestTranscriptReleaseSessionSelectionLeavesOtherResidueUntouched(t *testing.T) {
	setupTranscriptReleaseCLI(t)
	ageTranscriptReleaseSession(t, "delegated", 48*time.Hour)
	if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeCodex, []byte(`{"hook_event_name":"UserPromptSubmit","session_id":"other-residue","transcript_path":"/tmp/codex-other.jsonl","prompt":"other residue prompt"}`)); err != nil {
		t.Fatal(err)
	}
	ageTranscriptReleaseSession(t, "other-residue", 48*time.Hour)
	before, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := captureIntegrationsCLI(t, func() int {
		return transcriptRelease([]string{"--runtime", "codex", "--session", "delegated", "--yes"})
	})
	if code != 0 || !strings.Contains(stdout, "released 1 turn(s)") || strings.Contains(stdout, "other-residue") || !strings.Contains(stderr, transcriptReleasePrivacyCaveat) {
		t.Fatalf("session release = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	after, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	index := transcriptcapture.NewReadinessIndex(after)
	for _, item := range after {
		if item.Event.SessionID != "other-residue" {
			continue
		}
		if index.UploadReady(item) {
			t.Fatal("selected session release made other residue ready")
		}
		for _, original := range before {
			if original.Path == item.Path && !reflect.DeepEqual(original, item) {
				t.Fatal("selected session release changed other residue")
			}
		}
	}
	assertTranscriptReleaseCLIReady(t, "delegated")
}

func TestTranscriptReleaseAllReportsFailureAndContinues(t *testing.T) {
	setupTranscriptReleaseCLI(t)
	ageTranscriptReleaseSession(t, "delegated", 48*time.Hour)
	if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeCodex, []byte(`{"hook_event_name":"UserPromptSubmit","session_id":"bad-binding","transcript_path":"/tmp/codex-bad.jsonl","prompt":"foreign residue prompt"}`)); err != nil {
		t.Fatal(err)
	}
	ageTranscriptReleaseSession(t, "bad-binding", 48*time.Hour)
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	var foreignPath string
	var foreignBytes []byte
	for _, item := range pending {
		if item.Event.SessionID != "bad-binding" {
			continue
		}
		item.Event.AgentID = "foreign-agent"
		foreignBytes, err = json.Marshal(item.Event)
		if err != nil {
			t.Fatal(err)
		}
		foreignPath = item.Path
		if err := os.WriteFile(item.Path, foreignBytes, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stdout, stderr, code := captureIntegrationsCLI(t, func() int {
		return transcriptRelease([]string{"--runtime", "codex", "--all", "--yes"})
	})
	if code != 1 || !strings.Contains(stdout, "released 1 turn(s)") || !strings.Contains(stderr, "bad-binding") || !strings.Contains(stderr, transcriptReleasePrivacyCaveat) {
		t.Fatalf("partial release = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	after, err := os.ReadFile(foreignPath)
	if err != nil || !bytes.Equal(after, foreignBytes) {
		t.Fatalf("failed release changed foreign event: %v", err)
	}
	assertTranscriptReleaseCLIReady(t, "delegated")
}

func assertTranscriptReleaseCLIReady(t *testing.T, sessionID string) {
	t.Helper()
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	index := transcriptcapture.NewReadinessIndex(pending)
	foundPrompt, foundMarker := false, false
	for _, item := range pending {
		if item.Event.SessionID != sessionID {
			continue
		}
		if !index.UploadReady(item) {
			t.Fatalf("released event %s is not upload-ready", item.Event.Kind)
		}
		raw, err := json.Marshal(item.Event)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte("release-cli-result-canary")) {
			t.Fatal("released event retained tool result canary")
		}
		foundPrompt = foundPrompt || item.Event.Body == "prompt-cli-fence-canary"
		foundMarker = foundMarker || bytes.Contains(item.Event.Data, []byte(`"operator_release"`))
	}
	if !foundPrompt || !foundMarker {
		t.Fatalf("release retained prompt = %t, operator marker = %t", foundPrompt, foundMarker)
	}
}

func TestTranscriptReleaseInvalidArguments(t *testing.T) {
	setupTranscriptReleaseCLI(t)
	before := transcriptReleaseFiles(t)
	for _, args := range [][]string{
		nil,
		{"--runtime", "unknown"},
		{"--runtime", "codex", "--session", " "},
		{"--runtime", "codex", "--session", "delegated", "--all"},
		{"--runtime", "codex", "--older-than", "-1s"},
		{"--runtime", "codex", "--older-than", "yesterday"},
		{"--runtime", "codex", "--yes"},
		{"--runtime", "codex", "unexpected"},
	} {
		_, _, code := captureIntegrationsCLI(t, func() int { return transcriptRelease(args) })
		if code != 2 {
			t.Fatalf("release %q exit = %d, want 2", args, code)
		}
	}
	if !reflect.DeepEqual(before, transcriptReleaseFiles(t)) {
		t.Fatal("invalid arguments changed capture files")
	}
}
