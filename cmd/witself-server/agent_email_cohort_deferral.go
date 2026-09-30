package main

import (
	"fmt"
	"io"
	"sync"
	"time"
)

const agentEmailCohortDeferralLogInterval = time.Minute

func agentEmailCohortDeferralLine(count uint64) string {
	return fmt.Sprintf("witself-server: agent-email production receive cohort deferral count=%d", count)
}

type agentEmailCohortDeferralLog struct {
	mu      sync.Mutex
	out     io.Writer
	now     func() time.Time
	last    time.Time
	written bool
	pending uint64
}

func newAgentEmailCohortDeferralLog(out io.Writer, now func() time.Time) *agentEmailCohortDeferralLog {
	return &agentEmailCohortDeferralLog{out: out, now: now}
}

func (l *agentEmailCohortDeferralLog) observe() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending++
	now := l.now()
	if l.written && now.Sub(l.last) < agentEmailCohortDeferralLogInterval {
		return
	}
	_, _ = fmt.Fprintln(l.out, agentEmailCohortDeferralLine(l.pending))
	l.pending = 0
	l.last = now
	l.written = true
}
