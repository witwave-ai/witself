package localcell

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsPrivateTreeSnapshotRetriesVanishedLeaf(t *testing.T) {
	root := t.TempDir()
	transient := filepath.Join(root, "a-pending")
	stable := filepath.Join(root, "z-record")
	for _, path := range []string{transient, stable} {
		if os.WriteFile(path, nil, 0o600) != nil {
			t.Fatal("cannot create private tree snapshot fixture")
		}
	}
	passes, stableChecks := 0, 0
	walk := func(root string, visit fs.WalkDirFunc) error {
		passes++
		return filepath.WalkDir(root, visit)
	}
	validate := func(path string, directory bool) error {
		if path == transient && passes == 1 {
			if directory || os.Remove(path) != nil {
				t.Fatal("cannot remove enumerated transient fixture")
			}
			// Model the value-free Windows error retaining its missing cause.
			return fmt.Errorf("private object disappeared: %w", os.ErrNotExist)
		}
		if path == stable {
			stableChecks++
		}
		return nil
	}
	if err := validatePrivateTreeSnapshot(root, walk, validate, os.Lstat); err != nil || passes != 2 || stableChecks != 1 {
		t.Fatal("vanished leaf did not require a successful complete replacement pass")
	}
}

func TestWindowsPrivateTreeSnapshotRevalidatesReplacementPolicy(t *testing.T) {
	for _, policy := range []string{"inherited_dacl", "foreign_owner"} {
		t.Run(policy, func(t *testing.T) {
			root := t.TempDir()
			transient := filepath.Join(root, "a-pending")
			if os.WriteFile(transient, nil, 0o600) != nil {
				t.Fatal("cannot create transient fixture")
			}
			passes, rootChecks, replacementChecks := 0, 0, 0
			protected, userOwner := true, true
			refused := errors.New("synthetic private descriptor refused")
			walk := func(root string, visit fs.WalkDirFunc) error {
				passes++
				return filepath.WalkDir(root, visit)
			}
			validate := func(path string, directory bool) error {
				if path == root {
					rootChecks++
				}
				if path != transient {
					return nil
				}
				if passes == 1 {
					// A runner-created replacement has an inherited DACL or a
					// foreign owner in the fake policy. The complete new pass
					// must apply the unchanged validator to that replacement.
					if directory || os.Remove(path) != nil || os.WriteFile(path, nil, 0o600) != nil {
						t.Fatal("cannot replace enumerated transient fixture")
					}
					protected, userOwner = policy != "inherited_dacl", policy != "foreign_owner"
					return os.ErrNotExist
				}
				replacementChecks++
				if !protected || !userOwner {
					return refused
				}
				return nil
			}
			if err := validatePrivateTreeSnapshot(root, walk, validate, os.Lstat); !errors.Is(err, refused) || passes != 2 || rootChecks != 3 || replacementChecks != 1 {
				t.Fatal("snapshot retry bypassed replacement or parent descriptor validation")
			}
		})
	}
}

func TestWindowsPrivateTreeSnapshotRetryBoundary(t *testing.T) {
	for _, test := range []struct {
		name       string
		wantPasses int
	}{
		{"missing_root", 1},
		{"missing_directory", 1},
		{"missing_parent", 1},
		{"parent_privacy", 1},
		{"leaf_privacy", 1},
		{"walk_error", 1},
		{"churn_budget", 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			directory := filepath.Join(root, "directory")
			leaf := filepath.Join(directory, "pending")
			if os.Mkdir(directory, 0o700) != nil || os.WriteFile(leaf, nil, 0o600) != nil {
				t.Fatal("cannot create snapshot boundary fixture")
			}
			passes := 0
			refused := errors.New("synthetic private descriptor refused")
			walk := func(root string, visit fs.WalkDirFunc) error {
				passes++
				if test.name == "walk_error" {
					return visit(root, nil, os.ErrPermission)
				}
				return filepath.WalkDir(root, visit)
			}
			leafSeen := false
			validate := func(path string, _ bool) error {
				if test.name == "missing_root" && path == root || test.name == "missing_directory" && path == directory {
					return os.ErrNotExist
				}
				if test.name == "parent_privacy" && path == directory && leafSeen {
					return refused
				}
				if path == leaf {
					leafSeen = true
					if test.name == "leaf_privacy" {
						return refused
					}
					return os.ErrNotExist
				}
				return nil
			}
			stat := func(path string) (os.FileInfo, error) {
				if test.name == "missing_parent" {
					return nil, os.ErrNotExist
				}
				return os.Lstat(path)
			}
			err := validatePrivateTreeSnapshot(root, walk, validate, stat)
			if err == nil || passes != test.wantPasses {
				t.Fatal("snapshot retry accepted a refusal or crossed its fixed boundary")
			}
			if (test.name == "parent_privacy" || test.name == "leaf_privacy") && !errors.Is(err, refused) {
				t.Fatal("snapshot retry replaced a privacy refusal")
			}
		})
	}
}

func TestWindowsPrivateTreeSnapshotWiring(t *testing.T) {
	source, err := os.ReadFile("files_windows.go")
	if err != nil {
		t.Fatal("cannot read Windows private tree source")
	}
	file, err := parser.ParseFile(token.NewFileSet(), "files_windows.go", strings.ReplaceAll(string(source), "\r\n", "\n"), 0)
	if err != nil {
		t.Fatal("cannot parse Windows private tree source")
	}
	selector := func(expr ast.Expr, pkg, name string) bool {
		value, ok := expr.(*ast.SelectorExpr)
		if !ok || value.Sel.Name != name {
			return false
		}
		base, ok := value.X.(*ast.Ident)
		return ok && base.Name == pkg
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "validatePrivateTree" || function.Body == nil {
			continue
		}
		for _, statement := range function.Body.List {
			returned, ok := statement.(*ast.ReturnStmt)
			if !ok || len(returned.Results) != 1 {
				continue
			}
			call, ok := returned.Results[0].(*ast.CallExpr)
			if !ok || len(call.Args) != 4 {
				continue
			}
			name, ok := call.Fun.(*ast.Ident)
			if !ok || name.Name != "validatePrivateTreeSnapshot" || !selector(call.Args[1], "filepath", "WalkDir") || !selector(call.Args[3], "os", "Lstat") {
				continue
			}
			validator, ok := call.Args[2].(*ast.FuncLit)
			if !ok {
				t.Fatal("Windows snapshot lost its exact per-object validator")
			}
			strict, postgres := false, false
			ast.Inspect(validator, func(node ast.Node) bool {
				invoke, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if name, ok := invoke.Fun.(*ast.Ident); ok && name.Name == "validatePrivate" {
					strict = true
				}
				postgres = postgres || selector(invoke.Fun, "runtimeconfig", "WindowsPrivatePostgresValidate")
				return true
			})
			if strict && postgres {
				return
			}
		}
	}
	t.Fatal("Windows tree validation bypasses bounded snapshots or an unchanged object policy")
}
