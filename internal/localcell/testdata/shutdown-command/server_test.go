package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"github.com/witwave-ai/witself/internal/server"
)

func TestCommandShutdownProductionContext(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	parent, stop := context.WithCancel(t.Context())
	defer stop()
	var exact context.Context
	var fixture *localcell.CommandShutdownFixture
	var cancelCalls atomic.Int32
	installed := make(chan struct{})
	shutdownChannelFactory = func(ctx context.Context, cancel context.CancelFunc, _ runtimeconfig.LocalShutdown, role string) (localcell.LocalShutdownChannel, error) {
		if role != "server" {
			t.Error("command selected another role")
		}
		exact = ctx
		var err error
		fixture, err = localcell.NewCommandShutdownFixture(func() { cancelCalls.Add(1); cancel() }, role)
		close(installed)
		return fixture, err
	}
	defer func() { shutdownChannelFactory = nil }()
	started := time.Now()
	done := make(chan error, 1)
	go func() {
		ctx, channel, closeChannel, err := installCommandShutdown(parent, runtimeconfig.Config{
			Local: true, LocalShutdown: runtimeconfig.LocalShutdown{Transport: runtimeconfig.LocalShutdownNamedPipe, DescriptorPath: filepath.Join(t.TempDir(), "descriptor.json")},
		})
		if err != nil {
			done <- err
			return
		}
		defer closeChannel()
		if ctx != exact {
			t.Error("command replaced the channel cancellation context")
		}
		cfg := serverConfigWithShutdown(server.Config{APIAddr: "127.0.0.1:0", HealthAddr: "127.0.0.1:0", MetricsAddr: "127.0.0.1:0"}, channel)
		done <- server.Run(ctx, cfg)
	}()
	<-installed
	if fixture == nil {
		t.Fatal("protocol fixture did not install")
	}
	defer close(fixture.AllowClose)
	select {
	case <-fixture.Ready:
		t.Fatal("production runtime entered before release")
	default:
	}
	if response, err := fixture.Exchange("release"); err != nil || !response.Accepted {
		t.Fatal("release failed")
	}
	select {
	case <-fixture.Ready:
	case <-parent.Done():
		t.Fatal("production runtime did not enter")
	}
	// Behaviour watchdog scales with this fixture's actual protocol and runtime
	// startup; the outer 20-second test-process limit is only a hang guard.
	watchdog := 100 * time.Since(started)
	behavior, finish := context.WithTimeout(parent, watchdog)
	defer finish()
	deadline, _ := behavior.Deadline()
	fixture.Deadline(deadline)
	first, err := fixture.Exchange("cancel")
	if err != nil || !first.Accepted {
		t.Fatal("authenticated cancellation failed")
	}
	second, err := fixture.Exchange("cancel")
	if err != nil || first != second {
		t.Fatal("retried cancel did not return identical ACK")
	}
	// Exchange fences handler completion, including its deferred callback.
	if cancelCalls.Load() != 1 {
		t.Fatal("retry invoked cancellation more than once")
	}
	select {
	case err := <-done:
		if err != nil || exact.Err() != context.Canceled {
			t.Fatal("pipe request did not cancel the production server context")
		}
	case <-behavior.Done():
		t.Fatal("production server ignored the command cancellation context")
	}
}

func TestCommandShutdownHostedHasNoFactory(t *testing.T) {
	calls := 0
	shutdownChannelFactory = func(context.Context, context.CancelFunc, runtimeconfig.LocalShutdown, string) (localcell.LocalShutdownChannel, error) {
		calls++
		return nil, nil
	}
	defer func() { shutdownChannelFactory = nil }()
	ctx := t.Context()
	got, channel, cleanup, err := installCommandShutdown(ctx, runtimeconfig.Config{})
	defer cleanup()
	if got != ctx || channel != nil || err != nil || calls != 0 {
		t.Fatal("hosted command installed a listener or changed context")
	}
}

func TestCommandShutdownInvalidConfiguration(t *testing.T) {
	calls := 0
	shutdownChannelFactory = func(context.Context, context.CancelFunc, runtimeconfig.LocalShutdown, string) (localcell.LocalShutdownChannel, error) {
		calls++
		return nil, nil
	}
	defer func() { shutdownChannelFactory = nil }()
	cfg := runtimeconfig.Config{LocalShutdown: runtimeconfig.LocalShutdown{Transport: runtimeconfig.LocalShutdownNamedPipe, DescriptorPath: filepath.Join(t.TempDir(), "descriptor.json")}}
	for _, local := range []bool{false, true} {
		cfg.Local = local
		_, channel, cleanup, err := installCommandShutdown(t.Context(), cfg)
		cleanup()
		if channel != nil || !errors.Is(err, runtimeconfig.ErrLocalShutdownConfigInvalid) || err.Error() != "local_shutdown_config_invalid: shutdown:config" {
			t.Fatal("invalid command configuration lost its value-free raising site")
		}
		if (!local && calls != 0) || (local && calls != 1) {
			t.Fatal("command factory called before explicit local selection")
		}
	}
}
