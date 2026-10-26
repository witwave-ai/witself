package syncharness

import (
	"errors"
	"time"
)

// DeadlineKind names the four deadline families controlled by model time.
type DeadlineKind string

// Deadline families are explicit so model expiry does not imply SQL clock control.
const (
	GrantDeadline     DeadlineKind = "grant"
	LeaseDeadline     DeadlineKind = "lease"
	RetentionDeadline DeadlineKind = "retention"
	ACKDeadline       DeadlineKind = "ack"
)

// Deadline is a synthetic expiry point in the logical clock.
type Deadline struct {
	Kind DeadlineKind
	At   time.Time
}

var (
	// ErrClockRollback prevents uncertain expiry from granting an extension.
	ErrClockRollback = errors.New("sync harness: model clock rollback makes expiry uncertain")
	// ErrDeadline rejects unknown kinds and invalid durations or timestamps.
	ErrDeadline = errors.New("sync harness: invalid model deadline")
)

// Clock is a logical clock. Rollback uncertainty is sticky: catching up to an old
// reading cannot establish that an expired grant or lease remains valid.
type Clock struct {
	now       time.Time
	uncertain bool
}

// NewClock starts at a caller-supplied synthetic instant.
func NewClock(start time.Time) *Clock { return &Clock{now: start.UTC()} }

// Now returns logical time without consulting a wall clock.
func (c *Clock) Now() time.Time { return c.now }

// Uncertain reports whether any observed clock rollback makes expiry uncertain.
func (c *Clock) Uncertain() bool { return c.uncertain }

// Advance moves model time and records negative movement as sticky uncertainty.
func (c *Clock) Advance(delta time.Duration) error {
	c.now = c.now.Add(delta)
	if delta < 0 {
		c.uncertain = true
		return ErrClockRollback
	}
	return nil
}

func validDeadlineKind(kind DeadlineKind) bool {
	return kind == GrantDeadline || kind == LeaseDeadline || kind == RetentionDeadline || kind == ACKDeadline
}

// NewDeadline creates a bounded model deadline only while expiry is certain.
func (c *Clock) NewDeadline(kind DeadlineKind, duration time.Duration) (Deadline, error) {
	if !validDeadlineKind(kind) || duration < 0 {
		return Deadline{}, ErrDeadline
	}
	if c.uncertain {
		return Deadline{}, ErrClockRollback
	}
	return Deadline{Kind: kind, At: c.now.Add(duration)}, nil
}

// Expired includes the exact deadline boundary and refuses uncertain conclusions.
func (c *Clock) Expired(deadline Deadline) (bool, error) {
	if !validDeadlineKind(deadline.Kind) || deadline.At.IsZero() {
		return false, ErrDeadline
	}
	if c.uncertain {
		return false, ErrClockRollback
	}
	return !c.now.Before(deadline.At), nil
}

// Extend refuses changes after rollback and preserves the original on failure.
func (c *Clock) Extend(deadline Deadline, extension time.Duration) (Deadline, error) {
	if !validDeadlineKind(deadline.Kind) || deadline.At.IsZero() || extension < 0 {
		return deadline, ErrDeadline
	}
	if c.uncertain {
		return deadline, ErrClockRollback
	}
	deadline.At = deadline.At.Add(extension)
	return deadline, nil
}
