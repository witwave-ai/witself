package transcriptcapture

import "strings"

// Deferred bucket names explain, value-free, why a queued event is still held.
// They are stable console vocabulary, not stored data.
const (
	// DeferredBucketNoFence is the session's currently bound run waiting for a
	// terminal event: a Stop, a session end, a later prompt, or a companion
	// fence from the launcher that started the job.
	DeferredBucketNoFence = "no-fence"
	// DeferredBucketRunMismatch is a run the session no longer binds. A build
	// without run-rollover fencing orphaned these events; the launcher can
	// still close them with `transcript fence --latest`.
	DeferredBucketRunMismatch = "run-mismatch"
	// DeferredBucketSessionUnbound is a session with no local state at all, so
	// no run is bound and nothing local can derive the missing terminal.
	DeferredBucketSessionUnbound = "session-unbound"
	// DeferredBucketIdentityMismatch is queued under another account, realm,
	// agent or location of this runtime; held for that binding, it uploads if
	// that binding is installed again.
	DeferredBucketIdentityMismatch = "identity-mismatch"
)

// DeferredSummary counts held events per bucket. Counts are the only thing it
// carries: no session, run, turn, prompt, tool, or path value is ever exposed.
type DeferredSummary struct {
	NoFence          int
	RunMismatch      int
	SessionUnbound   int
	IdentityMismatch int
}

// Total is the number of queued events held for a binding or by the upload gate.
func (s DeferredSummary) Total() int {
	return s.NoFence + s.RunMismatch + s.SessionUnbound + s.IdentityMismatch
}

// SummarizeDeferred classifies events held for a binding or by the upload gate.
// With a binding, identity mismatches take precedence over readiness. Other
// events use local session state to distinguish orphaned runs from open runs.
// A nil binding preserves readiness-only classification. It never reports
// event content.
func SummarizeDeferred(runtime string, pending []PendingEvent, cfg *Config) (DeferredSummary, error) {
	runtime, err := NormalizeRuntime(runtime)
	if err != nil {
		return DeferredSummary{}, err
	}
	var summary DeferredSummary
	index := NewReadinessIndex(pending)
	boundRuns := make(map[string]string)
	for _, item := range pending {
		if item.Event.Runtime != runtime {
			continue
		}
		// Pathless Codex events belong to the existing ephemeral partition,
		// which a flush applies before inspecting the installed binding.
		ephemeral := runtime == RuntimeCodex && strings.TrimSpace(item.Event.SourceTranscriptPath) == ""
		if cfg != nil && !ephemeral && HeldForBinding(runtime, item.Event, *cfg) {
			summary.IdentityMismatch++
			continue
		}
		if index.UploadReady(item) {
			continue
		}
		sessionID := item.Event.SessionID
		boundRun, resolved := boundRuns[sessionID]
		if !resolved {
			state, err := loadSessionState(runtime, sessionID)
			if err != nil {
				return DeferredSummary{}, err
			}
			boundRun = state.RunID
			boundRuns[sessionID] = boundRun
		}
		switch {
		case boundRun == "":
			summary.SessionUnbound++
		case boundRun != item.Event.RunID:
			summary.RunMismatch++
		default:
			summary.NoFence++
		}
	}
	return summary, nil
}
