package store

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/activity"
)

func TestActivityEventUnitsAndQuantities(t *testing.T) {
	for _, dimension := range append(activity.Dimensions(), "activity_tracking_started") {
		t.Run(dimension, func(t *testing.T) {
			base := usageEventInput{AccountID: "acc_1", RealmID: "rlm_1", AgentID: "agt_1", Dimension: dimension, Unit: activity.Unit(dimension), Quantity: 1, SubjectType: "activity", SubjectID: "opaque", IdempotencyKey: "opaque-key"}
			for _, tc := range []struct {
				unit     string
				quantity int64
				valid    bool
			}{{base.Unit, 1, true}, {base.Unit, 0, false}, {base.Unit, -1, false}, {"byte", 1, false}, {base.Unit, 2, base.Unit == "record"}} {
				in := base
				in.Unit = tc.unit
				in.Quantity = tc.quantity
				if err := validateUsageEventInput(&in); (err == nil) != tc.valid {
					t.Fatalf("unit=%s quantity=%d: %v", tc.unit, tc.quantity, err)
				}
			}
		})
	}
}

func TestActivityReadMarkerWaitHonorsCancellation(t *testing.T) {
	st := &Store{}
	p := Principal{Kind: PrincipalAgent, AccountID: "acc_wait", RealmID: "rlm_wait", ID: "agt_wait"}
	marker := &activityReadMarker{initializing: make(chan struct{}, 1)}
	marker.initializing <- struct{}{}
	st.activityMarkers.Store([3]string{p.AccountID, p.RealmID, p.ID}, marker)
	ctx, cancel := context.WithCancel(activity.WithRequestID(t.Context(), activity.NewRequestID()))
	cancel()
	done := make(chan error, 1)
	go func() {
		_, finish := st.beginActivityRead(ctx, p, "facts.list")
		var readErr error
		finish(0, &readErr)
		done <- readErr
	}()
	select {
	case err := <-done:
		if err != nil || st.ActivityMeteringFailures() != 1 {
			t.Fatal("canceled meter changed the read result or missed its failure counter", err)
		}
	case <-time.After(5 * time.Second):
		// Release the synthetic in-flight initializer before reporting failure.
		<-marker.initializing
		t.Fatal("first-marker wait ignored cancellation")
	}
}

func TestActivityReadWarningRateLimitConcurrent(t *testing.T) {
	var limiter activityReadWarningLimiter
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if limiter.allow(now) {
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("concurrent warnings admitted = %d", admitted.Load())
	}
	if limiter.allow(now.Add(time.Minute - time.Nanosecond)) {
		t.Fatal("warning before one minute")
	}
	if !limiter.allow(now.Add(time.Minute)) {
		t.Fatal("warning suppressed after one minute")
	}
}

func TestActivityReadWarningIncludesCause(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	original := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = original; _ = w.Close() }()
	var limiter activityReadWarningLimiter
	now := time.Now()
	limiter.warn(now, "facts.list", errors.New("synthetic read meter failure"))
	limiter.warn(now, "memories.list", errors.New("suppressed failure"))
	_ = w.Close()
	output, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	want := "witself: activity read metering failed: operation=facts.list: synthetic read meter failure\n"
	if string(output) != want || strings.Count(string(output), "\n") != 1 {
		t.Fatalf("warning = %q", output)
	}
}
