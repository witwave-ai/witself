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

	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const (
	antigravityTransactionSchema    = "witself.antigravity-transaction.v1"
	antigravityTransactionInstall   = "install"
	antigravityTransactionUninstall = "uninstall"
)

type antigravityTransactionJournal struct {
	SchemaVersion string                    `json:"schema_version"`
	ID            string                    `json:"id"`
	Operation     string                    `json:"operation"`
	Previous      *transcriptcapture.Config `json:"previous,omitempty"`
	Desired       *transcriptcapture.Config `json:"desired,omitempty"`
}

func antigravityTransactionPath(configRoot string) string {
	return filepath.Join(configRoot, ".witself-antigravity-transaction.json")
}

func currentAntigravityConfigRoot() (string, error) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve Antigravity home: %w", err)
	}
	userHome, err = cleanAntigravityAbsolutePath("HOME", userHome)
	if err != nil {
		return "", err
	}
	return filepath.Join(userHome, ".gemini", "config"), nil
}

func beginAntigravityTransaction(operation string, previous, desired *transcriptcapture.Config) (antigravityTransactionJournal, error) {
	var binding *transcriptcapture.Config
	if desired != nil {
		binding = desired
	} else {
		binding = previous
	}
	if binding == nil {
		return antigravityTransactionJournal{}, errors.New("antigravity transaction has no binding")
	}
	if err := validateAntigravityTransactionConfig(binding.RuntimeConfigRoot, *binding); err != nil {
		return antigravityTransactionJournal{}, err
	}
	if previous != nil {
		if err := validateAntigravityTransactionConfig(binding.RuntimeConfigRoot, *previous); err != nil {
			return antigravityTransactionJournal{}, err
		}
		if _, err := verifiedAntigravitySourceBundle(*previous); err != nil {
			return antigravityTransactionJournal{}, fmt.Errorf("verify previous Antigravity transaction bundle: %w", err)
		}
	}
	if desired != nil {
		if err := validateAntigravityTransactionConfig(binding.RuntimeConfigRoot, *desired); err != nil {
			return antigravityTransactionJournal{}, err
		}
		bundle, err := antigravityBundleFromConfig(*desired)
		if err != nil || bundle.digest() != desired.RuntimePluginDigest {
			return antigravityTransactionJournal{}, errors.New("desired Antigravity transaction bundle does not match the current integration policy")
		}
	}
	if previous != nil && desired != nil {
		previousServer, previousErr := antigravityMCPServerName(*previous)
		desiredServer, desiredErr := antigravityMCPServerName(*desired)
		if previousErr != nil || desiredErr != nil || previous.RuntimeConfigRoot != desired.RuntimeConfigRoot ||
			previous.RuntimePluginPath != desired.RuntimePluginPath || previousServer != desiredServer {
			return antigravityTransactionJournal{}, errors.New("antigravity transaction cannot change its config root or collision-resistant binding identity")
		}
	}
	switch operation {
	case antigravityTransactionInstall:
		if desired == nil {
			return antigravityTransactionJournal{}, errors.New("antigravity install transaction has no desired binding")
		}
	case antigravityTransactionUninstall:
		if previous == nil || desired != nil {
			return antigravityTransactionJournal{}, errors.New("antigravity uninstall transaction must contain only the installed binding")
		}
	default:
		return antigravityTransactionJournal{}, errors.New("unknown Antigravity transaction operation")
	}
	identifier := make([]byte, 16)
	if _, err := rand.Read(identifier); err != nil {
		return antigravityTransactionJournal{}, fmt.Errorf("create Antigravity transaction id: %w", err)
	}
	journal := antigravityTransactionJournal{
		SchemaVersion: antigravityTransactionSchema,
		ID:            hex.EncodeToString(identifier),
		Operation:     operation,
		Previous:      previous,
		Desired:       desired,
	}
	if err := writeAntigravityTransactionJournal(binding.RuntimeConfigRoot, journal); err != nil {
		return antigravityTransactionJournal{}, err
	}
	return journal, nil
}

func writeAntigravityTransactionJournal(configRoot string, journal antigravityTransactionJournal) error {
	path := antigravityTransactionPath(configRoot)
	if _, err := os.Lstat(path); err == nil {
		return errors.New("an interrupted Antigravity transaction requires recovery")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(configRoot, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	temporary, err := os.CreateTemp(configRoot, ".witself-antigravity-transaction-")
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
	return syncAntigravityConfigRoot(configRoot)
}

func loadAntigravityTransactionJournal(configRoot string) (antigravityTransactionJournal, error) {
	path := antigravityTransactionPath(configRoot)
	info, err := os.Lstat(path)
	if err != nil {
		return antigravityTransactionJournal{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return antigravityTransactionJournal{}, errors.New("antigravity transaction journal must be a real 0600 regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return antigravityTransactionJournal{}, err
	}
	var journal antigravityTransactionJournal
	if err := json.Unmarshal(raw, &journal); err != nil {
		return antigravityTransactionJournal{}, fmt.Errorf("parse Antigravity transaction journal: %w", err)
	}
	if journal.SchemaVersion != antigravityTransactionSchema || len(journal.ID) != 32 {
		return antigravityTransactionJournal{}, errors.New("unsupported Antigravity transaction journal")
	}
	if _, err := hex.DecodeString(journal.ID); err != nil {
		return antigravityTransactionJournal{}, errors.New("invalid Antigravity transaction id")
	}
	return journal, nil
}

func clearAntigravityTransaction(configRoot string, expected antigravityTransactionJournal) error {
	current, err := loadAntigravityTransactionJournal(configRoot)
	if err != nil {
		return err
	}
	currentRaw, currentErr := json.Marshal(current)
	expectedRaw, expectedErr := json.Marshal(expected)
	if currentErr != nil || expectedErr != nil || !bytes.Equal(currentRaw, expectedRaw) {
		return errors.New("antigravity transaction journal changed; refusing to clear it")
	}
	if err := syncAntigravityCommittedState(current); err != nil {
		return fmt.Errorf("durably fence Antigravity transaction state: %w", err)
	}
	if err := os.Remove(antigravityTransactionPath(configRoot)); err != nil {
		return err
	}
	return syncAntigravityConfigRoot(configRoot)
}

// syncAntigravityCommittedState makes the selected config, plugin, and recovery
// source durable before the journal that can reconstruct them is removed. This
// covers host/power loss as well as an ordinary process stop on the supported
// macOS and Linux providers.
func syncAntigravityCommittedState(journal antigravityTransactionJournal) error {
	current, configState, err := currentAntigravityConfigState(journal.Previous, journal.Desired)
	if err != nil {
		return err
	}
	var committed *transcriptcapture.Config
	switch configState {
	case antigravityConfigDesired, antigravityConfigPrevious:
		committed = &current
	case antigravityConfigMissing:
	case antigravityConfigForeign:
		return errors.New("integration config changed before transaction commit")
	default:
		return errors.New("integration config has an unknown transaction state")
	}

	binding := journal.Desired
	if binding == nil {
		binding = journal.Previous
	}
	if binding == nil {
		return errors.New("transaction has no Antigravity binding")
	}
	if err := validateAntigravityCanonicalOwnershipPaths(*binding); err != nil {
		return err
	}

	configs := make([]*transcriptcapture.Config, 0, 2)
	if journal.Previous != nil {
		configs = append(configs, journal.Previous)
	}
	if journal.Desired != nil && (journal.Previous == nil || journal.Previous.RuntimePluginDigest != journal.Desired.RuntimePluginDigest) {
		configs = append(configs, journal.Desired)
	}
	for _, cfg := range configs {
		bundle, bundleErr := verifiedAntigravitySourceBundle(*cfg)
		if bundleErr != nil {
			return fmt.Errorf("verify transaction recovery source: %w", bundleErr)
		}
		if err := syncAntigravityBundleDirectory(cfg.RuntimePluginSource, bundle); err != nil {
			return fmt.Errorf("sync transaction recovery source: %w", err)
		}
		for _, scratch := range []string{
			antigravityBundleSwapPath(cfg.RuntimePluginPath, bundle),
			antigravityBundleRemovalPath(cfg.RuntimePluginPath, bundle),
		} {
			if _, statErr := os.Lstat(scratch); statErr == nil {
				return fmt.Errorf("transaction scratch remains at %s", scratch)
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return statErr
			}
		}
	}

	if committed == nil {
		if _, statErr := os.Lstat(binding.RuntimePluginPath); statErr == nil {
			return errors.New("antigravity plugin remains without its integration config")
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
	} else {
		if err := rejectAntigravityDiscoveryCollisions(*committed); err != nil {
			return err
		}
		bundle, bundleErr := verifiedAntigravitySourceBundle(*committed)
		if bundleErr != nil {
			return bundleErr
		}
		if err := syncAntigravityBundleDirectory(committed.RuntimePluginPath, bundle); err != nil {
			return fmt.Errorf("sync installed Antigravity plugin: %w", err)
		}
	}

	configPath, err := transcriptcapture.ConfigPath(transcriptcapture.RuntimeAntigravity)
	if err != nil {
		return err
	}
	if committed != nil {
		if err := syncAntigravityRegularFile(configPath); err != nil {
			return fmt.Errorf("sync Antigravity integration config: %w", err)
		}
	}
	for _, directory := range []string{
		filepath.Dir(configPath),
		filepath.Dir(binding.RuntimePluginPath),
		antigravityBundleScratchParent(binding.RuntimePluginPath),
	} {
		if err := syncAntigravityDirectoryIfPresent(directory); err != nil {
			return err
		}
	}
	return nil
}

func syncAntigravityBundleDirectory(path string, bundle antigravityPluginBundle) error {
	if err := verifyAntigravityBundleDirectory(path, bundle); err != nil {
		return err
	}
	for _, relativePath := range []string{"plugin.json", "mcp_config.json", "rules/witself.md"} {
		if err := syncAntigravityRegularFile(filepath.Join(path, filepath.FromSlash(relativePath))); err != nil {
			return err
		}
	}
	if err := syncAntigravityDirectoryIfPresent(filepath.Join(path, "rules")); err != nil {
		return err
	}
	if err := syncAntigravityDirectoryIfPresent(path); err != nil {
		return err
	}
	return syncAntigravityDirectoryIfPresent(filepath.Dir(path))
}

func syncAntigravityRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must be a real regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return file.Sync()
}

func syncAntigravityDirectoryIfPresent(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must be a real directory", path)
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

func syncAntigravityConfigRoot(configRoot string) error {
	directory, err := os.Open(configRoot)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

func validateAntigravityTransactionConfig(configRoot string, cfg transcriptcapture.Config) error {
	if cfg.Runtime != transcriptcapture.RuntimeAntigravity || cfg.SchemaVersion != transcriptcapture.SchemaVersion {
		return errors.New("antigravity transaction binding has an invalid runtime or schema")
	}
	if cfg.RuntimeConfigRoot != configRoot || !filepath.IsAbs(configRoot) || filepath.Clean(configRoot) != configRoot {
		return errors.New("antigravity transaction config root is invalid")
	}
	if err := validateAntigravityCanonicalOwnershipPaths(cfg); err != nil {
		return err
	}
	pluginName, err := antigravityPluginName(cfg)
	if err != nil {
		return err
	}
	if cfg.RuntimePluginPath != filepath.Join(configRoot, "plugins", pluginName) {
		return errors.New("antigravity transaction plugin path is invalid")
	}
	if len(cfg.MCPEnvironment) != 1 {
		return errors.New("antigravity transaction environment is invalid")
	}
	witselfHome, err := cleanAntigravityAbsolutePath("WITSELF_HOME", cfg.MCPEnvironment["WITSELF_HOME"])
	if err != nil || witselfHome != cfg.MCPEnvironment["WITSELF_HOME"] {
		return errors.New("antigravity transaction WITSELF_HOME is invalid")
	}
	if cfg.RuntimePluginSource != filepath.Join(witselfHome, "integrations", transcriptcapture.RuntimeAntigravity, "bundles", cfg.RuntimePluginDigest) {
		return errors.New("antigravity transaction recovery source is invalid")
	}
	if len(cfg.RuntimePluginDigest) != 64 {
		return errors.New("antigravity transaction bundle digest is invalid")
	}
	if _, err := hex.DecodeString(cfg.RuntimePluginDigest); err != nil {
		return errors.New("antigravity transaction bundle digest is invalid")
	}
	return nil
}

func recoverAntigravityTransaction(configRoot string) error {
	journal, err := loadAntigravityTransactionJournal(configRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var binding *transcriptcapture.Config
	if journal.Desired != nil {
		binding = journal.Desired
	} else {
		binding = journal.Previous
	}
	if binding == nil {
		return errors.New("antigravity transaction journal has no binding")
	}
	if err := validateAntigravityTransactionConfig(configRoot, *binding); err != nil {
		return err
	}
	if journal.Previous != nil {
		if err := validateAntigravityTransactionConfig(configRoot, *journal.Previous); err != nil {
			return err
		}
	}
	switch journal.Operation {
	case antigravityTransactionInstall:
		if journal.Desired == nil {
			return errors.New("antigravity install journal has no desired binding")
		}
	case antigravityTransactionUninstall:
		if journal.Previous == nil || journal.Desired != nil {
			return errors.New("antigravity uninstall journal has an invalid binding shape")
		}
	default:
		return errors.New("unknown Antigravity transaction operation")
	}
	if journal.Previous != nil && journal.Desired != nil {
		previousServer, previousErr := antigravityMCPServerName(*journal.Previous)
		desiredServer, desiredErr := antigravityMCPServerName(*journal.Desired)
		if previousErr != nil || desiredErr != nil ||
			journal.Previous.RuntimeConfigRoot != journal.Desired.RuntimeConfigRoot ||
			journal.Previous.RuntimePluginPath != journal.Desired.RuntimePluginPath ||
			previousServer != desiredServer {
			return errors.New("antigravity transaction journal changes its config root or binding identity")
		}
	}
	currentHome, err := local.Home()
	if err != nil {
		return err
	}
	currentHome, err = cleanAntigravityAbsolutePath("WITSELF_HOME", currentHome)
	if err != nil {
		return err
	}
	if currentHome != binding.MCPEnvironment["WITSELF_HOME"] {
		return errors.New("restore the WITSELF_HOME used by the interrupted Antigravity transaction before retrying")
	}
	switch journal.Operation {
	case antigravityTransactionInstall:
		return recoverAntigravityInstallTransaction(configRoot, journal)
	case antigravityTransactionUninstall:
		return recoverAntigravityUninstallTransaction(configRoot, journal)
	}
	return errors.New("unknown Antigravity transaction operation")
}

func recoverAntigravityInstallTransaction(configRoot string, journal antigravityTransactionJournal) error {
	if journal.Desired == nil {
		return errors.New("antigravity install journal has no desired binding")
	}
	desired := *journal.Desired
	desiredBundle, err := verifiedAntigravitySourceBundle(desired)
	if err != nil {
		return fmt.Errorf("recover desired Antigravity bundle: %w", err)
	}
	_, currentState, err := currentAntigravityConfigState(journal.Previous, journal.Desired)
	if err != nil {
		return err
	}
	liveState := antigravityBundlePathState(desired.RuntimePluginPath, desiredBundle)
	var previousBundle antigravityPluginBundle
	if journal.Previous != nil {
		previousBundle, err = verifiedAntigravitySourceBundle(*journal.Previous)
		if err != nil {
			return fmt.Errorf("recover previous Antigravity bundle: %w", err)
		}
	}
	if liveState == antigravityPathForeign && journal.Previous != nil {
		if antigravityBundlePathState(desired.RuntimePluginPath, previousBundle) == antigravityPathExact {
			liveState = antigravityPathPrevious
		}
	}
	swapPath := antigravityBundleSwapPath(desired.RuntimePluginPath, desiredBundle)
	cleanupAfterClear := make([]transcriptcapture.Config, 0, 1)
	switch liveState {
	case antigravityPathExact:
		if currentState == antigravityConfigForeign {
			return errors.New("integration config changed during interrupted Antigravity install")
		}
		if err := verifyAntigravityBundleDirectory(desired.RuntimePluginSource, desiredBundle); err != nil {
			return fmt.Errorf("verify desired Antigravity recovery source: %w", err)
		}
		if err := transcriptcapture.SaveConfig(desired); err != nil {
			return err
		}
		if err := removeAntigravityRecoveryScratch(swapPath, desiredBundle, journal.Previous, previousBundle); err != nil {
			return err
		}
		if journal.Previous != nil && journal.Previous.RuntimePluginSource != desired.RuntimePluginSource {
			cleanupAfterClear = append(cleanupAfterClear, *journal.Previous)
		}
	case antigravityPathPrevious:
		if currentState == antigravityConfigForeign {
			return errors.New("integration config changed during interrupted Antigravity install")
		}
		if err := transcriptcapture.SaveConfig(*journal.Previous); err != nil {
			return err
		}
		if err := removeAntigravityRecoveryScratch(swapPath, desiredBundle, journal.Previous, previousBundle); err != nil {
			return err
		}
		if journal.Previous.RuntimePluginSource != desired.RuntimePluginSource {
			cleanupAfterClear = append(cleanupAfterClear, desired)
		}
	case antigravityPathMissing:
		if currentState == antigravityConfigForeign {
			return errors.New("integration config changed during interrupted Antigravity install")
		}
		if journal.Previous == nil {
			if err := transcriptcapture.RemoveConfig(transcriptcapture.RuntimeAntigravity); err != nil {
				return err
			}
			if err := removeAntigravityRecoveryScratch(swapPath, desiredBundle, nil, antigravityPluginBundle{}); err != nil {
				return err
			}
			cleanupAfterClear = append(cleanupAfterClear, desired)
		} else {
			if err := verifyAntigravityBundleDirectory(journal.Previous.RuntimePluginSource, previousBundle); err != nil {
				return fmt.Errorf("verify previous Antigravity recovery source: %w", err)
			}
			if err := installAntigravityBundleDirectory(journal.Previous.RuntimePluginPath, previousBundle, nil); err != nil {
				return err
			}
			if err := transcriptcapture.SaveConfig(*journal.Previous); err != nil {
				return err
			}
			if err := removeAntigravityRecoveryScratch(swapPath, desiredBundle, journal.Previous, previousBundle); err != nil {
				return err
			}
			if journal.Previous.RuntimePluginSource != desired.RuntimePluginSource {
				cleanupAfterClear = append(cleanupAfterClear, desired)
			}
		}
	default:
		return errors.New("installed Antigravity plugin changed during interrupted install; refusing automatic recovery")
	}
	if err := clearAntigravityTransaction(configRoot, journal); err != nil {
		return err
	}
	for _, obsolete := range cleanupAfterClear {
		_ = removeAntigravitySourceBundle(obsolete)
	}
	return nil
}

func recoverAntigravityUninstallTransaction(configRoot string, journal antigravityTransactionJournal) error {
	if journal.Previous == nil {
		return errors.New("antigravity uninstall journal has no installed binding")
	}
	previous := *journal.Previous
	bundle, err := verifiedAntigravitySourceBundle(previous)
	if err != nil {
		return fmt.Errorf("recover installed Antigravity bundle: %w", err)
	}
	_, configState, err := currentAntigravityConfigState(journal.Previous, nil)
	if err != nil {
		return err
	}
	liveState := antigravityBundlePathState(previous.RuntimePluginPath, bundle)
	removalPath := antigravityBundleRemovalPath(previous.RuntimePluginPath, bundle)
	removalState := antigravityBundlePathState(removalPath, bundle)
	if liveState == antigravityPathForeign {
		return errors.New("antigravity plugin changed during interrupted uninstall; refusing automatic recovery")
	}
	if removalState == antigravityPathForeign {
		if err := os.RemoveAll(removalPath); err != nil {
			return fmt.Errorf("clean interrupted Antigravity removal scratch: %w", err)
		}
		removalState = antigravityPathMissing
	}
	cleanupSourceAfterClear := false
	switch configState {
	case antigravityConfigPrevious:
		if liveState == antigravityPathMissing {
			if removalState == antigravityPathExact {
				if err := renameManagedInstructionFileNoReplace(removalPath, previous.RuntimePluginPath); err != nil {
					return err
				}
			} else {
				if err := verifyAntigravityBundleDirectory(previous.RuntimePluginSource, bundle); err != nil {
					return fmt.Errorf("verify installed Antigravity recovery source: %w", err)
				}
				if err := installAntigravityBundleDirectory(previous.RuntimePluginPath, bundle, nil); err != nil {
					return err
				}
			}
		} else if removalState == antigravityPathExact {
			if err := removeVerifiedAntigravityScratch(removalPath, bundle); err != nil {
				return err
			}
		}
	case antigravityConfigMissing:
		if liveState == antigravityPathExact {
			return errors.New("antigravity config is missing while its plugin is still live; refusing ambiguous recovery")
		}
		if removalState == antigravityPathExact {
			if err := removeVerifiedAntigravityScratch(removalPath, bundle); err != nil {
				return err
			}
		}
		cleanupSourceAfterClear = true
	default:
		return errors.New("integration config changed during interrupted Antigravity uninstall")
	}
	if err := clearAntigravityTransaction(configRoot, journal); err != nil {
		return err
	}
	if cleanupSourceAfterClear {
		_ = removeAntigravitySourceBundle(previous)
	}
	return nil
}

const (
	antigravityPathMissing  = "missing"
	antigravityPathExact    = "exact"
	antigravityPathPrevious = "previous"
	antigravityPathForeign  = "foreign"

	antigravityConfigMissing  = "missing"
	antigravityConfigDesired  = "desired"
	antigravityConfigPrevious = "previous"
	antigravityConfigForeign  = "foreign"
)

func antigravityBundlePathState(path string, bundle antigravityPluginBundle) string {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return antigravityPathMissing
	}
	if err := verifyAntigravityBundleDirectory(path, bundle); err == nil {
		return antigravityPathExact
	}
	return antigravityPathForeign
}

func currentAntigravityConfigState(previous, desired *transcriptcapture.Config) (transcriptcapture.Config, string, error) {
	current, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity)
	if errors.Is(err, os.ErrNotExist) {
		return transcriptcapture.Config{}, antigravityConfigMissing, nil
	}
	if err != nil {
		return transcriptcapture.Config{}, antigravityConfigForeign, err
	}
	if desired != nil && equalAntigravityTransactionConfig(current, *desired) {
		return current, antigravityConfigDesired, nil
	}
	if previous != nil && equalAntigravityTransactionConfig(current, *previous) {
		return current, antigravityConfigPrevious, nil
	}
	return current, antigravityConfigForeign, nil
}

func equalAntigravityTransactionConfig(first, second transcriptcapture.Config) bool {
	first.SchemaVersion = transcriptcapture.SchemaVersion
	second.SchemaVersion = transcriptcapture.SchemaVersion
	firstRaw, firstErr := json.Marshal(first)
	secondRaw, secondErr := json.Marshal(second)
	return firstErr == nil && secondErr == nil && bytes.Equal(firstRaw, secondRaw)
}

func removeAntigravityRecoveryScratch(path string, desired antigravityPluginBundle, previous *transcriptcapture.Config, previousBundle antigravityPluginBundle) error {
	state := antigravityBundlePathState(path, desired)
	if state == antigravityPathExact {
		return removeVerifiedAntigravityScratch(path, desired)
	}
	if state == antigravityPathMissing {
		return nil
	}
	if previous != nil && antigravityBundlePathState(path, previousBundle) == antigravityPathExact {
		return removeVerifiedAntigravityScratch(path, previousBundle)
	}
	// The journal and digest reserve this exact non-live scratch path. A hard
	// process stop may leave it partially populated or partially deleted; never
	// infer ownership for the live plugin path, but this scratch can be rebuilt.
	return os.RemoveAll(path)
}

func removeVerifiedAntigravityScratch(path string, bundle antigravityPluginBundle) error {
	if err := verifyAntigravityBundleDirectory(path, bundle); err != nil {
		return err
	}
	return os.RemoveAll(path)
}

func antigravityTransactionPending(cfg transcriptcapture.Config) error {
	if _, err := os.Lstat(antigravityTransactionPath(cfg.RuntimeConfigRoot)); err == nil {
		return errors.New("an interrupted Antigravity install or uninstall is pending; rerun witself install antigravity or witself uninstall antigravity to recover it")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
