package main

import (
	"testing"

	"github.com/witwave-ai/witself/internal/tokenfile"
)

func TestLocalBootstrapNeverReadsHostedDefault(t *testing.T) {
	getenv := func(name string) string {
		if name == "WITSELF_RUNTIME" {
			return "local"
		}
		return ""
	}
	value, err := bootstrapTokenFromEnv(getenv, func(string, tokenfile.Options) (string, error) {
		t.Fatal("local runtime tried to read an ambient bootstrap credential")
		return "", nil
	})
	if err != nil || value != "" {
		t.Fatal("local runtime adopted an ambient bootstrap credential")
	}
}

func TestBootstrapSourceSelectionPreservesHostedAndExplicitSources(t *testing.T) {
	for _, tc := range []struct {
		name, runtime, file, value string
		readDefault                bool
	}{
		{name: "hosted-default", readDefault: true},
		{name: "hosted-explicit-file", file: "synthetic-explicit-path"},
		{name: "local-explicit-file", runtime: "local", file: "synthetic-explicit-path"},
		{name: "local-explicit-value", runtime: "local", value: "synthetic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			getenv := func(name string) string {
				switch name {
				case "WITSELF_RUNTIME":
					return tc.runtime
				case "WITSELF_BOOTSTRAP_TOKEN_FILE":
					return tc.file
				case "WITSELF_BOOTSTRAP_TOKEN":
					return tc.value
				default:
					return ""
				}
			}
			value, err := bootstrapTokenFromEnv(getenv, func(path string, options tokenfile.Options) (string, error) {
				calls++
				if tc.readDefault {
					if path != defaultBootstrapTokenFile || !options.AllowMissing {
						t.Fatal("hosted fallback semantics changed")
					}
				} else if path != tc.file || options.AllowMissing {
					t.Fatal("explicit bootstrap source semantics changed")
				}
				return "synthetic", nil
			})
			if err != nil || value != "synthetic" || (tc.value == "" && calls != 1) || (tc.value != "" && calls != 0) {
				t.Fatal("bootstrap source selection changed")
			}
		})
	}
}
