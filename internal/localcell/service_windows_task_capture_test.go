package localcell

import (
	"bytes"
	"sync"
	"testing"
)

func TestWindowsTaskCaptureCombinedCap(t *testing.T) {
	capture := &windowsTaskCapture{}
	var workers sync.WaitGroup
	for _, writer := range []windowsTaskCaptureWriter{capture.stdout(), capture.stderr()} {
		workers.Go(func() { _, _ = writer.Write(bytes.Repeat([]byte("x"), windowsTaskOutputLimit)) })
	}
	workers.Wait()
	if !capture.exceeded() || len(capture.output())+len(capture.errorData) > windowsTaskOutputLimit || capture.used != windowsTaskOutputLimit {
		t.Fatal("combined manager output escaped its cap")
	}
	_, _ = capture.stdout().Write([]byte("trailing"))
	if capture.used != windowsTaskOutputLimit {
		t.Fatal("overflow retained extra manager output")
	}
}

func TestWindowsTaskCaptureDoesNotMixStderr(t *testing.T) {
	capture := &windowsTaskCapture{}
	_, _ = capture.stderr().Write([]byte("ERROR absence in arbitrary language"))
	_, _ = capture.stdout().Write([]byte("<Task/>"))
	if string(capture.output()) != "<Task/>" || capture.exceeded() {
		t.Fatal("stderr became XML or classification evidence")
	}
}

func TestWindowsTaskOwnerPolicy(t *testing.T) {
	for _, tc := range []struct {
		uid            string
		elevated, want bool
	}{
		{"S-1-5-21-1-2-3-1000", false, true},
		{"S-1-5-21-1-2-3-1000", true, false},
		{"S-1-5-18", false, false}, {"S-1-5-19", false, false}, {"S-1-5-20", false, false},
		{"", false, false}, {"0", false, false},
	} {
		if validWindowsTaskOwner(tc.uid, tc.elevated) != tc.want {
			t.Fatal("task owner policy accepted elevated or service identity")
		}
	}
}
