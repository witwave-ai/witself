package gitopsvalues

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadPostgresImagePins(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, image, allow string
		bad, set           bool
	}{
		{name: "absent"},
		{name: "empty map", image: "{}"},
		{name: "empty fields", image: `{registry: "", repository: "", tag: "", digest: ""}`},
		{name: "upstream digest only", image: `{registry: registry-1.docker.io, repository: bitnami/postgresql, digest: ` + digest + `}`, set: true},
		{name: "upstream tag", image: `{registry: registry-1.docker.io, repository: bitnami/postgresql, tag: "18"}`, set: true},
		{name: "mirror", image: `{registry: ghcr.io, repository: witwave-ai/images/postgresql, tag: 0.0.999-civo-sandbox-use1-serving, digest: ` + digest + `}`, allow: "true", set: true},
		{name: "explicit false", allow: "false", set: true},
		{name: "null image", image: "null", bad: true},
		{name: "scalar image", image: "postgres:18", bad: true},
		{name: "sequence", image: "[]", bad: true},
		{name: "missing registry", image: `{repository: bitnami/postgresql, tag: latest}`, bad: true},
		{name: "missing repository", image: `{registry: ghcr.io, tag: latest}`, bad: true},
		{name: "missing tag and digest", image: `{registry: ghcr.io, repository: witwave-ai/images/postgresql}`, bad: true},
		{name: "invalid registry", image: `{registry: "https://ghcr.io", repository: witwave-ai/images/postgresql, tag: latest}`, bad: true},
		{name: "tag in repository", image: `{registry: ghcr.io, repository: witwave-ai/images/postgresql:latest, tag: latest}`, bad: true},
		{name: "invalid digest", image: `{registry: ghcr.io, repository: witwave-ai/images/postgresql, digest: sha256:short}`, bad: true},
		{name: "uppercase digest", image: `{registry: ghcr.io, repository: witwave-ai/images/postgresql, digest: sha256:` + strings.Repeat("A", 64) + `}`, bad: true},
		{name: "boolean field", image: `{registry: ghcr.io, repository: witwave-ai/images/postgresql, tag: false}`, bad: true},
		{name: "numeric tag", image: `{registry: ghcr.io, repository: witwave-ai/images/postgresql, tag: 18}`, bad: true},
		{name: "null digest", image: `{registry: ghcr.io, repository: witwave-ai/images/postgresql, tag: latest, digest: null}`, bad: true},
		{name: "unknown field", image: `{registry: ghcr.io, repository: witwave-ai/images/postgresql, tag: latest, extra: true}`, bad: true},
		{name: "duplicate field", image: `{registry: ghcr.io, repository: witwave-ai/images/postgresql, tag: latest, tag: old}`, bad: true},
		{name: "string opt in", allow: `"true"`, bad: true},
		{name: "null opt in", allow: "null", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "apps:\n  civoPostgres:\n    enabled: true\n"
			if tc.image != "" {
				body += "    image: " + tc.image + "\n"
			}
			if tc.allow != "" {
				body += "    allowInsecureImages: " + tc.allow + "\n"
			}
			got, err := parsePostgresImagePins([]byte(body))
			if tc.bad {
				if err == nil {
					t.Fatal("invalid PostgreSQL image selection accepted")
				}
				return
			}
			if err != nil || (got != nil) != tc.set {
				t.Fatal("PostgreSQL image presence or validation changed")
			}
		})
	}
}

func TestPostgresVerificationSettingKeepsDefaultImage(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	body := []byte("apps:\n  civoPostgres:\n    enabled: true\n    image:\n      registry: registry-1.docker.io\n      repository: bitnami/postgresql\n      digest: " + digest + "\n")
	for _, allow := range []bool{false, true} {
		got, err := addPostgresImagePins(body, &PostgresImagePins{allowInsecureImages: allow, allowInsecureImagesSet: true})
		if err != nil {
			t.Fatal(err)
		}
		pins, err := parsePostgresImagePins(got)
		if err != nil || pins == nil || pins.Registry != "registry-1.docker.io" || pins.Repository != "bitnami/postgresql" || pins.Digest != digest || pins.allowInsecureImages != allow {
			t.Fatal("verification setting changed the template's default image content")
		}
	}
}

func TestPostgresOverlayDigestRemainsGeneratorSourceOfTruth(t *testing.T) {
	for _, cell := range []string{"civo-sandbox-use1-backup", "civo-sandbox-use1-serving"} {
		for _, mirrored := range []bool{false, true} {
			name := cell + "/upstream"
			if mirrored {
				name = cell + "/mirror"
			}
			t.Run(name, func(t *testing.T) {
				root := copyGenerationFixture(t)
				path := filepath.Join(root, filepath.FromSlash(valuesRel(cell)))
				// Start both branches from the overlay's upstream selection even
				// when the committed cell already carries a mirror roll.
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				body = setFixtureYAMLField(t, body, "apps.civoPostgres.allowInsecureImages", "", "")
				body = setFixtureYAMLField(t, body, "apps.civoPostgres.image", "    image: {}\n", "")
				if err := os.WriteFile(path, body, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := Write(root, io.Discard); err != nil {
					t.Fatal(err)
				}
				pins, err := readPostgresImagePins(path)
				if err != nil || pins == nil || pins.Digest == "" {
					t.Fatal("fixture lacks PostgreSQL pins")
				}
				if mirrored {
					mirror := &PostgresImagePins{Registry: "ghcr.io", Repository: "witwave-ai/images/postgresql", Tag: "0.0.999-" + cell, Digest: pins.Digest}
					if err := RollCellWithImages(root, cell, "0.0.999", "sha256:"+strings.Repeat("a", 64), nil, mirror, io.Discard); err != nil {
						t.Fatal(err)
					}
				}
				before := readCellFixtureBytes(t, root)
				newDigest := "sha256:" + strings.Repeat("e", 64)
				// Templates are embedded at build time. Edit a temporary overlay
				// source and reparse that file to model rebuilding the generator.
				overlayName := cell + ".yaml.tmpl"
				overlay, err := embedded.ReadFile("overlays/" + overlayName)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Count(overlay, []byte(pins.Digest)) != 1 {
					t.Fatal("overlay must own exactly one PostgreSQL digest")
				}
				overlayPath := filepath.Join(t.TempDir(), overlayName)
				if err := os.WriteFile(overlayPath, bytes.Replace(overlay, []byte(pins.Digest), []byte(newDigest), 1), 0o600); err != nil {
					t.Fatal(err)
				}
				previousTemplates := templates
				t.Cleanup(func() { templates = previousTemplates })
				templates, err = templates.Clone()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := templates.ParseFiles(overlayPath); err != nil {
					t.Fatal(err)
				}
				if err := Check(root, io.Discard); err == nil || !strings.Contains(err.Error(), "PostgreSQL image digest drift") {
					t.Fatal("--check must report overlay digest drift before any write")
				}
				if err := RollCell(root, cell, "0.0.999", "sha256:"+strings.Repeat("a", 64), io.Discard); err == nil {
					t.Fatal("release roll accepted unresolved overlay digest drift")
				}
				for candidate, body := range readCellFixtureBytes(t, root) {
					if !bytes.Equal(body, before[candidate]) {
						t.Fatalf("drift check or rejected roll modified %s", candidate)
					}
				}
				var output bytes.Buffer
				if err := Write(root, &output); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(output.String(), "wrote "+valuesRel(cell)) {
					t.Fatal("--write did not report the repaired cell")
				}
				for candidate, body := range readCellFixtureBytes(t, root) {
					want := before[candidate]
					if candidate == cell {
						want = bytes.Replace(want, []byte(pins.Digest), []byte(newDigest), 1)
					}
					if !bytes.Equal(body, want) {
						t.Fatalf("--write must propagate only the overlay digest while preserving selection in %s", candidate)
					}
				}
				if err := Check(root, io.Discard); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestPostgresDigestDriftRejectsExplicitGeneratorOverride(t *testing.T) {
	root := copyGenerationFixture(t)
	cell := "civo-sandbox-use1-serving"
	cfg, err := loadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	charts, err := loadChartPins(root)
	if err != nil {
		t.Fatal(err)
	}
	wrong := &PostgresImagePins{Registry: "ghcr.io", Repository: "witwave-ai/images/postgresql", Tag: "0.0.999-" + cell, Digest: "sha256:" + strings.Repeat("c", 64)}
	for _, writeTemplateDigest := range []bool{false, true} {
		if _, err := generateCellWithImagePinsMode(root, cell, cfg, charts, nil, nil, wrong, writeTemplateDigest); err == nil || !strings.Contains(err.Error(), "PostgreSQL image digest drift") {
			t.Fatal("generator silently accepted a mismatched explicit digest override")
		}
	}
}

func TestPostgresImageRewritePreservesComments(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	body := []byte("apps:\n  civoPostgres:\n    enabled: true\n    image: # image identity\n      # registry explanation\n      registry: registry-1.docker.io # original registry\n      # repository explanation\n      repository: bitnami/postgresql\n\n      # digest explanation\n      digest: " + digest + " # reviewed content\n    networkPolicy:\n      enabled: true\n")
	pins := &PostgresImagePins{Registry: "ghcr.io", Repository: "witwave-ai/images/postgresql", Tag: "0.0.999-cell", Digest: digest, allowInsecureImages: true, allowInsecureImagesSet: true}
	got, err := addPostgresImagePins(body, pins)
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Replace(body, []byte("    image:"), []byte("    allowInsecureImages: true\n    image:"), 1)
	want = bytes.Replace(want, []byte("registry: registry-1.docker.io"), []byte("registry: \"ghcr.io\""), 1)
	want = bytes.Replace(want, []byte("repository: bitnami/postgresql\n"), []byte("repository: \"witwave-ai/images/postgresql\"\n      tag: \"0.0.999-cell\"\n"), 1)
	if !bytes.Equal(got, want) {
		t.Fatal("image rewrite changed or lost a comment, blank line, or unrelated field")
	}
	if parsed, err := parsePostgresImagePins(got); err != nil || parsed == nil || *parsed != *pins {
		t.Fatal("comment preservation changed the selected image")
	}
}

func TestRollPostgresMirrorPreservesContentAndRegenerates(t *testing.T) {
	for _, cell := range []string{"civo-sandbox-use1-backup", "civo-sandbox-use1-serving"} {
		t.Run(cell, func(t *testing.T) {
			root := copyGenerationFixture(t)
			version := "0.0.999"
			serverDigest := "sha256:" + strings.Repeat("a", 64)
			path := filepath.Join(root, filepath.FromSlash(valuesRel(cell)))
			upstream, err := readPostgresImagePins(path)
			if err != nil || upstream == nil || upstream.Digest == "" {
				t.Fatal("fixture lacks an upstream PostgreSQL digest")
			}
			if err := RollCell(root, cell, version, serverDigest, io.Discard); err != nil {
				t.Fatal(err)
			}
			before := readCellFixtureBytes(t, root)
			mirror := &PostgresImagePins{Registry: "ghcr.io", Repository: "witwave-ai/images/postgresql", Tag: version + "-" + cell, Digest: upstream.Digest}
			var out bytes.Buffer
			if err := RollCellWithImages(root, cell, version, serverDigest, nil, mirror, &out); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), mirror.Digest) || strings.Contains(out.String(), serverDigest) {
				t.Fatal("roll output contains digest bytes")
			}
			after := readCellFixtureBytes(t, root)
			for name, body := range before {
				expected := body
				if name == cell {
					block := "    image:\n      registry: \"" + mirror.Registry + "\"\n      repository: \"" + mirror.Repository + "\"\n      tag: \"" + mirror.Tag + "\"\n      digest: " + mirror.Digest + "\n"
					if upstream.allowInsecureImagesSet {
						expected = setFixtureYAMLField(t, expected, "apps.civoPostgres.allowInsecureImages", "    allowInsecureImages: true\n", "")
					} else {
						block = "    allowInsecureImages: true\n" + block
					}
					expected = setFixtureYAMLField(t, expected, "apps.civoPostgres.image", block, "")
				}
				if !bytes.Equal(after[name], expected) {
					t.Fatalf("PostgreSQL mirror changed unrelated bytes in %s", name)
				}
			}
			if err := Check(root, io.Discard); err != nil {
				t.Fatal(err)
			}
			if err := Write(root, io.Discard); err != nil {
				t.Fatal(err)
			}
			for name, body := range readCellFixtureBytes(t, root) {
				if !bytes.Equal(body, after[name]) {
					t.Fatalf("regeneration changed mirror pins in %s", name)
				}
			}
			backup := &BackupImagePins{Repository: backupReleaseRepository, Tag: "0.1.0", Digest: "sha256:" + strings.Repeat("b", 64)}
			if err := RollCellWithBackupImage(root, cell, backup.Tag, serverDigest, backup, io.Discard); err != nil {
				t.Fatal(err)
			}
			got, err := readPostgresImagePins(path)
			if err != nil || got == nil || got.Tag != mirror.Tag || got.Digest != upstream.Digest || !got.allowInsecureImages || !got.allowInsecureImagesSet {
				t.Fatal("server/backup roll failed to preserve PostgreSQL mirror content and opt-in")
			}
			mirror.Tag = "0.1.1-" + cell
			if err := RollCellWithImages(root, cell, "0.1.1", serverDigest, nil, mirror, io.Discard); err != nil {
				t.Fatal(err)
			}
			got, err = readPostgresImagePins(path)
			if err != nil || got == nil || got.Tag != mirror.Tag || got.Digest != upstream.Digest {
				t.Fatal("subsequent mirror roll failed to replace tag and preserve content")
			}
			if err := Check(root, io.Discard); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRollPostgresMirrorFailuresLeaveAllCellsUntouched(t *testing.T) {
	for _, name := range []string{"missing registry", "missing repository", "missing tag", "missing digest", "different digest", "wrong release", "wrong cell", "unreviewed registry", "unreviewed repository", "no existing pin", "drift"} {
		t.Run(name, func(t *testing.T) {
			root := copyGenerationFixture(t)
			cell := "civo-sandbox-use1-serving"
			path := filepath.Join(root, filepath.FromSlash(valuesRel(cell)))
			upstream, err := readPostgresImagePins(path)
			if err != nil || upstream == nil {
				t.Fatal("fixture lacks PostgreSQL pins")
			}
			mirror := &PostgresImagePins{Registry: "ghcr.io", Repository: "witwave-ai/images/postgresql", Tag: "0.0.999-" + cell, Digest: upstream.Digest}
			switch name {
			case "missing registry":
				mirror.Registry = ""
			case "missing repository":
				mirror.Repository = ""
			case "missing tag":
				mirror.Tag = ""
			case "missing digest":
				mirror.Digest = ""
			case "different digest":
				mirror.Digest = "sha256:" + strings.Repeat("c", 64)
			case "wrong release":
				mirror.Tag = "0.0.998-" + cell
			case "wrong cell":
				mirror.Tag = "0.0.999-civo-sandbox-use1-backup"
			case "unreviewed registry":
				mirror.Registry = "example.com"
			case "unreviewed repository":
				mirror.Repository = "other/postgresql"
			case "no existing pin":
				cell = "civo-sandbox-usw2-dev"
				mirror.Tag = "0.0.999-" + cell
			case "drift":
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(body, []byte("# operator edit\n")...), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := readCellFixtureBytes(t, root)
			backup := &BackupImagePins{Repository: backupReleaseRepository, Tag: "0.0.999", Digest: "sha256:" + strings.Repeat("b", 64)}
			if err := RollCellWithImages(root, cell, "0.0.999", "sha256:"+strings.Repeat("a", 64), backup, mirror, io.Discard); err == nil {
				t.Fatal("invalid PostgreSQL mirror roll accepted")
			}
			for candidate, body := range readCellFixtureBytes(t, root) {
				if !bytes.Equal(body, before[candidate]) {
					t.Fatalf("failed combined image roll modified %s", candidate)
				}
			}
		})
	}
}

func TestGenerationRejectsMalformedPostgresImageWithoutWrites(t *testing.T) {
	for _, tc := range []struct{ name, selection string }{
		{"null", "    image: null\n"},
		{"partial", "    image: {registry: ghcr.io}\n"},
		{"duplicate field", "    image: {registry: ghcr.io, repository: witwave-ai/images/postgresql, tag: latest, tag: old}\n"},
		{"duplicate image", "    image: {}\n    image: {}\n"},
		{"malformed opt in", "    image: {}\n    allowInsecureImages: 'true'\n"},
		{"duplicate opt in", "    image: {}\n    allowInsecureImages: true\n    allowInsecureImages: false\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := copyGenerationFixture(t)
			cell := "civo-sandbox-use1-serving"
			path := filepath.Join(root, filepath.FromSlash(valuesRel(cell)))
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			body = setFixtureYAMLField(t, body, "apps.civoPostgres.allowInsecureImages", "", "")
			body = setFixtureYAMLField(t, body, "apps.civoPostgres.image", tc.selection, "")
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			before := readCellFixtureBytes(t, root)
			if err := Write(root, io.Discard); err == nil {
				t.Fatal("generation accepted malformed PostgreSQL image")
			}
			if err := Check(root, io.Discard); err == nil {
				t.Fatal("check accepted malformed PostgreSQL image")
			}
			if err := RollCell(root, cell, "0.0.999", "sha256:"+strings.Repeat("a", 64), io.Discard); err == nil {
				t.Fatal("server roll accepted malformed PostgreSQL image")
			}
			for candidate, got := range readCellFixtureBytes(t, root) {
				if !bytes.Equal(got, before[candidate]) {
					t.Fatalf("malformed PostgreSQL image modified %s", candidate)
				}
			}
		})
	}
}
