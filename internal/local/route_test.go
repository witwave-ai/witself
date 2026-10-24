package local

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRouteRoundTripAndHostedConfigBytes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	p := filepath.Join(home, "config.json")
	const original = "{\n  \"accounts\": {\n    \"default\": {\n      \"id\": \"acc_local\",\n      \"email\": \"local@example.test\"\n    },\n    \"work\": {\n      \"id\": \"acc_work\"\n    }\n  }\n}\n"
	if err := os.WriteFile(p, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := write(cfg); err != nil {
		t.Fatal(err)
	}
	assertConfigBytes(t, p, []byte(original))
	for _, endpoint := range []string{"http://127.0.0.1:8048", "http://[::1]:8048"} {
		route := &Route{Kind: "local", Endpoint: endpoint}
		if err := SetRoute("default", route); err != nil {
			t.Fatal(err)
		}
		cfg, err = Load()
		if err != nil {
			t.Fatal(err)
		}
		acct := cfg.Accounts["default"]
		if !reflect.DeepEqual(acct.Route, route) || acct.ID != "acc_local" || acct.Email != "local@example.test" || cfg.Accounts["work"].Route != nil {
			t.Fatal("connection route round trip changed the account binding")
		}
	}
	if err := SetRoute("default", nil); err != nil {
		t.Fatal(err)
	}
	assertConfigBytes(t, p, []byte(original))
	if err := SetRoute("missing", &Route{Kind: "local", Endpoint: "http://127.0.0.1"}); err == nil {
		t.Fatal("setting a connection route created an account")
	}
	if err := SetRoute("missing", nil); err == nil {
		t.Fatal("clearing a missing account succeeded")
	}
	assertConfigBytes(t, p, []byte(original))
}

func TestRouteValidationAtLoadSetSaveAndWrite(t *testing.T) {
	invalid := []Route{
		{Kind: "private-unknown-kind", Endpoint: "http://127.0.0.1"},
		{Kind: "", Endpoint: "http://127.0.0.1"},
		{Kind: "local", Endpoint: "https://127.0.0.1"},
		{Kind: "local", Endpoint: "http://localhost"},
		{Kind: "local", Endpoint: "http://example.test"},
		{Kind: "local", Endpoint: "http://127.0.0.2"},
		{Kind: "local", Endpoint: "http://0.0.0.0"},
		{Kind: "local", Endpoint: "http://[::]"},
		{Kind: "local", Endpoint: "http://[::ffff:127.0.0.1]"},
		{Kind: "local", Endpoint: "http://::1"},
		{Kind: "local", Endpoint: "http://127.0.0.1:"},
		{Kind: "local", Endpoint: "http://127.0.0.1:0"},
		{Kind: "local", Endpoint: "http://127.0.0.1:65536"},
		{Kind: "local", Endpoint: "http://private-user@127.0.0.1"},
		{Kind: "local", Endpoint: "http://127.0.0.1?private-query"},
		{Kind: "local", Endpoint: "http://127.0.0.1?"},
		{Kind: "local", Endpoint: "http://127.0.0.1#private-fragment"},
		{Kind: "local", Endpoint: "http://127.0.0.1#"},
		{Kind: "local", Endpoint: "http://127.0.0.1\n"},
		{Kind: "local", Endpoint: ""},
	}
	for i, route := range invalid {
		t.Run(fmt.Sprintf("invalid-%02d", i), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("WITSELF_HOME", home)
			p := filepath.Join(home, "config.json")
			original := []byte("{\"accounts\":{\"default\":{\"id\":\"acc_local\"}}}\n")
			if err := os.WriteFile(p, original, 0o600); err != nil {
				t.Fatal(err)
			}
			checkError := func(err error) {
				t.Helper()
				if err == nil {
					t.Fatal("invalid connection route accepted")
				}
				if strings.Contains(err.Error(), "private-") || (route.Endpoint != "" && strings.Contains(err.Error(), route.Endpoint)) {
					t.Fatal("connection route validation included a configuration value")
				}
			}
			checkError(route.Validate())
			checkError(SetRoute("default", &route))
			assertConfigBytes(t, p, original)
			checkError(Save("new", Account{ID: "acc_new", Route: &route}, "test-only-token"))
			tokenPath, err := TokenPath("new")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(tokenPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid connection route saved a token")
			}
			assertConfigBytes(t, p, original)
			invalidCfg := &Config{Accounts: map[string]Account{"default": {ID: "acc_local", Route: &route}}}
			checkError(write(invalidCfg))
			assertConfigBytes(t, p, original)
			b, err := json.Marshal(invalidCfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, b, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = Load()
			checkError(err)
		})
	}
}

func TestRouteLoadMalformedShapeIsValueFree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	p := filepath.Join(home, "config.json")
	for _, routeJSON := range []string{
		`"private-route-value"`,
		`["private-route-value"]`,
		`{"kind":["private-route-value"],"endpoint":"http://127.0.0.1"}`,
		`{"kind":"local","endpoint":["private-route-value"]}`,
		`{"kind":"local","endpoint":{"private-route-value":true}}`,
	} {
		b := []byte(`{"accounts":{"default":{"id":"acc_local","route":` + routeJSON + `}}}`)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load()
		if err == nil || strings.Contains(err.Error(), "private-route-value") {
			t.Fatal("malformed connection route accepted or exposed its value")
		}
	}
}

func TestRouteValidateLoopbackEndpoints(t *testing.T) {
	for _, endpoint := range []string{"http://127.0.0.1", "http://127.0.0.1/", "http://127.0.0.1:8048", "http://[::1]", "http://[::1]:8048", "http://127.0.0.1:8048/api"} {
		if err := (Route{Kind: "local", Endpoint: endpoint}).Validate(); err != nil {
			t.Fatalf("valid loopback endpoint rejected: %v", err)
		}
	}
}

func TestConfigAtomicFailurePreservesPreviousBinding(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	p := filepath.Join(home, "config.json")
	old := &Config{Accounts: map[string]Account{"default": {ID: "acc_local"}}}
	if err := write(old); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	updated := &Config{Accounts: map[string]Account{"default": {ID: "acc_local", Route: &Route{Kind: "local", Endpoint: "http://127.0.0.1:8048"}}}}
	failure := errors.New("injected pre-rename failure")
	renameCalled := false
	err = writeWithRename(updated, func(tmp, destination string) error {
		renameCalled = true
		if filepath.Dir(tmp) != filepath.Dir(p) || tmp == p || destination != p {
			t.Fatal("config replacement is not staged beside the original")
		}
		assertConfigBytes(t, p, original)
		b, err := os.ReadFile(tmp)
		if err != nil {
			t.Fatal(err)
		}
		var staged Config
		if err := json.Unmarshal(b, &staged); err != nil || !reflect.DeepEqual(&staged, updated) {
			t.Fatal("staged config is not the complete new binding")
		}
		info, err := os.Stat(tmp)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatal("staged config is not private")
		}
		return failure
	})
	if !renameCalled || !errors.Is(err, failure) {
		t.Fatal("config write did not honor the injected commit failure")
	}
	assertConfigBytes(t, p, original)
	if _, err := Load(); err != nil {
		t.Fatal("failed activation left an unreadable config")
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Fatal("failed config replacement left a temporary file")
	}
}

func TestConfigAtomicReplacementKeepsOpenReaderSnapshot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	p := filepath.Join(home, "config.json")
	cfg := &Config{Accounts: map[string]Account{"default": {ID: "acc_local"}}}
	if err := write(cfg); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	if err := SetRoute("default", &Route{Kind: "local", Endpoint: "http://127.0.0.1:8048"}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(snapshot, original) {
		t.Fatal("config write modified the previous reader's file in place")
	}
	current, err := Load()
	if err != nil || current.Accounts["default"].Route == nil {
		t.Fatal("new readers did not receive the activated connection route")
	}
}

func TestConfigConcurrentReadersNeverSeePartialFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	p := filepath.Join(home, "config.json")
	cfg := &Config{Accounts: map[string]Account{"default": {ID: "acc_local", Email: strings.Repeat("x", 32*1024)}}}
	if err := write(cfg); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	ready := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(ready)
		for {
			b, err := os.ReadFile(p)
			if err != nil {
				result <- errors.New("concurrent reader could not read config")
				return
			}
			var observed Config
			if err := json.Unmarshal(b, &observed); err != nil || observed.Accounts["default"].ID != "acc_local" || len(observed.Accounts["default"].Email) != 32*1024 {
				result <- errors.New("concurrent reader observed an incomplete config")
				return
			}
			select {
			case <-done:
				result <- nil
				return
			default:
			}
		}
	}()
	<-ready
	var writeErr error
	for i := 0; i < 40; i++ {
		var route *Route
		if i%2 == 0 {
			route = &Route{Kind: "local", Endpoint: "http://127.0.0.1:8048"}
		}
		if writeErr = SetRoute("default", route); writeErr != nil {
			break
		}
	}
	close(done)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if writeErr != nil {
		t.Fatal(writeErr)
	}
}

func assertConfigBytes(t *testing.T, path string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatal("config bytes changed unexpectedly")
	}
}
