package localcell_test

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/witwave-ai/witself/internal/localcell"
)

// contenderErrorClass exposes only a fixed diagnostic vocabulary. Error text is
// used solely for equality with the private validators' fixed error strings.
func contenderErrorClass(err error, goos string) string {
	switch {
	case err == nil:
		return "nil"
	case errors.Is(err, localcell.ErrLocked):
		return "locked"
	case errors.Is(err, os.ErrNotExist):
		return "not_exist"
	case (goos == "windows" && errors.Is(err, syscall.Errno(32))) || err.Error() == "private Windows sharing-violation":
		return "sharing_violation"
	case (goos == "windows" && errors.Is(err, syscall.Errno(5))) || err.Error() == "private Windows access-denied":
		return "access_denied"
	case errors.Is(err, os.ErrPermission):
		return "permission"
	case err.Error() == "cannot inspect private local cell layout":
		return "layout_inspection"
	default:
		return "other"
	}
}

func TestContenderErrorClass(t *testing.T) {
	// Errno 5 also matches os.ErrPermission on Windows. With a non-Windows
	// diagnostic gate it must follow the portable permission branch there.
	nonWindowsAccess := "other"
	if errors.Is(syscall.Errno(5), os.ErrPermission) {
		nonWindowsAccess = "permission"
	}
	for _, tc := range []struct {
		name string
		err  error
		goos string
		want string
	}{
		{"nil", nil, "linux", "nil"},
		{"locked", localcell.ErrLocked, "linux", "locked"},
		{"wrapped_locked", fmt.Errorf("wrapped %w", localcell.ErrLocked), "windows", "locked"},
		{"not_exist", os.ErrNotExist, "linux", "not_exist"},
		{"wrapped_not_exist", fmt.Errorf("wrapped %w", os.ErrNotExist), "windows", "not_exist"},
		{"sharing_errno_windows", syscall.Errno(32), "windows", "sharing_violation"},
		{"wrapped_sharing_errno_windows", fmt.Errorf("wrapped %w", syscall.Errno(32)), "windows", "sharing_violation"},
		{"sharing_errno_linux", syscall.Errno(32), "linux", "other"},
		{"wrapped_sharing_errno_linux", fmt.Errorf("wrapped %w", syscall.Errno(32)), "linux", "other"},
		{"sharing_text_windows", errors.New("private Windows sharing-violation"), "windows", "sharing_violation"},
		{"sharing_text_linux", errors.New("private Windows sharing-violation"), "linux", "sharing_violation"},
		{"wrapped_sharing_text", fmt.Errorf("wrapped %w", errors.New("private Windows sharing-violation")), "windows", "other"},
		{"access_errno_windows", syscall.Errno(5), "windows", "access_denied"},
		{"wrapped_access_errno_windows", fmt.Errorf("wrapped %w", syscall.Errno(5)), "windows", "access_denied"},
		{"access_errno_linux", syscall.Errno(5), "linux", nonWindowsAccess},
		{"wrapped_access_errno_linux", fmt.Errorf("wrapped %w", syscall.Errno(5)), "linux", nonWindowsAccess},
		{"access_text_windows", errors.New("private Windows access-denied"), "windows", "access_denied"},
		{"access_text_linux", errors.New("private Windows access-denied"), "linux", "access_denied"},
		{"wrapped_access_text", fmt.Errorf("wrapped %w", errors.New("private Windows access-denied")), "windows", "other"},
		{"permission", os.ErrPermission, "windows", "permission"},
		{"wrapped_permission", fmt.Errorf("wrapped %w", os.ErrPermission), "linux", "permission"},
		{"layout_inspection", errors.New("cannot inspect private local cell layout"), "linux", "layout_inspection"},
		{"wrapped_layout_inspection", fmt.Errorf("wrapped %w", errors.New("cannot inspect private local cell layout")), "linux", "other"},
		{"other", errors.New("private diagnostic sentinel"), "windows", "other"},
		{"wrapped_other", fmt.Errorf("wrapped %w", errors.New("private diagnostic sentinel")), "linux", "other"},
		{"joined_locked_not_exist", errors.Join(localcell.ErrLocked, os.ErrNotExist), "windows", "locked"},
		{"joined_not_exist_locked", errors.Join(os.ErrNotExist, localcell.ErrLocked), "linux", "locked"},
		{"joined_locked_sharing", errors.Join(syscall.Errno(32), localcell.ErrLocked), "windows", "locked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if contenderErrorClass(tc.err, tc.goos) != tc.want {
				t.Fatal("contender error class differs")
			}
		})
	}
}

func TestContenderDiagnosticSourceContract(t *testing.T) {
	data, err := os.ReadFile("update_admission_lifecycle_test.go")
	if err != nil {
		t.Fatal("contender diagnostic source unavailable")
	}
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	const logText = `t.Logf("operation contention class: %s", contenderErrorClass(err, runtime.GOOS))`
	const fatalText = `t.Fatal("independent Start acquired the operation lock during readiness relaunch")`
	if strings.Count(source, logText) != 1 {
		t.Fatal("contender diagnostic must have exactly one fixed class log")
	}
	lines := strings.Split(source, "\n")
	logLines := 0
	for index, line := range lines {
		if strings.TrimSpace(line) != logText {
			continue
		}
		logLines++
		if index+1 == len(lines) || strings.TrimSpace(lines[index+1]) != fatalText {
			t.Fatal("contender diagnostic must immediately precede the unchanged fatal")
		}
	}
	if logLines != 1 {
		t.Fatal("contender diagnostic class log must occupy its own line")
	}
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, "update_admission_lifecycle_test.go", source, 0)
	if err != nil {
		t.Fatal("contender diagnostic source cannot be parsed")
	}
	text := func(node ast.Node) string {
		return source[positions.Position(node.Pos()).Offset:positions.Position(node.End()).Offset]
	}
	guardCount := 0
	ast.Inspect(file, func(node ast.Node) bool {
		guard, ok := node.(*ast.IfStmt)
		if !ok || len(guard.Body.List) != 2 || text(guard.Body.List[0]) != logText || text(guard.Body.List[1]) != fatalText {
			return true
		}
		guardCount++
		if text(guard.Cond) != "!errors.Is(err, localcell.ErrLocked)" {
			t.Fatal("contender diagnostic strict locked guard changed")
		}
		if guard.Init == nil || text(guard.Init) != "err := cell.Start(t.Context())" || guard.Else != nil {
			t.Fatal("contender diagnostic original start guard changed")
		}
		return true
	})
	if guardCount != 1 {
		t.Fatal("contender diagnostic log and fatal must share one original guard")
	}
}
