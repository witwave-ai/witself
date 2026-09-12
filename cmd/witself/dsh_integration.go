package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const (
	dshPatchFileName         = "cordis.patch.yml"
	dshPatchRowID            = "witself-mcp"
	dshHooksPatchRowID       = "witself-hooks"
	dshMCPClientPluginName   = "@deepseek-ai/dsh-mcp-client"
	dshHooksBridgePluginName = "@deepseek-ai/dsh-hooks-claude-code"
	dshHookConfigFileName    = "hooks.json"
	dshMCPServerName         = "witself"

	// One fenced block, versioned in the marker itself. A file carrying the
	// same fence name at another version was written by a different Witself
	// release; refuse it instead of guessing how to migrate foreign bytes.
	dshPatchBlockVersion     = "v1"
	dshPatchBlockName        = "dsh-runtime-integration"
	dshPatchBlockBeginPrefix = "# witself:managed:begin " + dshPatchBlockName
	dshPatchBlockEndPrefix   = "# witself:managed:end " + dshPatchBlockName
	dshPatchBlockBeginMarker = dshPatchBlockBeginPrefix + " " + dshPatchBlockVersion
	dshPatchBlockEndMarker   = dshPatchBlockEndPrefix + " " + dshPatchBlockVersion

	// The patch file dictates the command, arguments, and environment dsh
	// executes for the Witself MCP client, so it is held owner-only even when
	// the provider or an operator created it with a wider mode.
	dshPatchFileMode = os.FileMode(0o600)

	dshPatchReadLimit     = 1024 * 1024
	dshCLIOutputLimit     = 1024 * 1024
	dshCLIDiagnosticLimit = 4 * 1024
	dshDumpConfigTimeout  = 15 * time.Second
	dshCLIWaitDelay       = 2 * time.Second
)

// Patch-file shapes Witself is willing to own. Anything else keeps its exact
// bytes and produces a refusal naming the path.
const (
	dshPatchShapeMissing       = "missing"
	dshPatchShapeEmpty         = "empty-sequence"
	dshPatchShapeBlockSequence = "block-sequence"
	dshPatchShapeUnsupported   = "unsupported"
)

// The indirection keeps composed-tree verification production code while
// letting focused tests model dsh without booting Node.
var runDSHDumpConfig = runDSHDumpConfigCommand

// currentDSHConfigRoot resolves the exact DSH_HOME every dsh operation uses.
// Persisting it prevents a later shell from silently targeting another profile
// tree with its own home-level patch file.
func currentDSHConfigRoot() (string, error) {
	root := strings.TrimSpace(os.Getenv("DSH_HOME"))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve DeepSeek Harness home: %w", err)
		}
		root = filepath.Join(home, ".dsh")
	} else if root == "~" || strings.HasPrefix(root, "~/") || strings.HasPrefix(root, "~\\") {
		// dsh-home-paths expands a leading tilde itself, so Witself must land
		// on the same directory instead of resolving `~` against the cwd.
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve DeepSeek Harness home: %w", err)
		}
		if root == "~" {
			root = home
		} else {
			root = filepath.Join(home, root[2:])
		}
	}
	return cleanCopilotAbsolutePath("DSH_HOME", root)
}

func dshPatchPathAt(configRoot string) (string, error) {
	root, err := cleanCopilotAbsolutePath("DeepSeek Harness config root", configRoot)
	if err != nil {
		return "", err
	}
	if root != configRoot {
		return "", errors.New("DeepSeek Harness config root must be canonical")
	}
	return filepath.Join(root, dshPatchFileName), nil
}

// dshHookConfigPathAt is the one hook document dsh's claude-code bridge reads.
// The bridge requires an absolute configPath, so the patch row and the owned
// hook file must always name the same canonical path under the config root.
func dshHookConfigPathAt(configRoot string) (string, error) {
	root, err := cleanCopilotAbsolutePath("DeepSeek Harness config root", configRoot)
	if err != nil {
		return "", err
	}
	if root != configRoot {
		return "", errors.New("DeepSeek Harness config root must be canonical")
	}
	return filepath.Join(root, dshHookConfigFileName), nil
}

// captureDSHMCPEnvironment is the exact environment the managed block hands
// to the MCP server. dsh spawns MCP children over a scrubbed environment that
// drops every ambient DSH_* name, so DSH_HOME must travel in the block itself
// or `mcp serve` could never re-resolve the installed config root.
func captureDSHMCPEnvironment() (map[string]string, error) {
	witselfHome, err := local.Home()
	if err != nil {
		return nil, fmt.Errorf("resolve WITSELF_HOME: %w", err)
	}
	witselfHome, err = cleanCopilotAbsolutePath("WITSELF_HOME", witselfHome)
	if err != nil {
		return nil, err
	}
	root, err := currentDSHConfigRoot()
	if err != nil {
		return nil, err
	}
	return map[string]string{"DSH_HOME": root, "WITSELF_HOME": witselfHome}, nil
}

func equalDSHEnvironment(left, right map[string]string) bool {
	return equalCopilotEnvironment(left, right)
}

func configureDSHBinding(cfg *transcriptcapture.Config, runtimeCLI, witselfExecutable string) error {
	if cfg == nil {
		return errors.New("dsh integration config is required")
	}
	var err error
	runtimeCLI, err = cleanCopilotInvocationPath("DeepSeek Harness CLI", runtimeCLI)
	if err != nil {
		return err
	}
	witselfExecutable, err = cleanCopilotInvocationPath("Witself executable", witselfExecutable)
	if err != nil {
		return err
	}
	root, err := currentDSHConfigRoot()
	if err != nil {
		return err
	}
	patchPath, err := dshPatchPathAt(root)
	if err != nil {
		return err
	}
	environment, err := captureDSHMCPEnvironment()
	if err != nil {
		return err
	}
	cfg.RuntimeCLICommand = runtimeCLI
	cfg.MCPCommand = witselfExecutable
	cfg.MCPEnvironment = environment
	cfg.RuntimeConfigRoot = root
	cfg.RuntimeMCPConfigPath = patchPath
	return nil
}

func validateDSHCLISelection(runtimeCLI string, cfg transcriptcapture.Config) error {
	clean, err := cleanCopilotCommandPath("DeepSeek Harness CLI", runtimeCLI)
	if err != nil {
		return err
	}
	if clean != cfg.RuntimeCLICommand {
		return fmt.Errorf("DeepSeek Harness CLI %s does not match installed command %s", clean, cfg.RuntimeCLICommand)
	}
	if cfg.RuntimeConfigRoot == "" || !filepath.IsAbs(cfg.RuntimeConfigRoot) ||
		filepath.Clean(cfg.RuntimeConfigRoot) != cfg.RuntimeConfigRoot {
		return errors.New("DeepSeek Harness config root must be a clean absolute path")
	}
	if cfg.RuntimeMCPConfigPath != filepath.Join(cfg.RuntimeConfigRoot, dshPatchFileName) {
		return errors.New("DeepSeek Harness patch file path is not canonical for the installed config root")
	}
	if _, err := dshManagedHookRowPath(cfg); err != nil {
		return err
	}
	return nil
}

func validateDSHPreviousSelection(runtimeCLI string, desired, previous transcriptcapture.Config) error {
	if previous.RuntimeCLICommand != runtimeCLI {
		return errors.New("DeepSeek Harness CLI changed since installation; uninstall the existing integration before reinstalling")
	}
	if previous.RuntimeConfigRoot != desired.RuntimeConfigRoot {
		return errors.New("DSH_HOME changed since installation; restore it before reinstalling or uninstall the existing integration first")
	}
	if previous.RuntimeMCPConfigPath != desired.RuntimeMCPConfigPath {
		return errors.New("DeepSeek Harness patch file path changed since installation")
	}
	return nil
}

// dshManagedPatchBlock renders the single fenced block Witself owns. The
// rendering is deterministic so verification can require byte-exact equality
// without round-tripping foreign YAML, which would destroy `!!js` tags.
//
// The fence holds one `- insert:` entry with up to two rows: `witself-mcp`
// always, and `witself-hooks` whenever transcript hooks are installed. Keeping
// both rows in one fence means install, verification, and uninstall govern the
// MCP client and the hook bridge as a single owned region.
func dshManagedPatchBlock(cfg transcriptcapture.Config) ([]byte, error) {
	command, args, err := dshMCPInvocation(cfg)
	if err != nil {
		return nil, err
	}
	if err := validateDSHMCPEnvironmentForBlock(cfg.MCPEnvironment, cfg.RuntimeConfigRoot); err != nil {
		return nil, err
	}
	quotedCommand, err := dshQuoteYAMLScalar("DeepSeek Harness MCP command", command)
	if err != nil {
		return nil, err
	}
	quotedArgs := make([]string, 0, len(args))
	for _, arg := range args {
		quoted, err := dshQuoteYAMLScalar("DeepSeek Harness MCP argument", arg)
		if err != nil {
			return nil, err
		}
		quotedArgs = append(quotedArgs, quoted)
	}
	keys := make([]string, 0, len(cfg.MCPEnvironment))
	for key := range cfg.MCPEnvironment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		quotedKey, err := dshQuoteYAMLScalar("DeepSeek Harness MCP environment key", key)
		if err != nil {
			return nil, err
		}
		quotedValue, err := dshQuoteYAMLScalar("DeepSeek Harness MCP environment value", cfg.MCPEnvironment[key])
		if err != nil {
			return nil, err
		}
		environment = append(environment, quotedKey+": "+quotedValue)
	}
	quotedName, err := dshQuoteYAMLScalar("DeepSeek Harness MCP plugin name", dshMCPClientPluginName)
	if err != nil {
		return nil, err
	}
	hookConfigPath, err := dshManagedHookRowPath(cfg)
	if err != nil {
		return nil, err
	}
	var block bytes.Buffer
	block.WriteString(dshPatchBlockBeginMarker + "\n")
	block.WriteString("- insert:\n")
	block.WriteString("    - id: " + dshPatchRowID + "\n")
	block.WriteString("      name: " + quotedName + "\n")
	block.WriteString("      config:\n")
	block.WriteString("        transport: stdio\n")
	block.WriteString("        serverName: " + dshMCPServerName + "\n")
	block.WriteString("        command: " + quotedCommand + "\n")
	block.WriteString("        args: [" + strings.Join(quotedArgs, ", ") + "]\n")
	block.WriteString("        env: {" + strings.Join(environment, ", ") + "}\n")
	block.WriteString("        failOnStartupError: false\n")
	if hookConfigPath != "" {
		quotedBridge, err := dshQuoteYAMLScalar("DeepSeek Harness hook bridge plugin name", dshHooksBridgePluginName)
		if err != nil {
			return nil, err
		}
		quotedHookConfigPath, err := dshQuoteYAMLScalar("DeepSeek Harness hook config path", hookConfigPath)
		if err != nil {
			return nil, err
		}
		block.WriteString("    - id: " + dshHooksPatchRowID + "\n")
		block.WriteString("      name: " + quotedBridge + "\n")
		block.WriteString("      config:\n")
		block.WriteString("        configPath: " + quotedHookConfigPath + "\n")
	}
	block.WriteString(dshPatchBlockEndMarker)
	return block.Bytes(), nil
}

// dshManagedHookRowPath returns the absolute hook config path the bridge row
// must carry, or "" when this binding installs no transcript hooks. A binding
// that claims user hooks without the canonical path is refused rather than
// rendered with a path dsh would read from somewhere else.
func dshManagedHookRowPath(cfg transcriptcapture.Config) (string, error) {
	if cfg.HookMode != transcriptcapture.HookModeUser {
		return "", nil
	}
	expected, err := dshHookConfigPathAt(cfg.RuntimeConfigRoot)
	if err != nil {
		return "", err
	}
	if cfg.HookConfigPath != expected {
		return "", fmt.Errorf(
			"DeepSeek Harness hook config path must be %s for the installed config root", expected,
		)
	}
	return expected, nil
}

func dshMCPInvocation(cfg transcriptcapture.Config) (string, []string, error) {
	executable, err := cleanCopilotCommandPath("DeepSeek Harness MCP command", cfg.MCPCommand)
	if err != nil {
		return "", nil, err
	}
	account := strings.TrimSpace(cfg.Account)
	if account == "" {
		account = "default"
	}
	realm := strings.TrimSpace(cfg.Realm)
	if realm == "" {
		realm = "default"
	}
	agent := strings.TrimSpace(cfg.Agent)
	if agent == "" {
		agent = strings.TrimSpace(cfg.AgentName)
	}
	if agent == "" {
		return "", nil, errors.New("installed DeepSeek Harness integration has no agent name")
	}
	serveArgs := runtimeMCPServeArgs(
		transcriptcapture.RuntimeDSH, executable, account, realm, agent, cfg.Location.Name,
	)
	return serveArgs[0], append([]string(nil), serveArgs[1:]...), nil
}

func validateDSHMCPEnvironmentForBlock(environment map[string]string, configRoot string) error {
	if len(environment) != 2 {
		return errors.New("dsh MCP environment must contain exactly DSH_HOME and WITSELF_HOME")
	}
	for _, key := range []string{"DSH_HOME", "WITSELF_HOME"} {
		value, exists := environment[key]
		if !exists || value == "" || strings.TrimSpace(value) != value ||
			len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") ||
			!filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("dsh MCP environment %s must be a clean absolute path", key)
		}
	}
	if environment["DSH_HOME"] != configRoot {
		return errors.New("dsh MCP environment DSH_HOME must equal the installed config root")
	}
	return nil
}

// dshQuoteYAMLScalar emits a single-quoted YAML scalar. Single quotes are the
// only YAML style with no escape processing, so a Windows path or an argument
// containing a backslash survives byte-for-byte.
func dshQuoteYAMLScalar(label, value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("%s must not be empty", label)
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return "", fmt.Errorf("%s must not contain control characters", label)
		}
	}
	return "'" + strings.ReplaceAll(value, "'", "''") + "'", nil
}

type dshPatchSnapshot struct {
	path            string
	exists          bool
	mode            os.FileMode
	raw             []byte
	fileInfo        os.FileInfo
	lines           []string
	trailingNewline bool
	shape           string
	emptyIndex      int
	blockBegin      int
	blockEnd        int
}

func (snapshot dshPatchSnapshot) blockPresent() bool {
	return snapshot.blockBegin >= 0
}

func (snapshot dshPatchSnapshot) block() []byte {
	if !snapshot.blockPresent() {
		return nil
	}
	return []byte(strings.Join(snapshot.lines[snapshot.blockBegin:snapshot.blockEnd+1], "\n"))
}

func readDSHPatchSnapshot(path string) (dshPatchSnapshot, error) {
	snapshot := dshPatchSnapshot{
		path: path, mode: 0o600, shape: dshPatchShapeMissing,
		emptyIndex: -1, blockBegin: -1, blockEnd: -1,
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return dshPatchSnapshot{}, fmt.Errorf("inspect DeepSeek Harness patch file %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return dshPatchSnapshot{}, fmt.Errorf("DeepSeek Harness patch file %s is a symlink; refusing an ambiguous profile overlay", path)
	}
	if !info.Mode().IsRegular() {
		return dshPatchSnapshot{}, fmt.Errorf("DeepSeek Harness patch file %s is not a regular file", path)
	}
	if info.Size() > dshPatchReadLimit {
		return dshPatchSnapshot{}, fmt.Errorf("DeepSeek Harness patch file %s exceeds %d bytes", path, dshPatchReadLimit)
	}
	file, err := os.Open(path)
	if err != nil {
		return dshPatchSnapshot{}, fmt.Errorf("open DeepSeek Harness patch file %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return dshPatchSnapshot{}, fmt.Errorf("inspect open DeepSeek Harness patch file %s: %w", path, err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return dshPatchSnapshot{}, fmt.Errorf("DeepSeek Harness patch file %s changed while it was opened", path)
	}
	raw, err := io.ReadAll(io.LimitReader(file, dshPatchReadLimit+1))
	if err != nil {
		return dshPatchSnapshot{}, fmt.Errorf("read DeepSeek Harness patch file %s: %w", path, err)
	}
	if len(raw) > dshPatchReadLimit {
		return dshPatchSnapshot{}, fmt.Errorf("DeepSeek Harness patch file %s exceeds %d bytes", path, dshPatchReadLimit)
	}
	after, err := os.Lstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, after) ||
		after.Size() != int64(len(raw)) {
		if err == nil {
			err = errors.New("file identity or size changed")
		}
		return dshPatchSnapshot{}, fmt.Errorf("DeepSeek Harness patch file %s changed while it was read: %w", path, err)
	}
	snapshot.exists = true
	snapshot.mode = info.Mode()
	snapshot.raw = bytes.Clone(raw)
	snapshot.fileInfo = after
	snapshot.lines, snapshot.trailingNewline = splitDSHPatchLines(raw)
	if err := classifyDSHPatchLayout(&snapshot); err != nil {
		return dshPatchSnapshot{}, err
	}
	return snapshot, nil
}

func splitDSHPatchLines(raw []byte) ([]string, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	text := string(raw)
	trailing := strings.HasSuffix(text, "\n")
	if trailing {
		text = text[:len(text)-1]
	}
	return strings.Split(text, "\n"), trailing
}

func joinDSHPatchLines(lines []string, trailingNewline bool) []byte {
	joined := strings.Join(lines, "\n")
	if trailingNewline || joined == "" {
		joined += "\n"
	}
	return []byte(joined)
}

// classifyDSHPatchLayout records the managed-block range and the file's
// top-level shape without parsing any foreign row. Refusals name the path so
// the operator can convert the file by hand.
func classifyDSHPatchLayout(snapshot *dshPatchSnapshot) error {
	beginCount, endCount := 0, 0
	for index, line := range snapshot.lines {
		trimmed := strings.TrimRight(line, " \t\r")
		switch {
		case trimmed == dshPatchBlockBeginMarker:
			beginCount++
			snapshot.blockBegin = index
		case trimmed == dshPatchBlockEndMarker:
			endCount++
			snapshot.blockEnd = index
		case strings.HasPrefix(trimmed, dshPatchBlockBeginPrefix) || strings.HasPrefix(trimmed, dshPatchBlockEndPrefix):
			return fmt.Errorf(
				"DeepSeek Harness patch file %s carries a Witself managed block at another version (%q); refusing to modify it",
				snapshot.path, trimmed,
			)
		}
	}
	if beginCount > 1 || endCount > 1 {
		return fmt.Errorf("DeepSeek Harness patch file %s contains multiple Witself managed blocks; refusing to modify it", snapshot.path)
	}
	if beginCount != endCount || (beginCount == 1 && snapshot.blockEnd < snapshot.blockBegin) {
		return fmt.Errorf("DeepSeek Harness patch file %s contains a truncated Witself managed block fence; refusing to modify it", snapshot.path)
	}
	if beginCount == 0 {
		snapshot.blockBegin, snapshot.blockEnd = -1, -1
	}

	significant := make([]int, 0, len(snapshot.lines))
	for index, line := range snapshot.lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		significant = append(significant, index)
	}
	if len(significant) == 0 {
		// A file holding only comments and blank lines has no top-level node
		// yet. Treat it exactly like the freshly generated `[]` shape so the
		// block can be added while every comment byte is retained.
		snapshot.shape = dshPatchShapeEmpty
		return nil
	}
	first := snapshot.lines[significant[0]]
	trimmedFirst := strings.TrimSpace(first)
	switch {
	case trimmedFirst == "[]":
		if len(significant) != 1 {
			snapshot.shape = dshPatchShapeUnsupported
			return nil
		}
		snapshot.shape = dshPatchShapeEmpty
		snapshot.emptyIndex = significant[0]
	case first == "-" || strings.HasPrefix(first, "- "):
		snapshot.shape = dshPatchShapeBlockSequence
	default:
		snapshot.shape = dshPatchShapeUnsupported
	}
	return nil
}

func dshUnsupportedPatchShapeError(path string) error {
	return fmt.Errorf(
		"DeepSeek Harness patch file %s is not a block-style YAML list; convert it to one `- ` entry per row (or an empty `[]`) before installing Witself",
		path,
	)
}

// planDSHPatchContent renders the exact bytes the patch file should hold once
// the managed block is present. Every foreign byte is preserved.
func planDSHPatchContent(snapshot dshPatchSnapshot, block []byte) ([]byte, error) {
	blockLines := strings.Split(string(block), "\n")
	if snapshot.blockPresent() {
		updated := make([]string, 0, len(snapshot.lines)+len(blockLines))
		updated = append(updated, snapshot.lines[:snapshot.blockBegin]...)
		updated = append(updated, blockLines...)
		updated = append(updated, snapshot.lines[snapshot.blockEnd+1:]...)
		return joinDSHPatchLines(updated, snapshot.trailingNewline || snapshot.blockEnd == len(snapshot.lines)-1), nil
	}
	switch snapshot.shape {
	case dshPatchShapeMissing:
		return joinDSHPatchLines(blockLines, true), nil
	case dshPatchShapeEmpty:
		updated := make([]string, 0, len(snapshot.lines)+len(blockLines))
		if snapshot.emptyIndex < 0 {
			updated = append(updated, snapshot.lines...)
			updated = append(updated, blockLines...)
			return joinDSHPatchLines(updated, true), nil
		}
		updated = append(updated, snapshot.lines[:snapshot.emptyIndex]...)
		updated = append(updated, blockLines...)
		updated = append(updated, snapshot.lines[snapshot.emptyIndex+1:]...)
		return joinDSHPatchLines(updated, true), nil
	case dshPatchShapeBlockSequence:
		updated := make([]string, 0, len(snapshot.lines)+len(blockLines))
		updated = append(updated, snapshot.lines...)
		updated = append(updated, blockLines...)
		return joinDSHPatchLines(updated, true), nil
	default:
		return nil, dshUnsupportedPatchShapeError(snapshot.path)
	}
}

// planDSHPatchRemoval deletes the fenced block exactly and restores the
// freshly generated `[]` shape when the managed rows were the file's only
// content. Comments above the block are retained.
func planDSHPatchRemoval(snapshot dshPatchSnapshot) ([]byte, error) {
	if !snapshot.blockPresent() {
		return nil, errors.New("DeepSeek Harness patch file has no Witself managed block to remove")
	}
	remaining := make([]string, 0, len(snapshot.lines))
	remaining = append(remaining, snapshot.lines[:snapshot.blockBegin]...)
	remaining = append(remaining, snapshot.lines[snapshot.blockEnd+1:]...)
	foreign := false
	for _, line := range remaining {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			foreign = true
			break
		}
	}
	if !foreign {
		for len(remaining) > 0 && strings.TrimSpace(remaining[len(remaining)-1]) == "" {
			remaining = remaining[:len(remaining)-1]
		}
		remaining = append(remaining, "[]")
	}
	return joinDSHPatchLines(remaining, true), nil
}

type dshPatchInstallPlan struct {
	desired       transcriptcapture.Config
	block         []byte
	expected      dshPatchSnapshot
	writeRequired bool
}

// prepareDSHPatchInstallPlan refuses to claim an unrecorded managed block and
// captures the exact preimage that the mutation must revalidate. The patch
// file is a plain owned file, so no provider mutation happens here.
func prepareDSHPatchInstallPlan(runtimeCLI string, desired transcriptcapture.Config, previous *transcriptcapture.Config) (dshPatchInstallPlan, bool, error) {
	if err := validateDSHCLISelection(runtimeCLI, desired); err != nil {
		return dshPatchInstallPlan{}, false, err
	}
	if previous != nil {
		if err := validateDSHPreviousSelection(runtimeCLI, desired, *previous); err != nil {
			return dshPatchInstallPlan{}, false, err
		}
	}
	block, err := dshManagedPatchBlock(desired)
	if err != nil {
		return dshPatchInstallPlan{}, false, err
	}
	snapshot, err := readDSHPatchSnapshot(desired.RuntimeMCPConfigPath)
	if err != nil {
		return dshPatchInstallPlan{}, false, err
	}
	// The top-level shape is refused whether or not a managed fence is already
	// present. A fence appended to a mapping or to a non-empty flow array is
	// not a document Witself can own, and accepting it because the block
	// happens to be there would let that drift survive reinstall.
	if snapshot.shape == dshPatchShapeUnsupported {
		return dshPatchInstallPlan{}, false, dshUnsupportedPatchShapeError(snapshot.path)
	}
	// A fence at this exact version is Witself's own region whatever its inner
	// bytes say: an interrupted first install, a wiped ~/.witself, or an editor
	// that re-quoted the rows must never lock the operator out of installing
	// or uninstalling. Foreign rows outside the fence are still untouchable.
	if previous != nil {
		if _, err := dshManagedPatchBlock(*previous); err != nil {
			return dshPatchInstallPlan{}, false, err
		}
	}
	// An already-correct block still needs a rewrite when the file is wider
	// than owner-only, otherwise a reinstall could never repair the mode.
	writeRequired := !bytes.Equal(snapshot.block(), block) ||
		(snapshot.exists && !integrationFileModeMatches(snapshot.mode, dshPatchFileMode))
	return dshPatchInstallPlan{
		desired: desired, block: block, expected: snapshot,
		writeRequired: writeRequired,
	}, false, nil
}

func installDSHPatchBlock(cfg transcriptcapture.Config) (bool, error) {
	plan, _, err := prepareDSHPatchInstallPlan(cfg.RuntimeCLICommand, cfg, &cfg)
	if err != nil {
		return false, err
	}
	return installDSHPatchBlockWithPlan(plan)
}

func installDSHPatchBlockWithPlan(plan dshPatchInstallPlan) (bool, error) {
	before, err := readDSHPatchSnapshot(plan.desired.RuntimeMCPConfigPath)
	if err != nil {
		return false, err
	}
	if !dshPatchSnapshotMatchesExact(plan.expected, before) {
		return false, &providerPreflightChangedError{err: fmt.Errorf(
			"DeepSeek Harness patch file %s changed after preflight; refusing to modify it", before.path,
		)}
	}
	if !plan.writeRequired {
		return false, nil
	}
	updated, err := planDSHPatchContent(before, plan.block)
	if err != nil {
		return false, err
	}
	if err := writeDSHPatchFileAtomic(before, updated); err != nil {
		// A restored preimage means nothing was committed, so the operation is
		// retryable rather than an unattributable mutation.
		var changed *providerPreflightChangedError
		if errors.As(err, &changed) {
			return false, err
		}
		return true, &providerMutationUncertainError{err: err}
	}
	after, err := readDSHPatchSnapshot(before.path)
	if err != nil {
		return true, &providerMutationUncertainError{err: err}
	}
	if !bytes.Equal(after.block(), plan.block) {
		return true, &providerMutationUncertainError{err: fmt.Errorf(
			"DeepSeek Harness did not retain the exact Witself managed block in %s", after.path,
		)}
	}
	if err := verifyDSHPatchFileMode(after); err != nil {
		return true, &providerMutationUncertainError{err: err}
	}
	return true, nil
}

func removeDSHPatchBlock(expected *transcriptcapture.Config) (bool, error) {
	if expected == nil {
		return false, errors.New("expected DeepSeek Harness binding is required for safe patch removal")
	}
	return removeDSHPatchBlockWithSnapshot(*expected, nil)
}

func removeDSHPatchBlockWithSnapshot(expected transcriptcapture.Config, expectedSnapshot *dshPatchSnapshot) (bool, error) {
	block, err := dshManagedPatchBlock(expected)
	if err != nil {
		return false, err
	}
	before, err := readDSHPatchSnapshot(expected.RuntimeMCPConfigPath)
	if err != nil {
		return false, err
	}
	if expectedSnapshot != nil && !dshPatchSnapshotMatchesExact(*expectedSnapshot, before) {
		return false, &providerPreflightChangedError{err: fmt.Errorf(
			"DeepSeek Harness patch file %s changed after preflight; refusing to remove the managed block", before.path,
		)}
	}
	if !before.blockPresent() {
		return false, nil
	}
	// The exact fence is Witself's region; remove it even when its inner bytes
	// drifted from the installed rendering, so an operator is never locked
	// into a file Witself will not install over. Only the rendering is
	// validated here so a corrupt binding cannot drive a removal.
	_ = block
	updated, err := planDSHPatchRemoval(before)
	if err != nil {
		return false, err
	}
	if err := writeDSHPatchFileAtomic(before, updated); err != nil {
		var changed *providerPreflightChangedError
		if errors.As(err, &changed) {
			return false, err
		}
		return true, &providerMutationUncertainError{err: err}
	}
	after, err := readDSHPatchSnapshot(before.path)
	if err != nil {
		return true, &providerMutationUncertainError{err: err}
	}
	if after.blockPresent() {
		return true, &providerMutationUncertainError{err: fmt.Errorf(
			"DeepSeek Harness retained the Witself managed block in %s after removal", after.path,
		)}
	}
	if err := verifyDSHPatchFileMode(after); err != nil {
		return true, &providerMutationUncertainError{err: err}
	}
	return true, nil
}

// verifyDSHPatchFileMode keeps the file that dictates dsh's MCP command
// owner-only, so no other local account can rewrite what dsh executes.
func verifyDSHPatchFileMode(snapshot dshPatchSnapshot) error {
	if snapshot.exists && !integrationFileModeMatches(snapshot.mode, dshPatchFileMode) {
		return fmt.Errorf(
			"DeepSeek Harness patch file %s permissions are %04o; want %04o",
			snapshot.path, snapshot.mode.Perm(), dshPatchFileMode.Perm(),
		)
	}
	return nil
}

func dshPatchSnapshotMatchesExact(left, right dshPatchSnapshot) bool {
	if left.path != right.path || left.exists != right.exists ||
		left.shape != right.shape || !bytes.Equal(left.raw, right.raw) {
		return false
	}
	if left.exists && !integrationFileModeMatches(left.mode, right.mode) {
		return false
	}
	if left.exists &&
		(left.fileInfo == nil || right.fileInfo == nil || !os.SameFile(left.fileInfo, right.fileInfo)) {
		return false
	}
	return true
}

// dshPatchBeforeMutationForTest lets deterministic tests replace the patch file
// in the otherwise tiny interval between the preflight read and the mutation.
// Production code leaves it nil.
var dshPatchBeforeMutationForTest func()

// writeDSHPatchFileAtomic installs data at the snapshot's path without ever
// discarding a concurrent edit. dsh generates this file and operators hold
// their own rows in it, while the Witself operation lock serializes Witself
// against Witself only. A plain rename would overwrite a second writer's bytes
// and report success, so the replacement keeps the displaced inode and proves
// it is the exact preimage the plan was built from before committing.
func writeDSHPatchFileAtomic(before dshPatchSnapshot, data []byte) error {
	directory := filepath.Dir(before.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", directory, err)
	}
	scratchPath, err := stageDSHPatchFile(directory, data)
	if err != nil {
		return err
	}
	scratchOwned := true
	defer func() {
		if scratchOwned {
			_ = os.Remove(scratchPath)
		}
	}()
	if dshPatchBeforeMutationForTest != nil {
		dshPatchBeforeMutationForTest()
	}
	if !before.exists {
		// A no-replace rename refuses to clobber a file that appeared since the
		// preflight read instead of silently adopting it.
		if err := renameManagedInstructionFileNoReplace(scratchPath, before.path); err != nil {
			if errors.Is(err, os.ErrExist) {
				return &providerPreflightChangedError{err: fmt.Errorf(
					"refuse to create %s because it appeared during update", before.path,
				)}
			}
			return fmt.Errorf("create %s atomically: %w", before.path, err)
		}
		scratchOwned = false
		return syncIntegrationTransactionNearestDirectory(directory)
	}
	if err := exchangeManagedInstructionFiles(before.path, scratchPath); err != nil {
		return fmt.Errorf("replace %s atomically: %w", before.path, err)
	}
	// scratchPath now holds whatever occupied the canonical path at the
	// mutation instant, so the generic cleanup must not unlink it by name.
	scratchOwned = false
	stateErr := verifyManagedInstructionsFileState(scratchPath, before.raw, before.fileInfo)
	if stateErr == nil {
		if err := os.Remove(scratchPath); err != nil {
			return fmt.Errorf("remove the displaced %s: %w", dshPatchFileName, err)
		}
		return syncIntegrationTransactionNearestDirectory(directory)
	}
	// The canonical path changed after the preflight read. Put the displaced
	// version back and keep every byte that cannot be restored under a named
	// path rather than deleting either version.
	if restoreErr := exchangeManagedInstructionFiles(before.path, scratchPath); restoreErr != nil {
		return fmt.Errorf(
			"refuse to replace %s because it changed during update (%v); recovery failed: %w; both versions are preserved at %s",
			before.path, stateErr, restoreErr, scratchPath,
		)
	}
	if !dshPatchFileHoldsExactly(scratchPath, data) {
		return fmt.Errorf(
			"refuse to replace %s because it changed during update (%v); preserved state: %s",
			before.path, stateErr, scratchPath,
		)
	}
	if err := os.Remove(scratchPath); err != nil {
		return fmt.Errorf(
			"refuse to replace %s because it changed during update (%v); the Witself candidate is preserved at %s: %w",
			before.path, stateErr, scratchPath, err,
		)
	}
	return &providerPreflightChangedError{err: fmt.Errorf(
		"refuse to replace %s because it changed during update: %w", before.path, stateErr,
	)}
}

func stageDSHPatchFile(directory string, data []byte) (string, error) {
	temporary, err := os.CreateTemp(directory, "."+dshPatchFileName+".witself-*")
	if err != nil {
		return "", fmt.Errorf("create temporary %s: %w", dshPatchFileName, err)
	}
	temporaryPath := temporary.Name()
	staged := false
	defer func() {
		if !staged {
			_ = temporary.Close()
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(dshPatchFileMode.Perm()); err != nil {
		return "", fmt.Errorf("set temporary %s permissions: %w", dshPatchFileName, err)
	}
	if _, err := temporary.Write(data); err != nil {
		return "", fmt.Errorf("write temporary %s: %w", dshPatchFileName, err)
	}
	if err := temporary.Sync(); err != nil {
		return "", fmt.Errorf("sync temporary %s: %w", dshPatchFileName, err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close temporary %s: %w", dshPatchFileName, err)
	}
	staged = true
	return temporaryPath, nil
}

func dshPatchFileHoldsExactly(path string, data []byte) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(data)) {
		return false
	}
	raw, err := os.ReadFile(path)
	return err == nil && bytes.Equal(raw, data)
}

// validateDSHInstalledTopology is the strict verification used by
// `witself integrations --verify`: the selectors, the owned files, and the
// composed-profile probe must all agree with the installed binding.
func validateDSHInstalledTopology(cfg transcriptcapture.Config) error {
	if err := validateDSHInstalledSelectors(cfg); err != nil {
		return err
	}
	return validateDSHPersistedTopology(cfg)
}

// validateDSHServeTopology guards `mcp serve`. It checks the selectors and the
// owned files only: booting dsh's Node runtime on every MCP server start would
// add seconds to each session and would take the credential-bound server down
// with any transient probe failure, while the byte-exact block is what dsh
// actually executes.
func validateDSHServeTopology(cfg transcriptcapture.Config) error {
	if err := validateDSHInstalledSelectors(cfg); err != nil {
		return err
	}
	return validateDSHPersistedTopologyWithProbe(cfg, false)
}

// validateDSHCommitTopology finalizes an install or a recovered install. A
// composed tree that omits the row is drift and fails the commit, but a probe
// that cannot run (dsh missing, a profile not yet materialized, a Node boot
// failure) is reported as a warning instead: the block was written and
// verified byte for byte, and `witself integrations --verify` reports the
// unavailable probe until dsh can run.
func validateDSHCommitTopology(cfg transcriptcapture.Config) (string, error) {
	if err := validateDSHInstalledSelectors(cfg); err != nil {
		return "", err
	}
	err := validateDSHPersistedTopology(cfg)
	var classified *integrationTopologyClassError
	if errors.As(err, &classified) && classified.class == integrationVerificationUnavailable {
		return fmt.Sprintf("the DeepSeek Harness composed-config probe could not run (%v); run `witself integrations --verify` once dsh can start", err), nil
	}
	return "", err
}

func validateDSHInstalledSelectors(cfg transcriptcapture.Config) error {
	root, err := currentDSHConfigRoot()
	if err != nil {
		return err
	}
	if root != cfg.RuntimeConfigRoot || cfg.RuntimeMCPConfigPath != filepath.Join(root, dshPatchFileName) {
		return fmt.Errorf("DSH_HOME changed from installed %s to %s; refusing to expose the credential-bound MCP server", cfg.RuntimeConfigRoot, root)
	}
	environment, err := captureDSHMCPEnvironment()
	if err != nil {
		return err
	}
	if !equalDSHEnvironment(environment, cfg.MCPEnvironment) {
		return errors.New("WITSELF_HOME changed from the installed DeepSeek Harness binding; refusing to expose the credential-bound MCP server")
	}
	return nil
}

func validateDSHPersistedTopology(cfg transcriptcapture.Config) error {
	return validateDSHPersistedTopologyWithProbe(cfg, true)
}

func validateDSHPersistedTopologyWithProbe(cfg transcriptcapture.Config, probe bool) error {
	if err := validateDSHCLISelection(cfg.RuntimeCLICommand, cfg); err != nil {
		return err
	}
	expected, err := dshManagedPatchBlock(cfg)
	if err != nil {
		return err
	}
	snapshot, err := readDSHPatchSnapshot(cfg.RuntimeMCPConfigPath)
	if err != nil {
		return err
	}
	if !snapshot.exists || !snapshot.blockPresent() {
		return incompleteIntegrationTopology(fmt.Errorf(
			"DeepSeek Harness patch file %s is missing the Witself managed block", snapshot.path,
		))
	}
	// A correct block inside a document Witself would refuse to own, or in a
	// file any local account can rewrite, is drift rather than health.
	if snapshot.shape == dshPatchShapeUnsupported {
		return dshUnsupportedPatchShapeError(snapshot.path)
	}
	if err := verifyDSHPatchFileMode(snapshot); err != nil {
		return err
	}
	if !bytes.Equal(snapshot.block(), expected) {
		return fmt.Errorf("DeepSeek Harness patch file %s no longer matches the installed Witself managed block", snapshot.path)
	}
	if _, err := parseDSHManagedPatchBlock(expected); err != nil {
		return err
	}
	if probe {
		if err := validateDSHComposedConfig(cfg); err != nil {
			return err
		}
	}
	return validateDSHManagedInstructionsAt(cfg.RuntimeConfigRoot)
}

// validateDSHComposedConfig proves that the home-level patch actually reaches
// a composed profile. dsh boots Node for this, so it is skipped when the
// pinned CLI is unavailable rather than reported as drift.
func validateDSHComposedConfig(cfg transcriptcapture.Config) error {
	info, err := os.Stat(cfg.RuntimeCLICommand)
	if err != nil || !integrationExecutableModeIsUsable(info) {
		return nil
	}
	expected, err := dshManagedPatchBlock(cfg)
	if err != nil {
		return err
	}
	rows, err := parseDSHManagedPatchBlock(expected)
	if err != nil {
		return err
	}
	raw, err := runDSHDumpConfig(
		cfg.RuntimeCLICommand, cfg.RuntimeConfigRoot, dshDumpConfigTimeout,
		"--profile", "headless", "--dump-config",
	)
	if err != nil {
		return unavailableIntegrationTopology(fmt.Errorf("dump the DeepSeek Harness composed config: %w", err))
	}
	for _, row := range rows {
		if !dshComposedConfigHasManagedRow(raw, row.id, row.name) {
			return fmt.Errorf(
				"the DeepSeek Harness composed config does not mount row %s as %s; the home-level patch is not in effect",
				row.id, row.name,
			)
		}
	}
	return nil
}

func validateDSHManagedInstructionsAt(configRoot string) error {
	spec, err := dshManagedInstructionsSpecAt(configRoot)
	if err != nil {
		return err
	}
	spec, err = normalizeManagedInstructionsSpec(spec)
	if err != nil {
		return err
	}
	snapshot, err := readManagedInstructionsSnapshot(spec)
	if err != nil {
		return err
	}
	if !snapshot.existed {
		return incompleteIntegrationTopology(errors.New("DeepSeek Harness managed instruction file is missing"))
	}
	if _, changed, err := upsertManagedInstructionsBlock(snapshot.data, spec); err != nil {
		return err
	} else if changed {
		return errors.New("DeepSeek Harness managed instructions no longer match the installed policy")
	}
	return nil
}

// dshPatchRow is one mounted plugin row inside the managed fence.
type dshPatchRow struct {
	id   string
	name string
}

// parseDSHManagedPatchBlock validates the rendered block's YAML structure
// without a generic round-trip and returns its rows in order. Only the exact
// shape Witself renders is accepted, so a malformed render is caught before it
// reaches a user's profile tree.
func parseDSHManagedPatchBlock(block []byte) ([]dshPatchRow, error) {
	lines, _ := splitDSHPatchLines(block)
	if len(lines) < 4 ||
		strings.TrimRight(lines[0], " \t") != dshPatchBlockBeginMarker ||
		strings.TrimRight(lines[len(lines)-1], " \t") != dshPatchBlockEndMarker {
		return nil, errors.New("the DeepSeek Harness managed block is not fenced by its exact markers")
	}
	body := lines[1 : len(lines)-1]
	if strings.TrimRight(body[0], " \t") != "- insert:" {
		return nil, errors.New("the DeepSeek Harness managed block must be one `- insert:` sequence entry")
	}
	var rows []dshPatchRow
	previousIndent := 0
	for index, line := range body[1:] {
		if strings.TrimSpace(line) == "" {
			return nil, errors.New("the DeepSeek Harness managed block must not contain blank lines")
		}
		if strings.ContainsRune(line, '\t') {
			return nil, errors.New("the DeepSeek Harness managed block must not contain tabs")
		}
		key, value, indent, ok := parseDSHConfigLine(line)
		if !ok || indent == 0 || indent%2 != 0 {
			return nil, fmt.Errorf("the DeepSeek Harness managed block line %d is not a valid YAML mapping entry", index+2)
		}
		if indent > previousIndent+2 && index != 0 {
			return nil, fmt.Errorf("the DeepSeek Harness managed block line %d skips an indentation level", index+2)
		}
		previousIndent = indent
		switch key {
		case "id":
			if !dshLineStartsSequenceItem(line, indent) {
				return nil, fmt.Errorf("the DeepSeek Harness managed block line %d must open a row", index+2)
			}
			rows = append(rows, dshPatchRow{id: value})
		case "name":
			if len(rows) == 0 || rows[len(rows)-1].name != "" {
				return nil, fmt.Errorf("the DeepSeek Harness managed block line %d sets a name outside a row", index+2)
			}
			rows[len(rows)-1].name = value
		}
	}
	if len(rows) == 0 {
		return nil, errors.New("the DeepSeek Harness managed block must mount at least one row")
	}
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.id == "" || row.name == "" {
			return nil, errors.New("the DeepSeek Harness managed block must set both id and name on every row")
		}
		if seen[row.id] {
			return nil, fmt.Errorf("the DeepSeek Harness managed block mounts row %s more than once", row.id)
		}
		seen[row.id] = true
	}
	return rows, nil
}

// dshComposedConfigHasManagedRow scans a composed `--dump-config` tree for the
// mounted row. Only the two fields Witself owns are inspected; foreign rows
// and `!!js` tagged values are ignored rather than parsed.
func dshComposedConfigHasManagedRow(raw []byte, id, name string) bool {
	lines, _ := splitDSHPatchLines(raw)
	for index, line := range lines {
		key, value, indent, ok := parseDSHConfigLine(line)
		if !ok || key != "id" || value != id {
			continue
		}
		if dshSiblingMappingValue(lines, index, indent, "name") == name {
			return true
		}
	}
	return false
}

// dshSiblingMappingValue reads one sibling key from the mapping that contains
// lines[index]. The mapping's bounds are the enclosing sequence entry, so a
// neighbouring row's field and a nested row borrowing an ancestor's field are
// both excluded.
func dshSiblingMappingValue(lines []string, index, indent int, want string) string {
	start := index
	if !dshLineStartsSequenceItem(lines[index], indent) {
		for cursor := index - 1; cursor >= 0; cursor-- {
			_, _, lineIndent, ok := parseDSHConfigLine(lines[cursor])
			if !ok || lineIndent < indent {
				break
			}
			start = cursor
			if lineIndent == indent && dshLineStartsSequenceItem(lines[cursor], indent) {
				break
			}
		}
	}
	for cursor := start; cursor < len(lines); cursor++ {
		key, value, lineIndent, ok := parseDSHConfigLine(lines[cursor])
		if !ok || lineIndent < indent {
			break
		}
		if cursor != start && lineIndent == indent && dshLineStartsSequenceItem(lines[cursor], indent) {
			break
		}
		if lineIndent == indent && key == want {
			return value
		}
	}
	return ""
}

func dshLineStartsSequenceItem(line string, indent int) bool {
	trimmed := strings.TrimLeft(line, " ")
	return len(line)-len(trimmed) == indent-2 && strings.HasPrefix(trimmed, "- ")
}

// parseDSHConfigLine reads one `key: value` mapping entry and its effective
// indentation. A `- key: value` sequence entry reports the indentation of the
// mapping it opens, so sibling lookup treats both spellings alike.
func parseDSHConfigLine(line string) (string, string, int, bool) {
	if strings.ContainsRune(line, '\t') {
		return "", "", 0, false
	}
	trimmed := strings.TrimLeft(line, " ")
	indent := len(line) - len(trimmed)
	if strings.HasPrefix(trimmed, "- ") {
		trimmed = strings.TrimLeft(trimmed[2:], " ")
		indent += 2
	}
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", 0, false
	}
	key, value, found := strings.Cut(trimmed, ":")
	if !found {
		return "", "", 0, false
	}
	if value != "" && !strings.HasPrefix(value, " ") {
		return "", "", 0, false
	}
	key = dshUnquoteYAMLScalar(strings.TrimSpace(key))
	if key == "" {
		return "", "", 0, false
	}
	return key, dshUnquoteYAMLScalar(strings.TrimSpace(value)), indent, true
}

func dshUnquoteYAMLScalar(value string) string {
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'")
	}
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return value[1 : len(value)-1]
	}
	return value
}

func runDSHDumpConfigCommand(runtimeCLI, configRoot string, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, runtimeCLI, args...)
	cleanup, err := isolateProviderCLIWorkingDirectory(cmd, "DeepSeek Harness")
	if err != nil {
		return nil, err
	}
	defer cleanup()
	cmd.WaitDelay = dshCLIWaitDelay
	cmd.Env = dshCommandEnvironment(os.Environ(), configRoot)
	output := &dshBoundedBuffer{limit: dshCLIOutputLimit}
	cmd.Stdout = output
	// dsh writes boot diagnostics to stderr and the composed tree to stdout, so
	// the streams stay separate instead of being parsed interleaved. stderr is
	// still captured, bounded, because it carries the only explanation of why a
	// probe failed.
	diagnostics := &dshBoundedBuffer{limit: dshCLIDiagnosticLimit}
	cmd.Stderr = diagnostics
	err = cmd.Run()
	raw, truncated := output.snapshot()
	if truncated {
		return nil, fmt.Errorf("DeepSeek Harness CLI output exceeds %d bytes", dshCLIOutputLimit)
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("DeepSeek Harness CLI timed out after %s: %w", timeout, ctx.Err())
	}
	if err != nil {
		if detail := dshCLIDiagnostic(diagnostics); detail != "" {
			return nil, fmt.Errorf("%w: %s", err, detail)
		}
		return nil, err
	}
	return raw, nil
}

// dshCLIDiagnostic renders the provider's own bounded explanation of a failed
// probe so an unavailable verdict is not reported as a bare exit status.
func dshCLIDiagnostic(diagnostics *dshBoundedBuffer) string {
	raw, truncated := diagnostics.snapshot()
	detail := strings.TrimSpace(string(raw))
	if detail == "" {
		return ""
	}
	detail = strings.Join(strings.Fields(detail), " ")
	if truncated {
		detail += " (truncated)"
	}
	return detail
}

func dshCommandEnvironment(inherited []string, configRoot string) []string {
	values := make(map[string]string, len(inherited))
	for _, entry := range inherited {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	for key := range values {
		if strings.EqualFold(key, "DSH_HOME") {
			delete(values, key)
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys)+1)
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return append(result, "DSH_HOME="+configRoot)
}

type dshBoundedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (buffer *dshBoundedBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining > 0 {
		write := min(len(value), remaining)
		_, _ = buffer.buffer.Write(value[:write])
	}
	if len(value) > remaining {
		buffer.truncated = true
	}
	return len(value), nil
}

func (buffer *dshBoundedBuffer) snapshot() ([]byte, bool) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return bytes.Clone(buffer.buffer.Bytes()), buffer.truncated
}
