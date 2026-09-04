// Package envconfig provides small helpers for reading environment-backed
// configuration consistently.
package envconfig

import "strings"

// RawOr returns the value for key unchanged, or fallback when key is missing or
// explicitly empty. It matches os.Getenv-style configuration semantics.
func RawOr(lookup func(string) (string, bool), key, fallback string) string {
	value, _ := lookup(key)
	if value == "" {
		return fallback
	}
	return value
}

// TrimmedOr returns the trimmed value for key, or fallback when key is missing
// or its value is empty after trimming.
func TrimmedOr(lookup func(string) (string, bool), key, fallback string) string {
	value, ok := lookup(key)
	if !ok {
		return fallback
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

// TrimmedPresentOr returns fallback only when key is missing. A present value
// is trimmed and returned even when it becomes empty, allowing callers to
// validate an explicitly blank setting rather than silently defaulting it.
func TrimmedPresentOr(lookup func(string) (string, bool), key, fallback string) string {
	value, ok := lookup(key)
	if !ok {
		return fallback
	}
	return strings.TrimSpace(value)
}
