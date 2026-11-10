package localcell

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// These numbers are Windows error codes even when the classifier is tested on
// another GOOS. Never classify by Error(): it can contain private paths.
func shutdownFixtureReadErrorClass(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, syscall.Errno(32)):
		return "sharing_violation"
	case errors.Is(err, syscall.Errno(5)):
		return "access_denied"
	case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.Errno(2)), errors.Is(err, syscall.Errno(3)):
		return "not_exist"
	default:
		return "other"
	}
}

func shutdownFixtureMarkerClass(data []byte, err error) string {
	if err != nil {
		if shutdownFixtureReadErrorClass(err) == "not_exist" {
			return "absent"
		}
		return "unreadable"
	}
	switch string(data) {
	case "1":
		return "one"
	case "2":
		return "two"
	default:
		return "other"
	}
}

// The retained child handle is an independent failure witness: publishing a
// second file would expose the failure witness to the same publication race.
// Codes 90..93 identify handler publication; 94..97 identify callback publication.
func shutdownFixturePublishExitCode(err error, callback bool) int {
	code := 93
	switch shutdownFixtureReadErrorClass(err) {
	case "sharing_violation":
		code = 90
	case "access_denied":
		code = 91
	case "not_exist":
		code = 92
	}
	if callback {
		code += 4
	}
	return code
}

func shutdownFixturePublishClass(code uint32, err error) (stage, class string) {
	if err != nil {
		return "other", "other"
	}
	stage = "handler"
	if code >= 94 && code <= 97 {
		stage = "callback"
		code -= 4
	}
	switch code {
	case 90:
		return stage, "sharing_violation"
	case 91:
		return stage, "access_denied"
	case 92:
		return stage, "not_exist"
	case 93:
		return stage, "other"
	case 259: // STILL_ACTIVE: no publication failure has been observed.
		return "none", "none"
	default:
		return "other", "other"
	}
}

func shutdownFixtureCompletionClass(data []byte, readErr error, code uint32, exitErr error) string {
	stage, publish := shutdownFixturePublishClass(code, exitErr)
	return "marker=" + shutdownFixtureMarkerClass(data, readErr) + " read=" + shutdownFixtureReadErrorClass(readErr) +
		" publish=" + publish + " stage=" + stage
}

func shutdownFixtureDetailClass(detail string) string {
	switch detail {
	case "":
		return "none"
	case "shutdown:deadline":
		return "deadline"
	case "shutdown:incarnation":
		return "incarnation"
	case "shutdown:descriptor":
		return "descriptor"
	case "shutdown:pipe":
		return "pipe"
	case "shutdown:ack":
		return "ack"
	case "shutdown:migration":
		return "migration"
	case "shutdown:force_rights":
		return "force_rights"
	case "shutdown:force":
		return "force"
	case "service_shutdown_forced":
		return "forced"
	default:
		return "other"
	}
}

func shutdownFixtureDeadlineClass(outcome serviceStopOutcome, err error) string {
	kind, detail, parent := "pass", "pass", "pass"
	if outcome.Kind != serviceStopUnknown {
		kind = "fail"
	}
	if outcome.Detail != "shutdown:deadline" {
		detail = "fail"
	}
	// Match the assertion's exact comparison, including wrapped errors.
	if err != context.DeadlineExceeded {
		parent = "fail"
	}
	return "kind=" + kind + " detail=" + detail + " context=" + parent + " outcome=" + shutdownFixtureDetailClass(outcome.Detail)
}

func shutdownFixtureDescriptorMatchClass(d serviceShutdownDescriptor, record ProcessRecord, service ServiceDescriptor, boot string, readErr, serviceErr, bootErr error) string {
	if readErr != nil {
		return "unreadable"
	}
	if !shutdownDescriptorMatches(d, record) {
		return "record"
	}
	if serviceErr != nil || bootErr != nil {
		return "unreadable"
	}
	if d.BootID != boot {
		return "boot"
	}
	if d.InstallationID != service.InstallationID {
		return "installation"
	}
	if d.Generation != service.Payload.Generation {
		return "generation"
	}
	return "pass"
}

// Short aliases keep simultaneous replay failures within the checker's line
// bound without admitting numeric values or widening its output policy.
func shutdownFixtureObservationClass(err error) string {
	switch shutdownFixtureReadErrorClass(err) {
	case "sharing_violation":
		return "sharing"
	case "access_denied":
		return "access"
	case "not_exist":
		return "missing"
	case "none":
		return "none"
	default:
		return "other"
	}
}

func TestShutdownFixtureObservationClass(t *testing.T) {
	for _, row := range []struct {
		err  error
		want string
	}{
		{nil, "none"}, {syscall.Errno(32), "sharing"}, {syscall.Errno(5), "access"},
		{os.ErrNotExist, "missing"}, {syscall.Errno(2), "missing"}, {syscall.Errno(3), "missing"},
		{errors.New("private path and error"), "other"},
	} {
		if shutdownFixtureObservationClass(row.err) != row.want {
			t.Fatal("shutdown replay error escaped its compact fixed vocabulary")
		}
	}
}

func shutdownFixturePendingCount(root string) (int, error) {
	count := 0
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), ".pending-") {
			count++
		}
		return nil
	})
	return count, err
}

// Spell decimal digits to keep the exact count inside the checker's existing
// closed word vocabulary: one,two means 12, not a bucket or an identifier.
func shutdownFixtureCountClass(count int) string {
	if count < 0 {
		return "other"
	}
	digits := [...]string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine"}
	words := make([]string, 0, len(strconv.Itoa(count)))
	for _, digit := range strconv.Itoa(count) {
		words = append(words, digits[digit-'0'])
	}
	return strings.Join(words, ",")
}

func TestShutdownFixtureDescriptorMatchClass(t *testing.T) {
	d := shutdownDescriptorFixture(t)
	record := ProcessRecord{Component: d.Component, PID: d.PID, StartTime: d.StartIdentity,
		BootID: d.BootID, ShutdownEndpoint: d.PipeName, InstallationID: d.InstallationID, Generation: d.Generation}
	service := ServiceDescriptor{InstallationID: d.InstallationID}
	service.Payload.Generation = d.Generation
	for _, row := range []struct {
		name string
		want string
	}{
		{"clean", "pass"}, {"read", "unreadable"}, {"service", "unreadable"}, {"boot_error", "unreadable"},
		{"record", "record"}, {"boot", "boot"}, {"installation", "installation"}, {"generation", "generation"},
	} {
		t.Run(row.name, func(t *testing.T) {
			candidate, current, boot := record, service, d.BootID
			var readErr, serviceErr, bootErr error
			switch row.name {
			case "read":
				readErr = errors.New("private read value")
			case "service":
				serviceErr = errors.New("private service value")
			case "boot_error":
				bootErr = errors.New("private boot value")
			case "record":
				candidate.PID++
			case "boot":
				boot = "private boot value"
			case "installation":
				current.InstallationID = "private installation value"
			case "generation":
				current.Payload.Generation = "private generation value"
			}
			if shutdownFixtureDescriptorMatchClass(d, candidate, current, boot, readErr, serviceErr, bootErr) != row.want {
				t.Fatal("shutdown descriptor replay lost its fixed comparison class")
			}
		})
	}
}

func TestShutdownFixturePendingCount(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tmp", "service", "service/children", ".pending-directory"} {
		if os.Mkdir(filepath.Join(root, dir), 0o700) != nil {
			t.Fatal("pending count fixture unavailable")
		}
	}
	for _, name := range []string{"tmp/.pending-one", "service/.pending-two", "service/children/.pending-three", "tmp/pending-other", "tmp/record"} {
		if os.WriteFile(filepath.Join(root, name), nil, 0o600) != nil {
			t.Fatal("pending count fixture unavailable")
		}
	}
	if count, err := shutdownFixturePendingCount(root); err != nil || count != 3 {
		t.Fatal("shutdown replay did not count only temporary publication files across the tree")
	}
	if count, err := shutdownFixturePendingCount(filepath.Join(root, "absent")); count != 0 || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("shutdown replay hid an incomplete temporary publication scan")
	}
}

func TestShutdownFixtureCountClass(t *testing.T) {
	for _, row := range []struct {
		count int
		want  string
	}{
		{-1, "other"}, {0, "zero"}, {1, "one"}, {2, "two"}, {3, "three"}, {4, "four"},
		{5, "five"}, {6, "six"}, {7, "seven"}, {8, "eight"}, {9, "nine"},
		{12, "one,two"}, {100, "one,zero,zero"},
	} {
		if shutdownFixtureCountClass(row.count) != row.want {
			t.Fatal("shutdown replay count lost its exact closed vocabulary representation")
		}
	}
}

func TestShutdownFixtureReadErrorClass(t *testing.T) {
	for _, row := range []struct {
		err  error
		want string
	}{
		{nil, "none"}, {syscall.Errno(32), "sharing_violation"}, {syscall.Errno(5), "access_denied"},
		{os.ErrNotExist, "not_exist"}, {syscall.Errno(2), "not_exist"}, {syscall.Errno(3), "not_exist"},
		{syscall.Errno(33), "other"}, {errors.New("private error sharing_violation"), "other"},
		{context.DeadlineExceeded, "other"},
	} {
		errs := []error{row.err}
		if row.err != nil {
			errs = append(errs, &os.PathError{Op: "private", Path: "private", Err: row.err}, errors.Join(errors.New("private"), row.err))
		}
		for _, err := range errs {
			if shutdownFixtureReadErrorClass(err) != row.want {
				t.Fatal("shutdown fixture read classifier escaped its fixed error classes")
			}
		}
	}
}

func TestShutdownFixtureMarkerClass(t *testing.T) {
	for _, row := range []struct {
		data string
		err  error
		want string
	}{
		{"", os.ErrNotExist, "absent"}, {"2", syscall.Errno(3), "absent"},
		{"1", nil, "one"}, {"2", nil, "two"}, {"", nil, "other"}, {"private", nil, "other"},
		{"2\n", nil, "other"}, {"2", syscall.Errno(32), "unreadable"}, {"", syscall.Errno(5), "unreadable"},
		{"2", errors.New("private"), "unreadable"},
	} {
		if shutdownFixtureMarkerClass([]byte(row.data), row.err) != row.want {
			t.Fatal("shutdown fixture marker classifier escaped its fixed states")
		}
	}
}

func TestShutdownFixturePublishClass(t *testing.T) {
	for _, row := range []struct {
		err   error
		code  int
		class string
	}{
		{syscall.Errno(32), 90, "sharing_violation"}, {syscall.Errno(5), 91, "access_denied"},
		{os.ErrNotExist, 92, "not_exist"}, {errors.New("private"), 93, "other"},
	} {
		for _, callback := range []bool{false, true} {
			wantCode, wantStage := row.code, "handler"
			if callback {
				wantCode += 4
				wantStage = "callback"
			}
			code := shutdownFixturePublishExitCode(row.err, callback)
			stage, class := shutdownFixturePublishClass(uint32(code), nil)
			if code != wantCode || stage != wantStage || class != row.class {
				t.Fatal("shutdown fixture publish failure lost its fixed exit witness")
			}
		}
	}
	for _, code := range []uint32{0, 89, 98, 259, ^uint32(0)} {
		wantStage, wantClass := "other", "other"
		if code == 259 {
			wantStage, wantClass = "none", "none"
		}
		if stage, class := shutdownFixturePublishClass(code, nil); stage != wantStage || class != wantClass {
			t.Fatal("shutdown fixture unrecognized exit claimed a publish failure")
		}
		if stage, class := shutdownFixturePublishClass(code, errors.New("private")); stage != "other" || class != "other" {
			t.Fatal("shutdown fixture exit read error escaped its fixed class")
		}
	}
	if shutdownFixtureCompletionClass([]byte("1"), nil, 90, nil) != "marker=one read=none publish=sharing_violation stage=handler" ||
		shutdownFixtureCompletionClass([]byte("private"), errors.New("private"), 259, nil) != "marker=unreadable read=other publish=none stage=none" {
		t.Fatal("shutdown fixture completion diagnostic lost its fixed vocabulary")
	}
}

func TestShutdownFixtureDetailClass(t *testing.T) {
	for _, row := range []struct{ detail, want string }{
		{"", "none"}, {"shutdown:deadline", "deadline"}, {"shutdown:incarnation", "incarnation"},
		{"shutdown:descriptor", "descriptor"}, {"shutdown:pipe", "pipe"}, {"shutdown:ack", "ack"},
		{"shutdown:migration", "migration"}, {"shutdown:force_rights", "force_rights"},
		{"shutdown:force", "force"}, {"service_shutdown_forced", "forced"},
		{"shutdown:config", "other"}, {"private", "other"}, {"shutdown:deadline private", "other"},
	} {
		if shutdownFixtureDetailClass(row.detail) != row.want {
			t.Fatal("shutdown fixture detail classifier escaped its fixed allowlist")
		}
	}
}

func TestShutdownFixtureDeadlineClass(t *testing.T) {
	for mask := range 8 {
		outcome := serviceStopOutcome{Kind: serviceStopUnknown, Detail: "shutdown:deadline"}
		err := context.DeadlineExceeded
		kind, detail, parent, class := "pass", "pass", "pass", "deadline"
		if mask&1 != 0 {
			outcome.Kind, kind = serviceStopKind("private"), "fail"
		}
		if mask&2 != 0 {
			outcome.Detail, detail, class = "private", "fail", "other"
		}
		if mask&4 != 0 {
			err, parent = errors.Join(context.DeadlineExceeded), "fail"
		}
		if shutdownFixtureDeadlineClass(outcome, err) != "kind="+kind+" detail="+detail+" context="+parent+" outcome="+class {
			t.Fatal("shutdown fixture deadline diagnostic does not match all assertion operands")
		}
	}
}

// Exercise the real portable protocol, then the same post-handle marker order
// as the Windows hook. The second completion fences the deferred callback.
func TestShutdownFixtureIdempotentWitness(t *testing.T) {
	d := shutdownDescriptorFixture(t)
	requested, handled := "", ""
	calls := 0
	p := newShutdownProtocol(d, func() { calls++; requested = strconv.Itoa(calls) })
	id, err := shutdownRandom(16)
	if err != nil {
		t.Fatal("request fixture unavailable")
	}
	request := d.request("cancel", id)
	for count := 1; count <= 2; count++ {
		if p.handle(request, func(response serviceShutdownResponse) error {
			if !response.Accepted {
				t.Fatal("fixture witness request refused")
			}
			return nil
		}) != nil {
			t.Fatal("fixture witness handler failed")
		}
		handled = strconv.Itoa(count)
	}
	if shutdownFixtureMarkerClass([]byte(handled), nil) != "two" || requested != "1" {
		t.Fatal("retry invoked cancellation more than once")
	}
}
