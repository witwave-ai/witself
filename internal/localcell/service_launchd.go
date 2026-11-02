package localcell

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const launchdExitTimeout = 90 * time.Second

func serviceLaunchdLabel(d ServiceDescriptor) string {
	return "ai.witwave.witself.cell." + d.InstallationID
}

// RenderLaunchdAgentPlist renders only the explicit user host invocation.
// launchd.plist(5), KeepAlive: false leaves restarts to demand; the supervisor
// owns child restarts. RunAtLoad requests one launch when the agent is loaded.
// LimitLoadToSessionType is optional (launchd.plist(5)); the adapter instead
// requires the exact owning gui/<uid> domain (launchctl(1), domain targets).
func RenderLaunchdAgentPlist(d ServiceDescriptor) ([]byte, error) {
	if d.Adapter != "launchd-agent" {
		return nil, serviceSystemdError(ErrServiceOwnershipConflict, "spec:adapter", "invalid")
	}
	// Share the existing numeric UID, identity, root and payload contract without
	// changing the systemd renderer or its behavior.
	check := d
	check.Adapter = "systemd-user"
	if err := validateSystemdSpec(check); err != nil {
		return nil, err
	}
	args := append([]string{d.Payload.Host}, d.Payload.Args...)
	for _, value := range append(append([]string{}, args...), d.Root) {
		if !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 || strings.ContainsAny(value, "\ufffe\uffff") {
			return nil, serviceSystemdError(ErrServiceOwnershipConflict, "render:argument", "invalid_xml_or_control_character")
		}
	}
	var b bytes.Buffer
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\">\n<dict>\n")
	value := func(s string) {
		b.WriteString("<string>")
		_ = xml.EscapeText(&b, []byte(s)) // bytes.Buffer writes cannot fail.
		b.WriteString("</string>\n")
	}
	b.WriteString("<key>Label</key>\n")
	value(serviceLaunchdLabel(d))
	b.WriteString("<key>ProgramArguments</key>\n<array>\n")
	for _, arg := range args {
		value(arg)
	}
	b.WriteString("</array>\n<key>RunAtLoad</key>\n<true/>\n<key>KeepAlive</key>\n<false/>\n<key>ProcessType</key>\n<string>Standard</string>\n<key>ExitTimeOut</key>\n<integer>90</integer>\n<key>EnvironmentVariables</key>\n<dict>\n<key>WITSELF_RUNTIME</key>\n<string>local</string>\n</dict>\n<key>StandardErrorPath</key>\n")
	value(filepath.Join(d.Root, "service", "host-stderr.log"))
	b.WriteString("<key>StandardOutPath</key>\n")
	value(filepath.Join(d.Root, "service", "host-stdout.log"))
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes(), nil
}

type launchdAgentAdapter struct {
	uid, home, agentDir, launchctl string
	command                        serviceManagerCommand
	owned                          func(string, os.FileInfo) bool
	accessible                     func(string, bool) bool
	inspectFile                    func(string) (os.FileInfo, error)
	permissions                    func(os.FileInfo) os.FileMode
	writePlist                     func(*os.File, []byte) error
	syncDir                        func(string) error
	removalTimeout                 time.Duration
	unavailable                    bool
}

func (a *launchdAgentAdapter) domain() string { return "gui/" + a.uid }
func (a *launchdAgentAdapter) target(d ServiceDescriptor) string {
	return a.domain() + "/" + serviceLaunchdLabel(d)
}
func (a *launchdAgentAdapter) plistPath(d ServiceDescriptor) string {
	return filepath.Join(a.agentDir, serviceLaunchdLabel(d)+".plist")
}
func (a *launchdAgentAdapter) lstat(path string) (os.FileInfo, error) {
	if a.inspectFile != nil {
		return a.inspectFile(path)
	}
	return os.Lstat(path)
}
func (a *launchdAgentAdapter) sync(path string) error {
	if a.syncDir != nil {
		return a.syncDir(path)
	}
	return syncDirectory(path)
}
func (a *launchdAgentAdapter) fileMode(info os.FileInfo) os.FileMode {
	if a.permissions != nil {
		return a.permissions(info)
	}
	return info.Mode()
}
func (a *launchdAgentAdapter) commandError(reason error, step string, err error, root string) error {
	return serviceSystemdCommandError(reason, step, err, root, a.home, a.agentDir)
}
func (a *launchdAgentAdapter) directoryAccessible(path string, write bool) bool {
	if a.accessible != nil {
		return a.accessible(path, write)
	}
	info, err := a.lstat(path)
	mode := os.FileMode(0o100)
	if write {
		mode |= 0o200
	}
	return err == nil && a.fileMode(info).Perm()&mode == mode
}

// Walk only from the selected home, never from another user's or system agents.
// Missing directories are observed without creation until Apply.
func (a *launchdAgentAdapter) inspectDirectories(create bool) error {
	if !filepath.IsAbs(a.home) || filepath.Clean(a.home) != a.home || a.agentDir != filepath.Join(a.home, "Library", "LaunchAgents") {
		return serviceSystemdError(ErrServiceManagerUnavailable, "agent_dir_path", "not_home_launch_agents")
	}
	path := a.home
	for i, part := range []string{"", "Library", "LaunchAgents"} {
		step := fmt.Sprintf("agent_dir_walk:%d:", i)
		parent := path
		path = filepath.Join(path, part)
		info, err := a.lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			if i == 0 {
				return serviceSystemdError(ErrServiceManagerUnavailable, step+"stat", "home_missing")
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
			if err := a.sync(parent); err != nil {
				return a.commandError(ErrServiceRegistrationFailed, step+"sync", err, "")
			}
			info, err = a.lstat(path)
		}
		if err != nil {
			return a.commandError(ErrServiceOwnershipConflict, step+"stat", err, "")
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return serviceSystemdError(ErrServiceOwnershipConflict, step+"type", "not_directory_or_symlink")
		}
		if a.fileMode(info).Perm()&0o022 != 0 {
			return serviceSystemdError(ErrServiceOwnershipConflict, step+"mode", "group_or_world_writable")
		}
		if a.owned == nil || !a.owned(path, info) {
			return serviceSystemdError(ErrServiceOwnershipConflict, step+"owner", "not_current_uid")
		}
		if !a.directoryAccessible(path, i == 2) {
			return serviceSystemdError(ErrServiceManagerUnavailable, step+"access", "required_access_denied")
		}
	}
	return nil
}

func (a *launchdAgentAdapter) capability(ctx context.Context, root string) error {
	for _, check := range []struct {
		step string
		ok   bool
	}{
		{"identity", !a.unavailable && a.uid != "" && a.uid != "0"},
		{"command_runner", a.command != nil},
		{"launchctl_path", a.launchctl == "/bin/launchctl"},
	} {
		if !check.ok {
			return serviceSystemdError(ErrServiceManagerUnavailable, check.step, "unavailable")
		}
	}
	if err := a.inspectDirectories(false); err != nil {
		return err
	}
	// launchctl(1), gui/<uid>: this targets the GUI login domain. An absent
	// domain is a capability refusal, never permission to use user/ or system/.
	// reports/LC-S7C1-owner-premise-evidence.md, read-only probes, absent-domain
	// bullet observes exit 112; every failed probe is refused regardless of code.
	if _, err := a.command(ctx, a.launchctl, "print", a.domain()); err != nil {
		return a.commandError(ErrServiceManagerUnavailable, "capability:gui_domain", err, root)
	}
	return nil
}

func (a *launchdAgentAdapter) ownedPlist(d ServiceDescriptor, want []byte) (os.FileInfo, error) {
	if err := a.inspectDirectories(false); err != nil {
		return nil, err
	}
	path := a.plistPath(d)
	info, err := a.lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, a.commandError(ErrServiceOwnershipConflict, "plist_file:stat", err, d.Root)
	}
	if !info.Mode().IsRegular() {
		return nil, serviceSystemdError(ErrServiceOwnershipConflict, "plist_file:type", "not_regular")
	}
	if a.fileMode(info) != 0o600 {
		return nil, serviceSystemdError(ErrServiceOwnershipConflict, "plist_file:mode", "not_private")
	}
	if a.owned == nil || !a.owned(path, info) {
		return nil, serviceSystemdError(ErrServiceOwnershipConflict, "plist_file:owner", "not_current_uid")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, a.commandError(ErrServiceOwnershipConflict, "plist_file:open", err, d.Root)
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, serviceSystemdError(ErrServiceOwnershipConflict, "plist_file:identity", "changed")
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(len(want))+1))
	if err != nil {
		return nil, a.commandError(ErrServiceOwnershipConflict, "plist_file:read", err, d.Root)
	}
	current, err := a.lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) || a.fileMode(current) != 0o600 || !a.owned(path, current) {
		return nil, serviceSystemdError(ErrServiceOwnershipConflict, "plist_file:identity", "changed")
	}
	if !bytes.Equal(data, want) {
		return nil, serviceSystemdError(ErrServiceOwnershipConflict, "plist_file:content", "mismatch")
	}
	return info, nil
}

type launchdPrint struct {
	path, program string
	arguments     []string
}

// launchctl(1), print and CAVEATS, explicitly disclaims a stable output format.
// Accept only this bounded structural shape; never trim/unquote argument bytes
// or search substrings that could match nested environment fields. Native CI
// must confirm this shape on each macOS row; an unknown shape refuses ownership.
func parseLaunchdPrint(raw []byte, target string) (launchdPrint, error) {
	var result launchdPrint
	bad := func() (launchdPrint, error) {
		return launchdPrint{}, serviceSystemdError(ErrServiceOwnershipConflict, "inspect:parse", "unsupported_or_incomplete_print")
	}
	if len(raw) > 1<<20 || !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 || bytes.ContainsRune(raw, '\r') {
		return bad()
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) < 2 || lines[0] != target+" = {" || lines[len(lines)-1] != "}" {
		return bad()
	}
	depth, inArgs := 1, false
	seen := map[string]bool{}
	for _, line := range lines[1 : len(lines)-1] {
		if inArgs {
			if line == "\t}" {
				inArgs = false
				continue
			}
			if !strings.HasPrefix(line, "\t\t") || strings.IndexFunc(line[2:], unicode.IsControl) >= 0 {
				return bad()
			}
			result.arguments = append(result.arguments, line[2:])
			continue
		}
		if line == "" { // print separates sections with blank lines.
			continue
		}
		prefix := strings.Repeat("\t", depth)
		if depth > 1 && line == strings.Repeat("\t", depth-1)+"}" {
			depth--
			continue
		}
		if !strings.HasPrefix(line, prefix) {
			return bad()
		}
		key, value, ok := strings.Cut(line[len(prefix):], " = ")
		if !ok && depth > 1 {
			// Nested diagnostic dictionaries cannot supply ownership fields.
			// Accept an arrow assignment there without interpreting its value.
			key, value, ok = strings.Cut(line[len(prefix):], " => ")
		}
		if !ok || key == "" || strings.TrimSpace(key) != key || strings.IndexFunc(key, unicode.IsControl) >= 0 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return bad()
		}
		if depth == 1 {
			if seen[key] {
				return bad()
			}
			seen[key] = true
			switch key {
			case "path":
				result.path = value
			case "program":
				result.program = value
			case "arguments":
				if value != "{" {
					return bad()
				}
				inArgs = true
				continue
			}
		}
		if value == "{" {
			depth++
			if depth > 16 {
				return bad()
			}
		}
	}
	if depth != 1 || inArgs || result.path == "" || result.program == "" || !seen["arguments"] || len(result.arguments) == 0 {
		return bad()
	}
	return result, nil
}

// The explicit exit code AND stderr must agree; launchctl(1), error codes,
// describes separate bootstrap, POSIX and Mach error namespaces.
func launchdExitMatches(err error, code int, message string) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == code && strings.Contains(string(exitErr.Stderr), message)
}

// reports/LC-S7C1-owner-premise-evidence.md, read-only probes, missing-service
// bullet: exit 113 and "Could not find service" were observed together.
func launchdMissing(err error) bool { return launchdExitMatches(err, 113, "Could not find service") }

func (a *launchdAgentAdapter) Inspect(ctx context.Context, d ServiceIdentity) (ServiceObservation, error) {
	want, err := RenderLaunchdAgentPlist(d)
	conflict := ServiceObservation{Available: true, Registered: true, Reason: "service_ownership_conflict"}
	if err != nil {
		return conflict, err
	}
	if d.UID != a.uid {
		return conflict, serviceSystemdError(ErrServiceOwnershipConflict, "inspect:uid", "not_current_uid")
	}
	if err := a.capability(ctx, d.Root); err != nil {
		if errors.Is(err, ErrServiceOwnershipConflict) {
			return conflict, err
		}
		return serviceUnavailable(err)
	}
	file, err := a.ownedPlist(d, want)
	if err != nil {
		return conflict, err
	}
	raw, err := a.command(ctx, a.launchctl, "print", a.target(d))
	if launchdMissing(err) {
		if file != nil {
			return ServiceObservation{Available: true, Registered: true, Reason: "service_registration_failed"}, serviceSystemdError(ErrServiceRegistrationFailed, "inspect:not_loaded", "plist_present")
		}
		return ServiceObservation{Available: true, Owned: true}, nil
	}
	if err != nil {
		return serviceUnavailable(a.commandError(ErrServiceManagerUnavailable, "inspect:print", err, d.Root))
	}
	loaded, err := parseLaunchdPrint(raw, a.target(d))
	if err != nil {
		return conflict, err
	}
	for _, check := range []struct {
		step string
		ok   bool
	}{
		{"plist", file != nil},
		{"path", loaded.path == a.plistPath(d)},
		{"program", loaded.program == d.Payload.Host},
		{"arguments", slices.Equal(loaded.arguments, append([]string{d.Payload.Host}, d.Payload.Args...))},
	} {
		if !check.ok {
			return conflict, serviceSystemdError(ErrServiceOwnershipConflict, "inspect:"+check.step, "mismatch")
		}
	}
	return ServiceObservation{Available: true, Registered: true, Owned: true}, nil
}

func (a *launchdAgentAdapter) Plan(ctx context.Context, d ServiceSpec) (ServicePlan, error) {
	obs, err := a.Inspect(ctx, d)
	// Keep Inspect's unloaded-state evidence, but allow explicit install to
	// retry an interrupted bootstrap. Apply rechecks the exact private bytes.
	if errors.Is(err, ErrServiceRegistrationFailed) && obs.Reason == "service_registration_failed" {
		err = nil
	}
	return ServicePlan{Spec: d, Observation: obs, RequiredPrivilege: "user"}, err
}

func (a *launchdAgentAdapter) Apply(ctx context.Context, plan ServicePlan) error {
	if plan.RequiredPrivilege != "user" {
		return serviceSystemdError(ErrServiceOwnershipConflict, "apply:privilege", "not_user")
	}
	d := plan.Spec
	obs, err := a.Inspect(ctx, d)
	// A matching private plist left by an interrupted bootstrap can be reused;
	// Inspect still truthfully reports it as registered but not loaded/owned.
	if err != nil && (!errors.Is(err, ErrServiceRegistrationFailed) || obs.Reason != "service_registration_failed") {
		return err
	}
	if err == nil && obs.Registered && obs.Owned {
		return nil
	}
	want, err := RenderLaunchdAgentPlist(d)
	if err != nil {
		return err
	}
	if err := a.inspectDirectories(true); err != nil {
		return err
	}
	identity, err := a.ownedPlist(d, want)
	if err != nil {
		return err
	}
	var created os.FileInfo
	if identity == nil {
		file, err := os.OpenFile(a.plistPath(d), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return a.commandError(ErrServiceOwnershipConflict, "plist_file:create", err, d.Root)
		}
		created, err = file.Stat()
		if err == nil {
			err = file.Chmod(0o600)
		}
		if err == nil {
			if a.writePlist != nil {
				err = a.writePlist(file, want)
			} else {
				_, err = file.Write(want)
				if err == nil {
					err = file.Sync()
				}
			}
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			err = a.sync(a.agentDir)
		}
		if err != nil {
			_ = a.removePlistIdentity(d, created)
			return a.commandError(ErrServiceRegistrationFailed, "plist_file:persist", err, d.Root)
		}
		identity = created
	}
	fail := func(step string, err error) error {
		if created != nil {
			a.cleanupPartial(ctx, d, created)
		}
		return fmt.Errorf("%w: %s: %w", ErrServiceRegistrationFailed, step, err)
	}
	checkIdentity := func(step string) error {
		current, err := a.ownedPlist(d, want)
		if err == nil && (current == nil || !os.SameFile(identity, current)) {
			err = serviceSystemdError(ErrServiceOwnershipConflict, step, "plist_changed")
		}
		if err != nil {
			return fail(step, err)
		}
		return nil
	}
	if err := checkIdentity("pre_enable"); err != nil {
		return err
	}
	// launchctl(1), enable/disable: a disabled service cannot load until enabled;
	// overrides persist across boots. Clear that override before bootstrap.
	if _, err := a.command(ctx, a.launchctl, "enable", a.target(d)); err != nil {
		return fail("enable", a.commandError(ErrServiceRegistrationFailed, "enable", err, d.Root))
	}
	if err := checkIdentity("pre_bootstrap"); err != nil {
		return err
	}
	if _, err := a.command(ctx, a.launchctl, "bootstrap", a.domain(), a.plistPath(d)); err != nil {
		return fail("bootstrap", a.commandError(ErrServiceRegistrationFailed, "bootstrap", err, d.Root))
	}
	if observed, err := a.Inspect(ctx, d); err != nil || !observed.Owned || !observed.Registered {
		if err == nil {
			err = serviceSystemdError(ErrServiceRegistrationFailed, "post_bootstrap", "not_owned_or_loaded")
		}
		return fail("post_bootstrap", err)
	}
	return nil
}

func (a *launchdAgentAdapter) removePlistIdentity(d ServiceDescriptor, identity os.FileInfo) error {
	if identity == nil {
		return serviceSystemdError(ErrServiceOwnershipConflict, "remove:identity", "missing")
	}
	if err := a.inspectDirectories(false); err != nil {
		return err
	}
	current, err := a.lstat(a.plistPath(d))
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(identity, current) {
		return serviceSystemdError(ErrServiceOwnershipConflict, "remove:identity", "changed")
	}
	if err := os.Remove(a.plistPath(d)); err != nil {
		return a.commandError(ErrServiceRemovalFailed, "plist_file:remove", err, d.Root)
	}
	if err := a.sync(a.agentDir); err != nil {
		return a.commandError(ErrServiceRemovalFailed, "agent_dir:sync_removed", err, d.Root)
	}
	return nil
}

func (a *launchdAgentAdapter) cleanupPartial(ctx context.Context, d ServiceDescriptor, created os.FileInfo) {
	// A foreign print result never authorizes bootout. Only the original file
	// identity can be removed, including when bootstrap exposed a foreign label.
	obs, err := a.Inspect(ctx, d)
	if errors.Is(err, ErrServiceManagerUnavailable) {
		// Cancellation or a lost manager cannot establish absence. Keep the
		// private record so a partially loaded job remains explicitly removable.
		return
	}
	if err == nil && obs.Registered && obs.Owned {
		if err := a.bootout(ctx, d); err != nil {
			// Retain the exact ownership record if the verified registration
			// could not be removed; a later explicit Remove must remain usable.
			return
		}
	}
	_ = a.removePlistIdentity(d, created)
}

func (a *launchdAgentAdapter) bootout(ctx context.Context, d ServiceDescriptor) error {
	timeout := a.removalTimeout
	if timeout <= 0 || timeout > launchdExitTimeout+30*time.Second {
		timeout = launchdExitTimeout + 30*time.Second
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := a.command(bounded, a.launchctl, "bootout", a.target(d))
	// Defensive tolerance: unobserved; errno.h citation only (Darwin sys/errno.h
	// maps EINPROGRESS to 36). reports/LC-S7C1-owner-premise-evidence.md,
	// Bootstrap-path observations, bootout(running) and EINPROGRESS bullets:
	// the observed path is exit 0 followed immediately by missing-service 113.
	if err != nil && !launchdExitMatches(err, 36, "Operation now in progress") {
		return a.commandError(ErrServiceRemovalFailed, "remove:bootout", err, d.Root)
	}
	interval := min(timeout/128, time.Second)
	if interval <= 0 {
		interval = time.Nanosecond
	}
	for {
		if err := bounded.Err(); err != nil {
			return a.commandError(ErrServiceRemovalFailed, "remove:wait", err, d.Root)
		}
		raw, err := a.command(bounded, a.launchctl, "print", a.target(d))
		if launchdMissing(err) {
			// Distinguish service absence from a lost GUI domain before deletion.
			if _, err := a.command(bounded, a.launchctl, "print", a.domain()); err != nil {
				return a.commandError(ErrServiceRemovalFailed, "remove:gui_domain", err, d.Root)
			}
			return nil
		}
		if err != nil {
			return a.commandError(ErrServiceRemovalFailed, "remove:print", err, d.Root)
		}
		loaded, err := parseLaunchdPrint(raw, a.target(d))
		if err != nil || loaded.path != a.plistPath(d) || loaded.program != d.Payload.Host || !slices.Equal(loaded.arguments, append([]string{d.Payload.Host}, d.Payload.Args...)) {
			return serviceSystemdError(ErrServiceOwnershipConflict, "remove:ownership", "changed_while_waiting")
		}
		timer := time.NewTimer(interval)
		select {
		case <-bounded.Done():
			timer.Stop()
			return a.commandError(ErrServiceRemovalFailed, "remove:wait", bounded.Err(), d.Root)
		case <-timer.C:
		}
	}
}

func (a *launchdAgentAdapter) Remove(ctx context.Context, d ServiceIdentity) error {
	obs, err := a.Inspect(ctx, d)
	if err != nil {
		if errors.Is(err, ErrServiceOwnershipConflict) || errors.Is(err, ErrServiceRegistrationFailed) {
			return fmt.Errorf("%w: remove:ownership: %w", ErrServiceOwnershipConflict, err)
		}
		return err
	}
	if !obs.Registered {
		return nil
	}
	if !obs.Owned {
		return serviceSystemdError(ErrServiceOwnershipConflict, "remove:ownership", "not_owned")
	}
	want, err := RenderLaunchdAgentPlist(d)
	if err != nil {
		return err
	}
	identity, err := a.ownedPlist(d, want)
	if err != nil || identity == nil {
		return serviceSystemdError(ErrServiceOwnershipConflict, "remove:ownership", "plist_changed")
	}
	if err := a.bootout(ctx, d); err != nil {
		return err
	}
	return a.removePlistIdentity(d, identity)
}
