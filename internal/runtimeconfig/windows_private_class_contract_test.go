package runtimeconfig

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

func TestWindowsPrivateClassSourceContract(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// The Windows checkout may contain CRLF, including mutation sites.
		return strings.ReplaceAll(string(data), "\r\n", "\n")
	}
	windowsSource := read("windows_private_class.go")
	otherSource := read("windows_private_class_other.go")
	if err := windowsPrivateClassContract(windowsSource, otherSource); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct {
		name string
		stub bool
		old  string
		new  string
	}{
		{"sharing_to_validation", false, "case errWindowsPrivateSharing:\n\t\treturn \"sharing_violation\"", "case errWindowsPrivateSharing:\n\t\treturn \"validation\""},
		{"access_to_other", false, "case errWindowsPrivateAccess:\n\t\treturn \"access_denied\"", "case errWindowsPrivateAccess:\n\t\treturn \"other\""},
		{"stub_transient_true", true, "func WindowsPrivateTransient(error) bool { return false }", "func WindowsPrivateTransient(error) bool { return true }"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			win, other := windowsSource, otherSource
			target := &win
			if mutation.stub {
				target = &other
			}
			if strings.Count(*target, mutation.old) != 1 {
				t.Fatal("mutation must replace exactly one source site")
			}
			*target = strings.Replace(*target, mutation.old, mutation.new, 1)
			if err := windowsPrivateClassContract(win, other); err == nil {
				t.Fatal("source contract accepted the mutant")
			}
		})
	}
}

func windowsPrivateClassContract(windowsSource, otherSource string) error {
	const expectedWindows = `package runtimeconfig
func WindowsPrivateFailureClass(err error) string {
	var private windowsPrivateError
	if !errors.As(err, &private) {
		return "none"
	}
	switch private.reason {
	case errWindowsPrivateSharing:
		return "sharing_violation"
	case errWindowsPrivateAccess:
		return "access_denied"
	case errWindowsPrivateObject, errWindowsPrivateOwnerAdmin, errWindowsPrivateOwner,
		errWindowsPrivateACECount, errWindowsPrivateACEMask, errWindowsPrivateACEFlags,
		errWindowsPrivateProtected, errWindowsPrivateSystem:
		return "validation"
	default:
		return "other"
	}
}
func WindowsPrivateTransient(err error) bool {
	class := WindowsPrivateFailureClass(err)
	return class == "sharing_violation" || class == "access_denied"
}`
	const expectedOther = `package runtimeconfig
func WindowsPrivateFailureClass(error) string { return "none" }
func WindowsPrivateTransient(error) bool { return false }
`
	for _, source := range []struct {
		name string
		text string
		want string
		tag  string
	}{
		{"windows", windowsSource, expectedWindows, "//go:build windows\n"},
		{"other", otherSource, expectedOther, "//go:build !windows\n"},
	} {
		if !strings.HasPrefix(source.text, source.tag) {
			return fmt.Errorf("%s classifier build tag changed", source.name)
		}
		got, err := windowsPrivateClassFunctions(source.text)
		if err != nil {
			return fmt.Errorf("%s classifier parse: %w", source.name, err)
		}
		want, err := windowsPrivateClassFunctions(source.want)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("%s classifier mapping or closed vocabulary changed", source.name)
		}
	}
	return nil
}

func windowsPrivateClassFunctions(source string) (string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "classifier.go", source, 0)
	if err != nil {
		return "", err
	}
	var result bytes.Buffer
	for _, decl := range file.Decls {
		function, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if err := format.Node(&result, fset, function); err != nil {
			return "", err
		}
		result.WriteByte('\n')
	}
	return result.String(), nil
}
