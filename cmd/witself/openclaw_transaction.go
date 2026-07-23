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

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const (
	openClawTransactionSchema    = "witself.openclaw-transaction.v1"
	openClawTransactionInstall   = "install"
	openClawTransactionUninstall = "uninstall"
)

type openClawTransactionJournal struct {
	SchemaVersion  string                      `json:"schema_version"`
	ID             string                      `json:"id"`
	Operation      string                      `json:"operation"`
	Previous       *transcriptcapture.Config   `json:"previous,omitempty"`
	Desired        *transcriptcapture.Config   `json:"desired,omitempty"`
	ProviderBefore openClawTransactionSnapshot `json:"provider_before"`
}

type openClawTransactionSnapshot struct {
	TargetExists bool              `json:"target_exists"`
	Target       string            `json:"target,omitempty"`
	NonTarget    map[string]string `json:"non_target"`
}

func openClawTransactionRootFromEnvironment(environment map[string]string) (string, error) {
	if stateRoot := environment["OPENCLAW_STATE_DIR"]; stateRoot != "" {
		return cleanAbsoluteOpenClawEnvironmentPath("OPENCLAW_STATE_DIR", stateRoot)
	}
	if configPath := environment["OPENCLAW_CONFIG_PATH"]; configPath != "" {
		clean, err := cleanAbsoluteOpenClawEnvironmentPath("OPENCLAW_CONFIG_PATH", configPath)
		if err != nil {
			return "", err
		}
		return filepath.Dir(clean), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve default OpenClaw transaction root: %w", err)
	}
	home, err = cleanAbsoluteOpenClawEnvironmentPath("HOME", home)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".openclaw"), nil
}

func openClawTransactionRootFromConfig(cfg transcriptcapture.Config) (string, error) {
	return openClawTransactionRootFromEnvironment(cfg.MCPEnvironment)
}

func openClawTransactionPath(configRoot string) string {
	return filepath.Join(configRoot, ".witself-openclaw-transaction.json")
}

func beginOpenClawTransaction(operation string, previous, desired *transcriptcapture.Config) (openClawTransactionJournal, error) {
	binding := desired
	if binding == nil {
		binding = previous
	}
	if binding == nil {
		return openClawTransactionJournal{}, errors.New("OpenClaw transaction has no binding")
	}
	if err := validateOpenClawTransactionConfig(*binding); err != nil {
		return openClawTransactionJournal{}, err
	}
	if previous != nil {
		if err := validateOpenClawTransactionConfig(*previous); err != nil {
			return openClawTransactionJournal{}, err
		}
	}
	if desired != nil {
		if err := validateOpenClawTransactionConfig(*desired); err != nil {
			return openClawTransactionJournal{}, err
		}
	}
	switch operation {
	case openClawTransactionInstall:
		if desired == nil {
			return openClawTransactionJournal{}, errors.New("OpenClaw install transaction has no desired binding")
		}
	case openClawTransactionUninstall:
		if previous == nil || desired != nil {
			return openClawTransactionJournal{}, errors.New("OpenClaw uninstall transaction must contain only the installed binding")
		}
	default:
		return openClawTransactionJournal{}, errors.New("unknown OpenClaw transaction operation")
	}
	if previous != nil && desired != nil {
		if previous.RuntimeCLICommand != desired.RuntimeCLICommand {
			return openClawTransactionJournal{}, errors.New("OpenClaw transaction cannot change its CLI")
		}
		if previous.RuntimeWorkspace != desired.RuntimeWorkspace ||
			previous.RuntimeAgentID != desired.RuntimeAgentID {
			return openClawTransactionJournal{}, errors.New("OpenClaw transaction cannot change its owned workspace or runtime agent")
		}
		if !equalOpenClawMCPEnvironment(previous.MCPEnvironment, desired.MCPEnvironment) &&
			!legacyOpenClawDefaultEnvironmentCanMigrate(previous.MCPEnvironment, desired.MCPEnvironment) {
			return openClawTransactionJournal{}, errors.New("OpenClaw transaction cannot change its selector environment")
		}
	}
	_, _, before, err := inspectOpenClawMCPState(binding.RuntimeCLICommand, binding.MCPEnvironment)
	if err != nil {
		return openClawTransactionJournal{}, fmt.Errorf("snapshot OpenClaw MCP registry: %w", err)
	}
	configRoot, err := openClawTransactionRootFromConfig(*binding)
	if err != nil {
		return openClawTransactionJournal{}, err
	}
	identifier := make([]byte, 16)
	if _, err := rand.Read(identifier); err != nil {
		return openClawTransactionJournal{}, fmt.Errorf("create OpenClaw transaction id: %w", err)
	}
	journal := openClawTransactionJournal{
		SchemaVersion:  openClawTransactionSchema,
		ID:             hex.EncodeToString(identifier),
		Operation:      operation,
		Previous:       cloneOpenClawTransactionConfig(previous),
		Desired:        cloneOpenClawTransactionConfig(desired),
		ProviderBefore: openClawTransactionSnapshotFromLive(before),
	}
	if err := writeOpenClawTransactionJournal(configRoot, journal); err != nil {
		return openClawTransactionJournal{}, err
	}
	return journal, nil
}

func cloneOpenClawTransactionConfig(cfg *transcriptcapture.Config) *transcriptcapture.Config {
	if cfg == nil {
		return nil
	}
	configCopy := *cfg
	configCopy.MCPEnvironment = cloneOpenClawEnvironment(cfg.MCPEnvironment)
	configCopy.ManagedPermissions = append([]string(nil), cfg.ManagedPermissions...)
	return &configCopy
}

func validateOpenClawTransactionConfig(cfg transcriptcapture.Config) error {
	if cfg.Runtime != transcriptcapture.RuntimeOpenClaw {
		return errors.New("OpenClaw transaction contains a non-OpenClaw integration")
	}
	if cfg.RuntimeCLICommand == "" || !filepath.IsAbs(cfg.RuntimeCLICommand) {
		return errors.New("OpenClaw transaction requires the pinned runtime CLI")
	}
	if _, err := openClawTransactionRootFromConfig(cfg); err != nil {
		return err
	}
	_, err := openClawMCPBindingFromConfig(cfg.MCPCommand, cfg)
	return err
}

func openClawTransactionSnapshotFromLive(snapshot openClawMCPConfigSnapshot) openClawTransactionSnapshot {
	return openClawTransactionSnapshot{
		TargetExists: snapshot.targetExists,
		Target:       snapshot.target,
		NonTarget:    cloneCopilotSemanticFields(snapshot.nonTarget),
	}
}

func equalOpenClawTransactionSnapshot(left, right openClawTransactionSnapshot) bool {
	return left.TargetExists == right.TargetExists && left.Target == right.Target &&
		equalCopilotSemanticFields(left.NonTarget, right.NonTarget)
}

func writeOpenClawTransactionJournal(configRoot string, journal openClawTransactionJournal) error {
	path := openClawTransactionPath(configRoot)
	if _, err := os.Lstat(path); err == nil {
		return errors.New("an interrupted OpenClaw transaction requires recovery")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ensureOpenClawTransactionRoot(configRoot); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	temporary, err := os.CreateTemp(configRoot, ".witself-openclaw-transaction-")
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
	return syncOpenClawTransactionRoot(configRoot)
}

func ensureOpenClawTransactionRoot(configRoot string) error {
	if err := os.MkdirAll(configRoot, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(configRoot)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("OpenClaw transaction root must be a real directory")
	}
	return nil
}

func loadOpenClawTransactionJournal(configRoot string) (openClawTransactionJournal, error) {
	journal, _, err := loadOpenClawTransactionJournalFile(configRoot)
	return journal, err
}

func loadOpenClawTransactionJournalFile(configRoot string) (openClawTransactionJournal, integrationTransactionJournalFile, error) {
	path := openClawTransactionPath(configRoot)
	fileSnapshot, err := loadIntegrationTransactionJournalFile(path, "OpenClaw transaction journal")
	if err != nil {
		return openClawTransactionJournal{}, fileSnapshot, err
	}
	if err := rejectDuplicateJSONKeys(fileSnapshot.raw); err != nil {
		return openClawTransactionJournal{}, fileSnapshot, fmt.Errorf("parse OpenClaw transaction journal: %w", err)
	}
	var journal openClawTransactionJournal
	decoder := json.NewDecoder(bytes.NewReader(fileSnapshot.raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return openClawTransactionJournal{}, fileSnapshot, fmt.Errorf("parse OpenClaw transaction journal: %w", err)
	}
	if journal.SchemaVersion != openClawTransactionSchema || len(journal.ID) != 32 {
		return openClawTransactionJournal{}, fileSnapshot, errors.New("unsupported OpenClaw transaction journal")
	}
	if _, err := hex.DecodeString(journal.ID); err != nil {
		return openClawTransactionJournal{}, fileSnapshot, errors.New("invalid OpenClaw transaction id")
	}
	if err := validateOpenClawTransactionJournal(configRoot, journal); err != nil {
		return openClawTransactionJournal{}, fileSnapshot, err
	}
	return journal, fileSnapshot, nil
}

func validateOpenClawTransactionJournal(configRoot string, journal openClawTransactionJournal) error {
	switch journal.Operation {
	case openClawTransactionInstall:
		if journal.Desired == nil {
			return errors.New("OpenClaw install transaction is missing its desired binding")
		}
	case openClawTransactionUninstall:
		if journal.Previous == nil || journal.Desired != nil {
			return errors.New("OpenClaw uninstall transaction has invalid bindings")
		}
	default:
		return errors.New("unknown OpenClaw transaction operation")
	}
	binding := journal.Desired
	if binding == nil {
		binding = journal.Previous
	}
	if binding == nil {
		return errors.New("OpenClaw transaction journal has no binding")
	}
	for _, candidate := range []*transcriptcapture.Config{journal.Previous, journal.Desired} {
		if candidate == nil {
			continue
		}
		if err := validateOpenClawTransactionConfig(*candidate); err != nil {
			return err
		}
		root, err := openClawTransactionRootFromConfig(*candidate)
		if err != nil || root != configRoot {
			return errors.New("OpenClaw transaction journal does not match its config root")
		}
	}
	if journal.Previous != nil && journal.Desired != nil {
		if journal.Previous.RuntimeCLICommand != journal.Desired.RuntimeCLICommand {
			return errors.New("OpenClaw transaction journal changes its CLI")
		}
		if journal.Previous.RuntimeWorkspace != journal.Desired.RuntimeWorkspace ||
			journal.Previous.RuntimeAgentID != journal.Desired.RuntimeAgentID {
			return errors.New("OpenClaw transaction journal changes its owned workspace or runtime agent")
		}
		if !equalOpenClawMCPEnvironment(journal.Previous.MCPEnvironment, journal.Desired.MCPEnvironment) &&
			!legacyOpenClawDefaultEnvironmentCanMigrate(journal.Previous.MCPEnvironment, journal.Desired.MCPEnvironment) {
			return errors.New("OpenClaw transaction journal changes its selector environment")
		}
	}
	return nil
}

func clearOpenClawTransaction(configRoot string, expected openClawTransactionJournal) error {
	current, fileSnapshot, err := loadOpenClawTransactionJournalFile(configRoot)
	if err != nil {
		return err
	}
	currentRaw, currentErr := json.Marshal(current)
	expectedRaw, expectedErr := json.Marshal(expected)
	if currentErr != nil || expectedErr != nil || !bytes.Equal(currentRaw, expectedRaw) {
		return errors.New("OpenClaw transaction journal changed; refusing to clear it")
	}
	if err := syncOpenClawCommittedState(current); err != nil {
		return fmt.Errorf("durably commit OpenClaw transaction state: %w", err)
	}
	if err := removeIntegrationTransactionJournalFile(fileSnapshot); err != nil {
		return err
	}
	return syncOpenClawTransactionRoot(configRoot)
}

func syncOpenClawCommittedState(journal openClawTransactionJournal) error {
	binding := journal.Desired
	if binding == nil {
		binding = journal.Previous
	}
	if binding == nil {
		return errors.New("OpenClaw transaction has no binding to commit")
	}
	configPath, err := transcriptcapture.ConfigPath(transcriptcapture.RuntimeOpenClaw)
	if err != nil {
		return err
	}
	routing, _, managed, err := runtimeMemoryRoutingSpecAt(
		transcriptcapture.RuntimeOpenClaw,
		binding.RuntimeWorkspace,
	)
	if err != nil {
		return err
	}
	paths := []struct {
		path  string
		label string
	}{
		{configPath, "OpenClaw integration config"},
	}
	if providerConfigPath := binding.MCPEnvironment["OPENCLAW_CONFIG_PATH"]; providerConfigPath != "" {
		paths = append(paths, struct {
			path  string
			label string
		}{providerConfigPath, "OpenClaw selector config"})
	}
	if managed {
		paths = append(paths, struct {
			path  string
			label string
		}{routing.path, "OpenClaw routing policy"})
	}
	for _, state := range paths {
		if err := syncIntegrationTransactionFileState(state.path, state.label); err != nil {
			return err
		}
	}
	if stateRoot := binding.MCPEnvironment["OPENCLAW_STATE_DIR"]; stateRoot != "" {
		if err := syncIntegrationTransactionNearestDirectory(stateRoot); err != nil {
			return err
		}
	}
	// OpenClaw exposes its effective MCP registry only through the CLI. The
	// selector config and state root above are the only documented filesystem
	// handles available to this adapter, so CLI success plus exact post-state
	// inspection remains the durability boundary for the hidden registry.
	return nil
}

func syncOpenClawTransactionRoot(path string) error {
	return syncIntegrationTransactionDirectory(path)
}

func validateOpenClawTransactionProviderBefore(journal openClawTransactionJournal) error {
	binding := journal.Desired
	if binding == nil {
		binding = journal.Previous
	}
	if binding == nil {
		return errors.New("OpenClaw transaction has no provider binding")
	}
	_, _, snapshot, err := inspectOpenClawMCPState(binding.RuntimeCLICommand, binding.MCPEnvironment)
	if err != nil {
		return err
	}
	if !equalOpenClawTransactionSnapshot(
		openClawTransactionSnapshotFromLive(snapshot), journal.ProviderBefore,
	) {
		return errors.New("OpenClaw MCP registry changed after transaction journaling; refusing to mutate it")
	}
	return nil
}

func validateOpenClawTransactionNonTarget(journal openClawTransactionJournal, snapshot openClawMCPConfigSnapshot) error {
	if !equalCopilotSemanticFields(snapshot.nonTarget, journal.ProviderBefore.NonTarget) {
		return errors.New("OpenClaw sibling MCP servers changed during the interrupted transaction; refusing recovery")
	}
	return nil
}

func recoverOpenClawTransaction(configRoot string) error {
	journal, err := loadOpenClawTransactionJournal(configRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	switch journal.Operation {
	case openClawTransactionInstall:
		if err := recoverOpenClawInstallTransaction(journal); err != nil {
			return err
		}
	case openClawTransactionUninstall:
		if err := recoverOpenClawUninstallTransaction(journal); err != nil {
			return err
		}
	default:
		return errors.New("unknown OpenClaw transaction operation")
	}
	return clearOpenClawTransaction(configRoot, journal)
}

func recoverOpenClawInstallTransaction(journal openClawTransactionJournal) error {
	desired := *journal.Desired
	currentConfig, configErr := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw)
	if configErr != nil && !errors.Is(configErr, os.ErrNotExist) {
		return configErr
	}
	if configErr == nil &&
		!equalOpenClawTransactionConfig(currentConfig, desired) &&
		(journal.Previous == nil || !equalOpenClawTransactionConfig(currentConfig, *journal.Previous)) {
		return errors.New("OpenClaw integration config changed during the interrupted install transaction")
	}
	if errors.Is(configErr, os.ErrNotExist) && journal.Previous != nil {
		return errors.New("prior OpenClaw integration config disappeared during the interrupted install transaction")
	}
	if _, err := installRuntimeMemoryRoutingInstructionsAt(transcriptcapture.RuntimeOpenClaw, desired.RuntimeWorkspace); err != nil {
		return err
	}
	if err := convergeOpenClawTransactionDesiredMCP(journal); err != nil {
		return err
	}
	if err := transcriptcapture.SaveConfig(desired); err != nil {
		return err
	}
	return validateOpenClawInstalledIntegration(desired)
}

func convergeOpenClawTransactionDesiredMCP(journal openClawTransactionJournal) error {
	desired := *journal.Desired
	desiredBinding, err := openClawMCPBindingFromConfig(desired.MCPCommand, desired)
	if err != nil {
		return err
	}
	current, exists, snapshot, err := inspectOpenClawMCPState(desired.RuntimeCLICommand, desired.MCPEnvironment)
	if err != nil {
		return err
	}
	if err := validateOpenClawTransactionNonTarget(journal, snapshot); err != nil {
		return err
	}
	if exists && equalOpenClawMCPBinding(current, desiredBinding) {
		return nil
	}
	if exists {
		if journal.Previous == nil {
			return errors.New("OpenClaw MCP server witself changed to a foreign binding during interrupted recovery")
		}
		previousBinding, err := openClawMCPBindingFromConfig(journal.Previous.MCPCommand, *journal.Previous)
		if err != nil || !equalOpenClawMCPBinding(current, previousBinding) {
			return errors.New("OpenClaw MCP server witself changed to a foreign binding during interrupted recovery")
		}
		touched, removeErr := unregisterOpenClawMCPWithSnapshot(desired.RuntimeCLICommand, &previousBinding, &snapshot)
		if removeErr != nil {
			return fmt.Errorf("recover prior OpenClaw MCP removal (touched=%t): %w", touched, removeErr)
		}
	}
	_, exists, snapshot, err = inspectOpenClawMCPState(desired.RuntimeCLICommand, desired.MCPEnvironment)
	if err != nil {
		return err
	}
	if exists {
		return errors.New("OpenClaw MCP server witself appeared before recovery add; refusing to claim it")
	}
	if err := validateOpenClawTransactionNonTarget(journal, snapshot); err != nil {
		return err
	}
	touched, err := registerOpenClawMCPBindingWithPlan(desired.RuntimeCLICommand, openClawMCPInstallPlan{
		desired: desiredBinding, expected: snapshot, registerRequired: true,
	})
	if err != nil {
		return fmt.Errorf("recover desired OpenClaw MCP registration (touched=%t): %w", touched, err)
	}
	return nil
}

func recoverOpenClawUninstallTransaction(journal openClawTransactionJournal) error {
	previous := *journal.Previous
	currentConfig, configErr := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw)
	if configErr != nil && !errors.Is(configErr, os.ErrNotExist) {
		return configErr
	}
	if configErr == nil && !equalOpenClawTransactionConfig(currentConfig, previous) {
		return errors.New("OpenClaw integration config changed during the interrupted uninstall transaction")
	}
	current, exists, snapshot, err := inspectOpenClawMCPState(previous.RuntimeCLICommand, previous.MCPEnvironment)
	if err != nil {
		return err
	}
	if err := validateOpenClawTransactionNonTarget(journal, snapshot); err != nil {
		return err
	}
	if exists {
		expected, err := openClawMCPBindingFromConfig(previous.MCPCommand, previous)
		if err != nil || !equalOpenClawMCPBinding(current, expected) {
			return errors.New("installed OpenClaw MCP server changed during interrupted uninstall recovery")
		}
		touched, removeErr := unregisterOpenClawMCPWithSnapshot(previous.RuntimeCLICommand, &expected, &snapshot)
		if removeErr != nil {
			return fmt.Errorf("recover OpenClaw MCP removal (touched=%t): %w", touched, removeErr)
		}
	}
	if _, err := removeRuntimeMemoryRoutingInstructionsAt(transcriptcapture.RuntimeOpenClaw, previous.RuntimeWorkspace); err != nil {
		return err
	}
	if configErr == nil {
		if err := transcriptcapture.RemoveConfig(transcriptcapture.RuntimeOpenClaw); err != nil {
			return err
		}
	}
	_, exists, snapshot, err = inspectOpenClawMCPState(previous.RuntimeCLICommand, previous.MCPEnvironment)
	if err != nil {
		return err
	}
	if exists {
		return errors.New("OpenClaw MCP server reappeared during interrupted uninstall recovery")
	}
	return validateOpenClawTransactionNonTarget(journal, snapshot)
}

func equalOpenClawTransactionConfig(left, right transcriptcapture.Config) bool {
	leftRaw, leftErr := json.Marshal(left)
	rightRaw, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftRaw, rightRaw)
}
