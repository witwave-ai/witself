package local

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveResolveDelete(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())

	a := Account{ID: "acc_1", Email: "a@b.c"}
	if err := Save("default", a, "witself_opr_x"); err != nil {
		t.Fatal(err)
	}

	// Bare resolution finds "default".
	name, got, tok, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if name != "default" || got.ID != "acc_1" || tok != "witself_opr_x" {
		t.Errorf("resolved %q %+v %q", name, got, tok)
	}

	// Env wins over bare; explicit wins over env.
	if err := Save("work", Account{ID: "acc_2"}, "witself_opr_y"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITSELF_ACCOUNT", "work")
	if name, _, _, _ = Resolve(""); name != "work" {
		t.Errorf("env resolution = %q, want work", name)
	}
	if name, _, _, _ = Resolve("default"); name != "default" {
		t.Errorf("explicit resolution = %q, want default", name)
	}

	// No silent overwrite.
	if err := Save("default", a, "witself_opr_z"); !errors.Is(err, ErrNameTaken) {
		t.Errorf("duplicate save = %v, want ErrNameTaken", err)
	}

	// Delete retires binding + token.
	if err := Delete("work"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Resolve("work"); err == nil || !strings.Contains(err.Error(), "no local account") {
		t.Errorf("resolve after delete = %v, want not-found", err)
	}
}

func TestResolveMissing(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("WITSELF_ACCOUNT", "")
	if _, _, _, err := Resolve(""); err == nil {
		t.Error("want error when nothing is saved")
	}
}

func TestBadName(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	// Underscores are invalid — guarantees acc_... ids never collide with names.
	for _, bad := range []string{"Bad!", "acc_x", "-lead", ""} {
		if err := Save(bad, Account{}, "t"); err == nil {
			t.Errorf("Save(%q) succeeded, want error", bad)
		}
	}
}

func TestTokenLayoutAndLegacyFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)

	// New saves land in the per-account directory.
	if err := Save("test-account-1", Account{ID: "acc_1"}, "witself_opr_x"); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "tokens", "accounts", "test-account-1", "owner.token")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("token not at %s: %v", want, err)
	}

	// A legacy flat file still resolves, still counts as taken, and Delete
	// cleans it up.
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Accounts["old-name"] = Account{ID: "acc_2"}
	if err := write(cfg); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(home, "tokens", "accounts", "old-name.token")
	if err := os.WriteFile(legacy, []byte("witself_opr_legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, tok, err := Resolve("old-name"); err != nil || tok != "witself_opr_legacy" {
		t.Errorf("legacy resolve = %q, %v", tok, err)
	}
	delete(cfg.Accounts, "old-name")
	if err := write(cfg); err != nil {
		t.Fatal(err)
	}
	if err := Save("old-name", Account{ID: "acc_3"}, "t"); !errors.Is(err, ErrNameTaken) {
		t.Errorf("save over legacy file = %v, want ErrNameTaken", err)
	}
	cfg.Accounts["old-name"] = Account{ID: "acc_2"}
	if err := write(cfg); err != nil {
		t.Fatal(err)
	}
	if err := Delete("old-name"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("legacy file survived delete: %v", err)
	}
}

func TestAgentTokenLayoutAndLegacyFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)

	canonical, err := AgentTokenPath("default", "default", "scott")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "tokens", "accounts", "default", "realms", "default", "agents", "scott.token")
	if canonical != want {
		t.Fatalf("agent token path = %q, want %q", canonical, want)
	}
	if err := os.MkdirAll(filepath.Dir(canonical), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canonical, []byte("witself_agt_scott\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, path, err := ReadAgentToken("default", "default", "scott")
	if err != nil || tok != "witself_agt_scott" || path != canonical {
		t.Fatalf("canonical agent token = %q from %q, err %v", tok, path, err)
	}

	if err := os.Remove(canonical); err != nil {
		t.Fatal(err)
	}
	legacy, err := legacyAgentTokenPath("default", "scott")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("witself_agt_legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, path, err = ReadAgentToken("default", "default", "scott")
	if err != nil || tok != "witself_agt_legacy" || path != legacy {
		t.Fatalf("legacy agent token = %q from %q, err %v", tok, path, err)
	}
}

func TestAgentTokenPathRejectsTraversal(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	for _, tc := range []struct {
		account string
		realm   string
		agent   string
	}{
		{account: "../other", realm: "default", agent: "scott"},
		{account: "default", realm: "../other", agent: "scott"},
		{account: "default", realm: "default", agent: "../other"},
	} {
		if _, err := AgentTokenPath(tc.account, tc.realm, tc.agent); err == nil {
			t.Fatalf("AgentTokenPath(%q, %q, %q) succeeded", tc.account, tc.realm, tc.agent)
		}
	}
}
