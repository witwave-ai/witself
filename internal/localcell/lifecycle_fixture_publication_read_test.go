package localcell_test

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"syscall"
	"testing"
	"time"
)

type fixturePublicationReadState string

const (
	fixturePublicationPresent   fixturePublicationReadState = "present"
	fixturePublicationAbsent    fixturePublicationReadState = "absent"
	fixturePublicationTransient fixturePublicationReadState = "transient"
	fixturePublicationFatal     fixturePublicationReadState = "fatal"
)

const (
	fixturePublicationExpiredReadFailure   = "pubhold-a08 empty pending record missing after gate expiry"
	fixturePublicationPrematureReadFailure = "publication gate exposed the final record before release"
	fixturePublicationPendingReadFailure   = "cannot read publication fixture pending record"
	fixturePublicationFinalReadFailure     = "publication fixture did not publish its empty final record"
	fixturePublicationRetainedReadFailure  = "publication fixture retained its pending name after rename"
)

// Pin the origin/main diagnostics independently of both the real readers and
// the injected fatal-path test, which share the constants above.
func TestFixturePublicationReadMessages(t *testing.T) {
	for _, test := range []struct {
		name, got, want string
	}{
		{"expired", fixturePublicationExpiredReadFailure, "pubhold-a08 empty pending record missing after gate expiry"},
		{"premature", fixturePublicationPrematureReadFailure, "publication gate exposed the final record before release"},
		{"pending", fixturePublicationPendingReadFailure, "cannot read publication fixture pending record"},
		{"final", fixturePublicationFinalReadFailure, "publication fixture did not publish its empty final record"},
		{"retained", fixturePublicationRetainedReadFailure, "publication fixture retained its pending name after rename"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("publication read diagnostic = %q, want origin/main text %q", test.got, test.want)
			}
		})
	}
}

// A helper-only fatal test cannot catch a caller changing its message argument.
// Require every real reader to use its pinned constant, in source order.
func TestFixturePublicationReadMessageSites(t *testing.T) {
	const filename = "lifecycle_fixture_completion_test.go"
	source, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, bytes.ReplaceAll(source, []byte("\r\n"), []byte("\n")), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		function string
		messages []string
	}{
		{"TestFixtureEmptyPostgresRecordPublication", []string{
			"fixturePublicationExpiredReadFailure",
		}},
		{"testFixtureEmptyPostgresRecordPublicationReleased", []string{
			"fixturePublicationPrematureReadFailure",
			"fixturePublicationPendingReadFailure",
			"fixturePublicationFinalReadFailure",
			"fixturePublicationRetainedReadFailure",
		}},
	} {
		t.Run(test.function, func(t *testing.T) {
			var function *ast.FuncDecl
			for _, declaration := range file.Decls {
				if candidate, ok := declaration.(*ast.FuncDecl); ok && candidate.Name.Name == test.function {
					function = candidate
					break
				}
			}
			if function == nil {
				t.Fatalf("%s:1: missing publication reader function %s", filename, test.function)
			}
			var calls []*ast.CallExpr
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "readFixturePublication" {
						calls = append(calls, call)
					}
				}
				return true
			})
			if len(calls) != len(test.messages) {
				t.Fatalf("%s: publication read calls = %d, want %d pinned sites", fset.Position(function.Pos()), len(calls), len(test.messages))
			}
			for i, call := range calls {
				if len(call.Args) != 5 {
					t.Fatalf("%s: publication reader arguments = %d, want 5", fset.Position(call.Pos()), len(call.Args))
				}
				message, ok := call.Args[4].(*ast.Ident)
				if !ok || message.Name != test.messages[i] {
					t.Errorf("%s: publication reader must use %s as its fatal message", fset.Position(call.Args[4].Pos()), test.messages[i])
				}
			}
		})
	}
}

type fixturePublicationFataler interface {
	Helper()
	Fatal(...any)
}

// readFixturePublication leaves retry timing to the caller's existing deadline.
// The errno predicate is injectable so Linux can exercise Windows retry paths
// without treating its unrelated errno values as Windows sharing failures.
func readFixturePublication(t fixturePublicationFataler, path string, read func(string) ([]byte, error), transient func(syscall.Errno) bool, failure string) ([]byte, fixturePublicationReadState) {
	t.Helper()
	data, err := read(path)
	if err == nil {
		return data, fixturePublicationPresent
	}
	if os.IsNotExist(err) {
		return nil, fixturePublicationAbsent
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && transient(errno) {
		return nil, fixturePublicationTransient
	}
	t.Fatal(failure)
	return nil, fixturePublicationFatal
}

// pollFixturePublication also handles the post-rename absence assertion: a
// present pending name is a failure, while a sharing error remains retryable.
func pollFixturePublication(t fixturePublicationFataler, deadline time.Time, want fixturePublicationReadState, read func() ([]byte, fixturePublicationReadState), pending func(), timeout string) []byte {
	t.Helper()
	for {
		data, state := read()
		if state == want {
			return data
		}
		if state == fixturePublicationFatal {
			return nil // Fatal already reported by read; also stop test recorders.
		}
		if state == fixturePublicationPresent {
			t.Fatal(timeout)
			return nil
		}
		if pending != nil {
			pending()
		}
		if !time.Now().Before(deadline) {
			t.Fatal(timeout)
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type fixturePublicationFatalRecorder struct {
	messages []string
}

func (*fixturePublicationFatalRecorder) Helper() {}
func (r *fixturePublicationFatalRecorder) Fatal(args ...any) {
	r.messages = append(r.messages, fmt.Sprint(args...))
}

func TestFixturePublicationReadClassification(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want fixturePublicationReadState
	}{
		{"present", nil, fixturePublicationPresent},
		{"absent", os.ErrNotExist, fixturePublicationAbsent},
		{"wrapped-absent", &os.PathError{Op: "open", Path: "record.pending", Err: os.ErrNotExist}, fixturePublicationAbsent},
		{"ERROR_ACCESS_DENIED", syscall.Errno(5), fixturePublicationPlatformErrnoState},
		{"ERROR_SHARING_VIOLATION", syscall.Errno(32), fixturePublicationPlatformErrnoState},
		{"ERROR_LOCK_VIOLATION", syscall.Errno(33), fixturePublicationPlatformErrnoState},
		{"wrapped-access", &os.PathError{Op: "open", Path: "record.pending", Err: syscall.Errno(5)}, fixturePublicationPlatformErrnoState},
		{"wrapped-sharing", fmt.Errorf("read: %w", &os.PathError{Op: "open", Path: "record.pending", Err: syscall.Errno(32)}), fixturePublicationPlatformErrnoState},
		{"wrapped-lock", &os.PathError{Op: "open", Path: "record.pending", Err: syscall.Errno(33)}, fixturePublicationPlatformErrnoState},
		{"other-errno", syscall.Errno(6), fixturePublicationFatal},
		{"plain-error", errors.New("read failed"), fixturePublicationFatal},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := &fixturePublicationFatalRecorder{}
			wantData := []byte("complete record")
			read := func(path string) ([]byte, error) {
				if path != "record.pending" {
					t.Fatal("publication reader changed the requested path")
				}
				return wantData, test.err
			}
			data, state := readFixturePublication(recorder, "record.pending", read, fixturePublicationTransientErrno, "cannot read publication fixture pending record")
			if state != test.want {
				t.Fatalf("classification = %s, want %s", state, test.want)
			}
			if state == fixturePublicationPresent {
				if !bytes.Equal(data, wantData) {
					t.Fatal("publication reader changed record bytes")
				}
			} else if data != nil {
				t.Fatal("failed publication read returned partial bytes")
			}
			if state == fixturePublicationFatal {
				if len(recorder.messages) != 1 || recorder.messages[0] != "cannot read publication fixture pending record" {
					t.Fatalf("fatal messages = %q, want existing pending-record message", recorder.messages)
				}
			} else if len(recorder.messages) != 0 {
				t.Fatalf("nonfatal read called Fatal: %q", recorder.messages)
			}
		})
	}
}

func TestFixturePublicationReadPolling(t *testing.T) {
	for _, errno := range []syscall.Errno{5, 32, 33} {
		t.Run(fmt.Sprintf("errno-%d", errno), func(t *testing.T) {
			recorder := &fixturePublicationFatalRecorder{}
			calls := 0
			want := []byte("published record\n")
			read := func(string) ([]byte, error) {
				calls++
				if calls <= 2 {
					return nil, &os.PathError{Op: "open", Path: "record.pending", Err: errno}
				}
				return want, nil
			}
			got := pollFixturePublication(recorder, time.Now().Add(time.Second), fixturePublicationPresent, func() ([]byte, fixturePublicationReadState) {
				return readFixturePublication(recorder, "record.pending", read, func(e syscall.Errno) bool { return e == errno }, "cannot read publication fixture pending record")
			}, nil, "publication fixture did not stage its pending record")
			if len(recorder.messages) != 0 {
				t.Fatalf("transient publication read called Fatal: %q", recorder.messages)
			}
			if calls != 3 || !bytes.Equal(got, want) {
				t.Fatalf("publication polling calls = %d, bytes = %q; want 3 and %q", calls, got, want)
			}
		})
	}
}

func TestFixturePublicationReadPollingFatal(t *testing.T) {
	for _, test := range []struct {
		name, failure string
		want          fixturePublicationReadState
	}{
		{"expired", fixturePublicationExpiredReadFailure, fixturePublicationPresent},
		{"premature", fixturePublicationPrematureReadFailure, fixturePublicationPresent},
		{"pending", fixturePublicationPendingReadFailure, fixturePublicationPresent},
		{"final", fixturePublicationFinalReadFailure, fixturePublicationPresent},
		{"retained", fixturePublicationRetainedReadFailure, fixturePublicationAbsent},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := &fixturePublicationFatalRecorder{}
			calls := 0
			read := func(string) ([]byte, error) {
				calls++
				return nil, errors.New("permanent read failure")
			}
			pollFixturePublication(recorder, time.Now().Add(time.Second), test.want, func() ([]byte, fixturePublicationReadState) {
				return readFixturePublication(recorder, "record.pending", read, fixturePublicationTransientErrno, test.failure)
			}, nil, "publication fixture did not stage its pending record")
			if calls != 1 || len(recorder.messages) != 1 || recorder.messages[0] != test.failure {
				t.Fatalf("fatal polling calls = %d, messages = %q; want one call with %q", calls, recorder.messages, test.failure)
			}
		})
	}
}

func TestFixturePublicationReadPollingDeadline(t *testing.T) {
	for _, state := range []fixturePublicationReadState{fixturePublicationAbsent, fixturePublicationTransient} {
		t.Run(string(state), func(t *testing.T) {
			recorder := &fixturePublicationFatalRecorder{}
			calls, pending := 0, 0
			pollFixturePublication(recorder, time.Now().Add(-time.Second), fixturePublicationPresent, func() ([]byte, fixturePublicationReadState) {
				calls++
				return nil, state
			}, func() { pending++ }, "publication fixture did not stage its pending record")
			if calls != 1 || pending != 1 || len(recorder.messages) != 1 || recorder.messages[0] != "publication fixture did not stage its pending record" {
				t.Fatal("publication polling did not honor the existing deadline and message")
			}
		})
	}
}

func TestFixturePublicationReadPollingAbsence(t *testing.T) {
	for _, final := range []fixturePublicationReadState{fixturePublicationAbsent, fixturePublicationPresent} {
		t.Run(string(final), func(t *testing.T) {
			recorder := &fixturePublicationFatalRecorder{}
			calls := 0
			pollFixturePublication(recorder, time.Now().Add(time.Second), fixturePublicationAbsent, func() ([]byte, fixturePublicationReadState) {
				calls++
				if calls == 1 {
					return nil, fixturePublicationTransient
				}
				return nil, final
			}, nil, "publication fixture retained its pending name after rename")
			if calls != 2 {
				t.Fatal("pending-name check did not retry its transient read")
			}
			if final == fixturePublicationAbsent {
				if len(recorder.messages) != 0 {
					t.Fatal("pending-name check rejected absence")
				}
			} else if len(recorder.messages) != 1 || recorder.messages[0] != "publication fixture retained its pending name after rename" {
				t.Fatal("pending-name check did not reject a retained record with its existing message")
			}
		})
	}
}
