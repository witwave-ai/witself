package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

const serviceForegroundFallback = "localcellharness run"

// Resolve the same config search path as a user manager. HOME is optional: a
// stripped child environment can still resolve its passwd home. An explicit XDG
// config directory must be absolute; never silently register in another place.
func systemdUserDirectories(userHome, passwdHome func() (string, error), configHome string) (home, base, unitDir string) {
	home, err := userHome()
	if err != nil || !filepath.IsAbs(home) {
		home, err = passwdHome()
	}
	if err != nil || !filepath.IsAbs(home) {
		return "", "", ""
	}
	home = filepath.Clean(home)
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	if !filepath.IsAbs(configHome) {
		return home, "", ""
	}
	configHome = filepath.Clean(configHome)
	unitDir = filepath.Join(configHome, "systemd", "user")
	rel, err := filepath.Rel(home, configHome)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return home, home, unitDir
	}
	// For a separate XDG config tree, validate from its nearest existing parent.
	// capability will refuse an unsafe, unowned or unwritable anchor.
	base = configHome
	for {
		if _, err := os.Lstat(base); err == nil {
			resolved, err := filepath.EvalSymlinks(base)
			if err != nil || resolved != base {
				return home, "", ""
			}
			return home, base, unitDir
		} else if !errors.Is(err, os.ErrNotExist) {
			return home, "", ""
		}
		parent := filepath.Dir(base)
		if parent == base {
			return home, "", ""
		}
		base = parent
	}
}

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
			return nil, ErrServiceOwnershipConflict
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
	if err != nil || uid == 0 || strconv.FormatUint(uid, 10) != d.UID || d.Runtime != "local" || d.Scope != "user" || d.Adapter != "systemd-user" || !filepath.IsAbs(d.Root) || filepath.Clean(d.Root) != d.Root || !filepath.IsAbs(d.Payload.Host) || filepath.Clean(d.Payload.Host) != d.Payload.Host || d.Payload.Generation == "" || d.InstallationID == "" {
		return ErrServiceOwnershipConflict
	}
	for _, r := range d.InstallationID {
		allowed := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
		if !allowed {
			return ErrServiceOwnershipConflict
		}
	}
	if len(d.InstallationID) > 128 {
		return ErrServiceOwnershipConflict
	}
	return nil
}

type serviceManagerCommand func(context.Context, string, ...string) ([]byte, error)

type systemdUserAdapter struct {
	uid, home, runtimeDir, unitDir, systemctl, busctl string
	unitBase                                          string
	command                                           serviceManagerCommand
	owned                                             func(string, os.FileInfo) bool
	accessible                                        func(string, bool) bool
	inspectFile                                       func(string) (os.FileInfo, error)
	writeUnit                                         func(*os.File, []byte) error
	unavailable                                       bool
}

func (a *systemdUserAdapter) lstat(path string) (os.FileInfo, error) {
	if a.inspectFile != nil {
		return a.inspectFile(path)
	}
	return os.Lstat(path)
}

func serviceUnavailable() (ServiceObservation, error) {
	return ServiceObservation{Reason: "service_manager_unavailable", Fallback: serviceForegroundFallback}, ErrServiceManagerUnavailable
}

func (a *systemdUserAdapter) capability(ctx context.Context) error {
	// Probe the current non-root identity, absolute tools, private owned session
	// directory, resolvable safe unit path, search/write access to its existing
	// directories (or nearest existing parent), and bus reachability through both
	// tools. Plan creates nothing and does not probe daemon-reload/enable permission;
	// failures of those operations are registration failures during Apply.
	if a.unavailable || a.uid == "" || a.uid == "0" || a.command == nil || a.systemctl == "" || a.busctl == "" || !filepath.IsAbs(a.runtimeDir) || !filepath.IsAbs(a.home) || !filepath.IsAbs(a.unitDir) {
		return ErrServiceManagerUnavailable
	}
	info, err := a.lstat(a.runtimeDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || a.owned == nil || !a.owned(a.runtimeDir, info) {
		return ErrServiceManagerUnavailable
	}
	if err := a.inspectDirectories(false); err != nil {
		return err
	}
	if _, err := a.command(ctx, a.systemctl, "--user", "show", "--property=Version", "--value"); err != nil {
		return ErrServiceManagerUnavailable
	}
	if _, err := a.command(ctx, a.busctl, "--user", "--json=short", "get-property", "org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", "Version"); err != nil {
		return ErrServiceManagerUnavailable
	}
	return nil
}

func (a *systemdUserAdapter) unitPath(d ServiceDescriptor) string {
	return filepath.Join(a.unitDir, serviceUnitLabel(d))
}

// inspectDirectories never changes existing permissions or follows a unit-directory
// symlink. Existing non-private config directories are accepted only if owned and
// not group/world writable; the unit itself must be exactly owner-private.
func (a *systemdUserAdapter) inspectDirectories(create bool) error {
	base := a.unitBase
	if base == "" {
		base = a.home
	}
	if !filepath.IsAbs(base) || !filepath.IsAbs(a.unitDir) || filepath.Clean(base) != base || filepath.Clean(a.unitDir) != a.unitDir {
		return ErrServiceManagerUnavailable
	}
	rel, err := filepath.Rel(base, a.unitDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ErrServiceOwnershipConflict
	}
	path := base
	parts := append([]string{""}, strings.Split(rel, string(filepath.Separator))...)
	for i, part := range parts {
		parent := path
		path = filepath.Join(path, part)
		info, err := a.lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			if i == 0 || !a.directoryAccessible(parent, true) {
				return ErrServiceManagerUnavailable
			}
			if !create {
				return nil
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				return ErrServiceManagerUnavailable
			}
			info, err = a.lstat(path)
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || a.owned == nil || !a.owned(path, info) {
			return ErrServiceOwnershipConflict
		}
		if !a.directoryAccessible(path, i == len(parts)-1) {
			return ErrServiceManagerUnavailable
		}
	}
	return nil
}

func (a *systemdUserAdapter) directoryAccessible(path string, write bool) bool {
	if a.accessible != nil {
		return a.accessible(path, write)
	}
	// Portable fixtures model Unix owner permissions. The Linux adapter also
	// checks effective filesystem access, including ACL/read-only mount refusal.
	info, err := a.lstat(path)
	mode := os.FileMode(0o100)
	if write {
		mode |= 0o200
	}
	return err == nil && info.Mode().Perm()&mode == mode
}

func (a *systemdUserAdapter) ownedUnit(d ServiceDescriptor, want []byte) (bool, error) {
	if err := a.inspectDirectories(false); err != nil {
		return false, err
	}
	if err := a.inspectEnablement(d); err != nil {
		return false, err
	}
	path := a.unitPath(d)
	info, err := a.lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || a.owned == nil || !a.owned(path, info) {
		return false, ErrServiceOwnershipConflict
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, want) {
		return false, ErrServiceOwnershipConflict
	}
	return true, nil
}

func (a *systemdUserAdapter) inspectEnablement(d ServiceDescriptor) error {
	dir := filepath.Join(a.unitDir, "default.target.wants")
	info, err := a.lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || !a.owned(dir, info) {
		return ErrServiceOwnershipConflict
	}
	link := filepath.Join(dir, serviceUnitLabel(d))
	info, err = a.lstat(link)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink == 0 || !a.owned(link, info) {
		return ErrServiceOwnershipConflict
	}
	target, err := os.Readlink(link)
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	if err != nil || filepath.Clean(target) != a.unitPath(d) {
		return ErrServiceOwnershipConflict
	}
	return nil
}

func (a *systemdUserAdapter) Inspect(ctx context.Context, d ServiceIdentity) (ServiceObservation, error) {
	want, err := RenderSystemdUserUnit(d)
	// uid is captured from os.Getuid by the native constructor. A user manager's
	// optional User property is not an ownership boundary or identity authority.
	if err != nil || d.UID != a.uid {
		return ServiceObservation{Reason: "service_ownership_conflict"}, ErrServiceOwnershipConflict
	}
	if err := a.capability(ctx); err != nil {
		if errors.Is(err, ErrServiceOwnershipConflict) {
			return ServiceObservation{Reason: "service_ownership_conflict"}, err
		}
		return serviceUnavailable()
	}
	exists, err := a.ownedUnit(d, want)
	if err != nil {
		if errors.Is(err, ErrServiceManagerUnavailable) {
			return serviceUnavailable()
		}
		return ServiceObservation{Available: true, Reason: "service_ownership_conflict"}, err
	}
	// Scalar show properties have no argument-array ambiguity. Reject drop-ins:
	// otherwise matching the main file would not establish the loaded contract.
	raw, err := a.command(ctx, a.systemctl, "--user", "show", serviceUnitLabel(d), "--property=Id,LoadState,FragmentPath,DropInPaths", "--no-pager")
	if err != nil {
		return serviceUnavailable()
	}
	properties, err := systemdProperties(raw)
	if err != nil {
		return ServiceObservation{Available: true, Reason: "service_ownership_conflict"}, err
	}
	if properties["LoadState"] == "not-found" {
		return ServiceObservation{Available: true, Registered: exists, Owned: exists}, nil
	}
	if !exists || properties["Id"] != serviceUnitLabel(d) || properties["FragmentPath"] != a.unitPath(d) || properties["DropInPaths"] != "" {
		return ServiceObservation{Available: true, Registered: true, Reason: "service_ownership_conflict"}, ErrServiceOwnershipConflict
	}
	if properties["LoadState"] != "loaded" {
		return ServiceObservation{Available: true, Registered: true, Reason: "service_registration_failed"}, errors.New("service_registration_failed")
	}
	if err := a.inspectExecStart(ctx, d); err != nil {
		if errors.Is(err, ErrServiceManagerUnavailable) {
			return serviceUnavailable()
		}
		return ServiceObservation{Available: true, Registered: true, Reason: "service_ownership_conflict"}, err
	}
	return ServiceObservation{Available: true, Registered: true, Owned: true}, nil
}

func systemdProperties(raw []byte) (map[string]string, error) {
	props := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			return nil, ErrServiceOwnershipConflict
		}
		if _, seen := props[key]; seen {
			return nil, ErrServiceOwnershipConflict
		}
		props[key] = value
	}
	for _, key := range []string{"Id", "LoadState", "FragmentPath", "DropInPaths"} {
		if _, ok := props[key]; !ok {
			return nil, ErrServiceOwnershipConflict
		}
	}
	return props, nil
}

type systemdBusResult struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func (a *systemdUserAdapter) inspectExecStart(ctx context.Context, d ServiceDescriptor) error {
	raw, err := a.command(ctx, a.busctl, "--user", "--json=short", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", "GetUnit", "s", serviceUnitLabel(d))
	if err != nil {
		return ErrServiceManagerUnavailable
	}
	var result systemdBusResult
	var object []string
	if json.Unmarshal(raw, &result) != nil || result.Type != "o" {
		return ErrServiceOwnershipConflict
	}
	if json.Unmarshal(result.Data, &object) != nil {
		var path string
		if json.Unmarshal(result.Data, &path) != nil {
			return ErrServiceOwnershipConflict
		}
		object = []string{path}
	}
	if len(object) != 1 || !strings.HasPrefix(object[0], "/org/freedesktop/systemd1/unit/") {
		return ErrServiceOwnershipConflict
	}
	raw, err = a.command(ctx, a.busctl, "--user", "--json=short", "get-property", "org.freedesktop.systemd1", object[0], "org.freedesktop.systemd1.Service", "ExecStart")
	if err != nil {
		return ErrServiceManagerUnavailable
	}
	var records [][]json.RawMessage
	if json.Unmarshal(raw, &result) != nil || result.Type != "a(sasbttttuii)" || json.Unmarshal(result.Data, &records) != nil {
		return ErrServiceOwnershipConflict
	}
	// busctl's message serialization may wrap the single property value in
	// the message's argument array. Accept either structural representation;
	// both must contain exactly one executable with exactly the same argv.
	if len(records) == 1 && len(records[0]) == 1 {
		var envelope []json.RawMessage
		if json.Unmarshal(result.Data, &envelope) != nil || len(envelope) != 1 || json.Unmarshal(envelope[0], &records) != nil {
			return ErrServiceOwnershipConflict
		}
	}
	if len(records) != 1 || len(records[0]) != 10 {
		return ErrServiceOwnershipConflict
	}
	var path string
	var argv []string
	var ignoreFailure bool
	if json.Unmarshal(records[0][0], &path) != nil || json.Unmarshal(records[0][1], &argv) != nil || json.Unmarshal(records[0][2], &ignoreFailure) != nil || ignoreFailure || path != d.Payload.Host {
		return ErrServiceOwnershipConflict
	}
	want := append([]string{d.Payload.Host}, d.Payload.Args...)
	if len(argv) != len(want) {
		return ErrServiceOwnershipConflict
	}
	for i := range argv {
		if argv[i] != want[i] {
			return ErrServiceOwnershipConflict
		}
	}
	return nil
}

func (a *systemdUserAdapter) Plan(ctx context.Context, d ServiceSpec) (ServicePlan, error) {
	observation, err := a.Inspect(ctx, d)
	return ServicePlan{Spec: d, Observation: observation, RequiredPrivilege: "user"}, err
}

func (a *systemdUserAdapter) Apply(ctx context.Context, plan ServicePlan) error {
	if plan.RequiredPrivilege != "user" {
		return ErrServiceOwnershipConflict
	}
	if _, err := a.Inspect(ctx, plan.Spec); err != nil {
		return err
	}
	d := plan.Spec
	want, err := RenderSystemdUserUnit(d)
	if err != nil {
		return err
	}
	if err := a.inspectDirectories(true); err != nil {
		return err
	}
	exists, err := a.ownedUnit(d, want)
	if err != nil {
		return err
	}
	created := false
	if !exists {
		// Exclusive creation cannot overwrite a registration that appeared after Plan.
		file, err := os.OpenFile(a.unitPath(d), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return ErrServiceOwnershipConflict
		}
		identity, statErr := file.Stat()
		persist := a.writeUnit
		if persist == nil {
			persist = func(f *os.File, data []byte) error {
				if _, err := f.Write(data); err != nil {
					return err
				}
				return f.Sync()
			}
		}
		writeErr := statErr
		if writeErr == nil {
			writeErr = persist(file, want)
		}
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			a.removeCreatedUnit(d, identity)
			return errors.New("service_registration_failed")
		}
		created = true
		if err := syncDirectory(a.unitDir); err != nil {
			a.removeCreatedUnit(d, identity)
			return err
		}
	}
	if _, err := a.command(ctx, a.systemctl, "--user", "daemon-reload"); err != nil {
		if created {
			a.cleanupPartial(ctx, d, want)
		}
		return errors.New("service_registration_failed")
	}
	// Loading may expose a conflicting registration/drop-in; never enable it.
	if observation, err := a.Inspect(ctx, d); err != nil || !observation.Owned {
		if created {
			a.cleanupPartial(ctx, d, want)
		}
		return serviceRegistrationError(err)
	}
	if _, err := a.command(ctx, a.systemctl, "--user", "enable", "--now", serviceUnitLabel(d)); err != nil {
		if created {
			a.cleanupPartial(ctx, d, want)
		}
		return errors.New("service_registration_failed")
	}
	if observation, err := a.Inspect(ctx, d); err != nil || !observation.Owned {
		return serviceRegistrationError(err)
	}
	return nil
}

func serviceRegistrationError(err error) error {
	if errors.Is(err, ErrServiceOwnershipConflict) {
		return ErrServiceOwnershipConflict
	}
	return errors.New("service_registration_failed")
}

func (a *systemdUserAdapter) removeCreatedUnit(d ServiceDescriptor, identity os.FileInfo) {
	if identity == nil {
		return
	}
	info, err := os.Lstat(a.unitPath(d))
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(identity, info) {
		return
	}
	if os.Remove(a.unitPath(d)) == nil {
		_ = syncDirectory(a.unitDir)
	}
}

func (a *systemdUserAdapter) cleanupPartial(ctx context.Context, d ServiceDescriptor, want []byte) {
	// Cleanup never disables a foreign loaded service. A failed ownership check
	// leaves the partial registration for diagnostics rather than guessing.
	observation, err := a.Inspect(ctx, d)
	if err != nil || !observation.Owned {
		return
	}
	if _, err := a.command(ctx, a.systemctl, "--user", "disable", "--now", serviceUnitLabel(d)); err != nil {
		return
	}
	if exists, err := a.ownedUnit(d, want); err != nil || !exists {
		return
	}
	if os.Remove(a.unitPath(d)) == nil {
		_, _ = a.command(ctx, a.systemctl, "--user", "daemon-reload")
	}
}

func (a *systemdUserAdapter) Remove(ctx context.Context, d ServiceIdentity) error {
	observation, err := a.Inspect(ctx, d)
	if err != nil {
		return err
	}
	if !observation.Registered {
		return nil
	}
	if !observation.Owned {
		return ErrServiceOwnershipConflict
	}
	if _, err := a.command(ctx, a.systemctl, "--user", "disable", "--now", serviceUnitLabel(d)); err != nil {
		return errors.New("service_removal_failed")
	}
	want, err := RenderSystemdUserUnit(d)
	if err != nil {
		return err
	}
	if exists, err := a.ownedUnit(d, want); err != nil || !exists {
		return ErrServiceOwnershipConflict
	}
	if err := os.Remove(a.unitPath(d)); err != nil {
		return errors.New("service_removal_failed")
	}
	if err := syncDirectory(a.unitDir); err != nil {
		return err
	}
	if _, err := a.command(ctx, a.systemctl, "--user", "daemon-reload"); err != nil {
		return errors.New("service_removal_failed")
	}
	return nil
}

type unavailableServiceAdapter struct{}

func (unavailableServiceAdapter) Inspect(context.Context, ServiceIdentity) (ServiceObservation, error) {
	return serviceUnavailable()
}
func (a unavailableServiceAdapter) Plan(ctx context.Context, d ServiceSpec) (ServicePlan, error) {
	observation, err := a.Inspect(ctx, d)
	return ServicePlan{Spec: d, Observation: observation, RequiredPrivilege: "user"}, err
}
func (unavailableServiceAdapter) Apply(context.Context, ServicePlan) error {
	return fmt.Errorf("%w: %s", ErrServiceManagerUnavailable, serviceForegroundFallback)
}
func (unavailableServiceAdapter) Remove(context.Context, ServiceIdentity) error {
	return fmt.Errorf("%w: %s", ErrServiceManagerUnavailable, serviceForegroundFallback)
}
