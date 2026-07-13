package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const maxHookInputBytes = 16 * 1024 * 1024

const witselfExecutableTestEnv = "WITSELF_TEST_EXECUTABLE_PATH"

func installCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself install RUNTIME[,RUNTIME...] [--agent NAME] [--location home|work]")
		return 2
	}
	targets, err := runtimeTargets(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	if len(targets) > 1 {
		for _, target := range targets {
			if code := installCmd(append([]string{target}, args[1:]...)); code != 0 {
				return code
			}
		}
		return 0
	}
	runtime := targets[0]
	fs := flag.NewFlagSet("install "+args[0], flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	realm := fs.String("realm", "", `local realm name (default: WITSELF_REALM or "default")`)
	agent := fs.String("agent", "", "local agent name")
	location := fs.String("location", strings.TrimSpace(os.Getenv("WITSELF_LOCATION")), "stable human label for this machine, such as home or work")
	mode := fs.String("capture", transcriptcapture.ModeRaw, "messages|trace|raw")
	managedHooks := fs.Bool("managed-hooks", supportsManagedHooks(runtime), "install administrator-managed hooks without per-hook runtime approval")
	userHooks := fs.Bool("user-hooks", false, "install user-scoped hooks instead of requesting administrator access")
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing an agent token")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	setFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
	if *userHooks && setFlags["managed-hooks"] && *managedHooks {
		fmt.Fprintln(os.Stderr, "witself: --user-hooks conflicts with --managed-hooks")
		return 2
	}
	if *managedHooks && !supportsManagedHooks(runtime) {
		fmt.Fprintf(os.Stderr, "witself: %s uses approval-free global user hooks; --managed-hooks is not supported\n", runtime)
		return 2
	}
	useManagedHooks := *managedHooks && !*userHooks

	previousConfig, previousConfigErr := transcriptcapture.LoadConfig(runtime)
	if previousConfigErr != nil && !errors.Is(previousConfigErr, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "witself: read existing integration: %v\n", previousConfigErr)
		return 1
	}
	if previousConfigErr == nil {
		accountProvided := setFlags["account"] || strings.TrimSpace(os.Getenv("WITSELF_ACCOUNT")) != ""
		realmProvided := setFlags["realm"] || strings.TrimSpace(os.Getenv("WITSELF_REALM")) != ""
		agentProvided := setFlags["agent"] || strings.TrimSpace(os.Getenv("WITSELF_AGENT")) != ""
		locationProvided := setFlags["location"] || strings.TrimSpace(os.Getenv("WITSELF_LOCATION")) != ""
		credentialsProvided := setFlags["endpoint"] || setFlags["token-file"]
		if !accountProvided {
			*account = previousConfig.Account
		}
		if !realmProvided {
			*realm = previousConfig.Realm
		}
		if !agentProvided && !credentialsProvided {
			*agent = previousConfig.Agent
		}
		if !locationProvided {
			*location = previousConfig.Location.Name
		}
		if !setFlags["capture"] {
			*mode = previousConfig.CaptureMode
		}
		if !setFlags["endpoint"] {
			*endpoint = previousConfig.Endpoint
		}
		if !setFlags["token-file"] {
			*tokenFile = previousConfig.TokenFile
		}
	}
	if strings.TrimSpace(*agent) == "" && strings.TrimSpace(os.Getenv("WITSELF_AGENT")) == "" && strings.TrimSpace(*tokenFile) == "" {
		inferredAgent, err := inferInstallAgent(*account, *realm)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 2
		}
		*agent = inferredAgent
	}
	captureMode, err := transcriptcapture.NormalizeMode(*mode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	hookMode := transcriptcapture.HookModeUser
	if useManagedHooks {
		hookMode = transcriptcapture.HookModeManaged
	}
	runtimeCLI, err := findRuntimeCLI(runtime)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	runtimeVersion := detectRuntimeVersion(runtimeCLI)
	if runtimeVersion == "" && previousConfigErr == nil {
		runtimeVersion = previousConfig.RuntimeVersion
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
	if err := verifyInstallAgentIdentity(conn, self.Identity); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
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
		Runtime: runtime, RuntimeVersion: runtimeVersion, CaptureMode: captureMode, HookMode: hookMode,
		Account: accountName, AccountID: self.Identity.AccountID,
		Realm: conn.RealmName, RealmID: self.Identity.RealmID,
		Agent: self.Identity.AgentName, AgentID: self.Identity.AgentID, AgentName: self.Identity.AgentName,
		Endpoint: strings.TrimSpace(*endpoint), TokenFile: tokenPath,
		Location: loc, InstalledAt: time.Now().UTC(),
	}
	previousHooks, err := snapshotRuntimeHooks(runtime)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: inspect existing runtime hooks: %v\n", err)
		return 1
	}
	stagedConfig := cfg
	if previousConfigErr == nil {
		stagedConfig.HookMode = previousConfig.HookMode
	}
	if err := transcriptcapture.SaveConfig(stagedConfig); err != nil {
		fmt.Fprintf(os.Stderr, "witself: save integration: %v\n", err)
		return 1
	}
	restoreConfig := func() error {
		if previousConfigErr == nil {
			return transcriptcapture.SaveConfig(previousConfig)
		}
		return transcriptcapture.RemoveConfig(runtime)
	}
	witselfExecutable, err := currentExecutablePath()
	if err != nil {
		if rollbackErr := restoreConfig(); rollbackErr != nil {
			fmt.Fprintf(os.Stderr, "witself: warning: restore integration config: %v\n", rollbackErr)
		}
		fmt.Fprintf(os.Stderr, "witself: locate current executable: %v\n", err)
		return 1
	}
	memoryRouting, err := installRuntimeMemoryRoutingInstructions(runtime)
	if err != nil {
		if rollbackErr := restoreConfig(); rollbackErr != nil {
			fmt.Fprintf(os.Stderr, "witself: warning: restore integration config: %v\n", rollbackErr)
		}
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	var previousBinding *transcriptcapture.Config
	if previousConfigErr == nil {
		previous := previousConfig
		previousBinding = &previous
	}
	rollbackInstall := func(mcpTouched, hooksTouched bool) {
		if rollbackErr := restoreConfig(); rollbackErr != nil {
			fmt.Fprintf(os.Stderr, "witself: warning: restore integration config: %v\n", rollbackErr)
		}
		if hooksTouched {
			hookBinding := previousBinding
			if hookBinding == nil && (previousHooks.userPresent || previousHooks.managedPresent) {
				current := cfg
				hookBinding = &current
			}
			if rollbackErr := restoreRuntimeHooksSnapshot(runtime, witselfExecutable, hookBinding, previousHooks); rollbackErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: restore runtime hooks: %v\n", rollbackErr)
			}
		}
		if mcpTouched {
			if rollbackErr := restoreRuntimeMCPBinding(runtime, runtimeCLI, witselfExecutable, previousBinding); rollbackErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: restore MCP registration: %v\n", rollbackErr)
			}
		}
		if !memoryRouting.managed {
			return
		}
		if rollbackErr := memoryRouting.restore(); rollbackErr != nil {
			fmt.Fprintf(os.Stderr, "witself: warning: restore %s memory routing instructions: %v\n", memoryRouting.displayName, rollbackErr)
		}
	}
	if err := registerMCP(runtime, runtimeCLI, witselfExecutable, accountName, conn.RealmName, self.Identity.AgentName, loc.Name); err != nil {
		rollbackInstall(true, false)
		fmt.Fprintf(os.Stderr, "witself: register MCP: %v\n", err)
		return 1
	}
	var hookPath string
	if useManagedHooks {
		hookPath, err = installManagedRuntimeHooks(runtime, captureMode, witselfExecutable, accountName, conn.RealmName, self.Identity.AgentName, loc.Name)
		if err == nil {
			_, err = transcriptcapture.RemoveHooks(runtime)
		}
	} else {
		hookPath, err = transcriptcapture.InstallHooks(runtime, captureMode, witselfExecutable, accountName, conn.RealmName, self.Identity.AgentName, loc.Name)
		if err == nil && previousConfigErr == nil && previousConfig.HookMode == transcriptcapture.HookModeManaged {
			_, err = removeManagedRuntimeHooks(runtime)
		}
	}
	if err != nil {
		rollbackInstall(true, true)
		fmt.Fprintf(os.Stderr, "witself: install hooks: %v\n", err)
		return 1
	}
	if err := transcriptcapture.SaveConfig(cfg); err != nil {
		rollbackInstall(true, true)
		fmt.Fprintf(os.Stderr, "witself: finalize integration: %v\n", err)
		return 1
	}

	if loc.Name == "" {
		fmt.Printf("installed %s for agent %s\n", runtime, self.Identity.AgentName)
	} else {
		fmt.Printf("installed %s for agent %s at %s\n", runtime, self.Identity.AgentName, loc.Name)
	}
	fmt.Printf("hooks: %s (%s)\n", hookPath, hookMode)
	fmt.Println("mcp: witself")
	if memoryRouting.managed {
		fmt.Printf("memory routing: %s (managed)\n", memoryRouting.path)
	}
	if useManagedHooks {
		if memoryRouting.managed {
			fmt.Printf("next: restart %s and start a new task to load the managed memory-routing instructions; managed hooks require no per-hook review\n", memoryRouting.displayName)
		} else {
			fmt.Printf("next: restart %s; managed hooks require no per-hook review\n", runtime)
		}
	} else if runtime == transcriptcapture.RuntimeCodex {
		fmt.Println("next: open /hooks in Codex once to review and trust the installed command hook; then start a new Codex task (or restart Codex) to load the managed memory-routing instructions")
	} else if memoryRouting.managed {
		fmt.Printf("next: restart %s and start a new task to load the managed memory-routing instructions; global user hooks require no project trust\n", memoryRouting.displayName)
	} else {
		fmt.Printf("next: restart %s; global user hooks require no project trust\n", runtime)
	}
	return 0
}

// verifyInstallAgentIdentity pins an integration to the principal selected by
// the caller instead of merely trusting that the supplied token authenticates.
// Managed account resolution gives us an exact account id; explicit endpoint
// installs may not, but their requested realm and agent names still bind the
// token. The authenticated ids are persisted after this check and become the
// stronger runtime guard used by MCP.
func verifyInstallAgentIdentity(conn agentConnection, identity client.SelfIdentity) error {
	if identity.AccountID == "" || identity.RealmID == "" || identity.AgentID == "" ||
		identity.RealmName == "" || identity.AgentName == "" {
		return errors.New("server returned an incomplete agent identity; refusing to install")
	}
	if conn.AccountID != "" && conn.AccountID != identity.AccountID {
		return fmt.Errorf("local account %s authenticates against account %s; refusing to install an ambiguous binding", conn.AccountID, identity.AccountID)
	}
	if conn.RealmName != "" && conn.RealmName != identity.RealmName {
		return fmt.Errorf("requested realm %q authenticates as %q; refusing to install an ambiguous binding", conn.RealmName, identity.RealmName)
	}
	if conn.AgentName != "" && conn.AgentName != identity.AgentName {
		return fmt.Errorf("requested agent %q authenticates as %q; refusing to install an ambiguous binding", conn.AgentName, identity.AgentName)
	}
	return nil
}

func inferInstallAgent(accountName, realmName string) (string, error) {
	resolvedAccount, _, err := local.ResolveAccount(accountName)
	if err != nil {
		return "", err
	}
	realmName = strings.TrimSpace(realmName)
	if realmName == "" {
		realmName = strings.TrimSpace(os.Getenv("WITSELF_REALM"))
	}
	if realmName == "" {
		realmName = "default"
	}
	names, err := local.AgentNames(resolvedAccount, realmName)
	if err != nil {
		return "", err
	}
	switch len(names) {
	case 0:
		return "", fmt.Errorf("no local agent credential found for %s/%s; pass --agent after creating its token", resolvedAccount, realmName)
	case 1:
		return names[0], nil
	default:
		return "", fmt.Errorf("multiple local agents found for %s/%s (%s); pass --agent NAME", resolvedAccount, realmName, strings.Join(names, ", "))
	}
}

func uninstallCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself uninstall RUNTIME[,RUNTIME...] [--managed-hooks]")
		return 2
	}
	targets, err := runtimeTargets(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	if len(targets) > 1 {
		code := 0
		for _, target := range targets {
			if targetCode := uninstallCmd(append([]string{target}, args[1:]...)); targetCode != 0 {
				code = targetCode
			}
		}
		return code
	}
	runtime := targets[0]
	fs := flag.NewFlagSet("uninstall "+args[0], flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	managedHooks := fs.Bool("managed-hooks", false, "also remove administrator-managed hooks")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *managedHooks && !supportsManagedHooks(runtime) {
		fmt.Fprintf(os.Stderr, "witself: %s does not support administrator-managed hooks\n", runtime)
		return 2
	}
	cfg, cfgErr := transcriptcapture.LoadConfig(runtime)
	if cfgErr != nil && !errors.Is(cfgErr, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "witself: read integration: %v\n", cfgErr)
		return 1
	}
	if errors.Is(cfgErr, os.ErrNotExist) {
		fmt.Fprintf(
			os.Stderr,
			"witself: no %s integration record; no changes made (reinstall it to reconstruct a rollback-safe binding, then uninstall)\n",
			runtime,
		)
		return 1
	}
	// Reject malformed managed routing before invoking any runtime CLI. This is
	// intentionally read-only: CLI availability is the second preflight, and no
	// local integration state is changed unless both checks pass.
	if err := preflightRuntimeMemoryRoutingRemoval(runtime); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	previousHooks, err := snapshotRuntimeHooks(runtime)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: inspect existing runtime hooks: %v\n", err)
		return 1
	}
	var previousBinding *transcriptcapture.Config
	witselfExecutable := ""
	if cfgErr == nil {
		previous := cfg
		previousBinding = &previous
		witselfExecutable, err = currentExecutablePath()
		if err != nil {
			witselfExecutable, err = os.Executable()
			if err != nil {
				fmt.Fprintf(os.Stderr, "witself: locate current executable for rollback: %v\n", err)
				return 1
			}
		}
	}
	runtimeCLI, runtimeCLIErr := findRuntimeCLI(runtime)
	if runtime != transcriptcapture.RuntimeCursor && runtimeCLIErr != nil {
		// Non-Cursor MCP registrations are owned by the runtime CLI. Preserve all
		// local state so a later retry can remove the complete integration.
		fmt.Fprintf(os.Stderr, "witself: cannot remove MCP registration: %v\n", runtimeCLIErr)
		return 1
	}
	// Remove the preflighted managed instruction block before irreversible
	// teardown. Its snapshot is cheap to restore if a later hook, MCP, or config
	// operation fails.
	memoryRouting, err := removeRuntimeMemoryRoutingInstructions(runtime)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	rollbackUninstall := func(hooksTouched, mcpTouched bool) {
		if mcpTouched && previousBinding != nil && (runtimeCLIErr == nil || runtime == transcriptcapture.RuntimeCursor) {
			if rollbackErr := restoreRuntimeMCPBinding(runtime, runtimeCLI, witselfExecutable, previousBinding); rollbackErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: restore MCP registration: %v\n", rollbackErr)
			}
		}
		if hooksTouched && previousBinding != nil {
			if rollbackErr := restoreRuntimeHooksSnapshot(runtime, witselfExecutable, previousBinding, previousHooks); rollbackErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: restore runtime hooks: %v\n", rollbackErr)
			}
		}
		if memoryRouting.managed {
			if rollbackErr := memoryRouting.restore(); rollbackErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: restore %s memory routing instructions: %v\n", memoryRouting.displayName, rollbackErr)
			}
		}
	}
	removeManaged := *managedHooks || (cfgErr == nil && cfg.HookMode == transcriptcapture.HookModeManaged)
	if removeManaged {
		if _, err := removeManagedRuntimeHooks(runtime); err != nil {
			rollbackUninstall(true, false)
			fmt.Fprintf(os.Stderr, "witself: remove managed hooks: %v\n", err)
			return 1
		}
	}
	if _, err := transcriptcapture.RemoveHooks(runtime); err != nil {
		rollbackUninstall(true, false)
		fmt.Fprintf(os.Stderr, "witself: remove user hooks: %v\n", err)
		return 1
	}
	if runtime == transcriptcapture.RuntimeCursor {
		if err := unregisterMCP(runtime, runtimeCLI); err != nil {
			rollbackUninstall(true, true)
			fmt.Fprintf(os.Stderr, "witself: unregister MCP: %v\n", err)
			return 1
		}
	} else if runtimeCLIErr == nil {
		if err := unregisterMCP(runtime, runtimeCLI); err != nil {
			rollbackUninstall(true, true)
			fmt.Fprintf(os.Stderr, "witself: unregister MCP: %v\n", err)
			return 1
		}
	} else {
		fmt.Fprintf(os.Stderr, "witself: warning: MCP registration was not removed: %v\n", runtimeCLIErr)
	}
	if err := transcriptcapture.RemoveConfig(runtime); err != nil {
		rollbackUninstall(true, runtime == transcriptcapture.RuntimeCursor || runtimeCLIErr == nil)
		fmt.Fprintf(os.Stderr, "witself: remove integration: %v\n", err)
		return 1
	}
	fmt.Printf("uninstalled %s integration; tokens and pending transcript events were preserved\n", runtime)
	return 0
}

func runtimeTargets(value string) ([]string, error) {
	parts := strings.Split(value, ",")
	targets := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, errors.New("runtime list contains an empty item")
		}
		runtimeName, err := transcriptcapture.NormalizeRuntime(part)
		if err != nil {
			return nil, err
		}
		if !seen[runtimeName] {
			targets = append(targets, runtimeName)
			seen[runtimeName] = true
		}
	}
	return targets, nil
}

func transcriptHook(args []string) int {
	fs := flag.NewFlagSet("transcript hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	runtime := fs.String("runtime", "", "codex|claude-code|grok-build|cursor")
	account := fs.String("account", "", "installed account name")
	realm := fs.String("realm", "", "installed realm name")
	agent := fs.String("agent", "", "installed agent name")
	location := fs.String("location", "", "optional installation location label")
	if err := fs.Parse(args); err != nil {
		return 0
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, maxHookInputBytes+1))
	if err != nil || len(raw) > maxHookInputBytes {
		fmt.Fprintln(os.Stderr, "witself capture: hook input could not be queued")
		return 0
	}
	if _, err := transcriptcapture.EnqueueHookForBinding(*runtime, *account, *realm, *agent, *location, raw); err != nil {
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
	runtime := fs.String("runtime", "", "codex|claude-code|grok-build|cursor")
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
	if override := strings.TrimSpace(os.Getenv(witselfExecutableTestEnv)); override != "" {
		return filepath.Abs(override)
	}
	var path string
	var err error
	if strings.ContainsRune(os.Args[0], filepath.Separator) {
		path, err = filepath.Abs(os.Args[0])
	} else {
		path, err = exec.LookPath(os.Args[0])
	}
	if err != nil {
		return "", err
	}
	for _, part := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if strings.HasPrefix(part, "go-build") {
			return "", errors.New("go run uses a temporary executable; build Witself to a persistent path before installing")
		}
	}
	return path, nil
}

func supportsManagedHooks(runtime string) bool {
	return runtime == transcriptcapture.RuntimeCodex || runtime == transcriptcapture.RuntimeClaudeCode
}

var semanticVersionPattern = regexp.MustCompile(`(?i)\bv?([0-9]+\.[0-9]+(?:\.[0-9]+)?(?:[-+][0-9a-z][0-9a-z.-]*)?)\b`)

func detectRuntimeVersion(runtimeCLI string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, runtimeCLI, "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	if match := semanticVersionPattern.FindSubmatch(output); len(match) == 2 {
		return string(match[1])
	}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 256 {
			line = line[:256]
		}
		return line
	}
	return ""
}

func findRuntimeCLI(runtime string) (string, error) {
	candidates := []string{}
	var probeArgs []string
	switch runtime {
	case transcriptcapture.RuntimeCodex:
		candidates = append(candidates, strings.TrimSpace(os.Getenv("CODEX_CLI_PATH")))
		if path, err := exec.LookPath("codex"); err == nil {
			candidates = append(candidates, path)
		}
		candidates = append(candidates, "/Applications/ChatGPT.app/Contents/Resources/codex")
		probeArgs = []string{"mcp", "add", "--help"}
	case transcriptcapture.RuntimeClaudeCode:
		candidates = append(candidates, strings.TrimSpace(os.Getenv("CLAUDE_CLI_PATH")))
		if path, err := exec.LookPath("claude"); err == nil {
			candidates = append(candidates, path)
		}
		probeArgs = []string{"mcp", "add", "--help"}
	case transcriptcapture.RuntimeGrokBuild:
		candidates = append(candidates, strings.TrimSpace(os.Getenv("GROK_CLI_PATH")))
		if path, err := exec.LookPath("grok"); err == nil {
			candidates = append(candidates, path)
		}
		probeArgs = []string{"mcp", "add", "--help"}
	case transcriptcapture.RuntimeCursor:
		candidates = append(candidates, strings.TrimSpace(os.Getenv("CURSOR_CLI_PATH")))
		if path, err := exec.LookPath("cursor"); err == nil {
			candidates = append(candidates, path)
		}
		probeArgs = []string{"agent", "mcp", "list", "--help"}
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := exec.CommandContext(ctx, candidate, probeArgs...).Run()
		cancel()
		if err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no %s executable with MCP support was found", runtime)
}

func registerMCP(runtime, runtimeCLI, witselfExecutable, account, realm, agent, location string) error {
	serveArgs := []string{
		witselfExecutable, "mcp", "serve", "--runtime", runtime,
		"--account", account, "--realm", realm, "--agent", agent,
	}
	if location != "" {
		serveArgs = append(serveArgs, "--location", location)
	}
	if runtime == transcriptcapture.RuntimeCursor {
		if err := registerCursorMCP(serveArgs); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		output, err := exec.CommandContext(ctx, runtimeCLI, "agent", "mcp", "enable", "witself").CombinedOutput()
		if err != nil {
			_ = unregisterCursorMCP()
			return fmt.Errorf("approve Cursor MCP: %w: %s", err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var removeArgs, addArgs []string
	switch runtime {
	case transcriptcapture.RuntimeCodex:
		removeArgs = []string{"mcp", "remove", "witself"}
		addArgs = append([]string{"mcp", "add", "witself", "--"}, serveArgs...)
	case transcriptcapture.RuntimeClaudeCode:
		removeArgs = []string{"mcp", "remove", "--scope", "user", "witself"}
		addArgs = append([]string{"mcp", "add", "--scope", "user", "--transport", "stdio", "witself", "--"}, serveArgs...)
	case transcriptcapture.RuntimeGrokBuild:
		removeArgs = []string{"mcp", "remove", "--scope", "user", "witself"}
		addArgs = append([]string{"mcp", "add", "--scope", "user", "witself", "--"}, serveArgs...)
	default:
		return fmt.Errorf("unsupported runtime %q", runtime)
	}
	_ = exec.CommandContext(ctx, runtimeCLI, removeArgs...).Run()
	output, err := exec.CommandContext(ctx, runtimeCLI, addArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func unregisterMCP(runtime, runtimeCLI string) error {
	if runtime == transcriptcapture.RuntimeCursor {
		if runtimeCLI != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			_, _ = exec.CommandContext(ctx, runtimeCLI, "agent", "mcp", "disable", "witself").CombinedOutput()
			cancel()
		}
		removeErr := unregisterCursorMCP()
		if removeErr != nil {
			return removeErr
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	args := []string{"mcp", "remove", "witself"}
	if runtime == transcriptcapture.RuntimeClaudeCode || runtime == transcriptcapture.RuntimeGrokBuild {
		args = []string{"mcp", "remove", "--scope", "user", "witself"}
	}
	output, err := exec.CommandContext(ctx, runtimeCLI, args...).CombinedOutput()
	if err != nil {
		if mcpRegistrationAlreadyMissing(output) {
			return nil
		}
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
