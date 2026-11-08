package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"time"
)

// Typed is a native evidence fence, not a guess derived from stderr or a
// successful command. The native runner leaves it false until the required
// absent/access-denied/RPC distinction is proven on the Windows row.
type windowsTaskCommandResult struct {
	Output []byte
	Code   uint32
	Typed  bool
}
type windowsTaskCommand func(context.Context, ...string) (windowsTaskCommandResult, error)

type windowsLogonTaskAdapter struct {
	uid            string
	command        windowsTaskCommand
	capability     func(context.Context) error
	readDescriptor func(ServiceDescriptor) (ServiceDescriptor, error)
	drain          func(context.Context, ServiceDescriptor) error
	// Tests can inject construction, never the production host-exit transaction.
	removalCell func(ServiceDescriptor) (*Cell, error)
	lock        func(ServiceDescriptor) (func(), error)
	// The hermetic fake uses a temporary native directory while modelling a
	// drive-qualified Windows descriptor. Production always derives it from Root.
	recordDirectory func(ServiceDescriptor) string
}

type windowsTaskFailure struct {
	kind   error
	step   string
	code   uint32
	reason string
}

// Diagnostics have a closed vocabulary even if a future caller mistakenly
// supplies manager output or another private string. Source contracts separately
// require fixed arguments at every production raising site.
func validWindowsTaskDiagnostic(kind error, step, reason string) bool {
	switch kind {
	case ErrServiceOwnershipConflict, ErrServiceManagerUnavailable, ErrServiceRegistrationFailed, ErrServiceDesiredState, ErrServiceRemovalFailed:
	default:
		return false
	}
	switch step {
	case "capability:session", "query:result", "inspect:principal", "inspect:trigger", "inspect:action", "inspect:settings", "inspect:host_owner", "create", "post_create", "remove:host_exit", "remove:ownership", "delete", "remove:absence":
	default:
		return false
	}
	switch reason {
	case "diagnostic_argument_refused", "invalid", "unavailable", "interactive_token_unavailable", "mismatch", "query_result_untyped", "access_denied", "rpc_unavailable", "directory_unavailable", "private_pair_required", "missing", "not_user", "stale_plan", "private_create_failed", "appeared", "create_failed", "not_owned", "stopped", "run_failed", "host_exit_failed", "changed", "delete_failed", "still_present", "remove_failed", "too_large", "template_invalid", "xml_invalid", "task_value_percent_reference", "owner_token_refused", "system_directory_unavailable", "system_command_unavailable", "output_limit", "command_timeout", "command_failed", "verification_failed":
		return true
	default:
		return false
	}
}

func (e *windowsTaskFailure) Error() string {
	kind, step, reason := e.kind, e.step, e.reason
	if !validWindowsTaskDiagnostic(kind, step, reason) {
		kind, step, reason = ErrServiceOwnershipConflict, "query:result", "diagnostic_argument_refused"
	}
	return fmt.Sprintf("%s: %s: code=%d reason=%s", kind, step, e.code, reason)
}
func (e *windowsTaskFailure) Unwrap() error { return e.kind }
func windowsTaskError(kind error, step string, code uint32, reason string) error {
	if !validWindowsTaskDiagnostic(kind, step, reason) {
		kind, step, reason = ErrServiceOwnershipConflict, "query:result", "diagnostic_argument_refused"
	}
	return &windowsTaskFailure{kind: kind, step: step, code: code, reason: reason}
}

func (a *windowsLogonTaskAdapter) directory(d ServiceDescriptor) string {
	if a.recordDirectory != nil {
		return a.recordDirectory(d)
	}
	return filepath.Join(d.Root, "service")
}
func (a *windowsLogonTaskAdapter) xmlPath(d ServiceDescriptor) string {
	return filepath.Join(a.directory(d), "windows-task.xml")
}
func (a *windowsLogonTaskAdapter) receiptPath(d ServiceDescriptor) string {
	return filepath.Join(a.directory(d), "windows-task-receipt.json")
}

func windowsTaskImmutable(d ServiceDescriptor) ServiceDescriptor {
	d.DesiredState, d.Revision, d.RepairRevision, d.Faulted = "", 0, 0, false
	return d
}

func (a *windowsLogonTaskAdapter) current(d ServiceDescriptor) (ServiceDescriptor, error) {
	if a.readDescriptor != nil {
		return a.readDescriptor(d)
	}
	c, err := New(Config{Root: d.Root, Timeout: 60 * time.Second})
	if err != nil {
		return ServiceDescriptor{}, err
	}
	return c.ServiceDescriptor()
}

func (a *windowsLogonTaskAdapter) mutationLock(d ServiceDescriptor) (func(), error) {
	if a.lock != nil {
		return a.lock(d)
	}
	cell, err := New(Config{Root: d.Root, Timeout: 60 * time.Second})
	if err != nil {
		return nil, windowsTaskError(ErrServiceOwnershipConflict, "inspect:host_owner", 0, "unavailable")
	}
	lock, err := cell.own(false)
	if err != nil {
		return nil, windowsTaskError(ErrServiceOwnershipConflict, "inspect:host_owner", 0, "unavailable")
	}
	return func() { _ = lock.Close() }, nil
}

func (a *windowsLogonTaskAdapter) preflight(ctx context.Context, d ServiceDescriptor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.capability == nil {
		return windowsTaskError(ErrServiceManagerUnavailable, "capability:session", 0, "interactive_token_unavailable")
	}
	if err := a.capability(ctx); err != nil {
		return err
	}
	if err := validateWindowsTaskSpec(d); err != nil {
		return err
	}
	if a.uid != d.UID {
		return windowsTaskError(ErrServiceOwnershipConflict, "inspect:host_owner", 0, "mismatch")
	}
	if a.command == nil {
		return windowsTaskError(ErrServiceManagerUnavailable, "capability:session", 0, "unavailable")
	}
	current, err := a.current(d)
	if err != nil || !reflect.DeepEqual(windowsTaskImmutable(current), windowsTaskImmutable(d)) {
		return windowsTaskError(ErrServiceOwnershipConflict, "inspect:host_owner", 0, "mismatch")
	}
	return nil
}

// HRESULT_FROM_WIN32(ERROR_FILE_NOT_FOUND), E_ACCESSDENIED and
// HRESULT_FROM_WIN32(RPC_S_SERVER_UNAVAILABLE) are distinct only when a native
// fixture has established schtasks actually returns them on the runner build.
func classifyWindowsTaskQuery(result windowsTaskCommandResult, err error) (bool, error) {
	if err != nil || !result.Typed {
		return false, windowsTaskError(ErrServiceManagerUnavailable, "query:result", result.Code, "query_result_untyped")
	}
	switch result.Code {
	case 0:
		return true, nil
	case 0x80070002:
		if len(result.Output) != 0 {
			return false, windowsTaskError(ErrServiceManagerUnavailable, "query:result", result.Code, "query_result_untyped")
		}
		return false, nil
	case 0x80070005:
		return false, windowsTaskError(ErrServiceManagerUnavailable, "query:result", result.Code, "access_denied")
	case 0x800706ba:
		return false, windowsTaskError(ErrServiceManagerUnavailable, "query:result", result.Code, "rpc_unavailable")
	default:
		return false, windowsTaskError(ErrServiceManagerUnavailable, "query:result", result.Code, "query_result_untyped")
	}
}

func (a *windowsLogonTaskAdapter) query(ctx context.Context, d ServiceDescriptor) (bool, error) {
	name, _ := WindowsLogonTaskName(d)
	result, err := a.command(ctx, "/Query", "/TN", name, "/XML")
	present, err := classifyWindowsTaskQuery(result, err)
	if err != nil || !present {
		return present, err
	}
	if err := ValidateWindowsLogonTaskXML(result.Output, d); err != nil {
		return true, err
	}
	return true, nil
}

type windowsTaskReceipt struct {
	Version    int               `json:"version"`
	Descriptor ServiceDescriptor `json:"descriptor"`
}
type windowsTaskRecords struct {
	xml, receipt os.FileInfo
	exists       bool
}

func readWindowsTaskRecord(path string) ([]byte, os.FileInfo, error) {
	if err := servicePrivateRecord(path); err != nil {
		return nil, nil, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	// Windows FileInfo resolves its file ID lazily. Pin it before any operation
	// that can let a replacement occupy the same pathname.
	if !os.SameFile(before, before) {
		return nil, nil, errors.New("task record identity")
	}
	f, err := openPrivateFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, nil, errors.New("task record identity")
	}
	raw, err := io.ReadAll(io.LimitReader(f, windowsTaskXMLLimit+1))
	if err != nil || len(raw) > windowsTaskXMLLimit {
		return nil, nil, errors.New("task record read")
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || servicePrivateRecord(path) != nil {
		return nil, nil, errors.New("task record identity")
	}
	return raw, before, nil
}

func (a *windowsLogonTaskAdapter) records(d ServiceDescriptor) (windowsTaskRecords, error) {
	var records windowsTaskRecords
	if privateDir(a.directory(d), false) != nil {
		return records, windowsTaskError(ErrServiceOwnershipConflict, "inspect:host_owner", 0, "directory_unavailable")
	}
	xmlRaw, xmlInfo, xmlErr := readWindowsTaskRecord(a.xmlPath(d))
	receiptRaw, receiptInfo, receiptErr := readWindowsTaskRecord(a.receiptPath(d))
	if errors.Is(xmlErr, os.ErrNotExist) && errors.Is(receiptErr, os.ErrNotExist) {
		return records, nil
	}
	if xmlErr != nil || receiptErr != nil {
		return records, windowsTaskError(ErrServiceOwnershipConflict, "inspect:host_owner", 0, "private_pair_required")
	}
	if err := ValidateWindowsLogonTaskXML(xmlRaw, d); err != nil {
		return records, err
	}
	decoder := json.NewDecoder(bytes.NewReader(receiptRaw))
	decoder.DisallowUnknownFields()
	var receipt windowsTaskReceipt
	if decoder.Decode(&receipt) != nil || decoder.Decode(new(any)) != io.EOF || receipt.Version != 1 || !reflect.DeepEqual(receipt.Descriptor, windowsTaskImmutable(d)) {
		return records, windowsTaskError(ErrServiceOwnershipConflict, "inspect:host_owner", 0, "mismatch")
	}
	return windowsTaskRecords{xml: xmlInfo, receipt: receiptInfo, exists: true}, nil
}

func (a *windowsLogonTaskAdapter) Inspect(ctx context.Context, d ServiceIdentity) (ServiceObservation, error) {
	if err := a.preflight(ctx, d); err != nil {
		return serviceUnavailable(err)
	}
	present, err := a.query(ctx, d)
	if err != nil {
		if errors.Is(err, ErrServiceManagerUnavailable) {
			return serviceUnavailable(err)
		}
		return ServiceObservation{Available: true, Registered: present, Reason: "service_ownership_conflict"}, err
	}
	records, err := a.records(d)
	if err != nil {
		return ServiceObservation{Available: true, Registered: present, Reason: "service_ownership_conflict"}, err
	}
	if present && !records.exists {
		return ServiceObservation{Available: true, Registered: true, Reason: "service_ownership_conflict"}, windowsTaskError(ErrServiceOwnershipConflict, "inspect:host_owner", 0, "missing")
	}
	return ServiceObservation{Available: true, Registered: present, Owned: present && records.exists}, nil
}

func (a *windowsLogonTaskAdapter) Plan(ctx context.Context, d ServiceSpec) (ServicePlan, error) {
	observation, err := a.Inspect(ctx, d)
	return ServicePlan{Spec: d, Observation: observation, RequiredPrivilege: "user"}, err
}

func createWindowsTaskRecord(path string, raw []byte) error {
	f, err := openPrivateFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(raw)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func (a *windowsLogonTaskAdapter) Apply(ctx context.Context, plan ServicePlan) error {
	d := plan.Spec
	if plan.RequiredPrivilege != "user" {
		return windowsTaskError(ErrServiceOwnershipConflict, "create", 0, "not_user")
	}
	if err := a.preflight(ctx, d); err != nil {
		return err
	}
	unlock, err := a.mutationLock(d)
	if err != nil {
		return err
	}
	defer unlock()
	observation, err := a.Inspect(ctx, d)
	if err != nil {
		return err
	}
	current, err := a.current(d)
	if err != nil || !reflect.DeepEqual(current, d) || d.DesiredState != "running" || d.Faulted {
		return windowsTaskError(ErrServiceOwnershipConflict, "create", 0, "stale_plan")
	}
	if observation.Owned {
		return nil
	}
	records, err := a.records(d)
	if err != nil {
		return err
	}
	if !records.exists {
		raw, err := RenderWindowsLogonTaskXML(d)
		if err != nil {
			return err
		}
		if createWindowsTaskRecord(a.xmlPath(d), raw) != nil {
			return windowsTaskError(ErrServiceRegistrationFailed, "create", 0, "private_create_failed")
		}
		receipt, err := json.Marshal(windowsTaskReceipt{Version: 1, Descriptor: windowsTaskImmutable(d)})
		if err != nil || createWindowsTaskRecord(a.receiptPath(d), receipt) != nil {
			return windowsTaskError(ErrServiceRegistrationFailed, "create", 0, "private_create_failed")
		}
	}
	// Re-query after durable staging. No /F: a task appearing after this check
	// still causes the manager's exclusive create to fail without replacement.
	if present, err := a.query(ctx, d); err != nil {
		return err
	} else if present {
		return windowsTaskError(ErrServiceOwnershipConflict, "create", 0, "appeared")
	}
	if _, err := a.records(d); err != nil {
		return err
	}
	current, err = a.current(d)
	if err != nil || !reflect.DeepEqual(current, d) || current.DesiredState != "running" || current.Faulted {
		return windowsTaskError(ErrServiceOwnershipConflict, "create", 0, "stale_plan")
	}
	name, _ := WindowsLogonTaskName(d)
	result, err := a.command(ctx, "/Create", "/TN", name, "/XML", a.xmlPath(d))
	if err != nil || result.Code != 0 {
		return windowsTaskError(ErrServiceRegistrationFailed, "create", result.Code, "create_failed")
	}
	observed, err := a.Inspect(ctx, d)
	if err != nil {
		return windowsTaskError(ErrServiceRegistrationFailed, "post_create", 0, "verification_failed")
	}
	if !observed.Owned {
		return windowsTaskError(ErrServiceRegistrationFailed, "post_create", 0, "not_owned")
	}
	return nil
}

// Start is an explicit demand start after exact ownership revalidation. Apply
// only registers; it never changes persisted stopped intent to running.
func (a *windowsLogonTaskAdapter) Start(ctx context.Context, d ServiceIdentity) error {
	if err := a.preflight(ctx, d); err != nil {
		return err
	}
	unlock, err := a.mutationLock(d)
	if err != nil {
		return err
	}
	defer unlock()
	observation, err := a.Inspect(ctx, d)
	if err != nil {
		return err
	}
	current, err := a.current(d)
	if err != nil || current.DesiredState != "running" || current.Faulted {
		return windowsTaskError(ErrServiceDesiredState, "inspect:host_owner", 0, "stopped")
	}
	if !observation.Owned {
		return windowsTaskError(ErrServiceOwnershipConflict, "inspect:host_owner", 0, "not_owned")
	}
	name, _ := WindowsLogonTaskName(d)
	result, err := a.command(ctx, "/Run", "/TN", name)
	if err != nil || result.Code != 0 {
		return windowsTaskError(ErrServiceRegistrationFailed, "create", result.Code, "run_failed")
	}
	return nil
}

func (a *windowsLogonTaskAdapter) cellForRemoval(d ServiceDescriptor) (*Cell, error) {
	if a.removalCell != nil {
		return a.removalCell(d)
	}
	return New(Config{Root: d.Root, Timeout: 60 * time.Second})
}

func (a *windowsLogonTaskAdapter) Remove(ctx context.Context, d ServiceIdentity) error {
	if _, err := a.Inspect(ctx, d); err != nil {
		return err
	}
	// This existing transaction persists stopped intent, authenticates the child
	// drain and exit-host request, and observes the exact retained host and lease.
	if a.drain != nil {
		if err := a.drain(ctx, d); err != nil {
			return windowsTaskError(ErrServiceRemovalFailed, "remove:host_exit", 0, "host_exit_failed")
		}
	} else {
		cell, err := a.cellForRemoval(d)
		if err != nil || cell == nil || cell.exitServiceHostForRemoval(ctx) != nil {
			return windowsTaskError(ErrServiceRemovalFailed, "remove:host_exit", 0, "host_exit_failed")
		}
	}
	// The drain itself needs this operation lock, so acquire it only afterwards.
	unlock, err := a.mutationLock(d)
	if err != nil {
		return err
	}
	defer unlock()
	currentDescriptor, err := a.current(d)
	if err != nil || !reflect.DeepEqual(windowsTaskImmutable(currentDescriptor), windowsTaskImmutable(d)) || currentDescriptor.DesiredState != "stopped" {
		return windowsTaskError(ErrServiceOwnershipConflict, "remove:ownership", 0, "changed")
	}
	records, err := a.records(d)
	if err != nil {
		return err
	}
	// Query is immediately adjacent to the exact destructive manager command.
	// A matching label alone, an old Plan and an earlier XML snapshot never win.
	present, err := a.query(ctx, d)
	if err != nil {
		return err
	}
	if present {
		if !records.exists {
			return windowsTaskError(ErrServiceOwnershipConflict, "remove:ownership", 0, "missing")
		}
		name, _ := WindowsLogonTaskName(d)
		result, err := a.command(ctx, "/Delete", "/TN", name, "/F")
		if err != nil || result.Code != 0 {
			return windowsTaskError(ErrServiceRemovalFailed, "delete", result.Code, "delete_failed")
		}
	}
	present, err = a.query(ctx, d)
	if err != nil {
		return err
	}
	if present {
		return windowsTaskError(ErrServiceRemovalFailed, "remove:absence", 0, "still_present")
	}
	if !records.exists {
		return nil
	}
	current, err := a.records(d)
	if err != nil {
		return err
	}
	if !current.exists || !os.SameFile(records.xml, current.xml) || !os.SameFile(records.receipt, current.receipt) {
		return windowsTaskError(ErrServiceOwnershipConflict, "remove:ownership", 0, "changed")
	}
	if removePrivateFile(a.xmlPath(d)) != nil || removePrivateFile(a.receiptPath(d)) != nil || syncDirectory(a.directory(d)) != nil {
		return windowsTaskError(ErrServiceRemovalFailed, "remove:ownership", 0, "remove_failed")
	}
	return nil
}
