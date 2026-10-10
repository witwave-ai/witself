package routeinventory

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRouteInventoryParseDir(t *testing.T) {
	t.Run("literal registrations and source positions", func(t *testing.T) {
		routes, err := ParseDir(filepath.Join("testdata", "valid"), map[string]Listener{"register": ListenerAPI})
		if err != nil {
			t.Fatal(err)
		}
		var patterns []string
		byPattern := make(map[string]Route)
		for _, route := range routes {
			patterns = append(patterns, route.Pattern)
			byPattern[route.Pattern] = route
			if route.Listener != ListenerAPI {
				t.Errorf("route %q listener = %q, want api", route.Pattern, route.Listener)
			}
			for _, site := range route.Sites {
				if site.File != filepath.Join("testdata", "valid", "registrations.go") || site.Func != "register" {
					t.Errorf("route %q site = %+v", route.Pattern, site)
				}
			}
		}
		wantPatterns := []string{"/nested", "/ready", "GET /duplicate", "GET /single", "POST /multi", "PUT /handled"}
		if !reflect.DeepEqual(patterns, wantPatterns) {
			t.Fatalf("sorted patterns = %v, want %v", patterns, wantPatterns)
		}
		for _, tc := range []struct {
			pattern string
			method  string
			path    string
			lines   []int
		}{
			{"GET /single", "GET", "/single", []int{6}},
			{"POST /multi", "POST", "/multi", []int{7}},
			{"/ready", "", "/ready", []int{10}},
			{"GET /duplicate", "GET", "/duplicate", []int{12, 14}},
			{"PUT /handled", "PUT", "/handled", []int{16}},
			{"/nested", "", "/nested", []int{18}},
		} {
			route := byPattern[tc.pattern]
			if route.Method != tc.method || route.Path != tc.path {
				t.Errorf("route %q split = (%q, %q), want (%q, %q)", tc.pattern, route.Method, route.Path, tc.method, tc.path)
			}
			var lines []int
			for _, site := range route.Sites {
				lines = append(lines, site.Line)
			}
			if !reflect.DeepEqual(lines, tc.lines) {
				t.Errorf("route %q site lines = %v, want %v", tc.pattern, lines, tc.lines)
			}
		}
	})
	for _, tc := range []struct {
		name      string
		listeners map[string]Listener
		want      []string
	}{
		{
			name:      "nonliteral",
			listeners: map[string]Listener{"register": ListenerAPI},
			want:      []string{"registrations.go:6", "basic string literal"},
		},
		{
			name:      "unlisted",
			listeners: map[string]Listener{"register": ListenerAPI},
			want:      []string{"registrations.go:6", "function unlisted", "GET /unlisted", "add it to routeinventory.ServerListeners"},
		},
		{
			name:      "conflict",
			listeners: map[string]Listener{"first": ListenerAPI, "second": ListenerHealth},
			want:      []string{"registrations.go:10", "registrations.go:6", "GET /duplicate", "conflicting listeners", "api", "health"},
		},
		{
			name:      "package_scope",
			listeners: map[string]Listener{},
			want:      []string{"registrations.go:6", "function <package>", "GET /global", "add it to routeinventory.ServerListeners"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseDir(filepath.Join("testdata", tc.name), tc.listeners)
			if err == nil {
				t.Fatal("ParseDir accepted an unreviewable registration")
			}
			for _, fragment := range tc.want {
				if !strings.Contains(err.Error(), fragment) {
					t.Errorf("error %q does not contain %q", err, fragment)
				}
			}
		})
	}
}

func TestRouteInventoryServerSource(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	routes, err := ServerRoutes(root)
	if err != nil {
		t.Fatal(err)
	}
	byPattern := make(map[string]Route)
	for _, route := range routes {
		byPattern[route.Pattern] = route
		if route.Listener == ListenerAPI && !strings.HasPrefix(route.Path, "/v1/") {
			t.Errorf("API route %q does not begin /v1/", route.Pattern)
		}
	}
	for _, tc := range []struct {
		pattern  string
		listener Listener
		file     string
	}{
		{"GET /v1/memories/{memory}", ListenerAPI, "server.go"},
		{"POST /v1/internal/agent-email-send:provider-event", ListenerAPI, "agent_email_outbound.go"},
		{"/readyz", ListenerHealth, "server.go"},
		{"/metrics", ListenerMetrics, "server.go"},
	} {
		route, ok := byPattern[tc.pattern]
		if !ok {
			t.Errorf("missing production route %q", tc.pattern)
			continue
		}
		if route.Listener != tc.listener {
			t.Errorf("route %q listener = %q, want %q", tc.pattern, route.Listener, tc.listener)
		}
		foundFile := false
		for _, site := range route.Sites {
			if filepath.Base(site.File) == tc.file && site.Line > 0 {
				foundFile = true
			}
		}
		if !foundFile {
			t.Errorf("route %q has no site in %s: %+v", tc.pattern, tc.file, route.Sites)
		}
	}
	if route := byPattern["GET /v1/transcripts/{transcript}"]; len(route.Sites) != 2 {
		t.Errorf("transcript route sites = %d, want 2", len(route.Sites))
	}
	commandRoutes, err := ParseDir(filepath.Join(root, "cmd", "witself-server"), map[string]Listener{})
	if err != nil {
		t.Fatal(err)
	}
	if len(commandRoutes) != 0 {
		t.Errorf("cmd/witself-server registers routes: %+v", commandRoutes)
	}
}
