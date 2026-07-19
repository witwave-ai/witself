package main

import (
	"strings"
	"testing"
)

func TestPasswordGenerateCLI(t *testing.T) {
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return run([]string{
			"password", "generate", "--length", "48", "--symbols=false",
			"--exclude-ambiguous",
		})
	})
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	password := strings.TrimSuffix(stdout, "\n")
	if len(password) != 48 {
		t.Fatalf("password length = %d, want 48", len(password))
	}
	if strings.ContainsAny(password, "!@#$%^&*()-_=+[]{}:,.?01IOilo|") {
		t.Fatalf("password contains an excluded character")
	}
}

func TestPasswordGenerateCLIRejectsUnsafeOrImpossiblePolicy(t *testing.T) {
	for _, args := range [][]string{
		{"password"},
		{"password", "generate", "--length", "3"},
		{"password", "generate", "--lowercase=false", "--uppercase=false", "--digits=false", "--symbols=false"},
		{"password", "generate", "extra"},
	} {
		stdout, _, code := captureFactDeleteCLI(t, func() int { return run(args) })
		if code != 2 || stdout != "" {
			t.Fatalf("run(%q) = code %d stdout %q, want 2 and no value", args, code, stdout)
		}
	}
}
