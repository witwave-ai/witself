package main

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

func TestServiceControlDeferralPolicy(t *testing.T) {
	deferred := errors.New("localcell: service_control_deferred")
	other := errors.New("localcell: service_control_deferred: different refusal")
	accepted := localcell.ServiceStatus{State: "running", Healthy: true}
	for _, row := range []struct {
		name, action string
		replies      []error
		wantErr      error
		elapsed      time.Duration
	}{
		{"start", "start", []error{deferred, deferred, nil}, nil, 20 * time.Millisecond},
		{"health", "health", []error{deferred, nil}, nil, 10 * time.Millisecond},
		{"stop_refusal", "stop", []error{deferred, other}, other, 10 * time.Millisecond},
		{"immediate_refusal", "stop", []error{localcell.ErrServiceControlRefused}, localcell.ErrServiceControlRefused, 0},
		{"observe", "observe", []error{deferred}, deferred, 0},
		{"status", "status", []error{deferred}, deferred, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				calls := 0
				got, err := controlServiceCommand(t.Context(), row.action, func() (localcell.ServiceStatus, error) {
					if t.Context().Err() != nil || calls >= len(row.replies) {
						t.Fatal("unexpected control attempt")
					}
					if time.Since(start) != time.Duration(calls)*10*time.Millisecond {
						t.Fatal("service action retried outside the deferral interval")
					}
					reply := row.replies[calls]
					calls++
					if reply != nil {
						return localcell.ServiceStatus{}, reply
					}
					return accepted, nil
				})
				if err != row.wantErr || calls != len(row.replies) || time.Since(start) != row.elapsed {
					t.Fatal("service action did not follow its deferral policy")
				}
				if err == nil && (got.State != accepted.State || got.Healthy != accepted.Healthy) {
					t.Fatal("service action lost the accepted reply")
				}
			})
		})
	}
}

func TestServiceControlDeadlineWhileDeferred(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Millisecond)
		defer cancel()
		start := time.Now()
		deferred := errors.New("localcell: service_control_deferred")
		calls := 0
		_, err := controlServiceCommand(ctx, "stop", func() (localcell.ServiceStatus, error) {
			calls++
			if calls > 2 {
				t.Fatal("service action retried past its deadline")
			}
			return localcell.ServiceStatus{}, deferred
		})
		if err != deferred || ctx.Err() != context.DeadlineExceeded || calls != 2 || time.Since(start) != 15*time.Millisecond {
			t.Fatal("deadline did not return the outstanding deferral")
		}
	})
}
