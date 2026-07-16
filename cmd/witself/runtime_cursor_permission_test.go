package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCursorMCPPermissionMergeIsIdempotentAndPreservesUserConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CURSOR_CONFIG_DIR", root)
	path := filepath.Join(root, "cli-config.json")
	original := []byte(`{
  "version": 1,
  "permissions": {
    "allow": ["Shell(ls)"],
    "deny": ["Shell(rm:*)"]
  },
  "display": {"showLineNumbers": true}
}
`)
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}

	snapshot, err := snapshotCursorCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	changed, err := snapshot.ensureWitselfMCPPermission()
	if err != nil || !changed {
		t.Fatalf("first merge changed=%t err=%v", changed, err)
	}
	again, err := snapshotCursorCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	changed, err = again.ensureWitselfMCPPermission()
	if err != nil || changed {
		t.Fatalf("second merge changed=%t err=%v", changed, err)
	}
	rootObject := readTestJSONObject(t, path)
	permissions := rootObject["permissions"].(map[string]any)
	allow := permissions["allow"].([]any)
	if len(allow) != 2 || allow[0] != "Shell(ls)" || allow[1] != cursorWitselfMCPPermission {
		t.Fatalf("allow = %#v", allow)
	}
	if permissions["deny"].([]any)[0] != "Shell(rm:*)" ||
		rootObject["display"].(map[string]any)["showLineNumbers"] != true {
		t.Fatalf("unrelated Cursor config changed: %#v", rootObject)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("merged mode = %04o", info.Mode().Perm())
	}

	if err := snapshot.restore(); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, original) {
		t.Fatalf("restored config = %q, want %q", restored, original)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("restored mode = %04o", info.Mode().Perm())
	}
}

func TestCursorMCPPermissionRemovalPreservesUnrelatedRules(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CURSOR_CONFIG_DIR", root)
	path := filepath.Join(root, "cli-config.json")
	if err := os.WriteFile(path, []byte(`{"permissions":{"allow":["Shell(ls)","Mcp(witself:*)"],"deny":[]},"other":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := snapshotCursorCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	changed, err := snapshot.removeWitselfMCPPermission()
	if err != nil || !changed {
		t.Fatalf("remove changed=%t err=%v", changed, err)
	}
	rootObject := readTestJSONObject(t, path)
	permissions := rootObject["permissions"].(map[string]any)
	allow := permissions["allow"].([]any)
	if len(allow) != 1 || allow[0] != "Shell(ls)" || rootObject["other"] != true {
		t.Fatalf("Cursor config after removal = %#v", rootObject)
	}
}

func TestCursorMCPPermissionRemovalPreservesDuplicateUserRule(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CURSOR_CONFIG_DIR", root)
	path := filepath.Join(root, "cli-config.json")
	if err := os.WriteFile(path, []byte(`{"permissions":{"allow":["Mcp(witself:*)","Shell(ls)","Mcp(witself:*)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := snapshotCursorCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	changed, err := snapshot.removeWitselfMCPPermission()
	if err != nil || !changed {
		t.Fatalf("remove changed=%t err=%v", changed, err)
	}
	rootObject := readTestJSONObject(t, path)
	allow := rootObject["permissions"].(map[string]any)["allow"].([]any)
	if len(allow) != 2 || allow[0] != "Shell(ls)" || allow[1] != cursorWitselfMCPPermission {
		t.Fatalf("duplicate user permission was not preserved: %#v", allow)
	}
	if err := snapshot.restore(); err != nil {
		t.Fatal(err)
	}
	restored := readTestJSONObject(t, path)
	allow = restored["permissions"].(map[string]any)["allow"].([]any)
	if cursorPermissionCount(allow) != 2 {
		t.Fatalf("rollback restored %d permissions, want 2: %#v", cursorPermissionCount(allow), allow)
	}
}

func TestCursorMCPPermissionRejectsMalformedAllowListWithoutChanges(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CURSOR_CONFIG_DIR", root)
	path := filepath.Join(root, "cli-config.json")
	original := []byte(`{"permissions":{"allow":"Mcp(witself:*)"}}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotCursorCLIConfig(); err == nil {
		t.Fatal("malformed permissions.allow was accepted")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("malformed config changed: %q", got)
	}
}

func TestCursorMCPPermissionPreservesSymlinkedCLIConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CURSOR_CONFIG_DIR", root)
	targetDir := t.TempDir()
	targetPath := filepath.Join(targetDir, "cursor-cli-config.json")
	original := []byte(`{"permissions":{"allow":["Shell(ls)"]},"target":true}`)
	if err := os.WriteFile(targetPath, original, 0o640); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "cli-config.json")
	if err := os.Symlink(targetPath, configPath); err != nil {
		t.Fatal(err)
	}

	snapshot, err := snapshotCursorCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	changed, err := snapshot.ensureWitselfMCPPermission()
	if err != nil || !changed {
		t.Fatalf("merge changed=%t err=%v", changed, err)
	}
	linkInfo, err := os.Lstat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatal("Cursor cli-config.json symlink was replaced")
	}
	rootObject := readTestJSONObject(t, targetPath)
	allow := rootObject["permissions"].(map[string]any)["allow"].([]any)
	if len(allow) != 2 || allow[1] != cursorWitselfMCPPermission {
		t.Fatalf("target allow list = %#v", allow)
	}

	if err := snapshot.restore(); err != nil {
		t.Fatal(err)
	}
	linkInfo, err = os.Lstat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatal("Cursor cli-config.json symlink was replaced during rollback")
	}
	restored, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, original) {
		t.Fatalf("restored symlink target = %q, want %q", restored, original)
	}
}

func TestCursorMCPPermissionRejectsNullAndNonObjectConfig(t *testing.T) {
	for _, content := range []string{"null", "[]", `"value"`} {
		t.Run(content, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("CURSOR_CONFIG_DIR", root)
			path := filepath.Join(root, "cli-config.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := snapshotCursorCLIConfig(); err == nil {
				t.Fatalf("top-level %s config was accepted", content)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != content {
				t.Fatalf("invalid config changed to %q", got)
			}
		})
	}
}

func TestCursorMCPPermissionRetainsConcurrentUnrelatedChanges(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CURSOR_CONFIG_DIR", root)
	path := filepath.Join(root, "cli-config.json")
	if err := os.WriteFile(path, []byte(`{"base":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	snapshot, err := snapshotCursorCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"base":true,"before_merge":"kept"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := snapshot.ensureWitselfMCPPermission()
	if err != nil || !changed {
		t.Fatalf("merge changed=%t err=%v", changed, err)
	}
	merged := readTestJSONObject(t, path)
	if merged["before_merge"] != "kept" {
		t.Fatalf("pre-merge concurrent change lost: %#v", merged)
	}
	merged["after_merge"] = "kept"
	concurrentRaw, err := json.Marshal(merged)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, concurrentRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := snapshot.restore(); err != nil {
		t.Fatal(err)
	}
	restored := readTestJSONObject(t, path)
	if restored["base"] != true || restored["before_merge"] != "kept" || restored["after_merge"] != "kept" {
		t.Fatalf("concurrent changes lost during rollback: %#v", restored)
	}
	permissions, _ := restored["permissions"].(map[string]any)
	if permissions != nil {
		allow, _ := permissions["allow"].([]any)
		if cursorPermissionCount(allow) != 0 {
			t.Fatalf("Witself permission remains after rollback: %#v", restored)
		}
	}
}

func TestCursorMCPPermissionRollbackRefusesAmbiguousDuplicate(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CURSOR_CONFIG_DIR", root)
	path := filepath.Join(root, "cli-config.json")
	if err := os.WriteFile(path, []byte(`{"permissions":{"allow":["Shell(ls)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := snapshotCursorCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	changed, err := snapshot.ensureWitselfMCPPermission()
	if err != nil || !changed {
		t.Fatalf("merge changed=%t err=%v", changed, err)
	}
	rootObject := readTestJSONObject(t, path)
	permissions := rootObject["permissions"].(map[string]any)
	allow := permissions["allow"].([]any)
	permissions["allow"] = append([]any{cursorWitselfMCPPermission}, allow...)
	raw, err := json.Marshal(rootObject)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := snapshot.restore(); err == nil {
		t.Fatal("ambiguous duplicate permission rollback succeeded")
	}
	got := readTestJSONObject(t, path)
	gotAllow := got["permissions"].(map[string]any)["allow"].([]any)
	if cursorPermissionCount(gotAllow) != 2 {
		t.Fatalf("ambiguous permissions were changed: %#v", gotAllow)
	}
}
