package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const (
	dshTransactionSchema    = "witself.dsh-transaction.v1"
	dshTransactionInstall   = "install"
	dshTransactionUninstall = "uninstall"
)

type dshTransactionJournal struct {
	SchemaVersion    string                    `json:"schema_version"`
	ID               string                    `json:"id"`
	Operation        string                    `json:"operation"`
	Previous         *transcriptcapture.Config `json:"previous,omitempty"`
	Desired          *transcriptcapture.Config `json:"desired,omitempty"`
	ProviderBefore   dshTransactionSnapshot    `json:"provider_before"`
	providerFileInfo os.FileInfo
}

type dshTransactionSnapshot struct {
	Path         string `json:"path"`
	Exists       bool   `json:"exists"`
	Mode         uint32 `json:"mode,omitempty"`
	Shape        string `json:"shape"`
	SHA256       string `json:"sha256,omitempty"`
	BlockPresent bool   `json:"block_present"`
	BlockSHA256  string `json:"block_sha256,omitempty"`
	// ForeignSHA256 digests every line outside the managed fence so recovery
	// can prove it never disturbed an operator's own patch rows.
	ForeignSHA256 string `json:"foreign_sha256"`
}

func dshTransactionPath(configRoot string) string {
	return filepath.Join(configRoot, ".witself-dsh-transaction.json")
}

func beginDSHTransaction(operation string, previous, desired *transcriptcapture.Config) (dshTransactionJournal, error) {
	binding := desired
	if binding == nil {
		binding = previous
	}
	if binding == nil {
		return dshTransactionJournal{}, errors.New("the DeepSeek Harness transaction has no binding")
	}
	for _, candidate := range []*transcriptcapture.Config{previous, desired} {
		if candidate == nil {
			continue
		}
		if err := validateDSHTransactionConfig(*candidate); err != nil {
			return dshTransactionJournal{}, err
		}
	}
	switch operation {
	case dshTransactionInstall:
		if desired == nil {
			return dshTransactionJournal{}, errors.New("the DeepSeek Harness install transaction has no desired binding")
		}
	case dshTransactionUninstall:
		if previous == nil || desired != nil {
			return dshTransactionJournal{}, errors.New("the DeepSeek Harness uninstall transaction must contain only the installed binding")
		}
	default:
		return dshTransactionJournal{}, errors.New("unknown DeepSeek Harness transaction operation")
	}
	if previous != nil && desired != nil {
		if previous.RuntimeConfigRoot != desired.RuntimeConfigRoot ||
			previous.RuntimeMCPConfigPath != desired.RuntimeMCPConfigPath ||
			previous.RuntimeCLICommand != desired.RuntimeCLICommand ||
			!equalDSHEnvironment(previous.MCPEnvironment, desired.MCPEnvironment) {
			return dshTransactionJournal{}, errors.New("the DeepSeek Harness transaction cannot change its CLI, config root, patch file path, or selector environment")
		}
	}

	before, err := readDSHPatchSnapshot(binding.RuntimeMCPConfigPath)
	if err != nil {
		return dshTransactionJournal{}, fmt.Errorf("snapshot DeepSeek Harness patch file: %w", err)
	}
	identifier := make([]byte, 16)
	if _, err := rand.Read(identifier); err != nil {
		return dshTransactionJournal{}, fmt.Errorf("create DeepSeek Harness transaction id: %w", err)
	}
	journal := dshTransactionJournal{
		SchemaVersion:    dshTransactionSchema,
		ID:               hex.EncodeToString(identifier),
		Operation:        operation,
		Previous:         cloneDSHTransactionConfig(previous),
		Desired:          cloneDSHTransactionConfig(desired),
		ProviderBefore:   dshTransactionSnapshotFromLive(before),
		providerFileInfo: before.fileInfo,
	}
	if err := writeDSHTransactionJournal(binding.RuntimeConfigRoot, journal); err != nil {
		return dshTransactionJournal{}, err
	}
	return journal, nil
}

func cloneDSHTransactionConfig(cfg *transcriptcapture.Config) *transcriptcapture.Config {
	if cfg == nil {
		return nil
	}
	configCopy := *cfg
	configCopy.MCPEnvironment = cloneCopilotEnvironment(cfg.MCPEnvironment)
	configCopy.ManagedPermissions = append([]string(nil), cfg.ManagedPermissions...)
	return &configCopy
}

func validateDSHTransactionConfig(cfg transcriptcapture.Config) error {
	if cfg.Runtime != transcriptcapture.RuntimeDSH {
		return errors.New("the DeepSeek Harness transaction contains a non-dsh integration")
	}
	if err := validateDSHCLISelection(cfg.RuntimeCLICommand, cfg); err != nil {
		return err
	}
	_, err := dshManagedPatchBlock(cfg)
	return err
}

func dshTransactionSnapshotFromLive(snapshot dshPatchSnapshot) dshTransactionSnapshot {
	record := dshTransactionSnapshot{
		Path: snapshot.path, Exists: snapshot.exists, Mode: uint32(snapshot.mode.Perm()),
		Shape: snapshot.shape, BlockPresent: snapshot.blockPresent(),
		ForeignSHA256: dshForeignDigest(snapshot),
	}
	if snapshot.exists {
		digest := sha256.Sum256(snapshot.raw)
		record.SHA256 = hex.EncodeToString(digest[:])
	}
	if snapshot.blockPresent() {
		digest := sha256.Sum256(snapshot.block())
		record.BlockSHA256 = hex.EncodeToString(digest[:])
	}
	return record
}

// dshForeignDigest fingerprints the patch file with the managed fence removed,
// normalizing away the `[]` placeholder the installer itself adds or drops.
func dshForeignDigest(snapshot dshPatchSnapshot) string {
	remaining := make([]string, 0, len(snapshot.lines))
	for index, line := range snapshot.lines {
		if snapshot.blockPresent() && index >= snapshot.blockBegin && index <= snapshot.blockEnd {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed == "[]" {
			continue
		}
		remaining = append(remaining, line)
	}
	digest := sha256.Sum256([]byte(strings.Join(remaining, "\n")))
	return hex.EncodeToString(digest[:])
}

func writeDSHTransactionJournal(configRoot string, journal dshTransactionJournal) error {
	path := dshTransactionPath(configRoot)
	if _, err := os.Lstat(path); err == nil {
		return errors.New("an interrupted DeepSeek Harness transaction requires recovery")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ensureDSHTransactionRoot(configRoot); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	temporary, err := os.CreateTemp(configRoot, ".witself-dsh-transaction-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := renameManagedInstructionFileNoReplace(temporaryPath, path); err != nil {
		return err
	}
	return syncDSHTransactionRoot(configRoot)
}

func ensureDSHTransactionRoot(configRoot string) error {
	if err := os.MkdirAll(configRoot, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(configRoot)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("the DeepSeek Harness transaction root must be a real directory")
	}
	return nil
}

func loadDSHTransactionJournal(configRoot string) (dshTransactionJournal, error) {
	journal, _, err := loadDSHTransactionJournalFile(configRoot)
	return journal, err
}

func loadDSHTransactionJournalFile(configRoot string) (dshTransactionJournal, integrationTransactionJournalFile, error) {
	path := dshTransactionPath(configRoot)
	fileSnapshot, err := loadIntegrationTransactionJournalFile(path, "DeepSeek Harness transaction journal")
	if err != nil {
		return dshTransactionJournal{}, fileSnapshot, err
	}
	if err := rejectDuplicateJSONKeys(fileSnapshot.raw); err != nil {
		return dshTransactionJournal{}, fileSnapshot, fmt.Errorf("parse DeepSeek Harness transaction journal: %w", err)
	}
	var journal dshTransactionJournal
	decoder := json.NewDecoder(bytes.NewReader(fileSnapshot.raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return dshTransactionJournal{}, fileSnapshot, fmt.Errorf("parse DeepSeek Harness transaction journal: %w", err)
	}
	if journal.SchemaVersion != dshTransactionSchema || len(journal.ID) != 32 {
		return dshTransactionJournal{}, fileSnapshot, errors.New("unsupported DeepSeek Harness transaction journal")
	}
	if _, err := hex.DecodeString(journal.ID); err != nil {
		return dshTransactionJournal{}, fileSnapshot, errors.New("invalid DeepSeek Harness transaction id")
	}
	if err := validateDSHTransactionJournal(configRoot, journal); err != nil {
		return dshTransactionJournal{}, fileSnapshot, err
	}
	return journal, fileSnapshot, nil
}

func validateDSHTransactionJournal(configRoot string, journal dshTransactionJournal) error {
	switch journal.Operation {
	case dshTransactionInstall:
		if journal.Desired == nil {
			return errors.New("the DeepSeek Harness install transaction is missing its desired binding")
		}
	case dshTransactionUninstall:
		if journal.Previous == nil || journal.Desired != nil {
			return errors.New("the DeepSeek Harness uninstall transaction has invalid bindings")
		}
	default:
		return errors.New("unknown DeepSeek Harness transaction operation")
	}
	for _, candidate := range []*transcriptcapture.Config{journal.Previous, journal.Desired} {
		if candidate == nil {
			continue
		}
		if candidate.RuntimeConfigRoot != configRoot {
			return errors.New("the DeepSeek Harness transaction journal does not match its config root")
		}
		if err := validateDSHTransactionConfig(*candidate); err != nil {
			return err
		}
	}
	if journal.Previous != nil && journal.Desired != nil {
		if journal.Previous.RuntimeConfigRoot != journal.Desired.RuntimeConfigRoot ||
			journal.Previous.RuntimeMCPConfigPath != journal.Desired.RuntimeMCPConfigPath ||
			journal.Previous.RuntimeCLICommand != journal.Desired.RuntimeCLICommand ||
			!equalDSHEnvironment(journal.Previous.MCPEnvironment, journal.Desired.MCPEnvironment) {
			return errors.New("the DeepSeek Harness transaction journal changes its CLI, config root, patch file path, or selector environment")
		}
	}
	return nil
}

func clearDSHTransaction(configRoot string, expected dshTransactionJournal) error {
	current, fileSnapshot, err := loadDSHTransactionJournalFile(configRoot)
	if err != nil {
		return err
	}
	currentRaw, currentErr := json.Marshal(current)
	expectedRaw, expectedErr := json.Marshal(expected)
	if currentErr != nil || expectedErr != nil || !bytes.Equal(currentRaw, expectedRaw) {
		return errors.New("the DeepSeek Harness transaction journal changed; refusing to clear it")
	}
	if err := syncDSHCommittedState(current); err != nil {
		return fmt.Errorf("durably commit DeepSeek Harness transaction state: %w", err)
	}
	if err := removeIntegrationTransactionJournalFile(fileSnapshot); err != nil {
		return err
	}
	return syncDSHTransactionRoot(configRoot)
}

func syncDSHCommittedState(journal dshTransactionJournal) error {
	binding := journal.Desired
	if binding == nil {
		binding = journal.Previous
	}
	if binding == nil {
		return errors.New("the DeepSeek Harness transaction has no binding to commit")
	}
	configPath, err := transcriptcapture.ConfigPath(transcriptcapture.RuntimeDSH)
	if err != nil {
		return err
	}
	routing, err := dshManagedInstructionsSpecAt(binding.RuntimeConfigRoot)
	if err != nil {
		return err
	}
	// AGENTS.md is shared and may legitimately be a symlink into an
	// operator's dotfiles; the routing block was written through that link,
	// so durability is proven on its target.
	routingPath := routing.path
	if resolved, err := filepath.EvalSymlinks(routingPath); err == nil {
		routingPath = resolved
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	paths := []struct {
		path  string
		label string
	}{
		{binding.RuntimeMCPConfigPath, "DeepSeek Harness patch file"},
		{configPath, "DeepSeek Harness integration config"},
		{routingPath, "DeepSeek Harness routing policy"},
	}
	for _, state := range paths {
		if err := syncIntegrationTransactionFileState(state.path, state.label); err != nil {
			return err
		}
	}
	return nil
}

func syncDSHTransactionRoot(path string) error {
	return syncIntegrationTransactionDirectory(path)
}

func validateDSHTransactionProviderBefore(journal dshTransactionJournal) error {
	binding := journal.Desired
	if binding == nil {
		binding = journal.Previous
	}
	if binding == nil {
		return errors.New("the DeepSeek Harness transaction has no provider binding")
	}
	snapshot, err := readDSHPatchSnapshot(binding.RuntimeMCPConfigPath)
	if err != nil {
		return err
	}
	if !equalDSHTransactionSnapshot(dshTransactionSnapshotFromLive(snapshot), journal.ProviderBefore) {
		return errors.New("the DeepSeek Harness patch file changed after transaction journaling; refusing to mutate it")
	}
	if journal.ProviderBefore.Exists && journal.providerFileInfo != nil &&
		(snapshot.fileInfo == nil || !os.SameFile(journal.providerFileInfo, snapshot.fileInfo)) {
		return errors.New("the DeepSeek Harness patch file identity changed after transaction journaling; refusing to mutate it")
	}
	return nil
}

func equalDSHTransactionSnapshot(left, right dshTransactionSnapshot) bool {
	return left.Path == right.Path && left.Exists == right.Exists &&
		left.Mode == right.Mode && left.Shape == right.Shape &&
		left.SHA256 == right.SHA256 && left.BlockPresent == right.BlockPresent &&
		left.BlockSHA256 == right.BlockSHA256 && left.ForeignSHA256 == right.ForeignSHA256
}

func validateDSHTransactionNonTarget(journal dshTransactionJournal, snapshot dshPatchSnapshot) error {
	if snapshot.path != journal.ProviderBefore.Path ||
		dshForeignDigest(snapshot) != journal.ProviderBefore.ForeignSHA256 {
		return errors.New("foreign DeepSeek Harness patch rows changed during the interrupted transaction; refusing recovery")
	}
	return nil
}

func recoverDSHTransaction(configRoot string) error {
	journal, err := loadDSHTransactionJournal(configRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	switch journal.Operation {
	case dshTransactionInstall:
		if err := recoverDSHInstallTransaction(journal); err != nil {
			return err
		}
	case dshTransactionUninstall:
		if err := recoverDSHUninstallTransaction(journal); err != nil {
			return err
		}
	default:
		return errors.New("unknown DeepSeek Harness transaction operation")
	}
	return clearDSHTransaction(configRoot, journal)
}

func recoverDSHInstallTransaction(journal dshTransactionJournal) error {
	desired := *journal.Desired
	currentConfig, configErr := transcriptcapture.LoadConfig(transcriptcapture.RuntimeDSH)
	if configErr != nil && !errors.Is(configErr, os.ErrNotExist) {
		return configErr
	}
	if configErr == nil &&
		!equalDSHTransactionConfig(currentConfig, desired) &&
		(journal.Previous == nil || !equalDSHTransactionConfig(currentConfig, *journal.Previous)) {
		return errors.New("the DeepSeek Harness integration config changed during the interrupted install transaction")
	}
	if errors.Is(configErr, os.ErrNotExist) && journal.Previous != nil {
		return errors.New("prior DeepSeek Harness integration config disappeared during the interrupted install transaction")
	}
	routing, err := dshManagedInstructionsSpecAt(desired.RuntimeConfigRoot)
	if err != nil {
		return err
	}
	if _, err := installManagedInstructions(routing); err != nil {
		return fmt.Errorf("install DeepSeek Harness memory routing instructions: %w", err)
	}
	if err := convergeDSHTransactionDesiredPatch(journal); err != nil {
		return err
	}
	if err := transcriptcapture.SaveConfig(desired); err != nil {
		return err
	}
	// Recovery has no operator on the terminal; an unavailable probe must not
	// leave the transaction journal pending forever.
	_, err = validateDSHCommitTopology(desired)
	return err
}

func convergeDSHTransactionDesiredPatch(journal dshTransactionJournal) error {
	desired := *journal.Desired
	desiredBlock, err := dshManagedPatchBlock(desired)
	if err != nil {
		return err
	}
	snapshot, err := readDSHPatchSnapshot(desired.RuntimeMCPConfigPath)
	if err != nil {
		return err
	}
	if err := validateDSHTransactionNonTarget(journal, snapshot); err != nil {
		return err
	}
	if snapshot.blockPresent() && !bytes.Equal(snapshot.block(), desiredBlock) {
		// The only other block a crash can leave behind is the prior binding's.
		if journal.Previous == nil {
			return errors.New("the DeepSeek Harness managed block changed to a foreign binding during interrupted recovery")
		}
		previousBlock, err := dshManagedPatchBlock(*journal.Previous)
		if err != nil {
			return err
		}
		if !bytes.Equal(snapshot.block(), previousBlock) {
			return errors.New("the DeepSeek Harness managed block changed to a foreign binding during interrupted recovery")
		}
	}
	claim := journal.Previous
	if snapshot.blockPresent() && bytes.Equal(snapshot.block(), desiredBlock) {
		// The interrupted install already wrote exactly this block; claim it
		// as our own so a first install is recoverable after the patch write.
		claim = &desired
	}
	plan, _, err := prepareDSHPatchInstallPlan(desired.RuntimeCLICommand, desired, claim)
	if err != nil {
		return err
	}
	touched, err := installDSHPatchBlockWithPlan(plan)
	if err != nil {
		return fmt.Errorf("recover desired DeepSeek Harness patch block (touched=%t): %w", touched, err)
	}
	after, err := readDSHPatchSnapshot(desired.RuntimeMCPConfigPath)
	if err != nil {
		return err
	}
	return validateDSHTransactionNonTarget(journal, after)
}

func recoverDSHUninstallTransaction(journal dshTransactionJournal) error {
	previous := *journal.Previous
	currentConfig, configErr := transcriptcapture.LoadConfig(transcriptcapture.RuntimeDSH)
	if configErr != nil && !errors.Is(configErr, os.ErrNotExist) {
		return configErr
	}
	if configErr == nil && !equalDSHTransactionConfig(currentConfig, previous) {
		return errors.New("the DeepSeek Harness integration config changed during the interrupted uninstall transaction")
	}
	snapshot, err := readDSHPatchSnapshot(previous.RuntimeMCPConfigPath)
	if err != nil {
		return err
	}
	if err := validateDSHTransactionNonTarget(journal, snapshot); err != nil {
		return err
	}
	if snapshot.blockPresent() {
		expected, err := dshManagedPatchBlock(previous)
		if err != nil {
			return err
		}
		if !bytes.Equal(snapshot.block(), expected) {
			return errors.New("the installed DeepSeek Harness managed block changed during interrupted uninstall recovery")
		}
		touched, removeErr := removeDSHPatchBlockWithSnapshot(previous, &snapshot)
		if removeErr != nil {
			return fmt.Errorf("recover DeepSeek Harness patch removal (touched=%t): %w", touched, removeErr)
		}
	}
	routing, err := dshManagedInstructionsSpecAt(previous.RuntimeConfigRoot)
	if err != nil {
		return err
	}
	if _, err := removeManagedInstructions(routing); err != nil {
		return fmt.Errorf("remove DeepSeek Harness memory routing instructions: %w", err)
	}
	if configErr == nil {
		if err := transcriptcapture.RemoveConfig(transcriptcapture.RuntimeDSH); err != nil {
			return err
		}
	}
	after, err := readDSHPatchSnapshot(previous.RuntimeMCPConfigPath)
	if err != nil {
		return err
	}
	if after.blockPresent() {
		return errors.New("the DeepSeek Harness managed block reappeared during interrupted uninstall recovery")
	}
	return validateDSHTransactionNonTarget(journal, after)
}

// equalDSHTransactionConfig compares the binding, not save-time bookkeeping:
// SaveConfig stamps schema_version and installed_at, so a journaled desired
// binding never marshals identically to the config it produced.
func equalDSHTransactionConfig(left, right transcriptcapture.Config) bool {
	left.SchemaVersion, right.SchemaVersion = "", ""
	left.InstalledAt, right.InstalledAt = time.Time{}, time.Time{}
	leftRaw, leftErr := json.Marshal(left)
	rightRaw, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftRaw, rightRaw)
}
