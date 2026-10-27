package syncmodel

import (
	"errors"
	"time"
)

// LogicalAction retains the original actor/time/class across replay. Synthetic
// action classes are a closed miniature subset of the O-13 catalog.
type LogicalAction struct {
	AccountID     string
	Operation     OperationID
	ActionOrdinal Ordinal
	Actor         string
	OccurredAt    time.Time
	Class         string
	Dimension     string
	Quantity      uint64
}

type actionIdentity struct {
	account string
	op      OperationID
	ordinal Ordinal
}

// RollupKey includes complete account/actor/dimension and UTC bucket scope.
type RollupKey struct {
	AccountID string
	Actor     string
	Dimension string
	Bucket    string
	Start     time.Time
}

// Rollup contains derived action count and quantity, never another source event.
type Rollup struct {
	Count    uint64
	Quantity uint64
}

// AccountingCounts is the original-event/hour/day vector asserted by O-13.
type AccountingCounts struct {
	OriginalEvents uint64
	HourCount      uint64
	DayCount       uint64
	ReplayWork     uint64
}

// LogicalLedger separates immutable actions, replay diagnostics and rebuildable
// rollups. Its zero value is ready for use by a single model owner.
type LogicalLedger struct {
	actions map[actionIdentity]LogicalAction
	rollups map[RollupKey]Rollup
	replay  map[evidenceIdentity]bool
}

func (a LogicalAction) validate() error {
	if !validLabel(a.AccountID) || !validLabel(a.Actor) || !validLabel(a.Dimension) || a.OccurredAt.IsZero() || a.ActionOrdinal.Validate() != nil {
		return errors.New("invalid logical action")
	}
	switch a.Class {
	case "fixture_mutation", "fixture_audited_read", "fixture_compound":
		return nil
	default:
		return errors.New("unsupported logical action")
	}
}

// Record adds a required original action exactly once. Replaying changed
// provenance under the same action identity refuses rather than counting again.
func (l *LogicalLedger) Record(a LogicalAction) (bool, error) {
	if err := a.validate(); err != nil {
		return false, err
	}
	a.OccurredAt = a.OccurredAt.UTC().Round(0)
	k := actionIdentity{a.AccountID, a.Operation, a.ActionOrdinal}
	if old, ok := l.actions[k]; ok {
		if old != a {
			return false, errors.New("logical action provenance conflict")
		}
		return false, nil
	}
	if l.actions == nil {
		l.actions = make(map[actionIdentity]LogicalAction)
	}
	if l.rollups == nil {
		l.rollups = make(map[RollupKey]Rollup)
	}
	l.actions[k] = a
	l.aggregate(a)
	return true, nil
}

func (l *LogicalLedger) aggregate(a LogicalAction) {
	for _, bucket := range []struct {
		name string
		size time.Duration
	}{{"hour", time.Hour}, {"day", 24 * time.Hour}} {
		k := RollupKey{a.AccountID, a.Actor, a.Dimension, bucket.name, a.OccurredAt.Truncate(bucket.size)}
		r := l.rollups[k]
		r.Count++
		r.Quantity += a.Quantity
		l.rollups[k] = r
	}
}

// RecordReplay records actual node work using its own event identity. It never
// inserts an original action or changes an hour/day aggregate.
func (l *LogicalLedger) RecordReplay(account, node, event string) error {
	if !validLabel(account) || !validLabel(node) || !validLabel(event) {
		return errors.New("invalid replay event")
	}
	if l.replay == nil {
		l.replay = make(map[evidenceIdentity]bool)
	}
	l.replay[evidenceIdentity{account, node, event}] = true
	return nil
}

// Rebuild replaces derived state from original provenance without adding events.
func (l *LogicalLedger) Rebuild() {
	l.rollups = make(map[RollupKey]Rollup)
	for _, a := range l.actions {
		l.aggregate(a)
	}
}

// Counts reads the account's vector without producing a new action.
func (l *LogicalLedger) Counts(account string) AccountingCounts {
	var c AccountingCounts
	for k := range l.actions {
		if k.account == account {
			c.OriginalEvents++
		}
	}
	for k, r := range l.rollups {
		if k.AccountID == account {
			if k.Bucket == "hour" {
				c.HourCount += r.Count
			} else {
				c.DayCount += r.Count
			}
		}
	}
	for k := range l.replay {
		if k.account == account {
			c.ReplayWork++
		}
	}
	return c
}

// Aggregate returns one derived bucket; reading it is a passive operation.
func (l *LogicalLedger) Aggregate(k RollupKey) Rollup { return l.rollups[k] }
