package agenttui

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSecretRevealReasonsAreClosedAndValueFree(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"unenrolled", SecretUnenrolled, "Vault key is not enrolled here"},
		{"mismatch", SecretMismatch, "Local vault key does not match"},
		{"unavailable", SecretUnavailable, "Reveal unavailable"},
		{"canceled", SecretCanceled, "Reveal canceled"},
		{"context", context.Canceled, "Reveal canceled"},
		{"unknown enum", SecretRevealReason(255), "Reveal unavailable"},
		{"arbitrary error", errors.New("PRIVATE_VALUE /private/key/path"), "Reveal unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			m := testModel(t, f)
			openPanel(t, m, 6)
			owned := []byte("PRIVATE_VALUE")
			f.secret = func(context.Context, string, string) ([]byte, error) { return owned, tc.err }
			runPrivate(t, m, m.key(key("v")))
			if !allZero(owned) {
				t.Fatal("error buffer retained")
			}
			view := m.vp.View()
			if !strings.Contains(view, tc.want) {
				t.Fatalf("missing fixed reason %q", tc.want)
			}
			if strings.Contains(view, "PRIVATE_VALUE") || strings.Contains(view, "/private/key/path") {
				t.Fatal("private diagnostic rendered")
			}
		})
	}
}

func TestConsoleUnknownLaunchNotice(t *testing.T) {
	msg := consoleMsg{action: consoleOpen, status: ConsoleStatus{State: ConsoleRunning, Port: 1234}, unknown: true}
	if got := consoleOutcome(msg); got != "Browser launched; opening outcome is unknown." {
		t.Fatal("informational outcome rendered as failure or success")
	}
}
