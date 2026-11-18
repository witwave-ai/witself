package localcell

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// referencesSystems are the systems the package references contract checks
// every non-test file under (GOARCH amd64, cgo off), in a list line's order.
var referencesSystems = [...]string{"darwin", "freebsd", "linux", "solaris", "windows"}

// referencesListed is one package of a system's go list -deps output.
type referencesListed struct {
	ImportPath, Dir string
	GoFiles         []string
	ImportMap       map[string]string
}

// referencesWorld is one system's dependencies: listed once per test process,
// each type-checked from source without bodies on its first import, and used
// by one goroutine at a time.
type referencesWorld struct {
	sync.Mutex
	failed   bool
	self     string
	listed   map[string]*referencesListed
	context  build.Context
	fset     *token.FileSet
	packages map[string]*types.Package
}

var referencesWorlds [len(referencesSystems)]referencesWorld

// referencesView is one system's check: the files it compiles, its keys, the
// guarded names it resolves, and each declaration's count of references to
// each guarded object, keyed file, declaration and object.
type referencesView struct {
	compiled, keys, resolved map[string]bool
	counts                   map[string]int
}

// load lists the package's dependencies for system with the go command, its
// error output discarded; a failure leaves the world failed.
func (w *referencesWorld) load(system string) {
	w.listed, w.fset, w.packages = map[string]*referencesListed{}, token.NewFileSet(), map[string]*types.Package{}
	command := exec.Command("go", "list", "-deps", "-json=ImportPath,Dir,GoFiles,ImportMap", ".")
	command.Env, command.Stderr = append(os.Environ(), "GOOS="+system, "GOARCH=amd64", "CGO_ENABLED=0"), io.Discard
	output, err := command.Output()
	for decoder := json.NewDecoder(bytes.NewReader(output)); err == nil; {
		listed := &referencesListed{}
		if err = decoder.Decode(listed); err == nil {
			w.listed[listed.ImportPath], w.self = listed, listed.ImportPath
		}
	}
	// A JoinPath keeps go/build from running the go command itself.
	w.context = build.Default
	w.context.GOOS, w.context.GOARCH, w.context.CgoEnabled, w.context.JoinPath = system, "amd64", false, filepath.Join
	if runtime := w.listed["runtime"]; runtime != nil {
		w.context.GOROOT = filepath.Dir(filepath.Dir(runtime.Dir))
	}
	w.failed = err != io.EOF || w.listed[w.self] == nil || w.listed["runtime"] == nil
}

// importer imports for a package with importMap: a listed package, else one of
// the standard library, type-checked once; never the package under check.
func (w *referencesWorld) importer(importMap map[string]string, dir string) types.Importer {
	return readers4Importer(func(path string) (*types.Package, error) {
		path, refused := cmp.Or(importMap[path], path), errors.New("import not admitted")
		listed := w.listed[path]
		if path == "unsafe" {
			return types.Unsafe, nil
		} else if listed == nil {
			if found, err := w.context.Import(path, dir, 0); err == nil && found.Goroot {
				listed = &referencesListed{ImportPath: found.ImportPath, Dir: found.Dir, GoFiles: found.GoFiles}
			}
		}
		if listed == nil || listed.ImportPath == w.self {
			return nil, refused
		} else if loaded, seen := w.packages[listed.ImportPath]; seen && loaded != nil {
			return loaded, nil
		} else if seen {
			return nil, refused
		}
		w.packages[listed.ImportPath] = nil // a cycle, or a package that fails, is refused
		files, failed := []*ast.File{}, false
		for _, name := range listed.GoFiles {
			file, err := parser.ParseFile(w.fset, filepath.Join(listed.Dir, name), nil, parser.SkipObjectResolution)
			if err != nil {
				return nil, refused
			}
			files = append(files, file)
		}
		config := types.Config{Importer: w.importer(listed.ImportMap, listed.Dir), IgnoreFuncBodies: true,
			Sizes: types.SizesFor("gc", "amd64"), Error: func(error) { failed = true }}
		if loaded, _ := config.Check(listed.ImportPath, w.fset, files, nil); !failed {
			w.packages[listed.ImportPath] = loaded
			return loaded, nil
		}
		return nil, refused
	})
}

// resolve returns the object a guarded name spells on this system: a member of
// the checked package bare, any other by import path, a method as (T).m or
// (*T).m of the type that declares it.
func (w *referencesWorld) resolve(self *types.Package, name string) types.Object {
	receiver, method, _ := strings.Cut(strings.TrimPrefix(name, "("), ").")
	member, scope := strings.TrimPrefix(receiver, "*"), self.Scope()
	if dot := strings.LastIndex(member, "."); dot >= 0 {
		imported, err := w.importer(nil, "").Import(member[:dot])
		if err != nil {
			return nil
		}
		scope, member = imported.Scope(), member[dot+1:]
	}
	object := scope.Lookup(member)
	if typeName, ok := object.(*types.TypeName); ok && method != "" {
		found, _, _ := types.LookupFieldOrMethod(typeName.Type(), true, typeName.Pkg(), method)
		if function, ok := found.(*types.Func); ok && types.TypeString(function.Signature().Recv().Type(), types.RelativeTo(self)) == receiver {
			return function
		}
		return nil
	} else if method != "" {
		return nil
	}
	return object
}

// check type-checks the sources this system compiles, chosen by its build
// context from the sources themselves, and returns its view.
func (w *referencesWorld) check(system string, sources map[string]string, guarded []string) referencesView {
	w.Lock()
	defer w.Unlock()
	if w.listed == nil {
		w.load(system)
	}
	view := referencesView{compiled: map[string]bool{}, keys: map[string]bool{}, resolved: map[string]bool{}, counts: map[string]int{}}
	fail := func(key string, bad bool) bool { view.keys[key] = view.keys[key] || bad; return bad }
	context, fset, files := w.context, token.NewFileSet(), []*ast.File{}
	context.OpenFile = func(path string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(sources[path])), nil }
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		if match, err := context.MatchFile(".", name); !fail("parse:"+name, err != nil) && match {
			file, err := parser.ParseFile(fset, name, sources[name], parser.ParseComments|parser.SkipObjectResolution)
			if !fail("parse:"+name, err != nil) {
				view.compiled[name], files = true, append(files, file)
			}
		}
	}
	if fail("world:"+system, w.failed) {
		return view
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}}
	config := types.Config{Importer: w.importer(w.listed[w.self].ImportMap, w.listed[w.self].Dir), Sizes: types.SizesFor("gc", "amd64"),
		Error: func(err error) { fail("types:"+system+":"+fset.Position(err.(types.Error).Pos).Filename, true) }}
	self, _ := config.Check(w.self, fset, files, info)
	// objects are the guarded names each object stands for: a guarded object its
	// own, and a package variable or field each guarded name given to it.
	objects, concrete := map[types.Object][]string{}, []*types.Func{}
	for _, name := range guarded {
		object := w.resolve(self, name)
		if method, ok := object.(*types.Func); ok && method.Signature().Recv() != nil && !types.IsInterface(method.Signature().Recv().Type()) {
			concrete = append(concrete, method)
		}
		if object != nil {
			objects[object], view.resolved[name] = append(objects[object], name), true
		}
	}
	// targets adds, for an interface method, each guarded concrete method of its
	// name and identical signature (receivers are not compared, as
	// go/types/predicates.go:337-385 shows).
	targets := func(object types.Object) []string {
		if function, ok := object.(*types.Func); ok {
			object = function.Origin()
		} else if variable, ok := object.(*types.Var); ok {
			object = variable.Origin()
		}
		names := slices.Clone(objects[object])
		if method, ok := object.(*types.Func); ok && method.Signature().Recv() != nil && types.IsInterface(method.Signature().Recv().Type()) {
			for _, guard := range concrete {
				if guard.Name() == method.Name() && types.Identical(guard.Signature(), method.Signature()) {
					names = append(names, objects[guard]...)
				}
			}
		}
		return names
	}
	// A value is given by an initializer, an assignment or a composite-literal
	// key; only a package variable or a field takes it, never a local.
	given := map[ast.Expr]types.Object{}
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.ValueSpec:
				for index, value := range node.Values {
					if len(node.Names) == len(node.Values) {
						given[value] = info.Defs[node.Names[index]]
					}
				}
			case *ast.AssignStmt:
				for index, value := range node.Rhs {
					if len(node.Lhs) == len(node.Rhs) {
						given[value] = referencesNamed(info, node.Lhs[index])
					}
				}
			case *ast.KeyValueExpr:
				if field, ok := referencesNamed(info, node.Key).(*types.Var); ok && field.Kind() == types.FieldVar {
					given[node.Value] = field
				}
			}
			return true
		})
	}
	for changed := true; changed; {
		changed = false
		for value, to := range given {
			variable, ok := to.(*types.Var)
			for _, target := range targets(referencesNamed(info, value)) {
				if ok && (variable.Kind() == types.PackageVar || variable.Kind() == types.FieldVar) && !slices.Contains(objects[variable.Origin()], target) {
					objects[variable.Origin()], changed = append(objects[variable.Origin()], target), true
				}
			}
		}
	}
	for _, file := range files {
		name := fset.Position(file.Package).Filename
		for _, group := range file.Comments {
			for _, comment := range group.List {
				fail("linkname:"+name, strings.HasPrefix(comment.Text, "//go:linkname"))
			}
		}
		for _, declaration := range file.Decls {
			referencesDeclarations(declaration, func(declared string, node ast.Node) {
				function, ok := node.(*ast.FuncDecl)
				fail("bodyless:"+name+":"+declared, ok && function.Body == nil)
				ast.Inspect(node, func(node ast.Node) bool {
					identifier, _ := node.(*ast.Ident)
					for _, target := range targets(info.Uses[identifier]) {
						view.counts[name+"\t"+declared+"\t"+target]++
					}
					return true
				})
			})
		}
	}
	return view
}

// referencesNamed returns the object an expression names: an identifier, or a
// selector's member, through parentheses, instantiation and indexing.
func referencesNamed(info *types.Info, expression ast.Expr) types.Object {
	switch node := ast.Unparen(expression).(type) {
	case *ast.Ident:
		return info.Uses[node]
	case *ast.SelectorExpr:
		return info.Uses[node.Sel]
	case *ast.IndexExpr:
		return referencesNamed(info, node.X)
	case *ast.IndexListExpr:
		return referencesNamed(info, node.X)
	}
	return nil
}

// referencesDeclarations visits a top-level declaration's parts by name: a
// function, a method as (T).m or (*T).m, each value or type specification by
// its first name.
func referencesDeclarations(declaration ast.Decl, visit func(string, ast.Node)) {
	if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv.NumFields() > 0 {
		receiver, _, _ := strings.Cut(types.ExprString(function.Recv.List[0].Type), "[")
		visit("("+receiver+")."+function.Name.Name, function)
	} else if ok {
		visit(function.Name.Name, function)
	} else if general, ok := declaration.(*ast.GenDecl); ok {
		for _, spec := range general.Specs {
			if value, ok := spec.(*ast.ValueSpec); ok {
				visit(value.Names[0].Name, spec)
			} else if typeSpec, ok := spec.(*ast.TypeSpec); ok {
				visit(typeSpec.Name.Name, spec)
			}
		}
	}
}

// packageReferencesContract checks the sources on every system in parallel
// against the list and the guarded names, and returns its keys, sorted.
func packageReferencesContract(sources map[string]string, guarded []string, list string) []string {
	var views [len(referencesSystems)]referencesView
	var group sync.WaitGroup
	for index, system := range referencesSystems {
		group.Go(func() { views[index] = referencesWorlds[index].check(system, sources, guarded) })
	}
	group.Wait()
	listed, expected := referencesParseList(list)
	keys, compiled, resolved := map[string]bool{}, map[string]bool{}, map[string]bool{}
	fail := func(key string, bad bool) { keys[key] = keys[key] || bad }
	for index, view := range views {
		for key, bad := range view.keys {
			fail(key, bad)
		}
		maps.Copy(compiled, view.compiled)
		maps.Copy(resolved, view.resolved)
		for key := range expected {
			view.counts[key[:strings.LastIndex(key, "\t")]] += 0 // a listed reference counts on every system
		}
		for key, count := range view.counts {
			file, _, _ := strings.Cut(key, "\t")
			fail("ref:"+strings.ReplaceAll(key, "\t", ":"), view.compiled[file] && !view.keys["world:"+referencesSystems[index]] && count != expected[key+"\t"+referencesSystems[index]])
		}
	}
	for name := range listed {
		_, present := sources[name]
		fail("file:"+name, !present)
	}
	for name := range sources {
		fail("file:"+name, !listed[name])
		fail("uncompiled:"+name, !compiled[name] && !keys["parse:"+name])
	}
	for _, name := range guarded {
		fail("unknown:"+name, !resolved[name])
	}
	maps.DeleteFunc(keys, func(_ string, bad bool) bool { return !bad })
	return slices.Sorted(maps.Keys(keys))
}

// referencesParseList returns the list's files and its counts, keyed file,
// declaration, object and system: a line per file, then a tab-indented line
// per declaration, "<declaration> <object> [xN] [@system,...] <object> ...".
func referencesParseList(list string) (map[string]bool, map[string]int) {
	files, expected, file, pattern := map[string]bool{}, map[string]int{}, "", regexp.MustCompile(`(\S+)(?: x([0-9]+))?(?: @(\S+))?`)
	for line := range strings.Lines(strings.TrimSpace(list)) {
		declaration, objects, _ := strings.Cut(strings.TrimSpace(line), " ")
		if !strings.HasPrefix(line, "\t") {
			file, files[declaration] = declaration, true
		}
		for _, object := range pattern.FindAllStringSubmatch(objects, -1) {
			count, _ := strconv.Atoi(cmp.Or(object[2], "1"))
			for _, system := range strings.Split(cmp.Or(object[3], strings.Join(referencesSystems[:], ",")), ",") {
				expected[file+"\t"+declaration+"\t"+object[1]+"\t"+system] += count
			}
		}
	}
	return files, expected
}

// packageReferencesForeign returns a key for each directory entry a build
// takes as non-Go source.
func packageReferencesForeign(names []string) (keys []string) {
	for _, name := range names {
		if slices.Contains(strings.Fields(".s .c .cc .cxx .cpp .h .hh .hpp .hxx .m .f .for .f90 .syso .swig .swigcxx"), strings.ToLower(filepath.Ext(name))) {
			keys = append(keys, "foreign:"+name)
		}
	}
	return keys
}
