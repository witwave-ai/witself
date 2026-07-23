package transcriptcapture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const managedOwnedRunnerPrefix = "witself-transcript-hook-"

const managedHookFileReadLimit = 8 * 1024 * 1024

// ManagedHookOwnership is the durable capability required to refresh, verify,
// or remove one administrator-managed hook installation. PolicyDigest covers
// only the Codex marker fragment (so unrelated requirements may evolve) and
// the complete dedicated Claude drop-in. RunnerDigest covers the exact runner
// bytes. No later operation derives either deletion target from mutable policy.
type ManagedHookOwnership struct {
	PolicyPath   string `json:"policy_path"`
	ManagedDir   string `json:"managed_dir"`
	RunnerPath   string `json:"runner_path"`
	RunnerDigest string `json:"runner_digest"`
	PolicyDigest string `json:"policy_digest"`
}

// InstallManagedHooksOwned installs a collision-resistant runner and returns
// the exact ownership capability that callers must persist. A non-nil previous
// capability is verified before any replacement; marker-shaped policy without
// one is never claimed.
func InstallManagedHooksOwned(opts ManagedHooksOptions, previous *ManagedHookOwnership) (ManagedHookOwnership, bool, error) {
	normalized, err := normalizeManagedHooksOptions(opts, true)
	if err != nil {
		return ManagedHookOwnership{}, false, err
	}
	opts = normalized
	if previous != nil {
		previousCopy := *previous
		if err := validateManagedHookOwnership(opts.Runtime, previousCopy); err != nil {
			return ManagedHookOwnership{}, false, fmt.Errorf("validate previous managed hook ownership: %w", err)
		}
		previous = &previousCopy
		setManagedPolicyPath(&opts, previous.PolicyPath)
		setManagedDirectory(&opts, previous.ManagedDir)
	}
	switch opts.Runtime {
	case RuntimeCodex:
		return installOwnedCodexManagedHooks(opts, previous)
	case RuntimeClaudeCode:
		return installOwnedClaudeManagedHooks(opts, previous)
	default:
		return ManagedHookOwnership{}, false, fmt.Errorf("unsupported runtime %q", opts.Runtime)
	}
}

// PlanManagedHooksOwned computes the exact ownership capability without
// mutating policy or runner files. Transactional callers persist this plan
// before invoking InstallManagedHooksOwned so a process killed immediately
// after the atomic hook commit can recover only the exact journaled binding.
func PlanManagedHooksOwned(opts ManagedHooksOptions, previous *ManagedHookOwnership) (ManagedHookOwnership, error) {
	normalized, err := normalizeManagedHooksOptions(opts, true)
	if err != nil {
		return ManagedHookOwnership{}, err
	}
	opts = normalized
	if previous != nil {
		previousCopy := *previous
		if err := validateManagedHookOwnership(opts.Runtime, previousCopy); err != nil {
			return ManagedHookOwnership{}, fmt.Errorf("validate previous managed hook ownership: %w", err)
		}
		previous = &previousCopy
		setManagedPolicyPath(&opts, previous.PolicyPath)
		setManagedDirectory(&opts, previous.ManagedDir)
	}
	var ownership ManagedHookOwnership
	switch opts.Runtime {
	case RuntimeCodex:
		_, _, _, ownership, err = prepareOwnedCodexManagedHooks(opts, previous)
	case RuntimeClaudeCode:
		_, _, _, ownership, err = prepareOwnedClaudeManagedHooks(opts, previous)
	default:
		err = fmt.Errorf("unsupported runtime %q", opts.Runtime)
	}
	if err != nil {
		return ManagedHookOwnership{}, err
	}
	if err := preflightOwnedManagedRunnerTarget(ownership, previous); err != nil {
		return ManagedHookOwnership{}, err
	}
	return ownership, nil
}

// RemoveManagedHooksOwned removes only the exact persisted policy fragment and
// runner. It never consults the policy's current managed_dir to choose a target.
func RemoveManagedHooksOwned(runtimeName string, ownership ManagedHookOwnership) (bool, error) {
	runtimeName, err := NormalizeRuntime(runtimeName)
	if err != nil {
		return false, err
	}
	if err := validateManagedHookOwnership(runtimeName, ownership); err != nil {
		return false, err
	}
	policySnapshot, err := readManagedFileSnapshot(ownership.PolicyPath)
	if err != nil {
		return false, err
	}
	runnerSnapshot, err := readManagedFileSnapshot(ownership.RunnerPath)
	if err != nil {
		return false, err
	}
	policyRaw, policyExists := policySnapshot.raw, policySnapshot.exists
	runnerRaw, runnerExists := runnerSnapshot.raw, runnerSnapshot.exists
	if !policyExists && !runnerExists {
		return false, nil
	}
	if !policyExists || !runnerExists {
		return false, errors.New("managed hook policy and runner presence diverged; refusing partial removal")
	}
	if digestManagedBytes(runnerRaw) != ownership.RunnerDigest {
		return false, fmt.Errorf("managed hook runner %s differs from the persisted digest", ownership.RunnerPath)
	}

	var base []byte
	switch runtimeName {
	case RuntimeCodex:
		base, _, fragment, splitErr := splitCodexManagedBlockExact(policyRaw)
		if splitErr != nil {
			return false, splitErr
		}
		if digestManagedBytes(fragment) != ownership.PolicyDigest {
			return false, errors.New("the Codex managed hook fragment differs from the persisted digest")
		}
		if _, parseErr := parseRequirements(base, ownership.PolicyPath); parseErr != nil {
			return false, parseErr
		}
	case RuntimeClaudeCode:
		if digestManagedBytes(policyRaw) != ownership.PolicyDigest {
			return false, errors.New("the Claude managed hook drop-in differs from the persisted digest")
		}
	default:
		return false, fmt.Errorf("unsupported runtime %q", runtimeName)
	}

	runOwnedManagedHookBeforeMutationForTest(ownership.PolicyPath)
	if err := verifyManagedFileSnapshot(ownership.PolicyPath, policySnapshot); err != nil {
		return false, err
	}
	if err := verifyManagedFileSnapshot(ownership.RunnerPath, runnerSnapshot); err != nil {
		return false, err
	}
	if runtimeName == RuntimeCodex && len(bytes.TrimSpace(base)) != 0 {
		if err := writeManagedFileAtomic(ownership.PolicyPath, base, 0o644); err != nil {
			return false, err
		}
	} else if err := os.Remove(ownership.PolicyPath); err != nil {
		return false, err
	}
	committedPolicy, err := readManagedFileSnapshot(ownership.PolicyPath)
	if err != nil {
		return true, fmt.Errorf("inspect committed managed policy removal: %w", err)
	}
	runOwnedManagedHookBeforeMutationForTest(ownership.RunnerPath)
	if err := verifyManagedFileSnapshot(ownership.RunnerPath, runnerSnapshot); err != nil {
		err = fmt.Errorf("managed hook runner changed before removal: %w", err)
		if restoreErr := restoreManagedPolicySnapshot(ownership.PolicyPath, committedPolicy, policySnapshot); restoreErr != nil {
			return true, errors.Join(err, fmt.Errorf("restore managed policy after runner drift: %w", restoreErr))
		}
		return false, err
	}
	if err := os.Remove(ownership.RunnerPath); err != nil {
		// Restore the exact policy if runner cleanup could not complete. The
		// caller retains the ownership record and can retry without ambiguity.
		if restoreErr := restoreManagedPolicySnapshot(ownership.PolicyPath, committedPolicy, policySnapshot); restoreErr != nil {
			return true, errors.Join(err, fmt.Errorf("restore managed policy after runner removal failure: %w", restoreErr))
		}
		return false, err
	}
	_ = os.Remove(ownership.ManagedDir)
	if runtimeName == RuntimeClaudeCode {
		_ = os.Remove(filepath.Dir(ownership.PolicyPath))
	}
	return true, nil
}

// VerifyManagedHooksOwned verifies both exact digests and the pinned paths.
func VerifyManagedHooksOwned(runtimeName string, ownership ManagedHookOwnership) error {
	runtimeName, err := NormalizeRuntime(runtimeName)
	if err != nil {
		return err
	}
	if err := validateManagedHookOwnership(runtimeName, ownership); err != nil {
		return err
	}
	policyRaw, policyExists, err := readManagedRegularFile(ownership.PolicyPath)
	if err != nil {
		return err
	}
	runnerRaw, runnerExists, err := readManagedRegularFile(ownership.RunnerPath)
	if err != nil {
		return err
	}
	if !policyExists || !runnerExists {
		return errors.New("managed hook policy or runner is missing")
	}
	if digestManagedBytes(runnerRaw) != ownership.RunnerDigest {
		return errors.New("managed hook runner differs from the persisted digest")
	}
	switch runtimeName {
	case RuntimeCodex:
		_, found, fragment, err := splitCodexManagedBlockExact(policyRaw)
		if err != nil {
			return err
		}
		if !found || digestManagedBytes(fragment) != ownership.PolicyDigest {
			return errors.New("the Codex managed hook fragment differs from the persisted digest")
		}
		full, err := parseRequirements(policyRaw, ownership.PolicyPath)
		if err != nil {
			return err
		}
		hooks, _ := full["hooks"].(map[string]any)
		if hooks == nil || hooks["managed_dir"] != ownership.ManagedDir {
			return errors.New("the Codex managed_dir differs from the persisted hook ownership")
		}
	case RuntimeClaudeCode:
		if digestManagedBytes(policyRaw) != ownership.PolicyDigest {
			return errors.New("the Claude managed hook drop-in differs from the persisted digest")
		}
	default:
		return fmt.Errorf("unsupported runtime %q", runtimeName)
	}
	return nil
}

// ReconstructLegacyManagedHookOwnership narrowly adopts the former
// transcript-hook layout only when the complete generated policy and runner
// exactly match the supplied durable integration identity.
func ReconstructLegacyManagedHookOwnership(opts ManagedHooksOptions) (ManagedHookOwnership, error) {
	normalized, err := normalizeManagedHooksOptions(opts, true)
	if err != nil {
		return ManagedHookOwnership{}, err
	}
	opts = normalized
	switch opts.Runtime {
	case RuntimeCodex:
		raw, exists, err := readManagedRegularFile(opts.CodexRequirementsPath)
		if err != nil {
			return ManagedHookOwnership{}, err
		}
		if !exists {
			return ManagedHookOwnership{}, os.ErrNotExist
		}
		base, found, fragment, err := splitCodexManagedBlockExact(raw)
		if err != nil {
			return ManagedHookOwnership{}, err
		}
		if !found {
			return ManagedHookOwnership{}, os.ErrNotExist
		}
		full, err := parseRequirements(raw, opts.CodexRequirementsPath)
		if err != nil {
			return ManagedHookOwnership{}, err
		}
		hooks, _ := full["hooks"].(map[string]any)
		managedDir, _ := hooks["managed_dir"].(string)
		if !filepath.IsAbs(managedDir) {
			return ManagedHookOwnership{}, errors.New("the Codex legacy managed_dir is invalid")
		}
		runnerPath := filepath.Join(managedDir, managedRunnerName)
		matched := false
		for _, includeFeatures := range []bool{false, true} {
			for _, includeHooks := range []bool{false, true} {
				want := codexManagedFragment(
					opts.Runtime, opts.Mode, opts.Account, opts.Realm, opts.Agent,
					opts.Location, opts.WitselfHome, managedDir, runnerPath,
					includeFeatures, includeHooks,
				)
				if bytes.Equal(fragment, want) {
					matched = true
				}
			}
		}
		if !matched {
			return ManagedHookOwnership{}, errors.New("the Codex legacy managed hook fragment does not exactly match the persisted integration identity")
		}
		if _, err := parseRequirements(base, opts.CodexRequirementsPath); err != nil {
			return ManagedHookOwnership{}, err
		}
		return legacyManagedOwnership(opts.CodexRequirementsPath, managedDir, runnerPath, fragment, opts.Executable)
	case RuntimeClaudeCode:
		raw, exists, err := readManagedRegularFile(opts.ClaudeSettingsPath)
		if err != nil {
			return ManagedHookOwnership{}, err
		}
		if !exists {
			return ManagedHookOwnership{}, os.ErrNotExist
		}
		runnerPath := filepath.Join(opts.ClaudeManagedDir, managedRunnerName)
		want := claudeManagedPolicy(opts, runnerPath)
		if !bytes.Equal(raw, want) {
			return ManagedHookOwnership{}, errors.New("the Claude legacy managed drop-in does not exactly match the persisted integration identity")
		}
		return legacyManagedOwnership(opts.ClaudeSettingsPath, opts.ClaudeManagedDir, runnerPath, raw, opts.Executable)
	default:
		return ManagedHookOwnership{}, fmt.Errorf("unsupported runtime %q", opts.Runtime)
	}
}

func installOwnedCodexManagedHooks(opts ManagedHooksOptions, previous *ManagedHookOwnership) (ManagedHookOwnership, bool, error) {
	policySnapshot, combined, runnerRaw, ownership, err := prepareOwnedCodexManagedHooks(opts, previous)
	if err != nil {
		return ManagedHookOwnership{}, false, err
	}
	return commitOwnedManagedHooks(opts.Runtime, policySnapshot, combined, runnerRaw, ownership, previous)
}

func prepareOwnedCodexManagedHooks(
	opts ManagedHooksOptions,
	previous *ManagedHookOwnership,
) (managedFileSnapshot, []byte, []byte, ManagedHookOwnership, error) {
	policyPath := opts.CodexRequirementsPath
	policySnapshot, err := readManagedFileSnapshot(policyPath)
	if err != nil {
		return managedFileSnapshot{}, nil, nil, ManagedHookOwnership{}, err
	}
	raw := policySnapshot.raw
	base := raw
	if previous == nil {
		_, found, _, err := splitCodexManagedBlockExact(raw)
		if err != nil {
			return managedFileSnapshot{}, nil, nil, ManagedHookOwnership{}, err
		}
		if found {
			return managedFileSnapshot{}, nil, nil, ManagedHookOwnership{}, errors.New("the Codex policy already contains a Witself marker without a durable ownership record")
		}
	} else {
		var found bool
		var fragment []byte
		base, found, fragment, err = splitCodexManagedBlockExact(raw)
		if err != nil {
			return managedFileSnapshot{}, nil, nil, ManagedHookOwnership{}, err
		}
		if !found || digestManagedBytes(fragment) != previous.PolicyDigest {
			return managedFileSnapshot{}, nil, nil, ManagedHookOwnership{}, errors.New("the Codex managed hook fragment differs from the persisted prior ownership")
		}
		if err := verifyExactManagedRunner(*previous); err != nil {
			return managedFileSnapshot{}, nil, nil, ManagedHookOwnership{}, err
		}
	}
	root, err := parseRequirements(base, policyPath)
	if err != nil {
		return managedFileSnapshot{}, nil, nil, ManagedHookOwnership{}, err
	}
	managedDir := opts.CodexManagedDir
	includeHooksTable := true
	if rawHooks, ok := root["hooks"]; ok {
		hooks, ok := rawHooks.(map[string]any)
		if !ok {
			return managedFileSnapshot{}, nil, nil, ManagedHookOwnership{}, errors.New("existing Codex hooks policy is not a table")
		}
		existingDir, _ := hooks["managed_dir"].(string)
		if !filepath.IsAbs(existingDir) {
			return managedFileSnapshot{}, nil, nil, ManagedHookOwnership{}, errors.New("existing Codex hooks policy must define an absolute managed_dir")
		}
		if previous != nil && existingDir != previous.ManagedDir {
			return managedFileSnapshot{}, nil, nil, ManagedHookOwnership{}, errors.New("the Codex managed_dir changed from the persisted hook ownership")
		}
		managedDir = existingDir
		includeHooksTable = false
	}
	if previous != nil {
		managedDir = previous.ManagedDir
	}
	includeFeaturesTable := true
	if rawFeatures, ok := root["features"]; ok {
		includeFeaturesTable = false
		features, ok := rawFeatures.(map[string]any)
		if !ok {
			return managedFileSnapshot{}, nil, nil, ManagedHookOwnership{}, errors.New("existing Codex features policy is not a table")
		}
		if enabled, ok := features["hooks"].(bool); ok && !enabled {
			return managedFileSnapshot{}, nil, nil, ManagedHookOwnership{}, errors.New("existing Codex policy disables hooks")
		}
	}
	runnerRaw := managedRunnerBytes(opts.Executable)
	runnerPath := ownedManagedRunnerPath(opts, managedDir, runnerRaw)
	fragment := codexManagedFragment(
		opts.Runtime, opts.Mode, opts.Account, opts.Realm, opts.Agent, opts.Location,
		opts.WitselfHome, managedDir, runnerPath, includeFeaturesTable, includeHooksTable,
	)
	combined := appendManagedFragment(base, fragment)
	if _, err := parseRequirements(combined, policyPath); err != nil {
		return managedFileSnapshot{}, nil, nil, ManagedHookOwnership{}, fmt.Errorf("validate merged Codex requirements: %w", err)
	}
	ownership := ManagedHookOwnership{
		PolicyPath: policyPath, ManagedDir: managedDir, RunnerPath: runnerPath,
		RunnerDigest: digestManagedBytes(runnerRaw), PolicyDigest: digestManagedBytes(fragment),
	}
	return policySnapshot, combined, runnerRaw, ownership, nil
}

func installOwnedClaudeManagedHooks(opts ManagedHooksOptions, previous *ManagedHookOwnership) (ManagedHookOwnership, bool, error) {
	policySnapshot, policyRaw, runnerRaw, ownership, err := prepareOwnedClaudeManagedHooks(opts, previous)
	if err != nil {
		return ManagedHookOwnership{}, false, err
	}
	return commitOwnedManagedHooks(opts.Runtime, policySnapshot, policyRaw, runnerRaw, ownership, previous)
}

func prepareOwnedClaudeManagedHooks(
	opts ManagedHooksOptions,
	previous *ManagedHookOwnership,
) (managedFileSnapshot, []byte, []byte, ManagedHookOwnership, error) {
	policyPath := opts.ClaudeSettingsPath
	policySnapshot, err := readManagedFileSnapshot(policyPath)
	if err != nil {
		return managedFileSnapshot{}, nil, nil, ManagedHookOwnership{}, err
	}
	raw, policyExists := policySnapshot.raw, policySnapshot.exists
	if previous == nil {
		if policyExists {
			return managedFileSnapshot{}, nil, nil, ManagedHookOwnership{}, errors.New("the Claude Witself managed drop-in already exists without a durable ownership record")
		}
	} else {
		if !policyExists || digestManagedBytes(raw) != previous.PolicyDigest {
			return managedFileSnapshot{}, nil, nil, ManagedHookOwnership{}, errors.New("the Claude managed hook drop-in differs from the persisted prior ownership")
		}
		if err := verifyExactManagedRunner(*previous); err != nil {
			return managedFileSnapshot{}, nil, nil, ManagedHookOwnership{}, err
		}
	}
	managedDir := opts.ClaudeManagedDir
	if previous != nil {
		managedDir = previous.ManagedDir
	}
	runnerRaw := managedRunnerBytes(opts.Executable)
	runnerPath := ownedManagedRunnerPath(opts, managedDir, runnerRaw)
	policyRaw := claudeManagedPolicy(opts, runnerPath)
	ownership := ManagedHookOwnership{
		PolicyPath: policyPath, ManagedDir: managedDir, RunnerPath: runnerPath,
		RunnerDigest: digestManagedBytes(runnerRaw), PolicyDigest: digestManagedBytes(policyRaw),
	}
	return policySnapshot, policyRaw, runnerRaw, ownership, nil
}

func preflightOwnedManagedRunnerTarget(ownership ManagedHookOwnership, previous *ManagedHookOwnership) error {
	if previous == nil {
		return rejectUnownedManagedRunners(ownership.ManagedDir, ownership.RunnerPath)
	}
	if ownership.RunnerPath == previous.RunnerPath {
		return nil
	}
	if _, exists, err := readManagedRegularFile(ownership.RunnerPath); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("desired managed hook runner %s already exists outside the persisted prior ownership", ownership.RunnerPath)
	}
	return nil
}

func commitOwnedManagedHooks(
	runtimeName string,
	policySnapshot managedFileSnapshot,
	desiredPolicy, desiredRunner []byte,
	ownership ManagedHookOwnership,
	previous *ManagedHookOwnership,
) (ManagedHookOwnership, bool, error) {
	priorPolicy := policySnapshot.raw
	if err := preflightOwnedManagedRunnerTarget(ownership, previous); err != nil {
		return ManagedHookOwnership{}, false, err
	}
	runnerSnapshot, err := readManagedFileSnapshot(ownership.RunnerPath)
	if err != nil {
		return ManagedHookOwnership{}, false, err
	}
	currentRunner, currentRunnerExists := runnerSnapshot.raw, runnerSnapshot.exists
	if currentRunnerExists && (previous == nil || ownership.RunnerPath != previous.RunnerPath) {
		return ManagedHookOwnership{}, false, fmt.Errorf("managed hook runner %s already exists without matching prior ownership", ownership.RunnerPath)
	}
	if bytes.Equal(priorPolicy, desiredPolicy) && currentRunnerExists && bytes.Equal(currentRunner, desiredRunner) &&
		(previous == nil || ownership.RunnerPath == previous.RunnerPath) {
		return ownership, false, nil
	}

	runOwnedManagedHookBeforeMutationForTest(ownership.RunnerPath)
	if err := verifyManagedFileSnapshot(ownership.RunnerPath, runnerSnapshot); err != nil {
		return ManagedHookOwnership{}, false, err
	}
	if err := verifyManagedFileSnapshot(ownership.PolicyPath, policySnapshot); err != nil {
		return ManagedHookOwnership{}, false, err
	}
	if err := writeManagedFileAtomic(ownership.RunnerPath, desiredRunner, 0o755); err != nil {
		return ManagedHookOwnership{}, false, err
	}
	committedRunner, err := readManagedFileSnapshot(ownership.RunnerPath)
	if err != nil {
		return ownership, true, fmt.Errorf("inspect committed managed hook runner: %w", err)
	}
	runOwnedManagedHookBeforeMutationForTest(ownership.PolicyPath)
	if err := verifyManagedFileSnapshot(ownership.PolicyPath, policySnapshot); err != nil {
		restoreErr := restoreManagedRunnerSnapshot(ownership.RunnerPath, committedRunner, runnerSnapshot)
		if restoreErr == nil {
			return ManagedHookOwnership{}, false, err
		}
		return ownership, true, errors.Join(err, restoreErr)
	}
	if err := verifyManagedFileSnapshot(ownership.RunnerPath, committedRunner); err != nil {
		restoreErr := restoreManagedRunnerSnapshot(ownership.RunnerPath, committedRunner, runnerSnapshot)
		if restoreErr == nil {
			return ManagedHookOwnership{}, false, fmt.Errorf("managed hook runner changed before policy commit: %w", err)
		}
		return ownership, true, errors.Join(
			fmt.Errorf("managed hook runner changed before policy commit: %w", err),
			restoreErr,
		)
	}
	if err := writeManagedFileAtomic(ownership.PolicyPath, desiredPolicy, 0o644); err != nil {
		restoreErr := restoreManagedRunnerSnapshot(ownership.RunnerPath, committedRunner, runnerSnapshot)
		if restoreErr == nil {
			return ManagedHookOwnership{}, false, err
		}
		return ownership, true, errors.Join(err, restoreErr)
	}
	committedPolicy, err := readManagedFileSnapshot(ownership.PolicyPath)
	if err != nil {
		return ownership, true, fmt.Errorf("inspect committed managed hook policy: %w", err)
	}
	if previous != nil && previous.RunnerPath != ownership.RunnerPath {
		runOwnedManagedHookBeforeMutationForTest(previous.RunnerPath)
		if err := removeExactManagedRunner(*previous); err != nil {
			restorePolicyErr := restoreManagedPolicySnapshot(ownership.PolicyPath, committedPolicy, policySnapshot)
			removeNewErr := removeExactManagedRunner(ownership)
			if restorePolicyErr == nil && removeNewErr == nil {
				return ManagedHookOwnership{}, false, fmt.Errorf("remove superseded managed hook runner: %w", err)
			}
			return ownership, true, errors.Join(
				fmt.Errorf("remove superseded managed hook runner: %w", err),
				restorePolicyErr,
				removeNewErr,
			)
		}
		_ = os.Remove(previous.ManagedDir)
	}
	if err := VerifyManagedHooksOwned(runtimeName, ownership); err != nil {
		return ownership, true, fmt.Errorf("verify managed hook commit: %w", err)
	}
	return ownership, true, nil
}

func legacyManagedOwnership(policyPath, managedDir, runnerPath string, policyRaw []byte, executable string) (ManagedHookOwnership, error) {
	runnerRaw, exists, err := readManagedRegularFile(runnerPath)
	if err != nil {
		return ManagedHookOwnership{}, err
	}
	if !exists || !bytes.Equal(runnerRaw, managedRunnerBytes(executable)) {
		return ManagedHookOwnership{}, errors.New("legacy managed hook runner does not exactly match the persisted executable")
	}
	return ManagedHookOwnership{
		PolicyPath: policyPath, ManagedDir: managedDir, RunnerPath: runnerPath,
		RunnerDigest: digestManagedBytes(runnerRaw), PolicyDigest: digestManagedBytes(policyRaw),
	}, nil
}

func validateManagedHookOwnership(runtimeName string, ownership ManagedHookOwnership) error {
	for name, path := range map[string]string{
		"policy_path": ownership.PolicyPath,
		"managed_dir": ownership.ManagedDir,
		"runner_path": ownership.RunnerPath,
	} {
		if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
			return fmt.Errorf("%s must be a clean absolute path", name)
		}
	}
	if filepath.Dir(ownership.RunnerPath) != ownership.ManagedDir {
		return errors.New("managed hook runner_path must be inside managed_dir")
	}
	runnerName := filepath.Base(ownership.RunnerPath)
	if runnerName != managedRunnerName &&
		(!strings.HasPrefix(runnerName, managedOwnedRunnerPrefix) || len(strings.TrimPrefix(runnerName, managedOwnedRunnerPrefix)) != 24) {
		return errors.New("managed hook runner has an invalid owned name")
	}
	for name, digest := range map[string]string{
		"runner_digest": ownership.RunnerDigest,
		"policy_digest": ownership.PolicyDigest,
	} {
		if len(digest) != 64 {
			return fmt.Errorf("%s must be a lowercase SHA-256 digest", name)
		}
		if decoded, err := hex.DecodeString(digest); err != nil || hex.EncodeToString(decoded) != digest {
			return fmt.Errorf("%s must be a lowercase SHA-256 digest", name)
		}
	}
	switch runtimeName {
	case RuntimeCodex, RuntimeClaudeCode:
		return nil
	default:
		return fmt.Errorf("runtime %q does not support managed hook ownership", runtimeName)
	}
}

func normalizeManagedHooksOptions(opts ManagedHooksOptions, requireExecutable bool) (ManagedHooksOptions, error) {
	var err error
	opts.Runtime, err = NormalizeRuntime(opts.Runtime)
	if err != nil {
		return ManagedHooksOptions{}, err
	}
	opts.Mode, err = NormalizeMode(opts.Mode)
	if err != nil {
		return ManagedHooksOptions{}, err
	}
	opts.Executable = strings.TrimSpace(opts.Executable)
	opts.Account = strings.TrimSpace(opts.Account)
	opts.Realm = strings.TrimSpace(opts.Realm)
	opts.Agent = strings.TrimSpace(opts.Agent)
	opts.Location = strings.TrimSpace(opts.Location)
	opts.WitselfHome = strings.TrimSpace(opts.WitselfHome)
	if requireExecutable {
		if !filepath.IsAbs(opts.Executable) || filepath.Clean(opts.Executable) != opts.Executable {
			return ManagedHooksOptions{}, errors.New("managed hook executable must be a clean absolute path")
		}
		info, err := os.Stat(opts.Executable)
		if err != nil {
			return ManagedHooksOptions{}, fmt.Errorf("stat managed hook executable: %w", err)
		}
		if !info.Mode().IsRegular() {
			return ManagedHooksOptions{}, errors.New("managed hook executable must be a regular file")
		}
	}
	if opts.Account == "" || opts.Realm == "" || opts.Agent == "" {
		return ManagedHooksOptions{}, errors.New("managed hook account, realm, and agent are required")
	}
	if opts.Location != "" && !locationNamePattern.MatchString(opts.Location) {
		return ManagedHooksOptions{}, fmt.Errorf("invalid managed hook location %q", opts.Location)
	}
	if opts.WitselfHome != "" && (!filepath.IsAbs(opts.WitselfHome) || filepath.Clean(opts.WitselfHome) != opts.WitselfHome || strings.ContainsAny(opts.WitselfHome, "\x00\r\n")) {
		return ManagedHooksOptions{}, errors.New("managed hook WITSELF_HOME must be a clean absolute path")
	}
	return opts, nil
}

func setManagedPolicyPath(opts *ManagedHooksOptions, path string) {
	if opts.Runtime == RuntimeCodex {
		opts.CodexRequirementsPath = path
	} else {
		opts.ClaudeSettingsPath = path
	}
}

func setManagedDirectory(opts *ManagedHooksOptions, path string) {
	if opts.Runtime == RuntimeCodex {
		opts.CodexManagedDir = path
	} else {
		opts.ClaudeManagedDir = path
	}
}

func ownedManagedRunnerPath(opts ManagedHooksOptions, managedDir string, runnerRaw []byte) string {
	identity := strings.Join([]string{
		opts.Runtime, opts.Mode, opts.Executable, opts.Account, opts.Realm,
		opts.Agent, opts.Location, opts.WitselfHome, digestManagedBytes(runnerRaw),
	}, "\x00")
	sum := sha256.Sum256([]byte(identity))
	return filepath.Join(managedDir, managedOwnedRunnerPrefix+hex.EncodeToString(sum[:12]))
}

func managedRunnerBytes(executable string) []byte {
	return []byte("#!/bin/sh\nexec " + shellQuote(executable) + " transcript hook \"$@\"\n")
}

func claudeManagedPolicy(opts ManagedHooksOptions, runnerPath string) []byte {
	hooks := map[string]any{}
	command := shellQuote(runnerPath) + " " +
		hookBindingArgsWithWitselfHome(opts.Runtime, opts.Account, opts.Realm, opts.Agent, opts.Location, opts.WitselfHome)
	addWitselfHandlers(hooks, opts.Runtime, opts.Mode, command)
	root := map[string]any{
		"$schema": "https://json.schemastore.org/claude-code-settings.json",
		"hooks":   hooks,
	}
	raw, _ := json.MarshalIndent(root, "", "  ")
	return append(raw, '\n')
}

func splitCodexManagedBlockExact(raw []byte) ([]byte, bool, []byte, error) {
	start := bytes.Index(raw, []byte(codexManagedBlockBegin))
	base, found, err := stripCodexManagedBlock(raw)
	if err != nil || !found {
		return base, found, nil, err
	}
	return base, true, bytes.Clone(raw[start:]), nil
}

func digestManagedBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type managedFileSnapshot struct {
	exists bool
	raw    []byte
	info   os.FileInfo
}

func readManagedFileSnapshot(path string) (managedFileSnapshot, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return managedFileSnapshot{}, nil
	}
	if err != nil {
		return managedFileSnapshot{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return managedFileSnapshot{}, fmt.Errorf("managed hook path %s must be a real regular file", path)
	}
	if info.Size() > managedHookFileReadLimit {
		return managedFileSnapshot{}, fmt.Errorf("managed hook path %s exceeds %d bytes", path, managedHookFileReadLimit)
	}
	file, err := os.Open(path)
	if err != nil {
		return managedFileSnapshot{}, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return managedFileSnapshot{}, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return managedFileSnapshot{}, fmt.Errorf("managed hook path %s changed while opening", path)
	}
	raw, err := io.ReadAll(io.LimitReader(file, managedHookFileReadLimit+1))
	closeErr := file.Close()
	if err != nil {
		return managedFileSnapshot{}, err
	}
	if closeErr != nil {
		return managedFileSnapshot{}, closeErr
	}
	if len(raw) > managedHookFileReadLimit {
		return managedFileSnapshot{}, fmt.Errorf("managed hook path %s exceeds %d bytes", path, managedHookFileReadLimit)
	}
	after, err := os.Lstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() ||
		!os.SameFile(openedInfo, after) || after.Size() != int64(len(raw)) {
		if err == nil {
			err = errors.New("file identity changed")
		}
		return managedFileSnapshot{}, fmt.Errorf("managed hook path %s changed while reading: %w", path, err)
	}
	return managedFileSnapshot{exists: true, raw: raw, info: info}, nil
}

func verifyManagedFileSnapshot(path string, expected managedFileSnapshot) error {
	current, err := readManagedFileSnapshot(path)
	if err != nil {
		return err
	}
	if current.exists != expected.exists {
		return fmt.Errorf("managed hook path %s changed concurrently; refusing to overwrite it", path)
	}
	if !current.exists {
		return nil
	}
	if !os.SameFile(current.info, expected.info) || !bytes.Equal(current.raw, expected.raw) {
		return fmt.Errorf("managed hook path %s changed concurrently; refusing to overwrite it", path)
	}
	return nil
}

func restoreManagedPolicySnapshot(path string, committed, prior managedFileSnapshot) error {
	if err := verifyManagedFileSnapshot(path, committed); err != nil {
		return fmt.Errorf("managed policy changed after Witself's mutation; preserving the later value: %w", err)
	}
	if prior.exists {
		return writeManagedFileAtomic(path, prior.raw, 0o644)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func readManagedRegularFile(path string) ([]byte, bool, error) {
	snapshot, err := readManagedFileSnapshot(path)
	return snapshot.raw, snapshot.exists, err
}

func verifyExactManagedRunner(ownership ManagedHookOwnership) error {
	raw, exists, err := readManagedRegularFile(ownership.RunnerPath)
	if err != nil {
		return err
	}
	if !exists || digestManagedBytes(raw) != ownership.RunnerDigest {
		return fmt.Errorf("managed hook runner %s differs from the persisted prior ownership", ownership.RunnerPath)
	}
	return nil
}

func removeExactManagedRunner(ownership ManagedHookOwnership) error {
	if err := verifyExactManagedRunner(ownership); err != nil {
		return err
	}
	return os.Remove(ownership.RunnerPath)
}

func rejectUnownedManagedRunners(managedDir, desiredPath string) error {
	entries, err := os.ReadDir(managedDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == managedRunnerName || strings.HasPrefix(name, managedOwnedRunnerPrefix) {
			path := filepath.Join(managedDir, name)
			if path != desiredPath {
				return fmt.Errorf("managed hook directory contains unowned runner %s", path)
			}
			return fmt.Errorf("desired managed hook runner %s already exists without a durable ownership record", path)
		}
	}
	return nil
}

func restoreManagedRunnerSnapshot(path string, committed, prior managedFileSnapshot) error {
	if err := verifyManagedFileSnapshot(path, committed); err != nil {
		return fmt.Errorf("managed runner changed after Witself's mutation; preserving the later value: %w", err)
	}
	if prior.exists {
		return writeManagedFileAtomic(path, prior.raw, 0o755)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ownedManagedHookBeforeMutationForTest lets deterministic tests replace one
// managed policy or runner immediately before its exact snapshot is
// revalidated. Production code leaves it nil.
var ownedManagedHookBeforeMutationForTest func(string)

func runOwnedManagedHookBeforeMutationForTest(path string) {
	if ownedManagedHookBeforeMutationForTest != nil {
		ownedManagedHookBeforeMutationForTest(path)
	}
}
