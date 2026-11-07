package localcell

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestServiceStopOutcome(t *testing.T) {
	for _, kind := range []serviceStopKind{serviceStopGraceful, serviceStopAlreadyExited, serviceStopForced, serviceStopRefused, serviceStopUnknown, "invalid"} {
		t.Run(string(kind), func(t *testing.T) {
			o := serviceStopOutcome{Kind: kind, Component: "worker", Detail: "private-value"}
			want := kind == serviceStopGraceful || kind == serviceStopAlreadyExited || kind == serviceStopForced
			if o.permitsDrain() != want {
				t.Fatal("stop disposition granted incorrect drain authority")
			}
			if err := o.err(); err != nil && strings.Contains(err.Error(), "private-value") {
				t.Fatal("stop disposition leaked raw detail")
			}
			if serviceStopPermitsHostExit(o.err()) != want {
				t.Fatal("host exit disagreed with tree disposition")
			}
		})
	}
	if serviceStopPermitsHostExit(errors.Join(ErrServiceShutdownForced, errors.New("postgres refused"))) {
		t.Fatal("forced app exit masked a failed remaining tree")
	}
}

func TestServiceShutdownDrainOrder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   serviceStopKind
		want   []string
		forced bool
	}{
		{"graceful", serviceStopGraceful, []string{"worker", "server", "postgres"}, false},
		{"already_exited", serviceStopAlreadyExited, []string{"worker", "server", "postgres"}, false},
		{"forced_then_postgres", serviceStopForced, []string{"worker", "server", "postgres"}, true},
		{"unknown_blocks_postgres", serviceStopUnknown, []string{"worker"}, false},
		{"refused_blocks_postgres", serviceStopRefused, []string{"worker"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var order []string
			err := drainServiceTree(t.Context(), nil, func(_ context.Context, role string) serviceStopOutcome {
				order = append(order, role)
				return serviceStopOutcome{Kind: tc.kind, Component: role, Detail: "shutdown:ack"}
			}, func(context.Context) error { order = append(order, "postgres"); return nil })
			if !reflect.DeepEqual(order, tc.want) {
				t.Fatal("tree drain did not respect verified app exit order")
			}
			if (err == ErrServiceShutdownForced) != tc.forced {
				t.Fatal("forced shutdown was classified as graceful")
			}
			if (tc.kind == serviceStopRefused || tc.kind == serviceStopUnknown) && err == nil {
				t.Fatal("unsafe shutdown returned success")
			}
		})
	}
}

func TestServiceShutdownWholeTreeDeadline(t *testing.T) {
	const budget = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), budget)
	defer cancel()
	deadline, _ := ctx.Deadline()
	apps, pg := 0, false
	err := drainServiceTree(ctx, nil, func(child context.Context, role string) serviceStopOutcome {
		apps++
		got, ok := child.Deadline()
		if !ok || got.After(deadline) {
			t.Fatal("per-app drain extended the whole-tree deadline")
		}
		<-child.Done()
		return serviceStopOutcome{Kind: serviceStopGraceful, Component: role}
	}, func(context.Context) error { pg = true; return nil })
	if apps != 1 || pg || err == nil || !strings.Contains(err.Error(), "shutdown:deadline") {
		t.Fatal("expired drain budget started another component")
	}
}

func TestServiceShutdownNamedBudget(t *testing.T) {
	if ServiceShutdownTimeout != 2*serviceAppShutdownTimeout+servicePostgresShutdownTimeout+15*time.Second {
		t.Fatal("whole-tree shutdown budget no longer covers declared phases")
	}
	var deadline time.Time
	err := drainServiceTree(t.Context(), map[string]bool{"postgres": true}, func(ctx context.Context, role string) serviceStopOutcome {
		got, ok := ctx.Deadline()
		if !ok || time.Until(got) > ServiceShutdownTimeout {
			t.Fatal("tree drain has no named total deadline")
		}
		if !deadline.IsZero() && !got.Equal(deadline) {
			t.Fatal("app waits stacked independent total budgets")
		}
		deadline = got
		return serviceStopOutcome{Kind: serviceStopGraceful, Component: role}
	}, func(context.Context) error { t.Fatal("kept PostgreSQL was stopped"); return nil })
	if err != nil {
		t.Fatal("verified kept tree did not drain")
	}
}

func TestServiceShutdownPostgresBudget(t *testing.T) {
	called := false
	err := drainServiceTree(t.Context(), map[string]bool{"server": true, "worker": true},
		func(context.Context, string) serviceStopOutcome {
			t.Fatal("kept app was stopped")
			return serviceStopOutcome{}
		},
		func(ctx context.Context) error {
			called = true
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > servicePostgresShutdownTimeout {
				t.Fatal("PostgreSQL stop exceeded its reserved tree budget")
			}
			return nil
		})
	if err != nil || !called {
		t.Fatal("reserved PostgreSQL stop did not execute")
	}
}
