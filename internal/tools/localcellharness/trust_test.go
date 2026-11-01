package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell/trust"
)

func harnessTrustClock() trust.Clock {
	return trust.Clock{Now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC), Trusted: true}
}

func harnessTrustFixture(t *testing.T) (string, string, string) {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	root := privateHarnessRoot(t)
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal("cannot resolve private test root")
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr == nil && entry.IsDir() {
				_ = os.Chmod(path, 0700)
			}
			return nil
		})
	})
	directory := t.TempDir()
	read := func(name string) []byte {
		data, err := os.ReadFile(filepath.Join("../../localcell/trust/testdata/vectors", name))
		if err != nil {
			t.Fatal("cannot read frozen fixture")
		}
		return data
	}
	for destination, source := range map[string]string{
		"component-set.json": "positive-built-dist.json", "component-set.json.sig.json": "positive-built-dist.json.sig.json",
		"delegation.json": "setup-delegation.json", "delegation.json.sig.json": "setup-delegation.json.sig.json",
	} {
		if err := os.WriteFile(filepath.Join(directory, destination), read(source), 0600); err != nil {
			t.Fatal("cannot write fixture bundle")
		}
	}
	manifest, err := trust.ParseComponentSet(read("positive-built-dist.json"))
	if err != nil {
		t.Fatal("cannot parse frozen fixture")
	}
	// Reproduce the committed positive-built-dist recipe directly; no compiler,
	// generated signing key, or candidate execution contributes to this test.
	for _, component := range manifest.Platforms[0].Components {
		var archive bytes.Buffer
		gz, err := gzip.NewWriterLevel(&archive, gzip.NoCompression)
		if err != nil {
			t.Fatal("cannot prepare fixture archive")
		}
		gz.Header = gzip.Header{ModTime: time.Unix(0, 0).UTC(), OS: 255}
		tarWriter := tar.NewWriter(gz)
		header := &tar.Header{Name: component.Inventory[0].Path, Mode: 0755, Size: 4, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR, ModTime: time.Unix(0, 0).UTC()}
		if tarWriter.WriteHeader(header) != nil {
			t.Fatal("cannot prepare fixture member")
		}
		if _, err := tarWriter.Write([]byte("data")); err != nil {
			t.Fatal("cannot write fixture member")
		}
		if tarWriter.Close() != nil || gz.Close() != nil {
			t.Fatal("cannot finish fixture archive")
		}
		digest := sha256.Sum256(archive.Bytes())
		if hex.EncodeToString(digest[:]) != component.Archive.SHA256 || uint64(archive.Len()) != component.Archive.Size {
			t.Fatal("fixture recipe differs from frozen archive declaration")
		}
		if err := os.WriteFile(filepath.Join(directory, component.Archive.Name), archive.Bytes(), 0600); err != nil {
			t.Fatal("cannot write fixture archive")
		}
	}
	key, err := filepath.Abs("../../localcell/trust/testdata/keys/TEST-ONLY-root-a.public.pem")
	if err != nil {
		t.Fatal("cannot resolve public fixture")
	}
	return root, directory, key
}

func trustHarnessArgs(group, action, root, directory, key string, extra ...string) []string {
	args := []string{group, action, "--root", root, "--component-set-dir", directory, "--trust-root-pem", key, "--os", "linux", "--arch", "amd64"}
	return append(args, extra...)
}

func trustHarnessRun(t *testing.T, clock trust.Clock, args []string) (string, error) {
	t.Helper()
	var output bytes.Buffer
	err := runTrustCommand(t.Context(), args, &output, clock)
	if regexp.MustCompile(`[0-9a-fA-F]{64}`).Match(output.Bytes()) {
		t.Fatal("harness output revealed a private value")
	}
	for index, value := range args {
		if index > 0 && (args[index-1] == "--root" || args[index-1] == "--component-set-dir" || args[index-1] == "--owned-path") && strings.Contains(output.String(), value) {
			t.Fatal("harness output revealed a caller path")
		}
	}
	return output.String(), err
}

func TestTrustHarnessRequiresExplicitInputs(t *testing.T) {
	for _, pair := range [][2]string{{"payload", "stage"}, {"payload", "activate"}, {"payload", "retire"}, {"payload", "inspect"}, {"channel", "receipt"}, {"channel", "inspect"}, {"channel", "preflight"}} {
		t.Run(pair[0]+"_"+pair[1], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "must-not-exist")
			for _, args := range [][]string{
				{pair[0], pair[1], "--root", root, "--component-set-dir", "unused"},
				{pair[0], pair[1], "--component-set-dir", "unused", "--trust-root-pem", "unused"},
				{pair[0], pair[1], "--root", root, "--trust-root-pem", "unused"},
			} {
				var output bytes.Buffer
				if err := run(context.Background(), args, &output); err == nil || output.Len() != 0 {
					t.Fatal("missing explicit input accepted or emitted output")
				}
			}
			if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid command mutated its root")
			}
			if err := run(context.Background(), []string{pair[0], pair[1], "--root", root, "--component-set-dir", "unused"}, io.Discard); err != trust.ReasonNoTrustRoot {
				t.Fatal("missing production roots did not fail closed")
			}
		})
	}
}

func TestPayloadHarnessStagesActivatesInspects(t *testing.T) {
	root, directory, key := harnessTrustFixture(t)
	if harnessWindowsPersistenceRefusal(t, root, directory, key) {
		return
	}
	clock := harnessTrustClock()
	if _, err := trustHarnessRun(t, clock, trustHarnessArgs("payload", "stage", root, directory, key)); err != nil {
		t.Fatal("stage refused:", err)
	}
	journal, err := trust.InspectHandoff(t.Context(), root)
	if err != nil || len(journal.Generations) != 1 {
		t.Fatal("stage did not journal one generation")
	}
	var generation string
	for name := range journal.Generations {
		generation = name
	}
	if _, err := trustHarnessRun(t, clock, trustHarnessArgs("payload", "activate", root, directory, key, "--generation", generation, "--launch-check")); err != nil {
		t.Fatal("activation refused:", err)
	}
	output, err := trustHarnessRun(t, clock, trustHarnessArgs("payload", "inspect", root, directory, key))
	if err != nil || !strings.Contains(output, `"activated":1`) {
		t.Fatal("inspection omitted activated count")
	}
	if _, err := trustHarnessRun(t, clock, trustHarnessArgs("payload", "retire", root, directory, key, "--generation", generation)); err != trust.ReasonGenerationReferenced {
		t.Fatal("activated retirement did not refuse")
	}
	clock.Now = clock.Now.AddDate(20, 0, 0)
	if _, err := trustHarnessRun(t, clock, trustHarnessArgs("payload", "activate", root, directory, key, "--generation", generation, "--launch-check")); err != nil {
		t.Fatal("committed offline activation refused:", err)
	}
}

func TestPayloadHarnessExpiredFirstActivationAndWrongRootRefuse(t *testing.T) {
	root, directory, key := harnessTrustFixture(t)
	if harnessWindowsPersistenceRefusal(t, root, directory, key) {
		return
	}
	clock := harnessTrustClock()
	if _, err := trustHarnessRun(t, clock, trustHarnessArgs("payload", "stage", root, directory, key)); err != nil {
		t.Fatal("stage refused:", err)
	}
	journal, err := trust.InspectHandoff(t.Context(), root)
	if err != nil {
		t.Fatal("cannot inspect staged generation")
	}
	var generation string
	for name := range journal.Generations {
		generation = name
	}
	expired := clock
	expired.Now = clock.Now.AddDate(20, 0, 0)
	if _, err := trustHarnessRun(t, expired, trustHarnessArgs("payload", "activate", root, directory, key, "--generation", generation)); err != trust.ReasonExpired {
		t.Fatal("expired first activation did not refuse")
	}
	otherKey := filepath.Join(filepath.Dir(key), "TEST-ONLY-root-b.public.pem")
	if _, err := trustHarnessRun(t, clock, trustHarnessArgs("payload", "activate", root, directory, otherKey, "--generation", generation)); err != trust.ReasonStateInvalid {
		t.Fatal("different explicit root did not refuse")
	}
}

func TestChannelHarnessReceiptInspectPreflight(t *testing.T) {
	root, directory, key := harnessTrustFixture(t)
	if harnessWindowsPersistenceRefusal(t, root, directory, key) {
		return
	}
	clock := harnessTrustClock()
	owned := filepath.Join(filepath.Dir(root), "package-cli")
	if err := os.WriteFile(owned, []byte("fixture"), 0700); err != nil {
		t.Fatal("cannot write owned fixture")
	}
	if _, err := trustHarnessRun(t, clock, trustHarnessArgs("channel", "receipt", root, directory, key, "--channel", "homebrew", "--owned-path", owned, "--installing-identity", strconv.Itoa(os.Getuid()), "--revision", "1")); err != nil {
		t.Fatal("receipt refused:", err)
	}
	receipt, err := trust.ReadChannelReceipt(root)
	if err != nil || receipt.ComponentSet == nil || receipt.ComponentSet.Sequence == 0 {
		t.Fatal("receipt omitted verified component-set identity")
	}
	output, err := trustHarnessRun(t, clock, trustHarnessArgs("channel", "inspect", root, directory, key))
	var observation trust.ChannelObservation
	if err != nil || json.Unmarshal([]byte(output), &observation) != nil || !observation.ReceiptPresent || len(observation.Paths) != 1 || observation.Paths[0].Status != "present" {
		t.Fatal("inspect omitted owned fixture")
	}
	for _, candidate := range []struct {
		path    string
		allowed bool
	}{{owned, false}, {filepath.Join(root, "payload", "next"), true}} {
		output, err := trustHarnessRun(t, clock, trustHarnessArgs("channel", "preflight", root, directory, key, "--write-path", candidate.path))
		var decision trust.PreflightDecision
		if err != nil || json.Unmarshal([]byte(output), &decision) != nil || decision.Allowed != candidate.allowed {
			t.Fatal("preflight ownership classification incorrect")
		}
	}
}

func harnessWindowsPersistenceRefusal(t *testing.T, root, directory, key string) bool {
	t.Helper()
	if runtime.GOOS != "windows" {
		return false
	}
	if _, err := trustHarnessRun(t, harnessTrustClock(), trustHarnessArgs("payload", "stage", root, directory, key)); err != trust.ReasonStateIO {
		t.Fatal("Windows trust persistence did not fail closed")
	}
	t.Log("Windows fail-closed persistence verified; NOT_RUN generation lifecycle requires private ACL adapter")
	return true
}

func TestTrustHarnessRejectsTamperedAndUnboundedBundles(t *testing.T) {
	for _, name := range []string{"tampered", "oversize", "rotation_gap", "rotation_bound", "non_test_root"} {
		t.Run(name, func(t *testing.T) {
			root, directory, key := harnessTrustFixture(t)
			switch name {
			case "tampered":
				file, err := os.OpenFile(filepath.Join(directory, "component-set.json"), os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal("cannot mutate fixture")
				}
				_, writeErr := file.WriteString("\n")
				if file.Close() != nil || writeErr != nil {
					t.Fatal("cannot finish fixture mutation")
				}
			case "oversize":
				if os.WriteFile(filepath.Join(directory, "component-set.json"), bytes.Repeat([]byte{' '}, trust.MaxComponentSetBytes+1), 0600) != nil {
					t.Fatal("cannot write oversized fixture")
				}
			case "rotation_gap", "rotation_bound":
				index := "2"
				if name == "rotation_bound" {
					index = "34"
				}
				if os.WriteFile(filepath.Join(directory, "root-rotation-"+index+".json"), []byte("{}"), 0600) != nil {
					t.Fatal("cannot write rotation fixture")
				}
			case "non_test_root":
				data, err := os.ReadFile(key)
				if err != nil {
					t.Fatal("cannot read public root")
				}
				key = filepath.Join(directory, "public.pem")
				if os.WriteFile(key, bytes.ReplaceAll(data, []byte("TEST-ONLY PUBLIC KEY"), []byte("PUBLIC KEY")), 0600) != nil {
					t.Fatal("cannot write public root fixture")
				}
			}
			if _, err := trustHarnessRun(t, harnessTrustClock(), trustHarnessArgs("channel", "inspect", root, directory, key)); err == nil {
				t.Fatal("invalid bundle accepted")
			}
		})
	}
}

func TestChannelHarnessDoesNotInferOwnedPaths(t *testing.T) {
	for _, channel := range []string{"shell", "homebrew"} {
		t.Run(channel, func(t *testing.T) {
			root, directory, key := harnessTrustFixture(t)
			if harnessWindowsPersistenceRefusal(t, root, directory, key) {
				return
			}
			// Commit the signed fixture using its fixed clock so the top-level
			// dispatch below can authenticate it offline after fixture expiry.
			if _, err := trustHarnessRun(t, harnessTrustClock(), trustHarnessArgs("payload", "stage", root, directory, key)); err != nil {
				t.Fatal("cannot prepare committed fixture:", err)
			}
			ambient := t.TempDir()
			pathDir := filepath.Join(ambient, "path-bin")
			cellarDir := filepath.Join(ambient, "Cellar", "witself", "v1", "bin")
			for _, dir := range []string{filepath.Join(root, "bin"), pathDir, cellarDir} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal("create fake installation directory")
				}
				if err := os.WriteFile(filepath.Join(dir, "witself"), []byte("fake installed executable"), 0700); err != nil {
					t.Fatal("populate fake installation directory")
				}
			}
			t.Setenv("PATH", pathDir)
			t.Setenv("HOMEBREW_CELLAR", filepath.Dir(filepath.Dir(filepath.Dir(cellarDir))))
			args := trustHarnessArgs("channel", "receipt", root, directory, key, "--channel", channel, "--installing-identity", strconv.Itoa(os.Getuid()), "--revision", "1")
			var output bytes.Buffer
			if err := run(t.Context(), args, &output); err != nil {
				t.Fatal("empty ownership declaration dispatch refused:", err)
			}
			if regexp.MustCompile(`[0-9a-fA-F]{64}`).Match(output.Bytes()) {
				t.Fatal("receipt dispatch revealed a private value")
			}
			persisted, err := trust.ReadChannelReceipt(root)
			if err != nil || persisted.Channel != channel || persisted.Revision != 1 || persisted.ComponentSet == nil {
				t.Fatal("empty ownership declaration dispatch did not persist a verified receipt")
			}
			if len(persisted.OwnedPaths) != 0 {
				t.Fatal("empty declaration dispatch inferred package-owned paths")
			}
		})
	}
}
