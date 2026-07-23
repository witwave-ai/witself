package main

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

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

type antigravitySharedMCPDocument struct {
	path    string
	exists  bool
	raw     []byte
	mode    os.FileMode
	root    map[string]json.RawMessage
	servers map[string]json.RawMessage
}

const antigravitySharedMCPMutationSchema = "witself.antigravity-shared-mcp-mutation.v1"

type antigravitySharedMCPFingerprint struct {
	Exists bool   `json:"exists"`
	Mode   uint32 `json:"mode,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

type antigravitySharedMCPMutation struct {
	SchemaVersion string                          `json:"schema_version"`
	JournalID     string                          `json:"journal_id"`
	CanonicalPath string                          `json:"canonical_path"`
	Preimage      antigravitySharedMCPFingerprint `json:"preimage"`
	Candidate     antigravitySharedMCPFingerprint `json:"candidate"`
}

func antigravityCanonicalSharedMCPPath(cfg transcriptcapture.Config) (string, error) {
	expected := filepath.Join(cfg.RuntimeConfigRoot, "mcp_config.json")
	if cfg.RuntimeMCPConfigPath != "" && cfg.RuntimeMCPConfigPath != expected {
		return "", errors.New("antigravity shared MCP config path is not canonical")
	}
	return expected, nil
}

func antigravitySharedMCPTransitionIdentity(previous, desired *transcriptcapture.Config) (string, string, error) {
	reference := desired
	if reference == nil {
		reference = previous
	}
	if reference == nil {
		return "", "", errors.New("antigravity shared MCP transition has no binding")
	}
	path, err := antigravityCanonicalSharedMCPPath(*reference)
	if err != nil {
		return "", "", err
	}
	name, err := antigravityMCPServerName(*reference)
	if err != nil {
		return "", "", err
	}
	for _, cfg := range []*transcriptcapture.Config{previous, desired} {
		if cfg == nil {
			continue
		}
		candidatePath, candidateErr := antigravityCanonicalSharedMCPPath(*cfg)
		candidateName, nameErr := antigravityMCPServerName(*cfg)
		if candidateErr != nil || nameErr != nil || candidatePath != path || candidateName != name {
			return "", "", errors.New("antigravity shared MCP transition changes its binding identity")
		}
	}
	return path, name, nil
}

func readAntigravitySharedMCPDocument(path string) (antigravitySharedMCPDocument, error) {
	document := antigravitySharedMCPDocument{
		path: path, root: map[string]json.RawMessage{}, servers: map[string]json.RawMessage{},
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return document, nil
	}
	if err != nil {
		return document, fmt.Errorf("inspect Antigravity shared MCP config: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return document, errors.New("antigravity shared MCP config must be a real regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return document, fmt.Errorf("read Antigravity shared MCP config: %w", err)
	}
	document.exists = true
	document.raw = raw
	document.mode = info.Mode().Perm()
	if len(bytes.TrimSpace(raw)) == 0 {
		return document, nil
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return document, fmt.Errorf("parse Antigravity shared MCP config %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &document.root); err != nil || document.root == nil {
		if err == nil {
			err = errors.New("root must be a JSON object")
		}
		return document, fmt.Errorf("parse Antigravity shared MCP config %s: %w", path, err)
	}
	serversRaw, ok := document.root["mcpServers"]
	if !ok {
		return document, nil
	}
	if err := json.Unmarshal(serversRaw, &document.servers); err != nil || document.servers == nil {
		if err == nil {
			err = errors.New("mcpServers must be a JSON object")
		}
		return document, fmt.Errorf("parse Antigravity shared MCP servers %s: %w", path, err)
	}
	return document, nil
}

func rejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := consumeUniqueJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func consumeUniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if seen[key] {
				return fmt.Errorf("duplicate JSON object key %q", key)
			}
			seen[key] = true
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
	case '[':
		for decoder.More() {
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func antigravitySharedMCPEntryIsExact(raw json.RawMessage, expected antigravityMCPServer) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil || len(fields) != 3 {
		return false
	}
	for _, field := range []string{"command", "args", "env"} {
		if _, ok := fields[field]; !ok {
			return false
		}
	}
	var actual antigravityMCPServer
	if json.Unmarshal(raw, &actual) != nil {
		return false
	}
	return actual.Command == expected.Command && equalOrderedStrings(actual.Args, expected.Args) &&
		equalStringMaps(actual.Env, expected.Env)
}

func antigravitySharedMCPMatches(expected, reference *transcriptcapture.Config) (bool, error) {
	if reference == nil {
		reference = expected
	}
	if reference == nil {
		return false, errors.New("antigravity shared MCP state has no binding")
	}
	path, _, err := antigravitySharedMCPTransitionIdentity(expected, reference)
	if err != nil {
		return false, err
	}
	document, err := readAntigravitySharedMCPDocument(path)
	if err != nil {
		return false, err
	}
	return antigravitySharedMCPDocumentMatches(document, expected, reference)
}

func antigravitySharedMCPDocumentMatches(
	document antigravitySharedMCPDocument,
	expected, reference *transcriptcapture.Config,
) (bool, error) {
	if reference == nil {
		reference = expected
	}
	if reference == nil {
		return false, errors.New("antigravity shared MCP state has no binding")
	}
	name, err := antigravityMCPServerName(*reference)
	if err != nil {
		return false, err
	}
	for candidate := range document.servers {
		if strings.EqualFold(strings.TrimSpace(candidate), name) && candidate != name {
			return false, nil
		}
	}
	raw, present := document.servers[name]
	if expected == nil || expected.RuntimeMCPConfigPath == "" {
		return !present, nil
	}
	expectedServer, err := antigravityExpectedMCPServer(*expected)
	if err != nil {
		return false, err
	}
	return present && integrationFileModeMatches(document.mode, 0o600) && antigravitySharedMCPEntryIsExact(raw, expectedServer), nil
}

func preflightAntigravitySharedMCPTransition(previous, desired *transcriptcapture.Config) error {
	if _, _, err := antigravitySharedMCPTransitionIdentity(previous, desired); err != nil {
		return err
	}
	reference := desired
	if reference == nil {
		reference = previous
	}
	matches, err := antigravitySharedMCPMatches(previous, reference)
	if err != nil {
		return err
	}
	if !matches {
		return errors.New("antigravity shared MCP entry is missing, drifted, or already owned outside this integration")
	}
	return nil
}

func verifyAntigravitySharedMCPState(cfg transcriptcapture.Config) error {
	matches, err := antigravitySharedMCPMatches(&cfg, &cfg)
	if err != nil {
		return err
	}
	if !matches {
		if cfg.RuntimeMCPConfigPath == "" {
			return errors.New("antigravity legacy plugin collides with a shared MCP entry")
		}
		return errors.New("antigravity managed shared MCP entry is missing or drifted")
	}
	return nil
}

func convergeAntigravitySharedMCP(previous, desired *transcriptcapture.Config) (bool, error) {
	path, _, err := antigravitySharedMCPTransitionIdentity(previous, desired)
	if err != nil {
		return false, err
	}
	if err := preflightAntigravitySharedMCPTransition(previous, desired); err != nil {
		return false, err
	}
	reference := desired
	if reference == nil {
		reference = previous
	}
	if exact, matchErr := antigravitySharedMCPMatches(desired, reference); matchErr != nil {
		return false, matchErr
	} else if exact {
		return false, nil
	}
	document, err := readAntigravitySharedMCPDocument(path)
	if err != nil {
		return false, err
	}
	document, err = antigravitySharedMCPDocumentWithTarget(document, desired, reference)
	if err != nil {
		return false, err
	}
	if err := writeAntigravitySharedMCPDocument(document); err != nil {
		return true, err
	}
	if desired != nil {
		if err := verifyAntigravitySharedMCPState(*desired); err != nil {
			return true, err
		}
	} else {
		matches, matchErr := antigravitySharedMCPMatches(nil, previous)
		if matchErr != nil || !matches {
			if matchErr != nil {
				return true, matchErr
			}
			return true, errors.New("antigravity shared MCP entry remains after removal")
		}
	}
	return true, nil
}

func antigravitySharedMCPDocumentWithTarget(
	document antigravitySharedMCPDocument,
	desired, reference *transcriptcapture.Config,
) (antigravitySharedMCPDocument, error) {
	if reference == nil {
		reference = desired
	}
	if reference == nil {
		return document, errors.New("antigravity shared MCP mutation has no binding")
	}
	name, err := antigravityMCPServerName(*reference)
	if err != nil {
		return document, err
	}
	root := make(map[string]json.RawMessage, len(document.root))
	for key, raw := range document.root {
		root[key] = append(json.RawMessage(nil), raw...)
	}
	servers := make(map[string]json.RawMessage, len(document.servers))
	for key, raw := range document.servers {
		servers[key] = append(json.RawMessage(nil), raw...)
	}
	document.root = root
	document.servers = servers
	if desired == nil || desired.RuntimeMCPConfigPath == "" {
		delete(document.servers, name)
	} else {
		expected, err := antigravityExpectedMCPServer(*desired)
		if err != nil {
			return document, err
		}
		raw, err := json.Marshal(expected)
		if err != nil {
			return document, err
		}
		document.servers[name] = raw
	}
	if len(document.servers) == 0 {
		delete(document.root, "mcpServers")
	} else {
		serversRaw, err := json.Marshal(document.servers)
		if err != nil {
			return document, err
		}
		document.root["mcpServers"] = serversRaw
	}
	return document, nil
}

func antigravitySharedMCPDocumentOutput(document antigravitySharedMCPDocument) (bool, []byte, error) {
	if len(document.root) == 0 {
		return false, nil, nil
	}
	raw, err := json.MarshalIndent(document.root, "", "  ")
	if err != nil {
		return false, nil, err
	}
	return true, append(raw, '\n'), nil
}

func antigravitySharedMCPDocumentFingerprint(document antigravitySharedMCPDocument) antigravitySharedMCPFingerprint {
	if !document.exists {
		return antigravitySharedMCPFingerprint{}
	}
	sum := sha256.Sum256(document.raw)
	return antigravitySharedMCPFingerprint{
		Exists: true,
		Mode:   integrationFileModeFingerprint(document.mode),
		SHA256: fmt.Sprintf("%x", sum[:]),
	}
}

func antigravitySharedMCPCandidateFingerprint(exists bool, raw []byte) antigravitySharedMCPFingerprint {
	if !exists {
		return antigravitySharedMCPFingerprint{}
	}
	sum := sha256.Sum256(raw)
	return antigravitySharedMCPFingerprint{Exists: true, Mode: 0o600, SHA256: fmt.Sprintf("%x", sum[:])}
}

func writeAntigravitySharedMCPDocument(document antigravitySharedMCPDocument) error {
	current, err := readAntigravitySharedMCPDocument(document.path)
	if err != nil {
		return err
	}
	if current.exists != document.exists || !bytes.Equal(current.raw, document.raw) || current.mode != document.mode {
		return errors.New("antigravity shared MCP config changed during mutation")
	}
	desiredExists, desiredRaw, err := antigravitySharedMCPDocumentOutput(document)
	if err != nil {
		return err
	}
	if !desiredExists && !document.exists {
		return nil
	}
	directory := filepath.Dir(document.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	scratchPath, err := antigravitySharedMCPScratchPath(directory)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(scratchPath); err == nil {
		return errors.New("an interrupted Antigravity shared MCP mutation requires recovery")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Lstat(antigravitySharedMCPStagePath(scratchPath)); err == nil {
		return errors.New("an interrupted Antigravity shared MCP staging write requires recovery")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var mutation *antigravitySharedMCPMutation
	journal, journalErr := loadAntigravityTransactionJournal(directory)
	if journalErr == nil {
		candidate := antigravitySharedMCPCandidateFingerprint(desiredExists, desiredRaw)
		prepared := antigravitySharedMCPMutation{
			SchemaVersion: antigravitySharedMCPMutationSchema,
			JournalID:     journal.ID,
			CanonicalPath: document.path,
			Preimage:      antigravitySharedMCPDocumentFingerprint(document),
			Candidate:     candidate,
		}
		if err := writeAntigravitySharedMCPMutation(directory, prepared); err != nil {
			return err
		}
		mutation = &prepared
	} else if !errors.Is(journalErr, os.ErrNotExist) {
		return journalErr
	}
	scratchCandidateOwned := false
	mutationStarted := false
	defer func() {
		if scratchCandidateOwned {
			_ = os.Remove(scratchPath)
		}
		if mutation != nil && !mutationStarted {
			_ = removeAntigravitySharedMCPMutation(directory, *mutation)
		}
	}()
	if desiredExists {
		if err := writeAntigravitySharedMCPScratch(scratchPath, desiredRaw); err != nil {
			return err
		}
		scratchCandidateOwned = true
		if err := syncAntigravityConfigRoot(directory); err != nil {
			return err
		}
	}
	latest, err := readAntigravitySharedMCPDocument(document.path)
	if err != nil {
		return err
	}
	if latest.exists != document.exists || !bytes.Equal(latest.raw, document.raw) || latest.mode != document.mode {
		return errors.New("antigravity shared MCP config changed before atomic replacement")
	}

	switch {
	case !document.exists:
		if err := renameManagedInstructionFileNoReplace(scratchPath, document.path); err != nil {
			return err
		}
		scratchCandidateOwned = false
		mutationStarted = true
	case !desiredExists:
		if err := renameManagedInstructionFileNoReplace(document.path, scratchPath); err != nil {
			return err
		}
		mutationStarted = true
		if !antigravitySharedMCPFileMatchesDocument(scratchPath, document) {
			if restoreErr := renameManagedInstructionFileNoReplace(scratchPath, document.path); restoreErr == nil {
				mutationStarted = false
			}
			return errors.New("antigravity shared MCP config changed during atomic removal")
		}
		if err := os.Remove(scratchPath); err != nil {
			return err
		}
	default:
		if err := exchangeManagedInstructionFiles(document.path, scratchPath); err != nil {
			return err
		}
		scratchCandidateOwned = false
		mutationStarted = true
		if !antigravitySharedMCPFileMatchesDocument(scratchPath, document) {
			if restoreErr := exchangeManagedInstructionFiles(document.path, scratchPath); restoreErr == nil {
				if antigravitySharedMCPFileMatchesRaw(scratchPath, desiredRaw, 0o600) {
					if removeErr := os.Remove(scratchPath); removeErr == nil {
						mutationStarted = false
					}
				}
			}
			return errors.New("antigravity shared MCP config changed during atomic exchange")
		}
		installed, readErr := os.ReadFile(document.path)
		info, statErr := os.Lstat(document.path)
		if readErr != nil || statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
			!integrationFileModeMatches(info.Mode(), 0o600) || !bytes.Equal(installed, desiredRaw) {
			// Preserve both versions under the fingerprint fence for fail-closed
			// recovery.
			return errors.New("antigravity shared MCP config changed after atomic exchange")
		}
		if err := os.Remove(scratchPath); err != nil {
			return err
		}
	}
	if desiredExists {
		if err := syncAntigravityRegularFile(document.path); err != nil {
			return err
		}
	}
	if err := syncAntigravityConfigRoot(directory); err != nil {
		return err
	}
	if mutation != nil {
		if err := removeAntigravitySharedMCPMutation(directory, *mutation); err != nil {
			return err
		}
	}
	mutationStarted = false
	return nil
}

func antigravitySharedMCPScratchPath(configRoot string) (string, error) {
	journal, err := loadAntigravityTransactionJournal(configRoot)
	if err == nil {
		return antigravitySharedMCPScratchPathForJournal(configRoot, journal), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	temporary, err := os.CreateTemp(configRoot, ".witself-antigravity-mcp-swap-unfenced-")
	if err != nil {
		return "", err
	}
	path := temporary.Name()
	if err := temporary.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

func writeAntigravitySharedMCPScratch(path string, raw []byte) error {
	stagePath := antigravitySharedMCPStagePath(path)
	file, err := os.OpenFile(stagePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(stagePath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := renameManagedInstructionFileNoReplace(stagePath, path); err != nil {
		return err
	}
	keep = true
	return nil
}

func antigravitySharedMCPFileMatchesDocument(path string, expected antigravitySharedMCPDocument) bool {
	return antigravitySharedMCPFileMatchesRaw(path, expected.raw, expected.mode)
}

func antigravitySharedMCPFileMatchesRaw(path string, expectedRaw []byte, expectedMode os.FileMode) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !integrationFileModeMatches(info.Mode(), expectedMode) {
		return false
	}
	raw, err := os.ReadFile(path)
	return err == nil && bytes.Equal(raw, expectedRaw)
}

func antigravitySharedMCPScratchPathForJournal(configRoot string, journal antigravityTransactionJournal) string {
	return filepath.Join(configRoot, ".witself-antigravity-mcp-swap-"+journal.ID)
}

func antigravitySharedMCPStagePath(scratchPath string) string {
	return scratchPath + ".stage"
}

func antigravitySharedMCPMutationPath(configRoot string, journal antigravityTransactionJournal) string {
	return filepath.Join(configRoot, ".witself-antigravity-mcp-mutation-"+journal.ID+".json")
}

func writeAntigravitySharedMCPMutation(configRoot string, mutation antigravitySharedMCPMutation) error {
	journal := antigravityTransactionJournal{ID: mutation.JournalID}
	path := antigravitySharedMCPMutationPath(configRoot, journal)
	if _, err := os.Lstat(path); err == nil {
		return errors.New("an interrupted Antigravity shared MCP mutation requires recovery")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	raw, err := json.MarshalIndent(mutation, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	temporary, err := os.CreateTemp(configRoot, ".witself-antigravity-mcp-mutation-stage-")
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

func loadAntigravitySharedMCPMutation(
	configRoot string,
	journal antigravityTransactionJournal,
) (antigravitySharedMCPMutation, error) {
	path := antigravitySharedMCPMutationPath(configRoot, journal)
	info, err := os.Lstat(path)
	if err != nil {
		return antigravitySharedMCPMutation{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !integrationFileModeMatches(info.Mode(), 0o600) {
		return antigravitySharedMCPMutation{}, errors.New("antigravity shared MCP mutation fence must be a real 0600 regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return antigravitySharedMCPMutation{}, err
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return antigravitySharedMCPMutation{}, fmt.Errorf("parse Antigravity shared MCP mutation fence: %w", err)
	}
	var mutation antigravitySharedMCPMutation
	if err := json.Unmarshal(raw, &mutation); err != nil {
		return antigravitySharedMCPMutation{}, fmt.Errorf("parse Antigravity shared MCP mutation fence: %w", err)
	}
	if mutation.SchemaVersion != antigravitySharedMCPMutationSchema || mutation.JournalID != journal.ID {
		return antigravitySharedMCPMutation{}, errors.New("unsupported Antigravity shared MCP mutation fence")
	}
	if err := validateAntigravitySharedMCPFingerprint(mutation.Preimage); err != nil {
		return antigravitySharedMCPMutation{}, fmt.Errorf("invalid Antigravity shared MCP preimage fingerprint: %w", err)
	}
	if err := validateAntigravitySharedMCPFingerprint(mutation.Candidate); err != nil {
		return antigravitySharedMCPMutation{}, fmt.Errorf("invalid Antigravity shared MCP candidate fingerprint: %w", err)
	}
	return mutation, nil
}

func validateAntigravitySharedMCPFingerprint(fingerprint antigravitySharedMCPFingerprint) error {
	if !fingerprint.Exists {
		if fingerprint.Mode != 0 || fingerprint.SHA256 != "" {
			return errors.New("absent fingerprint contains file metadata")
		}
		return nil
	}
	if fingerprint.Mode > 0o777 {
		return errors.New("file mode is invalid")
	}
	decoded, err := hex.DecodeString(fingerprint.SHA256)
	if err != nil || len(decoded) != sha256.Size {
		return errors.New("SHA-256 digest is invalid")
	}
	return nil
}

func removeAntigravitySharedMCPMutation(configRoot string, expected antigravitySharedMCPMutation) error {
	journal := antigravityTransactionJournal{ID: expected.JournalID}
	current, err := loadAntigravitySharedMCPMutation(configRoot, journal)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if current != expected {
		return errors.New("antigravity shared MCP mutation fence changed; refusing to remove it")
	}
	if err := os.Remove(antigravitySharedMCPMutationPath(configRoot, journal)); err != nil {
		return err
	}
	return syncAntigravityConfigRoot(configRoot)
}

func antigravitySharedMCPFingerprintAt(path string) (antigravitySharedMCPFingerprint, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return antigravitySharedMCPFingerprint{}, nil
	}
	if err != nil {
		return antigravitySharedMCPFingerprint{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return antigravitySharedMCPFingerprint{}, errors.New("antigravity shared MCP mutation target must be a real regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return antigravitySharedMCPFingerprint{}, err
	}
	sum := sha256.Sum256(raw)
	return antigravitySharedMCPFingerprint{
		Exists: true,
		Mode:   integrationFileModeFingerprint(info.Mode()),
		SHA256: fmt.Sprintf("%x", sum[:]),
	}, nil
}

func reconcileAntigravitySharedMCPScratch(configRoot string, journal antigravityTransactionJournal) error {
	scratchPath := antigravitySharedMCPScratchPathForJournal(configRoot, journal)
	stagePath := antigravitySharedMCPStagePath(scratchPath)
	binding := journal.Desired
	if binding == nil {
		binding = journal.Previous
	}
	if binding == nil {
		return errors.New("antigravity shared MCP recovery scratch has no binding")
	}
	canonicalPath, err := antigravityCanonicalSharedMCPPath(*binding)
	if err != nil {
		return err
	}
	mutation, mutationErr := loadAntigravitySharedMCPMutation(configRoot, journal)
	if errors.Is(mutationErr, os.ErrNotExist) {
		if _, scratchErr := os.Lstat(scratchPath); errors.Is(scratchErr, os.ErrNotExist) {
			return nil
		} else if scratchErr != nil {
			return scratchErr
		}
		return errors.New("antigravity shared MCP recovery scratch has no fingerprint fence")
	}
	if mutationErr != nil {
		return mutationErr
	}
	if mutation.CanonicalPath != canonicalPath {
		return errors.New("antigravity shared MCP mutation fence has a non-canonical target")
	}
	if stageInfo, stageErr := os.Lstat(stagePath); stageErr == nil {
		if !stageInfo.Mode().IsRegular() || stageInfo.Mode()&os.ModeSymlink != 0 {
			return errors.New("antigravity shared MCP staging scratch must be a real regular file")
		}
		if _, scratchErr := os.Lstat(scratchPath); scratchErr == nil {
			return errors.New("antigravity shared MCP staging and recovery scratch both exist")
		} else if !errors.Is(scratchErr, os.ErrNotExist) {
			return scratchErr
		}
		// The no-replace publication has not happened, so canonical was never
		// mutated by this staged write. Remove even a partial app-owned stage
		// and let tuple recovery evaluate the untouched canonical state.
		if err := os.Remove(stagePath); err != nil {
			return err
		}
		if err := syncAntigravityConfigRoot(configRoot); err != nil {
			return err
		}
		return removeAntigravitySharedMCPMutation(configRoot, mutation)
	} else if !errors.Is(stageErr, os.ErrNotExist) {
		return stageErr
	}
	canonical, err := antigravitySharedMCPFingerprintAt(canonicalPath)
	if err != nil {
		return err
	}
	scratch, err := antigravitySharedMCPFingerprintAt(scratchPath)
	if err != nil {
		return err
	}
	if !scratch.Exists {
		if canonical != mutation.Preimage && canonical != mutation.Candidate {
			return errors.New("canonical Antigravity shared MCP config changed during interrupted mutation")
		}
		return removeAntigravitySharedMCPMutation(configRoot, mutation)
	}
	if scratch == mutation.Candidate {
		// The exact candidate is app-owned. A concurrent writer may have
		// changed the canonical file before the exchange or after a successful
		// rollback; discard only the fenced candidate and preserve canonical.
		if err := os.Remove(scratchPath); err != nil {
			return err
		}
		if err := syncAntigravityConfigRoot(configRoot); err != nil {
			return err
		}
		return removeAntigravitySharedMCPMutation(configRoot, mutation)
	}
	if canonical != mutation.Candidate || scratch != mutation.Preimage {
		return errors.New("antigravity shared MCP files do not match the fenced preimage/candidate pair")
	}
	switch {
	case mutation.Preimage.Exists && mutation.Candidate.Exists:
		if err := exchangeManagedInstructionFiles(canonicalPath, scratchPath); err != nil {
			return err
		}
		canonicalAfter, canonicalErr := antigravitySharedMCPFingerprintAt(canonicalPath)
		scratchAfter, scratchErr := antigravitySharedMCPFingerprintAt(scratchPath)
		if canonicalErr != nil || scratchErr != nil || canonicalAfter != mutation.Preimage || scratchAfter != mutation.Candidate {
			return errors.New("antigravity shared MCP files changed while rolling back interrupted exchange")
		}
		if err := os.Remove(scratchPath); err != nil {
			return err
		}
	case mutation.Preimage.Exists && !mutation.Candidate.Exists:
		if err := renameManagedInstructionFileNoReplace(scratchPath, canonicalPath); err != nil {
			return err
		}
		canonicalAfter, fingerprintErr := antigravitySharedMCPFingerprintAt(canonicalPath)
		if fingerprintErr != nil || canonicalAfter != mutation.Preimage {
			return errors.New("antigravity shared MCP preimage changed while rolling back interrupted removal")
		}
	default:
		return errors.New("antigravity shared MCP mutation fence has an impossible scratch state")
	}
	if err := syncAntigravityRegularFile(canonicalPath); err != nil {
		return err
	}
	if err := syncAntigravityConfigRoot(configRoot); err != nil {
		return err
	}
	return removeAntigravitySharedMCPMutation(configRoot, mutation)
}

func requireAntigravitySharedMCPScratchAbsent(configRoot string, journal antigravityTransactionJournal) error {
	paths := []string{
		antigravitySharedMCPScratchPathForJournal(configRoot, journal),
		antigravitySharedMCPStagePath(antigravitySharedMCPScratchPathForJournal(configRoot, journal)),
		antigravitySharedMCPMutationPath(configRoot, journal),
	}
	for _, path := range paths {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		return errors.New("antigravity shared MCP recovery artifact remains while committing transaction")
	}
	return nil
}

func syncAntigravitySharedMCPState(cfg transcriptcapture.Config) error {
	if err := verifyAntigravitySharedMCPState(cfg); err != nil {
		return err
	}
	if cfg.RuntimeMCPConfigPath == "" {
		return nil
	}
	if err := syncAntigravityRegularFile(cfg.RuntimeMCPConfigPath); err != nil {
		return err
	}
	return syncAntigravityConfigRoot(filepath.Dir(cfg.RuntimeMCPConfigPath))
}
