package local

import (
	"errors"
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
