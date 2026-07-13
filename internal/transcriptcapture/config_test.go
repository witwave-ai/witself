package transcriptcapture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigScopeIDsAreOptionalAndRoundTrip(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	loc, err := EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	legacy := Config{
		Runtime: RuntimeCodex, CaptureMode: ModeRaw,
		Account: "default", Realm: "default", Agent: "scott",
		AgentID: "agt_1", AgentName: "scott", Location: loc,
	}
	if err := SaveConfig(legacy); err != nil {
		t.Fatal(err)
	}
	path, err := ConfigPath(RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"account_id"`) || strings.Contains(string(raw), `"realm_id"`) {
		t.Fatalf("optional ids unexpectedly appeared in legacy-compatible config: %s", raw)
	}
	loaded, err := LoadConfig(RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AccountID != "" || loaded.RealmID != "" || loaded.AgentID != "agt_1" {
		t.Fatalf("legacy-compatible config = %#v", loaded)
	}

	loaded.AccountID = "acc_1"
	loaded.RealmID = "rlm_1"
	if err := SaveConfig(loaded); err != nil {
		t.Fatal(err)
	}
	loaded, err = LoadConfig(RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AccountID != "acc_1" || loaded.RealmID != "rlm_1" || loaded.AgentID != "agt_1" {
		t.Fatalf("scoped config = %#v", loaded)
	}
}
