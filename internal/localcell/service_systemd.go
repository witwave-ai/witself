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

func serviceUnavailable(err error) (ServiceObservation, error) {
	return ServiceObservation{Reason: "service_manager_unavailable", Fallback: serviceForegroundFallback}, err
}

func (a *systemdUserAdapter) capability(ctx context.Context) error {
	return a.capabilityForRoot(ctx, "")
}

func (a *systemdUserAdapter) capabilityForRoot(ctx context.Context, root string) error {
	// Probe the current non-root identity, absolute tools, private owned session
	// directory, resolvable safe unit path, search/write access to its existing
	// directories (or nearest existing parent), and bus reachability through both
	// tools. Plan creates nothing and does not probe daemon-reload/enable permission;
	// failures of those operations are registration failures during Apply.
	for _, check := range []struct {
		step string
		ok   bool
	}{
		{"identity", !a.unavailable && a.uid != "" && a.uid != "0"},
		{"command_runner", a.command != nil},
		{"systemctl_path", filepath.IsAbs(a.systemctl)},
		{"busctl_path", filepath.IsAbs(a.busctl)},
		{"runtime_dir_path", filepath.IsAbs(a.runtimeDir)},
		{"home_path", filepath.IsAbs(a.home)},
		{"unit_dir_path", filepath.IsAbs(a.unitDir)},
	} {
		if !check.ok {
			return serviceSystemdError(ErrServiceManagerUnavailable, check.step, "unavailable")
		}
	}
	info, err := a.lstat(a.runtimeDir)
	if err != nil {
		return a.commandError(ErrServiceManagerUnavailable, "runtime_dir_stat", err, root)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return serviceSystemdError(ErrServiceManagerUnavailable, "runtime_dir_type", "not_directory_or_symlink")
	}
	if info.Mode().Perm() != 0o700 {
		return serviceSystemdError(ErrServiceManagerUnavailable, "runtime_dir_mode", fmt.Sprintf("%04o", info.Mode().Perm()))
	}
	if a.owned == nil || !a.owned(a.runtimeDir, info) {
		return serviceSystemdError(ErrServiceManagerUnavailable, "runtime_dir_owner", "not_current_uid")
	}
	if err := a.inspectDirectories(false); err != nil {
		return err
	}
	if _, err := a.command(ctx, a.systemctl, "--user", "show", "--property=Version", "--value"); err != nil {
		return a.commandError(ErrServiceManagerUnavailable, "bus:systemctl_show", err, root)
	}
	if _, err := a.command(ctx, a.busctl, "--user", "--json=short", "get-property", "org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", "Version"); err != nil {
		return a.commandError(ErrServiceManagerUnavailable, "bus:busctl_version", err, root)
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
		return serviceSystemdError(ErrServiceManagerUnavailable, "unit_dir_path", "not_absolute_clean")
	}
	rel, err := filepath.Rel(base, a.unitDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return serviceSystemdError(ErrServiceOwnershipConflict, "unit_dir_path", "outside_base")
	}
	path := base
	parts := append([]string{""}, strings.Split(rel, string(filepath.Separator))...)
	for i, part := range parts {
		step := fmt.Sprintf("unit_dir_walk:%d:", i)
		parent := path
		path = filepath.Join(path, part)
		info, err := a.lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			if i == 0 {
				return serviceSystemdError(ErrServiceManagerUnavailable, step+"stat", "base_missing")
			}
			if !a.directoryAccessible(parent, true) {
				return serviceSystemdError(ErrServiceManagerUnavailable, step+"parent_access", "write_search_denied")
			}
			if !create {
				return nil
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				return a.commandError(ErrServiceManagerUnavailable, step+"mkdir", err, "")
			}
			info, err = a.lstat(path)
		}
		// Both walks use this same policy, including for freshly created paths.
		// Manager-created wants entries are checked separately, never as private
		// adapter-created directories. Owned 0755 directories are valid here.
		if err != nil {
			return a.commandError(ErrServiceOwnershipConflict, step+"stat", err, "")
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return serviceSystemdError(ErrServiceOwnershipConflict, step+"type", "not_directory_or_symlink")
		}
		if info.Mode().Perm()&0o022 != 0 {
			return serviceSystemdError(ErrServiceOwnershipConflict, step+"mode", fmt.Sprintf("%04o", info.Mode().Perm()))
		}
		if a.owned == nil || !a.owned(path, info) {
			return serviceSystemdError(ErrServiceOwnershipConflict, step+"owner", "not_current_uid")
		}
		if !a.directoryAccessible(path, i == len(parts)-1) {
			return serviceSystemdError(ErrServiceManagerUnavailable, step+"access", "required_access_denied")
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
	if err != nil {
		return false, a.commandError(ErrServiceOwnershipConflict, "unit_file:stat", err, d.Root)
	}
	if !info.Mode().IsRegular() {
		return false, serviceSystemdError(ErrServiceOwnershipConflict, "unit_file:type", "not_regular")
	}
	if info.Mode().Perm() != 0o600 {
		return false, serviceSystemdError(ErrServiceOwnershipConflict, "unit_file:mode", fmt.Sprintf("%04o", info.Mode().Perm()))
	}
	if a.owned == nil || !a.owned(path, info) {
		return false, serviceSystemdError(ErrServiceOwnershipConflict, "unit_file:owner", "not_current_uid")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, a.commandError(ErrServiceOwnershipConflict, "unit_file:read", err, d.Root)
	}
	if !bytes.Equal(data, want) {
		return false, serviceSystemdError(ErrServiceOwnershipConflict, "unit_file:content", "mismatch")
	}
	return true, nil
}

func (a *systemdUserAdapter) inspectEnablement(d ServiceDescriptor) error {
	dir := filepath.Join(a.unitDir, "default.target.wants")
	info, err := a.lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return a.commandError(ErrServiceOwnershipConflict, "enablement_dir:stat", err, d.Root)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return serviceSystemdError(ErrServiceOwnershipConflict, "enablement_dir:type", "not_directory_or_symlink")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return serviceSystemdError(ErrServiceOwnershipConflict, "enablement_dir:mode", fmt.Sprintf("%04o", info.Mode().Perm()))
	}
	if a.owned == nil || !a.owned(dir, info) {
		return serviceSystemdError(ErrServiceOwnershipConflict, "enablement_dir:owner", "not_current_uid")
	}
	link := filepath.Join(dir, serviceUnitLabel(d))
	info, err = a.lstat(link)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return a.commandError(ErrServiceOwnershipConflict, "enablement_link:stat", err, d.Root)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return serviceSystemdError(ErrServiceOwnershipConflict, "enablement_link:type", "not_symlink")
	}
	if !a.owned(link, info) {
		return serviceSystemdError(ErrServiceOwnershipConflict, "enablement_link:owner", "not_current_uid")
	}
	target, err := os.Readlink(link)
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	if err != nil {
		return a.commandError(ErrServiceOwnershipConflict, "enablement_link:read", err, d.Root)
	}
	if filepath.Clean(target) != a.unitPath(d) {
		return serviceSystemdError(ErrServiceOwnershipConflict, "enablement_link:target", "mismatch")
	}
	return nil
}

func (a *systemdUserAdapter) Inspect(ctx context.Context, d ServiceIdentity) (ServiceObservation, error) {
	want, err := RenderSystemdUserUnit(d)
	// uid is captured from os.Getuid by the native constructor. A user manager's
	// optional User property is not an ownership boundary or identity authority.
	if err != nil {
		return ServiceObservation{Reason: "service_ownership_conflict"}, err
	}
	if d.UID != a.uid {
		return ServiceObservation{Reason: "service_ownership_conflict"}, serviceSystemdError(ErrServiceOwnershipConflict, "inspect:uid", "not_current_uid")
	}
	if err := a.capabilityForRoot(ctx, d.Root); err != nil {
		if errors.Is(err, ErrServiceOwnershipConflict) {
			return ServiceObservation{Reason: "service_ownership_conflict"}, err
		}
		return serviceUnavailable(err)
	}
	exists, err := a.ownedUnit(d, want)
	if err != nil {
		if errors.Is(err, ErrServiceManagerUnavailable) {
			return serviceUnavailable(err)
		}
		return ServiceObservation{Available: true, Reason: "service_ownership_conflict"}, err
	}
	// Scalar show properties have no argument-array ambiguity. Reject drop-ins:
	// otherwise matching the main file would not establish the loaded contract.
	raw, err := a.command(ctx, a.systemctl, "--user", "show", serviceUnitLabel(d), "--property=Id,LoadState,FragmentPath,DropInPaths", "--no-pager")
	if err != nil {
		return serviceUnavailable(a.commandError(ErrServiceManagerUnavailable, "inspect:systemctl_show", err, d.Root))
	}
	properties, err := systemdProperties(raw)
	if err != nil {
		return ServiceObservation{Available: true, Reason: "service_ownership_conflict"}, err
	}
	if properties["LoadState"] == "not-found" {
		return ServiceObservation{Available: true, Registered: exists, Owned: exists}, nil
	}
	for _, check := range []struct {
		step string
		ok   bool
	}{
		{"unit_file", exists},
		{"id", properties["Id"] == serviceUnitLabel(d)},
		{"fragment", properties["FragmentPath"] == a.unitPath(d)},
		{"drop_ins", properties["DropInPaths"] == ""},
	} {
		if !check.ok {
			return ServiceObservation{Available: true, Registered: true, Reason: "service_ownership_conflict"}, serviceSystemdError(ErrServiceOwnershipConflict, "inspect:"+check.step, "mismatch")
		}
	}
	if properties["LoadState"] != "loaded" {
		return ServiceObservation{Available: true, Registered: true, Reason: "service_registration_failed"}, serviceSystemdError(ErrServiceRegistrationFailed, "inspect:load_state", "not_loaded")
	}
	if err := a.inspectExecStart(ctx, d); err != nil {
		if errors.Is(err, ErrServiceManagerUnavailable) {
			return serviceUnavailable(err)
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
			return nil, serviceSystemdError(ErrServiceOwnershipConflict, "inspect:properties", "malformed")
		}
		if _, seen := props[key]; seen {
			return nil, serviceSystemdError(ErrServiceOwnershipConflict, "inspect:properties", "duplicate")
		}
		props[key] = value
	}
	for _, key := range []string{"Id", "LoadState", "FragmentPath", "DropInPaths"} {
		if _, ok := props[key]; !ok {
			return nil, serviceSystemdError(ErrServiceOwnershipConflict, "inspect:properties", "missing_"+key)
		}
	}
	return props, nil
}

type systemdBusResult struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func (a *systemdUserAdapter) inspectExecStart(ctx context.Context, d ServiceDescriptor) error {
	var raw []byte
	var result systemdBusResult
	for attempt := 0; ; attempt++ {
		// GetUnit only finds units still held in memory. LoadUnit also resolves an
		// inactive on-disk unit without starting it or changing persistent state.
		loaded, err := a.command(ctx, a.busctl, "--user", "--json=short", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", "LoadUnit", "s", serviceUnitLabel(d))
		if err != nil {
			return a.commandError(ErrServiceManagerUnavailable, "inspect:busctl_load_unit", err, d.Root)
		}
		var object []string
		result = systemdBusResult{}
		if json.Unmarshal(loaded, &result) != nil || result.Type != "o" {
			return serviceSystemdError(ErrServiceOwnershipConflict, "inspect:busctl_load_unit", "invalid_type_or_json")
		}
		if json.Unmarshal(result.Data, &object) != nil {
			var path string
			if json.Unmarshal(result.Data, &path) != nil {
				return serviceSystemdError(ErrServiceOwnershipConflict, "inspect:busctl_load_unit", "invalid_object_json")
			}
			object = []string{path}
		}
		if len(object) != 1 || !strings.HasPrefix(object[0], "/org/freedesktop/systemd1/unit/") {
			return serviceSystemdError(ErrServiceOwnershipConflict, "inspect:busctl_load_unit", "invalid_object_path")
		}
		raw, err = a.command(ctx, a.busctl, "--user", "--json=short", "get-property", "org.freedesktop.systemd1", object[0], "org.freedesktop.systemd1.Service", "ExecStart")
		if err == nil {
			break
		}
		failure := a.commandError(ErrServiceManagerUnavailable, "inspect:busctl_exec_start", err, d.Root)
		// A transiently released unit gets one fresh load and property read.
		// Classify only sanitized command detail; unrelated failures are final.
		if attempt != 0 || (!strings.Contains(failure.Error(), "org.freedesktop.systemd1.NoSuchUnit") && !strings.Contains(failure.Error(), "not loaded")) {
			return failure
		}
	}
	var records [][]json.RawMessage
	if json.Unmarshal(raw, &result) != nil || result.Type != "a(sasbttttuii)" || json.Unmarshal(result.Data, &records) != nil {
		return serviceSystemdError(ErrServiceOwnershipConflict, "inspect:exec_start", "invalid_type_or_json")
	}
	// busctl's message serialization may wrap the single property value in
	// the message's argument array. Accept either structural representation;
	// both must contain exactly one executable with exactly the same argv.
	if len(records) == 1 && len(records[0]) == 1 {
		var envelope []json.RawMessage
		if json.Unmarshal(result.Data, &envelope) != nil || len(envelope) != 1 || json.Unmarshal(envelope[0], &records) != nil {
			return serviceSystemdError(ErrServiceOwnershipConflict, "inspect:exec_start", "invalid_envelope")
		}
	}
	if len(records) != 1 || len(records[0]) != 10 {
		return serviceSystemdError(ErrServiceOwnershipConflict, "inspect:exec_start", "invalid_record_count_or_shape")
	}
	var path string
	var argv []string
	var ignoreFailure bool
	if json.Unmarshal(records[0][0], &path) != nil || json.Unmarshal(records[0][1], &argv) != nil || json.Unmarshal(records[0][2], &ignoreFailure) != nil || ignoreFailure || path != d.Payload.Host {
		return serviceSystemdError(ErrServiceOwnershipConflict, "inspect:exec_start", "invalid_path_argv_or_failure_policy")
	}
	want := append([]string{d.Payload.Host}, d.Payload.Args...)
	if len(argv) != len(want) {
		return serviceSystemdError(ErrServiceOwnershipConflict, "inspect:exec_start", "argv_length_mismatch")
	}
	for i := range argv {
		if argv[i] != want[i] {
			return serviceSystemdError(ErrServiceOwnershipConflict, "inspect:exec_start", fmt.Sprintf("argv_%d_mismatch", i))
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
		return serviceSystemdError(ErrServiceOwnershipConflict, "apply:privilege", "not_user")
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
			return a.commandError(ErrServiceOwnershipConflict, "unit_file:create", err, d.Root)
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
			step := "unit_file:write"
			if statErr != nil {
				step = "unit_file:stat_created"
			} else if writeErr == nil {
				step, writeErr = "unit_file:close", closeErr
			}
			return a.commandError(ErrServiceRegistrationFailed, step, writeErr, d.Root)
		}
		created = true
		if err := syncDirectory(a.unitDir); err != nil {
			a.removeCreatedUnit(d, identity)
			return a.commandError(ErrServiceRegistrationFailed, "unit_dir:sync_created", err, d.Root)
		}
	}
	if _, err := a.command(ctx, a.systemctl, "--user", "daemon-reload"); err != nil {
		if created {
			a.cleanupPartial(ctx, d, want)
		}
		return a.commandError(ErrServiceRegistrationFailed, "daemon_reload", err, d.Root)
	}
	// Loading may expose a conflicting registration/drop-in; never enable it.
	if observation, err := a.Inspect(ctx, d); err != nil || !observation.Owned {
		if created {
			a.cleanupPartial(ctx, d, want)
		}
		return serviceRegistrationError("post_reload", err)
	}
	if _, err := a.command(ctx, a.systemctl, "--user", "enable", "--now", serviceUnitLabel(d)); err != nil {
		if created {
			a.cleanupPartial(ctx, d, want)
		}
		return a.commandError(ErrServiceRegistrationFailed, "enable_now", err, d.Root)
	}
	if observation, err := a.Inspect(ctx, d); err != nil || !observation.Owned {
		return serviceRegistrationError("post_enable", err)
	}
	return nil
}

func serviceRegistrationError(step string, err error) error {
	if errors.Is(err, ErrServiceOwnershipConflict) {
		return fmt.Errorf("%w: %s: %w", ErrServiceOwnershipConflict, step, err)
	}
	if err != nil {
		// Keep the registration prefix and original cause/sentinel. The caller
		// has already sanitized any command output before it reaches this layer.
		return fmt.Errorf("%w: %s: %w", ErrServiceRegistrationFailed, step, err)
	}
	return serviceSystemdError(ErrServiceRegistrationFailed, step, "not_owned")
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
		return serviceSystemdError(ErrServiceOwnershipConflict, "remove:ownership", "not_owned")
	}
	if _, err := a.command(ctx, a.systemctl, "--user", "disable", "--now", serviceUnitLabel(d)); err != nil {
		return a.commandError(ErrServiceRemovalFailed, "disable_now", err, d.Root)
	}
	want, err := RenderSystemdUserUnit(d)
	if err != nil {
		return err
	}
	if exists, err := a.ownedUnit(d, want); err != nil {
		return err
	} else if !exists {
		return serviceSystemdError(ErrServiceOwnershipConflict, "remove:unit_file", "missing")
	}
	if err := os.Remove(a.unitPath(d)); err != nil {
		return a.commandError(ErrServiceRemovalFailed, "unit_file:remove", err, d.Root)
	}
	if err := syncDirectory(a.unitDir); err != nil {
		return a.commandError(ErrServiceRemovalFailed, "unit_dir:sync_removed", err, d.Root)
	}
	if _, err := a.command(ctx, a.systemctl, "--user", "daemon-reload"); err != nil {
		return a.commandError(ErrServiceRemovalFailed, "daemon_reload", err, d.Root)
	}
	return nil
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
