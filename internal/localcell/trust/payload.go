package trust

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"time"
)

// Payload limits are checked against authenticated inventories before extraction.
const (
	DefaultPayloadBudget    uint64 = 2 << 30
	MaxPayloadGenerations          = 4
	ReasonPayloadRetention  Reason = "payload_retention_exceeded"
	ReasonPayloadBudget     Reason = "payload_budget_exceeded"
	ReasonGenerationExists  Reason = "generation_exists"
	ReasonGenerationInvalid Reason = "generation_invalid"
)

// VerifiedSet carries the exact signed bytes previously committed with AcceptSet.
// No exported parsed structure is treated as authentication evidence.
type VerifiedSet struct {
	Document []byte
	Envelope []byte
}

// GenerationComponent binds an authenticated component to its private subtree.
type GenerationComponent struct {
	Directory string    `json:"directory"`
	Component Component `json:"component"`
}

// Generation is a private local receipt, not a signed export or portable authority.
// Document and Envelope retain exact bytes, encoded as base64 in the JSON record.
// Trust retains the AcceptSet receipt needed for the explicit offline handoff.
type Generation struct {
	Version         int                   `json:"version"`
	Name            string                `json:"name"`
	ManifestDigest  string                `json:"manifest_digest"`
	Channel         string                `json:"channel"`
	Sequence        uint64                `json:"sequence"`
	Tag             string                `json:"tag"`
	OS              string                `json:"os"`
	Arch            string                `json:"arch"`
	Document        []byte                `json:"document"`
	Envelope        []byte                `json:"envelope"`
	Roots           []PublicKey           `json:"roots"`
	Trust           StateSnapshot         `json:"trust"`
	Components      []GenerationComponent `json:"components"`
	Host            string                `json:"host"`
	Args            []string              `json:"args"`
	InventoryDigest string                `json:"inventory_digest"`
	InventoryBytes  uint64                `json:"inventory_bytes"`
	StagedAt        time.Time             `json:"staged_at"`
}

func generationMetadata(set ComponentSet, components []Component, digest, selectedOS, selectedArch string) (Generation, error) {
	g := Generation{Version: 1, Name: fmt.Sprintf("%s-%d-%s", set.Channel, set.ReleaseSequence, digest[:16]), ManifestDigest: digest, Channel: set.Channel, Sequence: set.ReleaseSequence, Tag: set.ReleaseTag, OS: selectedOS, Arch: selectedArch, Args: []string{}}
	ordered := append([]Component(nil), components...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Role < ordered[j].Role })
	for _, component := range ordered {
		_, size, err := inventoryMap(component)
		if err != nil {
			return Generation{}, err
		}
		if size > DefaultPayloadBudget || g.InventoryBytes > DefaultPayloadBudget-size {
			return Generation{}, ReasonPayloadBudget
		}
		g.InventoryBytes += size
		g.Components = append(g.Components, GenerationComponent{Directory: component.Role, Component: component})
		if component.Role == "service_host" {
			for _, entry := range component.Inventory {
				if entry.Executable {
					if g.Host != "" {
						return Generation{}, ReasonGenerationInvalid
					}
					g.Host = filepath.ToSlash(filepath.Join(component.Role, filepath.FromSlash(entry.Path)))
				}
			}
		}
	}
	if g.Host == "" {
		return Generation{}, ReasonGenerationInvalid
	}
	encoded, err := json.Marshal(g.Components)
	if err != nil {
		return Generation{}, ReasonGenerationInvalid
	}
	g.InventoryDigest = digestBytes(encoded)
	return g, nil
}

func generationVerifier(g Generation, clock Clock) *Verifier {
	return NewVerifier(Options{Roots: g.Roots, State: &MemoryState{state: cloneState(g.Trust)}, Clock: clock, ExpectedTag: g.Tag, OS: g.OS, Arch: g.Arch})
}

func validateGeneration(g Generation) error {
	if g.Version != 1 || g.StagedAt.IsZero() || len(g.Args) != 0 || len(g.Document) > MaxComponentSetBytes || len(g.Envelope) > MaxEnvelopeBytes {
		return ReasonGenerationInvalid
	}
	set, err := ParseComponentSet(g.Document)
	if err != nil {
		return err
	}
	v := generationVerifier(g, Clock{})
	if out := v.AcceptForOfflineStart(g.Document, g.Envelope); !out.Accepted {
		return out.Reason
	}
	var components []Component
	for _, platform := range set.Platforms {
		if platform.OS == g.OS && platform.Arch == g.Arch {
			components = platform.Components
			break
		}
	}
	expected, err := generationMetadata(set, components, digestBytes(g.Document), g.OS, g.Arch)
	if err != nil {
		return err
	}
	// Reconstruct provenance stamps from exact committed bytes; JSON intentionally
	// cannot serialize them. Every other persisted declaration must match exactly.
	actualComponents, _ := json.Marshal(g.Components)
	expectedComponents, _ := json.Marshal(expected.Components)
	if string(actualComponents) != string(expectedComponents) || g.Name != expected.Name || g.ManifestDigest != expected.ManifestDigest || g.Channel != expected.Channel || g.Sequence != expected.Sequence || g.Tag != expected.Tag || g.Host != expected.Host || g.InventoryDigest != expected.InventoryDigest || g.InventoryBytes != expected.InventoryBytes {
		return ReasonGenerationInvalid
	}
	return nil
}

// StageGeneration extracts one exact AcceptSet-committed platform into a fresh
// private generation. The default aggregate inventory budget is two GiB and the
// retention cap is four generations. Existing generations are never overwritten.
func StageGeneration(ctx context.Context, verifier *Verifier, state TrustState, set VerifiedSet, components []Component, archives map[string]string, root string) (Generation, error) {
	return stageGeneration(ctx, verifier, state, set, components, archives, root, DefaultPayloadBudget)
}

func stageGeneration(ctx context.Context, verifier *Verifier, state TrustState, set VerifiedSet, components []Component, archives map[string]string, root string, budget uint64) (Generation, error) {
	if verifier == nil || state == nil {
		return Generation{}, ReasonNotInstalled
	}
	snapshot, err := state.Load()
	if err != nil {
		return Generation{}, ReasonStateIO
	}
	options := verifier.options
	options.State = &MemoryState{state: cloneState(snapshot)}
	v := NewVerifier(options)
	if out := v.AcceptForOfflineStart(set.Document, set.Envelope); !out.Accepted {
		return Generation{}, out.Reason
	}
	if out := v.VerifyComponentSet(set.Document, set.Envelope); !out.Accepted {
		return Generation{}, out.Reason
	}
	parsed, err := ParseComponentSet(set.Document)
	if err != nil {
		return Generation{}, err
	}
	selectedOS, selectedArch, ok := v.componentsMatch(parsed, components, digestBytes(set.Document))
	if !ok {
		return Generation{}, ReasonMixedSet
	}
	installed := snapshot.Installed[digestBytes(set.Document)]
	if installed.OS != selectedOS || installed.Arch != selectedArch {
		return Generation{}, ReasonMixedSet
	}
	g, err := generationMetadata(parsed, components, digestBytes(set.Document), selectedOS, selectedArch)
	if err != nil {
		return Generation{}, err
	}
	g.Document, g.Envelope = append([]byte(nil), set.Document...), append([]byte(nil), set.Envelope...)
	g.Roots, g.Trust, g.StagedAt = append([]PublicKey(nil), options.Roots...), snapshot, options.Clock.Now.UTC()
	if g.StagedAt.IsZero() {
		return Generation{}, ReasonUnknownClock
	}
	// An explicitly supplied root still cannot be PGDATA or its descendant.
	// Observe only the PostgreSQL marker's presence; never read database files.
	if err := payloadLocation(root); err != nil {
		return Generation{}, err
	}
	if err := privateDirectory(root, true); err != nil {
		return Generation{}, err
	}
	if err := checkPayloadOwnership(root); err != nil {
		return Generation{}, err
	}
	if err := persistGenerationTrust(root, g); err != nil {
		return Generation{}, err
	}
	payloadDir := filepath.Join(root, "payload")
	if err := privateDirectory(payloadDir, true); err != nil {
		return Generation{}, err
	}
	err = withPrivateTransaction(ctx, payloadDir, ".handoff.lock", func() error {
		journal, err := loadHandoff(root)
		if err != nil {
			return err
		}
		if err := reconcileHandoff(ctx, root, &journal, options.Clock); err != nil {
			return err
		}
		if _, exists := journal.Generations[g.Name]; exists {
			return ReasonGenerationExists
		}
		if err := generationCapacity(root, g, budget); err != nil {
			return err
		}
		return extractGeneration(ctx, root, g, archives)
	})
	if err != nil {
		return Generation{}, err
	}
	return g, nil
}

func payloadLocation(root string) error {
	if privatePathComponents(root) != nil {
		return ReasonStateIO
	}
	for current := root; ; current = filepath.Dir(current) {
		_, err := os.Lstat(filepath.Join(current, "PG_VERSION"))
		if err == nil {
			return ReasonOwnershipOverlap
		}
		if !errors.Is(err, os.ErrNotExist) {
			return ReasonStateIO
		}
		if filepath.Dir(current) == current {
			return nil
		}
	}
}

func generationCapacity(root string, next Generation, budget uint64) error {
	payloadDir := filepath.Join(root, "payload")
	if _, err := os.Lstat(filepath.Join(payloadDir, next.Name)); !errors.Is(err, os.ErrNotExist) {
		return ReasonGenerationExists
	}
	entries, err := os.ReadDir(payloadDir)
	if err != nil {
		return ReasonStateIO
	}
	count, total := 0, next.InventoryBytes
	if total > budget {
		return ReasonPayloadBudget
	}
	for _, entry := range entries {
		if entry.Name() == ".handoff.lock" || entry.Name() == "handoff.json" {
			continue
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return ReasonGenerationInvalid
		}
		count++
		if count >= MaxPayloadGenerations {
			return ReasonPayloadRetention
		}
		g, err := LoadGeneration(root, entry.Name())
		if err != nil {
			return err
		}
		if g.InventoryBytes > budget-total {
			return ReasonPayloadBudget
		}
		total += g.InventoryBytes
	}
	return nil
}

func extractGeneration(ctx context.Context, root string, g Generation, archives map[string]string) (result error) {
	dir := filepath.Join(root, "payload", g.Name)
	if err := os.Mkdir(dir, 0700); err != nil {
		return ReasonGenerationExists
	}
	cleanup := true
	defer func() {
		if result != nil && cleanup {
			_ = os.Chmod(dir, 0700)
			_ = os.RemoveAll(dir)
		}
	}()
	for _, component := range g.Components {
		archive, ok := archives[component.Component.Role]
		if !ok {
			return ExtractMissing
		}
		componentDir := filepath.Join(dir, component.Directory)
		if err := ExtractVerified(ctx, archive, component.Component, componentDir); err != nil {
			return err
		}
		if err := RecheckInventory(componentDir, component.Component); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ExtractCancelled
	}
	if err := writePrivateJSON(dir, "generation.json", g); err != nil {
		return err
	}
	// Preserve signed inventory modes. Only the generation container becomes
	// read-only; chmod does not promise resistance to the same installing user.
	if err := os.Chmod(dir, 0500); err != nil && runtime.GOOS != "windows" {
		return ReasonStateIO
	}
	if err := RecheckGeneration(root, g); err != nil {
		return err
	}
	if err := stageHandoff(root, g.Name); err != nil {
		// A post-rename directory-sync error can leave a complete committed
		// receipt. Never remove the generation named by that visible receipt,
		// nor guess after a failed reload whether publication took place.
		journal, readErr := loadHandoff(root)
		_, recorded := journal.Generations[g.Name]
		if readErr != nil || recorded {
			cleanup = false
		}
		return err
	}
	return nil
}

// LoadGeneration reads and revalidates private generation metadata. It checks
// every derived declaration against the exact document's committed receipt.
func LoadGeneration(root, name string) (Generation, error) {
	if !ValidPath(name) || filepath.Base(name) != name {
		return Generation{}, ReasonGenerationInvalid
	}
	if err := privateDirectory(root, false); err != nil {
		return Generation{}, err
	}
	if err := privateDirectory(filepath.Join(root, "payload"), false); err != nil {
		return Generation{}, err
	}
	dir := filepath.Join(root, "payload", name)
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || !inventoryInfoSafe(info) || (runtime.GOOS != "windows" && info.Mode().Perm() != 0500) {
		return Generation{}, ReasonGenerationInvalid
	}
	var g Generation
	if err := readPrivateJSON(dir, "generation.json", &g); err != nil {
		return Generation{}, err
	}
	if g.Name != name {
		return Generation{}, ReasonGenerationInvalid
	}
	if err := validateGeneration(g); err != nil {
		return Generation{}, err
	}
	// Restore provenance stamps for subsequent verifier calls.
	parsed, _ := ParseComponentSet(g.Document)
	for _, platform := range parsed.Platforms {
		if platform.OS == g.OS && platform.Arch == g.Arch {
			for i := range g.Components {
				for _, component := range platform.Components {
					if component.Role == g.Components[i].Component.Role {
						g.Components[i].Component = component
					}
				}
			}
		}
	}
	return g, nil
}

// RecheckGeneration checks every selected component plus the generation's full
// top-level inventory immediately before handing an executable to a caller.
func RecheckGeneration(root string, g Generation) error {
	loaded, err := LoadGeneration(root, g.Name)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(loaded, g) {
		return ReasonGenerationInvalid
	}
	dir := filepath.Join(root, "payload", g.Name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ReasonStateIO
	}
	expected := map[string]bool{"generation.json": true}
	for _, component := range g.Components {
		expected[component.Directory] = true
	}
	if len(entries) != len(expected) {
		return ExtractUndeclared
	}
	for _, entry := range entries {
		if !expected[entry.Name()] {
			return ExtractUndeclared
		}
	}
	for _, component := range g.Components {
		if err := RecheckInventory(filepath.Join(dir, component.Directory), component.Component); err != nil {
			return err
		}
	}
	return nil
}
