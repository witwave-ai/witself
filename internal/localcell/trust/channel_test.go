package trust

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func channelTestRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil || os.Chmod(root, 0700) != nil {
		t.Fatal("create channel fixture root")
	}
	return root
}

func channelTestIdentity() string {
	if runtime.GOOS == "windows" {
		return "S-1-5-21-1"
	}
	return strconv.Itoa(os.Geteuid())
}

func channelTestReceipt(root, channel string) ChannelReceipt {
	return ChannelReceipt{
		Channel: channel, OwnedPaths: []string{filepath.Join(root, "package-host")},
		InstallingIdentity: channelTestIdentity(), DeclaredAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Revision: 1,
		ComponentSet: &ComponentSetIdentity{ManifestDigest: strings.Repeat("a", 64), Channel: "stable", Sequence: 1, Tag: "v1"},
	}
}

func channelTestLayout(root string) ChannelLayout {
	return ChannelLayout{DataRoot: filepath.Join(root, "data"), PGDATA: filepath.Join(root, "pgdata"), PayloadRoot: filepath.Join(root, "payload")}
}

func channelSkipWindowsPersistence(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("NOT_RUN windows_private_acl_persistence_unavailable")
	}
}

func TestChannelReceiptRoundTripAndRevisionFence(t *testing.T) {
	channelSkipWindowsPersistence(t)
	root := channelTestRoot(t)
	receipt := channelTestReceipt(root, "shell")
	missing, err := ReadChannelReceipt(root)
	if err != nil || missing.Channel != "unmanaged" || missing.Revision != 0 {
		t.Fatal("missing receipt did not mean unmanaged")
	}
	if err := WriteChannelReceipt(context.Background(), root, receipt); err != nil {
		t.Fatal("receipt write failed", err)
	}
	loaded, err := ReadChannelReceipt(root)
	if err != nil || !reflect.DeepEqual(loaded, receipt) {
		t.Fatal("receipt round trip changed declaration")
	}
	info, err := os.Stat(filepath.Join(root, "channel", "receipt.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("receipt is not private")
	}
	if err := WriteChannelReceipt(context.Background(), root, receipt); !errors.Is(err, ReasonChannelRevision) {
		t.Fatal("stale receipt revision accepted")
	}
	receipt.Revision = 2
	if err := WriteChannelReceipt(context.Background(), root, receipt); err != nil {
		t.Fatal("next receipt revision refused")
	}
	loaded, err = ReadChannelReceipt(root)
	if err != nil || loaded.Revision != 2 {
		t.Fatal("next revision not committed")
	}
}

func TestChannelReceiptWriterFenceAndConcurrentRevision(t *testing.T) {
	channelSkipWindowsPersistence(t)
	root := channelTestRoot(t)
	receipt := channelTestReceipt(root, "shell")
	if err := WriteChannelReceipt(context.Background(), root, receipt); err != nil {
		t.Fatal("initial receipt write failed")
	}
	lock := filepath.Join(root, "channel", ".channel-receipt.lock")
	if err := os.WriteFile(lock, nil, 0600); err != nil {
		t.Fatal("fixture lock creation failed")
	}
	receipt.Revision = 2
	if err := WriteChannelReceipt(context.Background(), root, receipt); err == nil {
		t.Fatal("interrupted writer fence was ignored")
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal("fixture fence removal failed")
	}
	const writers = 8
	results := make(chan error, writers)
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() { results <- WriteChannelReceipt(context.Background(), root, receipt) })
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		}
	}
	loaded, err := ReadChannelReceipt(root)
	if accepted != 1 || err != nil || loaded.Revision != 2 {
		t.Fatal("concurrent receipt transaction lost its revision fence")
	}
}

func TestChannelReceiptBoundsPathsAndDeclarationValidation(t *testing.T) {
	channelSkipWindowsPersistence(t)
	root := channelTestRoot(t)
	cases := map[string]func(*ChannelReceipt){
		"unknown_channel":   func(r *ChannelReceipt) { r.Channel = "auto" },
		"zero_revision":     func(r *ChannelReceipt) { r.Revision = 0 },
		"missing_time":      func(r *ChannelReceipt) { r.DeclaredAt = time.Time{} },
		"invalid_identity":  func(r *ChannelReceipt) { r.InstallingIdentity = "inferred-user" },
		"relative":          func(r *ChannelReceipt) { r.OwnedPaths = []string{"package-host"} },
		"unclean":           func(r *ChannelReceipt) { r.OwnedPaths = []string{root + "/unused/../package-host"} },
		"too_many":          func(r *ChannelReceipt) { r.OwnedPaths = make([]string, 65) },
		"too_long":          func(r *ChannelReceipt) { r.OwnedPaths = []string{root + "/" + strings.Repeat("a", 1024)} },
		"duplicate":         func(r *ChannelReceipt) { r.OwnedPaths = append(r.OwnedPaths, r.OwnedPaths[0]) },
		"manifest_identity": func(r *ChannelReceipt) { r.ComponentSet.ManifestDigest = "invalid" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			receipt := channelTestReceipt(root, "shell")
			mutate(&receipt)
			if err := WriteChannelReceipt(context.Background(), root, receipt); !errors.Is(err, ReasonChannelReceiptInvalid) {
				t.Fatal("invalid declaration did not refuse")
			}
		})
	}
	actual := filepath.Join(root, "actual")
	if err := os.Mkdir(actual, 0700); err != nil {
		t.Fatal("create ancestor fixture")
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal("create symlink fixture")
	}
	receipt := channelTestReceipt(root, "shell")
	receipt.OwnedPaths = []string{filepath.Join(link, "missing-host")}
	if err := WriteChannelReceipt(context.Background(), root, receipt); !errors.Is(err, ReasonChannelReceiptInvalid) {
		t.Fatal("symlink ancestor accepted in declaration")
	}
}

func TestChannelReceiptReadRevalidatesPrivacyAndSymlinks(t *testing.T) {
	channelSkipWindowsPersistence(t)
	for _, name := range []string{"root_mode", "directory_mode", "directory_readonly", "file_mode", "directory_symlink", "file_symlink", "malformed"} {
		t.Run(name, func(t *testing.T) {
			root := channelTestRoot(t)
			if err := WriteChannelReceipt(context.Background(), root, channelTestReceipt(root, "shell")); err != nil {
				t.Fatal("initial receipt write failed")
			}
			dir := filepath.Join(root, "channel")
			file := filepath.Join(dir, "receipt.json")
			var err error
			switch name {
			case "root_mode":
				err = os.Chmod(root, 0755)
			case "directory_mode":
				err = os.Chmod(dir, 0755)
			case "directory_readonly":
				err = os.Chmod(dir, 0500)
				t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
			case "file_mode":
				err = os.Chmod(file, 0644)
			case "directory_symlink":
				err = os.Rename(dir, dir+"-old")
				if err == nil {
					err = os.Symlink(dir+"-old", dir)
				}
			case "file_symlink":
				err = os.Rename(file, file+"-old")
				if err == nil {
					err = os.Symlink(file+"-old", file)
				}
			case "malformed":
				err = os.WriteFile(file, []byte(`{"channel":"shell","channel":"homebrew"}`), 0600)
			}
			if err != nil {
				t.Fatal("privacy mutation failed")
			}
			if _, err := ReadChannelReceipt(root); err == nil {
				t.Fatal("changed privacy or malformed receipt accepted")
			}
		})
	}
}

func TestChannelAdaptersInspectDeclaredPaths(t *testing.T) {
	channelSkipWindowsPersistence(t)
	for _, channel := range []string{"shell", "homebrew"} {
		t.Run(channel, func(t *testing.T) {
			root := channelTestRoot(t)
			receipt := channelTestReceipt(root, channel)
			present := receipt.OwnedPaths[0]
			absent := filepath.Join(root, "absent")
			link := filepath.Join(root, "swapped")
			if err := os.WriteFile(present, []byte("fixture"), 0600); err != nil {
				t.Fatal("create owned fixture")
			}
			if err := os.Symlink(present, link); err != nil {
				t.Fatal("create swapped fixture")
			}
			receipt.OwnedPaths = []string{present, absent, link}
			adapter := NewChannelAdapter(channel, channelTestLayout(root))
			got, err := adapter.Inspect(context.Background(), receipt)
			want := []OwnedPathObservation{{Index: 0, Status: "present"}, {Index: 1, Status: "absent"}, {Index: 2, Status: "foreign"}}
			if err != nil || !got.ReceiptPresent || !reflect.DeepEqual(got.Paths, want) {
				t.Fatal("inspect did not preserve declared path statuses")
			}
			receipt.InstallingIdentity = strconv.Itoa(os.Geteuid() + 1)
			got, err = adapter.Inspect(context.Background(), receipt)
			if err != nil || got.Paths[0].Status != "foreign" {
				t.Fatal("foreign owner not reported")
			}
		})
	}
}

func TestChannelAdaptersOverlapAndLifecyclePreflight(t *testing.T) {
	channelSkipWindowsPersistence(t)
	root := channelTestRoot(t)
	layout := channelTestLayout(root)
	for _, channel := range []string{"shell", "homebrew"} {
		receipt := channelTestReceipt(root, channel)
		adapter := NewChannelAdapter(channel, layout)
		for _, protected := range []string{layout.DataRoot, layout.PGDATA, filepath.Join(layout.PayloadRoot, "generation")} {
			overlap := receipt
			overlap.OwnedPaths = []string{filepath.Join(protected, "host")}
			observation, err := adapter.Inspect(context.Background(), overlap)
			if !errors.Is(err, ReasonOwnershipOverlap) || observation.Reason != ReasonOwnershipOverlap {
				t.Fatal("lifecycle ownership overlap accepted")
			}
			decision, err := adapter.PreflightUpdate(context.Background(), overlap, ChannelCandidate{WritePaths: []string{filepath.Join(layout.PayloadRoot, "new")}})
			if err != nil || decision.Allowed || decision.Reason != ReasonOwnershipOverlap {
				t.Fatal("preflight ignored ownership overlap")
			}
		}
		for _, target := range []string{receipt.OwnedPaths[0], root, filepath.Join(root, "unclaimed")} {
			decision, err := adapter.PreflightUpdate(context.Background(), receipt, ChannelCandidate{WritePaths: []string{target}})
			if err != nil || decision.Allowed || decision.Reason != ReasonChannelActionRequired {
				t.Fatal("package or undeclared ownership overwrite allowed")
			}
		}
		decision, err := adapter.PreflightUpdate(context.Background(), receipt, ChannelCandidate{WritePaths: []string{filepath.Join(layout.PayloadRoot, "new-generation", "host")}})
		if err != nil || !decision.Allowed || decision.Reason != "allowed" {
			t.Fatal("explicit lifecycle target refused")
		}
	}
}

func TestChannelUnmanagedAndUnimplementedRefuse(t *testing.T) {
	root := channelTestRoot(t)
	layout := channelTestLayout(root)
	missing := ChannelReceipt{Channel: "unmanaged"}
	adapter := NewChannelAdapter("unmanaged", layout)
	observation, err := adapter.Inspect(context.Background(), missing)
	if err != nil || observation.ReceiptPresent {
		t.Fatal("unmanaged absence misreported")
	}
	decision, err := adapter.PreflightUpdate(context.Background(), missing, ChannelCandidate{WritePaths: []string{filepath.Join(layout.PayloadRoot, "host")}})
	if err != nil || decision.Allowed || decision.Reason != ReasonChannelActionRequired {
		t.Fatal("unmanaged mutation allowed")
	}
	for _, channel := range []string{"linux_package", "msi"} {
		adapter := NewChannelAdapter(channel, layout)
		receipt := channelTestReceipt(root, channel)
		observation, err := adapter.Inspect(context.Background(), receipt)
		if !errors.Is(err, ReasonChannelNotImplemented) || observation.Reason != ReasonChannelNotImplemented {
			t.Fatal("unimplemented inspect did not return fixed reason")
		}
		decision, err := adapter.PreflightUpdate(context.Background(), receipt, ChannelCandidate{WritePaths: receipt.OwnedPaths})
		if err != nil || decision.Allowed || decision.Reason != ReasonChannelNotImplemented {
			t.Fatal("unimplemented preflight did not return fixed reason")
		}
	}
}

func TestChannelOwnershipGuardAllowsMissingAndRefusesOverlap(t *testing.T) {
	channelSkipWindowsPersistence(t)
	root := channelTestRoot(t)
	if err := checkPayloadOwnership(root); err != nil {
		t.Fatal("missing receipt prevented private lifecycle staging")
	}
	receipt := channelTestReceipt(root, "homebrew")
	receipt.OwnedPaths = []string{filepath.Join(root, "payload", "host")}
	if err := WriteChannelReceipt(context.Background(), root, receipt); err != nil {
		t.Fatal("explicit overlap declaration write failed")
	}
	if err := checkPayloadOwnership(root); !errors.Is(err, ReasonOwnershipOverlap) {
		t.Fatal("payload ownership guard accepted package overlap")
	}
}

func TestChannelReceiptWindowsPersistenceFailsClosed(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("NOT_RUN windows_native_private_acl_refusal")
	}
	root := channelTestRoot(t)
	if err := WriteChannelReceipt(context.Background(), root, channelTestReceipt(root, "msi")); !errors.Is(err, ReasonStateIO) {
		t.Fatal("Windows mode bits incorrectly proved receipt privacy")
	}
	if _, err := ReadChannelReceipt(root); !errors.Is(err, ReasonStateIO) {
		t.Fatal("Windows receipt read did not fail closed")
	}
}

func TestChannelReceiptDoesNotInferOwnedPaths(t *testing.T) {
	channelSkipWindowsPersistence(t)
	for _, channel := range []string{"shell", "homebrew"} {
		t.Run(channel, func(t *testing.T) {
			root := channelTestRoot(t)
			ambient := channelTestRoot(t)
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
			t.Setenv("WITSELF_HOME", t.TempDir())
			t.Setenv("DSH_HOME", t.TempDir())
			receipt := channelTestReceipt(root, channel)
			receipt.OwnedPaths = []string{}
			if err := WriteChannelReceipt(t.Context(), root, receipt); err != nil {
				t.Fatal("empty ownership declaration refused:", err)
			}
			persisted, err := ReadChannelReceipt(root)
			if err != nil || persisted.Channel != channel || persisted.Revision != 1 {
				t.Fatal("empty ownership declaration was not persisted")
			}
			if len(persisted.OwnedPaths) != 0 {
				t.Fatal("empty declaration inferred package-owned paths")
			}
		})
	}
}
