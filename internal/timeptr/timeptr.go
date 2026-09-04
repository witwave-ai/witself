// Package timeptr provides explicit pointer conversions for time.Time values.
package timeptr

import "time"

// Of returns a pointer to an unchanged copy of value, including a zero value.
func Of(value time.Time) *time.Time {
	return &value
}

// NonZero returns nil for a zero value and otherwise returns a pointer to an
// unchanged copy. In particular, it preserves the value's location.
func NonZero(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return Of(value)
}

// UTC returns a non-nil pointer to value normalized to UTC. A zero value stays
// zero but still receives a non-nil pointer.
func UTC(value time.Time) *time.Time {
	return Of(value.UTC())
}
