package localcell

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

const serviceForegroundFallback = "localcellharness run"

// RenderSystemdUserUnit renders an explicitly selected per-user host. Systemd,
// rather than a shell, parses these arguments; percent and dollar expansion are
// disabled by escaping each argument. The supervisor owns child restarts.
func RenderSystemdUserUnit(d ServiceDescriptor) ([]byte, error) {
	if err := validateSystemdSpec(d); err != nil {
		return nil, err
	}
	args := append([]string{d.Payload.Host}, d.Payload.Args...)
	quoted := make([]string, len(args))
	for i, arg := range args {
		if strings.IndexFunc(arg, unicode.IsControl) >= 0 {
			return nil, serviceSystemdError(ErrServiceOwnershipConflict, "render:argument", "control_character")
		}
		arg = strings.ReplaceAll(arg, "\\", "\\\\")
		arg = strings.ReplaceAll(arg, "\"", "\\\"")
		arg = strings.ReplaceAll(arg, "%", "%%")
		arg = strings.ReplaceAll(arg, "$", "$$")
		quoted[i] = "\"" + arg + "\""
	}
	return []byte("[Unit]\nDescription=Witself local cell " + serviceUnitLabel(d) + "\n\n[Service]\nType=simple\nEnvironment=WITSELF_RUNTIME=local\nExecStart=" + strings.Join(quoted, " ") + "\nRestart=no\nKillMode=mixed\nTimeoutStopSec=90s\n\n[Install]\nWantedBy=default.target\n"), nil
}

func serviceUnitLabel(d ServiceDescriptor) string {
	return "witself-cell-" + d.InstallationID + ".service"
}

func validateSystemdSpec(d ServiceDescriptor) error {
	uid, err := strconv.ParseUint(d.UID, 10, 32)
	for _, check := range []struct {
		step string
		ok   bool
	}{
		{"uid", err == nil && uid != 0 && strconv.FormatUint(uid, 10) == d.UID},
		{"runtime", d.Runtime == "local"},
		{"scope", d.Scope == "user"},
		{"adapter", d.Adapter == "systemd-user"},
		{"root", filepath.IsAbs(d.Root) && filepath.Clean(d.Root) == d.Root},
		{"host", filepath.IsAbs(d.Payload.Host) && filepath.Clean(d.Payload.Host) == d.Payload.Host},
		{"generation", d.Payload.Generation != ""},
		{"installation_id", d.InstallationID != ""},
	} {
		if !check.ok {
			return serviceSystemdError(ErrServiceOwnershipConflict, "spec:"+check.step, "invalid")
		}
	}
	for _, r := range d.InstallationID {
		allowed := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
		if !allowed {
			return serviceSystemdError(ErrServiceOwnershipConflict, "spec:installation_id", "invalid_character")
		}
	}
	if len(d.InstallationID) > 128 {
		return serviceSystemdError(ErrServiceOwnershipConflict, "spec:installation_id", "too_long")
	}
	return nil
}

type serviceManagerCommand func(context.Context, string, ...string) ([]byte, error)

func serviceUnavailable(err error) (ServiceObservation, error) {
	return ServiceObservation{Reason: "service_manager_unavailable", Fallback: serviceForegroundFallback}, err
}

type unavailableServiceAdapter struct{}

func (unavailableServiceAdapter) Inspect(context.Context, ServiceIdentity) (ServiceObservation, error) {
	return serviceUnavailable(serviceSystemdError(ErrServiceManagerUnavailable, "adapter:platform", "unsupported"))
}
func (a unavailableServiceAdapter) Plan(ctx context.Context, d ServiceSpec) (ServicePlan, error) {
	observation, err := a.Inspect(ctx, d)
	return ServicePlan{Spec: d, Observation: observation, RequiredPrivilege: "user"}, err
}
func (unavailableServiceAdapter) Apply(context.Context, ServicePlan) error {
	return serviceSystemdError(ErrServiceManagerUnavailable, "adapter:platform", "unsupported")
}
func (unavailableServiceAdapter) Remove(context.Context, ServiceIdentity) error {
	return serviceSystemdError(ErrServiceManagerUnavailable, "adapter:platform", "unsupported")
}
