package gitopsvalues

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupValidationArchiveOriginOverlay(t *testing.T) {
	generated, err := generateAll(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range generated {
		present := bytes.Contains(data, []byte("validationArchiveOrigin: https://self.witwave.ai"))
		if present != (name == "civo-sandbox-use1-backup" || name == "civo-sandbox-use1-serving" || name == "civo-prod-use1-serving") {
			t.Errorf("unexpected archive origin configuration for %s", name)
		}
	}
}

func TestBackupValidationArchiveOriginChart(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatal("NOT RUN: helm is required for the archive-origin chart contract")
	}
	chart := filepath.Join(repoRoot(t), "charts", "witself-server")
	for _, origin := range []string{"", "https://self.witwave.ai"} {
		cmd := exec.Command("helm", "template", "test", chart, "--show-only", "templates/configmap.yaml", "--set-string", "backup.validation.archiveOrigin="+origin)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("helm template failed: %v", err)
		}
		key := "WITSELF_BACKUP_VALIDATION_ARCHIVE_ORIGIN:"
		if strings.Contains(string(output), key) != (origin != "") {
			t.Fatal("archive origin must render only when configured")
		}
		if origin != "" && !strings.Contains(string(output), key+" \""+origin+"\"") {
			t.Fatal("archive origin must render exactly")
		}
	}
}
