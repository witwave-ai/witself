package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const (
	genericProviderTransactionSchema    = "witself.generic-provider-transaction.v1"
	genericProviderTransactionInstall   = "install"
	genericProviderTransactionUninstall = "uninstall"
)

type genericProviderTransactionJournal struct {
	SchemaVersion   string                             `json:"schema_version"`
	ID              string                             `json:"id"`
	Operation       string                             `json:"operation"`
	Runtime         string                             `json:"runtime"`
	Previous        *transcriptcapture.Config          `json:"previous,omitempty"`
	PreviousBinding *transcriptcapture.Config          `json:"previous_binding,omitempty"`
	Staged          transcriptcapture.Config           `json:"staged"`
	Desired         transcriptcapture.Config           `json:"desired"`
	ProviderBefore  genericProviderTransactionSnapshot `json:"provider_before"`
}

type genericProviderTransactionSnapshot struct {
	Path      string                                   `json:"path"`
	Existed   bool                                     `json:"existed"`
	Mode      uint32                                   `json:"mode,omitempty"`
	Raw       []byte                                   `json:"raw,omitempty"`
	NonTarget string                                   `json:"non_target"`
	Target    *genericProviderTransactionBindingRecord `json:"target,omitempty"`
}

type genericProviderTransactionBindingRecord struct {
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Environment map[string]string `json:"environment,omitempty"`
}

type genericProviderTransactionConfigState uint8

const (
	genericProviderTransactionConfigPrevious genericProviderTransactionConfigState = iota + 1
	genericProviderTransactionConfigDesired
)

func genericProviderTransactionPath(runtimeName string) (string, error) {
	if !isGenericProviderRuntime(runtimeName) {
		return "", fmt.Errorf("runtime %q has no generic provider transaction journal", runtimeName)
	}
	home, err := local.Home()
	if err != nil {
		return "", err
	}
	home, err = cleanCopilotAbsolutePath("WITSELF_HOME transaction root", home)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "integrations", runtimeName, ".provider-transaction.json"), nil
}

func beginGenericProviderInstallTransaction(
	previous, previousBinding *transcriptcapture.Config,
	staged, desired transcriptcapture.Config,
	providerBefore genericMCPConfigSnapshot,
) (genericProviderTransactionJournal, error) {
	return beginGenericProviderTransaction(
		genericProviderTransactionInstall,
		previous,
		previousBinding,
		staged,
		desired,
		providerBefore,
	)
}

func beginGenericProviderUninstallTransaction(
	previous, previousBinding transcriptcapture.Config,
	providerBefore genericMCPConfigSnapshot,
) (genericProviderTransactionJournal, error) {
	return beginGenericProviderTransaction(
		genericProviderTransactionUninstall,
		&previous,
		&previousBinding,
		previousBinding,
		previousBinding,
		providerBefore,
	)
}

func beginGenericProviderTransaction(
	operation string,
	previous, previousBinding *transcriptcapture.Config,
	staged, desired transcriptcapture.Config,
	providerBefore genericMCPConfigSnapshot,
) (genericProviderTransactionJournal, error) {
	if !isGenericProviderRuntime(desired.Runtime) || staged.Runtime != desired.Runtime {
		return genericProviderTransactionJournal{}, errors.New("generic provider transaction has an invalid runtime")
	}
	staged.SchemaVersion = transcriptcapture.SchemaVersion
	desired.SchemaVersion = transcriptcapture.SchemaVersion
	if previous != nil {
		previousCopy := *previous
		previous = &previousCopy
	}
	if previousBinding != nil {
		previousBindingCopy := *previousBinding
		previousBindingCopy.SchemaVersion = transcriptcapture.SchemaVersion
		previousBinding = &previousBindingCopy
	}
	if err := validateGenericProviderTransactionConfigs(operation, previous, previousBinding, staged, desired); err != nil {
		return genericProviderTransactionJournal{}, err
	}
	if err := genericSnapshotStillCurrent(desired, providerBefore); err != nil {
		return genericProviderTransactionJournal{}, fmt.Errorf("provider configuration changed before transaction journal creation: %w", err)
	}
	record, err := genericProviderTransactionSnapshotFromLive(providerBefore)
	if err != nil {
		return genericProviderTransactionJournal{}, err
	}
	identifier := make([]byte, 16)
	if _, err := rand.Read(identifier); err != nil {
		return genericProviderTransactionJournal{}, fmt.Errorf("create generic provider transaction id: %w", err)
	}
	journal := genericProviderTransactionJournal{
		SchemaVersion:   genericProviderTransactionSchema,
		ID:              hex.EncodeToString(identifier),
		Operation:       operation,
		Runtime:         desired.Runtime,
		Previous:        previous,
		PreviousBinding: previousBinding,
		Staged:          staged,
		Desired:         desired,
		ProviderBefore:  record,
	}
	if err := writeGenericProviderTransactionJournal(journal); err != nil {
		return genericProviderTransactionJournal{}, err
	}
	return journal, nil
}

func genericProviderTransactionSnapshotFromLive(snapshot genericMCPConfigSnapshot) (genericProviderTransactionSnapshot, error) {
	result := genericProviderTransactionSnapshot{
		Path:      snapshot.path,
		Existed:   snapshot.existed,
		Mode:      uint32(snapshot.mode.Perm()),
		Raw:       bytes.Clone(snapshot.raw),
		NonTarget: snapshot.nonTarget,
	}
	if snapshot.targetSet {
		result.Target = genericProviderTransactionBindingFromLive(snapshot.target)
	}
	return result, nil
}

func genericProviderTransactionBindingFromLive(binding genericMCPBinding) *genericProviderTransactionBindingRecord {
	return &genericProviderTransactionBindingRecord{
		Command:     binding.Command,
		Args:        append([]string(nil), binding.Args...),
		Environment: cloneStringMap(binding.Environment),
	}
}

func (record *genericProviderTransactionBindingRecord) live() (genericMCPBinding, bool) {
	if record == nil {
		return genericMCPBinding{}, false
	}
	return genericMCPBinding{
		Command:     record.Command,
		Args:        append([]string(nil), record.Args...),
		Environment: cloneStringMap(record.Environment),
	}, true
}

func writeGenericProviderTransactionJournal(journal genericProviderTransactionJournal) error {
	path, err := genericProviderTransactionPath(journal.Runtime)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("an interrupted generic provider transaction requires recovery")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ensureGenericProviderTransactionDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".provider-transaction-")
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
	return syncGenericProviderTransactionDirectory(filepath.Dir(path))
}

func ensureGenericProviderTransactionDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	for current := directory; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("generic provider transaction directory %s must be a real directory", current)
		}
		if filepath.Base(current) == "integrations" {
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			return errors.New("generic provider transaction path is outside the Witself integrations directory")
		}
	}
	return nil
}

func loadGenericProviderTransactionJournal(runtimeName string) (genericProviderTransactionJournal, error) {
	journal, _, err := loadGenericProviderTransactionJournalFile(runtimeName)
	return journal, err
}

func loadGenericProviderTransactionJournalFile(runtimeName string) (genericProviderTransactionJournal, integrationTransactionJournalFile, error) {
	path, err := genericProviderTransactionPath(runtimeName)
	if err != nil {
		return genericProviderTransactionJournal{}, integrationTransactionJournalFile{}, err
	}
	fileSnapshot, err := loadIntegrationTransactionJournalFile(path, "generic provider transaction journal")
	if err != nil {
		return genericProviderTransactionJournal{}, fileSnapshot, err
	}
	var journal genericProviderTransactionJournal
	if err := rejectDuplicateJSONKeys(fileSnapshot.raw); err != nil {
		return genericProviderTransactionJournal{}, fileSnapshot, fmt.Errorf("parse generic provider transaction journal: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(fileSnapshot.raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return genericProviderTransactionJournal{}, fileSnapshot, fmt.Errorf("parse generic provider transaction journal: %w", err)
	}
	if err := validateGenericProviderTransactionJournal(runtimeName, journal); err != nil {
		return genericProviderTransactionJournal{}, fileSnapshot, err
	}
	return journal, fileSnapshot, nil
}

func validateGenericProviderTransactionJournal(runtimeName string, journal genericProviderTransactionJournal) error {
	if journal.SchemaVersion != genericProviderTransactionSchema ||
		(journal.Operation != genericProviderTransactionInstall && journal.Operation != genericProviderTransactionUninstall) ||
		journal.Runtime != runtimeName {
		return errors.New("unsupported generic provider transaction journal")
	}
	if len(journal.ID) != 32 {
		return errors.New("invalid generic provider transaction id")
	}
	if _, err := hex.DecodeString(journal.ID); err != nil {
		return errors.New("invalid generic provider transaction id")
	}
	if err := validateGenericProviderTransactionConfigs(journal.Operation, journal.Previous, journal.PreviousBinding, journal.Staged, journal.Desired); err != nil {
		return err
	}
	if journal.Operation == genericProviderTransactionUninstall {
		if journal.Previous == nil || journal.PreviousBinding == nil ||
			!equalGenericProviderTransactionConfig(journal.Staged, *journal.PreviousBinding) ||
			!equalGenericProviderTransactionConfig(journal.Desired, *journal.PreviousBinding) {
			return errors.New("generic provider uninstall transaction has an invalid prior binding")
		}
	}
	return validateGenericProviderTransactionSnapshot(journal.Desired, journal.ProviderBefore, journal.PreviousBinding)
}

func validateGenericProviderTransactionConfigs(
	operation string,
	previous, previousBinding *transcriptcapture.Config,
	staged, desired transcriptcapture.Config,
) error {
	if !isGenericProviderRuntime(desired.Runtime) || staged.Runtime != desired.Runtime ||
		desired.SchemaVersion != transcriptcapture.SchemaVersion || staged.SchemaVersion != transcriptcapture.SchemaVersion {
		return errors.New("generic provider transaction binding has an invalid runtime or schema")
	}
	if err := validateGenericProviderCurrentRoots(desired); err != nil {
		return err
	}
	legacyUninstall := operation == genericProviderTransactionUninstall && previous != nil &&
		!genericProviderConfigIsPinned(*previous) && previousBinding != nil && len(desired.MCPEnvironment) == 0
	if desired.RuntimeCLICommand == "" || desired.MCPCommand == "" ||
		desired.RuntimeConfigRoot == "" || desired.RuntimeMCPConfigPath == "" ||
		(desired.MCPEnvironment["WITSELF_HOME"] == "" && !legacyUninstall) {
		return errors.New("generic provider transaction desired binding is incomplete")
	}
	if staged.RuntimeCLICommand != desired.RuntimeCLICommand || staged.MCPCommand != desired.MCPCommand ||
		staged.RuntimeConfigRoot != desired.RuntimeConfigRoot || staged.RuntimeMCPConfigPath != desired.RuntimeMCPConfigPath ||
		staged.MCPEnvironment["WITSELF_HOME"] != desired.MCPEnvironment["WITSELF_HOME"] {
		return errors.New("generic provider staged and desired bindings do not share exact provider ownership")
	}
	desiredBinding, err := genericMCPBindingFromConfig(desired)
	if err != nil {
		return err
	}
	stagedBinding, err := genericMCPBindingFromConfig(staged)
	if err != nil {
		return err
	}
	if !equalGenericMCPBinding(stagedBinding, desiredBinding) {
		return errors.New("generic provider staged and desired MCP bindings differ")
	}
	if previous == nil {
		if previousBinding != nil {
			return errors.New("generic provider transaction has a prior MCP binding without a prior integration config")
		}
		return nil
	}
	if previous.Runtime != desired.Runtime || previousBinding == nil || previousBinding.Runtime != desired.Runtime {
		return errors.New("generic provider transaction prior bindings have an invalid runtime")
	}
	if genericProviderConfigIsPinned(*previous) {
		if err := validateGenericProviderPreviousSelection(desired, *previousBinding); err != nil {
			return err
		}
	} else {
		// Legacy records predate pinned provider fields. The transient hydrated
		// binding intentionally retains an empty MCP environment because that is
		// the exact pre-v201 registration being migrated. Require every selector
		// reconstructed from the current operation to match the desired binding,
		// but do not mistake the intentional empty legacy environment for
		// WITSELF_HOME drift.
		if previousBinding.RuntimeCLICommand != desired.RuntimeCLICommand ||
			previousBinding.MCPCommand != desired.MCPCommand ||
			previousBinding.RuntimeConfigRoot != desired.RuntimeConfigRoot ||
			previousBinding.RuntimeMCPConfigPath != desired.RuntimeMCPConfigPath ||
			len(previousBinding.MCPEnvironment) != 0 {
			return errors.New("generic provider transaction legacy binding reconstruction does not match the desired provider selection")
		}
	}
	_, err = genericMCPBindingFromConfig(*previousBinding)
	return err
}

func validateGenericProviderTransactionSnapshot(
	cfg transcriptcapture.Config,
	record genericProviderTransactionSnapshot,
	previousBinding *transcriptcapture.Config,
) error {
	if record.Path != cfg.RuntimeMCPConfigPath || len(record.Raw) > genericProviderConfigReadLimit {
		return errors.New("generic provider transaction preimage has an invalid path or size")
	}
	if record.Mode&^uint32(0o777) != 0 {
		return errors.New("generic provider transaction preimage has an invalid file mode")
	}
	if !record.Existed && (len(record.Raw) != 0 || record.Mode != 0 || record.Target != nil) {
		return errors.New("generic provider transaction missing-file preimage contains file state")
	}
	root := map[string]any{}
	if record.Existed {
		var err error
		root, err = parseGenericMCPConfigBytes(cfg.Runtime, record.Path, record.Raw)
		if err != nil {
			return err
		}
	}
	nonTarget, err := canonicalGenericMCPNonTarget(cfg.Runtime, root)
	if err != nil || nonTarget != record.NonTarget {
		return errors.New("generic provider transaction preimage non-target fence is invalid")
	}
	target, targetSet, err := genericMCPBindingFromRoot(cfg, root)
	if err != nil {
		return err
	}
	recordedTarget, recordedTargetSet := record.Target.live()
	if targetSet != recordedTargetSet || (targetSet && !equalGenericMCPBinding(target, recordedTarget)) {
		return errors.New("generic provider transaction preimage target fence is invalid")
	}
	if previousBinding == nil {
		if targetSet {
			return errors.New("generic provider first-install transaction preimage contains an owned target")
		}
		return nil
	}
	expected, err := genericMCPBindingFromConfig(*previousBinding)
	if err != nil {
		return err
	}
	if !targetSet || !equalGenericMCPBinding(target, expected) {
		return errors.New("generic provider reinstall transaction preimage does not match the prior exact binding")
	}
	return nil
}

func clearGenericProviderTransaction(expected genericProviderTransactionJournal) error {
	current, fileSnapshot, err := loadGenericProviderTransactionJournalFile(expected.Runtime)
	if err != nil {
		return err
	}
	currentRaw, currentErr := json.Marshal(current)
	expectedRaw, expectedErr := json.Marshal(expected)
	if currentErr != nil || expectedErr != nil || !bytes.Equal(currentRaw, expectedRaw) {
		return errors.New("generic provider transaction journal changed; refusing to clear it")
	}
	if err := syncGenericProviderTransactionState(current); err != nil {
		return fmt.Errorf("durably fence generic provider transaction state: %w", err)
	}
	if err := removeIntegrationTransactionJournalFile(fileSnapshot); err != nil {
		return err
	}
	path := fileSnapshot.path
	return syncGenericProviderTransactionDirectory(filepath.Dir(path))
}

func syncGenericProviderTransactionState(journal genericProviderTransactionJournal) error {
	configPath, err := transcriptcapture.ConfigPath(journal.Runtime)
	if err != nil {
		return err
	}
	states := []struct {
		path  string
		label string
	}{
		{journal.Desired.RuntimeMCPConfigPath, "generic provider MCP config"},
		{configPath, "generic provider integration config"},
	}
	for _, state := range states {
		if err := syncIntegrationTransactionFileState(state.path, state.label); err != nil {
			return err
		}
	}
	return nil
}

func syncGenericProviderTransactionDirectory(path string) error {
	return syncIntegrationTransactionDirectory(path)
}

func recoverGenericProviderTransaction(runtimeName string) error {
	journal, err := loadGenericProviderTransactionJournal(runtimeName)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	clearExpected, err := cloneGenericProviderTransactionJournal(journal)
	if err != nil {
		return err
	}
	if journal.Operation == genericProviderTransactionUninstall {
		if err := recoverGenericProviderUninstallTransaction(journal); err != nil {
			return err
		}
		return clearGenericProviderTransaction(clearExpected)
	}
	state, current, err := currentGenericProviderTransactionConfigState(journal)
	if err != nil {
		return err
	}
	switch state {
	case genericProviderTransactionConfigPrevious:
		if err := restoreGenericProviderTransactionPreimage(journal); err != nil {
			return err
		}
	case genericProviderTransactionConfigDesired:
		desired := journal.Desired
		if current != nil && equalGenericProviderTransactionConfigWithCursorManaged(*current, desired) {
			desired.ManagedPermissions = append([]string(nil), current.ManagedPermissions...)
		}
		if err := recoverGenericProviderDesiredState(journal, &desired); err != nil {
			return err
		}
	default:
		return errors.New("generic provider transaction has an unknown integration config state")
	}
	return clearGenericProviderTransaction(clearExpected)
}

func cloneGenericProviderTransactionJournal(journal genericProviderTransactionJournal) (genericProviderTransactionJournal, error) {
	raw, err := json.Marshal(journal)
	if err != nil {
		return genericProviderTransactionJournal{}, err
	}
	var cloned genericProviderTransactionJournal
	if err := json.Unmarshal(raw, &cloned); err != nil {
		return genericProviderTransactionJournal{}, err
	}
	return cloned, nil
}

func recoverGenericProviderUninstallTransaction(journal genericProviderTransactionJournal) error {
	current, err := transcriptcapture.LoadConfig(journal.Runtime)
	if err == nil {
		if journal.Previous == nil || !equalGenericProviderTransactionConfig(current, *journal.Previous) {
			return errors.New("integration config changed during interrupted generic provider uninstall")
		}
		return restoreGenericProviderUninstallState(journal)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read generic provider uninstall integration config: %w", err)
	}
	return convergeGenericProviderUninstallState(journal)
}

func restoreGenericProviderUninstallState(journal genericProviderTransactionJournal) error {
	if journal.PreviousBinding == nil {
		return errors.New("generic provider uninstall has no prior binding to restore")
	}
	cfg := *journal.PreviousBinding
	if _, err := installRuntimeMemoryRoutingInstructionsAt(cfg.Runtime, cfg.RuntimeWorkspace); err != nil {
		return err
	}
	if cfg.Runtime == transcriptcapture.RuntimeCursor && cursorConfigManagesWitselfMCPPermission(cfg.ManagedPermissions) {
		snapshot, err := snapshotCursorCLIConfig()
		if err != nil {
			return err
		}
		if _, err := snapshot.ensureWitselfMCPPermission(); err != nil {
			return err
		}
	}
	if err := restoreGenericProviderTransactionPreimage(journal); err != nil {
		return err
	}
	if err := restoreRuntimeHooksOwned(nil, &cfg); err != nil {
		return err
	}
	if err := validateGenericInstalledTopology(cfg); err != nil {
		return err
	}
	return verifyRuntimeHooksOwned(cfg)
}

func convergeGenericProviderUninstallState(journal genericProviderTransactionJournal) error {
	if journal.PreviousBinding == nil {
		return errors.New("generic provider uninstall has no prior binding to remove")
	}
	cfg := *journal.PreviousBinding
	if _, err := removeRuntimeMemoryRoutingInstructionsAt(cfg.Runtime, cfg.RuntimeWorkspace); err != nil {
		return err
	}
	if _, err := removeRuntimeHooksOwned(cfg); err != nil {
		return err
	}
	if err := convergeGenericProviderUninstallMCP(journal); err != nil {
		return err
	}
	if cfg.Runtime == transcriptcapture.RuntimeCursor && cursorConfigManagesWitselfMCPPermission(cfg.ManagedPermissions) {
		snapshot, err := snapshotCursorCLIConfig()
		if err != nil {
			return err
		}
		if _, err := snapshot.removeWitselfMCPPermission(); err != nil {
			return err
		}
	}
	if _, err := transcriptcapture.LoadConfig(journal.Runtime); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("generic provider integration config reappeared during uninstall recovery")
		}
		return err
	}
	return nil
}

func convergeGenericProviderUninstallMCP(journal genericProviderTransactionJournal) error {
	cfg := journal.Desired
	currentBinding, currentSet, current, err := inspectGenericMCP(cfg)
	if err != nil {
		return err
	}
	if current.nonTarget != journal.ProviderBefore.NonTarget {
		return errors.New("provider non-target configuration changed during interrupted uninstall; refusing recovery")
	}
	beforeBinding, beforeSet := journal.ProviderBefore.Target.live()
	if !beforeSet {
		return errors.New("generic provider uninstall journal has no exact-owned MCP preimage")
	}
	if currentSet && !equalGenericMCPBinding(currentBinding, beforeBinding) {
		return errors.New("provider MCP server witself changed to a foreign binding during interrupted uninstall; refusing recovery")
	}
	if currentSet {
		if err := genericSnapshotStillCurrent(cfg, current); err != nil {
			return fmt.Errorf("provider configuration changed before uninstall recovery: %w", err)
		}
		if err := removeGenericMCPUnchecked(cfg.RuntimeCLICommand, cfg); err != nil {
			return fmt.Errorf("recover generic provider MCP removal: %w", err)
		}
	}
	_, exists, after, err := inspectGenericMCP(cfg)
	if err != nil {
		return err
	}
	if exists || after.nonTarget != journal.ProviderBefore.NonTarget {
		return errors.New("generic provider uninstall recovery did not preserve exact non-target state")
	}
	if journal.Runtime == transcriptcapture.RuntimeGrokBuild {
		if err := convergeGrokEffectiveMCP(
			cfg.RuntimeCLICommand,
			cfg,
			genericMCPBinding{},
			false,
			journal.ProviderBefore.NonTarget,
		); err != nil {
			return fmt.Errorf("remove effective Grok MCP state during uninstall recovery: %w", err)
		}
	}
	return nil
}

func currentGenericProviderTransactionConfigState(
	journal genericProviderTransactionJournal,
) (genericProviderTransactionConfigState, *transcriptcapture.Config, error) {
	current, err := transcriptcapture.LoadConfig(journal.Runtime)
	if errors.Is(err, os.ErrNotExist) {
		if journal.Previous == nil {
			return genericProviderTransactionConfigPrevious, nil, nil
		}
		return 0, nil, errors.New("prior integration config disappeared during generic provider transaction")
	}
	if err != nil {
		return 0, nil, fmt.Errorf("read generic provider transaction integration config: %w", err)
	}
	if equalGenericProviderTransactionConfig(current, journal.Staged) ||
		equalGenericProviderTransactionConfig(current, journal.Desired) ||
		equalGenericProviderTransactionConfigWithCursorManaged(current, journal.Desired) {
		return genericProviderTransactionConfigDesired, &current, nil
	}
	if journal.Previous != nil && equalGenericProviderTransactionConfig(current, *journal.Previous) {
		return genericProviderTransactionConfigPrevious, &current, nil
	}
	return 0, &current, errors.New("integration config changed during interrupted generic provider transaction")
}

func equalGenericProviderTransactionConfig(left, right transcriptcapture.Config) bool {
	leftRaw, leftErr := json.Marshal(left)
	rightRaw, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftRaw, rightRaw)
}

func equalGenericProviderTransactionConfigWithCursorManaged(left, right transcriptcapture.Config) bool {
	if right.Runtime != transcriptcapture.RuntimeCursor {
		return false
	}
	right.ManagedPermissions = addManagedCursorMCPPermission(append([]string(nil), right.ManagedPermissions...))
	return equalGenericProviderTransactionConfig(left, right)
}

func restoreGenericProviderTransactionPreimage(journal genericProviderTransactionJournal) error {
	currentBinding, currentSet, current, err := inspectGenericMCP(journal.Desired)
	if err != nil {
		return err
	}
	if current.nonTarget != journal.ProviderBefore.NonTarget {
		return errors.New("provider non-target configuration changed during interrupted transaction; refusing recovery")
	}
	beforeBinding, beforeSet := journal.ProviderBefore.Target.live()
	desiredBinding, err := genericMCPBindingFromConfig(journal.Desired)
	if err != nil {
		return err
	}
	if currentSet && !equalGenericMCPBinding(currentBinding, desiredBinding) &&
		(!beforeSet || !equalGenericMCPBinding(currentBinding, beforeBinding)) {
		return errors.New("provider MCP server witself changed to a foreign binding during interrupted transaction; refusing recovery")
	}
	if journal.Runtime == transcriptcapture.RuntimeGrokBuild {
		if err := convergeGrokEffectiveMCP(
			journal.Desired.RuntimeCLICommand,
			journal.Desired,
			beforeBinding,
			beforeSet,
			journal.ProviderBefore.NonTarget,
		); err != nil {
			return fmt.Errorf("restore effective Grok transaction preimage: %w", err)
		}
		currentBinding, currentSet, current, err = inspectGenericMCP(journal.Desired)
		if err != nil {
			return err
		}
		if current.nonTarget != journal.ProviderBefore.NonTarget || currentSet != beforeSet ||
			(currentSet && !equalGenericMCPBinding(currentBinding, beforeBinding)) {
			return errors.New("effective Grok transaction recovery did not restore its exact provider preimage")
		}
	}
	if current.existed == journal.ProviderBefore.Existed && current.mode.Perm() == os.FileMode(journal.ProviderBefore.Mode).Perm() &&
		bytes.Equal(current.raw, journal.ProviderBefore.Raw) {
		return nil
	}
	if err := genericSnapshotStillCurrent(journal.Desired, current); err != nil {
		return fmt.Errorf("provider configuration changed immediately before transaction rollback: %w", err)
	}
	if journal.ProviderBefore.Existed {
		if err := writeFileAtomic(journal.ProviderBefore.Path, journal.ProviderBefore.Raw, os.FileMode(journal.ProviderBefore.Mode)); err != nil {
			return fmt.Errorf("restore exact generic provider preimage: %w", err)
		}
	} else if err := os.Remove(journal.ProviderBefore.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("restore missing generic provider preimage: %w", err)
	}
	if journal.Runtime == transcriptcapture.RuntimeCursor {
		verb := "disable"
		if beforeSet {
			verb = "enable"
		}
		_, _ = runGenericProviderCLI(journal.Desired.RuntimeCLICommand, journal.Desired, 15*time.Second, "mcp", verb, "witself")
	}
	_, _, restored, err := inspectGenericMCP(journal.Desired)
	if err != nil {
		return err
	}
	if restored.existed != journal.ProviderBefore.Existed || restored.mode.Perm() != os.FileMode(journal.ProviderBefore.Mode).Perm() ||
		!bytes.Equal(restored.raw, journal.ProviderBefore.Raw) {
		return errors.New("generic provider preimage did not restore exactly")
	}
	return nil
}

func recoverGenericProviderDesiredState(journal genericProviderTransactionJournal, desired *transcriptcapture.Config) error {
	if _, err := installRuntimeMemoryRoutingInstructionsAt(journal.Runtime, desired.RuntimeWorkspace); err != nil {
		return err
	}
	if journal.Runtime == transcriptcapture.RuntimeCursor {
		permissionSnapshot, err := snapshotCursorCLIConfig()
		if err != nil {
			return err
		}
		touched, err := permissionSnapshot.ensureWitselfMCPPermission()
		if err != nil {
			return err
		}
		if touched {
			desired.ManagedPermissions = addManagedCursorMCPPermission(desired.ManagedPermissions)
		}
	}
	if err := convergeGenericProviderTransactionMCP(journal); err != nil {
		return err
	}
	previousHooks := journal.PreviousBinding
	if previousHooks == nil {
		previousHooks = journal.Previous
	}
	if err := recoverRuntimeHooksOwned(desired, previousHooks); err != nil {
		return err
	}
	if err := transcriptcapture.SaveConfig(*desired); err != nil {
		return err
	}
	if err := validateGenericInstalledTopology(*desired); err != nil {
		return err
	}
	return verifyRuntimeHooksOwned(*desired)
}

func convergeGenericProviderTransactionMCP(journal genericProviderTransactionJournal) error {
	desiredBinding, err := genericMCPBindingFromConfig(journal.Desired)
	if err != nil {
		return err
	}
	currentBinding, currentSet, current, err := inspectGenericMCP(journal.Desired)
	if err != nil {
		return err
	}
	if current.nonTarget != journal.ProviderBefore.NonTarget {
		return errors.New("provider non-target configuration changed during interrupted transaction; refusing recovery")
	}
	if currentSet && equalGenericMCPBinding(currentBinding, desiredBinding) {
		if journal.Runtime == transcriptcapture.RuntimeGrokBuild {
			if err := convergeGrokEffectiveMCP(
				journal.Desired.RuntimeCLICommand,
				journal.Desired,
				desiredBinding,
				true,
				current.nonTarget,
			); err != nil {
				return err
			}
		}
		return validateGenericInstalledTopology(journal.Desired)
	}
	beforeBinding, beforeSet := journal.ProviderBefore.Target.live()
	if currentSet && (!beforeSet || !equalGenericMCPBinding(currentBinding, beforeBinding)) {
		return errors.New("provider MCP server witself changed to a foreign binding during interrupted transaction; refusing recovery")
	}
	if currentSet && journal.PreviousBinding != nil {
		_, err := registerGenericMCPWithMutation(journal.Desired.RuntimeCLICommand, journal.Desired, journal.PreviousBinding)
		return err
	}
	if err := genericSnapshotStillCurrent(journal.Desired, current); err != nil {
		return fmt.Errorf("provider configuration changed immediately before transaction recovery add: %w", err)
	}
	if err := addGenericMCPUnchecked(journal.Desired.RuntimeCLICommand, journal.Desired, desiredBinding); err != nil {
		return fmt.Errorf("recover generic provider MCP registration: %w", err)
	}
	return validateGenericInstalledTopology(journal.Desired)
}
