//go:build linux

package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise both real executable entrypoints with a test-only observation at
// syscall.Exec. Scheduling main/TestMain on another thread makes the regression
// deterministic: an initializer dispatches before that scheduling can happen.
// The overlay changes neither helper argument validation nor its handshake.
func TestProcessHelperDispatchInitialThread(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal("helper source root unavailable")
	}
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	for _, entrypoint := range []string{"test_binary", "harness"} {
		t.Run(entrypoint, func(t *testing.T) {
			scratch := t.TempDir()
			replacements := make(map[string]string)
			overlaySource := func(relative string, transform func(string) string) {
				t.Helper()
				original := filepath.Join(root, filepath.FromSlash(relative))
				data, err := os.ReadFile(original)
				if err != nil {
					t.Fatal("helper source unavailable")
				}
				source := strings.ReplaceAll(string(data), "\r\n", "\n")
				changed := transform(source)
				if changed == source {
					t.Fatal("helper observation insertion unavailable")
				}
				target := filepath.Join(scratch, filepath.Base(relative))
				if os.WriteFile(target, []byte(changed), 0o600) != nil {
					t.Fatal("helper source overlay unavailable")
				}
				replacements[original] = target
			}
			overlaySource("internal/localcell/launch_unix.go", func(source string) string {
				return strings.Replace(source, "\tif syscall.Exec(binary,", `	if syscall.Gettid() != os.Getpid() {
		_, _ = os.Stderr.WriteString("helper dispatch did not run on initial OS thread\n")
		os.Exit(124)
	}
	if syscall.Exec(binary,`, 1)
			})
			wrapper := `
// Test-only scheduling adversary. No identifier values leave this process.
func helperDispatchNonLeader(run func()) {
	runtime.LockOSThread()
	if syscall.Gettid() != os.Getpid() {
		run()
		return
	}
	go func() {
		runtime.LockOSThread()
		run()
	}()
	select {}
}
`
			var buildArgs []string
			if entrypoint == "test_binary" {
				overlaySource("internal/localcell/process_test.go", func(source string) string {
					source = strings.Replace(source, "import (", "import (\n\"runtime\"\n\"syscall\"", 1)
					source = strings.Replace(source, "func TestMain(m *testing.M) {", "func helperDispatchOriginalTestMain(m *testing.M) {", 1)
					return source + wrapper + "\nfunc TestMain(m *testing.M) { helperDispatchNonLeader(func() { helperDispatchOriginalTestMain(m) }) }\n"
				})
				buildArgs = []string{"test", "-c", "./internal/localcell"}
			} else {
				overlaySource("internal/tools/localcellharness/main.go", func(source string) string {
					source = strings.Replace(source, "func main() {", "func helperDispatchOriginalMain() {", 1)
					return source + wrapper + "\nfunc main() { helperDispatchNonLeader(helperDispatchOriginalMain) }\n"
				})
				buildArgs = []string{"build", "./internal/tools/localcellharness"}
			}
			overlay, err := json.Marshal(struct{ Replace map[string]string }{replacements})
			if err != nil {
				t.Fatal("helper overlay encoding failed")
			}
			overlayPath := filepath.Join(scratch, "overlay.json")
			if os.WriteFile(overlayPath, overlay, 0o600) != nil {
				t.Fatal("helper overlay write failed")
			}
			binary := filepath.Join(scratch, "helper")
			args := append([]string{buildArgs[0], "-overlay", overlayPath, "-o", binary}, buildArgs[1:]...)
			build := exec.CommandContext(ctx, "go", args...)
			build.Dir = root
			if build.Run() != nil {
				t.Fatal("instrumented helper compilation failed")
			}
			cellRoot := filepath.Join(scratch, "cell")
			if os.Mkdir(cellRoot, 0o700) != nil || os.Mkdir(filepath.Join(cellRoot, "run"), 0o700) != nil {
				t.Fatal("helper private root unavailable")
			}
			// true accepts the normal helper's serve argument and needs no listener.
			replacement, err := exec.LookPath("true")
			if err != nil {
				t.Fatal("helper replacement unavailable")
			}
			child := exec.CommandContext(ctx, binary, childLaunchArgument, cellRoot, "server", replacement)
			child.Env = []string{}
			child.Stdin = bytes.NewReader([]byte{1})
			output, err := child.CombinedOutput()
			if bytes.Contains(output, []byte("helper dispatch did not run on initial OS thread")) {
				t.Fatal("helper dispatch did not run on initial OS thread")
			}
			if err != nil || len(output) != 0 {
				t.Fatal("helper did not complete its observed exec dispatch")
			}
		})
	}
}
