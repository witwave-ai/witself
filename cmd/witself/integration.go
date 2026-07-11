package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const maxHookInputBytes = 16 * 1024 * 1024

func installCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself install codex|claude --agent NAME [--location home|work]")
		return 2
	}
	runtime, err := transcriptcapture.NormalizeRuntime(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	fs := flag.NewFlagSet("install "+args[0], flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	realm := fs.String("realm", "default", "local realm name")
	agent := fs.String("agent", "", "local agent name")
	location := fs.String("location", strings.TrimSpace(os.Getenv("WITSELF_LOCATION")), "stable human label for this machine, such as home or work")
	mode := fs.String("capture", transcriptcapture.ModeRaw, "messages|trace|raw")
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing an agent token")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if strings.TrimSpace(*agent) == "" && strings.TrimSpace(*tokenFile) == "" {
		fmt.Fprintln(os.Stderr, "witself: --agent is required when --token-file is not supplied")
		return 2
	}
	captureMode, err := transcriptcapture.NormalizeMode(*mode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	runtimeCLI, err := findRuntimeCLI(runtime)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	self, err := client.GetSelf(ctx, conn.Endpoint, conn.Token, client.SelfOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: verify agent identity: %v\n", err)
		return 1
	}
	if conn.AgentName != "" && conn.AgentName != self.Identity.AgentName {
		fmt.Fprintf(os.Stderr, "witself: local agent %q authenticates as %q; refusing to install an ambiguous binding\n", conn.AgentName, self.Identity.AgentName)
		return 1
	}
	loc, err := transcriptcapture.EnsureLocation(*location)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	accountName := conn.AccountName
	if accountName == "" {
		accountName = strings.TrimSpace(*account)
		if accountName == "" {
			accountName = "default"
		}
	}
	tokenPath := strings.TrimSpace(*tokenFile)
	if tokenPath != "" {
		tokenPath, err = filepath.Abs(tokenPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: resolve token path: %v\n", err)
			return 1
		}
	}
	cfg := transcriptcapture.Config{
		Runtime: runtime, CaptureMode: captureMode,
		Account: accountName, Realm: conn.RealmName, Agent: self.Identity.AgentName,
		AgentID: self.Identity.AgentID, AgentName: self.Identity.AgentName,
		Endpoint: strings.TrimSpace(*endpoint), TokenFile: tokenPath,
		Location: loc, InstalledAt: time.Now().UTC(),
	}
	if err := transcriptcapture.SaveConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "witself: save integration: %v\n", err)
		return 1
	}
	witselfExecutable, err := currentExecutablePath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: locate current executable: %v\n", err)
		return 1
	}
	if err := registerMCP(runtime, runtimeCLI, witselfExecutable); err != nil {
		fmt.Fprintf(os.Stderr, "witself: register MCP: %v\n", err)
		return 1
	}
	hookPath, err := transcriptcapture.InstallHooks(runtime, captureMode, witselfExecutable)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: install hooks: %v\n", err)
		return 1
	}

	fmt.Printf("installed %s for agent %s at %s\n", runtime, self.Identity.AgentName, loc.Name)
	fmt.Printf("hooks: %s\n", hookPath)
	fmt.Println("mcp: witself")
	if runtime == transcriptcapture.RuntimeCodex {
		fmt.Println("next: open /hooks in Codex once to review and trust the installed command hook")
	}
	return 0
}

func transcriptHook(args []string) int {
	fs := flag.NewFlagSet("transcript hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	runtime := fs.String("runtime", "", "codex|claude-code")
	if err := fs.Parse(args); err != nil {
		return 0
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, maxHookInputBytes+1))
	if err != nil || len(raw) > maxHookInputBytes {
		fmt.Fprintln(os.Stderr, "witself capture: hook input could not be queued")
		return 0
	}
	if _, err := transcriptcapture.EnqueueHook(*runtime, raw); err != nil {
		fmt.Fprintf(os.Stderr, "witself capture: %v\n", err)
		return 0
	}
	if os.Getenv("WITSELF_CAPTURE_NO_FLUSH") == "" {
		if err := startBackgroundFlush(*runtime); err != nil {
			fmt.Fprintf(os.Stderr, "witself capture: queued locally; background flush did not start: %v\n", err)
		}
	}
	return 0
}

func transcriptFlush(args []string) int {
	fs := flag.NewFlagSet("transcript flush", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	runtime := fs.String("runtime", "", "codex|claude-code")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	runtimeName, err := transcriptcapture.NormalizeRuntime(*runtime)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	release, acquired, err := transcriptcapture.AcquireFlushLock(runtimeName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: acquire capture lock: %v\n", err)
		return 1
	}
	if !acquired {
		return 0
	}
	lockHeld := true
	defer func() {
		if lockHeld {
			release()
		}
	}()
	pending, err := transcriptcapture.Pending(runtimeName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: read capture outbox: %v\n", err)
		return 1
	}
	if len(pending) == 0 {
		release()
		lockHeld = false
		remaining, err := transcriptcapture.Pending(runtimeName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: recheck capture outbox: %v\n", err)
			return 1
		}
		if len(remaining) > 0 {
			if err := startBackgroundFlush(runtimeName); err != nil {
				fmt.Fprintf(os.Stderr, "witself: restart capture flush: %v\n", err)
				return 1
			}
		}
		return 0
	}
	cfg, err := transcriptcapture.LoadConfig(runtimeName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, err := connectAgent(ctx, cfg.Account, cfg.Realm, cfg.Agent, cfg.Endpoint, cfg.TokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: connect capture agent: %v\n", err)
		return 1
	}
	flushed := 0
	for len(pending) > 0 {
		for _, pendingEvent := range pending {
			event := pendingEvent.Event
			tr, err := client.CreateTranscript(ctx, conn.Endpoint, conn.Token, client.CreateTranscriptInput{
				ExternalID: event.TranscriptExternalID(),
				Title:      event.TranscriptTitle(),
				Metadata:   event.TranscriptMetadata(),
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "witself: create capture transcript: %v\n", err)
				return 1
			}
			captureEntries := event.Entries()
			for start := 0; start < len(captureEntries); start += 100 {
				end := min(start+100, len(captureEntries))
				inputs := make([]client.AppendTranscriptEntryInput, end-start)
				for i, entry := range captureEntries[start:end] {
					inputs[i] = client.AppendTranscriptEntryInput{
						ExternalID: entry.ExternalID, Role: entry.Role, Body: entry.Body,
						Payload: entry.Payload, Model: entry.Model,
						ReplyToExternalID: entry.ReplyToExternalID,
					}
				}
				if _, err := client.AppendTranscriptEntries(ctx, conn.Endpoint, conn.Token, tr.ID, inputs); err != nil {
					fmt.Fprintf(os.Stderr, "witself: append capture event: %v\n", err)
					return 1
				}
			}
			if err := transcriptcapture.RemovePending(pendingEvent.Path); err != nil {
				fmt.Fprintf(os.Stderr, "witself: acknowledge capture event: %v\n", err)
				return 1
			}
			flushed++
		}
		pending, err = transcriptcapture.Pending(runtimeName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: read capture outbox: %v\n", err)
			return 1
		}
	}

	// Close the enqueue-vs-unlock race: an event whose competing flusher saw
	// this lock is visible after release and gets a fresh flusher.
	release()
	lockHeld = false
	remaining, err := transcriptcapture.Pending(runtimeName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: recheck capture outbox: %v\n", err)
		return 1
	}
	if len(remaining) > 0 {
		if err := startBackgroundFlush(runtimeName); err != nil {
			fmt.Fprintf(os.Stderr, "witself: restart capture flush: %v\n", err)
			return 1
		}
	}
	fmt.Fprintf(os.Stderr, "flushed %d %s transcript event(s)\n", flushed, runtimeName)
	return 0
}

func transcriptTail(args []string) int {
	transcriptID := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		transcriptID = strings.TrimSpace(args[0])
		args = args[1:]
	}
	fs := flag.NewFlagSet("transcript tail", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	realm := fs.String("realm", "default", "local realm name")
	agent := fs.String("agent", "", "local agent name")
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing an agent or operator token")
	limit := fs.Int("limit", 20, "newest entries to return (1-500)")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if transcriptID == "" {
		transcriptID = strings.TrimSpace(fs.Arg(0))
	}
	if transcriptID == "" || *limit < 1 || *limit > 500 {
		fmt.Fprintln(os.Stderr, "usage: witself transcript tail TRANSCRIPT_ID [--limit 20]")
		return 2
	}
	ctx := context.Background()
	var ep, tok string
	var err error
	if strings.TrimSpace(*agent) != "" {
		conn, connectErr := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
		err = connectErr
		ep, tok = conn.Endpoint, conn.Token
	} else {
		ep, tok, err = connectDomain(ctx, *account, *endpoint, *tokenFile)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	page, err := client.GetTranscriptPage(ctx, ep, tok, transcriptID, client.TranscriptPageOptions{Limit: *limit, Tail: true})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(page)
	}
	for _, entry := range page.Entries {
		fmt.Printf("--- %d  %s  %s  %s ---\n", entry.Sequence, entry.Role, entry.ID, formatTime(entry.CreatedAt))
		if entry.Body != "" {
			fmt.Printf("%s\n", safeText(entry.Body))
		}
		fmt.Println()
	}
	return 0
}

func startBackgroundFlush(runtime string) error {
	executable, err := currentExecutablePath()
	if err != nil {
		return err
	}
	cmd := exec.Command(executable, "transcript", "flush", "--runtime", runtime)
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func currentExecutablePath() (string, error) {
	if strings.ContainsRune(os.Args[0], filepath.Separator) {
		return filepath.Abs(os.Args[0])
	}
	return exec.LookPath(os.Args[0])
}

func findRuntimeCLI(runtime string) (string, error) {
	candidates := []string{}
	if runtime == transcriptcapture.RuntimeCodex {
		candidates = append(candidates, strings.TrimSpace(os.Getenv("CODEX_CLI_PATH")))
		if path, err := exec.LookPath("codex"); err == nil {
			candidates = append(candidates, path)
		}
		candidates = append(candidates, "/Applications/ChatGPT.app/Contents/Resources/codex")
	} else {
		candidates = append(candidates, strings.TrimSpace(os.Getenv("CLAUDE_CLI_PATH")))
		if path, err := exec.LookPath("claude"); err == nil {
			candidates = append(candidates, path)
		}
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := exec.CommandContext(ctx, candidate, "mcp", "add", "--help").Run()
		cancel()
		if err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no %s executable with MCP support was found", runtime)
}

func registerMCP(runtime, runtimeCLI, witselfExecutable string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var removeArgs, addArgs []string
	if runtime == transcriptcapture.RuntimeCodex {
		removeArgs = []string{"mcp", "remove", "witself"}
		addArgs = []string{"mcp", "add", "witself", "--", witselfExecutable, "mcp", "serve", "--runtime", runtime}
	} else {
		removeArgs = []string{"mcp", "remove", "--scope", "user", "witself"}
		addArgs = []string{"mcp", "add", "--scope", "user", "--transport", "stdio", "witself", "--", witselfExecutable, "mcp", "serve", "--runtime", runtime}
	}
	_ = exec.CommandContext(ctx, runtimeCLI, removeArgs...).Run()
	output, err := exec.CommandContext(ctx, runtimeCLI, addArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
