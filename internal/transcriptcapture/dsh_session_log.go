package transcriptcapture

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	// dshSessionLogMaxBytes bounds the on-disk artifact this reader will open.
	dshSessionLogMaxBytes = 64 * 1024 * 1024
	// dshSessionLogMaxRecords bounds decoded record count. A dsh session log
	// keeps every step, tool call, and request header for the life of the
	// session, so it is allowed many more records than a per-turn transcript.
	dshSessionLogMaxRecords   = 500_000
	dshSessionLogPollInterval = 50 * time.Millisecond
	dshSessionLogMaxWait      = 2 * time.Second
)

// dshSessionLogFilePattern matches the immutable generation artifacts dsh
// writes beside each session: `session.v<N>.jsonl` optionally Zstandard
// encoded. Only the highest generation is the live append target.
var dshSessionLogFilePattern = regexp.MustCompile(`^session\.v([0-9]{1,4})\.jsonl(\.zstd)?$`)

// dshZstandardMagic opens every Zstandard frame. The dsh container is a
// concatenation of independent checksummed frames, so the magic identifies the
// encoding from the artifact's first four bytes regardless of suffix.
var dshZstandardMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// A session log only ever grows, so a crossed bound is permanent: unlike a torn
// tail it never clears on retry. These are sentinels so the finalizer can settle
// the affected Stop event instead of blocking every later event of the same
// transcript behind a read that can only keep failing.
var (
	errDSHSessionLogTooLarge       = errors.New("dsh session log exceeds the bounded read limit")
	errDSHSessionLogTooManyRecords = errors.New("dsh session log exceeds the bounded record limit")
)

func dshSessionLogBoundExceeded(err error) bool {
	return errors.Is(err, errDSHSessionLogTooLarge) ||
		errors.Is(err, errDSHSessionLogTooManyRecords)
}

// dshSessionTurn projects one user prompt segment of a native dsh turn.
type dshSessionTurn struct {
	// Complete reports that the log contains this turn's `turn/end`. Anything
	// less is a turn dsh is still writing, never an empty response.
	Complete bool
	Sealed   bool
	// Unresolved distinguishes missing/ambiguous correlation from a matched
	// native turn that is still waiting for turn/end.
	Unresolved bool
	Body       string
	Model      string
	Provider   string
	Usage      dshTokenUsage
	// Steps holds every step of the turn in order. The last step's assistant
	// text is Body; the earlier steps are the turn's intermediate work.
	Steps []dshSessionStep
}

// dshSessionStep is one model call of a turn plus the tools it requested.
type dshTurnCorrelation struct {
	PromptSHA256   string
	PromptHookSeq  int64
	PromptAfterSeq int64
	StopOrdinal    int
}

type dshSessionStep struct {
	Step        int
	Text        string
	ToolCalls   []dshSessionToolCall
	ToolResults []dshSessionToolResult
}

type dshTokenUsage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

func (u dshTokenUsage) empty() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheReadTokens == 0 && u.CacheWriteTokens == 0
}

type dshSessionToolCall struct {
	CallID    string
	Name      string
	Arguments string
	Sealed    bool
}

type dshSessionToolResult struct {
	CallID  string
	Name    string
	Body    string
	IsError bool
	Sealed  bool
}

// dshSessionRecord is one line of the decoded JSONL container. The header line
// and every event share the `type` discriminator; only events carry `data`.
type dshSessionRecord struct {
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	Time int64           `json:"time"`
	Data json.RawMessage `json:"data"`
	// Header-only fields, present on the first record.
	Version         int    `json:"version"`
	ID              string `json:"id"`
	CWD             string `json:"cwd"`
	DelegationDepth *int   `json:"delegationDepth"`
	IsSeeded        *bool  `json:"isSeeded"`
}

type dshRecordData struct {
	Turn     *int              `json:"turn"`
	Step     *int              `json:"step"`
	CallID   string            `json:"callId"`
	Name     string            `json:"name"`
	Args     string            `json:"arguments"`
	Role     string            `json:"role"`
	Content  []dshContentBlock `json:"content"`
	Source   dshMessageSource  `json:"source"`
	Message  json.RawMessage   `json:"message"`
	Usage    *dshUsageRecord   `json:"usage"`
	Reason   dshTurnEndReason  `json:"reason"`
	Point    string            `json:"point"`
	HasError *struct {
		Name string `json:"name"`
		Code string `json:"code"`
	} `json:"error"`
}

type dshTurnEndReason struct {
	Kind string `json:"kind"`
}

type dshMessageSource struct {
	Kind     string `json:"kind"`
	Plugin   string `json:"plugin"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	CallID   string `json:"callId"`
}

type dshContentBlock struct {
	Type       string            `json:"type"`
	Text       string            `json:"text"`
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Arguments  string            `json:"arguments"`
	ToolCallID string            `json:"toolCallId"`
	IsError    bool              `json:"isError"`
	Content    []dshContentBlock `json:"content"`
}

type dshMessageRecord struct {
	ID      string            `json:"id"`
	Role    string            `json:"role"`
	Content []dshContentBlock `json:"content"`
	Source  dshMessageSource  `json:"source"`
}

type dshUsageRecord struct {
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens"`
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
}

// readCompleteDSHTurnWithin waits, bounded, for the dsh session log to contain
// the finished turn a Stop hook just observed. The bridge fires Stop from the
// harness's turn-stopping point, so the turn's `turn/end` may not be durable
// yet even though its last assistant message is. Quiet time proves nothing;
// only the provider's own turn fence does.
func readCompleteDSHTurnWithin(
	sessionID, cwd string, ordinal int,
	maxWait, pollInterval time.Duration, correlation ...dshTurnCorrelation,
) (dshSessionTurn, error) {
	deadline := time.Now().Add(maxWait)
	var cachedPath string
	var cachedInfo os.FileInfo
	var cachedTurn dshSessionTurn
	for {
		path, err := resolveDSHSessionLogPath(sessionID, cwd)
		if err != nil {
			return dshSessionTurn{}, err
		}
		turn := dshSessionTurn{Unresolved: true}
		if path != "" {
			info, statErr := os.Stat(path)
			if statErr != nil {
				return dshSessionTurn{}, statErr
			}
			if path != cachedPath || cachedInfo == nil || !os.SameFile(info, cachedInfo) ||
				info.Size() != cachedInfo.Size() || !info.ModTime().Equal(cachedInfo.ModTime()) {
				records, readErr := readDSHSessionLogRecords(path, sessionID)
				if readErr != nil {
					return dshSessionTurn{}, readErr
				}
				cachedTurn = projectDSHSessionTurn(records, ordinal, correlation...)
				cachedPath, cachedInfo = path, info
			}
			turn = cachedTurn
		}
		if turn.Complete {
			return turn, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return turn, nil
		}
		time.Sleep(min(pollInterval, remaining))
	}
}

// readDSHSessionTurn resolves the session's live log artifact and projects one
// turn out of it. A log that is missing, still being written, or does not yet
// contain the requested turn yields an incomplete result rather than an error,
// so the caller retries instead of losing the event.
func readDSHSessionTurn(sessionID, cwd string, ordinal int, correlation ...dshTurnCorrelation) (dshSessionTurn, error) {
	path, err := resolveDSHSessionLogPath(sessionID, cwd)
	if err != nil {
		return dshSessionTurn{}, err
	}
	if path == "" {
		return dshSessionTurn{}, nil
	}
	records, err := readDSHSessionLogRecords(path, sessionID)
	if err != nil {
		return dshSessionTurn{}, err
	}
	return projectDSHSessionTurn(records, ordinal, correlation...), nil
}

// countDSHSessionPrompts anchors a newly observed or resumed binding to the
// prompts already durable before SessionStart. Missing logs contain no prompts;
// the hook caller also treats an unreadable log as an unavailable anchor.
func countDSHSessionPrompts(sessionID, cwd string) (int, error) {
	path, err := resolveDSHSessionLogPath(sessionID, cwd)
	if err != nil || path == "" {
		return 0, err
	}
	records, err := readDSHSessionLogRecords(path, sessionID)
	if err != nil {
		return 0, err
	}
	return dshPromptCount(records), nil
}

// dshPromptRecordWatermark bounds the occurrence being submitted. The native
// user/message is written after UserPromptSubmit returns, so an already durable
// message cannot belong to this new hook, even when its text is identical.
func dshPromptRecordWatermark(sessionID, cwd string) int64 {
	path, err := resolveDSHSessionLogPath(sessionID, cwd)
	if err != nil || path == "" {
		return 0
	}
	records, err := readDSHSessionLogRecords(path, sessionID)
	if err != nil {
		return 0
	}
	var seq int64
	for _, record := range records {
		seq = max(seq, record.Seq)
	}
	return seq
}

func dshPromptCount(records []dshSessionRecord) int {
	count := 0
	for _, record := range records {
		if record.Type != "user/message" {
			continue
		}
		var data dshRecordData
		if json.Unmarshal(record.Data, &data) == nil && data.Source.Kind == "user" {
			count++
		}
	}
	return count
}

// dshPromptSHA256 is a local correlation value, never transcript content.
func dshPromptSHA256(prompt string) string {
	digest := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(digest[:])
}

// resolveDSHSessionLogPath finds the live artifact for one session id. It
// returns "" when nothing is resolvable yet, and an error only when a resolved
// candidate fails the trust chain: a real regular file, owned by this user and
// not group- or world-writable, reached through directories with the same
// property, under `$DSH_HOME/sessions`, and within the size cap.
func resolveDSHSessionLogPath(sessionID, cwd string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", errors.New("dsh session log lookup needs a session id")
	}
	segment := dshEncodeSegment(sessionID)
	if segment == "" || len(segment) > 255 {
		return "", errors.New("dsh session id does not encode to one path segment")
	}
	root, err := dshSessionsRoot()
	if err != nil {
		return "", err
	}
	sessionsRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	info, err := os.Stat(sessionsRoot)
	if err != nil || !info.IsDir() || !trustedPathIdentity(sessionsRoot, info) {
		return "", errors.New("dsh session store is not a trusted directory")
	}

	entries, err := os.ReadDir(sessionsRoot)
	if err != nil {
		return "", err
	}
	preferred := ""
	if trimmed := strings.TrimSpace(cwd); trimmed != "" {
		preferred = dshProjectKey(trimmed)
	}
	var candidates []string
	exact := ""
	for _, entry := range entries {
		if !entry.IsDir() {
			// A symlinked project directory is never followed: os.ReadDir
			// reports the link itself, and only a real directory is a
			// candidate.
			continue
		}
		dir := filepath.Join(sessionsRoot, entry.Name(), segment)
		if dirInfo, err := os.Lstat(dir); err != nil || !dirInfo.IsDir() {
			continue
		}
		if entry.Name() == preferred {
			exact = dir
		}
		candidates = append(candidates, dir)
	}
	switch {
	case exact != "":
		candidates = []string{exact}
	case len(candidates) == 0:
		return "", nil
	case len(candidates) > 1:
		// Two project directories hold a session with this id. Guessing which
		// one produced the Stop hook could attach another project's assistant
		// text to this turn, so leave the event pending instead.
		return "", nil
	}

	path, err := dshLiveGenerationPath(candidates[0])
	if err != nil || path == "" {
		return "", err
	}
	if err := assertTrustedDSHSessionLog(sessionsRoot, path); err != nil {
		return "", err
	}
	return path, nil
}

// dshLiveGenerationPath selects the highest `session.vN.jsonl[.zstd]`
// generation in one session directory. A generation present in both encodings
// is ambiguous and resolves to nothing.
func dshLiveGenerationPath(sessionDir string) (string, error) {
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	best, bestGeneration, duplicate := "", -1, false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		match := dshSessionLogFilePattern.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		generation, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		switch {
		case generation > bestGeneration:
			best, bestGeneration, duplicate = filepath.Join(sessionDir, entry.Name()), generation, false
		case generation == bestGeneration:
			duplicate = true
		}
	}
	if duplicate {
		return "", nil
	}
	return best, nil
}

func assertTrustedDSHSessionLog(sessionsRoot, path string) error {
	linked, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if linked.Mode()&os.ModeSymlink != 0 || !linked.Mode().IsRegular() {
		return errors.New("dsh session log must be a real regular file")
	}
	if !trustedPathIdentity(path, linked) {
		return errors.New("dsh session log is not owned by this user")
	}
	if linked.Size() > dshSessionLogMaxBytes {
		return errDSHSessionLogTooLarge
	}
	if !trustedNativeDirectoryChain(sessionsRoot, path) {
		return errors.New("dsh session log is outside the trusted session store")
	}
	return nil
}

// dshSessionsRoot uses the installed binding because dsh removes DSH_* from
// hook child environments. Without a loadable binding it mirrors dsh-home-paths.
func dshSessionsRoot() (string, error) {
	if cfg, err := LoadConfig(RuntimeDSH); err == nil && cfg.RuntimeConfigRoot != "" {
		return filepath.Join(cfg.RuntimeConfigRoot, "sessions"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	root := strings.TrimSpace(os.Getenv("DSH_HOME"))
	switch {
	case root == "":
		root = filepath.Join(home, ".dsh")
	case root == "~":
		root = home
	case strings.HasPrefix(root, "~/") || strings.HasPrefix(root, `~\`):
		root = filepath.Join(home, root[2:])
	}
	return filepath.Join(root, "sessions"), nil
}

// dshEncodeSegment reproduces the persistence backend's injective segment
// encoding. `A-Za-z0-9._-` survive literally except `~`; every other UTF-16
// code unit becomes `~XXXX`, and the two traversal segments are escaped whole.
func dshEncodeSegment(raw string) string {
	if raw == "" {
		return ""
	}
	if raw == "." {
		return "~002E"
	}
	if raw == ".." {
		return "~002E~002E"
	}
	var out strings.Builder
	for _, unit := range utf16Units(raw) {
		if unit != '~' && dshSafeSegmentUnit(unit) {
			out.WriteByte(byte(unit))
			continue
		}
		fmt.Fprintf(&out, "~%04X", unit)
	}
	return out.String()
}

// dshProjectKey reproduces the backend's human-navigable project directory
// name. Path and drive separators collapse to a single `-`, the same escape
// applies to every other unsafe unit, leading separators are dropped, and the
// readable part is capped before the `--…--` wrapper is applied.
func dshProjectKey(cwd string) string {
	if cwd == "" {
		return ""
	}
	var readable strings.Builder
	separatorRun := false
	for _, unit := range utf16Units(cwd) {
		switch {
		case unit == '/' || unit == '\\' || unit == ':':
			if !separatorRun {
				readable.WriteByte('-')
			}
			separatorRun = true
		case unit != '~' && dshSafeSegmentUnit(unit):
			readable.WriteByte(byte(unit))
			separatorRun = false
		default:
			fmt.Fprintf(&readable, "~%04X", unit)
			separatorRun = false
		}
	}
	trimmed := strings.TrimLeft(readable.String(), "-")
	if trimmed == "" {
		trimmed = "root"
	}
	if len(trimmed) > 251 {
		trimmed = trimmed[:251]
	}
	return "--" + trimmed + "--"
}

func dshSafeSegmentUnit(unit uint16) bool {
	switch {
	case unit >= 'A' && unit <= 'Z', unit >= 'a' && unit <= 'z', unit >= '0' && unit <= '9':
		return true
	case unit == '.' || unit == '_' || unit == '-':
		return true
	default:
		return false
	}
}

// utf16Units yields the UTF-16 code units of a Go string, matching the
// JavaScript `charCodeAt` loop the backend encodes with. Invalid UTF-8 is
// replaced the same way a Go range loop would, which cannot occur for a path
// dsh itself wrote.
func utf16Units(value string) []uint16 {
	units := make([]uint16, 0, len(value))
	for _, r := range value {
		if r > 0xFFFF {
			r -= 0x10000
			units = append(units, uint16(0xD800+(r>>10)), uint16(0xDC00+(r&0x3FF)))
			continue
		}
		units = append(units, uint16(r))
	}
	return units
}

// readDSHSessionLogRecords decodes the container and parses its lines. A
// truncated trailing frame or line is dropped: dsh appends whole frames, so a
// partial tail is a write in flight, not corruption.
func readDSHSessionLogRecords(path, expectedSessionID string) ([]dshSessionRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, dshSessionLogMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > dshSessionLogMaxBytes {
		return nil, errDSHSessionLogTooLarge
	}
	decoded, err := decodeDSHSessionLog(raw)
	if err != nil {
		return nil, err
	}

	lines := bytes.Split(decoded, []byte{'\n'})
	if len(lines) != 0 && len(lines[len(lines)-1]) != 0 {
		// dsh terminates every durable line. An unterminated tail is a partial
		// append this read must not interpret.
		lines = lines[:len(lines)-1]
	}
	records := make([]dshSessionRecord, 0, len(lines))
	headerRead := false
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if len(records) >= dshSessionLogMaxRecords {
			return nil, errDSHSessionLogTooManyRecords
		}
		var record dshSessionRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("parse dsh session log: %w", err)
		}
		// The binding runs on the first record of the log, not the first line:
		// a blank line before the header must not turn the header into an
		// ordinary record and let one session's Stop be finalized from another
		// session's log.
		if !headerRead {
			headerRead = true
			if record.Type != "session" {
				return nil, errors.New("dsh session log does not begin with its session header")
			}
			if strings.TrimSpace(expectedSessionID) != "" && record.ID != expectedSessionID {
				return nil, errors.New("dsh session log header names a different session")
			}
			// Preserve the native session provenance for delegated-capture
			// suppression on both the live hook and recovery paths.
		}
		records = append(records, record)
	}
	return records, nil
}

func decodeDSHSessionLog(raw []byte) ([]byte, error) {
	if !bytes.HasPrefix(raw, dshZstandardMagic) {
		return raw, nil
	}
	reader, err := zstd.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("open dsh session log: %w", err)
	}
	defer reader.Close()
	var decoded bytes.Buffer
	scanner := bufio.NewReaderSize(reader, 64*1024)
	if _, err := decoded.ReadFrom(io.LimitReader(scanner, dshSessionLogMaxBytes+1)); err != nil {
		// A torn trailing frame is the expected shape of a log dsh is still
		// appending to. Whatever whole frames decoded before it stay usable;
		// the caller decides completeness from the turn fence, not from the
		// container.
		if decoded.Len() == 0 {
			return nil, fmt.Errorf("decode dsh session log: %w", err)
		}
		return decoded.Bytes(), nil
	}
	if decoded.Len() > dshSessionLogMaxBytes {
		return nil, errDSHSessionLogTooLarge
	}
	return decoded.Bytes(), nil
}

// projectDSHSessionTurn selects one turn and folds it into the shape a
// finalized Stop event needs. An anchored ordinal selects a user prompt; its
// local digest validates that selection and recovers a shifted ordinal. A
// legacy Stop without either anchor follows the latest started turn so an open
// current turn can never be confused with its completed predecessor.
func projectDSHSessionTurn(records []dshSessionRecord, ordinal int, correlation ...dshTurnCorrelation) dshSessionTurn {
	if !dshSessionHeaderProvesRoot(records) {
		// A delegated session labels its inherited prompt source:user. The
		// entire child session stays value-free, including recovery after
		// missing tool hooks or a later apparent user boundary.
		delegated := dshSessionHeaderIsDelegated(records)
		return dshSessionTurn{Complete: delegated, Sealed: delegated, Unresolved: !delegated}
	}
	type promptAnchor struct {
		turn, record, stepStart   int
		firstOrdinal, lastOrdinal int
		text                      string
		matches                   bool
		hookSeqs                  []int64
	}
	currentTurn, currentStepStart := -1, -1
	lastEndedTurn := -1
	promptCount := 0
	digest := ""
	var promptHookSeq, promptAfterSeq int64
	var pendingHookSeqs []int64
	stopOrdinal := 0
	if len(correlation) != 0 {
		digest = correlation[0].PromptSHA256
		promptHookSeq, promptAfterSeq = correlation[0].PromptHookSeq, correlation[0].PromptAfterSeq
		stopOrdinal = correlation[0].StopOrdinal
	}
	ended := map[int]bool{}
	prompts := []promptAnchor{}

	for index, record := range records {
		var data dshRecordData
		if len(record.Data) != 0 {
			_ = json.Unmarshal(record.Data, &data)
		}
		switch record.Type {
		case "turn/start":
			pendingHookSeqs = nil
			if data.Turn != nil {
				currentTurn, currentStepStart = *data.Turn, index
			}
		case "step/start":
			if data.Turn != nil && *data.Turn == currentTurn {
				currentStepStart = index
			}
		case "turn/end":
			if data.Turn != nil {
				ended[*data.Turn] = true
				lastEndedTurn = *data.Turn
			}
		case "hook/invoked":
			if data.Point == "UserPromptSubmit" {
				pendingHookSeqs = append(pendingHookSeqs, record.Seq)
			}
		case "user/message":
			if data.Source.Kind != "user" {
				continue
			}
			promptCount++
			// A single pre-step bridge hook can claim several human inbox
			// messages. They form one prompt and one assistant segment;
			// plugin context interleaved in the same step is not prompt text.
			if len(prompts) == 0 || prompts[len(prompts)-1].turn != currentTurn ||
				prompts[len(prompts)-1].stepStart != currentStepStart {
				prompts = append(prompts, promptAnchor{
					turn: currentTurn, record: index, stepStart: currentStepStart,
					firstOrdinal: promptCount,
					hookSeqs:     pendingHookSeqs,
				})
				pendingHookSeqs = nil
			}
			anchor := &prompts[len(prompts)-1]
			anchor.lastOrdinal = promptCount
			anchor.text += dshVisibleText(data.Content)
		}
	}

	selected := -1
	matchingPrompt, matchCount := -1, 0
	for index := range prompts {
		anchor := &prompts[index]
		anchor.matches = digest != "" && dshPromptSHA256(anchor.text) == digest && records[anchor.record].Seq > promptAfterSeq
		if promptHookSeq > 0 {
			found := false
			for _, seq := range anchor.hookSeqs {
				found = found || seq == promptHookSeq
			}
			anchor.matches = anchor.matches && found
		}
		if anchor.matches {
			matchingPrompt, matchCount = index, matchCount+1
		}
		if ordinal >= anchor.firstOrdinal && ordinal <= anchor.lastOrdinal {
			selected = index
		}
	}
	if digest != "" {
		if selected < 0 || selected >= len(prompts) || !prompts[selected].matches {
			selected = -1
			if matchCount == 1 {
				selected = matchingPrompt
			}
		}
		if selected < 0 {
			// Missing batches and duplicate text cannot establish identity.
			// Keep polling; only the durable retry deadline may settle this.
			return dshSessionTurn{Unresolved: true}
		}
	} else if ordinal <= 0 {
		// A legacy Stop must wait for the latest started turn, even while
		// the previous turn already has a completion fence.
		for index := len(prompts) - 1; index >= 0; index-- {
			if prompts[index].turn == currentTurn {
				selected = index
				break
			}
		}
	}
	if selected < 0 || selected >= len(prompts) {
		if ordinal <= 0 && digest == "" {
			target := currentTurn
			if target < 0 {
				target = lastEndedTurn
			}
			if target >= 0 {
				turn := collectDSHTurn(records, target)
				turn.Complete = ended[target]
				return turn
			}
		}
		return dshSessionTurn{}
	}
	anchor := prompts[selected]
	if anchor.turn < 0 {
		return dshSessionTurn{}
	}
	start := anchor.stepStart
	if start < 0 {
		start = anchor.record
	}
	end := len(records)
	if selected+1 < len(prompts) && prompts[selected+1].turn == anchor.turn {
		// Mid-turn steering emits another prompt and Stop inside the same
		// native turn. Project just this prompt's steps and accounting,
		// while still waiting for the provider's original turn/end fence.
		next := prompts[selected+1]
		end = next.stepStart
		if end <= anchor.record {
			end = next.record
		}
	}
	segment := records[start:end]
	if stopOrdinal > 0 {
		var found bool
		var offset int
		segment, offset, found = dshStopSegment(segment, anchor.turn, stopOrdinal)
		if !found {
			return dshSessionTurn{Unresolved: true}
		}
		start += offset
	}
	turn := collectDSHTurn(segment, anchor.turn, dshHistorySealed(records[:start]))
	turn.Complete = ended[anchor.turn]
	return turn
}

// dshStopSegment separates stopping attempts within one human prompt. A
// plugin continuation can trigger another Stop without another user/message.
// Several configured hook commands may emit Stop markers for the same step;
// those markers describe one stopping attempt and must count only once.
func dshStopSegment(records []dshSessionRecord, target, ordinal int) ([]dshSessionRecord, int, bool) {
	stepStart, lastStopStep := -1, -2
	segmentStart, count := 0, 0
	for index, record := range records {
		var data dshRecordData
		if json.Unmarshal(record.Data, &data) != nil || data.Turn == nil || *data.Turn != target {
			continue
		}
		if record.Type == "step/start" {
			stepStart = index
		}
		if record.Type != "hook/invoked" || data.Point != "Stop" || stepStart == lastStopStep {
			continue
		}
		count++
		if count == ordinal {
			return records[segmentStart:index], segmentStart, true
		}
		lastStopStep, segmentStart = stepStart, index+1
	}
	// Existing fixtures and old bridge logs can omit hook markers entirely.
	// They can substantiate only the first Stop's unsplit prompt segment.
	if count == 0 && ordinal == 1 {
		return records, 0, true
	}
	return nil, 0, false
}

// dshHistorySealed folds privacy evidence before an output segment. Neither a
// Stop nor turn/end proves a human boundary: plugin/goal work can continue with
// the same revealed values. Only a native, nonempty source:user message resets
// the fence, matching the hook provenance requirement.
func dshHistorySealed(records []dshSessionRecord) bool {
	sealed := false
	callNames := map[string]string{}
	for _, record := range records {
		var data dshRecordData
		if json.Unmarshal(record.Data, &data) != nil {
			continue
		}
		switch record.Type {
		case "user/message":
			if data.Source.Kind == "user" && dshVisibleText(data.Content) != "" {
				sealed = false
			}
		case "tool/call":
			callNames[data.CallID] = data.Name
			sealed = sealed || dshSealedToolPayload(data.Name, data.Args)
		case "tool/result":
			var message dshMessageRecord
			if json.Unmarshal(data.Message, &message) != nil {
				continue
			}
			callID := message.Source.CallID
			if callID == "" && len(message.Content) > 0 {
				callID = message.Content[0].ToolCallID
			}
			sealed = sealed || dshSealedToolPayload(callNames[callID], dshVisibleToolResultText(message.Content))
		}
	}
	return sealed
}

func collectDSHTurn(records []dshSessionRecord, target int, precedingSeal ...bool) dshSessionTurn {
	turn := dshSessionTurn{}
	order := []int{}
	byStep := map[int]*dshSessionStep{}
	sealedCalls := map[string]bool{}
	callNames := map[string]string{}
	lastModel, lastProvider := "", ""
	// Records arrive in the order the harness ran them, so this reproduces the
	// hook plane's SensitiveTurn fence: once the turn has handled sealed
	// material, every later tool payload and every later assistant text is a
	// possible copy of it.
	sealedTurn := len(precedingSeal) > 0 && precedingSeal[0]

	stepOf := func(step int) *dshSessionStep {
		entry, ok := byStep[step]
		if !ok {
			entry = &dshSessionStep{Step: step}
			byStep[step] = entry
			order = append(order, step)
		}
		return entry
	}

	for _, record := range records {
		var data dshRecordData
		if len(record.Data) != 0 {
			_ = json.Unmarshal(record.Data, &data)
		}
		if record.Type == "user/message" && data.Source.Kind == "user" && dshVisibleText(data.Content) != "" {
			sealedTurn = false
		}
		if data.Turn == nil || *data.Turn != target {
			continue
		}
		step := 0
		if data.Step != nil {
			step = *data.Step
		}
		switch record.Type {
		case "step/start", "assistant/attempt":
			// A failed final model call has no assistant/message. Preserve its
			// empty step so preceding intermediate text never becomes Body.
			stepOf(step)
		case "assistant/message":
			var message dshMessageRecord
			if len(data.Message) == 0 || json.Unmarshal(data.Message, &message) != nil {
				continue
			}
			entry := stepOf(step)
			if !sealedTurn {
				entry.Text = dshVisibleText(message.Content)
			}
			if model := strings.TrimSpace(message.Source.Model); model != "" {
				lastModel = model
			}
			if provider := strings.TrimSpace(message.Source.Provider); provider != "" {
				lastProvider = provider
			}
			if data.Usage != nil {
				// A turn is one Stop event, so its accounting is the sum over
				// every model call the turn made, not just the final one.
				turn.Usage.InputTokens += data.Usage.InputTokens
				turn.Usage.OutputTokens += data.Usage.OutputTokens
				turn.Usage.CacheReadTokens += data.Usage.CacheReadTokens
				turn.Usage.CacheWriteTokens += data.Usage.CacheWriteTokens
			}
		case "tool/call":
			name := strings.TrimSpace(data.Name)
			sealed := sealedTurn || dshSealedToolPayload(name, data.Args)
			sealedTurn = sealed
			callNames[data.CallID] = name
			sealedCalls[data.CallID] = sealed
			call := dshSessionToolCall{CallID: data.CallID, Name: name, Sealed: sealed}
			if !sealed {
				call.Arguments = data.Args
			}
			entry := stepOf(step)
			entry.ToolCalls = append(entry.ToolCalls, call)
		case "tool/result":
			var message dshMessageRecord
			if len(data.Message) == 0 || json.Unmarshal(data.Message, &message) != nil {
				continue
			}
			callID := strings.TrimSpace(message.Source.CallID)
			if callID == "" && len(message.Content) != 0 {
				callID = strings.TrimSpace(message.Content[0].ToolCallID)
			}
			result := dshSessionToolResult{
				CallID:  callID,
				Name:    callNames[callID],
				IsError: data.HasError != nil,
				Sealed:  sealedTurn || sealedCalls[callID],
			}
			if len(message.Content) != 0 {
				result.IsError = result.IsError || message.Content[0].IsError
			}
			body := dshVisibleToolResultText(message.Content)
			if !result.Sealed && dshSealedToolPayload(result.Name, body) {
				// PostToolUse seals on the response as well as the request, so
				// a wrapper whose result names the sealed tool it dispatched to
				// is sealed even when the request did not say so.
				result.Sealed = true
			}
			sealedTurn = result.Sealed
			if !result.Sealed {
				// A sealed tool's model-facing result may carry the exact
				// revealed value the hook plane deliberately suppressed.
				result.Body = body
			}
			entry := stepOf(step)
			entry.ToolResults = append(entry.ToolResults, result)
		}
	}

	sort.Ints(order)
	turn.Steps = make([]dshSessionStep, 0, len(order))
	for _, step := range order {
		turn.Steps = append(turn.Steps, *byStep[step])
	}
	if len(turn.Steps) != 0 {
		turn.Body = turn.Steps[len(turn.Steps)-1].Text
	}
	turn.Model, turn.Provider = lastModel, lastProvider
	return turn
}

// dshSealedToolPayload decides sealing for one session-log tool record the way
// sensitiveToolEvent decides it for a hook payload. The sealed tool name is only
// the first of the hook plane's three fences: an MCP wrapper call whose payload
// names a sealed tool, and a shell call whose command line invokes the sealed
// CLI, carry the same values under an unremarkable tool name. Standing in for
// hook events that were never captured means standing in for all three.
func dshSealedToolPayload(name string, payloads ...string) bool {
	raw := make([]json.RawMessage, 0, len(payloads))
	for _, payload := range payloads {
		raw = append(raw, dshToolPayloadJSON(payload))
	}
	return dshSensitiveToolPayload(name, raw...)
}

// dshToolPayloadJSON reinterprets a session-log payload as the JSON document
// the hook plane's payload scanners walk. dsh stores tool arguments as a string
// and tool results as text; a payload that is not itself JSON carries none of
// the keys those scanners key on.
func dshToolPayloadJSON(payload string) json.RawMessage {
	trimmed := strings.TrimSpace(payload)
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return nil
	}
	return json.RawMessage(trimmed)
}

// dshVisibleText concatenates the model-visible text of one message. Reasoning
// blocks are deliberately excluded: dsh records them separately and they are
// not the assistant's answer.
func dshVisibleText(blocks []dshContentBlock) string {
	var out strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			out.WriteString(block.Text)
		}
	}
	return out.String()
}

func dshVisibleToolResultText(blocks []dshContentBlock) string {
	var out strings.Builder
	for _, block := range blocks {
		if block.Type == "tool-result" {
			out.WriteString(dshVisibleText(block.Content))
			continue
		}
		if block.Type == "text" {
			out.WriteString(block.Text)
		}
	}
	return out.String()
}
