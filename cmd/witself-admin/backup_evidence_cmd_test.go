package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeBackupEvidenceFixture lays down one complete, internally consistent
// artifact triple exactly as scripts/civo-pre-migration-backup.sh writes it.
func writeBackupEvidenceFixture(t *testing.T, root, cell, release, suffix string) string {
	t.Helper()
	createdAt := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	backupID := fmt.Sprintf("%s-pre-v%s-%s-%s", cell, release, createdAt.Format("20060102T150405Z"), suffix)
	dir := filepath.Join(root, backupID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	artifact := []byte("cli-fixture-ciphertext-" + suffix)
	sum := sha256.Sum256(artifact)
	digest := hex.EncodeToString(sum[:])
	write := func(name string, data []byte) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(backupID+".dump.age", artifact)
	write(backupID+".sha256", []byte(fmt.Sprintf("%s  %s\n", digest, backupID+".dump.age")))
	manifest := map[string]any{
		"schema":    "witself.civo-pre-migration-backup.v1",
		"backup_id": backupID,
		"source": map[string]any{
			"cell":                         cell,
			"kubernetes_context":           "civo-admin@witself",
			"postgresql_version_num":       180003,
			"schema_version":               91,
			"pgvector_extension_installed": true,
		},
		"target_release": release,
		"created_at":     createdAt.Format("2006-01-02T15:04:05Z"),
		"artifact": map[string]any{
			"file":               backupID + ".dump.age",
			"bytes":              len(artifact),
			"encryption":         "age",
			"checksum_algorithm": "sha256",
			"ciphertext_sha256":  digest,
			"checksum_file":      backupID + ".sha256",
		},
		"procedure": map[string]any{"script_sha256": strings.Repeat("ab", 32)},
		"restore_verification": map[string]any{
			"status":                            "verified",
			"verified_at":                       createdAt.Add(2 * time.Minute).Format("2006-01-02T15:04:05Z"),
			"network":                           "none",
			"plaintext_storage":                 "container tmpfs",
			"image_ref":                         "pgvector/pgvector:pg18",
			"image_id":                          "sha256:" + strings.Repeat("cd", 32),
			"schema_version":                    91,
			"public_table_count":                120,
			"account_count":                     3,
			"invalid_index_count":               0,
			"unvalidated_constraint_count":      0,
			"pgvector_extension_installed":      true,
			"pgvector_extension_matches_source": true,
			"disposable_target_cleaned":         true,
		},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	write(backupID+".json", raw)
	return dir
}

func TestBackupEvidenceVerifyCmdPass(t *testing.T) {
	root := t.TempDir()
	dirA := writeBackupEvidenceFixture(t, root, "civo-sandbox-use1-backup", "0.0.258", "0a1b2c3d")
	dirB := writeBackupEvidenceFixture(t, root, "civo-sandbox-usw2-dev", "0.0.258", "4e5f6071")
	if code := backupEvidenceCmd([]string{"verify", "--release", "0.0.258", dirA, dirB}); code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestBackupEvidenceVerifyCmdFailsOnMissingCell(t *testing.T) {
	root := t.TempDir()
	dir := writeBackupEvidenceFixture(t, root, "civo-sandbox-usw2-dev", "0.0.258", "0a1b2c3d")
	if code := backupEvidenceCmd([]string{"verify", "--release", "0.0.258", dir}); code != 1 {
		t.Fatalf("expected exit 1 for missing required cell, got %d", code)
	}
}

func TestBackupEvidenceVerifyCmdSingleCellFlag(t *testing.T) {
	root := t.TempDir()
	dir := writeBackupEvidenceFixture(t, root, "civo-sandbox-usw2-dev", "0.0.258", "0a1b2c3d")
	code := backupEvidenceCmd([]string{
		"verify", "--release", "0.0.258", "--cell", "civo-sandbox-usw2-dev", dir,
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestBackupEvidenceVerifyCmdReleaseMismatch(t *testing.T) {
	root := t.TempDir()
	dirA := writeBackupEvidenceFixture(t, root, "civo-sandbox-use1-backup", "0.0.258", "0a1b2c3d")
	dirB := writeBackupEvidenceFixture(t, root, "civo-sandbox-usw2-dev", "0.0.258", "4e5f6071")
	if code := backupEvidenceCmd([]string{"verify", "--release", "0.0.259", dirA, dirB}); code != 1 {
		t.Fatalf("expected exit 1 for release mismatch, got %d", code)
	}
}

func TestBackupEvidenceVerifyCmdUsageErrors(t *testing.T) {
	if code := backupEvidenceCmd(nil); code != 2 {
		t.Fatalf("expected exit 2 for missing subcommand, got %d", code)
	}
	if code := backupEvidenceCmd([]string{"bogus"}); code != 2 {
		t.Fatalf("expected exit 2 for unknown subcommand, got %d", code)
	}
	if code := backupEvidenceCmd([]string{"verify"}); code != 2 {
		t.Fatalf("expected exit 2 for missing release and dirs, got %d", code)
	}
	if code := backupEvidenceCmd([]string{"verify", "--release", "0.0.258"}); code != 2 {
		t.Fatalf("expected exit 2 for missing dirs, got %d", code)
	}
	if code := backupEvidenceCmd([]string{"verify", "--release", "0.0.258", "--max-age", "-1h", "dir"}); code != 2 {
		t.Fatalf("expected exit 2 for negative max-age, got %d", code)
	}
}

func TestBackupEvidenceVerifyCmdEvidenceOut(t *testing.T) {
	root := t.TempDir()
	dirA := writeBackupEvidenceFixture(t, root, "civo-sandbox-use1-backup", "0.0.258", "0a1b2c3d")
	dirB := writeBackupEvidenceFixture(t, root, "civo-sandbox-usw2-dev", "0.0.258", "4e5f6071")
	out := filepath.Join(t.TempDir(), "evidence.json")
	code := backupEvidenceCmd([]string{
		"verify", "--release", "0.0.258", "--evidence-out", out, dirA, dirB,
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	info, err := os.Lstat(out)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("evidence mode %v, want 0600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("evidence not valid JSON: %v", err)
	}
	if report["schema"] != "witself.civo-pre-migration-backup.verify.v1" || report["result"] != "pass" {
		t.Fatalf("unexpected evidence payload: %s", raw)
	}
	for _, needle := range []string{dirA, dirB, "backup_id", "civo-sandbox-usw2-dev"} {
		if strings.Contains(string(raw), needle) {
			t.Fatalf("evidence leaked %q: %s", needle, raw)
		}
	}
	// A second run must refuse to overwrite the retained evidence file.
	code = backupEvidenceCmd([]string{
		"verify", "--release", "0.0.258", "--evidence-out", out, dirA, dirB,
	})
	if code != 1 {
		t.Fatalf("expected exit 1 when evidence file exists, got %d", code)
	}
}
