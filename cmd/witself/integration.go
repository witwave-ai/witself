package main

import (
	"context"
	"encoding/json"
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
	"github.com/witwave-ai/witself/internal/memorycurator"
	"github.com/witwave-ai/witself/internal/memoryhydration"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const (
	maxHookInputBytes             = 16 * 1024 * 1024
	captureDetachedFlushEnv       = "WITSELF_CAPTURE_DETACHED_FLUSH"
	detachedFlushMaxDuration      = 2 * time.Minute
	foregroundFlushLockMaxWait    = detachedFlushMaxDuration + 30*time.Second
	foregroundFlushLockPollPeriod = 50 * time.Millisecond
	maxCaptureAppendRequestBytes  = 7 * 1024 * 1024
	maxCaptureAppendBatchEntries  = 100
)

// GUI-backed runtime CLIs can take several seconds to cold-start before their
// MCP subcommand is available. Cursor has exceeded ten seconds on a healthy
// workstation, so keep this capability probe distinct from the fast version
// probe.
const runtimeCLICapabilityProbeTimeout = 30 * time.Second

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
	routingOnly := fs.Bool("routing-only", false, "refresh only managed static routing instructions; leave hooks, MCP, and the integration binding unchanged")
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing an agent token")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	setFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
	if !supportsTranscriptHooks(runtime) {
		for _, name := range []string{"capture", "managed-hooks", "user-hooks"} {
			if setFlags[name] {
				fmt.Fprintf(os.Stderr, "witself: --%s is not supported for %s because this integration has no transcript hooks\n", name, runtime)
				return 2
			}
		}
	}
	if *userHooks && setFlags["managed-hooks"] && *managedHooks {
		fmt.Fprintln(os.Stderr, "witself: --user-hooks conflicts with --managed-hooks")
		return 2
	}
	if *managedHooks && !supportsManagedHooks(runtime) {
		fmt.Fprintf(os.Stderr, "witself: %s uses approval-free global user hooks; --managed-hooks is not supported\n", runtime)
		return 2
	}
	if *routingOnly {
		for name := range setFlags {
			if name != "routing-only" {
				fmt.Fprintf(os.Stderr, "witself: --routing-only conflicts with --%s\n", name)
				return 2
			}
		}
		runtimeWorkspace := ""
		if runtime == transcriptcapture.RuntimeOpenClaw {
			if installed, loadErr := transcriptcapture.LoadConfig(runtime); loadErr == nil {
				runtimeWorkspace = installed.RuntimeWorkspace
			} else if !errors.Is(loadErr, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "witself: read existing integration: %v\n", loadErr)
				return 1
			} else {
				environment, environmentErr := captureOpenClawMCPEnvironment()
				if environmentErr != nil {
					fmt.Fprintf(os.Stderr, "witself: %v\n", environmentErr)
					return 1
				}
				runtimeCLI, cliErr := findRuntimeCLIWithEnvironment(runtime, environment)
				if cliErr != nil {
					fmt.Fprintf(os.Stderr, "witself: %v\n", cliErr)
					return 1
				}
				openClawAgent, inspectErr := inspectOpenClawDefaultAgentWithEnvironment(runtimeCLI, environment)
				cliErr = inspectErr
				if cliErr != nil {
					fmt.Fprintf(os.Stderr, "witself: %v\n", cliErr)
					return 1
				}
				runtimeWorkspace = openClawAgent.Workspace
			}
		}
		routing, err := installRuntimeMemoryRoutingInstructionsAt(runtime, runtimeWorkspace)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
		if !routing.managed {
			fmt.Fprintf(os.Stderr, "witself: %s has no managed static routing instructions\n", runtime)
			return 1
		}
		fmt.Printf("refreshed %s static routing instructions\n", routing.displayName)
		fmt.Printf("memory routing: %s (managed)\n", routing.path)
		fmt.Printf("next: restart %s or start a new task to load the refreshed instructions\n", routing.displayName)
		return 0
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
	hookMode := transcriptcapture.HookModeNone
	if supportsTranscriptHooks(runtime) {
		hookMode = transcriptcapture.HookModeUser
	}
	if useManagedHooks {
		hookMode = transcriptcapture.HookModeManaged
	}
	var openClawEnvironment map[string]string
	if runtime == transcriptcapture.RuntimeOpenClaw {
		openClawEnvironment, err = captureOpenClawMCPEnvironment()
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
		if previousConfigErr == nil && !equalOpenClawMCPEnvironment(previousConfig.MCPEnvironment, openClawEnvironment) {
			fmt.Fprintln(os.Stderr, "witself: OpenClaw selector environment changed since installation; restore the installed OPENCLAW_CONFIG_PATH, OPENCLAW_STATE_DIR, and OPENCLAW_PROFILE values before reinstalling or uninstall the existing integration first")
			return 1
		}
	}
	runtimeCLI, err := findRuntimeCLIWithEnvironment(runtime, openClawEnvironment)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	openClawWorkspace := ""
	openClawAgentID := ""
	if runtime == transcriptcapture.RuntimeOpenClaw {
		var openClawAgent openClawAgent
		openClawAgent, err = inspectOpenClawDefaultAgentWithEnvironment(runtimeCLI, openClawEnvironment)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
		openClawWorkspace = openClawAgent.Workspace
		openClawAgentID = openClawAgent.ID
		if errors.Is(previousConfigErr, os.ErrNotExist) {
			_, exists, inspectErr := inspectOpenClawMCPWithEnvironment(runtimeCLI, openClawEnvironment)
			if inspectErr != nil {
				fmt.Fprintf(os.Stderr, "witself: inspect OpenClaw-managed mcp.servers before install: %v\n", inspectErr)
				return 1
			}
			if exists {
				fmt.Fprintln(os.Stderr, "witself: OpenClaw-managed mcp.servers.witself exists without a Witself integration record; refusing to claim or replace it")
				return 1
			}
		}
	}
	runtimeVersion := detectRuntimeVersionWithEnvironment(runtime, runtimeCLI, openClawEnvironment)
	if runtimeVersion == "" && previousConfigErr == nil {
		runtimeVersion = previousConfig.RuntimeVersion
	}
	witselfExecutable, err := currentExecutablePath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: locate current executable: %v\n", err)
		return 1
	}
	if runtime == transcriptcapture.RuntimeGrokBuild {
		report, inspectErr := inspectGrokCompatibility(runtimeCLI)
		if inspectErr != nil {
			fmt.Fprintf(os.Stderr, "witself: Grok compatibility preflight failed: %v\n", inspectErr)
			return 1
		}
		if err := validateGrokCompatibilityReport(report, witselfExecutable); err != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
		writeGrokCompatibilityWarnings(os.Stderr, report)
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
	if runtime == transcriptcapture.RuntimeOpenClaw {
		cfg.RuntimeCLICommand = runtimeCLI
		cfg.MCPCommand = witselfExecutable
		cfg.MCPEnvironment = cloneOpenClawEnvironment(openClawEnvironment)
		cfg.MCPConnectTimeoutSeconds = openClawMCPConnectTimeoutSeconds
		cfg.RuntimeWorkspace = openClawWorkspace
		cfg.RuntimeAgentID = openClawAgentID
		if previousConfigErr == nil {
			switch {
			case previousConfig.RuntimeCLICommand != cfg.RuntimeCLICommand:
				fmt.Fprintf(os.Stderr, "witself: OpenClaw CLI changed from %s to %s; uninstall the existing integration before reinstalling it\n", previousConfig.RuntimeCLICommand, cfg.RuntimeCLICommand)
				return 1
			case previousConfig.RuntimeAgentID != cfg.RuntimeAgentID:
				fmt.Fprintf(os.Stderr, "witself: OpenClaw default agent changed from %s to %s; uninstall the existing integration before reinstalling it\n", previousConfig.RuntimeAgentID, cfg.RuntimeAgentID)
				return 1
			case previousConfig.RuntimeWorkspace != cfg.RuntimeWorkspace:
				fmt.Fprintf(os.Stderr, "witself: OpenClaw default workspace changed from %s to %s; uninstall the existing integration before reinstalling it\n", previousConfig.RuntimeWorkspace, cfg.RuntimeWorkspace)
				return 1
			}
		}
	}
	var cursorPermissionSnapshot cursorCLIConfigSnapshot
	if runtime == transcriptcapture.RuntimeCursor {
		cursorPermissionSnapshot, err = snapshotCursorCLIConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: inspect Cursor CLI permissions: %v\n", err)
			return 1
		}
		if previousConfigErr == nil {
			cfg.ManagedPermissions = append([]string(nil), previousConfig.ManagedPermissions...)
		}
	}
	previousHooks, err := snapshotRuntimeHooks(runtime)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: inspect existing runtime hooks: %v\n", err)
		return 1
	}
	stagedConfig := cfg
	if previousConfigErr == nil && supportsTranscriptHooks(runtime) {
		stagedConfig.HookMode = previousConfig.HookMode
	}
	if runtime != transcriptcapture.RuntimeOpenClaw || errors.Is(previousConfigErr, os.ErrNotExist) {
		if err := transcriptcapture.SaveConfig(stagedConfig); err != nil {
			fmt.Fprintf(os.Stderr, "witself: save integration: %v\n", err)
			return 1
		}
	}
	restoreConfig := func() error {
		if previousConfigErr == nil {
			return transcriptcapture.SaveConfig(previousConfig)
		}
		return transcriptcapture.RemoveConfig(runtime)
	}
	memoryRouting, err := installRuntimeMemoryRoutingInstructionsAt(runtime, cfg.RuntimeWorkspace)
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
	cursorPermissionTouched := false
	rollbackInstall := func(mcpTouched, hooksTouched bool) {
		if runtime == transcriptcapture.RuntimeOpenClaw {
			if previousBinding != nil {
				// Keep the newly installed policy in place until the previous exact
				// MCP binding is restored. An older policy may not cover tools added
				// by the attempted binding if MCP rollback itself fails.
				if mcpTouched {
					if rollbackErr := restoreRuntimeMCPBinding(runtime, runtimeCLI, witselfExecutable, previousBinding, &cfg); rollbackErr != nil {
						fmt.Fprintf(os.Stderr, "witself: warning: restore MCP registration: %v; preserving current OpenClaw routing and integration recovery state\n", rollbackErr)
						return
					}
				}
				if memoryRouting.managed {
					if rollbackErr := memoryRouting.restore(); rollbackErr != nil {
						fmt.Fprintf(os.Stderr, "witself: warning: restore %s memory routing instructions: %v\n", memoryRouting.displayName, rollbackErr)
						return
					}
				}
				if rollbackErr := restoreConfig(); rollbackErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: restore integration config: %v\n", rollbackErr)
				}
				return
			}

			// On a first install, the newly installed policy and staged config are
			// the only durable recovery fence for a credential-bound MCP that may
			// still be live. Remove neither until exact MCP absence is proven.
			if mcpTouched {
				if rollbackErr := restoreRuntimeMCPBinding(runtime, runtimeCLI, witselfExecutable, nil, &cfg); rollbackErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: remove attempted MCP registration: %v; preserving OpenClaw routing and integration recovery state\n", rollbackErr)
					return
				}
			}
			if memoryRouting.managed {
				if rollbackErr := memoryRouting.restore(); rollbackErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: restore %s memory routing instructions: %v\n", memoryRouting.displayName, rollbackErr)
					return
				}
			}
			if rollbackErr := restoreConfig(); rollbackErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: restore integration config: %v\n", rollbackErr)
			}
			return
		}
		if rollbackErr := restoreConfig(); rollbackErr != nil {
			fmt.Fprintf(os.Stderr, "witself: warning: restore integration config: %v\n", rollbackErr)
		}
		if cursorPermissionTouched {
			if rollbackErr := cursorPermissionSnapshot.restore(); rollbackErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: restore Cursor CLI permissions: %v\n", rollbackErr)
			}
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
			if rollbackErr := restoreRuntimeMCPBinding(runtime, runtimeCLI, witselfExecutable, previousBinding, &cfg); rollbackErr != nil {
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
	if runtime == transcriptcapture.RuntimeCursor {
		cursorPermissionTouched, err = cursorPermissionSnapshot.ensureWitselfMCPPermission()
		if err != nil {
			rollbackInstall(false, false)
			fmt.Fprintf(os.Stderr, "witself: configure Cursor CLI permissions: %v\n", err)
			return 1
		}
		if cursorPermissionTouched {
			cfg.ManagedPermissions = addManagedCursorMCPPermission(cfg.ManagedPermissions)
		}
	}
	openClawMCPPreTouched := false
	if runtime == transcriptcapture.RuntimeOpenClaw {
		openClawMCPPreTouched, err = prepareOpenClawMCPInstall(runtimeCLI, witselfExecutable, cfg, previousBinding)
		if err != nil {
			rollbackInstall(openClawMCPPreTouched, false)
			fmt.Fprintf(os.Stderr, "witself: register MCP: %v\n", err)
			return 1
		}
	}
	var registerErr error
	if runtime == transcriptcapture.RuntimeOpenClaw {
		desiredBinding, bindingErr := openClawMCPBindingFromConfig(witselfExecutable, cfg)
		if bindingErr != nil {
			registerErr = bindingErr
		} else {
			registerErr = registerOpenClawMCPBinding(runtimeCLI, desiredBinding)
		}
	} else {
		registerErr = registerMCP(runtime, runtimeCLI, witselfExecutable, accountName, conn.RealmName, self.Identity.AgentName, loc.Name)
	}
	if registerErr != nil {
		rollbackInstall(true, false)
		fmt.Fprintf(os.Stderr, "witself: register MCP: %v\n", registerErr)
		return 1
	}
	var hookPath string
	if !supportsTranscriptHooks(runtime) {
		// OpenClaw's first integration phase is MCP plus static routing only.
		// Keep the explicit none mode durable so later code never infers hooks.
	} else if useManagedHooks {
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
	if supportsTranscriptHooks(runtime) {
		fmt.Printf("hooks: %s (%s)\n", hookPath, hookMode)
	} else {
		fmt.Println("transcript capture: unavailable (no supported OpenClaw hooks)")
	}
	fmt.Println("mcp: witself")
	hydrationCapability := memoryhydration.CapabilityFor(runtime)
	fmt.Printf("memory hydration: session=%s automatic=%t task=%s automatic=%t\n",
		hydrationCapability.SessionHydration.Delivery, hydrationCapability.SessionHydration.Automatic,
		hydrationCapability.TaskRecall.Delivery, hydrationCapability.TaskRecall.Automatic)
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
	} else if runtime == transcriptcapture.RuntimeOpenClaw {
		fmt.Println("next: start a new OpenClaw task to load the managed memory-routing instructions and guided MCP fallback")
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
	var cursorPermissionSnapshot cursorCLIConfigSnapshot
	cursorPermissionManaged := runtime == transcriptcapture.RuntimeCursor &&
		cursorConfigManagesWitselfMCPPermission(cfg.ManagedPermissions)
	if cursorPermissionManaged {
		cursorPermissionSnapshot, err = snapshotCursorCLIConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: inspect Cursor CLI permissions: %v\n", err)
			return 1
		}
	}
	// Reject malformed managed routing before invoking any runtime CLI. This is
	// intentionally read-only: CLI availability is the second preflight, and no
	// local integration state is changed unless both checks pass.
	if err := preflightRuntimeMemoryRoutingRemovalAt(runtime, cfg.RuntimeWorkspace); err != nil {
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
	runtimeCLI := ""
	var runtimeCLIErr error
	if runtime == transcriptcapture.RuntimeOpenClaw {
		// The OpenClaw namespace is selected by the installed CLI binding.
		// Uninstall through that exact persisted command instead of whichever
		// `openclaw` binary now happens to win PATH lookup.
		runtimeCLI = cfg.RuntimeCLICommand
		_, _, runtimeCLIErr = inspectOpenClawMCPWithEnvironment(runtimeCLI, cfg.MCPEnvironment)
	} else {
		runtimeCLI, runtimeCLIErr = findRuntimeCLI(runtime)
	}
	if runtime != transcriptcapture.RuntimeCursor && runtimeCLIErr != nil {
		// Non-Cursor MCP registrations are owned by the runtime CLI. Preserve all
		// local state so a later retry can remove the complete integration.
		fmt.Fprintf(os.Stderr, "witself: cannot remove MCP registration: %v\n", runtimeCLIErr)
		return 1
	}
	// OpenClaw ignores MCP initialization instructions, so its AGENTS.md block
	// is the only model-visible policy surface. Keep that block in place until
	// after its credential-bound MCP registration is gone. Other runtimes retain
	// the existing routing-first teardown, whose snapshot supports rollback.
	var memoryRouting runtimeMemoryRoutingSnapshot
	if runtime != transcriptcapture.RuntimeOpenClaw {
		memoryRouting, err = removeRuntimeMemoryRoutingInstructionsAt(runtime, cfg.RuntimeWorkspace)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
	}
	cursorPermissionTouched := false
	rollbackUninstall := func(hooksTouched, mcpTouched bool) {
		routingHandled := false
		mcpRestoreAllowed := true
		if runtime == transcriptcapture.RuntimeOpenClaw && memoryRouting.managed {
			routingHandled = true
			if rollbackErr := memoryRouting.restore(); rollbackErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: restore %s memory routing instructions: %v\n", memoryRouting.displayName, rollbackErr)
				mcpRestoreAllowed = false
			}
		}
		if cursorPermissionTouched {
			if rollbackErr := cursorPermissionSnapshot.restore(); rollbackErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: restore Cursor CLI permissions: %v\n", rollbackErr)
			}
		}
		if mcpTouched && mcpRestoreAllowed && previousBinding != nil && (runtimeCLIErr == nil || runtime == transcriptcapture.RuntimeCursor) {
			if rollbackErr := restoreRuntimeMCPBinding(runtime, runtimeCLI, witselfExecutable, previousBinding, previousBinding); rollbackErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: restore MCP registration: %v\n", rollbackErr)
			}
		}
		if hooksTouched && previousBinding != nil {
			if rollbackErr := restoreRuntimeHooksSnapshot(runtime, witselfExecutable, previousBinding, previousHooks); rollbackErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: restore runtime hooks: %v\n", rollbackErr)
			}
		}
		if memoryRouting.managed && !routingHandled {
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
	if supportsTranscriptHooks(runtime) {
		if _, err := transcriptcapture.RemoveHooks(runtime); err != nil {
			rollbackUninstall(true, false)
			fmt.Fprintf(os.Stderr, "witself: remove user hooks: %v\n", err)
			return 1
		}
	}
	if runtime == transcriptcapture.RuntimeOpenClaw {
		expected, expectedErr := openClawMCPBindingFromConfig(witselfExecutable, cfg)
		if expectedErr != nil {
			rollbackUninstall(true, false)
			fmt.Fprintf(os.Stderr, "witself: resolve installed MCP binding: %v\n", expectedErr)
			return 1
		}
		if err := unregisterOpenClawMCP(runtimeCLI, &expected); err != nil {
			rollbackUninstall(true, true)
			fmt.Fprintf(os.Stderr, "witself: unregister MCP: %v\n", err)
			return 1
		}
		memoryRouting, err = removeRuntimeMemoryRoutingInstructionsAt(runtime, cfg.RuntimeWorkspace)
		if err != nil {
			// A failed removal returns no trustworthy snapshot. Leave MCP absent
			// rather than risk restoring credential-bound tools without policy.
			rollbackUninstall(false, false)
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
	} else if runtime == transcriptcapture.RuntimeCursor {
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
	if cursorPermissionManaged {
		cursorPermissionTouched, err = cursorPermissionSnapshot.removeWitselfMCPPermission()
		if err != nil {
			rollbackUninstall(true, runtime == transcriptcapture.RuntimeCursor || runtimeCLIErr == nil)
			fmt.Fprintf(os.Stderr, "witself: remove Cursor CLI permissions: %v\n", err)
			return 1
		}
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
	// Curator processes intentionally inherit provider/runtime credentials so
	// they can run local inference, but their own hooks must not feed the
	// resulting synthesis conversation back into the transcript ledger. That
	// would create a self-sustaining curation loop.
	if strings.TrimSpace(os.Getenv("WITSELF_CURATOR_SESSION")) != "" {
		return 0
	}
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
	if foreignGrokCompatibilityHook(*runtime, os.Getenv(grokHookEventEnv)) {
		return 0
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, maxHookInputBytes+1))
	if err != nil || len(raw) > maxHookInputBytes {
		fmt.Fprintln(os.Stderr, "witself capture: hook input could not be queued")
		return 0
	}
	event, err := transcriptcapture.EnqueueHookForBinding(*runtime, *account, *realm, *agent, *location, raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself capture: %v\n", err)
		return 0
	}
	if os.Getenv("WITSELF_CAPTURE_NO_FLUSH") == "" {
		if err := startBackgroundFlush(*runtime); err != nil {
			fmt.Fprintf(os.Stderr, "witself capture: queued locally; background flush did not start: %v\n", err)
		}
	}
	output, err := automaticHydrationHook(context.Background(), event)
	if err != nil {
		// Hydration is optional context, never an availability dependency for a
		// user prompt. The transcript event remains queued and the runtime must
		// continue without model-visible Witself context.
		fmt.Fprintf(os.Stderr, "witself hydration: unavailable; continuing without automatic context: %v\n", err)
		return 0
	}
	if len(output) > 0 {
		_, _ = fmt.Fprintln(os.Stdout, string(output))
	}
	return 0
}

type installedHydrationSource struct {
	cfg  transcriptcapture.Config
	conn *agentConnection
}

func (s *installedHydrationSource) connect(ctx context.Context) (agentConnection, error) {
	if s.conn != nil {
		return *s.conn, nil
	}
	conn, err := connectAgent(ctx, s.cfg.Account, s.cfg.Realm, s.cfg.Agent, s.cfg.Endpoint, s.cfg.TokenFile)
	if err != nil {
		return agentConnection{}, err
	}
	s.conn = &conn
	return conn, nil
}

func (s *installedHydrationSource) Self(ctx context.Context, opts client.SelfOptions) (client.SelfDigest, error) {
	conn, err := s.connect(ctx)
	if err != nil {
		return client.SelfDigest{}, err
	}
	return client.GetSelf(ctx, conn.Endpoint, conn.Token, opts)
}

func (s *installedHydrationSource) Recall(ctx context.Context, in client.MemoryRecallInput) (client.MemoryRecallPage, error) {
	conn, err := s.connect(ctx)
	if err != nil {
		return client.MemoryRecallPage{}, err
	}
	page, err := client.RecallMemories(ctx, conn.Endpoint, conn.Token, in)
	if err != nil {
		return client.MemoryRecallPage{}, err
	}
	return *page, nil
}

// automaticHydrationHook uses the exact installed identity and emits context
// only for runtime/event pairs with a documented model-visible hook channel.
// Execute owns the short deadline and the renderer's byte/sensitivity bounds.
func automaticHydrationHook(ctx context.Context, event transcriptcapture.Event) ([]byte, error) {
	cfg, err := transcriptcapture.LoadConfig(event.Runtime)
	if err != nil {
		return nil, err
	}
	result, err := memoryhydration.Execute(ctx, memoryhydration.Config{}, memoryhydration.Binding{
		AccountID: cfg.AccountID, RealmID: cfg.RealmID, RealmName: cfg.Realm,
		AgentID: cfg.AgentID, AgentName: cfg.AgentName,
	}, memoryhydration.Request{
		Runtime: event.Runtime, Event: event.HookEvent, Prompt: event.Body,
	}, &installedHydrationSource{cfg: cfg})
	if err != nil || !result.Injected {
		return nil, err
	}
	return memoryhydration.HookOutput(event.Runtime, event.HookEvent, result.Context)
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
	detached := strings.TrimSpace(os.Getenv(captureDetachedFlushEnv)) != ""
	lockDeadline := time.Now().Add(foregroundFlushLockMaxWait)
	var release func()
	for {
		var acquired bool
		release, acquired, err = transcriptcapture.AcquireFlushLock(runtimeName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: acquire capture lock: %v\n", err)
			return 1
		}
		if acquired {
			break
		}
		if detached {
			return 0
		}
		if !time.Now().Before(lockDeadline) {
			fmt.Fprintln(os.Stderr, "witself: timed out waiting for the active transcript flush")
			return 1
		}
		time.Sleep(foregroundFlushLockPollPeriod)
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
	blockedTranscripts := map[string]error{}
	seenPaths := map[string]struct{}{}
	rememberCapturePaths(pending, seenPaths)
	ready, prepareErr := prepareTranscriptFlushEvents(pending, cfg, blockedTranscripts)
	var deferredErr error
	if prepareErr != nil {
		deferredErr = prepareErr
	}
	ctx, cancel := transcriptFlushContext(detached)
	defer cancel()
	var conn agentConnection
	connected := false
	flushed := 0
	for len(ready) > 0 {
		if !connected {
			conn, err = connectAgent(ctx, cfg.Account, cfg.Realm, cfg.Agent, cfg.Endpoint, cfg.TokenFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "witself: connect capture agent: %v\n", err)
				return 1
			}
			connected = true
		}
		activityRetryPending := false
		for _, pendingEvent := range ready {
			event := pendingEvent.Event
			observation := event.ActivityObservation()
			_, activityErr := client.TouchAgentActivity(ctx, conn.Endpoint, conn.Token, client.AgentActivityInput{
				Runtime: observation.Runtime, LocationID: observation.LocationID, Location: observation.Location,
				Event: observation.Event, EventID: observation.EventID, EventOccurredAt: observation.EventOccurredAt,
			})
			if activityErr != nil {
				// A new CLI may briefly talk to an older server during a rolling
				// upgrade. Only the bare route-missing 404 is a compatibility
				// fallback. Every other touch failure keeps the event pending, but
				// it must not prevent the established transcript ledger from making
				// progress; transcript creation and appends are idempotent on retry.
				if errors.Is(activityErr, client.ErrNotFound) && activityErr.Error() == "not found" {
					activityErr = nil
				}
			}
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
			inputBatches, err := captureAppendBatches(captureEntries)
			if err != nil {
				fmt.Fprintf(os.Stderr, "witself: prepare capture event: %v\n", err)
				return 1
			}
			for _, inputs := range inputBatches {
				if _, err := client.AppendTranscriptEntries(ctx, conn.Endpoint, conn.Token, tr.ID, inputs); err != nil {
					fmt.Fprintf(os.Stderr, "witself: append capture event: %v\n", err)
					return 1
				}
			}
			if activityErr != nil {
				fmt.Fprintf(os.Stderr, "witself: record agent activity: %v\n", activityErr)
				activityRetryPending = true
				continue
			}
			if err := transcriptcapture.RemovePending(pendingEvent.Path); err != nil {
				fmt.Fprintf(os.Stderr, "witself: acknowledge capture event: %v\n", err)
				return 1
			}
			flushed++
		}
		if activityRetryPending {
			return 1
		}
		pending, err = transcriptcapture.Pending(runtimeName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: read capture outbox: %v\n", err)
			return 1
		}
		reopenRetryableBlockedTranscripts(pending, blockedTranscripts, seenPaths)
		ready, prepareErr = prepareTranscriptFlushEvents(pending, cfg, blockedTranscripts)
		if deferredErr == nil && prepareErr != nil {
			deferredErr = prepareErr
		}
	}

	// Close the enqueue-vs-unlock race: an event whose competing flusher saw
	// this lock is visible after release and gets a fresh flusher. Deliberately
	// deferred Grok turns do not respawn themselves in a tight process loop.
	release()
	lockHeld = false
	remaining, err := transcriptcapture.Pending(runtimeName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: recheck capture outbox: %v\n", err)
		return 1
	}
	reopenRetryableBlockedTranscripts(remaining, blockedTranscripts, seenPaths)
	if hasUnblockedCaptureEvent(remaining, blockedTranscripts) {
		if err := startBackgroundFlush(runtimeName); err != nil {
			fmt.Fprintf(os.Stderr, "witself: restart capture flush: %v\n", err)
			return 1
		}
	}
	deferred := countBlockedCaptureEvents(remaining, blockedTranscripts)
	if deferred > 0 {
		if deferredErr != nil {
			fmt.Fprintf(os.Stderr, "witself: finalize capture event: %v\n", deferredErr)
		}
		fmt.Fprintf(os.Stderr, "flushed %d %s transcript event(s); deferred %d incomplete or mismatched event(s)\n", flushed, runtimeName, deferred)
		return 1
	}
	fmt.Fprintf(os.Stderr, "flushed %d %s transcript event(s)\n", flushed, runtimeName)
	return 0
}

func transcriptFlushContext(detached bool) (context.Context, context.CancelFunc) {
	if detached {
		return context.WithTimeout(context.Background(), detachedFlushMaxDuration)
	}
	// An explicit foreground flush is the deterministic delivery fence. Each
	// HTTP request remains bounded by the client timeout, but a valid large
	// backlog must not fail merely because the full drain takes two minutes.
	return context.WithCancel(context.Background())
}

func prepareTranscriptFlushEvents(
	pending []transcriptcapture.PendingEvent,
	cfg transcriptcapture.Config,
	blocked map[string]error,
) ([]transcriptcapture.PendingEvent, error) {
	ready := make([]transcriptcapture.PendingEvent, 0, len(pending))
	var firstErr error
	for _, pendingEvent := range pending {
		transcriptID := pendingEvent.Event.TranscriptExternalID()
		if _, exists := blocked[transcriptID]; exists {
			continue
		}
		if !transcriptcapture.PendingEventUploadReady(pendingEvent, pending) {
			// A prompt and its turn stay local until a terminal fence reveals
			// whether a later sealed tool requires synchronous suppression.
			// A later hook creates a new outbox path and reopens this retryable
			// transcript through the normal background-flush logic.
			blocked[transcriptID] = nil
			continue
		}
		if err := captureEventBindingError(pendingEvent.Event, cfg); err != nil {
			blocked[transcriptID] = err
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		finalized, uploadReady, err := transcriptcapture.FinalizePending(pendingEvent)
		if err != nil {
			blocked[transcriptID] = err
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !uploadReady {
			blocked[transcriptID] = nil
			continue
		}
		ready = append(ready, finalized)
	}
	return ready, firstErr
}

func captureEventBindingError(event transcriptcapture.Event, cfg transcriptcapture.Config) error {
	legacyEventContext := event.AccountID == "" && event.RealmID == "" &&
		event.AgentID != "" && event.AgentID == cfg.AgentID &&
		event.Location.ID != "" && event.Location.ID == cfg.Location.ID
	accountMatches := stableCaptureIdentityMatches(event.AccountID, cfg.AccountID, event.Account, cfg.Account, legacyEventContext)
	realmMatches := stableCaptureIdentityMatches(event.RealmID, cfg.RealmID, event.Realm, cfg.Realm, legacyEventContext)
	agentMatches := stableCaptureIdentityMatches(event.AgentID, cfg.AgentID,
		event.Agent+"\x00"+event.AgentName, cfg.Agent+"\x00"+cfg.AgentName, false)
	if event.Runtime != cfg.Runtime || !accountMatches || !realmMatches || !agentMatches ||
		event.Location.ID != cfg.Location.ID {
		return errors.New("queued transcript identity does not match the installed runtime binding")
	}
	return nil
}

func stableCaptureIdentityMatches(eventID, configID, eventName, configName string, allowLegacyEvent bool) bool {
	if eventID != "" && configID != "" {
		return eventID == configID
	}
	if eventID == "" && configID == "" {
		return eventName == configName
	}
	if eventID == "" && configID != "" && allowLegacyEvent {
		return eventName == configName
	}
	return false
}

func hasUnblockedCaptureEvent(pending []transcriptcapture.PendingEvent, blocked map[string]error) bool {
	for _, pendingEvent := range pending {
		if _, exists := blocked[pendingEvent.Event.TranscriptExternalID()]; !exists {
			return true
		}
	}
	return false
}

func countBlockedCaptureEvents(pending []transcriptcapture.PendingEvent, blocked map[string]error) int {
	count := 0
	for _, pendingEvent := range pending {
		if _, exists := blocked[pendingEvent.Event.TranscriptExternalID()]; exists {
			count++
		}
	}
	return count
}

func captureAppendBatches(entries []transcriptcapture.Entry) ([][]client.AppendTranscriptEntryInput, error) {
	const envelopeBytes = len(`{"entries":[]}`)
	batches := make([][]client.AppendTranscriptEntryInput, 0, len(entries)/maxCaptureAppendBatchEntries+1)
	current := make([]client.AppendTranscriptEntryInput, 0, min(len(entries), maxCaptureAppendBatchEntries))
	currentBytes := envelopeBytes
	flushCurrent := func() {
		if len(current) == 0 {
			return
		}
		batches = append(batches, current)
		current = make([]client.AppendTranscriptEntryInput, 0, min(len(entries), maxCaptureAppendBatchEntries))
		currentBytes = envelopeBytes
	}
	for _, entry := range entries {
		input := client.AppendTranscriptEntryInput{
			ExternalID: entry.ExternalID, Role: entry.Role, Body: entry.Body,
			Payload: entry.Payload, Model: entry.Model,
			ReplyToExternalID: entry.ReplyToExternalID,
		}
		raw, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		separatorBytes := 0
		if len(current) > 0 {
			separatorBytes = 1
		}
		if len(current) == maxCaptureAppendBatchEntries ||
			currentBytes+separatorBytes+len(raw) > maxCaptureAppendRequestBytes {
			flushCurrent()
			separatorBytes = 0
		}
		if currentBytes+separatorBytes+len(raw) > maxCaptureAppendRequestBytes {
			return nil, errors.New("one capture entry exceeds the safe append request size")
		}
		current = append(current, input)
		currentBytes += separatorBytes + len(raw)
	}
	flushCurrent()
	return batches, nil
}

func rememberCapturePaths(pending []transcriptcapture.PendingEvent, seen map[string]struct{}) {
	for _, pendingEvent := range pending {
		seen[pendingEvent.Path] = struct{}{}
	}
}

func reopenRetryableBlockedTranscripts(
	pending []transcriptcapture.PendingEvent,
	blocked map[string]error,
	seen map[string]struct{},
) {
	for _, pendingEvent := range pending {
		if _, alreadySeen := seen[pendingEvent.Path]; alreadySeen {
			continue
		}
		transcriptID := pendingEvent.Event.TranscriptExternalID()
		if reason, exists := blocked[transcriptID]; exists && reason == nil {
			delete(blocked, transcriptID)
		}
	}
	rememberCapturePaths(pending, seen)
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
	cmd.Env = append(os.Environ(), captureDetachedFlushEnv+"=1")
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// startBackgroundAutomaticCuratorIfPending remains only for the explicitly
// invoked legacy `memory curate auto` command. Runtime transcript hooks never
// call it; normal curation is performed by an active foreground agent.
func startBackgroundAutomaticCuratorIfPending(runtimeName string, cfg transcriptcapture.Config) error {
	if strings.TrimSpace(cfg.AgentID) == "" {
		return nil
	}
	store, err := memorycurator.DefaultAutoStore(cfg.AgentID)
	if err != nil {
		return err
	}
	inspection, err := store.Inspect()
	if err != nil {
		return err
	}
	if !inspection.Configured || !inspection.Config.Enabled || inspection.PendingWakeCount == 0 {
		return nil
	}
	executable, err := currentExecutablePath()
	if err != nil {
		return err
	}
	cmd := exec.Command(executable, "memory", "curate", "auto", "run", "--runtime", runtimeName, "--supervise")
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

func supportsTranscriptHooks(runtime string) bool {
	switch runtime {
	case transcriptcapture.RuntimeCodex,
		transcriptcapture.RuntimeClaudeCode,
		transcriptcapture.RuntimeGrokBuild,
		transcriptcapture.RuntimeCursor:
		return true
	default:
		return false
	}
}

var semanticVersionPattern = regexp.MustCompile(`(?i)\bv?([0-9]+\.[0-9]+(?:\.[0-9]+)?(?:[-+][0-9a-z][0-9a-z.-]*)?)\b`)

func detectRuntimeVersion(runtimeName, runtimeCLI string) string {
	return detectRuntimeVersionWithEnvironment(runtimeName, runtimeCLI, nil)
}

func detectRuntimeVersionWithEnvironment(runtimeName, runtimeCLI string, environment map[string]string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Version output is part of the executable's stdout contract. Keep stderr
	// separate: GUI-backed CLIs such as Cursor can emit unrelated diagnostics
	// there before printing their real semantic version on stdout.
	args := []string{"--version"}
	if runtimeName == transcriptcapture.RuntimeCursor {
		// Cursor's MCP-capable Agent is shipped and versioned separately from
		// the desktop launcher. Native hook payloads identify this Agent build,
		// so bind and verify that same executable surface.
		args = []string{"agent", "--version"}
	}
	cmd := exec.CommandContext(ctx, runtimeCLI, args...)
	if runtimeName == transcriptcapture.RuntimeOpenClaw && environment != nil {
		cmd = openClawCommandContext(ctx, runtimeCLI, environment, args...)
	}
	output, err := cmd.Output()
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
	return findRuntimeCLIWithEnvironment(runtime, nil)
}

func findRuntimeCLIWithEnvironment(runtime string, environment map[string]string) (string, error) {
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
	case transcriptcapture.RuntimeOpenClaw:
		candidates = append(candidates, strings.TrimSpace(os.Getenv("OPENCLAW_CLI_PATH")))
		if path, err := exec.LookPath("openclaw"); err == nil {
			candidates = append(candidates, path)
		}
		probeArgs = []string{"mcp", "add", "--help"}
	}
	seen := map[string]bool{}
	probeTimeout := runtimeCLICapabilityProbeTimeout
	if runtime == transcriptcapture.RuntimeOpenClaw {
		probeTimeout = openClawCLIReadTimeout
	}
	for _, candidate := range candidates {
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		cmd := exec.CommandContext(ctx, candidate, probeArgs...)
		if runtime == transcriptcapture.RuntimeOpenClaw && environment != nil {
			cmd = openClawCommandContext(ctx, candidate, environment, probeArgs...)
		}
		err := cmd.Run()
		cancel()
		if err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no %s executable with MCP support was found", runtime)
}

func registerMCP(runtime, runtimeCLI, witselfExecutable, account, realm, agent, location string) error {
	serveArgs := runtimeMCPServeArgs(runtime, witselfExecutable, account, realm, agent, location)
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
	case transcriptcapture.RuntimeOpenClaw:
		return registerOpenClawMCP(runtimeCLI, serveArgs)
	default:
		return fmt.Errorf("unsupported runtime %q", runtime)
	}
	_ = exec.CommandContext(ctx, runtimeCLI, removeArgs...).Run()
	output, err := exec.CommandContext(ctx, runtimeCLI, addArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	if runtime == transcriptcapture.RuntimeGrokBuild {
		_, err := verifyGrokNativeMCPBinding(runtimeCLI, serveArgs)
		if err != nil {
			return fmt.Errorf("verify native Grok MCP registration: %w", err)
		}
	}
	return nil
}

func runtimeMCPServeArgs(runtime, witselfExecutable, account, realm, agent, location string) []string {
	serveArgs := []string{
		witselfExecutable, "mcp", "serve", "--runtime", runtime,
		"--account", account, "--realm", realm, "--agent", agent,
	}
	if location != "" {
		serveArgs = append(serveArgs, "--location", location)
	}
	return serveArgs
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
	if runtime == transcriptcapture.RuntimeOpenClaw {
		return unregisterOpenClawMCP(runtimeCLI, nil)
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
