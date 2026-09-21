package gitopsvalues

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// existingServerDigestLine matches a server image digest pin already present
// in a committed cell values file.
var existingServerDigestLine = regexp.MustCompile(`(?m)^    imageDigest: sha256:[0-9a-f]{64}\n`)

func TestReadServerPinsDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name string
		line string
		want string
		bad  bool
	}{
		{name: "absent"},
		{name: "empty", line: `    imageDigest: ""`},
		{name: "canonical", line: "    imageDigest: " + digest, want: digest},
		{name: "short", line: "    imageDigest: sha256:abcd", bad: true},
		{name: "uppercase", line: "    imageDigest: sha256:" + strings.Repeat("A", 64), bad: true},
		{name: "other algorithm", line: "    imageDigest: sha512:" + strings.Repeat("a", 64), bad: true},
		{name: "trailing whitespace", line: "    imageDigest: '" + digest + " '", bad: true},
		{name: "boolean", line: "    imageDigest: false", bad: true},
		{name: "number", line: "    imageDigest: 123", bad: true},
		{name: "null", line: "    imageDigest: null", bad: true},
		{name: "mapping", line: "    imageDigest: {}", bad: true},
		{name: "sequence", line: "    imageDigest: []", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "values.yaml")
			body := "apps:\n  witselfServer:\n    chartVersion: 0.0.1\n    imageTag: 0.0.1\n" + tc.line + "\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := readServerPins(path, chartPins{})
			if tc.bad {
				if err == nil {
					t.Fatal("invalid digest was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.ChartVersion != "0.0.1" || got.ImageTag != "0.0.1" || got.ImageDigest != tc.want {
				t.Fatal("readServerPins did not preserve release pins")
			}
		})
	}
}

func TestRollCellPreservesAllOtherBytesAndRegenerates(t *testing.T) {
	cfg, err := loadCatalog(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for cell := range cfg.Cells {
		t.Run(cell, func(t *testing.T) {
			root := copyGenerationFixture(t)
			before := readCellFixtureBytes(t, root)
			digest := "sha256:" + strings.Repeat("a", 64)
			var out bytes.Buffer
			if err := RollCell(root, cell, "0.0.999", digest, &out); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), digest) {
				t.Fatal("roll output includes digest bytes")
			}
			after := readCellFixtureBytes(t, root)
			for name, old := range before {
				if name != cell {
					if !bytes.Equal(old, after[name]) {
						t.Fatalf("roll modified unselected cell %s", name)
					}
					continue
				}
				pins, err := readServerPins(filepath.Join(root, filepath.FromSlash(valuesRel(name))), chartPins{})
				if err != nil {
					t.Fatal(err)
				}
				if pins.ChartVersion != "0.0.999" || pins.ImageTag != "0.0.999" || pins.ImageDigest != digest {
					t.Fatal("roll did not set complete pin set")
				}
				// Compare every original byte after replacing only the two original
				// pins and adding the new digest, including comments and whitespace.
				var docPins struct {
					Apps struct {
						WitselfServer serverPins `yaml:"witselfServer"`
					} `yaml:"apps"`
				}
				if err := yaml.Unmarshal(old, &docPins); err != nil {
					t.Fatal(err)
				}
				expected := bytes.Replace(old, []byte("    chartVersion: "+docPins.Apps.WitselfServer.ChartVersion+"\n"), []byte("    chartVersion: 0.0.999\n"), 1)
				// A baseline that already carries a digest pin (every cell after its
				// first pinned roll) is rewritten in place, not appended to.
				expected = existingServerDigestLine.ReplaceAll(expected, nil)
				expected = bytes.Replace(expected, []byte("    imageTag: "+docPins.Apps.WitselfServer.ImageTag+"\n"), []byte("    imageTag: 0.0.999\n    imageDigest: "+digest+"\n"), 1)
				if !bytes.Equal(expected, after[name]) {
					t.Fatal("roll changed bytes outside server pins")
				}
			}
			if err := Check(root, io.Discard); err != nil {
				t.Fatalf("rolled values do not regenerate: %v", err)
			}
			if err := Write(root, io.Discard); err != nil {
				t.Fatal(err)
			}
			for name, body := range readCellFixtureBytes(t, root) {
				if !bytes.Equal(body, after[name]) {
					t.Fatalf("ordinary regeneration changed rolled cell %s", name)
				}
			}
		})
	}
}

func TestRollCellFailureLeavesAllValuesUntouched(t *testing.T) {
	for _, name := range []string{"empty digest", "bad digest", "bad version", "unknown cell", "drift", "malformed existing digest"} {
		t.Run(name, func(t *testing.T) {
			root := copyGenerationFixture(t)
			cell, version, digest := "civo-sandbox-use1-serving", "0.0.999", "sha256:"+strings.Repeat("a", 64)
			path := filepath.Join(root, filepath.FromSlash(valuesRel(cell)))
			switch name {
			case "empty digest":
				digest = ""
			case "bad digest":
				digest = "sha256:short"
			case "bad version":
				version = "0.0.999\nimageDigest: unexpected"
			case "unknown cell":
				cell = "../../outside"
			case "drift", "malformed existing digest":
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if name == "drift" {
					body = append(body, []byte("# operator edit\n")...)
				} else {
					body = setFixtureYAMLField(t, body, "apps.witselfServer.imageDigest", "    imageDigest: sha256:short\n", "imageTag")
				}
				if err := os.WriteFile(path, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := readCellFixtureBytes(t, root)
			if err := RollCell(root, cell, version, digest, io.Discard); err == nil {
				t.Fatal("invalid roll was accepted")
			}
			for name, body := range readCellFixtureBytes(t, root) {
				if !bytes.Equal(body, before[name]) {
					t.Fatalf("failed roll modified %s", name)
				}
			}
		})
	}
}

func TestRollCellReplacesExistingDigest(t *testing.T) {
	root := copyGenerationFixture(t)
	cell := "civo-sandbox-use1-serving"
	first := "sha256:" + strings.Repeat("a", 64)
	second := "sha256:" + strings.Repeat("b", 64)
	if err := RollCell(root, cell, "0.0.998", first, io.Discard); err != nil {
		t.Fatal(err)
	}
	before := readCellFixtureBytes(t, root)
	if err := RollCell(root, cell, "0.0.999", second, io.Discard); err != nil {
		t.Fatal(err)
	}
	after := readCellFixtureBytes(t, root)
	expected := bytes.Replace(before[cell], []byte("    chartVersion: 0.0.998\n"), []byte("    chartVersion: 0.0.999\n"), 1)
	expected = bytes.Replace(expected, []byte("    imageTag: 0.0.998\n"), []byte("    imageTag: 0.0.999\n"), 1)
	expected = bytes.Replace(expected, []byte(first), []byte(second), 1)
	if !bytes.Equal(expected, after[cell]) || bytes.Count(after[cell], []byte("    imageDigest:")) != 1 {
		t.Fatal("second roll did not replace the prior digest exactly once")
	}
	for name, body := range after {
		if name != cell && !bytes.Equal(before[name], body) {
			t.Fatalf("second roll modified unselected cell %s", name)
		}
	}
	if err := Check(root, io.Discard); err != nil {
		t.Fatalf("second roll did not regenerate: %v", err)
	}
}

func copyGenerationFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	source := repoRoot(t)
	paths := []string{catalogRelPath, platformValuesRel, appsValuesRel}
	for cell := range readCellFixtureBytes(t, source) {
		paths = append(paths, valuesRel(cell))
	}
	for _, rel := range paths {
		body, err := os.ReadFile(filepath.Join(source, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func readCellFixtureBytes(t *testing.T, root string) map[string][]byte {
	t.Helper()
	cfg, err := loadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	bodies := make(map[string][]byte, len(cfg.Cells))
	for cell := range cfg.Cells {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(valuesRel(cell))))
		if err != nil {
			t.Fatal(err)
		}
		bodies[cell] = body
	}
	return bodies
}

// setFixtureYAMLField replaces only the named field's original lines. If the
// field is absent, it inserts after the named sibling; an empty replacement
// removes it. Looking up the full YAML path keeps backup.image distinct from
// civoPostgres.image and preserves every byte outside the selected field.
// Expectations use this independent text edit rather than the production pin
// writer, and malformed fixtures replace valid pins instead of duplicating them.
func setFixtureYAMLField(t *testing.T, body []byte, path, replacement, after string) []byte {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Content) != 1 {
		t.Fatal("fixture must contain one YAML document")
	}
	parts := strings.Split(path, ".")
	parent := doc.Content[0]
	field := func(node *yaml.Node, name string) (key, value *yaml.Node) {
		if node.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(node.Content); i += 2 {
				if node.Content[i].Value == name {
					return node.Content[i], node.Content[i+1]
				}
			}
		}
		return nil, nil
	}
	for _, part := range parts[:len(parts)-1] {
		_, parent = field(parent, part)
		if parent == nil {
			t.Fatalf("fixture lacks parent mapping for %s", path)
		}
	}
	var lastLine func(*yaml.Node) int
	lastLine = func(node *yaml.Node) int {
		last := node.Line
		for _, child := range node.Content {
			if line := lastLine(child); line > last {
				last = line
			}
		}
		return last
	}
	key, value := field(parent, parts[len(parts)-1])
	var start, end int
	if key != nil {
		start, end = key.Line-1, lastLine(value)
	} else {
		if replacement == "" {
			return body
		}
		_, anchor := field(parent, after)
		if anchor == nil {
			t.Fatalf("fixture lacks insertion anchor for %s", path)
		}
		start, end = lastLine(anchor), lastLine(anchor)
	}
	lines := strings.SplitAfter(string(body), "\n")
	return []byte(strings.Join(lines[:start], "") + replacement + strings.Join(lines[end:], ""))
}
