// Package routeinventory reads Go source to list the cell server's HTTP routes
// for tests. It must never be imported by product code.
package routeinventory

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Listener identifies the cell listener on which a route is registered.
type Listener string

// ListenerAPI, ListenerHealth and ListenerMetrics name the cell's three listeners.
const (
	ListenerAPI     Listener = "api"
	ListenerHealth  Listener = "health"
	ListenerMetrics Listener = "metrics"
)

// ServerListeners is deliberately explicit: a new registering function must be
// reviewed even when it registers a route already found elsewhere in the source.
var ServerListeners = map[string]Listener{
	"apiMux": ListenerAPI,
	"registerAgentEmailOutboundProviderEventRoute": ListenerAPI,
	"healthMux": ListenerHealth,
	// Local-runtime health mux additionally serving /sync-readyz (LC-S6B).
	"healthMuxWithSync": ListenerHealth,
	"metricsMuxFor":     ListenerMetrics,
}

// Site records the source position of a Handle or HandleFunc call.
type Site struct {
	File string
	Line int
	Func string
}

// Route is one unique ServeMux pattern, including all conditional registrations.
type Route struct {
	Pattern  string
	Method   string
	Path     string
	Listener Listener
	Sites    []Site
}

// RepoRoot finds this package's repository, independent of the test working
// directory. It requires the repository's exact module declaration.
func RepoRoot() (string, error) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("routeinventory: cannot locate source file")
	}
	for dir := filepath.Dir(source); ; dir = filepath.Dir(dir) {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("routeinventory: read go.mod in %s: %w", dir, err)
		}
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 2 && fields[0] == "module" {
					module := fields[1]
					if unquoted, err := strconv.Unquote(module); err == nil {
						module = unquoted
					}
					if module == "github.com/witwave-ai/witself" {
						return dir, nil
					}
				}
			}
		}
		if filepath.Dir(dir) == dir {
			return "", fmt.Errorf("routeinventory: no witself module above %s", source)
		}
	}
}

// ServerRoutes lists registrations on the cell API, health and metrics listeners.
func ServerRoutes(repoRoot string) ([]Route, error) {
	return ParseDir(filepath.Join(repoRoot, "internal", "server"), ServerListeners)
}

// ParseDir parses every non-test Go file, including files excluded by build tags.
// Every selector call named Handle or HandleFunc with at least two arguments is
// treated as a registration. Unknown functions and dynamic patterns fail closed.
func ParseDir(dir string, listeners map[string]Listener) ([]Route, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	byPattern := make(map[string]*Route)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), nil, 0)
		if err != nil {
			return nil, err
		}
		for _, decl := range file.Decls {
			function := "<package>"
			if fn, ok := decl.(*ast.FuncDecl); ok {
				function = fn.Name.Name
			}
			var inspectErr error
			ast.Inspect(decl, func(node ast.Node) bool {
				if inspectErr != nil {
					return false
				}
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) < 2 {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (selector.Sel.Name != "HandleFunc" && selector.Sel.Name != "Handle") {
					return true
				}
				pos := fset.Position(call.Pos())
				literal, ok := call.Args[0].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					inspectErr = fmt.Errorf("%s:%d: %s registration pattern must be a basic string literal", pos.Filename, pos.Line, selector.Sel.Name)
					return false
				}
				pattern, err := strconv.Unquote(literal.Value)
				if err != nil {
					inspectErr = fmt.Errorf("%s:%d: invalid route string: %w", pos.Filename, pos.Line, err)
					return false
				}
				listener, ok := listeners[function]
				if !ok {
					inspectErr = fmt.Errorf("%s:%d: function %s registers route %q; add it to routeinventory.ServerListeners", pos.Filename, pos.Line, function, pattern)
					return false
				}
				site := Site{File: pos.Filename, Line: pos.Line, Func: function}
				if route, ok := byPattern[pattern]; ok {
					if route.Listener != listener {
						first := route.Sites[0]
						inspectErr = fmt.Errorf("%s:%d: route %q has conflicting listeners %q and %q (first registered at %s:%d)", pos.Filename, pos.Line, pattern, route.Listener, listener, first.File, first.Line)
						return false
					}
					route.Sites = append(route.Sites, site)
					return true
				}
				method, path := "", pattern
				if split := strings.IndexAny(pattern, " \t"); split >= 0 {
					method, path = pattern[:split], strings.TrimLeft(pattern[split:], " \t")
				}
				byPattern[pattern] = &Route{Pattern: pattern, Method: method, Path: path, Listener: listener, Sites: []Site{site}}
				return true
			})
			if inspectErr != nil {
				return nil, inspectErr
			}
		}
	}
	routes := make([]Route, 0, len(byPattern))
	for _, route := range byPattern {
		routes = append(routes, *route)
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].Pattern < routes[j].Pattern })
	return routes, nil
}
