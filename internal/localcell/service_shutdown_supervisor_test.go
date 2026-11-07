package localcell

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

func TestServiceSupervisorWindowsStopRefusalLatch(t *testing.T) {
	for _, kind := range []string{"unknown", "refused", "caller-canceled", "forced", "contention", "unix-unchanged"} {
		if kind == "unix-unchanged" && runtime.GOOS == "windows" {
			continue
		}
		t.Run(kind, func(t *testing.T) {
			c, _ := unitCell(t)
			d := prepareSupervisorTest(t, c)
			h, _, starts, stops, _ := supervisorTestHooks()
			h.windowsShutdownPolicy = kind != "unix-unchanged"
			s := newSupervisor(c, SupervisorPolicy{hooks: &h})
			if s.load(d) != nil {
				t.Fatal("cannot load stop fault fixture")
			}
			s.running = true
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s.hooks.stop = func(context.Context) error {
				*stops++
				switch kind {
				case "forced":
					return ErrServiceShutdownForced
				case "contention":
					return ErrLocked
				case "caller-canceled":
					cancel()
					return ctx.Err()
				default:
					return errors.New("synthetic unknown stop")
				}
			}
			if _, err := s.command(ctx, "stop"); err == nil {
				t.Fatal("failed stop unexpectedly succeeded")
			}
			want := kind == "unknown" || kind == "refused" || kind == "caller-canceled"
			after, err := c.ServiceDescriptor()
			if err != nil || s.shutdownBlocked != want || after.Faulted != want {
				t.Fatal("unsafe stop did not establish correct durable fault fence")
			}
			if want {
				if s.step(t.Context()) != nil {
					t.Fatal("faulted supervisor could not remain observable")
				}
				if *stops != 1 || *starts != 0 {
					t.Fatal("faulted tick retried shutdown or replaced child")
				}
				if _, err := s.command(t.Context(), "stop"); err == nil || *stops != 1 {
					t.Fatal("ordinary stop reopened faulted tree without repair")
				}
			}
		})
	}
}
