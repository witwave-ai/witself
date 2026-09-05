package transcriptcapture

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReleaseUpgradeBarrierPreservesLiveOldFlusher(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%t", force), func(t *testing.T) {
			cfg := setupFenceCapture(t, ModeRaw)
			const canary = "release-live-old-flusher-canary"
			releaseTestEvent(t, cfg, "barrier", "run", "turn", "PostToolUse", "tool.result", canary, nil, time.Now().UTC().Add(-48*time.Hour))
			dir, err := outboxDir(cfg.Runtime)
			if err != nil {
				t.Fatal(err)
			}
			lock := filepath.Join(dir, ".flush.lock")
			if err := os.WriteFile(lock, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-2 * staleFlushLockAge)
			if err := os.Chtimes(lock, old, old); err != nil {
				t.Fatal(err)
			}
			before := releaseFileSnapshot(t)
			canaryFound := false
			for _, raw := range before {
				canaryFound = canaryFound || strings.Contains(raw, canary)
			}
			if !canaryFound {
				t.Fatal("fixture did not retain the plaintext canary")
			}
			count, err := ReleaseResidue(cfg.Runtime, "barrier", 24*time.Hour, force)
			var barrier *ReleaseUpgradeBarrierError
			if count != 0 || !errors.As(err, &barrier) || barrier.Runtime != cfg.Runtime ||
				!strings.Contains(err.Error(), "witself transcript flush --runtime "+cfg.Runtime) ||
				!strings.Contains(err.Error(), "foreground") {
				t.Fatalf("live old uploader release = %d, %v; want typed foreground-flush refusal", count, err)
			}
			if !reflect.DeepEqual(before, releaseFileSnapshot(t)) {
				t.Fatal("refused release changed the live uploader's lock, canary, sidecars, or session state")
			}
		})
	}
}

func TestReleaseUpgradeBarrierRefusesEveryLegacyOutboxEvent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session string
		kind    string
		safe    bool
	}{
		{name: "target message", session: "barrier", kind: "message.user"},
		{name: "other session message", session: "other", kind: "message.user"},
		{name: "other session metadata", session: "other", kind: "session.started"},
		{name: "already redacted result", session: "other", kind: "tool.result", safe: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := setupFenceCapture(t, ModeRaw)
			const canary = "release-legacy-barrier-canary"
			at := time.Now().UTC().Add(-48 * time.Hour)
			releaseTestEvent(t, cfg, "barrier", "run", "turn", "PostToolUse", "tool.result", canary, nil, at)
			legacy := releaseTestEvent(t, cfg, tc.session, "legacy-run", "legacy-turn", "Legacy", tc.kind, "legacy body", nil, at.Add(time.Second))
			pending, err := Pending(cfg.Runtime)
			if err != nil {
				t.Fatal(err)
			}
			legacyFound, canaryFound := false, false
			for _, item := range pending {
				canaryFound = canaryFound || bytes.Contains([]byte(item.Event.Body), []byte(canary))
				if item.Event.ID != legacy.ID {
					continue
				}
				legacyFound = true
				if tc.safe {
					event := redactOperatorReleasedEvent(item.Event)
					if !operatorReleasedEventSafe(event) {
						t.Fatal("legacy fixture is not already safely redacted")
					}
					if err := writeJSONAtomic(item.Path, event); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Remove(submissionPath(item.Path)); err != nil {
					t.Fatal(err)
				}
			}
			if !legacyFound || !canaryFound {
				t.Fatal("fixture is missing its legacy event or canary")
			}
			before := releaseFileSnapshot(t)
			count, err := ReleaseResidue(cfg.Runtime, "barrier", 0, true)
			var barrier *ReleaseUpgradeBarrierError
			if count != 0 || !errors.As(err, &barrier) || !errors.Is(err, errLegacySubmissionUnknown) ||
				!strings.Contains(err.Error(), "witself transcript flush --runtime "+cfg.Runtime) ||
				!strings.Contains(err.Error(), "foreground") {
				t.Fatalf("legacy outbox release = %d, %v; want typed foreground-flush refusal", count, err)
			}
			if !reflect.DeepEqual(before, releaseFileSnapshot(t)) {
				t.Fatal("legacy barrier refusal changed the canary, outbox, sidecars, or session state")
			}
		})
	}
}
