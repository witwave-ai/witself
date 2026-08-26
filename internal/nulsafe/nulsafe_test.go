package nulsafe

import (
	"encoding/json"
	"strings"
	"testing"
)

// esc builds JSON text containing the NUL escape without ever putting the
// escape sequence into this file as an interpreted literal.
func esc(parts ...string) string { return strings.Join(parts, `\u0000`) }

func TestReplaceString(t *testing.T) {
	if got := ReplaceString("plain"); got != "plain" {
		t.Fatalf("plain string changed: %q", got)
	}
	in := "a\x00b\x00"
	want := "a�b�"
	if got := ReplaceString(in); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if !ContainsString(in) || ContainsString(want) {
		t.Fatal("ContainsString disagrees with ReplaceString")
	}
}

func TestSanitizeJSONPassthroughWithoutNUL(t *testing.T) {
	raw := json.RawMessage(`{"z":1,"a":{"nested":[1,2,"x"]},"n":12345678901234567890.5}`)
	out, changed, err := SanitizeJSON(raw)
	if err != nil || changed {
		t.Fatalf("unexpected: changed=%v err=%v", changed, err)
	}
	if string(out) != string(raw) {
		t.Fatalf("bytes changed on passthrough: %s", out)
	}
}

func TestSanitizeJSONReplacesDeepNULs(t *testing.T) {
	raw := json.RawMessage(`{"k` + esc("", "") + `ey":{"list":["a` + esc("", "") + `b",{"deep":"` + esc("", "") + `"}],"num":42}}`)
	out, changed, err := SanitizeJSON(raw)
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	if !changed {
		t.Fatal("expected change")
	}
	if strings.Contains(string(out), `\u0000`) {
		t.Fatalf("escape survived: %s", out)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	inner, ok := decoded["k�ey"].(map[string]any)
	if !ok {
		t.Fatalf("sanitized key missing: %v", decoded)
	}
	list := inner["list"].([]any)
	if list[0] != "a�b" {
		t.Fatalf("value not sanitized: %q", list[0])
	}
	if list[1].(map[string]any)["deep"] != "�" {
		t.Fatalf("deep value not sanitized: %v", list[1])
	}
	if inner["num"].(float64) != 42 {
		t.Fatalf("number corrupted: %v", inner["num"])
	}
}

func TestSanitizeJSONPreservesEscapedBackslashText(t *testing.T) {
	// The six bytes backslash-backslash-u-0-0-0-0 are the two-character text
	// `\u0000` spelled with an escaped backslash — literal text, not a NUL.
	raw := json.RawMessage(`{"text":"\\u0000 stays"}`)
	out, changed, err := SanitizeJSON(raw)
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	if changed {
		t.Fatalf("escaped-backslash text must not change: %s", out)
	}
}

func TestSanitizeJSONPreservesExactNumberText(t *testing.T) {
	raw := json.RawMessage(`{"big":123456789012345678901234567890,"nul":"` + esc("", "") + `"}`)
	out, _, err := SanitizeJSON(raw)
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	if !strings.Contains(string(out), "123456789012345678901234567890") {
		t.Fatalf("number text altered: %s", out)
	}
}

func TestSanitizeJSONRejectsInvalidJSON(t *testing.T) {
	if _, _, err := SanitizeJSON(json.RawMessage(`{"a":` + esc("", "") + `}`)); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	if _, _, err := SanitizeJSON(json.RawMessage(`{"a":"` + esc("", "") + `"} trailing`)); err == nil {
		t.Fatal("trailing content accepted")
	}
}

func TestSanitizeJSONEmptyInput(t *testing.T) {
	out, changed, err := SanitizeJSON(nil)
	if err != nil || changed || out != nil {
		t.Fatalf("empty input mishandled: %v %v %v", out, changed, err)
	}
}
