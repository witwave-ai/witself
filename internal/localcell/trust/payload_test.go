package trust

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

type payloadFixtureInput struct {
	root       string
	v          *Verifier
	state      *MemoryState
	set        VerifiedSet
	components []Component
	archives   map[string]string
	clock      Clock
}

func payloadInput(t *testing.T) payloadFixtureInput {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("NOT_RUN windows_private_acl_persistence_unavailable; read_only_marking_not_proven")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("fixture root unavailable")
	}
	if os.Chmod(root, 0700) != nil {
		t.Fatal("fixture root privacy setup failed")
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(path, 0700)
			}
			return nil
		})
	})
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata", "vectors", name))
		if err != nil {
			t.Fatal("fixture unavailable")
		}
		return b
	}
	_, public := verifierFixtureKey(t, "root-a")
	clock := Clock{Now: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), Trusted: true}
	state := NewMemoryState()
	v := NewVerifier(Options{Roots: []PublicKey{public}, State: state, Clock: clock, OS: "linux", Arch: "amd64"})
	if out := v.VerifyDelegation(read("setup-delegation.json"), read("setup-delegation.json.sig.json")); !out.Accepted {
		t.Fatalf("delegation refused: %s", out.Reason)
	}
	set := VerifiedSet{Document: read("positive-built-dist.json"), Envelope: read("positive-built-dist.json.sig.json")}
	parsed, err := ParseComponentSet(set.Document)
	if err != nil {
		t.Fatal("fixture set parse failed")
	}
	components := parsed.Platforms[0].Components
	if out := v.AcceptSet(set.Document, set.Envelope, components); !out.Accepted {
		t.Fatalf("fixture set refused: %s", out.Reason)
	}
	archiveRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("fixture archive root unavailable")
	}
	archives := map[string]string{}
	for _, c := range components {
		b := payloadArchive(t, c.Inventory[0].Path, []byte("data"))
		if uint64(len(b)) != c.Archive.Size || digestBytes(b) != c.Archive.SHA256 {
			t.Fatal("deterministic B1 recipe changed")
		}
		name := filepath.Join(archiveRoot, c.Archive.Name)
		if os.WriteFile(name, b, 0600) != nil {
			t.Fatal("fixture archive write failed")
		}
		archives[c.Role] = name
	}
	return payloadFixtureInput{root: root, v: v, state: state, set: set, components: components, archives: archives, clock: clock}
}

func payloadArchive(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	gz, err := gzip.NewWriterLevel(&b, gzip.NoCompression)
	if err != nil {
		t.Fatal("fixture compression setup failed")
	}
	gz.Header = gzip.Header{ModTime: time.Unix(0, 0).UTC(), OS: 255}
	tw := tar.NewWriter(gz)
	header := &tar.Header{Name: name, Mode: 0755, Size: int64(len(body)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR, ModTime: time.Unix(0, 0).UTC()}
	if tw.WriteHeader(header) != nil {
		t.Fatal("fixture archive header failed")
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal("fixture archive body failed")
	}
	if tw.Close() != nil || gz.Close() != nil {
		t.Fatal("fixture archive close failed")
	}
	return b.Bytes()
}

func payloadFixture(t *testing.T) (string, Generation, Clock) {
	t.Helper()
	f := payloadInput(t)
	g, err := StageGeneration(context.Background(), f.v, f.state, f.set, f.components, f.archives, f.root)
	if err != nil {
		t.Fatalf("stage fixture: %s", reasonOf(err))
	}
	return f.root, g, f.clock
}

func payloadNext(t *testing.T, f payloadFixtureInput, sequence uint64) payloadFixtureInput {
	t.Helper()
	set, err := ParseComponentSet(f.set.Document)
	if err != nil {
		t.Fatal("fixture parse failed")
	}
	set.ReleaseSequence = sequence
	release, _ := verifierFixtureKey(t, "release-1")
	f.set.Document, f.set.Envelope = verifierSign(t, set, "component_set", release)
	set, err = ParseComponentSet(f.set.Document)
	if err != nil {
		t.Fatal("fixture parse failed")
	}
	f.components = set.Platforms[0].Components
	if out := f.v.AcceptSet(f.set.Document, f.set.Envelope, f.components); !out.Accepted {
		t.Fatalf("fixture acceptance: %s", out.Reason)
	}
	return f
}

func TestStageGenerationB1VectorAndReadOnly(t *testing.T) {
	f := payloadInput(t)
	g, err := StageGeneration(context.Background(), f.v, f.state, f.set, f.components, f.archives, f.root)
	if err != nil {
		t.Fatalf("stage: %s", reasonOf(err))
	}
	if g.Name != "stable-1-"+digestBytes(f.set.Document)[:16] || g.InventoryBytes != 20 || len(g.Components) != 5 {
		t.Fatal("generation metadata differs from authenticated vector")
	}
	loaded, err := LoadGeneration(f.root, g.Name)
	if err != nil || !bytes.Equal(loaded.Document, f.set.Document) || !bytes.Equal(loaded.Envelope, f.set.Envelope) {
		t.Fatal("exact signed bytes did not round trip")
	}
	dir := filepath.Join(f.root, "payload", g.Name)
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0500 {
		t.Fatal("generation is not owner read-only")
	}
	info, err = os.Stat(filepath.Join(dir, "generation.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("generation receipt is not private")
	}
	if err := RecheckGeneration(f.root, loaded); err != nil {
		t.Fatalf("generation inventory recheck: %s", reasonOf(err))
	}
}

func TestStageGenerationRequiresCommittedExactSet(t *testing.T) {
	f := payloadInput(t)
	empty := NewMemoryState()
	if _, err := StageGeneration(context.Background(), f.v, empty, f.set, f.components, f.archives, f.root); err != ReasonNotInstalled && err != ReasonStateInvalid {
		t.Fatal("uncommitted set staged")
	}
	second := payloadNext(t, f, 2)
	if _, err := StageGeneration(context.Background(), second.v, second.state, second.set, f.components, second.archives, f.root); err != ReasonMixedSet {
		t.Fatalf("mixed valid manifest not refused: %s", reasonOf(err))
	}
}

func TestStageGenerationExistingAndRetentionBeforeExtraction(t *testing.T) {
	f := payloadInput(t)
	for sequence := uint64(1); sequence <= 4; sequence++ {
		if sequence > 1 {
			f = payloadNext(t, f, sequence)
		}
		if _, err := StageGeneration(context.Background(), f.v, f.state, f.set, f.components, f.archives, f.root); err != nil {
			t.Fatalf("stage retained generation: %s", reasonOf(err))
		}
	}
	if _, err := StageGeneration(context.Background(), f.v, f.state, f.set, f.components, nil, f.root); err != ReasonGenerationExists {
		t.Fatal("existing generation not refused before archive access")
	}
	f = payloadNext(t, f, 5)
	if _, err := StageGeneration(context.Background(), f.v, f.state, f.set, f.components, nil, f.root); err != ReasonPayloadRetention {
		t.Fatalf("retention did not refuse before extraction: %s", reasonOf(err))
	}
}

func TestStageGenerationBudgetBeforeExtraction(t *testing.T) {
	f := payloadInput(t)
	if _, err := stageGeneration(context.Background(), f.v, f.state, f.set, f.components, nil, f.root, 19); err != ReasonPayloadBudget {
		t.Fatal("new inventory budget did not refuse before extraction")
	}
	if _, err := StageGeneration(context.Background(), f.v, f.state, f.set, f.components, f.archives, f.root); err != nil {
		t.Fatalf("stage first generation: %s", reasonOf(err))
	}
	f = payloadNext(t, f, 2)
	if _, err := stageGeneration(context.Background(), f.v, f.state, f.set, f.components, nil, f.root, 39); err != ReasonPayloadBudget {
		t.Fatal("aggregate inventory budget did not refuse before extraction")
	}
}

func TestStageGenerationFailureCleansOnlyCreatedDirectory(t *testing.T) {
	f := payloadInput(t)
	first, err := StageGeneration(context.Background(), f.v, f.state, f.set, f.components, f.archives, f.root)
	if err != nil {
		t.Fatal("fixture stage failed")
	}
	f = payloadNext(t, f, 2)
	if os.WriteFile(f.archives["worker"], []byte("bad archive"), 0600) != nil {
		t.Fatal("fixture corruption failed")
	}
	if _, err := StageGeneration(context.Background(), f.v, f.state, f.set, f.components, f.archives, f.root); err == nil {
		t.Fatal("corrupted archive staged")
	}
	if err := RecheckGeneration(f.root, first); err != nil {
		t.Fatal("failure damaged existing generation")
	}
	entries, err := os.ReadDir(filepath.Join(f.root, "payload"))
	if err != nil {
		t.Fatal("generation list failed")
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			count++
		}
	}
	if count != 1 {
		t.Fatal("failed staging directory remained")
	}
}

func TestGenerationReceiptCannotSubstituteUnsignedMetadata(t *testing.T) {
	for _, mutate := range []struct {
		name string
		fn   func(*Generation)
	}{
		{"host", func(g *Generation) { g.Host = "app/bin/app" }},
		{"inventory_size", func(g *Generation) { g.InventoryBytes = 0 }},
		{"component", func(g *Generation) { g.Components[0].Component.Inventory[0].Size++ }},
		{"args", func(g *Generation) { g.Args = []string{"untrusted"} }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			root, g, _ := payloadFixture(t)
			mutate.fn(&g)
			b, err := json.Marshal(g)
			if err != nil {
				t.Fatal("fixture encoding failed")
			}
			if os.WriteFile(filepath.Join(root, "payload", g.Name, "generation.json"), b, 0600) != nil {
				t.Fatal("fixture receipt mutation failed")
			}
			if _, err := LoadGeneration(root, g.Name); err == nil {
				t.Fatal("unsigned generation metadata accepted")
			}
		})
	}
}

func TestStageGenerationOwnershipAndWriterFence(t *testing.T) {
	t.Run("ownership_overlap", func(t *testing.T) {
		f := payloadInput(t)
		receipt := ChannelReceipt{Channel: "shell", OwnedPaths: []string{filepath.Join(f.root, "payload", "declared-host")}, InstallingIdentity: strconv.Itoa(os.Getuid()), DeclaredAt: f.clock.Now, Revision: 1}
		if err := WriteChannelReceipt(context.Background(), f.root, receipt); err != nil {
			t.Fatalf("fixture receipt: %s", reasonOf(err))
		}
		if _, err := StageGeneration(context.Background(), f.v, f.state, f.set, f.components, f.archives, f.root); err != ReasonOwnershipOverlap {
			t.Fatal("package ownership overlap staged")
		}
		if _, err := os.Lstat(filepath.Join(f.root, "payload")); !os.IsNotExist(err) {
			t.Fatal("ownership refusal created payload directory")
		}
	})
	t.Run("writer_fence", func(t *testing.T) {
		f := payloadInput(t)
		dir := filepath.Join(f.root, "payload")
		if os.Mkdir(dir, 0700) != nil || os.WriteFile(filepath.Join(dir, ".handoff.lock"), nil, 0600) != nil {
			t.Fatal("fixture fence setup failed")
		}
		if _, err := StageGeneration(context.Background(), f.v, f.state, f.set, f.components, f.archives, f.root); err != ReasonStateIO {
			t.Fatal("staging ignored handoff writer fence")
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 1 || entries[0].Name() != ".handoff.lock" {
			t.Fatal("staging disturbed existing writer fence")
		}
	})
}

func TestStageGenerationReconcilesInterruptedActivation(t *testing.T) {
	f := payloadInput(t)
	first, err := StageGeneration(context.Background(), f.v, f.state, f.set, f.components, f.archives, f.root)
	if err != nil {
		t.Fatal("fixture stage failed")
	}
	j, err := loadHandoff(f.root)
	if err != nil {
		t.Fatal("fixture journal read failed")
	}
	j.Generations[first.Name] = HandoffRecord{State: "activating"}
	if err := saveHandoff(f.root, &j); err != nil {
		t.Fatal("fixture intent write failed")
	}
	if _, err := StageGeneration(context.Background(), f.v, f.state, f.set, f.components, f.archives, f.root); err != ReasonGenerationExists {
		t.Fatalf("existing generation refusal changed: %s", reasonOf(err))
	}
	j, err = loadHandoff(f.root)
	if err != nil || j.Generations[first.Name].State != "activated" || j.Generations[first.Name].Receipt == nil {
		t.Fatal("next stage did not reconcile activation intent")
	}
}

func TestStageGenerationRefusesPGDATAAndDescendants(t *testing.T) {
	for _, child := range []bool{false, true} {
		t.Run(strconv.FormatBool(child), func(t *testing.T) {
			f := payloadInput(t)
			if os.WriteFile(filepath.Join(f.root, "PG_VERSION"), []byte("16\n"), 0600) != nil {
				t.Fatal("fixture PGDATA marker")
			}
			root := f.root
			if child {
				root = filepath.Join(root, "nested")
			}
			if _, err := StageGeneration(t.Context(), f.v, f.state, f.set, f.components, f.archives, root); err != ReasonOwnershipOverlap {
				t.Fatal("generation staged within PGDATA")
			}
			if _, err := os.Lstat(filepath.Join(root, "payload")); !os.IsNotExist(err) {
				t.Fatal("PGDATA refusal created payload")
			}
		})
	}
}

func TestStageGenerationDoesNotReuseRetiredIdentity(t *testing.T) {
	f := payloadInput(t)
	g, err := StageGeneration(context.Background(), f.v, f.state, f.set, f.components, f.archives, f.root)
	if err != nil {
		t.Fatal("fixture stage failed")
	}
	j, err := loadHandoff(f.root)
	if err != nil {
		t.Fatal("fixture journal read failed")
	}
	j.Generations[g.Name] = HandoffRecord{State: "retired"}
	if err := saveHandoff(f.root, &j); err != nil {
		t.Fatal("fixture retirement record failed")
	}
	dir := filepath.Join(f.root, "payload", g.Name)
	if os.Chmod(dir, 0700) != nil || os.RemoveAll(dir) != nil {
		t.Fatal("fixture retired generation removal failed")
	}
	if _, err := StageGeneration(context.Background(), f.v, f.state, f.set, f.components, f.archives, f.root); err != ReasonGenerationExists {
		t.Fatal("retired immutable identity reused")
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatal("retired immutable generation recreated")
	}
}
