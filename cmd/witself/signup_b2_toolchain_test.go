//go:build signup_b2_acceptance

package main

import "testing"

func TestSignupB2CLIToolchain(t *testing.T) {
	type testCase struct {
		name      string
		goMod     string
		goVersion string
		wantError string
	}
	tests := []testCase{
		{
			name:      "explicit pin overrides language version",
			goMod:     "module example.test/cli\ngo 1.27.0\ntoolchain go1.27.1\n",
			goVersion: "go1.27.1",
		},
		{
			name:      "toolchain bump follows module",
			goMod:     "module example.test/cli\ngo 1.28.0\ntoolchain go1.28.2\n",
			goVersion: "go1.28.2",
		},
		{
			name:      "comments and whitespace",
			goMod:     "// toolchain go1.26.6\r\n\ttoolchain\tgo1.29rc1  // pinned compiler\r\n",
			goVersion: "go1.29rc1",
		},
		{
			name:      "stale artifact rejected with both versions",
			goMod:     "go 1.27.0\ntoolchain go1.27.1\n",
			goVersion: "go1.26.6",
			wantError: `B2 artifact Go version "go1.26.6" does not match go.mod toolchain "go1.27.1"`,
		},
		{
			name:      "newer artifact also rejected",
			goMod:     "go 1.27.0\ntoolchain go1.27.1\n",
			goVersion: "go1.28.2",
			wantError: `B2 artifact Go version "go1.28.2" does not match go.mod toolchain "go1.27.1"`,
		},
		{
			name:      "language version is not a toolchain pin",
			goMod:     "go 1.27.1\n// toolchain go1.27.1\n",
			goVersion: "go1.27.1",
			wantError: "B2 go.mod is missing a pinned toolchain directive",
		},
	}
	for _, directive := range []string{
		"toolchain",
		"toolchain default",
		"toolchain not-a-go-version",
		"toolchain go1.27.1 extra",
		"toolchain go1.27.1\ntoolchain go1.27.1",
	} {
		tests = append(tests, testCase{
			name:      "invalid pin/" + directive,
			goMod:     directive + "\n",
			goVersion: "go1.27.1",
			wantError: "B2 go.mod must contain one valid pinned toolchain directive",
		})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := b2CLICheckToolchain(tt.goVersion, []byte(tt.goMod))
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("matching artifact rejected: %v", err)
				}
			} else if err == nil || err.Error() != tt.wantError {
				t.Fatalf("toolchain check error = %v, want %q", err, tt.wantError)
			}
		})
	}
}
