package transcriptcapture

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/witwave-ai/witself/internal/local"
)

// Operator release holds deliberately live outside capture/state: installed
// hooks may still use an older executable that rewrites that directory without
// preserving new fields and removes session files on SessionEnd. Both capture
// and upload load this separate record through loadSessionState.
func operatorReleaseStatePath(runtime, sessionID string) (string, error) {
	runtime, err := NormalizeRuntime(runtime)
	if err != nil {
		return "", err
	}
	home, err := local.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "capture", "operator-releases", runtime, sessionHash(sessionID)+".json"), nil
}

func loadOperatorReleaseState(runtime, sessionID string) (*operatorReleaseState, error) {
	path, err := operatorReleaseStatePath(runtime, sessionID)
	if err != nil {
		return nil, err
	}
	file, _, err := openTrustedRegularFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read operator release state: %w", err)
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, maxSessionStateBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw) > maxSessionStateBytes {
		return nil, errors.New("operator release state is invalid")
	}
	var state operatorReleaseState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("parse operator release state: %w", err)
	}
	if (state.Status != "pending" && state.Status != "completed") || state.Since.IsZero() || len(state.ReleasedTurnIDs) == 0 {
		return nil, errors.New("operator release state is invalid")
	}
	return &state, nil
}

func saveOperatorReleaseState(runtime, sessionID string, state *operatorReleaseState) error {
	path, err := operatorReleaseStatePath(runtime, sessionID)
	if err != nil {
		return err
	}
	return writeJSONAtomic(path, state)
}
