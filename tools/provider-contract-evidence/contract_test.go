package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) aggregate {
	t.Helper()
	raw, e := os.ReadFile("testdata/valid-release-matrix.json")
	if e != nil {
		t.Fatal(e)
	}
	var a aggregate
	if decodeStrict(raw, &a) != nil || validateAggregate(a) != nil {
		t.Fatal("invalid static test fixture")
	}
	return a
}
func marshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return raw
}
func TestStaticReleaseFixture(t *testing.T) {
	a := fixture(t)
	if len(a.Matrix) != 35 || a.ExpectedTestOutcomes != 75 || a.PassedTestOutcomes != 73 || a.NotApplicableTestOutcomes != 2 {
		t.Fatal("wrong aggregate counts")
	}
}
func TestStrictReportParsing(t *testing.T) {
	a := fixture(t)
	raw := marshal(t, a)
	cases := map[string][]byte{
		"duplicate":                 append([]byte(`{"schema_version":"forged",`), raw[1:]...),
		"unknown":                   append([]byte(`{"credential":"PRIVATE_SENTINEL",`), raw[1:]...),
		"alias alongside canonical": append([]byte(`{"Schema_Version":"witself.provider-contract.matrix.v1",`), raw[1:]...),
		"nested alias":              bytes.Replace(raw, []byte(`"repository":`), []byte(`"Repository":"witwave-ai/witself","repository":`), 1),
		"missing false":             bytes.Replace(raw, []byte(`"published_bytes_tested":false,`), nil, 1),
		"missing empty":             bytes.Replace(raw, []byte(`"binary_vcs_revision":"`+strings.Repeat("a", 40)+`",`), nil, 1),
		"null bool":                 bytes.Replace(raw, []byte(`"published_bytes_tested":false`), []byte(`"published_bytes_tested":null`), 1),
		"trailing":                  append(append([]byte(nil), raw...), []byte(`{}`)...),
		"truncated":                 raw[:len(raw)-1],
		"oversized":                 bytes.Repeat([]byte(" "), maxReportBytes+1),
		"deep":                      []byte(strings.Repeat("[", 25) + "0" + strings.Repeat("]", 25)),
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			var got aggregate
			if decodeStrict(bad, &got) == nil {
				t.Fatal("accepted malformed report")
			}
		})
	}
}
func TestAggregateRefusals(t *testing.T) {
	cases := map[string]func(*aggregate){
		"missing target":           func(a *aggregate) { a.Cells = a.Cells[:4] },
		"duplicate target":         func(a *aggregate) { a.Cells[4] = a.Cells[0] },
		"wrong OS":                 func(a *aggregate) { a.Cells[0].GOOS = "darwin" },
		"mixed commit":             func(a *aggregate) { a.Cells[0].Identity.SourceCommit = strings.Repeat("b", 40) },
		"mixed repository":         func(a *aggregate) { a.Cells[0].Identity.Repository = "other/repo" },
		"mixed run":                func(a *aggregate) { a.Cells[0].Identity.RunID = "101" },
		"mixed attempt":            func(a *aggregate) { a.Cells[0].Identity.RunAttempt = 2 },
		"mixed workflow":           func(a *aggregate) { a.Cells[0].Identity.Workflow = "ci" },
		"mixed source ref":         func(a *aggregate) { a.Cells[0].Identity.SourceRef = "refs/heads/main" },
		"wrong release tag":        func(a *aggregate) { a.Cells[0].Identity.ReleaseTag = "v9.8.8" },
		"missing phase":            func(a *aggregate) { a.Cells[0].Phases = a.Cells[0].Phases[:2] },
		"wrong phase":              func(a *aggregate) { a.Cells[0].Phases[0].Name = "installed-snapshot" },
		"missing test":             func(a *aggregate) { a.Cells[0].Phases[0].Tests = a.Cells[0].Phases[0].Tests[:1] },
		"unexpected skip":          func(a *aggregate) { a.Cells[0].Phases[0].Tests[0].Status = "not_applicable" },
		"missing Windows NA":       func(a *aggregate) { a.Cells[4].Phases[1].Tests[2].Status = "passed" },
		"failed test":              func(a *aggregate) { a.Cells[0].Phases[0].Tests[0].Status = "failed" },
		"failed process":           func(a *aggregate) { x := 1; a.Cells[0].Phases[0].ProcessExit = &x },
		"missing process":          func(a *aggregate) { a.Cells[0].Phases[0].ProcessExit = nil },
		"incomplete cell":          func(a *aggregate) { a.Cells[0].Status = "incomplete" },
		"fabricated count":         func(a *aggregate) { a.PassedTestOutcomes = 75 },
		"fabricated matrix":        func(a *aggregate) { a.Matrix[0].ClientResult = "passed" },
		"real runtime claim":       func(a *aggregate) { a.Cells[0].RuntimeKind = "real" },
		"vendor claim":             func(a *aggregate) { a.Cells[0].VendorVersion = "1.2.3" },
		"final bytes claim":        func(a *aggregate) { a.Cells[0].PublishedBytesTested = true },
		"invalid hash":             func(a *aggregate) { a.Cells[0].Artifact.BinarySHA256 = "PRIVATE_SENTINEL" },
		"wrong short commit":       func(a *aggregate) { a.Cells[0].Artifact.BinaryReportedCommit = "bbbbbbb" },
		"wrong full binary commit": func(a *aggregate) { a.Cells[0].Artifact.BinaryVCSRevision = strings.Repeat("b", 40) },
		"final version":            func(a *aggregate) { a.Cells[0].Artifact.SnapshotVersion = "9.8.7" },
		"archive path":             func(a *aggregate) { a.Cells[0].Artifact.ArchiveName = "/private/sensitive/archive.tar.gz" },
		"future completion":        func(a *aggregate) { a.Cells[0].CompletedAt = "2026-09-07T00:00:00Z" },
		"overlapping phases":       func(a *aggregate) { a.Cells[0].Phases[1].StartedAt = a.Cells[0].StartedAt },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			a := fixture(t)
			change(&a)
			if validateAggregate(a) == nil {
				t.Fatal("accepted invalid evidence")
			}
		})
	}
}
func TestIdentityBoundaries(t *testing.T) {
	for _, ref := range []string{"refs/heads/main", "refs/heads/a-branch", "refs/tags/v9.8.7"} {
		t.Run("manual "+ref, func(t *testing.T) {
			a := fixture(t)
			i := a.Identity
			i.ReleaseTag = ""
			i.SourceRef = ref
			for n := range a.Cells {
				a.Cells[n].Identity = i
			}
			if _, e := makeAggregate(a.Cells, i, a.GeneratedAt); e != nil {
				t.Fatal("manual snapshot identity rejected")
			}
		})
	}
	a := fixture(t)
	i := a.Identity
	i.Workflow = "ci"
	i.ReleaseTag = ""
	i.SourceRef = "refs/pull/123/merge"
	if !validIdentity(i) {
		t.Fatal("synthetic merge commit identity rejected")
	}
	for _, ref := range []string{"refs/heads/private|token", "refs/heads/../main", "refs/pull/123/head", "refs/tags/v9.8.7", "refs/heads/topic"} {
		i.SourceRef = ref
		if validIdentity(i) {
			t.Fatalf("unsafe/unsupported CI ref accepted: %q", ref)
		}
	}
}
func TestReleaseValidatorCLI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.json")
	a := fixture(t)
	if writeJSON(path, a) != nil {
		t.Fatal("write")
	}
	args := []string{"validate-release", "--input", path, "--version", "9.8.7", "--commit", strings.Repeat("a", 40), "--repository", "witwave-ai/witself", "--run-id", "100", "--run-attempt", "1"}
	var out bytes.Buffer
	if cli(args, &out) != 0 || out.Len() != 0 {
		t.Fatal("valid release refused")
	}
	a.Identity.ReleaseTag = ""
	for n := range a.Cells {
		a.Cells[n].Identity = a.Identity
	}
	a, _ = makeAggregate(a.Cells, a.Identity, a.GeneratedAt)
	if writeJSON(path, a) != nil {
		t.Fatal("write")
	}
	if cli(args, &out) == 0 {
		t.Fatal("manual tag run certified as publishing run")
	}
	out.Reset()
	if cli(append(args, "--command", "PRIVATE_SENTINEL"), &out) == 0 || strings.Contains(out.String(), "PRIVATE_SENTINEL") {
		t.Fatal("unsafe CLI arguments accepted/leaked")
	}
}
func writeCellLayout(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, c := range fixture(t).Cells {
		dir := filepath.Join(root, "artifact-"+c.Target)
		if os.Mkdir(dir, 0o700) != nil {
			t.Fatal("mkdir")
		}
		if writeJSON(filepath.Join(dir, "provider-contract-"+c.Target+".json"), c) != nil {
			t.Fatal("write")
		}
	}
	return root
}
func TestAggregateDownloadLayout(t *testing.T) {
	t.Run("distinct directories", func(t *testing.T) {
		root := writeCellLayout(t)
		cells, e := readCells(root)
		if e != nil || len(cells) != 5 {
			t.Fatal("valid download layout refused")
		}
	})
	for _, kind := range []string{"unexpected", "duplicate", "symlink", "missing", "directory depth"} {
		t.Run(kind, func(t *testing.T) {
			root := writeCellLayout(t)
			p := filepath.Join(root, "artifact-linux-x64", "provider-contract-linux-x64.json")
			switch kind {
			case "unexpected":
				_ = os.WriteFile(filepath.Join(root, "raw-private.log"), []byte("PRIVATE_SENTINEL"), 0o600)
			case "duplicate":
				_ = writeJSON(filepath.Join(root, "provider-contract-linux-x64.json"), fixture(t).Cells[0])
			case "symlink":
				raw, _ := os.ReadFile(p)
				q := filepath.Join(t.TempDir(), "other.json")
				_ = os.WriteFile(q, raw, 0o600)
				_ = os.Remove(p)
				if e := os.Symlink(q, p); e != nil {
					t.Skip("symlink unavailable")
				}
			case "missing":
				_ = os.Remove(p)
			case "directory depth":
				_ = os.MkdirAll(filepath.Join(root, "one", "two", "three"), 0o700)
			}
			if _, e := readCells(root); e == nil {
				t.Fatal("invalid download layout accepted")
			}
		})
	}
}
