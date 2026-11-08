package localcell

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"
)

func TestServiceShutdownLostACKPolicy(t *testing.T) {
	// Measure the fake child's scheduling interval, then derive its application
	// deadline proportionally. These cases assert ordering, not wall latency.
	started := time.Now()
	timer := time.NewTimer(time.Millisecond)
	<-timer.C
	appBudget := 32 * time.Since(started)
	for _, tc := range []struct {
		name     string
		kind     serviceStopKind
		detail   string
		requests int
		forced   bool
		pg       bool
	}{
		{"lost_then_accepted_observed_exit", serviceStopGraceful, "", 2, false, true},
		{"lost_then_observed_exit", serviceStopAlreadyExited, "", 1, false, true},
		{"lost_then_accepted_forced_exit", serviceStopForced, "service_shutdown_forced", 2, true, true},
		{"all_lost_refuses_drain", serviceStopUnknown, "shutdown:ack", 2, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := shutdownDescriptorFixture(t)
			d.Component = "worker"
			id, err := shutdownRandom(16)
			if err != nil {
				t.Fatal("fixture request generation failed")
			}
			request := d.request("cancel", id)
			record := ProcessRecord{Component: d.Component, PID: d.PID, StartTime: d.StartIdentity,
				BootID: d.BootID, InstallationID: d.InstallationID, Generation: d.Generation}
			var requests []serviceShutdownRequest
			var observations []ProcessRecord
			var order []string
			exited, forced, pg := false, false, false
			process := serviceShutdownProcess{
				verify: func(candidate ProcessRecord) (bool, error) {
					observations = append(observations, candidate)
					if candidate != record {
						t.Fatal("retry changed the retained process incarnation")
					}
					return !exited, nil
				},
				send: func(_ context.Context, got serviceShutdownRequest) (serviceShutdownResponse, error) {
					requests = append(requests, got)
					if got != request || !d.accepts(got) {
						t.Fatal("retry changed the authenticated request")
					}
					if len(requests) == 1 || tc.name == "all_lost_refuses_drain" {
						return serviceShutdownResponse{}, errShutdownAckLost
					}
					return serviceShutdownResponse{RequestID: got.RequestID, Nonce: got.Nonce, Accepted: true, Phase: shutdownPhaseReady}, nil
				},
				wait: func(ctx context.Context, _ time.Duration) bool {
					if tc.name == "lost_then_observed_exit" || forced || (len(requests) == 2 && tc.kind == serviceStopGraceful) {
						exited = true
						order = append(order, "observed_exit")
						return true
					}
					if len(requests) == 1 {
						return false
					}
					<-ctx.Done()
					return false
				},
				force: func() error {
					if len(requests) != 2 || tc.kind != serviceStopForced {
						t.Fatal("lost ACK authorized force")
					}
					forced = true
					order = append(order, "force")
					return nil
				},
			}
			var outcome serviceStopOutcome
			drainErr := drainServiceTree(t.Context(), map[string]bool{"server": true},
				func(ctx context.Context, component string) serviceStopOutcome {
					if component != record.Component {
						t.Fatal("drain selected another incarnation")
					}
					outcome = runServiceShutdownPolicy(ctx, record, request, true, appBudget, process)
					return outcome
				}, func(context.Context) error {
					pg = true
					order = append(order, "postgres")
					return nil
				})
			want := serviceStopOutcome{Kind: tc.kind, Component: record.Component, Detail: tc.detail}
			if outcome != want {
				t.Fatal("ACK-loss policy returned the wrong exact outcome/detail")
			}
			if len(requests) != tc.requests || len(observations) < tc.requests {
				t.Fatal("ACK-loss policy did not retry and observe the retained incarnation")
			}
			if forced != tc.forced || pg != tc.pg {
				t.Fatal("ACK-loss policy authorized the wrong force/PostgreSQL drain")
			}
			var wantOrder []string
			if tc.forced {
				wantOrder = append(wantOrder, "force")
			}
			if tc.pg {
				wantOrder = append(wantOrder, "observed_exit", "postgres")
			}
			if !reflect.DeepEqual(order, wantOrder) {
				t.Fatal("PostgreSQL stopped before the retained child exit was observed")
			}
			switch tc.kind {
			case serviceStopUnknown:
				if !errors.Is(drainErr, errLocalShutdownRefused) || drainErr.Error() != "localcell: service_shutdown_refused: shutdown:ack" {
					t.Fatal("all lost ACKs did not retain the typed refusal and exact detail")
				}
			case serviceStopForced:
				if !errors.Is(drainErr, ErrServiceShutdownForced) {
					t.Fatal("accepted forced exit became graceful")
				}
			default:
				if drainErr != nil {
					t.Fatal("observed exit did not permit ordinary drain")
				}
			}
		})
	}
}

func TestServiceStopOutcomeRefusalIdentity(t *testing.T) {
	for _, kind := range []serviceStopKind{serviceStopRefused, serviceStopUnknown} {
		err := (serviceStopOutcome{Kind: kind, Detail: "shutdown:ack"}).err()
		if !errors.Is(err, errLocalShutdownRefused) {
			t.Fatal("stop outcome lost the typed shutdown refusal sentinel")
		}
		var refusal *localShutdownRefusal
		if !errors.As(err, &refusal) || refusal.step != "shutdown:ack" {
			t.Fatal("stop outcome lost the typed shutdown refusal detail")
		}
		if err.Error() != "localcell: service_shutdown_refused: shutdown:ack" {
			t.Fatal("typed wrapping changed the value-free refusal text")
		}
	}
}

func TestServiceShutdownPolicyLostAckBudget(t *testing.T) {
	for _, tc := range []struct {
		name              string
		lostBudgetPercent int
		exitAfter         time.Duration
		kind              serviceStopKind
		detail            string
		requests          int
		elapsed           time.Duration
		forced            bool
	}{
		{"budget_covers_lost_and_accepted_send", 40, 80 * time.Millisecond, serviceStopGraceful, "", 2, 80 * time.Millisecond, false},
		{"exit_before_second_send", 40, 50 * time.Millisecond, serviceStopAlreadyExited, "", 1, 50 * time.Millisecond, false},
		{"lost_send_consumes_budget", 100, 110 * time.Millisecond, serviceStopUnknown, "shutdown:ack", 1, 100 * time.Millisecond, false},
		{"accepted_send_exit_after_budget", 40, 110 * time.Millisecond, serviceStopForced, "service_shutdown_forced", 2, 100 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const appBudget = 100 * time.Millisecond
				const acceptedSendCost = 5 * time.Millisecond
				lostSendCost := appBudget * time.Duration(tc.lostBudgetPercent) / 100
				started := time.Now()
				ctx, cancel := context.WithTimeout(t.Context(), 2*appBudget)
				defer cancel()
				record := ProcessRecord{Component: "server", PID: 42, StartTime: "fixture-incarnation"}
				request := serviceShutdownRequest{Action: "cancel", Component: record.Component, PID: record.PID, StartIdentity: record.StartTime}
				requests, forced := 0, false
				exited := func() bool { return forced || time.Since(started) >= tc.exitAfter }
				process := serviceShutdownProcess{
					verify: func(candidate ProcessRecord) (bool, error) {
						if candidate != record {
							t.Fatal("lost-ACK budget proof changed the retained incarnation")
						}
						return !exited(), nil
					},
					send: func(_ context.Context, got serviceShutdownRequest) (serviceShutdownResponse, error) {
						if got != request {
							t.Fatal("lost-ACK budget proof changed the retained request")
						}
						requests++
						if requests == 1 {
							// synctest advances fake time, including the real policy's
							// context timer, without spending this interval on the host.
							time.Sleep(lostSendCost)
							synctest.Wait()
							return serviceShutdownResponse{}, errShutdownAckLost
						}
						time.Sleep(acceptedSendCost)
						synctest.Wait()
						return serviceShutdownResponse{Accepted: true, Phase: shutdownPhaseReady}, nil
					},
					wait: func(waitCtx context.Context, interval time.Duration) bool {
						if exited() {
							return true
						}
						if waitCtx.Err() != nil {
							return false
						}
						until := time.Now().Add(interval)
						if exitAt := started.Add(tc.exitAfter); exitAt.Before(until) {
							until = exitAt
						}
						if deadline, ok := waitCtx.Deadline(); ok && deadline.Before(until) {
							until = deadline
						}
						time.Sleep(time.Until(until))
						synctest.Wait()
						return exited()
					},
					force: func() error {
						forced = true
						return nil
					},
				}
				outcome := runServiceShutdownPolicy(ctx, record, request, true, appBudget, process)
				want := serviceStopOutcome{Kind: tc.kind, Component: record.Component, Detail: tc.detail}
				if outcome != want {
					t.Fatalf("lost-ACK budget outcome kind=%s detail=%s; want kind=%s detail=%s", outcome.Kind, outcome.Detail, tc.kind, tc.detail)
				}
				if requests != tc.requests || forced != tc.forced {
					t.Fatal("lost-ACK budget proof took the wrong resend or force path")
				}
				if time.Since(started) != tc.elapsed || ctx.Err() != nil {
					t.Fatal("lost-ACK budget proof used the wrong elapsed time or outer deadline")
				}
			})
		})
	}
}
