//go:build !darwin && !linux

package main

import "testing"

func TestAntigravityIntegrationReportsUnsupportedAtomicPlatform(t *testing.T) {
	if release, err := acquireAntigravityOperationLock(); err == nil {
		release()
		t.Fatal("Antigravity integration lock unexpectedly succeeded on an unsupported platform")
	}
}
