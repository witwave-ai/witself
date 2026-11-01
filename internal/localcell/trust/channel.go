package trust

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Channel refusal reasons expose no local paths or installing identities.
const (
	ReasonChannelReceiptInvalid Reason = "channel_receipt_invalid"
	ReasonChannelRevision       Reason = "channel_revision_conflict"
	ReasonChannelActionRequired Reason = "channel_action_required"
	ReasonChannelNotImplemented Reason = "channel_not_implemented"
	ReasonOwnershipOverlap      Reason = "ownership_overlap"
)

// ComponentSetIdentity binds a channel declaration to its known source manifest.
// These private receipt values must not be copied into ordinary observations.
type ComponentSetIdentity struct {
	ManifestDigest string `json:"manifest_digest"`
	Channel        string `json:"channel"`
	Sequence       uint64 `json:"sequence"`
	Tag            string `json:"tag"`
}

// ChannelReceipt is a caller declaration, never a discovery result. Revision is
// one for the first write and exactly the previous committed revision plus one
// for a replacement. An absent receipt is represented by unmanaged revision zero.
type ChannelReceipt struct {
	Channel            string                `json:"channel"`
	OwnedPaths         []string              `json:"owned_paths"`
	InstallingIdentity string                `json:"installing_identity"`
	DeclaredAt         time.Time             `json:"declared_at"`
	Revision           uint64                `json:"revision"`
	ComponentSet       *ComponentSetIdentity `json:"component_set,omitempty"`
}

// ChannelLayout explicitly identifies lifecycle-owned directories. All three
// paths must be supplied; adapters never infer ownership from directory names.
type ChannelLayout struct {
	DataRoot    string
	PGDATA      string
	PayloadRoot string
}

// OwnedPathObservation identifies a declaration by index, without revealing it.
type OwnedPathObservation struct {
	Index  int    `json:"index"`
	Status string `json:"status"` // present, absent or foreign
}

// ChannelObservation contains value-free observations, never package success.
type ChannelObservation struct {
	ReceiptPresent bool                   `json:"receipt_present"`
	Paths          []OwnedPathObservation `json:"paths"`
	Reason         Reason                 `json:"reason,omitempty"`
}

// ChannelCandidate declares every path a proposed update would write or remove.
// A caller must include parent directory replacement/removal in this declaration.
type ChannelCandidate struct {
	WritePaths []string
}

// PreflightDecision is advisory. B3 never executes a package manager or writes
// an owned package path, including when a package action is required.
type PreflightDecision struct {
	Allowed bool   `json:"allowed"`
	Reason  Reason `json:"reason"`
}

// ChannelAdapter observes explicit declarations and checks proposed writes only.
type ChannelAdapter interface {
	Inspect(context.Context, ChannelReceipt) (ChannelObservation, error)
	PreflightUpdate(context.Context, ChannelReceipt, ChannelCandidate) (PreflightDecision, error)
}

// WriteChannelReceipt atomically persists one bounded caller declaration under a
// private root. Concurrent/interrupted writer fences are never taken over.
func WriteChannelReceipt(ctx context.Context, root string, receipt ChannelReceipt) error {
	if err := validateChannelReceipt(receipt, false); err != nil {
		return err
	}
	for _, name := range receipt.OwnedPaths {
		if _, err := channelPathStatus(name, receipt.InstallingIdentity); err != nil {
			return err
		}
		if err := channelNoSymlinks(name); err != nil {
			return err
		}
	}
	if !channelCleanAbsolute(root) {
		return ReasonChannelReceiptInvalid
	}
	if err := privateDirectory(root, true); err != nil {
		return err
	}
	dir := filepath.Join(root, "channel")
	if err := privateDirectory(dir, true); err != nil {
		return err
	}
	return withPrivateTransaction(ctx, dir, ".channel-receipt.lock", func() error {
		previous, err := ReadChannelReceipt(root)
		if err != nil {
			return err
		}
		if previous.Revision == ^uint64(0) || receipt.Revision != previous.Revision+1 {
			return ReasonChannelRevision
		}
		// Recheck after taking the fence: a caller cannot commit a declaration
		// whose ancestor changed into a symlink while it waited to transact.
		for _, name := range receipt.OwnedPaths {
			if err := channelNoSymlinks(name); err != nil {
				return err
			}
		}
		return writePrivateJSON(dir, "receipt.json", receipt)
	})
}

// ReadChannelReceipt revalidates private storage on every read. Absence means
// unmanaged; malformed or non-private storage never becomes an absent receipt.
func ReadChannelReceipt(root string) (ChannelReceipt, error) {
	if !channelCleanAbsolute(root) {
		return ChannelReceipt{}, ReasonChannelReceiptInvalid
	}
	if err := privateDirectory(root, false); err != nil {
		return ChannelReceipt{}, err
	}
	dir := filepath.Join(root, "channel")
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return ChannelReceipt{Channel: "unmanaged"}, nil
	}
	if err := privateDirectory(dir, false); err != nil {
		return ChannelReceipt{}, err
	}
	var receipt ChannelReceipt
	if err := readPrivateJSON(dir, "receipt.json", &receipt); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ChannelReceipt{Channel: "unmanaged"}, nil
		}
		return ChannelReceipt{}, err
	}
	if err := validateChannelReceipt(receipt, false); err != nil {
		return ChannelReceipt{}, err
	}
	return receipt, nil
}

// NewChannelAdapter selects a named observe/preflight adapter. Package adapters
// without B3 implementations return channel_not_implemented and do no work.
func NewChannelAdapter(channel string, layout ChannelLayout) ChannelAdapter {
	return declaredChannelAdapter{channel: channel, layout: layout}
}

type declaredChannelAdapter struct {
	channel string
	layout  ChannelLayout
}

func (a declaredChannelAdapter) Inspect(ctx context.Context, receipt ChannelReceipt) (ChannelObservation, error) {
	if err := ctx.Err(); err != nil {
		return ChannelObservation{}, err
	}
	if a.channel != "shell" && a.channel != "homebrew" && a.channel != "unmanaged" {
		return ChannelObservation{Reason: ReasonChannelNotImplemented}, ReasonChannelNotImplemented
	}
	if receipt.Channel != a.channel || validateChannelReceipt(receipt, true) != nil || !a.validLayout() {
		return ChannelObservation{Reason: ReasonChannelReceiptInvalid}, ReasonChannelReceiptInvalid
	}
	observation := ChannelObservation{ReceiptPresent: receipt.Revision != 0, Paths: make([]OwnedPathObservation, 0, len(receipt.OwnedPaths))}
	for i, name := range receipt.OwnedPaths {
		if err := ctx.Err(); err != nil {
			return ChannelObservation{}, err
		}
		status, err := channelPathStatus(name, receipt.InstallingIdentity)
		if err != nil {
			return ChannelObservation{}, err
		}
		observation.Paths = append(observation.Paths, OwnedPathObservation{Index: i, Status: status})
		// A link remains an observable foreign declaration, never evidence of
		// ownership. Preflight independently refuses these unsafe declarations.
		if status == "foreign" && channelNoSymlinks(name) != nil {
			continue
		}
		for _, root := range a.lifecycleRoots() {
			overlap, err := channelPathsOverlap(root, name)
			if err != nil {
				return observation, err
			}
			if overlap {
				observation.Reason = ReasonOwnershipOverlap
			}
		}
	}
	if observation.Reason != "" {
		return observation, observation.Reason
	}
	return observation, nil
}

func (a declaredChannelAdapter) PreflightUpdate(ctx context.Context, receipt ChannelReceipt, candidate ChannelCandidate) (PreflightDecision, error) {
	refuse := func(reason Reason) (PreflightDecision, error) { return PreflightDecision{Reason: reason}, nil }
	if _, err := a.Inspect(ctx, receipt); err != nil {
		var reason Reason
		if errors.As(err, &reason) {
			return refuse(reason)
		}
		return PreflightDecision{}, err
	}
	if a.channel == "unmanaged" {
		return refuse(ReasonChannelActionRequired)
	}
	if len(candidate.WritePaths) == 0 || len(candidate.WritePaths) > 64 {
		return refuse(ReasonChannelReceiptInvalid)
	}
	for _, target := range candidate.WritePaths {
		if !channelCleanAbsolute(target) {
			return refuse(ReasonChannelReceiptInvalid)
		}
		if err := channelNoSymlinks(target); err != nil {
			return refuse(ReasonChannelActionRequired)
		}
		for _, owned := range receipt.OwnedPaths {
			overlap, err := channelPathsOverlap(owned, target)
			if err != nil || overlap {
				return refuse(ReasonChannelActionRequired)
			}
		}
		lifecycleOwned := false
		for _, root := range a.lifecycleRoots() {
			within, err := channelPathWithin(root, target)
			if err != nil {
				return refuse(ReasonChannelActionRequired)
			}
			lifecycleOwned = lifecycleOwned || within
		}
		if !lifecycleOwned {
			return refuse(ReasonChannelActionRequired)
		}
	}
	return PreflightDecision{Allowed: true, Reason: "allowed"}, nil
}

func (a declaredChannelAdapter) lifecycleRoots() []string {
	return []string{a.layout.DataRoot, a.layout.PGDATA, a.layout.PayloadRoot}
}

func (a declaredChannelAdapter) validLayout() bool {
	for _, name := range a.lifecycleRoots() {
		if !channelCleanAbsolute(name) || channelNoSymlinks(name) != nil {
			return false
		}
	}
	return true
}

func validateChannelReceipt(receipt ChannelReceipt, allowMissing bool) error {
	if allowMissing && receipt.Channel == "unmanaged" && receipt.Revision == 0 && len(receipt.OwnedPaths) == 0 && receipt.InstallingIdentity == "" && receipt.DeclaredAt.IsZero() && receipt.ComponentSet == nil {
		return nil
	}
	switch receipt.Channel {
	case "shell", "homebrew", "linux_package", "msi", "unmanaged":
	default:
		return ReasonChannelReceiptInvalid
	}
	if receipt.Revision == 0 || receipt.DeclaredAt.IsZero() || receipt.DeclaredAt.Year() < 1 || receipt.DeclaredAt.Year() > 9999 || !validChannelIdentity(receipt.InstallingIdentity) || len(receipt.OwnedPaths) > 64 {
		return ReasonChannelReceiptInvalid
	}
	seen := make(map[string]bool, len(receipt.OwnedPaths))
	for _, name := range receipt.OwnedPaths {
		if !channelCleanAbsolute(name) || seen[name] {
			return ReasonChannelReceiptInvalid
		}
		seen[name] = true
	}
	if identity := receipt.ComponentSet; identity != nil {
		digest, err := hex.DecodeString(identity.ManifestDigest)
		if err != nil || len(digest) != 32 || identity.ManifestDigest != strings.ToLower(identity.ManifestDigest) || identity.Channel == "" || len(identity.Channel) > 64 || identity.Sequence == 0 || identity.Tag == "" || len(identity.Tag) > 128 {
			return ReasonChannelReceiptInvalid
		}
	}
	return nil
}

func channelCleanAbsolute(name string) bool {
	if name == "" || len(name) > 1024 || strings.ContainsRune(name, 0) || !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return false
	}
	for _, segment := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		if segment == ".." {
			return false
		}
	}
	return true
}

// channelNoSymlinks permits missing suffixes, but never follows a known link.
func channelNoSymlinks(name string) error {
	for current := name; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return ReasonStateIO
		}
		if err == nil && (!inventoryInfoSafe(info) || (current != name && !info.IsDir())) {
			return ReasonChannelReceiptInvalid
		}
		if current == filepath.Dir(current) {
			return nil
		}
	}
}

func channelPathStatus(name, identity string) (string, error) {
	if err := channelNoSymlinks(name); err != nil {
		if errors.Is(err, ReasonChannelReceiptInvalid) {
			return "foreign", nil
		}
		return "", err
	}
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return "absent", nil
	}
	if err != nil {
		return "", ReasonStateIO
	}
	if !inventoryInfoSafe(info) || (!info.Mode().IsRegular() && !info.IsDir()) || !channelIdentityMatches(name, info, identity) {
		return "foreign", nil
	}
	return "present", nil
}

// checkPayloadOwnership refuses any explicit package declaration that overlaps
// the harness's private lifecycle layout. Receipt absence cannot claim package
// ownership, and does not prevent staging in this separately declared root.
func checkPayloadOwnership(root string) error {
	receipt, err := ReadChannelReceipt(root)
	if err != nil {
		return err
	}
	for _, owned := range receipt.OwnedPaths {
		for _, lifecycle := range []string{root, filepath.Join(root, "pg"), filepath.Join(root, "payload")} {
			overlap, err := channelPathsOverlap(lifecycle, owned)
			if err != nil {
				return err
			}
			if overlap {
				return ReasonOwnershipOverlap
			}
		}
	}
	return nil
}
