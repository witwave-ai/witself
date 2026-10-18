package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// Keep the cell's bound above IDLE_TIMEOUT_MS in
// infra/cloudflare/control-plane/src/account-backup-runtime.mjs so the control
// plane's 120-second idle watchdog can abandon the pull first.
const defaultExportWriteStallTimeout = 150 * time.Second

func exportWriteStallTimeout(cfg Config) time.Duration {
	if cfg.ExportWriteStallTimeout > 0 {
		return cfg.ExportWriteStallTimeout
	}
	return defaultExportWriteStallTimeout
}

// exportWriteGuard bounds each write call and each flush, not the whole stream.
// A slow database read between writes does not count. An archive write can carry
// about 64 KiB of compressed data and must finish within 150 seconds by default:
// a reader slower than about 440 bytes per second for that write can be cut off.
type exportWriteGuard struct {
	writer io.Writer
	rc     *http.ResponseController
	idle   time.Duration
	cancel context.CancelFunc
}

func newExportWriteGuard(w http.ResponseWriter, idle time.Duration, cancel context.CancelFunc) *exportWriteGuard {
	return &exportWriteGuard{writer: w, rc: http.NewResponseController(w), idle: idle, cancel: cancel}
}

func (g *exportWriteGuard) arm() error {
	err := g.rc.SetWriteDeadline(time.Now().Add(g.idle))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	if err != nil {
		g.cancel()
	}
	return err
}

func (g *exportWriteGuard) Write(p []byte) (int, error) {
	if err := g.arm(); err != nil {
		return 0, err
	}
	n, err := g.writer.Write(p)
	if err != nil {
		g.cancel()
	}
	return n, err
}

func (g *exportWriteGuard) Flush() error {
	if err := g.arm(); err != nil {
		return err
	}
	err := g.rc.Flush()
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	if err != nil {
		g.cancel()
	}
	return err
}

// finish also leaves a fresh deadline for net/http's final response framing.
func (g *exportWriteGuard) finish() error {
	return g.Flush()
}
