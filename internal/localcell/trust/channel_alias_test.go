package trust

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func requireChannelAlias(t *testing.T, original, alias string) {
	t.Helper()
	first, err := os.Lstat(original)
	if err != nil {
		t.Fatal("original alias fixture unavailable")
	}
	second, err := os.Lstat(alias)
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("NOT RUN alias_not_same_file")
	}
	if err != nil {
		t.Fatal("alias fixture observation failed")
	}
	if !os.SameFile(first, second) {
		t.Skip("NOT RUN alias_not_same_file")
	}
}

func TestChannelCaseAliasRefusesOverlap(t *testing.T) {
	root := channelTestRoot(t)
	layout := channelTestLayout(root)
	if os.Mkdir(layout.DataRoot, 0700) != nil || os.WriteFile(filepath.Join(layout.DataRoot, "host"), []byte("fixture"), 0700) != nil {
		t.Fatal("create file alias fixture")
	}
	alias := filepath.Join(root, "DATA", "host")
	requireChannelAlias(t, filepath.Join(layout.DataRoot, "host"), alias)
	for _, channel := range []string{"shell", "homebrew"} {
		t.Run(channel, func(t *testing.T) {
			receipt := channelTestReceipt(root, channel)
			receipt.OwnedPaths = []string{alias}
			adapter := NewChannelAdapter(channel, layout)
			got, err := adapter.Inspect(t.Context(), receipt)
			if !errors.Is(err, ReasonOwnershipOverlap) || got.Reason != ReasonOwnershipOverlap {
				t.Error("file alias ownership overlap not reported")
			}
			decision, err := adapter.PreflightUpdate(t.Context(), receipt, ChannelCandidate{WritePaths: []string{filepath.Join(layout.DataRoot, "host")}})
			if err != nil || decision.Allowed || decision.Reason != ReasonOwnershipOverlap {
				t.Error("package-owned alias write not refused")
			}
		})
	}
}

func TestChannelDirectoryAliasRefusesMissingSuffix(t *testing.T) {
	root := channelTestRoot(t)
	layout := channelTestLayout(root)
	if err := os.Mkdir(layout.DataRoot, 0700); err != nil {
		t.Fatal("create directory alias fixture")
	}
	alias := filepath.Join(root, "DATA")
	requireChannelAlias(t, layout.DataRoot, alias)
	target := filepath.Join(layout.DataRoot, "sub", "new-file")
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing suffix fixture already exists")
	}
	for _, channel := range []string{"shell", "homebrew"} {
		t.Run(channel, func(t *testing.T) {
			receipt := channelTestReceipt(root, channel)
			receipt.OwnedPaths = []string{alias}
			adapter := NewChannelAdapter(channel, layout)
			got, err := adapter.Inspect(t.Context(), receipt)
			if !errors.Is(err, ReasonOwnershipOverlap) || got.Reason != ReasonOwnershipOverlap {
				t.Error("directory alias ownership overlap not reported")
			}
			decision, err := adapter.PreflightUpdate(t.Context(), receipt, ChannelCandidate{WritePaths: []string{target}})
			if err != nil || decision.Allowed || decision.Reason != ReasonOwnershipOverlap {
				t.Error("missing descendant of package-owned alias allowed")
			}
		})
	}
}

func TestStageGenerationCaseAliasOwnership(t *testing.T) {
	f := payloadInput(t)
	root := filepath.Join(f.root, "data")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal("create staging alias fixture")
	}
	alias := filepath.Join(f.root, "DATA")
	requireChannelAlias(t, root, alias)
	receipt := channelTestReceipt(root, "shell")
	receipt.OwnedPaths = []string{filepath.Join(alias, "sub", "new-file")}
	if err := WriteChannelReceipt(t.Context(), root, receipt); err != nil {
		t.Fatal("write staging alias declaration")
	}
	if _, err := StageGeneration(t.Context(), f.v, f.state, f.set, f.components, f.archives, root); !errors.Is(err, ReasonOwnershipOverlap) {
		t.Fatal("package-owned alias allowed staging")
	}
	if _, err := os.Lstat(filepath.Join(root, "payload")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ownership refusal created payload directory")
	}
}

func TestChannelLifecyclePreflightUsesAliasAncestry(t *testing.T) {
	root := channelTestRoot(t)
	data := filepath.Join(root, "data")
	if err := os.Mkdir(data, 0700); err != nil {
		t.Fatal("create lifecycle alias fixture")
	}
	alias := filepath.Join(root, "DATA")
	requireChannelAlias(t, data, alias)
	receipt := channelTestReceipt(root, "shell")
	receipt.OwnedPaths = nil
	for _, suffix := range []string{"", "pending"} {
		layout := channelTestLayout(root)
		layout.DataRoot = filepath.Join(alias, suffix)
		target := filepath.Join(data, suffix, "sub", "new-file")
		decision, err := NewChannelAdapter("shell", layout).PreflightUpdate(t.Context(), receipt, ChannelCandidate{WritePaths: []string{target}})
		if err != nil || !decision.Allowed {
			t.Fatal("existing alias identity did not prove lifecycle ancestry")
		}
	}
}

func TestChannelPathAncestryRefusesAmbiguityAndLinks(t *testing.T) {
	root := channelTestRoot(t)
	for _, suffix := range []string{"PENDING", "pending.", "pending ", "pending:stream", "PENDIN~1", "pénding"} {
		if _, err := channelPathsOverlap(filepath.Join(root, "pending"), filepath.Join(root, suffix, "new-file")); err == nil {
			t.Error("unresolved spelling ambiguity treated as disjoint ownership")
		}
	}
	data := filepath.Join(root, "data")
	if err := os.Mkdir(data, 0700); err != nil {
		t.Fatal("create link destination fixture")
	}
	link := filepath.Join(root, "package-link")
	if err := os.Symlink(data, link); err != nil {
		t.Skip("NOT RUN symlink_fixture_unavailable")
	}
	receipt := channelTestReceipt(root, "shell")
	receipt.OwnedPaths = []string{link}
	adapter := NewChannelAdapter("shell", channelTestLayout(root))
	observation, err := adapter.Inspect(t.Context(), receipt)
	if err != nil || len(observation.Paths) != 1 || observation.Paths[0].Status != "foreign" {
		t.Fatal("unsafe declaration did not remain observable as foreign")
	}
	decision, err := adapter.PreflightUpdate(t.Context(), receipt, ChannelCandidate{WritePaths: []string{filepath.Join(data, "new-file")}})
	if err != nil || decision.Allowed {
		t.Fatal("foreign link declaration authorized a lifecycle write")
	}
}

func TestChannelAncestryRecheckRefusesChangedObservation(t *testing.T) {
	for _, change := range []string{"ancestor_replaced", "suffix_created"} {
		t.Run(change, func(t *testing.T) {
			root := channelTestRoot(t)
			data := filepath.Join(root, "data")
			if err := os.Mkdir(data, 0700); err != nil {
				t.Fatal("create observed ancestry fixture")
			}
			observed, err := observeChannelAncestry(filepath.Join(data, "pending", "host"))
			if err != nil {
				t.Fatal("observe ancestry fixture")
			}
			if change == "ancestor_replaced" {
				if os.Rename(data, data+"-old") != nil || os.Mkdir(data, 0700) != nil {
					t.Fatal("replace observed ancestor")
				}
			} else if os.Mkdir(filepath.Join(data, "pending"), 0700) != nil {
				t.Fatal("create previously absent suffix")
			}
			if observed.recheck() == nil {
				t.Fatal("changed ancestry retained ownership authority")
			}
		})
	}
}
