package gitopsvalues

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

var requestBudgetFixture = RequestBudgetSwitch{
	ReadPerMinute: 600, ReadBurst: 60, WritePerMinute: 120, WriteBurst: 20, MaxTrackedAccounts: 10000,
}

const requestBudgetBlock = `    # From catalog switches.request_budget: per account, per minute, per replica.
    requestBudget:
      enabled: true
      readPerMinute: 600
      readBurst: 60
      writePerMinute: 120
      writeBurst: 20
      maxTrackedAccounts: 10000
`

var requestBudgetCells = map[string]bool{}

func insertRequestBudgetBlock(t *testing.T, body []byte) []byte {
	t.Helper()
	const marker = "\n    worker:\n"
	if bytes.Count(body, []byte(marker)) != 1 {
		t.Fatal("request budget insertion requires exactly one worker block")
	}
	return bytes.Replace(body, []byte(marker), []byte("\n"+requestBudgetBlock+"    worker:\n"), 1)
}

func requestBudgetReady(t *testing.T) string {
	t.Helper()
	body := readRequestBudgetTestFile(t, ".gitops/charts/apps/templates/witself-server.yaml")
	matches := regexp.MustCompile(`(?m)^\{\{- \$requestBudgetMinVersion := "(0\.0\.[0-9]+)" -\}\}$`).FindAllStringSubmatch(body, -1)
	if len(matches) != 1 {
		t.Fatal("apps template must declare requestBudgetMinVersion exactly once")
	}
	return matches[0][1]
}

func readRequestBudgetTestFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), path))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestLoadCatalogRequestBudgetSwitch(t *testing.T) {
	committed, err := loadCatalog(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	header, err := yaml.Marshal(struct {
		Schema   string          `yaml:"schema"`
		Gitops   GitopsDefaults  `yaml:"gitops"`
		Defaults CatalogDefaults `yaml:"defaults"`
	}{committed.Schema, committed.Gitops, committed.Defaults})
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{"read_per_minute", "read_burst", "write_per_minute", "write_burst", "max_tracked_accounts"}
	fixture := []int{requestBudgetFixture.ReadPerMinute, requestBudgetFixture.ReadBurst, requestBudgetFixture.WritePerMinute, requestBudgetFixture.WriteBurst, requestBudgetFixture.MaxTrackedAccounts}
	block := func(overrides map[string]string, omitted string) string {
		var b strings.Builder
		b.WriteString("      request_budget:\n")
		for i, key := range keys {
			if key == omitted {
				continue
			}
			value := strconv.Itoa(fixture[i])
			if override, ok := overrides[key]; ok {
				value = override
			}
			fmt.Fprintf(&b, "        %s: %s\n", key, value)
		}
		return b.String()
	}
	type testCase struct {
		name, serving, gcp, errorCell, errorSuffix, parseContains string
		servingFixture                                            bool
	}
	const serving = "civo-sandbox-use1-serving"
	const gcp = "gcp-sandbox-use1-dev"
	rateError := func(key string) string {
		return "switches.request_budget." + key + " must be an integer from 1 to 1000000"
	}
	const maxError = "switches.request_budget.max_tracked_accounts must be an integer from 100 to 1000000"
	const mappingError = "switches.request_budget must be a mapping of five integers; remove the key to turn it off"
	cases := []testCase{
		{name: "fixture", serving: block(nil, ""), servingFixture: true},
		{name: "absent"},
		{name: "rate error order", serving: block(map[string]string{"read_burst": "0", "write_burst": "0"}, ""), errorCell: serving, errorSuffix: rateError("read_burst")},
		{name: "without overlay", gcp: block(nil, ""), errorCell: gcp, errorSuffix: "sets switches.request_budget without an overlay; only overlay templates render it"},
		{name: "unknown enabled", serving: block(nil, "") + "        enabled: true\n", parseContains: "field enabled not found"},
		{name: "noninteger", serving: block(map[string]string{"read_per_minute": "1.5"}, ""), parseContains: "cannot unmarshal"},
		{name: "nonnumeric", serving: block(map[string]string{"read_per_minute": "abc"}, ""), parseContains: "cannot unmarshal"},
		{name: "nonmapping", serving: "      request_budget: true\n", parseContains: "cannot unmarshal"},
	}
	for _, key := range keys[1:] {
		cases = append(cases, testCase{name: key + "/noninteger", serving: block(map[string]string{key: "100.5"}, ""), parseContains: "cannot unmarshal"})
	}
	for _, key := range keys[:4] {
		for _, value := range []int{0, -1, 1000001, 1, 1000000} {
			tc := testCase{name: fmt.Sprintf("%s/%d", key, value), serving: block(map[string]string{key: strconv.Itoa(value)}, "")}
			if value < 1 || value > 1000000 {
				tc.errorCell, tc.errorSuffix = serving, rateError(key)
			}
			cases = append(cases, tc)
		}
		cases = append(cases, testCase{name: key + "/omitted", serving: block(nil, key), errorCell: serving, errorSuffix: rateError(key)})
	}
	for _, value := range []int{99, 1000001, 100, 1000000} {
		tc := testCase{name: fmt.Sprintf("max_tracked_accounts/%d", value), serving: block(map[string]string{"max_tracked_accounts": strconv.Itoa(value)}, "")}
		if value < 100 || value > 1000000 {
			tc.errorCell, tc.errorSuffix = serving, maxError
		}
		cases = append(cases, tc)
	}
	cases = append(cases, testCase{name: "max_tracked_accounts/omitted", serving: block(nil, "max_tracked_accounts"), errorCell: serving, errorSuffix: maxError})
	for _, value := range []string{"", " null"} {
		for _, cell := range []string{serving, gcp} {
			tc := testCase{name: cell + "/null" + value, errorCell: cell, errorSuffix: mappingError}
			if cell == serving {
				tc.serving = "      request_budget:" + value + "\n"
			} else {
				tc.gcp = "      request_budget:" + value + "\n"
			}
			cases = append(cases, tc)
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, catalogRelPath)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			switches := func(s string) string {
				if s == "" {
					return "    switches: {}\n"
				}
				return "    switches:\n" + s
			}
			body := string(header) + "cells:\n  " + serving + ":\n    cloud: civo\n    region: nyc1\n    role: serving\n    overlay: civo-sandbox-use1-serving.yaml.tmpl\n" + switches(tc.serving) + "  " + gcp + ":\n    cloud: gcp\n    region: us-east1\n    gcp_project: witself-sandbox\n" + switches(tc.gcp)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadCatalog(root)
			if tc.parseContains != "" {
				if err == nil || !strings.HasPrefix(err.Error(), "parse "+path+": ") || !strings.Contains(err.Error(), tc.parseContains) {
					t.Fatalf("catalog parse refusal mismatch: %v", err)
				}
				return
			}
			if tc.errorSuffix != "" {
				want := fmt.Sprintf("%s: cell %q %s", path, tc.errorCell, tc.errorSuffix)
				if err == nil || err.Error() != want {
					t.Fatalf("catalog validation mismatch: got %v, want %s", err, want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.servingFixture && !reflect.DeepEqual(cfg.Cells[serving].Switches.RequestBudget, &requestBudgetFixture) {
				t.Fatal("fixture did not decode into request budget")
			}
			if tc.serving == "" && cfg.Cells[serving].Switches.RequestBudget != nil {
				t.Fatal("absent serving request budget must be nil")
			}
			if cfg.Cells[gcp].Switches.RequestBudget != nil {
				t.Fatal("absent GCP request budget must be nil")
			}
		})
	}
}

func TestRequestBudgetReachesTemplateData(t *testing.T) {
	cfg, err := loadCatalog(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for name := range cfg.Cells {
		data, err := resolveCell(name, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if (data.RequestBudget != nil) != requestBudgetCells[name] {
			t.Errorf("%s: template data request budget differs from allow-list", name)
		}
	}
	for _, name := range civoRollCells(t) {
		fixture := requestBudgetFixture
		cell := cfg.Cells[name]
		cell.Switches.RequestBudget = &fixture
		cfg.Cells[name] = cell
		data, err := resolveCell(name, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(data.RequestBudget, &fixture) {
			t.Errorf("%s: request budget did not reach template data", name)
		}
	}
}

func TestRequestBudgetOverlayTemplates(t *testing.T) {
	names, err := fs.Glob(embedded, "overlays/*.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"civo-sandbox-use1-serving", "civo-sandbox-use1-backup", "civo-prod-use1-serving"} {
		found := false
		for _, name := range names {
			found = found || name == "overlays/"+required+".yaml.tmpl"
		}
		if !found {
			t.Errorf("%s: required overlay missing", required)
		}
	}
	for _, name := range names {
		t.Run(filepath.Base(name), func(t *testing.T) {
			data := templateData{ChartVersion: "0.0.1", ImageTag: "0.0.1", CivoPostgresChartVersion: "1.0.0", CertManagerChartVersion: "v1.0.0"}
			var without, with bytes.Buffer
			if err := templates.ExecuteTemplate(&without, filepath.Base(name), data); err != nil {
				t.Fatal(err)
			}
			data.RequestBudget = &requestBudgetFixture
			if err := templates.ExecuteTemplate(&with, filepath.Base(name), data); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(without.Bytes(), []byte("requestBudget")) {
				t.Fatal("nil request budget must not render")
			}
			if !bytes.Equal(with.Bytes(), insertRequestBudgetBlock(t, without.Bytes())) {
				t.Fatal("request budget block differs from the exact insertion before worker")
			}
			var values struct {
				Apps struct {
					WitselfServer struct {
						RequestBudget map[string]any `yaml:"requestBudget"`
					} `yaml:"witselfServer"`
				} `yaml:"apps"`
			}
			if err := yaml.Unmarshal(with.Bytes(), &values); err != nil {
				t.Fatalf("decode overlay: %v", err)
			}
			want := map[string]any{"enabled": true, "readPerMinute": 600, "readBurst": 60, "writePerMinute": 120, "writeBurst": 20, "maxTrackedAccounts": 10000}
			if !reflect.DeepEqual(values.Apps.WitselfServer.RequestBudget, want) {
				t.Fatal("decoded overlay request budget differs from fixture")
			}
		})
	}
}

func TestGeneratedValuesRequestBudgetFollowsAllowList(t *testing.T) {
	root := repoRoot(t)
	generated, err := generateAll(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"civo-sandbox-use1-serving", "civo-sandbox-use1-backup", "civo-prod-use1-serving"} {
		if _, ok := generated[name]; !ok {
			t.Errorf("%s: required generated cell missing", name)
		}
	}
	for name, body := range generated {
		if bytes.Contains(body, []byte("requestBudget")) != requestBudgetCells[name] || (cfg.Cells[name].Switches.RequestBudget != nil) != requestBudgetCells[name] {
			t.Errorf("%s: request budget must stay off unless the cell is in requestBudgetCells (founder decision)", name)
		}
	}
}

func TestGeneratedValuesRequestBudgetOptIn(t *testing.T) {
	root := repoRoot(t)
	charts, err := loadChartPins(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range civoRollCells(t) {
		t.Run(name, func(t *testing.T) {
			without, err := loadCatalog(root)
			if err != nil {
				t.Fatal(err)
			}
			with, err := loadCatalog(root)
			if err != nil {
				t.Fatal(err)
			}
			cell := without.Cells[name]
			cell.Switches.RequestBudget = nil
			without.Cells[name] = cell
			fixture := requestBudgetFixture
			cell.Switches.RequestBudget = &fixture
			with.Cells[name] = cell
			base, err := generateCell(root, name, without, charts)
			if err != nil {
				t.Fatal(err)
			}
			enabled, err := generateCell(root, name, with, charts)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(enabled, insertRequestBudgetBlock(t, base)) {
				t.Fatal("opt-in must add exactly the request budget block before worker")
			}
			rolled, err := generateCellWithImagePins(root, name, with, charts, &serverPins{ChartVersion: "0.0.334", ImageTag: "0.0.334", ImageDigest: "sha256:" + strings.Repeat("c", 64)}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Count(rolled, []byte(requestBudgetBlock)) != 1 || !bytes.Contains(rolled, []byte("imageTag: 0.0.334")) || !bytes.Contains(rolled, []byte("imageDigest: ")) {
				t.Fatal("roll must retain exactly one request budget block and render new image pins")
			}
		})
	}
}

func TestRequestBudgetSuiteIsWired(t *testing.T) {
	body := readRequestBudgetTestFile(t, "scripts/test-helm-rollout.sh")
	const want = `ruby "$repo_root/scripts/testdata/apps-server-request-budget.rb" "$repo_root"`
	if countExactRequestBudgetLines(body, want) != 1 {
		t.Fatal("helm rollout must run the apps request budget suite exactly once")
	}
}

func TestRollStateFixtureRollsToCommittedVersion(t *testing.T) {
	body := readRequestBudgetTestFile(t, "scripts/lib/test-cell-roll-states.sh")
	const want = `    version = values.fetch("apps").fetch("witselfServer").fetch("imageTag")`
	if countExactRequestBudgetLines(body, want) != 1 || regexp.MustCompile(`(?m)^\s*version = "`).MatchString(body) {
		t.Fatal("roll-state fixture must use each cell's committed image version exactly once")
	}
}

func countExactRequestBudgetLines(body, want string) int {
	count := 0
	for _, line := range strings.Split(body, "\n") {
		if line == want {
			count++
		}
	}
	return count
}

func TestOldPinCivoRendersPinRequestBudgetOff(t *testing.T) {
	parseVersion := func(version string) [3]int {
		var result [3]int
		parts := strings.Split(version, ".")
		if len(parts) != len(result) {
			t.Fatal("request budget gate version must have three integer components")
		}
		for i, part := range parts {
			value, err := strconv.Atoi(part)
			if err != nil {
				t.Fatal("request budget gate version must have three integer components")
			}
			result[i] = value
		}
		return result
	}
	ready := parseVersion(requestBudgetReady(t))
	old := func(version string) bool {
		got := parseVersion(version)
		for i := range ready {
			if got[i] != ready[i] {
				return got[i] < ready[i]
			}
		}
		return false
	}
	t.Run("helm rollout", func(t *testing.T) {
		body := readRequestBudgetTestFile(t, "scripts/test-helm-rollout.sh")
		versions := regexp.MustCompile(`apps\.witselfServer\.(?:chartVersion|imageTag)=(0\.0\.[0-9]+)`)
		count, first := 0, 1
		var joined strings.Builder
		for i, line := range strings.Split(body, "\n") {
			if joined.Len() == 0 {
				first = i + 1
			}
			if strings.HasSuffix(line, "\\") {
				joined.WriteString(strings.TrimSuffix(line, "\\"))
				joined.WriteByte(' ')
				continue
			}
			joined.WriteString(line)
			command := joined.String()
			joined.Reset()
			rendersCivoCell := strings.Contains(command, `--values "$civo_cell"`) ||
				strings.Contains(command, `--values "$civo_backup_cell"`) ||
				strings.Contains(command, `--values "$civo_recovery_cell"`)
			if !strings.Contains(command, `helm template witself-apps "$apps_chart"`) || !rendersCivoCell {
				continue
			}
			qualifies := strings.Contains(command, `--values "$apps_email_production_profile"`)
			for _, match := range versions.FindAllStringSubmatch(command, -1) {
				qualifies = qualifies || old(match[1])
			}
			if qualifies {
				count++
				if !strings.Contains(command, "--set apps.witselfServer.requestBudget.enabled=false") {
					t.Errorf("scripts/test-helm-rollout.sh:%d: old-pin Civo render must pin request budget off", first)
				}
			}
		}
		if count < 9 {
			t.Errorf("old-pin Civo render guard found %d commands, want at least nine", count)
		}
	})
	t.Run("monitoring extensions", func(t *testing.T) {
		body := readRequestBudgetTestFile(t, "scripts/testdata/monitoring-extensions.rb")
		versions := regexp.MustCompile(`apps\.witselfServer\.chartVersion=(0\.0\.[0-9]+)`)
		count := 0
		for i, line := range strings.Split(body, "\n") {
			for _, match := range versions.FindAllStringSubmatch(line, -1) {
				if old(match[1]) {
					count++
					if !strings.Contains(line, `"apps.witselfServer.requestBudget.enabled=false"`) {
						t.Errorf("scripts/testdata/monitoring-extensions.rb:%d: old-pin Civo render must pin request budget off", i+1)
					}
				}
			}
		}
		if count == 0 {
			t.Fatal("monitoring request budget guard found no old-pin render")
		}
	})
	t.Run("egress helper", func(t *testing.T) {
		body := readRequestBudgetTestFile(t, "scripts/testdata/egress-networkpolicy.rb")
		const want = "  overrides = {'apps.witselfServer.requestBudget.enabled' => false}.merge(overrides) if release == 'witself-apps'"
		if countExactRequestBudgetLines(body, want) != 1 {
			t.Fatal("egress helper must pin request budget off exactly once")
		}
		start, pin, capture := -1, -1, -1
		for i, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "def helm(") {
				start = i
			}
			if line == want {
				pin = i
			}
			if start >= 0 && capture < 0 && strings.Contains(line, "capture(*args)") {
				capture = i
			}
		}
		if start < 0 || pin <= start || capture <= pin {
			t.Fatal("egress request budget off pin must be inside helm before capture")
		}
	})
}
