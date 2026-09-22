package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

// Clipboard writes occur only after a deliberate copy action. Values travel
// over stdin, never command arguments, shell text, terminal escapes, or logs.
// Like the browser console, this uses a best-effort clear after 45 seconds.
// Clearing may overwrite a subsequent clipboard value from another program;
// it is not a promise that a clipboard manager or synced device forgot a copy.
type tuiClipboard struct {
	mu         sync.Mutex
	write      func(context.Context, string) error
	timer      *time.Timer
	generation uint64
	copied     bool
	closed     bool
}

func newTUIClipboard() *tuiClipboard {
	c := &tuiClipboard{}
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "pbcopy"
	case "windows":
		name = "clip.exe"
	case "linux":
		if path, err := exec.LookPath("wl-copy"); err == nil && os.Getenv("WAYLAND_DISPLAY") != "" {
			name = path
		} else if path, err := exec.LookPath("xclip"); err == nil && os.Getenv("DISPLAY") != "" {
			name, args = path, []string{"-selection", "clipboard"}
		} else if path, err := exec.LookPath("xsel"); err == nil && os.Getenv("DISPLAY") != "" {
			name, args = path, []string{"--clipboard", "--input"}
		}
	}
	if name == "" {
		return c
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return c
	}
	c.write = func(ctx context.Context, value string) error {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, path, args...)
		if runtime.GOOS == "windows" {
			// clip.exe recognizes a UTF-16LE BOM, including for non-ASCII facts.
			var buf bytes.Buffer
			buf.Write([]byte{0xff, 0xfe})
			for _, r := range utf16.Encode([]rune(value)) {
				buf.WriteByte(byte(r))
				buf.WriteByte(byte(r >> 8))
			}
			cmd.Stdin = &buf
		} else {
			cmd.Stdin = strings.NewReader(value)
		}
		// nil output streams are discarded by os/exec. Do not return a
		// provider's stderr, since a clipboard utility may echo input.
		if err := cmd.Run(); err != nil {
			return errors.New("clipboard unavailable")
		}
		return nil
	}
	return c
}

func (c *tuiClipboard) Copy(ctx context.Context, value string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.write == nil {
		return errors.New("clipboard unavailable")
	}
	// A utility can accept the value and then fail or be canceled before
	// reporting success. Keep cleanup responsibility for every attempted write.
	err := c.write(ctx, value)
	c.copied = true
	c.generation++
	generation := c.generation
	if c.timer != nil {
		c.timer.Stop()
	}
	c.timer = time.AfterFunc(45*time.Second, func() { c.clear(generation) })
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if c.write(cleanup, "") == nil {
			c.copied = false
			c.timer.Stop()
		}
	}
	return err
}

func (c *tuiClipboard) clear(generation uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.copied || c.generation != generation {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if c.write(ctx, "") == nil {
		c.copied = false
	}
}

func (c *tuiClipboard) Close() {
	c.mu.Lock()
	c.closed = true
	if c.timer != nil {
		c.timer.Stop()
	}
	generation := c.generation
	c.mu.Unlock()
	c.clear(generation)
}
