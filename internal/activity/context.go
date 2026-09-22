// Package activity defines cooperative, nonbilling activity intent. It conveys
// neither authorization nor legacy usage/ranking/read-acknowledgment semantics.
package activity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

type key uint8

const (
	observationKey key = iota
	requestKey
	operationKey
)

// These headers convey cooperative activity intent, never authentication.
const (
	RequestIDHeader   = "X-Witself-Activity-ID"
	ObservationHeader = "X-Witself-Activity-Observation"
)

// WithObservation suppresses only the new nonbilling activity scope.
func WithObservation(ctx context.Context) context.Context {
	return context.WithValue(ctx, observationKey, true)
}

// WithDeliberate clears observation for an explicit action, preserving its key.
func WithDeliberate(ctx context.Context) context.Context {
	return context.WithValue(ctx, observationKey, false)
}

// IsObservation reports whether the current activity scope is passive.
func IsObservation(ctx context.Context) bool { v, _ := ctx.Value(observationKey).(bool); return v }

// WithRequestID scopes one bounded request, including its exact retries. Callers
// must not share this scope across different requests or pages.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestKey, id)
}

// RequestID returns the caller key for one bounded request, or an empty string.
func RequestID(ctx context.Context) string { v, _ := ctx.Value(requestKey).(string); return v }

// ValidRequestID permits only bounded opaque ASCII keys, never free-form text.
func ValidRequestID(s string) bool {
	if len(s) < 16 || len(s) > 96 {
		return false
	}
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// NewRequestID returns a safe opaque random key for a new bounded request.
func NewRequestID() string { var b [24]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }

// WithOperation is a server/store descriptor selected only from Catalog. An
// empty descriptor explicitly excludes nested work (hydration, listeners, etc.).
func WithOperation(ctx context.Context, operation string) context.Context {
	return context.WithValue(ctx, operationKey, operation)
}

// Operation returns the outer descriptor, distinguishing exclusion from absence.
func Operation(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(operationKey).(string)
	return v, ok
}

// DefaultOperation selects a descriptor only when no outer scope exists.
func DefaultOperation(ctx context.Context, operation string) context.Context {
	if _, ok := Operation(ctx); ok {
		return ctx
	}
	return WithOperation(ctx, operation)
}
