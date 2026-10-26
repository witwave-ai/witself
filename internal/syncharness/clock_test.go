package syncharness

import (
	"errors"
	"testing"
	"time"
)

func TestClockDeadlinesAndRollback(t *testing.T) {
	for _, kind := range []DeadlineKind{GrantDeadline, LeaseDeadline, RetentionDeadline, ACKDeadline} {
		t.Run(string(kind), func(t *testing.T) {
			start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
			clock := NewClock(start)
			deadline, err := clock.NewDeadline(kind, 30*24*time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if err := clock.Advance(30*24*time.Hour - time.Nanosecond); err != nil {
				t.Fatal(err)
			}
			if expired, err := clock.Expired(deadline); expired || err != nil {
				t.Fatal("expired before boundary")
			}
			if err := clock.Advance(time.Nanosecond); err != nil {
				t.Fatal(err)
			}
			if expired, err := clock.Expired(deadline); !expired || err != nil {
				t.Fatal("deadline boundary ignored")
			}
			extended, err := clock.Extend(deadline, 7*24*time.Hour)
			if err != nil || !extended.At.Equal(start.Add(37*24*time.Hour)) {
				t.Fatal("retention extension boundary wrong")
			}
			if err := clock.Advance(-time.Hour); !errors.Is(err, ErrClockRollback) {
				t.Fatal("rollback not reported")
			}
			if !clock.Now().Equal(start.Add(30*24*time.Hour - time.Hour)) {
				t.Fatal("rollback not represented")
			}
			if got, err := clock.Extend(extended, time.Hour); !errors.Is(err, ErrClockRollback) || got != extended {
				t.Fatal("uncertain expiry extended deadline")
			}
			if _, err := clock.Expired(extended); !errors.Is(err, ErrClockRollback) {
				t.Fatal("uncertain expiry reported certainty")
			}
			if _, err := clock.NewDeadline(kind, time.Hour); !errors.Is(err, ErrClockRollback) {
				t.Fatal("rollback allowed new grant/deadline")
			}
			if err := clock.Advance(8 * 24 * time.Hour); err != nil {
				t.Fatal(err)
			}
			if !clock.Uncertain() {
				t.Fatal("catching up silently cleared uncertainty")
			}
		})
	}
}

func TestClockRejectsUnknownDeadlineAndNegativeExtension(t *testing.T) {
	clock := NewClock(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if _, err := clock.NewDeadline("unknown", time.Hour); !errors.Is(err, ErrDeadline) {
		t.Fatal("unknown kind accepted")
	}
	deadline, err := clock.NewDeadline(GrantDeadline, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := clock.Extend(deadline, -time.Second); !errors.Is(err, ErrDeadline) || got != deadline {
		t.Fatal("negative extension accepted")
	}
}
