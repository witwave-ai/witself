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

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const (
	copilotTransactionSchema    = "witself.copilot-transaction.v1"
	copilotTransactionInstall   = "install"
	copilotTransactionUninstall = "uninstall"
)

type copilotTransactionJournal struct {
	SchemaVersion    string                     `json:"schema_version"`
	ID               string                     `json:"id"`
	Operation        string                     `json:"operation"`
	Previous         *transcriptcapture.Config  `json:"previous,omitempty"`
	Desired          *transcriptcapture.Config  `json:"desired,omitempty"`
	ProviderBefore   copilotTransactionSnapshot `json:"provider_before"`
	providerFileInfo os.FileInfo
}

type copilotTransactionSnapshot struct {
	Path         string            `json:"path"`
	Exists       bool              `json:"exists"`
	Mode         uint32            `json:"mode,omitempty"`
	SHA256       string            `json:"sha256,omitempty"`
	RootFields   map[string]string `json:"root_fields"`
	Siblings     map[string]string `json:"siblings"`
	TargetName   string            `json:"target_name"`
	PreviousName string            `json:"previous_name,omitempty"`
}

func copilotTransactionPath(configRoot string) string {
	return filepath.Join(configRoot, ".witself-copilot-transaction.json")
}

func beginCopilotTransaction(operation string, previous, desired *transcriptcapture.Config) (copilotTransactionJournal, error) {
	binding := desired
	if binding == nil {
		binding = previous
	}
	if binding == nil {
		return copilotTransactionJournal{}, errors.New("the Copilot transaction has no binding")
	}
	if err := validateCopilotTransactionConfig(*binding); err != nil {
		return copilotTransactionJournal{}, err
	}
	if previous != nil {
		if err := validateCopilotTransactionConfig(*previous); err != nil {
			return copilotTransactionJournal{}, err
		}
	}
	if desired != nil {
		if err := validateCopilotTransactionConfig(*desired); err != nil {
			return copilotTransactionJournal{}, err
		}
	}
	switch operation {
	case copilotTransactionInstall:
		if desired == nil {
			return copilotTransactionJournal{}, errors.New("the Copilot install transaction has no desired binding")
		}
	case copilotTransactionUninstall:
		if previous == nil || desired != nil {
			return copilotTransactionJournal{}, errors.New("the Copilot uninstall transaction must contain only the installed binding")
		}
	default:
		return copilotTransactionJournal{}, errors.New("unknown Copilot transaction operation")
	}
	if previous != nil && desired != nil {
		if previous.RuntimeConfigRoot != desired.RuntimeConfigRoot ||
			previous.RuntimeMCPConfigPath != desired.RuntimeMCPConfigPath ||
			previous.RuntimeCLICommand != desired.RuntimeCLICommand ||
			!equalCopilotEnvironment(previous.MCPEnvironment, desired.MCPEnvironment) {
			return copilotTransactionJournal{}, errors.New("the Copilot transaction cannot change its CLI, config root, MCP registry path, or selector environment")
		}
	}

	targetName, _, err := copilotMCPBindingFromConfig(binding.MCPCommand, *binding)
	if err != nil {
		return copilotTransactionJournal{}, err
	}
	_, _, before, err := inspectCopilotMCPState(binding.RuntimeCLICommand, *binding)
	if err != nil {
		return copilotTransactionJournal{}, fmt.Errorf("snapshot GitHub Copilot MCP registry: %w", err)
	}
	previousName := ""
	if previous != nil {
		previousName, _, err = copilotMCPBindingFromConfig(previous.MCPCommand, *previous)
		if err != nil {
			return copilotTransactionJournal{}, err
		}
	}
	record := copilotTransactionSnapshotFromLive(before, targetName, previousName)
	identifier := make([]byte, 16)
	if _, err := rand.Read(identifier); err != nil {
		return copilotTransactionJournal{}, fmt.Errorf("create Copilot transaction id: %w", err)
	}
	journal := copilotTransactionJournal{
		SchemaVersion:    copilotTransactionSchema,
		ID:               hex.EncodeToString(identifier),
		Operation:        operation,
		Previous:         cloneCopilotTransactionConfig(previous),
		Desired:          cloneCopilotTransactionConfig(desired),
		ProviderBefore:   record,
		providerFileInfo: before.fileInfo,
	}
	if err := writeCopilotTransactionJournal(binding.RuntimeConfigRoot, journal); err != nil {
		return copilotTransactionJournal{}, err
	}
	return journal, nil
}

func cloneCopilotTransactionConfig(cfg *transcriptcapture.Config) *transcriptcapture.Config {
	if cfg == nil {
		return nil
	}
	configCopy := *cfg
	configCopy.MCPEnvironment = cloneCopilotEnvironment(cfg.MCPEnvironment)
	configCopy.ManagedPermissions = append([]string(nil), cfg.ManagedPermissions...)
	return &configCopy
}

func validateCopilotTransactionConfig(cfg transcriptcapture.Config) error {
	if cfg.Runtime != transcriptcapture.RuntimeCopilot {
		return errors.New("the Copilot transaction contains a non-Copilot integration")
	}
	if cfg.RuntimeConfigRoot == "" || !filepath.IsAbs(cfg.RuntimeConfigRoot) ||
		filepath.Clean(cfg.RuntimeConfigRoot) != cfg.RuntimeConfigRoot {
		return errors.New("the Copilot transaction config root must be a clean absolute path")
	}
	if cfg.RuntimeMCPConfigPath != filepath.Join(cfg.RuntimeConfigRoot, "mcp-config.json") {
		return errors.New("the Copilot transaction MCP config path is outside its pinned config root")
	}
	if err := validateCopilotCLISelection(cfg.RuntimeCLICommand, cfg); err != nil {
		return err
	}
	_, _, err := copilotMCPBindingFromConfig(cfg.MCPCommand, cfg)
	return err
}

func copilotTransactionSnapshotFromLive(snapshot copilotMCPConfigSnapshot, targetName, previousName string) copilotTransactionSnapshot {
	siblings := cloneCopilotSemanticFields(snapshot.siblings)
	if previousName != "" && previousName != targetName {
		delete(siblings, previousName)
	}
	record := copilotTransactionSnapshot{
		Path: snapshot.path, Exists: snapshot.exists, Mode: uint32(snapshot.mode.Perm()),
		RootFields: cloneCopilotSemanticFields(snapshot.rootFields),
		Siblings:   siblings, TargetName: targetName, PreviousName: previousName,
	}
	if snapshot.exists {
		digest := sha256.Sum256(snapshot.raw)
		record.SHA256 = hex.EncodeToString(digest[:])
	}
	return record
}

func cloneCopilotSemanticFields(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func writeCopilotTransactionJournal(configRoot string, journal copilotTransactionJournal) error {
	path := copilotTransactionPath(configRoot)
	if _, err := os.Lstat(path); err == nil {
		return errors.New("an interrupted Copilot transaction requires recovery")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ensureCopilotTransactionRoot(configRoot); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	temporary, err := os.CreateTemp(configRoot, ".witself-copilot-transaction-")
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
	return syncCopilotTransactionRoot(configRoot)
}

func ensureCopilotTransactionRoot(configRoot string) error {
	if err := os.MkdirAll(configRoot, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(configRoot)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("the Copilot transaction root must be a real directory")
	}
	return nil
}

func loadCopilotTransactionJournal(configRoot string) (copilotTransactionJournal, error) {
	journal, _, err := loadCopilotTransactionJournalFile(configRoot)
	return journal, err
}

func loadCopilotTransactionJournalFile(configRoot string) (copilotTransactionJournal, integrationTransactionJournalFile, error) {
	path := copilotTransactionPath(configRoot)
	fileSnapshot, err := loadIntegrationTransactionJournalFile(path, "Copilot transaction journal")
	if err != nil {
		return copilotTransactionJournal{}, fileSnapshot, err
	}
	if err := rejectDuplicateJSONKeys(fileSnapshot.raw); err != nil {
		return copilotTransactionJournal{}, fileSnapshot, fmt.Errorf("parse Copilot transaction journal: %w", err)
	}
	var journal copilotTransactionJournal
	decoder := json.NewDecoder(bytes.NewReader(fileSnapshot.raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return copilotTransactionJournal{}, fileSnapshot, fmt.Errorf("parse Copilot transaction journal: %w", err)
	}
	if journal.SchemaVersion != copilotTransactionSchema || len(journal.ID) != 32 {
		return copilotTransactionJournal{}, fileSnapshot, errors.New("unsupported Copilot transaction journal")
	}
	if _, err := hex.DecodeString(journal.ID); err != nil {
		return copilotTransactionJournal{}, fileSnapshot, errors.New("invalid Copilot transaction id")
	}
	if err := validateCopilotTransactionJournal(configRoot, journal); err != nil {
		return copilotTransactionJournal{}, fileSnapshot, err
	}
	return journal, fileSnapshot, nil
}

func validateCopilotTransactionJournal(configRoot string, journal copilotTransactionJournal) error {
	switch journal.Operation {
	case copilotTransactionInstall:
		if journal.Desired == nil {
			return errors.New("the Copilot install transaction is missing its desired binding")
		}
	case copilotTransactionUninstall:
		if journal.Previous == nil || journal.Desired != nil {
			return errors.New("the Copilot uninstall transaction has invalid bindings")
		}
	default:
		return errors.New("unknown Copilot transaction operation")
	}
	for _, candidate := range []*transcriptcapture.Config{journal.Previous, journal.Desired} {
		if candidate == nil {
			continue
		}
		if candidate.RuntimeConfigRoot != configRoot {
			return errors.New("the Copilot transaction journal does not match its config root")
		}
		if err := validateCopilotTransactionConfig(*candidate); err != nil {
			return err
		}
	}
	if journal.Previous != nil && journal.Desired != nil {
		if journal.Previous.RuntimeConfigRoot != journal.Desired.RuntimeConfigRoot ||
			journal.Previous.RuntimeMCPConfigPath != journal.Desired.RuntimeMCPConfigPath ||
			journal.Previous.RuntimeCLICommand != journal.Desired.RuntimeCLICommand ||
			!equalCopilotEnvironment(journal.Previous.MCPEnvironment, journal.Desired.MCPEnvironment) {
			return errors.New("the Copilot transaction journal changes its CLI, config root, MCP registry path, or selector environment")
		}
	}
	return nil
}

func clearCopilotTransaction(configRoot string, expected copilotTransactionJournal) error {
	current, fileSnapshot, err := loadCopilotTransactionJournalFile(configRoot)
	if err != nil {
		return err
	}
	currentRaw, currentErr := json.Marshal(current)
	expectedRaw, expectedErr := json.Marshal(expected)
	if currentErr != nil || expectedErr != nil || !bytes.Equal(currentRaw, expectedRaw) {
		return errors.New("the Copilot transaction journal changed; refusing to clear it")
	}
	if err := syncCopilotCommittedState(current); err != nil {
		return fmt.Errorf("durably commit GitHub Copilot transaction state: %w", err)
	}
	if err := removeIntegrationTransactionJournalFile(fileSnapshot); err != nil {
		return err
	}
	return syncCopilotTransactionRoot(configRoot)
}

func syncCopilotCommittedState(journal copilotTransactionJournal) error {
	binding := journal.Desired
	if binding == nil {
		binding = journal.Previous
	}
	if binding == nil {
		return errors.New("the Copilot transaction has no binding to commit")
	}
	configPath, err := transcriptcapture.ConfigPath(transcriptcapture.RuntimeCopilot)
	if err != nil {
		return err
	}
	routing, err := copilotManagedInstructionsSpecAt(binding.RuntimeConfigRoot)
	if err != nil {
		return err
	}
	paths := []struct {
		path  string
		label string
	}{
		{binding.RuntimeMCPConfigPath, "GitHub Copilot MCP registry"},
		{configPath, "GitHub Copilot integration config"},
		{routing.path, "GitHub Copilot routing policy"},
	}
	for _, state := range paths {
		if err := syncIntegrationTransactionFileState(state.path, state.label); err != nil {
			return err
		}
	}
	return nil
}

func syncCopilotTransactionRoot(path string) error {
	return syncIntegrationTransactionDirectory(path)
}

func validateCopilotTransactionProviderBefore(journal copilotTransactionJournal) error {
	binding := journal.Desired
	if binding == nil {
		binding = journal.Previous
	}
	if binding == nil {
		return errors.New("the Copilot transaction has no provider binding")
	}
	_, _, snapshot, err := inspectCopilotMCPState(binding.RuntimeCLICommand, *binding)
	if err != nil {
		return err
	}
	record := copilotTransactionSnapshotFromLive(
		snapshot, journal.ProviderBefore.TargetName, journal.ProviderBefore.PreviousName,
	)
	if !equalCopilotTransactionSnapshot(record, journal.ProviderBefore) {
		return errors.New("GitHub Copilot MCP registry changed after transaction journaling; refusing to mutate it")
	}
	if journal.ProviderBefore.Exists && journal.providerFileInfo != nil &&
		(snapshot.fileInfo == nil || !os.SameFile(journal.providerFileInfo, snapshot.fileInfo)) {
		return errors.New("GitHub Copilot MCP registry file identity changed after transaction journaling; refusing to mutate it")
	}
	return nil
}

func equalCopilotTransactionSnapshot(left, right copilotTransactionSnapshot) bool {
	return left.Path == right.Path && left.Exists == right.Exists &&
		left.Mode == right.Mode && left.SHA256 == right.SHA256 &&
		left.TargetName == right.TargetName && left.PreviousName == right.PreviousName &&
		equalCopilotSemanticFields(left.RootFields, right.RootFields) &&
		equalCopilotSemanticFields(left.Siblings, right.Siblings)
}

func validateCopilotTransactionNonTarget(journal copilotTransactionJournal, snapshot copilotMCPConfigSnapshot) error {
	current := copilotTransactionSnapshotFromLive(
		snapshot, journal.ProviderBefore.TargetName, journal.ProviderBefore.PreviousName,
	)
	if current.Path != journal.ProviderBefore.Path ||
		!equalCopilotSemanticFields(current.RootFields, journal.ProviderBefore.RootFields) ||
		!equalCopilotSemanticFields(current.Siblings, journal.ProviderBefore.Siblings) {
		return errors.New("GitHub Copilot non-target registry fields changed during the interrupted transaction; refusing recovery")
	}
	return nil
}

func recoverCopilotTransaction(configRoot string) error {
	journal, err := loadCopilotTransactionJournal(configRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	switch journal.Operation {
	case copilotTransactionInstall:
		if err := recoverCopilotInstallTransaction(journal); err != nil {
			return err
		}
	case copilotTransactionUninstall:
		if err := recoverCopilotUninstallTransaction(journal); err != nil {
			return err
		}
	default:
		return errors.New("unknown Copilot transaction operation")
	}
	return clearCopilotTransaction(configRoot, journal)
}

func recoverCopilotInstallTransaction(journal copilotTransactionJournal) error {
	desired := *journal.Desired
	currentConfig, configErr := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCopilot)
	if configErr != nil && !errors.Is(configErr, os.ErrNotExist) {
		return configErr
	}
	if configErr == nil &&
		!equalCopilotTransactionConfig(currentConfig, desired) &&
		(journal.Previous == nil || !equalCopilotTransactionConfig(currentConfig, *journal.Previous)) {
		return errors.New("the Copilot integration config changed during the interrupted install transaction")
	}
	if errors.Is(configErr, os.ErrNotExist) && journal.Previous != nil {
		return errors.New("prior Copilot integration config disappeared during the interrupted install transaction")
	}
	routing, err := copilotManagedInstructionsSpecAt(desired.RuntimeConfigRoot)
	if err != nil {
		return err
	}
	if _, err := installManagedInstructions(routing); err != nil {
		return fmt.Errorf("install GitHub Copilot memory routing instructions: %w", err)
	}
	if err := convergeCopilotTransactionDesiredMCP(journal); err != nil {
		return err
	}
	if err := transcriptcapture.SaveConfig(desired); err != nil {
		return err
	}
	return validateCopilotPersistedTopology(desired)
}

func convergeCopilotTransactionDesiredMCP(journal copilotTransactionJournal) error {
	desired := *journal.Desired
	desiredName, desiredBinding, err := copilotMCPBindingFromConfig(desired.MCPCommand, desired)
	if err != nil {
		return err
	}
	current, desiredExists, desiredSnapshot, err := inspectCopilotMCPState(desired.RuntimeCLICommand, desired)
	if err != nil {
		return err
	}
	if err := validateCopilotTransactionNonTarget(journal, desiredSnapshot); err != nil {
		return err
	}
	previousName := ""
	if journal.Previous != nil {
		previousName, _, err = copilotMCPBindingFromConfig(journal.Previous.MCPCommand, *journal.Previous)
		if err != nil {
			return err
		}
	}
	if desiredExists && equalCopilotMCPBinding(current, desiredBinding) {
		if previousName != "" && previousName != desiredName {
			_, previousExists, _, err := inspectCopilotMCPState(desired.RuntimeCLICommand, *journal.Previous)
			if err != nil {
				return err
			}
			if previousExists {
				return fmt.Errorf("GitHub Copilot MCP servers %s and %s both exist during interrupted recovery", previousName, desiredName)
			}
		}
		return nil
	}
	if desiredExists {
		if journal.Previous == nil || previousName != desiredName {
			return fmt.Errorf("GitHub Copilot MCP server %s changed to a foreign binding during interrupted recovery", desiredName)
		}
		_, previousBinding, err := copilotMCPBindingFromConfig(journal.Previous.MCPCommand, *journal.Previous)
		if err != nil || !equalCopilotMCPBinding(current, previousBinding) {
			return fmt.Errorf("GitHub Copilot MCP server %s changed to a foreign binding during interrupted recovery", desiredName)
		}
		touched, removeErr := unregisterCopilotMCPWithSnapshot(desired.RuntimeCLICommand, journal.Previous, &desiredSnapshot)
		if removeErr != nil {
			return fmt.Errorf("recover prior Copilot MCP removal (touched=%t): %w", touched, removeErr)
		}
	} else if journal.Previous != nil && previousName != desiredName {
		previousCurrent, previousExists, previousSnapshot, err := inspectCopilotMCPState(desired.RuntimeCLICommand, *journal.Previous)
		if err != nil {
			return err
		}
		if previousExists {
			_, previousBinding, err := copilotMCPBindingFromConfig(journal.Previous.MCPCommand, *journal.Previous)
			if err != nil || !equalCopilotMCPBinding(previousCurrent, previousBinding) {
				return fmt.Errorf("prior GitHub Copilot MCP server %s changed during interrupted recovery", previousName)
			}
			touched, removeErr := unregisterCopilotMCPWithSnapshot(desired.RuntimeCLICommand, journal.Previous, &previousSnapshot)
			if removeErr != nil {
				return fmt.Errorf("recover prior Copilot MCP removal (touched=%t): %w", touched, removeErr)
			}
		}
	}

	_, desiredExists, desiredSnapshot, err = inspectCopilotMCPState(desired.RuntimeCLICommand, desired)
	if err != nil {
		return err
	}
	if desiredExists {
		return fmt.Errorf("GitHub Copilot MCP server %s appeared before recovery add; refusing to claim it", desiredName)
	}
	if err := validateCopilotTransactionNonTarget(journal, desiredSnapshot); err != nil {
		return err
	}
	touched, err := registerCopilotMCPWithPlan(desired.RuntimeCLICommand, copilotMCPInstallPlan{
		desired: desired, desiredName: desiredName, desiredBinding: desiredBinding,
		expected: desiredSnapshot, registerRequired: true,
	})
	if err != nil {
		return fmt.Errorf("recover desired Copilot MCP registration (touched=%t): %w", touched, err)
	}
	return nil
}

func recoverCopilotUninstallTransaction(journal copilotTransactionJournal) error {
	previous := *journal.Previous
	currentConfig, configErr := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCopilot)
	if configErr != nil && !errors.Is(configErr, os.ErrNotExist) {
		return configErr
	}
	if configErr == nil && !equalCopilotTransactionConfig(currentConfig, previous) {
		return errors.New("the Copilot integration config changed during the interrupted uninstall transaction")
	}
	current, exists, snapshot, err := inspectCopilotMCPState(previous.RuntimeCLICommand, previous)
	if err != nil {
		return err
	}
	if err := validateCopilotTransactionNonTarget(journal, snapshot); err != nil {
		return err
	}
	if exists {
		_, expected, err := copilotMCPBindingFromConfig(previous.MCPCommand, previous)
		if err != nil || !equalCopilotMCPBinding(current, expected) {
			return errors.New("installed GitHub Copilot MCP server changed during interrupted uninstall recovery")
		}
		touched, removeErr := unregisterCopilotMCPWithSnapshot(previous.RuntimeCLICommand, &previous, &snapshot)
		if removeErr != nil {
			return fmt.Errorf("recover Copilot MCP removal (touched=%t): %w", touched, removeErr)
		}
	}
	routing, err := copilotManagedInstructionsSpecAt(previous.RuntimeConfigRoot)
	if err != nil {
		return err
	}
	if _, err := removeManagedInstructions(routing); err != nil {
		return fmt.Errorf("remove GitHub Copilot memory routing instructions: %w", err)
	}
	if configErr == nil {
		if err := transcriptcapture.RemoveConfig(transcriptcapture.RuntimeCopilot); err != nil {
			return err
		}
	}
	_, exists, snapshot, err = inspectCopilotMCPState(previous.RuntimeCLICommand, previous)
	if err != nil {
		return err
	}
	if exists {
		return errors.New("GitHub Copilot MCP server reappeared during interrupted uninstall recovery")
	}
	return validateCopilotTransactionNonTarget(journal, snapshot)
}

func equalCopilotTransactionConfig(left, right transcriptcapture.Config) bool {
	leftRaw, leftErr := json.Marshal(left)
	rightRaw, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftRaw, rightRaw)
}
