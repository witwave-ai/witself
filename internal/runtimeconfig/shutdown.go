package runtimeconfig

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// LocalShutdownNamedPipe is the sole supported child shutdown transport.
const LocalShutdownNamedPipe = "named_pipe"

// ErrLocalShutdownConfigInvalid is intentionally value-free. Invalid selectors
// must not disclose the private descriptor location or any supplied value.
var ErrLocalShutdownConfigInvalid = errors.New("local_shutdown_config_invalid")

// LocalShutdown opts one local child into its incarnation-bound shutdown pipe.
// The zero value creates no listener and reads no descriptor.
type LocalShutdown struct {
	Transport      string
	DescriptorPath string
}

func localShutdownFromEnv(role, runtime string, lookup func(string) (string, bool), environ []string) (LocalShutdown, error) {
	const descriptorName = "WITSELF_LOCAL_SHUTDOWN_DESCRIPTOR"
	const transportName = "WITSELF_LOCAL_SHUTDOWN_TRANSPORT"
	descriptor, descriptorPresent := lookup(descriptorName)
	transport, transportPresent := lookup(transportName)
	invalid := func() (LocalShutdown, error) {
		return LocalShutdown{}, fmt.Errorf("%w: shutdown:config", ErrLocalShutdownConfigInvalid)
	}
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(name), "WITSELF_LOCAL_SHUTDOWN_") {
			continue
		}
		// Reject aliases even where the operating system's lookup is insensitive.
		if runtime != "local" || (name != descriptorName && name != transportName) {
			return invalid()
		}
	}
	if !descriptorPresent && !transportPresent {
		return LocalShutdown{}, nil
	}
	if runtime != "local" || (role != "server" && role != "worker") || !descriptorPresent {
		return invalid()
	}
	// The launch environment carries only the descriptor path. A caller may
	// explicitly assert the transport, but cannot select one without a descriptor.
	if transportPresent && transport != LocalShutdownNamedPipe {
		return invalid()
	}
	if descriptor == "" || descriptor != strings.TrimSpace(descriptor) ||
		strings.ContainsAny(descriptor, "\x00\r\n") || !filepath.IsAbs(descriptor) ||
		filepath.Clean(descriptor) != descriptor || filepath.Base(descriptor) == "." ||
		filepath.Base(descriptor) == string(filepath.Separator) || !localShutdownPathValid(descriptor) {
		return invalid()
	}
	return LocalShutdown{Transport: LocalShutdownNamedPipe, DescriptorPath: descriptor}, nil
}
