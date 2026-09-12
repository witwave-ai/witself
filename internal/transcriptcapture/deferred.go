package transcriptcapture

// Deferred bucket names explain, value-free, why the upload gate still holds a
// queued event. They are stable console vocabulary, not stored data.
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
)

// DeferredSummary counts held events per bucket. Counts are the only thing it
// carries: no session, run, turn, prompt, tool, or path value is ever exposed.
type DeferredSummary struct {
	NoFence        int
	RunMismatch    int
	SessionUnbound int
}

// Total is the number of queued events the upload gate holds.
func (s DeferredSummary) Total() int {
	return s.NoFence + s.RunMismatch + s.SessionUnbound
}

// SummarizeDeferred classifies every queued event that the upload gate holds.
// It reads local session state to tell an orphaned run from a run that is
// merely still open, and never loads or reports event content.
func SummarizeDeferred(runtime string, pending []PendingEvent) (DeferredSummary, error) {
	runtime, err := NormalizeRuntime(runtime)
	if err != nil {
		return DeferredSummary{}, err
	}
	var summary DeferredSummary
	index := NewReadinessIndex(pending)
	boundRuns := make(map[string]string)
	for _, item := range pending {
		if item.Event.Runtime != runtime || index.UploadReady(item) {
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
