package gitopsvalues

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type retentionPolicyPair struct {
	enabled string
	mode    string
}

type retentionPolicyTable struct {
	cells []string
	rows  map[string]map[string]retentionPolicyPair
}

var retentionPolicyKeys = []string{"accountPurge", "agentEmailRetention", "transcriptRetention", "messageRetention"}

func parseRetentionPolicyTable(page []byte) (retentionPolicyTable, error) {
	table := retentionPolicyTable{rows: make(map[string]map[string]retentionPolicyPair)}
	lines := strings.Split(string(page), "\n")
	columns := func(line string) []string {
		return strings.Split(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "|"), "|"), "|")
	}
	header := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "|") && strings.TrimSpace(columns(line)[0]) == "Retention class / chart flags" {
			if header != -1 {
				return table, fmt.Errorf("multiple retention table headers")
			}
			header = i
		}
	}
	if header == -1 {
		return table, fmt.Errorf("missing retention table header")
	}
	token := regexp.MustCompile("`([a-z0-9-]+)`")
	seen := make(map[string]bool)
	for _, col := range columns(lines[header])[1:] {
		matches := token.FindAllStringSubmatch(col, -1)
		if len(matches) != 1 {
			return table, fmt.Errorf("header column must contain exactly one cell token")
		}
		cell := matches[0][1]
		if seen[cell] {
			return table, fmt.Errorf("duplicate header cell %s", cell)
		}
		seen[cell] = true
		table.cells = append(table.cells, cell)
	}
	setting := regexp.MustCompile("`worker\\.([^`]+)\\.enabled`")
	pair := regexp.MustCompile("^`(true|false)` / `(preview|enforce)`")
	for _, line := range lines[header+1:] {
		if !strings.HasPrefix(line, "|") {
			break
		}
		cols := columns(line)
		matches := setting.FindAllStringSubmatch(cols[0], -1)
		if len(matches) == 0 {
			continue
		}
		if len(matches) != 1 {
			return table, fmt.Errorf("settings row must name exactly one worker key")
		}
		key := matches[0][1]
		known := false
		for _, allowed := range retentionPolicyKeys {
			known = known || key == allowed
		}
		if !known {
			return table, fmt.Errorf("unknown retention worker key")
		}
		if _, exists := table.rows[key]; exists {
			return table, fmt.Errorf("duplicate retention row %s", key)
		}
		if len(cols) != len(table.cells)+1 {
			return table, fmt.Errorf("%s: settings row column count differs from header", key)
		}
		row := make(map[string]retentionPolicyPair)
		for i, cell := range table.cells {
			match := pair.FindStringSubmatch(strings.TrimSpace(cols[i+1]))
			if match == nil {
				return table, fmt.Errorf("%s: %s: missing backticked enabled / mode pair", cell, key)
			}
			row[cell] = retentionPolicyPair{match[1], match[2]}
		}
		table.rows[key] = row
	}
	for _, key := range retentionPolicyKeys {
		if _, exists := table.rows[key]; !exists {
			return table, fmt.Errorf("missing retention row %s", key)
		}
	}
	return table, nil
}

func effectiveRetentionSetting(key string, workers ...map[string]any) (bool, string, error) {
	var enabled bool
	var mode string
	var hasEnabled, hasMode bool
	for _, worker := range workers {
		value, exists := worker[key]
		if !exists {
			continue
		}
		block, ok := value.(map[string]any)
		if !ok {
			return false, "", fmt.Errorf("%s: worker block is not a map", key)
		}
		if value, exists := block["enabled"]; exists && !hasEnabled {
			enabled, ok = value.(bool)
			if !ok {
				return false, "", fmt.Errorf("%s.enabled is not a boolean", key)
			}
			hasEnabled = true
		}
		if value, exists := block["mode"]; exists && !hasMode {
			mode, ok = value.(string)
			if !ok {
				return false, "", fmt.Errorf("%s.mode is not a string", key)
			}
			hasMode = true
		}
		if hasEnabled && hasMode {
			return enabled, mode, nil
		}
	}
	return false, "", fmt.Errorf("%s: missing enabled or mode", key)
}

func effectiveWorkerEnabled(workers ...map[string]any) (bool, error) {
	for _, worker := range workers {
		if value, exists := worker["enabled"]; exists {
			enabled, ok := value.(bool)
			if !ok {
				return false, fmt.Errorf("worker.enabled is not a boolean")
			}
			return enabled, nil
		}
	}
	return false, fmt.Errorf("missing worker.enabled")
}

func checkRetentionPolicyTable(table retentionPolicyTable, wanted []string, lookup func(cell, key string) (bool, string, error)) error {
	cells := make(map[string]bool, len(wanted))
	for _, cell := range wanted {
		cells[cell] = true
	}
	for _, cell := range table.cells {
		if !cells[cell] {
			return fmt.Errorf("cell set: unexpected cell %s", cell)
		}
		delete(cells, cell)
	}
	for _, cell := range wanted {
		if cells[cell] {
			return fmt.Errorf("cell set: missing cell %s", cell)
		}
	}
	for _, cell := range table.cells {
		for _, key := range retentionPolicyKeys {
			enabled, mode, err := lookup(cell, key)
			if err != nil {
				return fmt.Errorf("%s: %s: %w", cell, key, err)
			}
			got := table.rows[key][cell]
			want := retentionPolicyPair{strconv.FormatBool(enabled), mode}
			if got != want {
				return fmt.Errorf("%s: %s: page pair %s / %s; wanted pair %s / %s", cell, key, got.enabled, got.mode, want.enabled, want.mode)
			}
		}
	}
	return nil
}

// Chart defaults are read at HEAD, not at each cell's pinned chart version.
// Every cell sets both fields of all four classes today, so the inheritance
// path runs only in the in-memory TestEffectiveRetentionSettingLayers.
func TestRetentionPolicyDocMatchesCheckedInCells(t *testing.T) {
	const guidance = "update the table, its dated lead-in, the Status block, the cadence sentence, the paragraph below the table and the account-purge paragraph of docs/data-retention-policy.md in the same change"
	// Also leave guidance when a shared repository/catalog helper fails.
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(guidance)
		}
	})
	fail := func(err error) {
		t.Helper()
		// Never expose a digest even if an invalid mode contains one.
		message := regexp.MustCompile(`[a-fA-F0-9]{64}`).ReplaceAllString(err.Error(), "<digest-redacted>")
		t.Fatalf("%s; %s", message, guidance)
	}
	root := repoRoot(t)
	page, err := os.ReadFile(filepath.Join(root, "docs/data-retention-policy.md"))
	if err != nil {
		fail(fmt.Errorf("read retention policy page: %w", err))
	}
	table, err := parseRetentionPolicyTable(page)
	if err != nil {
		fail(err)
	}
	// Preflight errors without dumping YAML values through the shared helper.
	cfg, err := loadCatalog(root)
	if err != nil {
		fail(fmt.Errorf("cannot load cell catalog"))
	}
	hasCells := false
	for _, cell := range cfg.Cells {
		hasCells = hasCells || isCivoRollCell(cell)
	}
	if !hasCells {
		fail(fmt.Errorf("catalog has no Civo roll cells"))
	}
	cells := civoRollCells(t)
	readWorker := func(path string, keys ...string) map[string]any {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			fail(fmt.Errorf("read %s: %w", path, err))
		}
		var values map[string]any
		if err := yaml.Unmarshal(body, &values); err != nil {
			fail(fmt.Errorf("decode %s: invalid YAML", path))
		}
		for _, key := range keys {
			if _, exists := values[key]; !exists {
				return nil // An omitted worker map inherits the next layer.
			}
			var ok bool
			values, ok = values[key].(map[string]any)
			if !ok {
				fail(fmt.Errorf("%s: %s is not a map", path, key))
			}
		}
		return values
	}
	apps := readWorker(".gitops/charts/apps/values.yaml", "apps", "witselfServer", "worker")
	server := readWorker("charts/witself-server/values.yaml", "worker")
	workers := make(map[string]map[string]any, len(cells))
	for _, cell := range cells {
		workers[cell] = readWorker(filepath.Join(".gitops/cells", cell, "values.yaml"), "apps", "witselfServer", "worker")
	}
	err = checkRetentionPolicyTable(table, cells, func(cell, key string) (bool, string, error) {
		return effectiveRetentionSetting(key, workers[cell], apps, server)
	})
	if err != nil {
		fail(err)
	}
	for _, cell := range cells {
		enabled, err := effectiveWorkerEnabled(workers[cell], apps, server)
		if err != nil {
			fail(fmt.Errorf("%s: %w", cell, err))
		}
		if !enabled {
			fail(fmt.Errorf("%s: worker.enabled is false; the table and the sentence below it assume that every listed cell sets it", cell))
		}
	}
}

func TestRetentionPolicyTableCheckDetectsDrift(t *testing.T) {
	const current = "| Retention class / chart flags | `civo-sandbox-use1-serving` (serving) | `civo-sandbox-use1-backup` (backup) | `civo-prod-use1-serving` (serving; onboarded, not yet provisioned; see note) |\n|---|---|---|---|\n| Account closure: `worker.accountPurge.enabled` / `.mode` | `true` / `enforce` | `true` / `enforce` | `true` / `enforce` |\n| Agent email: `worker.agentEmailRetention.enabled` / `.mode` | `true` / `enforce` | `true` / `preview` | `true` / `enforce` |\n| Transcripts: `worker.transcriptRetention.enabled` / `.mode` | `true` / `preview` | `true` / `preview` | `true` / `preview` |\n| Messages: `worker.messageRetention.enabled` / `.mode` | `true` / `preview` | `true` / `preview` | `true` / `preview` |\n| Audit trail | No implemented retention worker or chart flag | No implemented retention worker or chart flag | No implemented retention worker or chart flag |"
	const previous = "| Retention class / chart flags | `civo-sandbox-use1-serving` (serving) | `civo-sandbox-use1-backup` (rollback) |\n|---|---|---|\n| Account closure: `worker.accountPurge.enabled` / `.mode` | `true` / `enforce` | `true` / `enforce` |\n| Agent email: `worker.agentEmailRetention.enabled` / `.mode` | `true` / `enforce` | `false` / `preview` (inactive defaults) |\n| Transcripts: `worker.transcriptRetention.enabled` / `.mode` | `false` / `preview` (inactive defaults) | `false` / `preview` (inactive defaults) |\n| Messages: `worker.messageRetention.enabled` / `.mode` | `false` / `preview` (inactive defaults) | `false` / `preview` (inactive defaults) |\n| Audit trail | No implemented retention worker or chart flag | No implemented retention worker or chart flag |"
	cells := []string{"civo-sandbox-use1-serving", "civo-sandbox-use1-backup", "civo-prod-use1-serving"}
	lines := strings.Split(current, "\n")
	backupOff := strings.Replace(current, lines[3], strings.Replace(lines[3], "`true` / `preview`", "`false` / `preview`", 1), 1)
	transcriptEnforce := strings.Replace(current, lines[4], strings.Replace(lines[4], "`true` / `preview`", "`true` / `enforce`", 1), 1)
	cases := []struct {
		name        string
		page        string
		flipFixture bool
		wantError   []string
	}{
		{name: "current table", page: current},
		{name: "pre-slice table", page: previous, wantError: []string{"cell set"}},
		{name: "backup email disabled", page: backupOff, wantError: []string{cells[1], "agentEmailRetention", "page pair false / preview; wanted pair true / preview"}},
		{name: "transcript mode drift", page: transcriptEnforce, wantError: []string{cells[0], "transcriptRetention", "page pair true / enforce; wanted pair true / preview"}},
		{name: "reviewed transcript flip", page: transcriptEnforce, flipFixture: true},
		{name: "missing messages", page: strings.Replace(current, lines[5]+"\n", "", 1), wantError: []string{"missing retention row messageRetention"}},
		{name: "unknown fourth cell", page: strings.Join([]string{
			lines[0] + " `unknown-cell` |", lines[1] + "---|",
			lines[2] + " `true` / `enforce` |", lines[3] + " `true` / `enforce` |",
			lines[4] + " `true` / `preview` |", lines[5] + " `true` / `preview` |", lines[6],
		}, "\n"), wantError: []string{"cell set", "unknown-cell"}},
		{name: "missing backticks", page: strings.Replace(current, "`true` / `enforce`", "true / enforce", 1), wantError: []string{"missing backticked"}},
		{name: "no table", page: "# A page\n", wantError: []string{"missing retention table header"}},
		{name: "duplicate header", page: current + "\n\n" + current, wantError: []string{"multiple retention table headers"}},
		{name: "duplicate cell", page: strings.Replace(current, cells[1], cells[0], 1), wantError: []string{"duplicate header cell"}},
		{name: "two cell tokens", page: strings.Replace(current, "(backup)", "(backup; `another-cell`)", 1), wantError: []string{"exactly one cell token"}},
		{name: "invalid cell token", page: strings.Replace(current, cells[1], "Invalid_Cell", 1), wantError: []string{"exactly one cell token"}},
		{name: "unknown worker", page: strings.Replace(current, "worker.messageRetention.enabled", "worker.other.enabled", 1), wantError: []string{"unknown retention worker"}},
		{name: "duplicate worker", page: strings.Replace(current, lines[5], lines[4], 1), wantError: []string{"duplicate retention row"}},
		{name: "short settings row", page: strings.Replace(current, lines[2], strings.TrimSuffix(lines[2], " `true` / `enforce` |"), 1), wantError: []string{"column count"}},
		{name: "body stops at blank line", page: strings.Replace(current, lines[5], "\n"+lines[5], 1), wantError: []string{"missing retention row messageRetention"}},
		{name: "pair suffix allowed", page: strings.ReplaceAll(current, "`true` / `preview`", "`true` / `preview` (counts only)")},
		{name: "retired cell still listed", page: current, wantError: []string{"cell set", cells[2]}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			fixture := map[string]map[string]retentionPolicyPair{}
			for _, cell := range cells {
				fixture[cell] = map[string]retentionPolicyPair{
					"accountPurge":        {"true", "enforce"},
					"agentEmailRetention": {"true", "enforce"},
					"transcriptRetention": {"true", "preview"},
					"messageRetention":    {"true", "preview"},
				}
			}
			fixture[cells[1]]["agentEmailRetention"] = retentionPolicyPair{"true", "preview"}
			if tt.flipFixture {
				fixture[cells[0]]["transcriptRetention"] = retentionPolicyPair{"true", "enforce"}
			}
			wanted := cells
			if tt.name == "retired cell still listed" {
				wanted = cells[:2]
			}
			table, err := parseRetentionPolicyTable([]byte(tt.page))
			if err == nil {
				err = checkRetentionPolicyTable(table, wanted, func(cell, key string) (bool, string, error) {
					pair, ok := fixture[cell][key]
					if !ok {
						return false, "", fmt.Errorf("missing fixture pair")
					}
					return pair.enabled == "true", pair.mode, nil
				})
			}
			if len(tt.wantError) == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected drift or malformed table to fail")
			}
			for _, fragment := range tt.wantError {
				if !strings.Contains(err.Error(), fragment) {
					t.Errorf("error %q does not contain %q", err, fragment)
				}
			}
		})
	}
}

func TestEffectiveRetentionSettingLayers(t *testing.T) {
	block := func(fields map[string]any) map[string]any {
		return map[string]any{"transcriptRetention": fields}
	}
	defaults := block(map[string]any{"enabled": false, "mode": "preview"})
	cases := []struct {
		name    string
		layers  []map[string]any
		enabled bool
		mode    string
		wantErr bool
	}{
		{name: "cell sets both", layers: []map[string]any{block(map[string]any{"enabled": true, "mode": "enforce"}), defaults}, enabled: true, mode: "enforce"},
		{name: "inherit enabled", layers: []map[string]any{block(map[string]any{"mode": "enforce"}), defaults}, mode: "enforce"},
		{name: "inherit last layer", layers: []map[string]any{{}, {}, defaults}, mode: "preview"},
		{name: "missing block", layers: []map[string]any{{}, {}}, wantErr: true},
		{name: "inherit mode and preserve false", layers: []map[string]any{block(map[string]any{"enabled": false}), block(map[string]any{"enabled": true, "mode": "enforce"})}, mode: "enforce"},
		{name: "missing mode", layers: []map[string]any{block(map[string]any{"enabled": true})}, wantErr: true},
		{name: "missing enabled", layers: []map[string]any{block(map[string]any{"mode": "preview"})}, wantErr: true},
		{name: "wrong block type", layers: []map[string]any{{"transcriptRetention": true}, defaults}, wantErr: true},
		{name: "wrong enabled type", layers: []map[string]any{block(map[string]any{"enabled": "true"}), defaults}, wantErr: true},
		{name: "wrong mode type", layers: []map[string]any{block(map[string]any{"mode": true}), defaults}, wantErr: true},
		{name: "null enabled", layers: []map[string]any{block(map[string]any{"enabled": nil}), defaults}, wantErr: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			enabled, mode, err := effectiveRetentionSetting("transcriptRetention", tt.layers...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, want error %t", err, tt.wantErr)
			}
			if err == nil && (enabled != tt.enabled || mode != tt.mode) {
				t.Fatalf("got %t / %s, want %t / %s", enabled, mode, tt.enabled, tt.mode)
			}
		})
	}
	masterCases := []struct {
		name    string
		layers  []map[string]any
		enabled bool
		wantErr bool
	}{
		{name: "cell sets master", layers: []map[string]any{{"enabled": true}, {"enabled": false}}, enabled: true},
		{name: "inherit master", layers: []map[string]any{{}, {"enabled": true}}, enabled: true},
		{name: "missing master", layers: []map[string]any{{}, {}}, wantErr: true},
		{name: "cell disables master", layers: []map[string]any{{"enabled": false}, {"enabled": true}}},
		{name: "wrong master type", layers: []map[string]any{{"enabled": "true"}, {"enabled": true}}, wantErr: true},
		{name: "null master", layers: []map[string]any{{"enabled": nil}, {"enabled": true}}, wantErr: true},
	}
	for _, tt := range masterCases {
		t.Run(tt.name, func(t *testing.T) {
			enabled, err := effectiveWorkerEnabled(tt.layers...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, want error %t", err, tt.wantErr)
			}
			if err == nil && enabled != tt.enabled {
				t.Fatalf("got %t, want %t", enabled, tt.enabled)
			}
		})
	}
}
