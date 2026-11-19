package localcell

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"go/ast"
	"go/build"
	"go/parser"
	"go/scanner"
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

// referencesWorld is one system's dependencies: listed once per test process
// and each type-checked from source without bodies on its first import, both
// under its lock; its checks run concurrently.
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

// referencesViews holds a view per system, in referencesSystems' order; a
// system set is a mask over that order.
type referencesViews = [len(referencesSystems)]referencesView

const referencesAll = 1<<len(referencesSystems) - 1

// referencesParsed shares parses across checks, systems and rows, in one file
// set: per file name and line ending, the last source parsed, reused only while
// the source is byte-identical.
var referencesParsed = struct {
	sync.Mutex
	fset  *token.FileSet
	files map[string]referencesParse
}{fset: token.NewFileSet(), files: map[string]referencesParse{}}

type referencesParse struct {
	source string
	file   *ast.File
	err    error
}

// referencesParseFile returns the parse of a source, shared while unchanged.
func referencesParseFile(name, source string) (*ast.File, error) {
	key := name + "\t" + strconv.FormatBool(strings.Contains(source, "\r"))
	referencesParsed.Lock()
	parsed, seen := referencesParsed.files[key]
	referencesParsed.Unlock()
	if seen && parsed.source == source {
		return parsed.file, parsed.err
	}
	file, err := parser.ParseFile(referencesParsed.fset, name, source, parser.ParseComments|parser.SkipObjectResolution)
	referencesParsed.Lock()
	referencesParsed.files[key] = referencesParse{source, file, err}
	referencesParsed.Unlock()
	return file, err
}

// load lists the package's dependencies for system with the go command (its
// error output discarded, its module index off so that it writes nothing to
// the build cache) and admits the listing.
func (w *referencesWorld) load(system string) {
	command := exec.Command("go", "list", "-deps", "-json=ImportPath,Dir,GoFiles,ImportMap", ".")
	command.Env = append(os.Environ(), "GOOS="+system, "GOARCH=amd64", "CGO_ENABLED=0", "GODEBUG="+strings.TrimPrefix(os.Getenv("GODEBUG")+",goindex=0", ","))
	// A JoinPath keeps go/build from running the go command itself.
	w.context, command.Stderr = build.Default, io.Discard
	w.context.GOOS, w.context.GOARCH, w.context.CgoEnabled, w.context.JoinPath = system, "amd64", false, filepath.Join
	w.admit(command.Output())
}

// admit decodes a listing: a go command error, a listing that does not decode,
// or one without the package or runtime leaves the world failed.
func (w *referencesWorld) admit(output []byte, err error) {
	w.listed, w.fset, w.packages = map[string]*referencesListed{}, token.NewFileSet(), map[string]*types.Package{}
	for decoder := json.NewDecoder(bytes.NewReader(output)); err == nil; {
		listed := &referencesListed{}
		if err = decoder.Decode(listed); err == nil {
			w.listed[listed.ImportPath], w.self = listed, listed.ImportPath
		}
	}
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

// locked returns importer holding the world's lock while it imports.
func (w *referencesWorld) locked(importer types.Importer) types.Importer {
	return readers4Importer(func(path string) (*types.Package, error) {
		w.Lock()
		defer w.Unlock()
		return importer.Import(path)
	})
}

// resolve returns the object a guarded name spells on this system: a member of
// the checked package bare, any other by import path, a method as (T).m or
// (*T).m of the type that declares it.
func (w *referencesWorld) resolve(self *types.Package, name string) types.Object {
	receiver, method, _ := strings.Cut(strings.TrimPrefix(name, "("), ").")
	member, scope := strings.TrimPrefix(receiver, "*"), self.Scope()
	if dot := strings.LastIndex(member, "."); dot >= 0 {
		imported, err := w.locked(w.importer(nil, "")).Import(member[:dot])
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
	if w.listed == nil {
		w.load(system)
	}
	w.Unlock()
	view := referencesView{compiled: map[string]bool{}, keys: map[string]bool{}, resolved: map[string]bool{}, counts: map[string]int{}}
	fail := func(key string, bad bool) bool { view.keys[key] = view.keys[key] || bad; return bad }
	context, fset, files, size := w.context, referencesParsed.fset, []*ast.File{}, 0
	context.OpenFile = func(path string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(sources[path])), nil }
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		if match, err := context.MatchFile(".", name); !fail("parse:"+name, err != nil) && match {
			file, err := referencesParseFile(name, sources[name])
			if !fail("parse:"+name, err != nil) {
				view.compiled[name], files, size = true, append(files, file), size+len(sources[name])
			}
		}
	}
	if fail("world:"+system, w.failed) {
		return view
	}
	// A file is named by its source key, never by a position a line directive
	// adjusts. The maps are sized from the sources (about one typed expression
	// in 13 bytes) so that they do not grow.
	info := &types.Info{Uses: make(map[*ast.Ident]types.Object, size/16), Defs: make(map[*ast.Ident]types.Object, size/80), Instances: map[*ast.Ident]types.Instance{}, Types: make(map[ast.Expr]types.TypeAndValue, size/11)}
	config := types.Config{Importer: w.locked(w.importer(w.listed[w.self].ImportMap, w.listed[w.self].Dir)), Sizes: types.SizesFor("gc", "amd64"),
		Error: func(err error) {
			fail("types:"+system+":"+fset.PositionFor(err.(types.Error).Pos, false).Filename, true)
		}}
	self, _ := config.Check(w.self, fset, files, info)
	// objects are the guarded names each object stands for: a guarded object its
	// own, and a package variable or field each guarded name given to it.
	objects, methods := map[types.Object][]string{}, []*types.Func{}
	for _, name := range guarded {
		object := w.resolve(self, name)
		if method, ok := object.(*types.Func); ok && method.Signature().Recv() != nil {
			methods = append(methods, method)
		}
		if object != nil {
			objects[object], view.resolved[name] = append(objects[object], name), true
		}
	}
	// targets adds, for an interface method, each other guarded method of its
	// name and identical signature as used, an instance's and not its origin's
	// (receivers are not compared, as go/types/predicates.go:337-385 shows).
	targets := func(object types.Object) []string {
		if object == nil {
			return nil
		}
		method, _ := object.(*types.Func)
		if method != nil {
			object = method.Origin()
		} else if variable, ok := object.(*types.Var); ok {
			object = variable.Origin()
		}
		names := slices.Clone(objects[object])
		if method != nil && method.Signature().Recv() != nil && types.IsInterface(method.Signature().Recv().Type()) {
			for _, guard := range methods {
				if guard != method.Origin() && guard.Name() == method.Name() && types.Identical(guard.Signature(), method.Signature()) {
					names = append(names, objects[guard]...)
				}
			}
		}
		return names
	}
	// supplied adds, at an instantiation, each guarded method a type argument
	// supplies for a method of its type parameter's constraint.
	supplied := func(identifier *ast.Ident) (names []string) {
		var generic *types.TypeParamList
		if function, ok := info.Uses[identifier].(*types.Func); ok {
			generic = function.Origin().Signature().TypeParams()
		} else if name, ok := info.Uses[identifier].(*types.TypeName); ok {
			if named, ok := types.Unalias(name.Type()).(*types.Named); ok {
				generic = named.Origin().TypeParams()
			}
		}
		if generic.Len() == 0 {
			return nil
		}
		arguments := info.Instances[identifier].TypeArgs
		for index := range min(generic.Len(), arguments.Len()) {
			if constraint, ok := generic.At(index).Constraint().Underlying().(*types.Interface); ok {
				for method := range constraint.Methods() {
					found, _, _ := types.LookupFieldOrMethod(arguments.At(index), true, self, method.Name())
					names = append(names, targets(found)...)
				}
			}
		}
		return names
	}
	// typesIn returns a tuple's types; typesOf, those of values, a value of
	// several results spread.
	typesIn := func(tuple *types.Tuple) (found []types.Type) {
		for variable := range tuple.Variables() {
			found = append(found, variable.Type())
		}
		return found
	}
	typesOf := func(values ...ast.Expr) (found []types.Type) {
		for _, value := range values {
			if tuple, ok := info.TypeOf(value).(*types.Tuple); ok {
				found = append(found, typesIn(tuple)...)
			} else {
				found = append(found, info.TypeOf(value))
			}
		}
		return found
	}
	underlying := func(t types.Type) types.Type { return cmp.Or(t, types.Type(types.Typ[types.Invalid])).Underlying() }
	// converted returns, for a value of a concrete type in from that a site
	// converts to the interface type at its index in to, each guarded method it
	// supplies for a method the interface declares; matched, each guarded method
	// that can supply a method of an interface type a value is asserted to.
	converted := func(from, to []types.Type) (names []string) {
		for index := range min(len(from), len(to)) {
			if face, ok := underlying(to[index]).(*types.Interface); ok && from[index] != nil && !types.IsInterface(from[index]) {
				for method := range face.Methods() {
					found, _, _ := types.LookupFieldOrMethod(from[index], true, self, method.Name())
					names = append(names, targets(found)...)
				}
			}
		}
		return names
	}
	matched := func(t types.Type) (names []string) {
		if face, ok := underlying(t).(*types.Interface); ok {
			for method := range face.Methods() {
				names = append(names, targets(method)...)
			}
		}
		return names
	}
	// A value is given by an initializer, an assignment, a composite-literal
	// key, a call argument, a send or a range. A package variable or a field
	// takes it as an alias, counted at every use; a local or a parameter carries
	// it, never counted. A struct conversion carries a field into each field of
	// its name and identical type: fields holds them. results holds each return's
	// result types.
	given, fields, carried, results := map[ast.Expr]types.Object{}, map[string][]*types.Var{}, map[types.Object][]string{}, map[*ast.ReturnStmt]*types.Tuple{}
	named := func(expression ast.Expr) types.Object {
		identifier, _ := expression.(*ast.Ident)
		return cmp.Or(referencesNamed(info, expression), info.Defs[identifier])
	}
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			body, signature := (*ast.BlockStmt)(nil), types.Type(nil)
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
						given[value] = named(node.Lhs[index])
					}
				}
			case *ast.KeyValueExpr:
				if field, ok := referencesNamed(info, node.Key).(*types.Var); ok && field.Kind() == types.FieldVar {
					given[node.Value] = field
				}
			case *ast.CallExpr:
				if function, ok := referencesNamed(info, node.Fun).(*types.Func); ok && function.Pkg() == self {
					for index, argument := range node.Args {
						parameters := function.Origin().Signature().Params()
						given[argument] = parameters.At(min(index, parameters.Len()-1))
					}
				}
			case *ast.SendStmt:
				given[node.Value] = named(node.Chan)
			case *ast.RangeStmt:
				given[node.X] = named(cmp.Or(node.Value, node.Key))
			case *ast.FuncDecl:
				body, signature = node.Body, info.TypeOf(node.Name)
			case *ast.FuncLit:
				body, signature = node.Body, info.TypeOf(node)
			}
			if signature, ok := signature.(*types.Signature); ok && body != nil {
				ast.Inspect(body, func(inner ast.Node) bool {
					if statement, ok := inner.(*ast.ReturnStmt); ok {
						results[statement] = signature.Results()
					}
					_, literal := inner.(*ast.FuncLit)
					return !literal
				})
			}
			return true
		})
	}
	// referenced returns the names a node references: an identifier's guarded
	// object or alias and an instantiation's supply; at a site that converts a
	// value to an interface type, what the value supplies for it; at an assertion
	// to an interface type or a range assignment to one, what can supply it.
	referenced := func(node ast.Node) []string {
		if identifier, ok := node.(*ast.Ident); ok {
			return append(targets(info.Uses[identifier]), supplied(identifier)...)
		}
		names, to := []string(nil), []types.Type(nil)
		switch node := node.(type) {
		case *ast.CallExpr:
			callee := info.Types[ast.Unparen(node.Fun)]
			if signature, ok := callee.Type.(*types.Signature); ok && !callee.IsType() {
				if to = typesIn(signature.Params()); signature.Variadic() && !node.Ellipsis.IsValid() {
					last := to[len(to)-1].(*types.Slice).Elem()
					to = append(to[:len(to)-1], slices.Repeat([]types.Type{last}, max(len(node.Args)-len(to)+1, 0))...)
				}
			} else if callee.IsType() {
				to = append(to, callee.Type)
			}
			return append(names, converted(typesOf(node.Args...), to)...)
		case *ast.AssignStmt:
			for _, left := range node.Lhs {
				to = append(to, info.TypeOf(left))
			}
			return append(names, converted(typesOf(node.Rhs...), to)...)
		case *ast.ValueSpec:
			for _, name := range node.Names {
				to = append(to, info.TypeOf(name))
			}
			return append(names, converted(typesOf(node.Values...), to)...)
		case *ast.ReturnStmt:
			return append(names, converted(typesOf(node.Results...), typesIn(results[node]))...)
		case *ast.SendStmt:
			if channel, ok := underlying(info.TypeOf(node.Chan)).(*types.Chan); ok {
				return append(names, converted(typesOf(node.Value), []types.Type{channel.Elem()})...)
			}
		case *ast.IndexExpr:
			if container, ok := underlying(info.TypeOf(node.X)).(*types.Map); ok {
				return append(names, converted(typesOf(node.Index), []types.Type{container.Key()})...)
			}
		case *ast.CompositeLit:
			container := underlying(info.TypeOf(node))
			if pointer, ok := container.(*types.Pointer); ok {
				container = pointer.Elem().Underlying()
			}
			for index, element := range node.Elts {
				key, value := ast.Expr(nil), element
				if pair, ok := element.(*ast.KeyValueExpr); ok {
					key, value = pair.Key, pair.Value
				}
				switch container := container.(type) {
				case *types.Struct:
					to = []types.Type{info.TypeOf(key)}
					if key == nil && index < container.NumFields() {
						to = []types.Type{container.Field(index).Type()}
					}
				case interface{ Elem() types.Type }:
					to = []types.Type{container.Elem()}
				}
				if container, ok := container.(*types.Map); ok {
					names = append(names, converted(typesOf(key), []types.Type{container.Key()})...)
				}
				names = append(names, converted(typesOf(value), to)...)
			}
		case *ast.TypeAssertExpr:
			return append(names, matched(info.TypeOf(node.Type))...)
		case *ast.CaseClause:
			for _, expression := range node.List {
				if info.Types[expression].IsType() {
					names = append(names, matched(info.Types[expression].Type)...)
				}
			}
		case *ast.RangeStmt:
			if node.Tok == token.ASSIGN {
				return append(append(names, matched(info.TypeOf(node.Key))...), matched(info.TypeOf(node.Value))...)
			}
		}
		return names
	}
	// held returns the names a given value holds: a function literal, what it
	// references; any other value, each guarded identifier or local in it that is
	// not called and has no basic type, and what each function literal in it
	// holds. A call holds an argument it converts or can return, or when a result
	// is neither of a basic type nor an interface with methods. What a value
	// holds is found once (shape): its identifiers, and the names that cannot
	// change; an identifier's own are read on every pass.
	type valueNames struct {
		static      []string
		identifiers []*ast.Ident
	}
	shapes := map[ast.Expr]*valueNames{}
	var shape func(*valueNames, ast.Node, bool)
	shape = func(names *valueNames, value ast.Node, whole bool) {
		var skipped map[ast.Node]bool
		ast.Inspect(value, func(node ast.Node) bool {
			identifier, _ := node.(*ast.Ident)
			expression, _ := node.(ast.Expr)
			basic, _ := underlying(info.TypeOf(expression)).(*types.Basic)
			literal, nested := node.(*ast.FuncLit)
			switch call, isCall := node.(*ast.CallExpr); {
			case whole && identifier != nil:
				names.identifiers, names.static = append(names.identifiers, identifier), append(names.static, supplied(identifier)...)
			case whole:
				names.static = append(names.static, referenced(node)...)
			case skipped[node] || basic != nil && basic.Kind() != types.Uintptr && basic.Kind() != types.UnsafePointer:
				return false
			case nested:
				shape(names, literal, true)
				return false
			case identifier != nil:
				names.identifiers = append(names.identifiers, identifier)
			case isCall:
				if skipped == nil {
					skipped = map[ast.Node]bool{}
				}
				_, invoked := ast.Unparen(call.Fun).(*ast.FuncLit)
				skipped[call.Fun] = !invoked
				for _, argument := range call.Args {
					kept := info.Types[ast.Unparen(call.Fun)].IsType()
					for _, result := range typesOf(call) {
						face, _ := underlying(result).(*types.Interface)
						_, basic := underlying(result).(*types.Basic)
						kept = kept || !basic && (face == nil || face.NumMethods() == 0) || info.TypeOf(argument) != nil && types.AssignableTo(info.TypeOf(argument), result)
					}
					skipped[argument] = !kept
				}
			}
			return true
		})
	}
	held := func(value ast.Expr) []string {
		if shapes[value] == nil {
			_, whole := ast.Unparen(value).(*ast.FuncLit)
			shapes[value] = &valueNames{}
			shape(shapes[value], value, whole)
		}
		names := slices.Clone(shapes[value].static)
		for _, identifier := range shapes[value].identifiers {
			names = append(append(names, targets(info.Uses[identifier])...), carried[info.Uses[identifier]]...)
		}
		return names
	}
	for _, uses := range []map[*ast.Ident]types.Object{info.Defs, info.Uses} {
		for _, object := range uses {
			if field, ok := object.(*types.Var); ok && field.Kind() == types.FieldVar && !slices.Contains(fields[field.Name()], field.Origin()) {
				fields[field.Name()] = append(fields[field.Name()], field.Origin())
			}
		}
	}
	for changed := true; changed; {
		changed = false
		add := func(to types.Object, names []string) {
			variable, ok := to.(*types.Var)
			holder := carried
			if ok && (variable.Kind() == types.PackageVar || variable.Kind() == types.FieldVar) {
				holder = objects
			}
			for _, target := range names {
				if ok && !slices.Contains(holder[variable.Origin()], target) {
					holder[variable.Origin()], changed = append(holder[variable.Origin()], target), true
				}
			}
		}
		for value, to := range given {
			add(to, held(value))
		}
		for object, names := range objects {
			if field, ok := object.(*types.Var); ok && field.Kind() == types.FieldVar {
				for _, other := range fields[field.Name()] {
					if types.Identical(other.Type(), field.Type()) {
						add(other, names)
					}
				}
			}
		}
	}
	for _, file := range files {
		name := fset.PositionFor(file.Package, false).Filename
		for _, group := range file.Comments {
			for _, comment := range group.List {
				fail("linkname:"+name, strings.HasPrefix(comment.Text, "//go:linkname"))
				fail("line:"+name, strings.HasPrefix(comment.Text, "//line ") || strings.HasPrefix(comment.Text, "/*line "))
			}
		}
		for _, spec := range file.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			view.counts[name+"\timport\t"+path] = 1
		}
		for _, declaration := range file.Decls {
			referencesDeclarations(declaration, func(declared string, node ast.Node) {
				function, ok := node.(*ast.FuncDecl)
				fail("bodyless:"+name+":"+declared, ok && function.Body == nil)
				ast.Inspect(node, func(node ast.Node) bool {
					for _, target := range referenced(node) {
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
	return info.Uses[referencesIdentifier(expression)]
}

// referencesIdentifier returns the identifier that names it.
func referencesIdentifier(expression ast.Expr) *ast.Ident {
	switch node := ast.Unparen(expression).(type) {
	case *ast.Ident:
		return node
	case *ast.SelectorExpr:
		return node.Sel
	case *ast.IndexExpr:
		return referencesIdentifier(node.X)
	case *ast.IndexListExpr:
		return referencesIdentifier(node.X)
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

// referencesSlots bounds the checks in flight, whichever their systems, to one
// per system.
var referencesSlots = make(chan struct{}, len(referencesSystems))

// referencesCheck type-checks the sources in parallel on each system of checks
// and keeps each other system's view from views.
func referencesCheck(views referencesViews, checks uint8, sources map[string]string, guarded []string) referencesViews {
	var group sync.WaitGroup
	for index, system := range referencesSystems {
		if checks>>index&1 == 1 {
			group.Go(func() {
				referencesSlots <- struct{}{}
				defer func() { <-referencesSlots }()
				views[index] = referencesWorlds[index].check(system, sources, guarded)
			})
		}
	}
	group.Wait()
	return views
}

// referencesPlan returns the systems a row's edits are type-checked on, from
// clean, the clean package's views, and those it replays CRLF on. An edit in
// place (before, after, replace or append) of a file the clean package has is
// checked on each system that compiles it; any other on every system: a file
// added, copied or dropped, a guarded name, a directory entry, or a package
// clause, //go: directive or build constraint in its anchor or text. CRLF is
// replayed on the first of them, or on each when an edit holds a line
// directive, a //go: directive, a build constraint or a raw string that spans
// lines.
func referencesPlan(clean referencesViews, edits [][4]string) (lf, crlf uint8) {
	every, lines := false, false
	for _, edit := range edits {
		known := false
		for index, view := range clean {
			if view.compiled[edit[0]] {
				known, lf = true, lf|1<<index
			}
		}
		shape, line := referencesTokens(edit[2], edit[3])
		every = every || !known || !slices.Contains(strings.Fields("before after replace append"), edit[1]) || shape
		lines = lines || line
	}
	if every {
		lf = referencesAll
	}
	if crlf = lf & -lf; lines {
		crlf = lf
	}
	return lf, crlf
}

// referencesTokens reports whether texts hold a package clause, a //go:
// directive or a build constraint (shape), and whether they hold a line
// directive, a //go: directive, a build constraint or a raw string that spans
// lines (lines); a text that does not scan holds both.
func referencesTokens(texts ...string) (shape, lines bool) {
	for _, text := range texts {
		var scan scanner.Scanner
		scan.Init(token.NewFileSet().AddFile("", -1, len(text)), []byte(text), func(token.Position, string) { shape, lines = true, true }, scanner.ScanComments)
		for _, kind, literal := scan.Scan(); kind != token.EOF; _, kind, literal = scan.Scan() {
			directive := kind == token.COMMENT && (strings.HasPrefix(literal, "//go:") || strings.Contains(literal, "+build"))
			shape = shape || kind == token.PACKAGE || directive
			lines = lines || directive || kind == token.COMMENT && (strings.HasPrefix(literal, "//line ") || strings.HasPrefix(literal, "/*line ")) ||
				kind == token.STRING && strings.HasPrefix(literal, "`") && strings.Contains(literal, "\n")
		}
	}
	return shape, lines
}

// packageReferencesContract merges the views of the sources against the list
// and the guarded names, a system without a view failing closed, and returns
// its keys, sorted, and for each key of a count the wanted and found counts on
// each system that compiles its file.
func packageReferencesContract(views referencesViews, sources map[string]string, guarded []string, list string) ([]string, map[string]string) {
	listed, expected := referencesParseList(list)
	keys, counted, compiled, resolved := map[string]bool{}, map[string]string{}, map[string]bool{}, map[string]bool{}
	fail := func(key string, bad bool) { keys[key] = keys[key] || bad }
	for index, view := range views {
		fail("unchecked:"+referencesSystems[index], view.keys == nil)
		for key, bad := range view.keys {
			fail(key, bad)
		}
		maps.Copy(compiled, view.compiled)
		maps.Copy(resolved, view.resolved)
		counts := maps.Collect(maps.All(view.counts))
		for key := range expected {
			counts[key[:strings.LastIndex(key, "\t")]] += 0 // a listed reference counts on every system
		}
		for key, count := range counts {
			file, rest, _ := strings.Cut(key, "\t")
			named, system := "ref:"+strings.ReplaceAll(key, "\t", ":"), referencesSystems[index]
			if path, imported := strings.CutPrefix(rest, "import\t"); imported {
				named = "import:" + file + ":" + path
			}
			fail(named, view.compiled[file] && !view.keys["world:"+system] && count != expected[key+"\t"+system])
			counted[named] = key
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
	details := map[string]string{}
	for named, key := range counted {
		file, want, got := key[:strings.Index(key, "\t")], "", ""
		for index, view := range views {
			if system := referencesSystems[index]; keys[named] && view.compiled[file] && !view.keys["world:"+system] {
				want, got = want+" "+system+"="+strconv.Itoa(expected[key+"\t"+system]), got+" "+system+"="+strconv.Itoa(view.counts[key])
				details[named] = " want" + want + " got" + got
			}
		}
	}
	return slices.Sorted(maps.Keys(keys)), details
}

// referencesParseList returns the list's files and its counts, keyed file,
// declaration, object and system: a line per file, then tab-indented lines,
// "import <path> ..." for its imports and, per declaration, "<declaration>
// <object> [xN] [@system,...] <object> ...".
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
