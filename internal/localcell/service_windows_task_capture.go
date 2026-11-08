package localcell

import "sync"

const windowsTaskOutputLimit = 1 << 20

// stdout and stderr share one budget even though os/exec may write concurrently.
// Untrusted bytes beyond the cap are drained, not retained. A truncated result
// is never handed to the parser or classified as an absent registration.
type windowsTaskCapture struct {
	mu        sync.Mutex
	data      []byte
	errorData []byte
	used      int
	overflow  bool
}
type windowsTaskCaptureWriter struct {
	capture *windowsTaskCapture
	keep    bool
}

func (c *windowsTaskCapture) stdout() windowsTaskCaptureWriter {
	return windowsTaskCaptureWriter{c, true}
}
func (c *windowsTaskCapture) stderr() windowsTaskCaptureWriter {
	return windowsTaskCaptureWriter{c, false}
}
func (w windowsTaskCaptureWriter) Write(p []byte) (int, error) {
	c := w.capture
	c.mu.Lock()
	defer c.mu.Unlock()
	remaining := windowsTaskOutputLimit - c.used
	n := min(len(p), remaining)
	if w.keep {
		c.data = append(c.data, p[:n]...)
	} else {
		c.errorData = append(c.errorData, p[:n]...)
	}
	c.used += n
	c.overflow = c.overflow || n != len(p)
	return len(p), nil
}
func (c *windowsTaskCapture) exceeded() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.overflow }
func (c *windowsTaskCapture) output() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.data...)
}
