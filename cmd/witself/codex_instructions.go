package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	codexMemoryRoutingBeginMarker = "<!-- BEGIN WITSELF MANAGED MEMORY ROUTING -->"
	codexMemoryRoutingEndMarker   = "<!-- END WITSELF MANAGED MEMORY ROUTING -->"

	// codexMemoryRoutingInstructions is the canonical contract shared by the
	// Codex AGENTS.md integration and runtime-facing guidance. Keep the policy in
	// one place so natural-language storage and retrieval use the same routing
	// rules regardless of where Codex first sees them.
	codexMemoryRoutingInstructions = `## Witself facts and Codex memory

On an explicit remember/save/store request, call witself.fact.set in the same turn for an atomic durable assertion. A merely stated fact is only a review candidate, not authority for canonical truth. Mark private personal values sensitive; never put them in subject metadata. Narrative requests stay eligible for Codex native memory, which is best-effort, not an immediate write. Never silently duplicate across providers; use both only when explicitly requested.

When the user asks you to remember, save, or store something, route it by shape:

- An atomic, durable, independently retrievable assertion belongs in Witself. Examples include a name or relationship, birthday or other date, address or location, URL, durable preference, identifier, and a stable status. Store it in the same turn with the appropriate Witself fact tool; an explicit request to remember the assertion is authority to call witself.fact.set. Do not also create a Codex memory file for it.
- For facts about another person, place, project, or entity, resolve one stable subject with the Witself subject tools. Keep subject keys, display names, and aliases non-sensitive. Store private personal values such as a spouse's name or home address only as sensitive facts; never put those values in subject metadata.
- Narrative context stays eligible for Codex native memory. Examples include reasoning, project history, a multi-step incident, lessons learned, and material whose meaning depends on retaining a passage. Do not reduce that material to a Witself fact merely because the user said "remember." Codex memory is generated asynchronously in the background, not through a transactional write API: never promise immediate persistence, claim that it was stored, or create a manual Markdown memory file as a substitute.
- If a request clearly contains both kinds, split it: store only the atomic assertions in Witself and leave the narrative remainder eligible for Codex native memory. Tell the user which facts were stored, but describe native-memory handling as best-effort and do not claim that the narrative was stored. If the boundary is genuinely ambiguous, ask before storing.
- Honor an explicit destination such as "as a Witself fact," "in Codex memory," or "in both" even if the automatic classification would differ, while preserving the best-effort limitation of Codex memory.
- Never silently store the same information in both systems. Store it in both only when the user explicitly requests both.
- Do not change Codex memory settings as part of routing or retrieval.

For retrieval, use the same two-source model:

- For a specific fact lookup, query the relevant Witself fact tools first. Consult available Codex memory context too when the request or context indicates that narrative history may matter. Keep sensitive facts redacted by default; reveal one only for an exact, intentional lookup when the authenticated user needs the value.
- For a broad request such as "what do you remember about this?", search Witself facts and consult any Codex native-memory context available or injected into the task. Codex has no transactional full-memory search API, so never claim that all Codex memory was searched; characterize its contribution as partial and best-effort when completeness matters. Also search Witself memory only when a dedicated Witself memory-recall tool is actually available, and never claim that provider was searched when it was unavailable. Do not conclude that nothing is known until all available, relevant sources have been checked. Keep sensitive facts redacted in broad results.
- Witself transcripts are interaction records, not memories. Search them only when the user explicitly requests transcript or conversation history.
- If the user explicitly names one source, use only that source.
- Merge results without duplicating them and identify each result's provenance. Present Witself facts as canonical assertions and memories as advisory context. Surface conflicts and uncertainty instead of silently choosing one source. If any requested provider or source is unavailable, report the partial result and name what could not be searched.`
)

var codexMemoryRoutingBlock = []byte(
	codexMemoryRoutingBeginMarker + "\n" +
		codexMemoryRoutingInstructions + "\n" +
		codexMemoryRoutingEndMarker,
)

type codexInstructionsSnapshot struct {
	path    string
	data    []byte
	mode    fs.FileMode
	existed bool
}

func installCodexMemoryRoutingInstructions() (codexInstructionsSnapshot, error) {
	path, err := codexAgentsPath()
	if err != nil {
		return codexInstructionsSnapshot{}, err
	}
	if err := validateCodexAgentsFileIsActive(path); err != nil {
		return codexInstructionsSnapshot{}, err
	}
	snapshot, err := readCodexInstructionsSnapshot(path)
	if err != nil {
		return codexInstructionsSnapshot{}, err
	}
	updated, changed, err := upsertCodexMemoryRoutingBlock(snapshot.data)
	if err != nil {
		return codexInstructionsSnapshot{}, err
	}
	if !changed {
		return snapshot, nil
	}
	mode := snapshot.mode
	if !snapshot.existed {
		mode = 0o600
	}
	if err := writeCodexInstructionsFile(snapshot.path, updated, mode); err != nil {
		return codexInstructionsSnapshot{}, err
	}
	return snapshot, nil
}

func removeCodexMemoryRoutingInstructions() (codexInstructionsSnapshot, error) {
	path, err := codexAgentsPath()
	if err != nil {
		return codexInstructionsSnapshot{}, err
	}
	snapshot, err := readCodexInstructionsSnapshot(path)
	if err != nil {
		return codexInstructionsSnapshot{}, err
	}
	if !snapshot.existed {
		return snapshot, nil
	}
	updated, changed, err := removeCodexMemoryRoutingBlock(snapshot.data)
	if err != nil {
		return codexInstructionsSnapshot{}, err
	}
	if !changed {
		return snapshot, nil
	}
	// Retain an empty AGENTS.md rather than guessing whether Witself created it.
	// This preserves a pre-existing empty file and still removes every managed
	// byte when the block was the file's only content.
	if err := writeCodexInstructionsFile(snapshot.path, updated, snapshot.mode); err != nil {
		return codexInstructionsSnapshot{}, err
	}
	return snapshot, nil
}

func (snapshot codexInstructionsSnapshot) restore() error {
	if snapshot.path == "" {
		return nil
	}
	if !snapshot.existed {
		if err := os.Remove(snapshot.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", snapshot.path, err)
		}
		return nil
	}
	return writeCodexInstructionsFile(snapshot.path, snapshot.data, snapshot.mode)
}

func codexAgentsPath() (string, error) {
	root := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".codex")
	}
	return filepath.Join(root, "AGENTS.md"), nil
}

func validateCodexAgentsFileIsActive(path string) error {
	overridePath := filepath.Join(filepath.Dir(path), "AGENTS.override.md")
	raw, err := os.ReadFile(overridePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", overridePath, err)
	}
	if strings.TrimSpace(string(raw)) != "" {
		return fmt.Errorf("%s is non-empty and shadows %s; remove it or merge its instructions into AGENTS.md before installing Witself", overridePath, path)
	}
	return nil
}

func readCodexInstructionsSnapshot(path string) (codexInstructionsSnapshot, error) {
	snapshot := codexInstructionsSnapshot{path: path, mode: 0o600}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return codexInstructionsSnapshot{}, fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return codexInstructionsSnapshot{}, fmt.Errorf("resolve %s: %w", path, err)
		}
		path, err = filepath.Abs(resolved)
		if err != nil {
			return codexInstructionsSnapshot{}, fmt.Errorf("resolve %s: %w", resolved, err)
		}
		info, err = os.Stat(path)
		if err != nil {
			return codexInstructionsSnapshot{}, fmt.Errorf("inspect %s: %w", path, err)
		}
		snapshot.path = path
	}
	if !info.Mode().IsRegular() {
		return codexInstructionsSnapshot{}, fmt.Errorf("%s is not a regular file", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return codexInstructionsSnapshot{}, fmt.Errorf("read %s: %w", path, err)
	}
	snapshot.data = raw
	snapshot.mode = info.Mode()
	snapshot.existed = true
	return snapshot, nil
}

func upsertCodexMemoryRoutingBlock(raw []byte) ([]byte, bool, error) {
	start, end, found, err := codexMemoryRoutingBlockRange(raw)
	if err != nil {
		return nil, false, err
	}
	if found {
		if bytes.Equal(raw[start:end], codexMemoryRoutingBlock) {
			return raw, false, nil
		}
		updated := make([]byte, 0, len(raw)-(end-start)+len(codexMemoryRoutingBlock))
		updated = append(updated, raw[:start]...)
		updated = append(updated, codexMemoryRoutingBlock...)
		updated = append(updated, raw[end:]...)
		return updated, true, nil
	}
	updated := make([]byte, 0, len(codexMemoryRoutingBlock)+2+len(raw))
	updated = append(updated, codexMemoryRoutingBlock...)
	if len(raw) == 0 {
		updated = append(updated, '\n')
	} else {
		updated = append(updated, '\n', '\n')
		updated = append(updated, raw...)
	}
	return updated, true, nil
}

func removeCodexMemoryRoutingBlock(raw []byte) ([]byte, bool, error) {
	start, end, found, err := codexMemoryRoutingBlockRange(raw)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return raw, false, nil
	}
	// The installer owns the separator immediately following its managed block.
	// Removing it restores a pre-existing AGENTS.md byte-for-byte in the normal
	// prefix installation layout while leaving all unrelated content untouched.
	if bytes.HasPrefix(raw[end:], []byte("\n\n")) {
		end += 2
	} else if bytes.HasPrefix(raw[end:], []byte("\n")) {
		end++
	}
	updated := make([]byte, 0, len(raw)-(end-start))
	updated = append(updated, raw[:start]...)
	updated = append(updated, raw[end:]...)
	return updated, true, nil
}

func codexMemoryRoutingBlockRange(raw []byte) (int, int, bool, error) {
	begin := []byte(codexMemoryRoutingBeginMarker)
	endMarker := []byte(codexMemoryRoutingEndMarker)
	start := bytes.Index(raw, begin)
	endStart := bytes.Index(raw, endMarker)
	if start == -1 && endStart == -1 {
		return 0, 0, false, nil
	}
	if start == -1 || endStart == -1 || endStart < start {
		return 0, 0, false, errors.New("AGENTS.md contains an incomplete Witself managed memory routing block")
	}
	end := endStart + len(endMarker)
	if bytes.Contains(raw[start+len(begin):endStart], begin) ||
		bytes.Contains(raw[end:], begin) ||
		bytes.Contains(raw[end:], endMarker) {
		return 0, 0, false, errors.New("AGENTS.md contains multiple Witself managed memory routing markers")
	}
	return start, end, true, nil
}

func writeCodexInstructionsFile(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".AGENTS.md.witself-*")
	if err != nil {
		return fmt.Errorf("create temporary AGENTS.md: %w", err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(mode.Perm()); err != nil {
		_ = temp.Close()
		return fmt.Errorf("set temporary AGENTS.md permissions: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temporary AGENTS.md: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync temporary AGENTS.md: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary AGENTS.md: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
