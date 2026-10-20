package gitopsvalues

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestBackupImageToolLedgerIsOrderedAndMatchesDockerfile(t *testing.T) {
	assertBackupImagePolicyError(t, validateBackupImagePolicy(defaultBackupImagePolicy), nil)
	if len(backupImageToolReleases) < 4 {
		t.Fatal("append-only ledger lost historical entries")
	}
	for i, want := range []struct{ first, ageX8664, ageAarch64, awscli string }{
		{"0.0.291", "1.3.1-r6", "1.3.1-r6", "2.34.63-r0"},
		{"0.0.330", "1.3.2-r0", "1.3.2-r0", "2.34.63-r0"},
		{"0.0.336", "1.3.2-r0", "1.3.2-r1", "2.34.63-r0"},
		{"0.0.337", "1.3.2-r1", "1.3.2-r1", "2.34.63-r0"},
	} {
		got := backupImageToolReleases[i]
		if got.FirstRelease != want.first || got.AgeX8664 != want.ageX8664 || got.AgeAarch64 != want.ageAarch64 || got.AWSCLI != want.awscli {
			t.Fatalf("historical ledger entry %d changed", i)
		}
		if got.Base != backupImageToolReleases[0].Base {
			t.Fatalf("historical ledger entry %d changed its original base", i)
		}
	}
	got, err := parseBackupImageDockerfile(readBackupImageDockerfileFixture(t))
	assertBackupImagePolicyError(t, err, nil)
	if got.FirstRelease != "" {
		t.Fatal("Dockerfile parser invented a first release")
	}
	latest := backupImageToolReleases[len(backupImageToolReleases)-1]
	if got.Base != latest.Base || got.AgeX8664 != latest.AgeX8664 || got.AgeAarch64 != latest.AgeAarch64 || got.AWSCLI != latest.AWSCLI {
		t.Fatal("newest ledger entry does not match Dockerfile tools")
	}
}

func TestBackupImageToolLedgerRejectsMalformedEntries(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*backupImagePolicy)
		want []string
	}{
		{"empty", func(p *backupImagePolicy) { p.Ledger = nil }, []string{"ledger"}},
		{"descending", func(p *backupImagePolicy) { p.Ledger[0], p.Ledger[1] = p.Ledger[1], p.Ledger[0] }, []string{"strictly ascending"}},
		{"duplicate release", func(p *backupImagePolicy) { p.Ledger[1].FirstRelease = p.Ledger[0].FirstRelease }, []string{"strictly ascending"}},
		{"numeric descending", func(p *backupImagePolicy) { p.Ledger[0].FirstRelease, p.Ledger[1].FirstRelease = "0.0.100", "0.0.99" }, []string{"strictly ascending"}},
		{"numeric ascending", func(p *backupImagePolicy) { p.Ledger[0].FirstRelease, p.Ledger[1].FirstRelease = "0.0.99", "0.0.100" }, nil},
		{"v prefix", func(p *backupImagePolicy) { p.Ledger[1].FirstRelease = "v0.0.330" }, []string{"release version"}},
		{"suffix", func(p *backupImagePolicy) { p.Ledger[1].FirstRelease = "0.0.33a" }, []string{"release version"}},
		{"overflow", func(p *backupImagePolicy) { p.Ledger[1].FirstRelease = "0.0.18446744073709551616" }, []string{"release version"}},
		{"x86 age lacks revision", func(p *backupImagePolicy) { p.Ledger[1].AgeX8664 = "1.3.2" }, []string{""}},
		{"x86 age empty revision", func(p *backupImagePolicy) { p.Ledger[1].AgeX8664 = "1.3.2-r" }, []string{""}},
		{"aarch64 age lacks revision", func(p *backupImagePolicy) { p.Ledger[1].AgeAarch64 = "1.3.2" }, []string{""}},
		{"aarch64 age empty", func(p *backupImagePolicy) { p.Ledger[1].AgeAarch64 = "" }, []string{""}},
		{"awscli empty", func(p *backupImagePolicy) { p.Ledger[1].AWSCLI = "" }, []string{""}},
		{"base unpinned", func(p *backupImagePolicy) { p.Ledger[1].Base = "postgres:18-alpine3.23" }, []string{""}},
		{"base short", func(p *backupImagePolicy) {
			p.Ledger[1].Base = "postgres:18-alpine3.23@sha256:" + strings.Repeat("a", 63)
		}, []string{""}},
		{"same tools", func(p *backupImagePolicy) {
			p.Ledger[1] = p.Ledger[0]
			p.Ledger[1].FirstRelease = "0.0.330"
		}, []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := cloneBackupImageTestPolicy()
			tc.edit(&p)
			assertBackupImagePolicyError(t, validateBackupImagePolicy(p), tc.want)
		})
	}
}

func TestBackupImageDockerfileTie(t *testing.T) {
	original := readBackupImageDockerfileFixture(t)
	parsed, err := parseBackupImageDockerfile(original)
	assertBackupImagePolicyError(t, err, nil)
	changedBase := parsed.Base[:len(parsed.Base)-1] + "a"
	if changedBase == parsed.Base {
		changedBase = parsed.Base[:len(parsed.Base)-1] + "b"
	}
	replace := func(before, after string) []byte {
		t.Helper()
		if bytes.Count(original, []byte(before)) != 1 {
			t.Fatal("Dockerfile mutation must replace exactly one fixture fragment")
		}
		return bytes.Replace(original, []byte(before), []byte(after), 1)
	}
	// Drift values derive from the current pins, so the next real bump never
	// collides with them.
	nextRevision := func(v string) string {
		t.Helper()
		i := strings.LastIndex(v, "-r")
		if i < 0 {
			t.Fatalf("pin %q lacks an -rN revision", v)
		}
		n, err := strconv.Atoi(v[i+2:])
		if err != nil {
			t.Fatalf("pin %q lacks an -rN revision", v)
		}
		return v[:i] + "-r" + strconv.Itoa(n+1)
	}
	x86 := "x86_64) age_version=" + parsed.AgeX8664 + " ;;"
	arm := "aarch64) age_version=" + parsed.AgeAarch64 + " ;;"
	unsupported := `*) echo "unsupported backup image architecture" >&2; exit 1 ;;`
	apk := `apk add --no-cache "age=$age_version" aws-cli=` + parsed.AWSCLI + ";"
	for _, tc := range []struct {
		name string
		body []byte
		want []string
	}{
		{"unchanged", original, nil},
		{"x86 age", replace(x86, "x86_64) age_version="+nextRevision(parsed.AgeX8664)+" ;;"), []string{"internal/gitopsvalues/backup_image_tools.go"}},
		{"aarch64 age", replace(arm, "aarch64) age_version="+nextRevision(parsed.AgeAarch64)+" ;;"), []string{"internal/gitopsvalues/backup_image_tools.go"}},
		{"x86 reverted revision", replace(x86, "x86_64) age_version=1.3.2-r0 ;;"), []string{"internal/gitopsvalues/backup_image_tools.go"}},
		{"awscli", replace("aws-cli="+parsed.AWSCLI, "aws-cli="+nextRevision(parsed.AWSCLI)), []string{"internal/gitopsvalues/backup_image_tools.go"}},
		{"base", replace(parsed.Base, changedBase), []string{"a different base image"}},
		{"duplicate apk", append(bytes.Clone(original), []byte("\nRUN "+apk+"\n")...), []string{""}},
		{"duplicate FROM", append(bytes.Clone(original), []byte("\nFROM "+parsed.Base+"\n")...), []string{""}},
		{"x86 missing revision", replace(x86, "x86_64) age_version=1.3.2 ;;"), []string{""}},
		{"aarch64 missing revision", replace(arm, "aarch64) age_version=1.3.2 ;;"), []string{""}},
		{"missing x86", replace(x86, ""), []string{""}},
		{"missing aarch64", replace(arm, ""), []string{""}},
		{"duplicate x86", replace(arm, x86), []string{""}},
		{"duplicate aarch64", replace(x86, arm), []string{""}},
		{"unknown architecture", replace(arm, "armv7) age_version="+parsed.AgeAarch64+" ;;"), []string{""}},
		{"extra architecture", replace(arm, arm+" \\\n      armv7) age_version="+parsed.AgeAarch64+" ;;"), []string{""}},
		{"missing case", replace(`case "$(apk --print-arch)" in`, ""), []string{""}},
		{"wrong case source", replace(`case "$(apk --print-arch)" in`, `case "$TARGETARCH" in`), []string{""}},
		{"missing esac", replace("esac;", ""), []string{""}},
		{"age overridden after case", replace("esac;", "esac; \\\n    age_version=1.3.2-r0;"), []string{""}},
		{"malformed case terminator", replace(x86, "x86_64) age_version="+parsed.AgeX8664+" ;"), []string{""}},
		{"malformed age assignment", replace(x86, "x86_64) age_version ="+parsed.AgeX8664+" ;;"), []string{""}},
		{"missing default", replace(unsupported, ""), []string{""}},
		{"default does not reject", replace(unsupported, `*) echo "unsupported backup image architecture" >&2; exit 0 ;;`), []string{""}},
		{"default lacks unsupported", replace(unsupported, `*) echo "backup image architecture" >&2; exit 1 ;;`), []string{""}},
		{"duplicate default", replace(unsupported, unsupported+" \\\n      "+unsupported), []string{""}},
		{"literal age install", replace(apk, "apk add --no-cache age="+parsed.AgeX8664+" aws-cli="+parsed.AWSCLI+";"), []string{""}},
		{"unquoted age install", replace(apk, "apk add --no-cache age=$age_version aws-cli="+parsed.AWSCLI+";"), []string{""}},
		{"wrong age variable", replace(apk, `apk add --no-cache "age=$other_version" aws-cli=`+parsed.AWSCLI+";"), []string{""}},
		{"awscli lacks revision", replace(apk, `apk add --no-cache "age=$age_version" aws-cli=2.34.63;`), []string{""}},
		{"missing apk", replace(apk, ""), []string{""}},
		{"missing", nil, []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := copyBackupImageToolsFixture(t)
			path := filepath.Join(root, backupImageDockerfileRel)
			if tc.body == nil {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, tc.body, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := CheckBackupImagePins(root)
			assertBackupImagePolicyError(t, err, tc.want)
		})
	}
	t.Run("entry point validates policy", func(t *testing.T) {
		p := cloneBackupImageTestPolicy()
		p.Ledger[0], p.Ledger[1] = p.Ledger[1], p.Ledger[0]
		checked, err := checkBackupImagePinsWith(copyBackupImageToolsFixture(t), p)
		assertBackupImagePolicyError(t, err, []string{"strictly ascending"})
		if checked != nil {
			t.Fatal("invalid policy must return a nil checked list")
		}
	})
}

func TestBackupImagePinPolicy(t *testing.T) {
	on, off := cloneBackupImageTestPolicy(), cloneBackupImageTestPolicy()
	on.MaxPatchLag, off.MaxPatchLag = 20, 0
	// The ordering cases use exact two-entry synthetic ledgers, never the live
	// ledger: an appended live entry must not change what these cases prove.
	numeric := cloneBackupImageTestPolicy()
	numeric.MaxPatchLag = 0
	numeric.Ledger = numeric.Ledger[:2]
	numeric.Ledger[0].FirstRelease, numeric.Ledger[1].FirstRelease = "0.0.99", "0.0.100"
	minor := cloneBackupImageTestPolicy()
	minor.MaxPatchLag = 0
	minor.Ledger = minor.Ledger[:2]
	minor.Ledger[0].FirstRelease, minor.Ledger[1].FirstRelease = "0.0.1", "0.1.0"
	distanceOn, distanceOff := cloneBackupImageTestPolicy(), cloneBackupImageTestPolicy()
	distanceOn.Ledger, distanceOff.Ledger = distanceOn.Ledger[:1], distanceOff.Ledger[:1]
	distanceOn.Ledger[0].FirstRelease, distanceOff.Ledger[0].FirstRelease = "0.0.1", "0.0.1"
	distanceOn.MaxPatchLag, distanceOff.MaxPatchLag = 20, 0
	baseChange := cloneBackupImageTestPolicy()
	baseChange.MaxPatchLag = 0
	baseChange.Ledger[1].Base = "postgres:18-alpine3.23@sha256:" + strings.Repeat("e", 64)
	awsChange := cloneBackupImageTestPolicy()
	awsChange.MaxPatchLag = 0
	awsChange.Ledger[1].AgeX8664 = awsChange.Ledger[0].AgeX8664
	awsChange.Ledger[1].AgeAarch64 = awsChange.Ledger[0].AgeAarch64
	awsChange.Ledger[1].AWSCLI = "2.34.64-r0"
	// A base-only change: identical tools, different PostgreSQL base. Proves the
	// base comparison on its own (the other stale rules cannot mask it).
	baseOnly := cloneBackupImageTestPolicy()
	baseOnly.MaxPatchLag = 0
	baseOnly.Ledger = []backupImageTools{baseOnly.Ledger[0], baseOnly.Ledger[0]}
	baseOnly.Ledger[0].FirstRelease, baseOnly.Ledger[1].FirstRelease = "0.0.1", "0.0.2"
	baseOnly.Ledger[1].Base = "postgres:18-alpine3.23@sha256:" + strings.Repeat("e", 64)
	if err := validateBackupImagePolicy(baseOnly); err != nil {
		t.Fatalf("base-only synthetic policy is invalid: %v", err)
	}
	for _, tc := range []struct {
		name    string
		policy  backupImagePolicy
		server  string
		pin     *BackupImagePins
		checked bool
		want    []string
		absent  []string
	}{
		{"old tools distance on", on, "0.0.333", backupImageTestPin("0.0.292"), true, []string{"test-cell", "0.0.292", "1.3.1-r6", "1.3.2-r0", "2.34.63-r0", "--backup-image", "scripts/roll-cell.sh test-cell 0.0.333"}, nil},
		{"old tools distance off", off, "0.0.333", backupImageTestPin("0.0.292"), true, []string{"test-cell", "0.0.292", "1.3.1-r6", "1.3.2-r0", "--backup-image"}, nil},
		{"current", on, "0.0.333", backupImageTestPin("0.0.333"), true, nil, nil},
		{"tools boundary old", off, "0.0.330", backupImageTestPin("0.0.329"), true, []string{"1.3.2-r0"}, nil},
		{"tools boundary equal", off, "0.0.330", backupImageTestPin("0.0.330"), true, nil, nil},
		{"aarch64 revision boundary", off, "0.0.336", backupImageTestPin("0.0.334"), true, []string{"aarch64", "1.3.2-r0", "1.3.2-r1"}, nil},
		{"x86 revision boundary", off, "0.0.337", backupImageTestPin("0.0.336"), true, []string{"x86_64", "1.3.2-r0", "1.3.2-r1"}, nil},
		{"both revisions boundary", off, "0.0.337", backupImageTestPin("0.0.334"), true, []string{"x86_64", "aarch64", "1.3.2-r0", "1.3.2-r1"}, nil},
		{"current architecture revisions", on, "0.0.337", backupImageTestPin("0.0.337"), true, nil, nil},
		{"newer architecture revisions", on, "0.0.336", backupImageTestPin("0.0.337"), true, nil, nil},
		{"same tools nine behind", on, "0.0.329", backupImageTestPin("0.0.320"), true, nil, nil},
		{"same tools distance off", off, "0.0.329", backupImageTestPin("0.0.292"), true, nil, nil},
		{"same tools distance on", on, "0.0.329", backupImageTestPin("0.0.292"), true, []string{"37 patch releases behind", "limit 20"}, []string{"1.3.2-r0"}},
		{"pin newer than server", on, "0.0.292", backupImageTestPin("0.0.333"), true, nil, nil},
		{"numeric patch order", numeric, "0.0.100", backupImageTestPin("0.0.99"), true, []string{"1.3.2-r0"}, nil},
		{"numeric minor order", minor, "0.1.0", backupImageTestPin("0.0.999"), true, []string{"1.3.2-r0"}, nil},
		{"numeric equal", minor, "0.0.999", backupImageTestPin("0.0.999"), true, nil, nil},
		{"predates", on, "0.0.333", backupImageTestPin("0.0.290"), true, []string{"predates the backup image tool ledger"}, nil},
		{"digest required", on, "0.0.333", &BackupImagePins{Repository: backupImageRepository, Tag: "0.0.333"}, true, []string{"must be pinned by digest"}, nil},
		{"pin invalid", on, "0.0.333", backupImageTestPin("latest"), true, []string{"release version"}, nil},
		{"server invalid", on, "latest", backupImageTestPin("0.0.333"), true, []string{"release version"}, nil},
		{"pin overflow", on, "0.0.333", backupImageTestPin("0.0.18446744073709551616"), true, []string{"release version"}, nil},
		{"server overflow", on, "18446744073709551616.0.0", backupImageTestPin("0.0.333"), true, []string{"release version"}, nil},
		{"nil", on, "0.0.333", nil, false, nil, nil},
		{"legacy", on, "0.0.333", &BackupImagePins{legacy: "postgres:18-alpine3.23"}, false, nil, nil},
		{"other repository", on, "0.0.333", &BackupImagePins{Repository: "postgres", Tag: "18"}, false, nil, nil},
		{"distance twenty", distanceOn, "0.0.120", backupImageTestPin("0.0.100"), true, nil, nil},
		{"distance twenty one", distanceOn, "0.0.121", backupImageTestPin("0.0.100"), true, []string{"21 patch releases behind", "limit 20"}, nil},
		{"distance disabled", distanceOff, "0.0.121", backupImageTestPin("0.0.100"), true, nil, nil},
		{"minor distance on", distanceOn, "0.1.0", backupImageTestPin("0.0.999"), true, []string{"older minor or major release than server release 0.1.0"}, nil},
		{"minor distance off", distanceOff, "0.1.0", backupImageTestPin("0.0.999"), true, nil, nil},
		{"major distance on", distanceOn, "1.0.0", backupImageTestPin("0.99.999"), true, []string{"older minor or major release than server release 1.0.0"}, nil},
		{"base changed", baseChange, "0.0.330", backupImageTestPin("0.0.329"), true, []string{"a different PostgreSQL base"}, nil},
		{"awscli changed", awsChange, "0.0.330", backupImageTestPin("0.0.329"), true, []string{"2.34.63-r0", "2.34.64-r0"}, nil},
		{"base only changed", baseOnly, "0.0.2", backupImageTestPin("0.0.1"), true, []string{"a different PostgreSQL base", "--backup-image"}, nil},
		{"base only current", baseOnly, "0.0.2", backupImageTestPin("0.0.2"), true, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checked, err := checkBackupImagePin(tc.policy, "test-cell", tc.server, tc.pin)
			assertBackupImagePolicyError(t, err, tc.want, tc.absent...)
			if checked != tc.checked {
				t.Fatalf("checked = %t, want %t", checked, tc.checked)
			}
		})
	}
}

func TestBackupImagePinToolRevisions(t *testing.T) {
	setRevision := func(tools *backupImageTools, pkg, revision string) {
		switch pkg {
		case "x86_64":
			tools.AgeX8664 = revision
		case "aarch64":
			tools.AgeAarch64 = revision
		case "aws-cli":
			tools.AWSCLI = revision
		}
	}
	for _, pkg := range []string{"x86_64", "aarch64", "aws-cli"} {
		for _, tc := range []struct {
			name, pinRevision, serverRevision string
			stale                             bool
		}{
			{"major numeric order", "9.99.99-r99", "10.0.0-r0", true},
			{"minor numeric order", "1.9.99-r99", "1.10.0-r0", true},
			{"patch numeric order", "1.2.9-r99", "1.2.10-r0", true},
			{"revision numeric order", "1.2.3-r9", "1.2.3-r10", true},
			{"newer major allowed", "10.0.0-r0", "9.99.99-r99", false},
			{"newer minor allowed", "1.10.0-r0", "1.9.99-r99", false},
			{"newer patch allowed", "1.2.10-r0", "1.2.9-r99", false},
			{"newer revision allowed", "1.2.3-r10", "1.2.3-r9", false},
		} {
			t.Run(pkg+"/"+tc.name, func(t *testing.T) {
				p := cloneBackupImageTestPolicy()
				p.MaxPatchLag = 0
				p.Ledger = []backupImageTools{p.Ledger[0], p.Ledger[0]}
				p.Ledger[0].FirstRelease, p.Ledger[1].FirstRelease = "0.0.1", "0.0.2"
				setRevision(&p.Ledger[0], pkg, tc.pinRevision)
				setRevision(&p.Ledger[1], pkg, tc.serverRevision)
				assertBackupImagePolicyError(t, validateBackupImagePolicy(p), nil)
				checked, err := checkBackupImagePin(p, "test-cell", "0.0.2", backupImageTestPin("0.0.1"))
				var want []string
				if tc.stale {
					want = []string{pkg, tc.pinRevision, tc.serverRevision, "--backup-image"}
				}
				assertBackupImagePolicyError(t, err, want)
				if !checked {
					t.Fatal("numeric package revision comparison skipped release pin")
				}
			})
		}
	}
	t.Run("one older architecture still fails", func(t *testing.T) {
		p := cloneBackupImageTestPolicy()
		p.MaxPatchLag = 0
		p.Ledger = []backupImageTools{p.Ledger[0], p.Ledger[0]}
		p.Ledger[0].FirstRelease, p.Ledger[1].FirstRelease = "0.0.1", "0.0.2"
		p.Ledger[0].AgeX8664, p.Ledger[1].AgeX8664 = "1.3.2-r0", "1.3.2-r1"
		p.Ledger[0].AgeAarch64, p.Ledger[1].AgeAarch64 = "1.3.2-r2", "1.3.2-r1"
		assertBackupImagePolicyError(t, validateBackupImagePolicy(p), nil)
		_, err := checkBackupImagePin(p, "test-cell", "0.0.2", backupImageTestPin("0.0.1"))
		assertBackupImagePolicyError(t, err, []string{"x86_64", "1.3.2-r0", "1.3.2-r1", "--backup-image"})
	})
}

func TestCommittedBackupImagePinsCarryReleasedTools(t *testing.T) {
	root := repoRoot(t)
	checked, err := CheckBackupImagePins(root)
	assertBackupImagePolicyError(t, err, nil)
	cfg, err := loadCatalog(root)
	assertBackupImagePolicyError(t, err, nil)
	var want []string
	for cell := range cfg.Cells {
		path := filepath.Join(root, valuesRel(cell))
		pins, err := readBackupImagePins(path)
		assertBackupImagePolicyError(t, err, nil)
		if pins == nil || pins.legacy != "" || pins.Repository != backupImageRepository {
			continue
		}
		if !backupImageCheckBackupOff && !backupImageFixtureEnabled(t, root, cell) {
			continue
		}
		want = append(want, cell)
	}
	slices.Sort(want)
	if !slices.Equal(checked, want) {
		t.Fatalf("checked cells = %v, want independently scanned %v", checked, want)
	}
}

func TestBackupImageGuardCatchesServerOnlyRollPastNewTools(t *testing.T) {
	// Versions follow the newest ledger entry so an appended entry moves the
	// scenario with it. At 0.0.337 only x86_64 changes; aarch64 stays at r1.
	ledger := defaultBackupImagePolicy.Ledger
	newest := backupImageTestPatch(t, ledger[len(ledger)-1].FirstRelease)
	previous := backupImageTestPatch(t, ledger[len(ledger)-2].FirstRelease)
	start := max(newest-10, previous)
	newTools := ledger[len(ledger)-1].AgeX8664
	release := func(patch uint64) string { return fmt.Sprintf("0.0.%d", patch) }
	for _, cell := range civoRollCells(t) {
		t.Run(cell, func(t *testing.T) {
			root := copyBackupImageToolsFixture(t)
			isChecked := backupImageCheckBackupOff || backupImageFixtureEnabled(t, root, cell)
			rollBackupImageToolsFixture(t, root, cell, release(start), "a", "b")
			_, err := CheckBackupImagePins(root)
			assertBackupImagePolicyError(t, err, nil)
			if err := RollCell(root, cell, release(newest-1), "sha256:"+strings.Repeat("c", 64), io.Discard); err != nil {
				assertBackupImagePolicyError(t, err, nil)
			}
			_, err = CheckBackupImagePins(root)
			assertBackupImagePolicyError(t, err, nil)
			if err := RollCell(root, cell, release(newest), "sha256:"+strings.Repeat("c", 64), io.Discard); err != nil {
				assertBackupImagePolicyError(t, err, nil)
			}
			var want []string
			if isChecked {
				want = []string{cell, release(start), newTools}
			}
			checked, err := CheckBackupImagePins(root)
			assertBackupImagePolicyError(t, err, want)
			if slices.Contains(checked, cell) != isChecked {
				t.Fatal("server-only roll has incorrect checked-cell membership")
			}
			rollBackupImageToolsFixture(t, root, cell, release(newest), "c", "e")
			_, err = CheckBackupImagePins(root)
			assertBackupImagePolicyError(t, err, nil)
			atLimit := release(newest + backupImageMaxPatchLag)
			if err := RollCell(root, cell, atLimit, "sha256:"+strings.Repeat("c", 64), io.Discard); err != nil {
				assertBackupImagePolicyError(t, err, nil)
			}
			_, err = CheckBackupImagePins(root)
			assertBackupImagePolicyError(t, err, nil)
			pastLimit := release(newest + 1 + backupImageMaxPatchLag)
			if err := RollCell(root, cell, pastLimit, "sha256:"+strings.Repeat("c", 64), io.Discard); err != nil {
				assertBackupImagePolicyError(t, err, nil)
			}
			want = nil
			if backupImageMaxPatchLag > 0 && isChecked {
				want = []string{cell, "patch releases behind"}
			}
			_, err = CheckBackupImagePins(root)
			assertBackupImagePolicyError(t, err, want)
		})
	}
}

func TestBackupImagePinExceptions(t *testing.T) {
	const cell = "civo-sandbox-use1-serving"
	ex := cloneBackupImageTestPolicy()
	ex.Exceptions = []backupImagePinException{{Cell: cell, Tag: "0.0.292", Reason: "0.0.330 image failed the dump"}}
	assertBackupImagePolicyError(t, validateBackupImagePolicy(ex), nil)
	for _, tc := range []struct {
		name string
		cell string
		pin  *BackupImagePins
		want []string
	}{
		{"matching", cell, backupImageTestPin("0.0.292"), nil},
		{"different tag", cell, backupImageTestPin("0.0.291"), []string{"--backup-image"}},
		{"different cell", "civo-prod-use1-serving", backupImageTestPin("0.0.292"), []string{"--backup-image"}},
		{"digest still required", cell, &BackupImagePins{Repository: backupImageRepository, Tag: "0.0.292"}, []string{"digest"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checked, err := checkBackupImagePin(ex, tc.cell, "0.0.333", tc.pin)
			assertBackupImagePolicyError(t, err, tc.want)
			if !checked {
				t.Fatal("exception pin must remain checked")
			}
		})
	}
	for _, tc := range []struct {
		name string
		edit func(*backupImagePolicy)
		want []string
	}{
		{"blank reason", func(p *backupImagePolicy) { p.Exceptions[0].Reason = "  " }, []string{"reason"}},
		{"invalid tag", func(p *backupImagePolicy) { p.Exceptions[0].Tag = "latest" }, []string{"release version"}},
		{"predating tag", func(p *backupImagePolicy) { p.Exceptions[0].Tag = "0.0.290" }, []string{""}},
		{"duplicate cell", func(p *backupImagePolicy) { p.Exceptions = append(p.Exceptions, p.Exceptions[0]) }, []string{""}},
		{"empty cell", func(p *backupImagePolicy) { p.Exceptions[0].Cell = "" }, []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := ex
			p.Exceptions = slices.Clone(ex.Exceptions)
			tc.edit(&p)
			assertBackupImagePolicyError(t, validateBackupImagePolicy(p), tc.want)
		})
	}
	t.Run("unknown catalog cell", func(t *testing.T) {
		p := cloneBackupImageTestPolicy()
		p.Exceptions = []backupImagePinException{{Cell: "no-such-cell", Tag: "0.0.333", Reason: "test"}}
		_, err := checkBackupImagePinsWith(copyBackupImageToolsFixture(t), p)
		assertBackupImagePolicyError(t, err, []string{"not in the cell catalog"})
	})
}

func TestBackupImagePolicyBackupOffSwitch(t *testing.T) {
	root := copyBackupImageToolsFixture(t)
	cells := civoRollCells(t)
	for _, cell := range cells {
		rollBackupImageToolsFixture(t, root, cell, "0.0.320", "a", "b")
		if err := RollCell(root, cell, "0.0.330", "sha256:"+strings.Repeat("c", 64), io.Discard); err != nil {
			assertBackupImagePolicyError(t, err, nil)
		}
	}
	t.Run("check backup off", func(t *testing.T) {
		p := cloneBackupImageTestPolicy()
		p.CheckBackupOff = true
		checked, err := checkBackupImagePinsWith(root, p)
		assertBackupImagePolicyError(t, err, cells)
		if !slices.Equal(checked, cells) {
			t.Fatalf("checked cells = %v, want %v", checked, cells)
		}
	})
	t.Run("skip backup off", func(t *testing.T) {
		p := cloneBackupImageTestPolicy()
		p.CheckBackupOff = false
		want := []string{"civo-sandbox-use1-serving"}
		checked, err := checkBackupImagePinsWith(root, p)
		assertBackupImagePolicyError(t, err, want, "civo-prod-use1-serving", "civo-sandbox-use1-backup")
		if !slices.Equal(checked, want) {
			t.Fatalf("checked cells = %v, want %v", checked, want)
		}
	})
}

func cloneBackupImageTestPolicy() backupImagePolicy {
	p := defaultBackupImagePolicy
	p.Ledger = slices.Clone(p.Ledger)
	p.Exceptions = slices.Clone(p.Exceptions)
	return p
}

func backupImageTestPin(tag string) *BackupImagePins {
	return &BackupImagePins{Repository: backupImageRepository, Tag: tag, Digest: "sha256:" + strings.Repeat("b", 64)}
}

func readBackupImageDockerfileFixture(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), backupImageDockerfileRel))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// Isolated tests start with real generator rolls, so unrelated committed pins
// and either roll-state fixture cannot influence a targeted failure. The
// committed-tree test deliberately uses the repository without normalization.
func copyBackupImageToolsFixture(t *testing.T) string {
	t.Helper()
	root := copyGenerationFixture(t)
	path := filepath.Join(root, backupImageDockerfileRel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, readBackupImageDockerfileFixture(t), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cell := range civoRollCells(t) {
		rollBackupImageToolsFixture(t, root, cell, "0.0.330", "a", "b")
	}
	return root
}

func rollBackupImageToolsFixture(t *testing.T, root, cell, version, serverByte, backupByte string) {
	t.Helper()
	pin := backupImageTestPin(version)
	pin.Digest = "sha256:" + strings.Repeat(backupByte, 64)
	err := RollCellWithBackupImage(root, cell, version, "sha256:"+strings.Repeat(serverByte, 64), pin, io.Discard)
	assertBackupImagePolicyError(t, err, nil)
}

// Decode enablement independently of the guard's parser and catalog switches.
func backupImageFixtureEnabled(t *testing.T, root, cell string) bool {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, valuesRel(cell)))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatal("cannot decode fixture values")
	}
	current := doc
	for _, part := range []string{"apps", "civoPostgres", "backup"} {
		next, ok := current[part].(map[string]any)
		if !ok {
			return false
		}
		current = next
	}
	return current["enabled"] == true
}

var backupImageTestHexPattern = regexp.MustCompile(`[a-fA-F0-9]{64}`)

// Check privacy before reporting any guard diagnostic, including unexpected
// failures. No assertion prints a Dockerfile, pin struct, or base reference.
func assertBackupImagePolicyError(t *testing.T, err error, want []string, absent ...string) {
	t.Helper()
	if err == nil {
		if want != nil {
			t.Fatal("expected backup image policy error")
		}
		return
	}
	message := err.Error()
	if strings.Contains(message, "sha256:") || backupImageTestHexPattern.MatchString(message) {
		t.Fatal("backup image policy diagnostic exposed image content")
	}
	if want == nil {
		t.Fatalf("unexpected backup image policy error: %s", message)
	}
	for _, part := range want {
		if !strings.Contains(message, part) {
			t.Fatalf("policy error lacks %q: %s", part, message)
		}
	}
	for _, part := range absent {
		if strings.Contains(message, part) {
			t.Fatalf("policy error unexpectedly includes %q: %s", part, message)
		}
	}
}

// backupImageTestPatch returns the patch number of a 0.0.N ledger release. A
// ledger entry outside 0.0.x needs this scenario rewritten, so it fails loudly.
func backupImageTestPatch(t *testing.T, release string) uint64 {
	t.Helper()
	v, err := parseReleaseVersion(release)
	if err != nil || v.major != 0 || v.minor != 0 || v.patch > 1<<20 {
		t.Fatalf("ledger release %q is not a small 0.0.N release: %v", release, err)
	}
	return v.patch
}
