package localcell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func windowsTaskTestDescriptor() ServiceDescriptor {
	return ServiceDescriptor{Version: 1, InstallationID: "abc12345", UID: "S-1-5-21-100-200-300-1001", Root: `C:\Witself & Co\é`, Adapter: "windows-logon-task", Scope: "user", Runtime: "local", DesiredState: "running", ResourceProfile: "unconfigured", Revision: 1, Payload: ServicePayload{Host: `C:\Program Files\Witself\cell.exe`, Generation: "generation-1", Args: []string{"run", "--root", `C:\Witself & Co\é`}}}
}

func TestWindowsTaskXMLGolden(t *testing.T) {
	d := windowsTaskTestDescriptor()
	raw, err := RenderWindowsLogonTaskXML(d)
	if err != nil {
		t.Fatal("task renderer refused valid descriptor")
	}
	want, err := os.ReadFile("testdata/windows-task.xml")
	if err != nil {
		t.Fatal("task golden unavailable")
	}
	if !bytes.Equal(raw, want) {
		t.Fatal("task UTF16LE BOM golden changed")
	}
	if err := ValidateWindowsLogonTaskXML(raw, d); err != nil {
		t.Fatal("rendered task did not parse")
	}
	if len(windowsTaskBenignDefaults) != 0 {
		t.Fatal("uncaptured benign defaults were enrolled")
	}
}

func TestWindowsTaskXMLRoundTripAndStrictRefusals(t *testing.T) {
	d := windowsTaskTestDescriptor()
	raw, err := RenderWindowsLogonTaskXML(d)
	if err != nil {
		t.Fatal("task renderer failed")
	}
	decoded, err := decodeWindowsTaskXML(raw)
	if err != nil {
		t.Fatal("task fixture decode failed")
	}
	utf8XML := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>" + string(decoded)
	for _, variant := range []string{utf8XML, strings.ReplaceAll(utf8XML, "é", "&#233;"), strings.ReplaceAll(utf8XML, ">\n<", ">\r\n  <")} {
		if ValidateWindowsLogonTaskXML([]byte(variant), d) != nil {
			t.Fatal("equivalent XML representation refused")
		}
	}
	mutations := map[string]string{
		"DTD":                  strings.Replace(utf8XML, "<Task ", "<!DOCTYPE Task [<!ENTITY value SYSTEM 'file:///secret'>]><Task ", 1),
		"external_entity":      strings.Replace(utf8XML, "Witself local cell", "&foreign;", 1),
		"duplicate_singleton":  strings.Replace(utf8XML, "<Enabled>true</Enabled>", "<Enabled>true</Enabled><Enabled>true</Enabled>", 1),
		"duplicate_attribute":  strings.Replace(utf8XML, "version=\"1.2\"", "version=\"1.2\" version=\"1.2\"", 1),
		"namespace":            strings.Replace(utf8XML, windowsTaskNamespace, windowsTaskNamespace+"-foreign", 1),
		"extra_principal":      strings.Replace(utf8XML, "</Principals>", "<Principal id=\"Other\"><UserId>S-1-5-18</UserId></Principal></Principals>", 1),
		"extra_trigger":        strings.Replace(utf8XML, "</Triggers>", "<BootTrigger/></Triggers>", 1),
		"extra_action":         strings.Replace(utf8XML, "</Actions>", "<ComHandler/></Actions>", 1),
		"unknown_setting":      strings.Replace(utf8XML, "</Settings>", "<RestartOnFailure/></Settings>", 1),
		"highest":              strings.Replace(utf8XML, "LeastPrivilege", "HighestAvailable", 1),
		"s4u":                  strings.Replace(utf8XML, "InteractiveToken", "S4U", 1),
		"hard_terminate":       strings.Replace(utf8XML, "<AllowHardTerminate>false", "<AllowHardTerminate>true", 1),
		"runtime":              strings.Replace(utf8XML, "--runtime=local", "--runtime=hosted", 1),
		"different_host":       strings.Replace(utf8XML, "cell.exe", "foreign.exe", 1),
		"different_generation": strings.Replace(utf8XML, "generation-1", "generation-2", 1),
		"wrong_encoding":       strings.Replace(utf8XML, "UTF-8", "UTF-16", 1),
		"truncated":            utf8XML[:len(utf8XML)-20],
		"trailing_root":        utf8XML + "<Task/>",
		"comment":              strings.Replace(utf8XML, "<Task ", "<!--unrecognized--><Task ", 1),
		"extra_attribute":      strings.Replace(utf8XML, "<Exec>", "<Exec other=\"true\">", 1),
	}
	for name, mutated := range mutations {
		t.Run(name, func(t *testing.T) {
			if !errors.Is(ValidateWindowsLogonTaskXML([]byte(mutated), d), ErrServiceOwnershipConflict) {
				t.Fatal("unsafe or foreign XML accepted")
			}
		})
	}
	for _, bad := range [][]byte{raw[2:], append(append([]byte{}, raw...), 0), {0xff, 0xfe, 0x00, 0xd8}, {0xfe, 0xff, 0, 60}, bytes.Repeat([]byte("x"), windowsTaskXMLLimit+1)} {
		if ValidateWindowsLogonTaskXML(bad, d) == nil {
			t.Fatal("unverified encoding or unbounded XML accepted")
		}
	}
	d.Root = `C:\` + strings.Repeat("非ASCII", 300) + `\a&b`
	d.Payload.Host = `C:\` + strings.Repeat("long", 300) + `\cell.exe`
	d.Payload.Generation = `gen-\"-😀`
	raw, err = RenderWindowsLogonTaskXML(d)
	if err != nil || ValidateWindowsLogonTaskXML(raw, d) != nil {
		t.Fatal("long Unicode and escaped arguments failed round trip")
	}
}

func TestWindowsTaskXMLRejectsUnsafeDescriptor(t *testing.T) {
	mutations := map[string]func(*ServiceDescriptor){
		"hyphen_id": func(d *ServiceDescriptor) { d.InstallationID = "abc-12345" },
		"short_id":  func(d *ServiceDescriptor) { d.InstallationID = "short" }, "uppercase_id": func(d *ServiceDescriptor) { d.InstallationID = "Abc12345" },
		"long_id": func(d *ServiceDescriptor) { d.InstallationID = strings.Repeat("a", 33) }, "task_path": func(d *ServiceDescriptor) { d.InstallationID = `abc12345\evil` },
		"system": func(d *ServiceDescriptor) { d.UID = "S-1-5-18" }, "service": func(d *ServiceDescriptor) { d.UID = "S-1-5-19" },
		"unc": func(d *ServiceDescriptor) { d.Payload.Host = `\\server\cell.exe` }, "relative": func(d *ServiceDescriptor) { d.Root = `C:relative` },
		"parent_alias": func(d *ServiceDescriptor) { d.Root = `C:\one\..\two` }, "alternate_stream": func(d *ServiceDescriptor) { d.Payload.Host = `C:\cell.exe:stream` },
		"control": func(d *ServiceDescriptor) { d.Payload.Generation = "gen\n" }, "hosted": func(d *ServiceDescriptor) { d.Runtime = "hosted" },
		"scope": func(d *ServiceDescriptor) { d.Scope = "system" }, "adapter": func(d *ServiceDescriptor) { d.Adapter = "foreground" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			d := windowsTaskTestDescriptor()
			mutate(&d)
			if Render, err := RenderWindowsLogonTaskXML(d); err == nil || len(Render) != 0 {
				t.Fatal("unsafe task descriptor rendered")
			}
		})
	}
}

func TestWindowsTaskArgumentQuoting(t *testing.T) {
	for _, pair := range [][2]string{{"", `""`}, {"plain", `"plain"`}, {`a b`, `"a b"`}, {`a\`, `"a\\"`}, {`a\"b`, `"a\\\"b"`}} {
		if windowsTaskQuote(pair[0]) != pair[1] {
			t.Fatal("Windows task argument escaping changed")
		}
	}
}

type windowsTaskFake struct {
	t                                                              *testing.T
	a                                                              *windowsLogonTaskAdapter
	cell                                                           *Cell
	d                                                              ServiceDescriptor
	current                                                        ServiceDescriptor
	xml                                                            []byte
	calls                                                          [][]string
	queryCode                                                      uint32
	untyped                                                        bool
	createFailure, partialCreate, deleteFailure, retainAfterDelete bool
	afterDrain                                                     func()
	afterQuery                                                     func()
	afterDelete                                                    func()
	drained                                                        bool
}

func newWindowsTaskFake(t *testing.T) *windowsTaskFake {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "service")
	if makePrivateDir(dir) != nil {
		t.Fatal("private task fixture failed")
	}
	cell, _ := unitCell(t)
	lock, err := cell.own(false)
	if err != nil {
		t.Fatal("task fixture lock unavailable")
	}
	if err := lock.Close(); err != nil {
		t.Fatal("task fixture lock did not release")
	}
	f := &windowsTaskFake{t: t, cell: cell, d: windowsTaskTestDescriptor()}
	f.current = f.d
	f.a = &windowsLogonTaskAdapter{uid: f.d.UID, capability: func(context.Context) error { return nil }, readDescriptor: func(ServiceDescriptor) (ServiceDescriptor, error) { return f.current, nil }, recordDirectory: func(ServiceDescriptor) string { return dir }, drain: func(context.Context, ServiceDescriptor) error {
		f.drained = true
		f.current.DesiredState = "stopped"
		f.current.Revision++
		if f.afterDrain != nil {
			f.afterDrain()
		}
		return nil
	}}
	f.a.lock = func(ServiceDescriptor) (*Cell, func(), error) {
		lock, err := cell.own(false)
		if err != nil {
			return nil, nil, windowsTaskError(ErrServiceOwnershipConflict, "inspect:host_owner", 0, "unavailable")
		}
		return cell, func() { _ = lock.Close() }, nil
	}
	f.a.command = f.command
	return f
}
func (f *windowsTaskFake) command(ctx context.Context, args ...string) (windowsTaskCommandResult, error) {
	if err := ctx.Err(); err != nil {
		return windowsTaskCommandResult{}, err
	}
	f.calls = append(f.calls, append([]string{}, args...))
	name, _ := WindowsLogonTaskName(f.d)
	if len(args) < 3 || args[1] != "/TN" || args[2] != name {
		f.t.Fatal("task manager command escaped exact name")
	}
	result := windowsTaskCommandResult{Typed: !f.untyped}
	switch args[0] {
	case "/Query":
		if !reflect.DeepEqual(args, []string{"/Query", "/TN", name, "/XML"}) {
			f.t.Fatal("query argument contract changed")
		}
		if f.afterQuery != nil {
			f.afterQuery()
		}
		if f.queryCode != 0 {
			result.Code = f.queryCode
			return result, nil
		}
		if f.xml == nil {
			result.Code = 0x80070002
		} else {
			result.Output = append([]byte{}, f.xml...)
		}
	case "/Create":
		if !reflect.DeepEqual(args, []string{"/Create", "/TN", name, "/XML", f.a.xmlPath(f.d)}) {
			f.t.Fatal("create used force or alternate principal")
		}
		if f.xml != nil {
			result.Code = 0x800700b7
			return result, nil
		}
		if !f.createFailure || f.partialCreate {
			raw, err := readPrivateFile(f.a.xmlPath(f.d))
			if err != nil {
				f.t.Fatal("create missing private XML")
			}
			f.xml = raw
		}
		if f.createFailure {
			result.Code = 0x80070005
		}
	case "/Run":
		if !reflect.DeepEqual(args, []string{"/Run", "/TN", name}) {
			f.t.Fatal("run arguments changed")
		}
		if f.xml == nil {
			result.Code = 0x80070002
		}
	case "/Delete":
		if !reflect.DeepEqual(args, []string{"/Delete", "/TN", name, "/F"}) {
			f.t.Fatal("delete escaped exact force target")
		}
		if !f.drained {
			f.t.Fatal("task deleted before authenticated drain")
		}
		if f.deleteFailure {
			result.Code = 0x80070005
		} else if !f.retainAfterDelete {
			f.xml = nil
		}
		if f.afterDelete != nil {
			f.afterDelete()
		}
	default:
		f.t.Fatal("unexpected task manager command")
	}
	return result, nil
}
func (f *windowsTaskFake) count(verb string) int {
	count := 0
	for _, call := range f.calls {
		if call[0] == verb {
			count++
		}
	}
	return count
}
func (f *windowsTaskFake) apply() {
	f.t.Helper()
	plan, err := f.a.Plan(f.t.Context(), f.d)
	if err != nil || f.a.Apply(f.t.Context(), plan) != nil {
		f.t.Fatal("owned fake task registration failed")
	}
}

func TestWindowsTaskAdapterLifecycleAndExactRemoval(t *testing.T) {
	f := newWindowsTaskFake(t)
	f.apply()
	f.apply()
	if f.count("/Create") != 1 {
		t.Fatal("idempotent registration recreated task")
	}
	if f.a.Start(t.Context(), f.d) != nil || f.count("/Run") != 1 {
		t.Fatal("owned on demand start failed")
	}
	data := filepath.Join(filepath.Dir(f.a.directory(f.d)), "retained-data")
	if os.WriteFile(data, []byte("retained"), 0o600) != nil {
		t.Fatal("retention fixture failed")
	}
	if err := f.a.Remove(t.Context(), f.d); err != nil {
		t.Fatal("owned exact removal failed")
	}
	if f.count("/Delete") != 1 || f.current.DesiredState != "stopped" {
		t.Fatal("removal failed stop or exact delete")
	}
	for _, path := range []string{f.a.xmlPath(f.d), f.a.receiptPath(f.d)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("owned private task record retained")
		}
	}
	if raw, err := os.ReadFile(data); err != nil || string(raw) != "retained" {
		t.Fatal("task removal removed installation data")
	}
	if err := f.a.Remove(t.Context(), f.d); err != nil || f.count("/Delete") != 1 {
		t.Fatal("idempotent removal repeated deletion")
	}
	if err := f.a.Start(t.Context(), f.d); !errors.Is(err, ErrServiceDesiredState) {
		t.Fatal("durable stopped intent permitted restart")
	}
}

func TestWindowsTaskAdapterRefusesUntypedAndFailedQueries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    uint32
		untyped bool
		reason  string
	}{{"untyped", 0, true, "query_result_untyped"}, {"plain_exit", 1, false, "query_result_untyped"}, {"access_denied", 0x80070005, false, "access_denied"}, {"rpc", 0x800706ba, false, "rpc_unavailable"}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWindowsTaskFake(t)
			f.queryCode, f.untyped = tc.code, tc.untyped
			plan, err := f.a.Plan(t.Context(), f.d)
			if !errors.Is(err, ErrServiceManagerUnavailable) || !strings.Contains(err.Error(), "reason="+tc.reason) {
				t.Fatal("query classification lost fixed refusal")
			}
			if f.a.Apply(t.Context(), plan) == nil || f.a.Remove(t.Context(), f.d) == nil {
				t.Fatal("failed query permitted manager mutation")
			}
			if f.count("/Create")+f.count("/Delete")+f.count("/Run") != 0 {
				t.Fatal("unavailable query mutated task manager")
			}
		})
	}
	for _, raw := range [][]byte{nil, []byte("ERROR: The system cannot find the file specified."), []byte("Access is denied.")} {
		if _, err := classifyWindowsTaskQuery(windowsTaskCommandResult{Output: raw, Code: 1, Typed: true}, nil); !errors.Is(err, ErrServiceManagerUnavailable) {
			t.Fatal("English stderr classified task absence")
		}
	}
}

func TestWindowsTaskAdapterForeignAndStalePlan(t *testing.T) {
	t.Run("stale_after_staging", func(t *testing.T) {
		f := newWindowsTaskFake(t)
		plan, err := f.a.Plan(t.Context(), f.d)
		if err != nil {
			t.Fatal("plan failed")
		}
		f.afterQuery = func() {
			if f.count("/Query") == 3 {
				f.current.Revision++
			}
		}
		if err := f.a.Apply(t.Context(), plan); !errors.Is(err, ErrServiceOwnershipConflict) || f.count("/Create") != 0 {
			t.Fatal("descriptor changed after staging permitted create")
		}
	})
	t.Run("foreign_matching_xml", func(t *testing.T) {
		f := newWindowsTaskFake(t)
		f.xml, _ = RenderWindowsLogonTaskXML(f.d)
		if _, err := f.a.Plan(t.Context(), f.d); !errors.Is(err, ErrServiceOwnershipConflict) {
			t.Fatal("unreceipted foreign task adopted")
		}
	})
	t.Run("stale_plan", func(t *testing.T) {
		f := newWindowsTaskFake(t)
		plan, err := f.a.Plan(t.Context(), f.d)
		if err != nil {
			t.Fatal("plan failed")
		}
		f.current.Revision++
		if err := f.a.Apply(t.Context(), plan); !errors.Is(err, ErrServiceOwnershipConflict) || f.count("/Create") != 0 {
			t.Fatal("stale plan mutated manager")
		}
	})
	t.Run("foreign_after_plan", func(t *testing.T) {
		f := newWindowsTaskFake(t)
		plan, err := f.a.Plan(t.Context(), f.d)
		if err != nil {
			t.Fatal("plan failed")
		}
		f.xml, _ = RenderWindowsLogonTaskXML(f.d)
		if err := f.a.Apply(t.Context(), plan); !errors.Is(err, ErrServiceOwnershipConflict) || f.count("/Create") != 0 {
			t.Fatal("task appearing after plan was replaced")
		}
	})
	t.Run("truncated_query", func(t *testing.T) {
		f := newWindowsTaskFake(t)
		f.apply()
		f.xml = f.xml[:len(f.xml)/2]
		if f.a.Start(t.Context(), f.d) == nil || f.a.Remove(t.Context(), f.d) == nil || f.count("/Run")+f.count("/Delete") != 0 {
			t.Fatal("truncated task XML permitted mutation")
		}
	})
}

func TestWindowsTaskAdapterPartialCreateAndRemovalFailures(t *testing.T) {
	t.Run("stopped_task_start", func(t *testing.T) {
		f := newWindowsTaskFake(t)
		f.apply()
		f.current.DesiredState = "stopped"
		f.current.Revision++
		if !errors.Is(f.a.Start(t.Context(), f.d), ErrServiceDesiredState) || f.count("/Run") != 0 {
			t.Fatal("stopped registered task permitted demand start")
		}
	})
	t.Run("descriptor_changed_during_drain", func(t *testing.T) {
		f := newWindowsTaskFake(t)
		f.apply()
		f.afterDrain = func() { f.current.Payload.Generation = "foreign-generation" }
		if !errors.Is(f.a.Remove(t.Context(), f.d), ErrServiceOwnershipConflict) || f.count("/Delete") != 0 {
			t.Fatal("descriptor changed during drain permitted deletion")
		}
	})
	t.Run("record_replaced_after_delete", func(t *testing.T) {
		f := newWindowsTaskFake(t)
		f.apply()
		f.afterDelete = func() {
			raw, err := RenderWindowsLogonTaskXML(f.d)
			if err != nil || writeAtomicBytes(f.a.xmlPath(f.d), raw, nil) != nil {
				t.Fatal("replacement fixture failed")
			}
		}
		if !errors.Is(f.a.Remove(t.Context(), f.d), ErrServiceOwnershipConflict) {
			t.Fatal("replaced private XML was removed")
		}
		if _, err := os.Stat(f.a.xmlPath(f.d)); err != nil {
			t.Fatal("replacement private XML did not survive")
		}
	})
	t.Run("partial_create", func(t *testing.T) {
		f := newWindowsTaskFake(t)
		f.createFailure, f.partialCreate = true, true
		plan, err := f.a.Plan(t.Context(), f.d)
		if err != nil {
			t.Fatal("plan failed")
		}
		if !errors.Is(f.a.Apply(t.Context(), plan), ErrServiceRegistrationFailed) || f.xml == nil {
			t.Fatal("partial create failure was hidden")
		}
		f.createFailure = false
		f.apply()
		if f.count("/Create") != 1 {
			t.Fatal("partial owned create was overwritten")
		}
	})
	t.Run("foreign_before_delete", func(t *testing.T) {
		f := newWindowsTaskFake(t)
		f.apply()
		f.afterDrain = func() {
			other := f.d
			other.Payload.Generation = "foreign"
			f.xml, _ = RenderWindowsLogonTaskXML(other)
		}
		if !errors.Is(f.a.Remove(t.Context(), f.d), ErrServiceOwnershipConflict) || f.count("/Delete") != 0 {
			t.Fatal("task replaced during drain was deleted")
		}
	})
	t.Run("drain_failure", func(t *testing.T) {
		f := newWindowsTaskFake(t)
		f.apply()
		f.a.drain = func(context.Context, ServiceDescriptor) error { return errors.New("fixture drain failure") }
		if f.a.Remove(t.Context(), f.d) == nil || f.count("/Delete") != 0 {
			t.Fatal("failed host drain permitted deletion")
		}
	})
	t.Run("absence_required", func(t *testing.T) {
		f := newWindowsTaskFake(t)
		f.apply()
		f.retainAfterDelete = true
		if err := f.a.Remove(t.Context(), f.d); !errors.Is(err, ErrServiceRemovalFailed) || !strings.Contains(err.Error(), "remove:absence") {
			t.Fatal("delete success without absence was accepted")
		}
		if _, err := os.Stat(f.a.receiptPath(f.d)); err != nil {
			t.Fatal("unproven absence removed receipt")
		}
	})
	t.Run("delete_failed", func(t *testing.T) {
		f := newWindowsTaskFake(t)
		f.apply()
		f.deleteFailure = true
		if !errors.Is(f.a.Remove(t.Context(), f.d), ErrServiceRemovalFailed) {
			t.Fatal("delete failure hidden")
		}
		if _, err := os.Stat(f.a.xmlPath(f.d)); err != nil {
			t.Fatal("delete failure removed XML custody")
		}
	})
	t.Run("private_record_changed", func(t *testing.T) {
		f := newWindowsTaskFake(t)
		f.apply()
		if writeAtomicBytes(f.a.xmlPath(f.d), []byte("foreign"), nil) != nil {
			t.Fatal("record mutation fixture failed")
		}
		if f.a.Remove(t.Context(), f.d) == nil || f.count("/Delete") != 0 {
			t.Fatal("changed private XML was ignored")
		}
	})
}

func TestWindowsTaskAdapterMutationLockOrdering(t *testing.T) {
	f := newWindowsTaskFake(t)
	held := false
	lock := f.a.lock
	f.a.lock = func(d ServiceDescriptor) (*Cell, func(), error) {
		if held {
			t.Fatal("task mutation lock nested")
		}
		cell, unlock, err := lock(d)
		if err != nil {
			return nil, nil, err
		}
		held = true
		return cell, func() { unlock(); held = false }, nil
	}
	command := f.a.command
	f.a.command = func(ctx context.Context, args ...string) (windowsTaskCommandResult, error) {
		if args[0] != "/Query" && !held {
			t.Fatal("manager mutation escaped operation lock")
		}
		return command(ctx, args...)
	}
	drain := f.a.drain
	f.a.drain = func(ctx context.Context, d ServiceDescriptor) error {
		if held {
			t.Fatal("host drain held operation lock")
		}
		return drain(ctx, d)
	}
	f.apply()
	if f.a.Start(t.Context(), f.d) != nil || f.a.Remove(t.Context(), f.d) != nil || held {
		t.Fatal("mutation transaction lock ordering failed")
	}
}
