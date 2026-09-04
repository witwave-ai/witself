package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/envconfig"
	"github.com/witwave-ai/witself/internal/supportrunner"
)

type inertRunner struct{}

func (inertRunner) Run(context.Context) error { return nil }

func TestServeRefusesDarkGateBeforeConstructingDependencies(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{name: "unset", env: nil},
		{name: "empty", env: map[string]string{"WITSELF_SUPPORT_RUNNER_ENABLED": ""}},
		{name: "false", env: map[string]string{"WITSELF_SUPPORT_RUNNER_ENABLED": "false"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			constructed := false
			factory := func(supportrunner.Config, func(string)) (runner, error) {
				constructed = true
				return inertRunner{}, nil
			}
			var stderr bytes.Buffer
			if code := serve(mapLookup(tc.env), factory, &stderr); code == 0 {
				t.Fatal("serve returned success with the dark gate off")
			}
			if constructed {
				t.Fatal("dark serve constructed the API/LLM runner")
			}
			if !strings.Contains(stderr.String(), "WITSELF_SUPPORT_RUNNER_ENABLED=true") {
				t.Fatalf("stderr = %q; want explicit enablement refusal", stderr.String())
			}
		})
	}
}

func TestServeRejectsNonBooleanGateBeforeConstructingDependencies(t *testing.T) {
	constructed := false
	factory := func(supportrunner.Config, func(string)) (runner, error) {
		constructed = true
		return inertRunner{}, nil
	}
	var stderr bytes.Buffer
	code := serve(
		mapLookup(map[string]string{"WITSELF_SUPPORT_RUNNER_ENABLED": "yes"}),
		factory,
		&stderr,
	)
	if code == 0 {
		t.Fatal("serve accepted a non-boolean enablement flag")
	}
	if constructed {
		t.Fatal("invalid gate constructed the API/LLM runner")
	}
	if !strings.Contains(stderr.String(), "must be true or false") {
		t.Fatalf("stderr = %q; want boolean validation error", stderr.String())
	}
}

func TestRunVersionHelpAndUnknownDoNotConstructRunner(t *testing.T) {
	constructed := false
	factory := func(supportrunner.Config, func(string)) (runner, error) {
		constructed = true
		return inertRunner{}, nil
	}

	for _, tc := range []struct {
		name     string
		args     []string
		wantCode int
	}{
		{name: "empty shows help", args: nil, wantCode: 0},
		{name: "help", args: []string{"help"}, wantCode: 0},
		{name: "version", args: []string{"version"}, wantCode: 0},
		{name: "unknown", args: []string{"unknown"}, wantCode: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := runWith(tc.args, mapLookup(nil), factory, &stdout, &stderr); code != tc.wantCode {
				t.Fatalf("runWith() = %d, want %d", code, tc.wantCode)
			}
		})
	}
	if constructed {
		t.Fatal("non-serve commands constructed the API/LLM runner")
	}
}

func TestEnvOr(t *testing.T) {
	lookup := mapLookup(map[string]string{"SET": " 127.0.0.1:0 ", "EMPTY": " "})
	if got := envconfig.TrimmedOr(lookup, "SET", "fallback"); got != "127.0.0.1:0" {
		t.Fatalf("SET = %q", got)
	}
	if got := envconfig.TrimmedOr(lookup, "EMPTY", "fallback"); got != "fallback" {
		t.Fatalf("EMPTY = %q", got)
	}
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
