package main

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

func TestAgentEmailCohortDeferralLine(t *testing.T) {
	if got := agentEmailCohortDeferralLine(7); got != "witself-server: agent-email production receive cohort deferral count=7" {
		t.Fatalf("deferral line = %q", got)
	}
}

func TestAgentEmailCohortDeferralLogThrottles(t *testing.T) {
	var out bytes.Buffer
	start := time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC)
	now := start
	log := newAgentEmailCohortDeferralLog(&out, func() time.Time { return now })
	log.observe()
	want := agentEmailCohortDeferralLine(1) + "\n"
	if out.String() != want {
		t.Fatal("first deferral did not write exactly one line")
	}
	for _, elapsed := range []time.Duration{time.Second, 30 * time.Second, 59 * time.Second} {
		now = start.Add(elapsed)
		log.observe()
		if out.String() != want {
			t.Fatal("deferral logged within throttle interval")
		}
	}
	now = start.Add(time.Minute)
	log.observe()
	want += agentEmailCohortDeferralLine(4) + "\n"
	if out.String() != want {
		t.Fatal("interval boundary did not report all pending deferrals")
	}
	log.observe()
	if out.String() != want {
		t.Fatal("immediate deferral bypassed throttle")
	}
	now = now.Add(-time.Hour)
	log.observe()
	if out.String() != want {
		t.Fatal("backwards clock bypassed throttle")
	}
}

func TestAgentEmailCohortDeferralLogConcurrent(t *testing.T) {
	var out bytes.Buffer
	log := newAgentEmailCohortDeferralLog(&out, func() time.Time { return time.Time{} })
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.observe()
		}()
	}
	wg.Wait()
	if out.String() != agentEmailCohortDeferralLine(1)+"\n" {
		t.Fatal("concurrent observations did not produce exactly one line")
	}
}
