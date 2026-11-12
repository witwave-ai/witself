package main

import (
	"sync"
	"testing"
	"time"
)

// The sender closes done after publishing its result, so cleanup also joins
// when the normal assertion path has already received that result.
func registerSenderCleanup[T any](t *testing.T, stop func(), done <-chan T, failureText ...string) func() {
	t.Helper()
	var once sync.Once
	cleanup := func() {
		t.Helper()
		once.Do(func() {
			stop()
			timer := time.NewTimer(consoleStartupTimeout)
			defer timer.Stop()
			for {
				select {
				case _, open := <-done:
					if !open {
						return
					}
				case <-timer.C:
					message := "sender cleanup did not finish"
					if len(failureText) != 0 {
						message = failureText[0]
					}
					t.Error(message)
					return
				}
			}
		})
	}
	t.Cleanup(cleanup)
	return cleanup
}
