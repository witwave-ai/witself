package trust

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type channelAncestor struct {
	name     string
	identity HostIdentity
}

type channelAncestry struct {
	existing []channelAncestor // filesystem root through nearest existing ancestor
	missing  []string          // unresolved suffix, in pathname order
}

// observeChannelAncestry walks from the filesystem root so a known symlink or
// reparse point is refused before inspecting anything below it. Existing names
// are bound to file identities, not their spelling. Missing suffixes remain
// relative to the nearest observed ancestor, including when it has an alias.
func observeChannelAncestry(name string) (channelAncestry, error) {
	var observed channelAncestry
	if !channelCleanAbsolute(name) {
		return observed, ReasonChannelReceiptInvalid
	}
	var names []string
	for current := name; ; current = filepath.Dir(current) {
		names = append(names, current)
		if current == filepath.Dir(current) {
			break
		}
	}
	slices.Reverse(names)
	for i, current := range names {
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) && len(observed.existing) != 0 {
			for _, missing := range names[i:] {
				observed.missing = append(observed.missing, filepath.Base(missing))
			}
			break
		}
		if err != nil {
			return channelAncestry{}, ReasonStateIO
		}
		if !inventoryInfoSafe(info) || (!info.IsDir() && (i != len(names)-1 || !info.Mode().IsRegular())) {
			return channelAncestry{}, ReasonChannelReceiptInvalid
		}
		identity, err := channelFileIdentity(current, info)
		if err != nil {
			return channelAncestry{}, err
		}
		observed.existing = append(observed.existing, channelAncestor{current, identity})
	}
	return observed, nil
}

func (a channelAncestry) recheck() error {
	for _, ancestor := range a.existing {
		info, err := os.Lstat(ancestor.name)
		if err != nil || !inventoryInfoSafe(info) {
			return ReasonChannelReceiptInvalid
		}
		identity, err := channelFileIdentity(ancestor.name, info)
		if err != nil || identity != ancestor.identity {
			return ReasonChannelReceiptInvalid
		}
	}
	if len(a.missing) != 0 {
		first := filepath.Join(a.existing[len(a.existing)-1].name, a.missing[0])
		if _, err := os.Lstat(first); !errors.Is(err, os.ErrNotExist) {
			return ReasonChannelReceiptInvalid
		}
	}
	return nil
}

// channelPathWithin is a check-time observation, not an atomic mutation fence.
// Incomparable missing spellings fail closed instead of guessing case rules for
// a volume (or for a Windows directory with its own case-sensitivity setting).
func channelPathWithin(root, name string) (bool, error) {
	parent, err := observeChannelAncestry(root)
	if err != nil {
		return false, err
	}
	child, err := observeChannelAncestry(name)
	if err != nil {
		return false, err
	}
	if err := parent.recheck(); err != nil {
		return false, err
	}
	if err := child.recheck(); err != nil {
		return false, err
	}
	parentBase := parent.existing[len(parent.existing)-1].identity
	if len(parent.missing) == 0 {
		for _, ancestor := range child.existing {
			if parentBase == ancestor.identity {
				return true, nil
			}
		}
		return false, nil
	}
	if len(child.missing) == 0 || parentBase != child.existing[len(child.existing)-1].identity {
		return false, nil
	}
	for i := 0; i < min(len(parent.missing), len(child.missing)); i++ {
		a, b := parent.missing[i], child.missing[i]
		if a == b {
			continue
		}
		// No file identity exists to disambiguate case or Unicode normalization.
		// Win32 also normalizes trailing dots/spaces and special characters.
		// Otherwise-distinct ordinary ASCII components are safely distinct;
		// ambiguous names need an existing ancestor before authorizing a write.
		if strings.EqualFold(a, b) || !channelPlainMissingName(a) || !channelPlainMissingName(b) {
			return false, ReasonChannelReceiptInvalid
		}
		return false, nil
	}
	return len(parent.missing) <= len(child.missing), nil
}

func channelPlainMissingName(name string) bool {
	if strings.TrimRight(name, ". ") != name || strings.ContainsAny(name, `<>:"|?*~`) {
		return false
	}
	for _, r := range name {
		if r < 32 || r > 126 {
			return false
		}
	}
	return true
}

func channelPathsOverlap(first, second string) (bool, error) {
	within, err := channelPathWithin(first, second)
	if err != nil || within {
		return within, err
	}
	return channelPathWithin(second, first)
}
