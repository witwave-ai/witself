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
	runtimepkg "runtime"
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

// Kept as a narrow test seam for the selector-drift race immediately after
// transaction recovery. Ordinary provider-root selection and lock acquisition
// always call genericProviderOperationLockRoot directly.
var genericProviderOperationLockRootAfterRecovery = genericProviderOperationLockRoot

var (
	saveRuntimeIntegrationConfig     = transcriptcapture.SaveConfig
	finalizeRuntimeIntegrationConfig = transcriptcapture.SaveConfig
	removeRuntimeIntegrationConfig   = transcriptcapture.RemoveConfig
)

func installCmd(args []string) int {
	const groupUsage = "usage: witself install RUNTIME[,RUNTIME...]|all [flags]"
	if commandHelpRequested(args) {
		printCommandGroupHelp(os.Stdout, groupUsage,
			"witself install RUNTIME[,RUNTIME...] [flags]",
			"witself install all [flags]",
			"witself integrations [--verify] [--json]  # show available AI runtimes",
		)
		return 0
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, groupUsage)
		return 2
	}
	if strings.EqualFold(strings.TrimSpace(args[0]), "all") {
		return installAllCmd(args[1:])
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
	configureCommandUsage(fs, "usage: witself install RUNTIME[,RUNTIME...] [flags]")
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
	flagArgs := args[1:]
	if commandHelpRequested(flagArgs) {
		flagArgs = []string{"--help"}
	}
	if parsed, code := parseCommandFlags(fs, flagArgs); !parsed {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself install RUNTIME[,RUNTIME...] [flags]")
		return 2
	}
	if err := validateIntegrationInstallPlatform(runtime, runtimepkg.GOOS); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "witself: %s does not support administrator-managed hooks on %s; use user-scoped hooks\n", runtime, runtimepkg.GOOS)
		return 2
	}
	releaseOperationLock, lockedProviderRoot, lockErr := acquireRuntimeIntegrationOperationLockWithProviderRoot(runtime)
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "witself: acquire %s integration lock: %v\n", integrationDisplayName(runtime), lockErr)
		return 1
	}
	defer releaseOperationLock()
	if isGenericProviderRuntime(runtime) {
		configRoot := lockedProviderRoot
		pendingOperation := ""
		if pending, pendingErr := loadGenericProviderTransactionJournal(runtime); pendingErr == nil {
			pendingOperation = pending.Operation
		} else if !errors.Is(pendingErr, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "witself: inspect interrupted %s transaction: %v\n", integrationDisplayName(runtime), pendingErr)
			return 1
		}
		if recoveryErr := recoverGenericProviderTransaction(runtime); recoveryErr != nil {
			fmt.Fprintf(os.Stderr, "witself: recover interrupted %s transaction: %v\n", integrationDisplayName(runtime), recoveryErr)
			return 1
		}
		recoveredRoot, recoveredRootErr := genericProviderOperationLockRootAfterRecovery(runtime)
		if recoveredRootErr != nil {
			fmt.Fprintf(os.Stderr, "witself: resolve %s provider lock root after recovery: %v\n", integrationDisplayName(runtime), recoveredRootErr)
			return 1
		}
		if recoveredRoot != configRoot {
			fmt.Fprintf(os.Stderr, "witself: %s recovery changed the provider lock root; rerun install so the current provider selector is locked before any new mutation\n", integrationDisplayName(runtime))
			return 1
		}
		if pendingOperation == genericProviderTransactionUninstall {
			if _, loadErr := transcriptcapture.LoadConfig(runtime); errors.Is(loadErr, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "witself: recovered an interrupted %s uninstall; rerun install so the current provider selector is locked before any new mutation\n", integrationDisplayName(runtime))
				return 1
			}
		}
	}
	if runtime == transcriptcapture.RuntimeOpenClaw {
		configRoot, rootErr := openClawOperationLockRoot()
		if rootErr != nil {
			fmt.Fprintf(os.Stderr, "witself: resolve OpenClaw transaction root: %v\n", rootErr)
			return 1
		}
		pendingOperation := ""
		if pending, pendingErr := loadOpenClawTransactionJournal(configRoot); pendingErr == nil {
			pendingOperation = pending.Operation
		} else if !errors.Is(pendingErr, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "witself: inspect interrupted OpenClaw transaction: %v\n", pendingErr)
			return 1
		}
		if recoveryErr := recoverOpenClawTransaction(configRoot); recoveryErr != nil {
			fmt.Fprintf(os.Stderr, "witself: recover interrupted OpenClaw transaction: %v\n", recoveryErr)
			return 1
		}
		recoveredRoot, recoveredRootErr := openClawOperationLockRoot()
		if recoveredRootErr != nil {
			fmt.Fprintf(os.Stderr, "witself: resolve OpenClaw transaction root after recovery: %v\n", recoveredRootErr)
			return 1
		}
		if recoveredRoot != configRoot {
			fmt.Fprintln(os.Stderr, "witself: OpenClaw recovery changed the provider lock root; rerun install so the current provider selector is locked before any new mutation")
			return 1
		}
		if pendingOperation == openClawTransactionUninstall {
			if _, loadErr := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw); errors.Is(loadErr, os.ErrNotExist) {
				fmt.Fprintln(os.Stderr, "witself: recovered an interrupted OpenClaw uninstall; rerun install so the current provider selector is locked before any new mutation")
				return 1
			}
		}
	}
	if runtime == transcriptcapture.RuntimeCopilot {
		configRoot, rootErr := copilotOperationLockRoot()
		if rootErr != nil {
			fmt.Fprintf(os.Stderr, "witself: resolve GitHub Copilot config root: %v\n", rootErr)
			return 1
		}
		pendingOperation := ""
		if pending, pendingErr := loadCopilotTransactionJournal(configRoot); pendingErr == nil {
			pendingOperation = pending.Operation
		} else if !errors.Is(pendingErr, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "witself: inspect interrupted GitHub Copilot transaction: %v\n", pendingErr)
			return 1
		}
		if recoveryErr := recoverCopilotTransaction(configRoot); recoveryErr != nil {
			fmt.Fprintf(os.Stderr, "witself: recover interrupted GitHub Copilot transaction: %v\n", recoveryErr)
			return 1
		}
		recoveredRoot, recoveredRootErr := copilotOperationLockRoot()
		if recoveredRootErr != nil {
			fmt.Fprintf(os.Stderr, "witself: resolve GitHub Copilot config root after recovery: %v\n", recoveredRootErr)
			return 1
		}
		if recoveredRoot != configRoot {
			fmt.Fprintln(os.Stderr, "witself: GitHub Copilot recovery changed the provider lock root; rerun install so the current provider selector is locked before any new mutation")
			return 1
		}
		if pendingOperation == copilotTransactionUninstall {
			if _, loadErr := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCopilot); errors.Is(loadErr, os.ErrNotExist) {
				fmt.Fprintln(os.Stderr, "witself: recovered an interrupted GitHub Copilot uninstall; rerun install so the current provider selector is locked before any new mutation")
				return 1
			}
		}
	}
	if *routingOnly {
		if runtime == transcriptcapture.RuntimeAntigravity {
			fmt.Fprintln(os.Stderr, "witself: --routing-only is not supported for antigravity because its MCP binding and always-on routing policy are one transactionally managed integration unit")
			return 2
		}
		for name := range setFlags {
			if name != "routing-only" {
				fmt.Fprintf(os.Stderr, "witself: --routing-only conflicts with --%s\n", name)
				return 2
			}
		}
		runtimeWorkspace := ""
		if isGenericProviderRuntime(runtime) {
			if installed, loadErr := transcriptcapture.LoadConfig(runtime); loadErr == nil {
				if err := validateGenericProviderCurrentRoots(installed); err != nil {
					fmt.Fprintf(os.Stderr, "witself: %v\n", err)
					return 1
				}
			} else if !errors.Is(loadErr, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "witself: read existing integration: %v\n", loadErr)
				return 1
			} else if _, _, err := genericProviderConfigPaths(runtime); err != nil {
				fmt.Fprintf(os.Stderr, "witself: resolve %s provider config: %v\n", runtime, err)
				return 1
			}
		}
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
		if runtime == transcriptcapture.RuntimeCopilot {
			if installed, loadErr := transcriptcapture.LoadConfig(runtime); loadErr == nil {
				currentRoot, rootErr := currentCopilotConfigRoot()
				if rootErr != nil {
					fmt.Fprintf(os.Stderr, "witself: resolve GitHub Copilot config root: %v\n", rootErr)
					return 1
				}
				if currentRoot != installed.RuntimeConfigRoot {
					fmt.Fprintf(os.Stderr, "witself: COPILOT_HOME changed from installed %s to %s; restore the installed value before refreshing routing\n", installed.RuntimeConfigRoot, currentRoot)
					return 1
				}
			} else if !errors.Is(loadErr, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "witself: read existing integration: %v\n", loadErr)
				return 1
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
	if runtime == transcriptcapture.RuntimeAntigravity {
		configRoot, rootErr := antigravityOperationLockRoot()
		if rootErr != nil {
			fmt.Fprintf(os.Stderr, "witself: resolve Antigravity config root: %v\n", rootErr)
			return 1
		}
		pendingOperation := ""
		if pending, pendingErr := loadAntigravityTransactionJournal(configRoot); pendingErr == nil {
			pendingOperation = pending.Operation
		} else if !errors.Is(pendingErr, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "witself: inspect interrupted Antigravity transaction: %v\n", pendingErr)
			return 1
		}
		if recoveryErr := recoverAntigravityTransaction(configRoot); recoveryErr != nil {
			fmt.Fprintf(os.Stderr, "witself: recover interrupted Antigravity transaction: %v\n", recoveryErr)
			return 1
		}
		recoveredRoot, recoveredRootErr := antigravityOperationLockRoot()
		if recoveredRootErr != nil {
			fmt.Fprintf(os.Stderr, "witself: resolve Antigravity config root after recovery: %v\n", recoveredRootErr)
			return 1
		}
		if recoveredRoot != configRoot {
			fmt.Fprintln(os.Stderr, "witself: Antigravity recovery changed the provider lock root; rerun install so the current provider selector is locked before any new mutation")
			return 1
		}
		if pendingOperation == antigravityTransactionUninstall {
			if _, loadErr := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity); errors.Is(loadErr, os.ErrNotExist) {
				fmt.Fprintln(os.Stderr, "witself: recovered an interrupted Antigravity uninstall; rerun install so the current provider selector is locked before any new mutation")
				return 1
			}
		}
	}
	previousConfig, previousConfigErr := transcriptcapture.LoadConfig(runtime)
	previousConfigOriginal := previousConfig
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
		if previousConfigErr == nil &&
			!equalOpenClawMCPEnvironment(previousConfig.MCPEnvironment, openClawEnvironment) &&
			!legacyOpenClawDefaultEnvironmentCanMigrate(previousConfig.MCPEnvironment, openClawEnvironment) {
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
	var genericPreviousBinding *transcriptcapture.Config
	var genericProviderBefore genericMCPConfigSnapshot
	if isGenericProviderRuntime(runtime) {
		if err := configureGenericProviderBinding(&cfg, runtimeCLI, witselfExecutable); err != nil {
			fmt.Fprintf(os.Stderr, "witself: configure %s provider binding: %v\n", runtime, err)
			return 1
		}
		runtimeCLI = cfg.RuntimeCLICommand
		witselfExecutable = cfg.MCPCommand
		if previousConfigErr == nil {
			if err := validateGenericProviderPreviousSelection(cfg, previousConfig); err != nil {
				fmt.Fprintf(os.Stderr, "witself: %v\n", err)
				return 1
			}
			previous, err := hydrateLegacyGenericProviderConfig(previousConfig, runtimeCLI, witselfExecutable)
			if err != nil {
				fmt.Fprintf(os.Stderr, "witself: reconstruct existing %s binding: %v\n", runtime, err)
				return 1
			}
			genericPreviousBinding = &previous
		}
		genericProviderBefore, err = prepareGenericMCPInstallSnapshot(runtimeCLI, cfg, genericPreviousBinding)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: preflight %s MCP ownership: %v\n", runtime, err)
			return 1
		}
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
	if runtime == transcriptcapture.RuntimeAntigravity {
		if err := configureAntigravityBinding(&cfg, runtimeCLI, witselfExecutable); err != nil {
			fmt.Fprintf(os.Stderr, "witself: configure Antigravity plugin: %v\n", err)
			return 1
		}
		if previousConfigErr == nil {
			switch {
			case previousConfig.RuntimeCLICommand != cfg.RuntimeCLICommand:
				fmt.Fprintf(os.Stderr, "witself: Antigravity CLI changed from %s to %s; uninstall the existing integration before reinstalling it\n", previousConfig.RuntimeCLICommand, cfg.RuntimeCLICommand)
				return 1
			case previousConfig.RuntimeConfigRoot != cfg.RuntimeConfigRoot:
				fmt.Fprintf(os.Stderr, "witself: Antigravity config root changed from %s to %s; restore the installed HOME before reinstalling or uninstall the existing integration first\n", previousConfig.RuntimeConfigRoot, cfg.RuntimeConfigRoot)
				return 1
			case previousConfig.RuntimePluginPath != cfg.RuntimePluginPath:
				fmt.Fprintln(os.Stderr, "witself: Antigravity plugin path changed; uninstall the existing integration before reinstalling it")
				return 1
			case previousConfig.MCPEnvironment["WITSELF_HOME"] != cfg.MCPEnvironment["WITSELF_HOME"]:
				fmt.Fprintln(os.Stderr, "witself: WITSELF_HOME changed since Antigravity installation; restore it before reinstalling or uninstall the existing integration first")
				return 1
			}
		}
		var previous *transcriptcapture.Config
		if previousConfigErr == nil {
			previousConfigCopy := previousConfig
			previous = &previousConfigCopy
		}
		if err := preflightAntigravityInstall(cfg, previous); err != nil {
			fmt.Fprintf(os.Stderr, "witself: preflight Antigravity integration: %v\n", err)
			return 1
		}
		if err := stageAntigravitySourceBundle(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
		cfg.SchemaVersion = transcriptcapture.SchemaVersion
	}
	if runtime == transcriptcapture.RuntimeCopilot {
		if err := configureCopilotBinding(&cfg, runtimeCLI, witselfExecutable); err != nil {
			fmt.Fprintf(os.Stderr, "witself: configure GitHub Copilot integration: %v\n", err)
			return 1
		}
		if previousConfigErr == nil {
			switch {
			case previousConfig.RuntimeCLICommand != cfg.RuntimeCLICommand:
				fmt.Fprintf(os.Stderr, "witself: GitHub Copilot CLI changed from %s to %s; uninstall the existing integration before reinstalling it\n", previousConfig.RuntimeCLICommand, cfg.RuntimeCLICommand)
				return 1
			case previousConfig.RuntimeConfigRoot != cfg.RuntimeConfigRoot:
				fmt.Fprintf(os.Stderr, "witself: COPILOT_HOME changed from %s to %s; restore the installed value before reinstalling or uninstall the existing integration first\n", previousConfig.RuntimeConfigRoot, cfg.RuntimeConfigRoot)
				return 1
			case previousConfig.RuntimeMCPConfigPath != cfg.RuntimeMCPConfigPath:
				fmt.Fprintln(os.Stderr, "witself: GitHub Copilot MCP config path changed; uninstall the existing integration before reinstalling it")
				return 1
			case !equalCopilotEnvironment(previousConfig.MCPEnvironment, cfg.MCPEnvironment):
				fmt.Fprintln(os.Stderr, "witself: WITSELF_HOME changed since GitHub Copilot installation; restore it before reinstalling or uninstall the existing integration first")
				return 1
			}
		}
	}
	if previousConfigErr == nil && previousConfig.HookMode != transcriptcapture.HookModeNone {
		previousForHooks := &previousConfig
		if genericPreviousBinding != nil {
			previousForHooks = genericPreviousBinding
		}
		if err := hydrateLegacyRuntimeHookOwnership(previousForHooks, previousForHooks.MCPCommand); err != nil {
			fmt.Fprintf(os.Stderr, "witself: inspect prior runtime hook ownership: %v\n", err)
			return 1
		}
		copyHookOwnership(&previousConfig, *previousForHooks)
		if genericPreviousBinding != nil {
			previousHooksCopy := *previousForHooks
			genericPreviousBinding = &previousHooksCopy
		}
	}
	if supportsTranscriptHooks(runtime) {
		var previousForHooks *transcriptcapture.Config
		if previousConfigErr == nil {
			previousForHooks = &previousConfig
			if genericPreviousBinding != nil {
				previousForHooks = genericPreviousBinding
			}
		}
		if err := planRuntimeHooksOwned(&cfg, previousForHooks); err != nil {
			fmt.Fprintf(os.Stderr, "witself: plan runtime hook ownership: %v\n", err)
			return 1
		}
	}
	var antigravityJournal *antigravityTransactionJournal
	var antigravityPreviousBinding *transcriptcapture.Config
	if runtime == transcriptcapture.RuntimeAntigravity {
		if previousConfigErr == nil {
			previous := previousConfig
			antigravityPreviousBinding = &previous
		}
		journal, journalErr := beginAntigravityTransaction(
			antigravityTransactionInstall,
			antigravityPreviousBinding,
			&cfg,
		)
		if journalErr != nil {
			_, journalStatErr := os.Lstat(antigravityTransactionPath(cfg.RuntimeConfigRoot))
			if errors.Is(journalStatErr, os.ErrNotExist) &&
				(antigravityPreviousBinding == nil || antigravityPreviousBinding.RuntimePluginSource != cfg.RuntimePluginSource) {
				_ = removeAntigravitySourceBundle(cfg)
			}
			fmt.Fprintf(os.Stderr, "witself: begin Antigravity transaction: %v\n", journalErr)
			return 1
		}
		antigravityJournal = &journal
	}
	var openClawJournal *openClawTransactionJournal
	if runtime == transcriptcapture.RuntimeOpenClaw {
		var previous *transcriptcapture.Config
		if previousConfigErr == nil {
			previousCopy := previousConfigOriginal
			previous = &previousCopy
		}
		desired := cfg
		journal, journalErr := beginOpenClawTransaction(openClawTransactionInstall, previous, &desired)
		if journalErr != nil {
			fmt.Fprintf(os.Stderr, "witself: begin OpenClaw transaction: %v\n", journalErr)
			return 1
		}
		openClawJournal = &journal
	}
	var copilotJournal *copilotTransactionJournal
	if runtime == transcriptcapture.RuntimeCopilot {
		var previous *transcriptcapture.Config
		if previousConfigErr == nil {
			previousCopy := previousConfigOriginal
			previous = &previousCopy
		}
		desired := cfg
		journal, journalErr := beginCopilotTransaction(copilotTransactionInstall, previous, &desired)
		if journalErr != nil {
			fmt.Fprintf(os.Stderr, "witself: begin GitHub Copilot transaction: %v\n", journalErr)
			return 1
		}
		copilotJournal = &journal
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
	stagedConfig := cfg
	if previousConfigErr == nil && supportsTranscriptHooks(runtime) {
		stagedConfig.HookMode = previousConfig.HookMode
		copyHookOwnership(&stagedConfig, previousConfig)
	}
	var genericProviderJournal *genericProviderTransactionJournal
	if isGenericProviderRuntime(runtime) {
		var previous *transcriptcapture.Config
		if previousConfigErr == nil {
			previousCopy := previousConfigOriginal
			previous = &previousCopy
		}
		journal, journalErr := beginGenericProviderInstallTransaction(
			previous,
			genericPreviousBinding,
			stagedConfig,
			cfg,
			genericProviderBefore,
		)
		if journalErr != nil {
			fmt.Fprintf(os.Stderr, "witself: begin %s transaction: %v\n", integrationDisplayName(runtime), journalErr)
			return 1
		}
		genericProviderJournal = &journal
	}
	if (runtime != transcriptcapture.RuntimeOpenClaw && runtime != transcriptcapture.RuntimeCopilot) ||
		errors.Is(previousConfigErr, os.ErrNotExist) {
		if err := saveRuntimeIntegrationConfig(stagedConfig); err != nil {
			journalCleared := antigravityJournal == nil
			if antigravityJournal != nil {
				if clearErr := clearAntigravityTransaction(cfg.RuntimeConfigRoot, *antigravityJournal); clearErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: clear failed Antigravity transaction: %v\n", clearErr)
				} else {
					journalCleared = true
				}
			}
			if journalCleared && runtime == transcriptcapture.RuntimeAntigravity &&
				(previousConfigErr != nil || previousConfig.RuntimePluginSource != cfg.RuntimePluginSource) {
				if cleanupErr := removeAntigravitySourceBundle(cfg); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
					fmt.Fprintf(os.Stderr, "witself: warning: remove unrecorded Antigravity recovery bundle: %v\n", cleanupErr)
				}
			}
			if genericProviderJournal != nil {
				if recoveryErr := recoverGenericProviderTransaction(runtime); recoveryErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: recover failed %s staging transaction: %v\n", integrationDisplayName(runtime), recoveryErr)
				}
			}
			if openClawJournal != nil {
				configRoot, rootErr := openClawTransactionRootFromConfig(cfg)
				if rootErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: resolve OpenClaw transaction root: %v\n", rootErr)
				} else if clearErr := clearOpenClawTransaction(configRoot, *openClawJournal); clearErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: clear failed OpenClaw transaction: %v\n", clearErr)
				}
			}
			if copilotJournal != nil {
				if clearErr := clearCopilotTransaction(cfg.RuntimeConfigRoot, *copilotJournal); clearErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: clear failed GitHub Copilot transaction: %v\n", clearErr)
				}
			}
			fmt.Fprintf(os.Stderr, "witself: save integration: %v\n", err)
			return 1
		}
	}
	restoreConfig := func() error {
		if previousConfigErr == nil {
			return transcriptcapture.SaveConfig(previousConfigOriginal)
		}
		return transcriptcapture.RemoveConfig(runtime)
	}
	memoryRouting, err := installRuntimeMemoryRoutingInstructionsAt(runtime, cfg.RuntimeWorkspace)
	if err != nil {
		rollbackErr := restoreConfig()
		if rollbackErr != nil {
			fmt.Fprintf(os.Stderr, "witself: warning: restore integration config: %v\n", rollbackErr)
		} else if antigravityJournal != nil {
			if clearErr := clearAntigravityTransaction(cfg.RuntimeConfigRoot, *antigravityJournal); clearErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: clear failed Antigravity transaction: %v\n", clearErr)
			} else if previousConfigErr != nil || previousConfig.RuntimePluginSource != cfg.RuntimePluginSource {
				if cleanupErr := removeAntigravitySourceBundle(cfg); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
					fmt.Fprintf(os.Stderr, "witself: warning: remove unrecorded Antigravity recovery bundle: %v\n", cleanupErr)
				}
			}
		}
		if openClawJournal != nil {
			configRoot, rootErr := openClawTransactionRootFromConfig(cfg)
			if rootErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: resolve OpenClaw transaction root: %v\n", rootErr)
			} else if clearErr := clearOpenClawTransaction(configRoot, *openClawJournal); clearErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: clear failed OpenClaw transaction: %v\n", clearErr)
			}
		}
		if copilotJournal != nil {
			if clearErr := clearCopilotTransaction(cfg.RuntimeConfigRoot, *copilotJournal); clearErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: clear failed GitHub Copilot transaction: %v\n", clearErr)
			}
		}
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	var previousBinding *transcriptcapture.Config
	if previousConfigErr == nil {
		if genericPreviousBinding != nil {
			previousBinding = genericPreviousBinding
		} else if antigravityPreviousBinding != nil {
			previousBinding = antigravityPreviousBinding
		} else {
			previous := previousConfig
			previousBinding = &previous
		}
	}
	cursorPermissionTouched := false
	rollbackInstall := func(mcpTouched, hooksTouched bool) {
		if runtime == transcriptcapture.RuntimeAntigravity {
			if mcpTouched {
				if rollbackErr := restoreAntigravityPlugin(previousBinding, &cfg); rollbackErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: restore Antigravity plugin: %v; preserving the current safety policy and integration recovery state\n", rollbackErr)
					return
				}
			}
			if rollbackErr := restoreConfig(); rollbackErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: restore integration config: %v\n", rollbackErr)
				return
			}
			if antigravityJournal != nil {
				if clearErr := clearAntigravityTransaction(cfg.RuntimeConfigRoot, *antigravityJournal); clearErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: clear failed Antigravity transaction: %v\n", clearErr)
					return
				}
			}
			if previousBinding == nil || previousBinding.RuntimePluginSource != cfg.RuntimePluginSource {
				if cleanupErr := removeAntigravitySourceBundle(cfg); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
					fmt.Fprintf(os.Stderr, "witself: warning: remove attempted Antigravity recovery bundle: %v\n", cleanupErr)
				}
			}
			return
		}
		if runtime == transcriptcapture.RuntimeOpenClaw || runtime == transcriptcapture.RuntimeCopilot {
			if previousBinding != nil {
				// Keep the newly installed policy in place until the previous exact
				// MCP binding is restored. An older policy may not cover tools added
				// by the attempted binding if MCP rollback itself fails.
				if mcpTouched {
					if rollbackErr := restoreRuntimeMCPBinding(runtime, runtimeCLI, witselfExecutable, previousBinding, &cfg); rollbackErr != nil {
						fmt.Fprintf(os.Stderr, "witself: warning: restore MCP registration: %v; preserving current %s routing and integration recovery state\n", rollbackErr, integrationDisplayName(runtime))
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
					return
				}
				if openClawJournal != nil {
					configRoot, rootErr := openClawTransactionRootFromConfig(cfg)
					if rootErr != nil {
						fmt.Fprintf(os.Stderr, "witself: warning: resolve OpenClaw transaction root: %v\n", rootErr)
					} else if clearErr := clearOpenClawTransaction(configRoot, *openClawJournal); clearErr != nil {
						fmt.Fprintf(os.Stderr, "witself: warning: clear failed OpenClaw transaction: %v\n", clearErr)
					}
				}
				if copilotJournal != nil {
					if clearErr := clearCopilotTransaction(cfg.RuntimeConfigRoot, *copilotJournal); clearErr != nil {
						fmt.Fprintf(os.Stderr, "witself: warning: clear failed GitHub Copilot transaction: %v\n", clearErr)
					}
				}
				return
			}

			// On a first install, the newly installed policy and staged config are
			// the only durable recovery fence for a credential-bound MCP that may
			// still be live. Remove neither until exact MCP absence is proven.
			if mcpTouched {
				if rollbackErr := restoreRuntimeMCPBinding(runtime, runtimeCLI, witselfExecutable, nil, &cfg); rollbackErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: remove attempted MCP registration: %v; preserving %s routing and integration recovery state\n", rollbackErr, integrationDisplayName(runtime))
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
				return
			}
			if openClawJournal != nil {
				configRoot, rootErr := openClawTransactionRootFromConfig(cfg)
				if rootErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: resolve OpenClaw transaction root: %v\n", rootErr)
				} else if clearErr := clearOpenClawTransaction(configRoot, *openClawJournal); clearErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: clear failed OpenClaw transaction: %v\n", clearErr)
				}
			}
			if copilotJournal != nil {
				if clearErr := clearCopilotTransaction(cfg.RuntimeConfigRoot, *copilotJournal); clearErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: clear failed GitHub Copilot transaction: %v\n", clearErr)
				}
			}
			return
		}
		if isGenericProviderRuntime(runtime) {
			// The staged config is the recovery fence for the exact-owned provider
			// binding. Keep it until MCP, hooks, provider permissions, and routing
			// have all been restored successfully.
			if mcpTouched {
				if rollbackErr := restoreRuntimeMCPBinding(runtime, runtimeCLI, witselfExecutable, previousBinding, &cfg); rollbackErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: restore MCP registration: %v; preserving current %s routing and integration recovery state\n", rollbackErr, integrationDisplayName(runtime))
					return
				}
			}
			if cursorPermissionTouched {
				if rollbackErr := cursorPermissionSnapshot.restore(); rollbackErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: restore Cursor CLI permissions: %v; preserving integration recovery state\n", rollbackErr)
					return
				}
			}
			if hooksTouched {
				if rollbackErr := restoreRuntimeHooksOwned(&cfg, previousBinding); rollbackErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: restore runtime hooks: %v; preserving integration recovery state\n", rollbackErr)
					return
				}
			}
			if memoryRouting.managed {
				if rollbackErr := memoryRouting.restore(); rollbackErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: restore %s memory routing instructions: %v; preserving integration recovery state\n", memoryRouting.displayName, rollbackErr)
					return
				}
			}
			if rollbackErr := restoreConfig(); rollbackErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: restore integration config: %v\n", rollbackErr)
				return
			}
			if genericProviderJournal != nil {
				if clearErr := clearGenericProviderTransaction(*genericProviderJournal); clearErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: clear failed %s transaction: %v\n", integrationDisplayName(runtime), clearErr)
				}
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
			if rollbackErr := restoreRuntimeHooksOwned(&cfg, previousBinding); rollbackErr != nil {
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
	var openClawMCPPlan openClawMCPInstallPlan
	if runtime == transcriptcapture.RuntimeOpenClaw {
		if openClawJournal == nil {
			fmt.Fprintln(os.Stderr, "witself: OpenClaw transaction journal is missing before MCP mutation")
			return 1
		}
		if err = validateOpenClawTransactionProviderBefore(*openClawJournal); err == nil {
			openClawMCPPlan, openClawMCPPreTouched, err = prepareOpenClawMCPInstallPlan(runtimeCLI, witselfExecutable, cfg, previousBinding)
		}
		if err != nil {
			if providerMutationUncertain(err) ||
				(providerPreflightChanged(err) && openClawMCPPreTouched) {
				fmt.Fprintf(os.Stderr, "witself: register MCP: %v; preserving OpenClaw routing and transaction journal\n", err)
				return 1
			}
			rollbackInstall(openClawMCPPreTouched, false)
			fmt.Fprintf(os.Stderr, "witself: register MCP: %v\n", err)
			return 1
		}
	}
	copilotMCPPreTouched := false
	var copilotMCPPlan copilotMCPInstallPlan
	if runtime == transcriptcapture.RuntimeCopilot {
		if copilotJournal == nil {
			fmt.Fprintln(os.Stderr, "witself: GitHub Copilot transaction journal is missing before MCP mutation")
			return 1
		}
		if err = validateCopilotTransactionProviderBefore(*copilotJournal); err == nil {
			copilotMCPPlan, copilotMCPPreTouched, err = prepareCopilotMCPInstallPlan(runtimeCLI, witselfExecutable, cfg, previousBinding)
		}
		if err != nil {
			if providerMutationUncertain(err) ||
				(providerPreflightChanged(err) && copilotMCPPreTouched) {
				fmt.Fprintf(os.Stderr, "witself: register MCP: %v; preserving GitHub Copilot routing and transaction journal\n", err)
				return 1
			}
			rollbackInstall(copilotMCPPreTouched, false)
			fmt.Fprintf(os.Stderr, "witself: register MCP: %v\n", err)
			return 1
		}
	}
	var registerErr error
	registerTouched := false
	switch runtime {
	case transcriptcapture.RuntimeOpenClaw:
		registerTouched, registerErr = registerOpenClawMCPBindingWithPlan(runtimeCLI, openClawMCPPlan)
	case transcriptcapture.RuntimeAntigravity:
		registerTouched, registerErr = installAntigravityPlugin(cfg, previousBinding)
	case transcriptcapture.RuntimeCopilot:
		registerTouched, registerErr = registerCopilotMCPWithPlan(runtimeCLI, copilotMCPPlan)
	case transcriptcapture.RuntimeCodex,
		transcriptcapture.RuntimeClaudeCode,
		transcriptcapture.RuntimeGrokBuild,
		transcriptcapture.RuntimeCursor:
		registerTouched, registerErr = registerGenericMCPWithMutation(runtimeCLI, cfg, previousBinding)
	default:
		registerErr = fmt.Errorf("unsupported runtime %q", runtime)
	}
	if registerErr != nil {
		mcpTouched := registerTouched
		switch runtime {
		case transcriptcapture.RuntimeOpenClaw:
			mcpTouched = mcpTouched || openClawMCPPreTouched
		case transcriptcapture.RuntimeCopilot:
			mcpTouched = mcpTouched || copilotMCPPreTouched
		}
		if providerMutationUncertain(registerErr) ||
			(providerPreflightChanged(registerErr) && mcpTouched) {
			fmt.Fprintf(os.Stderr, "witself: register MCP: %v; preserving %s routing and integration recovery state\n", registerErr, integrationDisplayName(runtime))
			return 1
		}
		rollbackInstall(mcpTouched, false)
		fmt.Fprintf(os.Stderr, "witself: register MCP: %v\n", registerErr)
		return 1
	}
	registerTouched = registerTouched || openClawMCPPreTouched || copilotMCPPreTouched
	var hookPath string
	hooksTouched := false
	// Phase-one OpenClaw, Antigravity, and Copilot integrations intentionally retain
	// HookModeNone and install no transcript hooks.
	if supportsTranscriptHooks(runtime) {
		hookPath, hooksTouched, err = installRuntimeHooksOwned(&cfg, previousBinding)
	} else if previousConfigErr == nil && previousConfig.HookMode == transcriptcapture.HookModeUser {
		// A prior release could serialize an untested POSIX hook command for a
		// native Windows Claude or Grok profile. When upgrading to the MCP-only
		// Windows contract, remove that exact user hook instead of leaving a
		// stale launcher behind.
		hooksTouched, err = removeRuntimeHooksOwned(previousConfig)
	}
	if err != nil {
		rollbackInstall(registerTouched, hooksTouched)
		fmt.Fprintf(os.Stderr, "witself: install hooks: %v\n", err)
		return 1
	}
	if err := finalizeRuntimeIntegrationConfig(cfg); err != nil {
		rollbackInstall(registerTouched, hooksTouched)
		fmt.Fprintf(os.Stderr, "witself: finalize integration: %v\n", err)
		return 1
	}
	if isGenericProviderRuntime(runtime) {
		if err := validateGenericInstalledTopology(cfg); err != nil {
			rollbackInstall(registerTouched, hooksTouched)
			fmt.Fprintf(os.Stderr, "witself: finalize %s topology: %v\n", runtime, err)
			return 1
		}
		if err := verifyRuntimeHooksOwned(cfg); err != nil {
			rollbackInstall(registerTouched, hooksTouched)
			fmt.Fprintf(os.Stderr, "witself: finalize %s hooks: %v\n", runtime, err)
			return 1
		}
		if genericProviderJournal != nil {
			if err := clearGenericProviderTransaction(*genericProviderJournal); err != nil {
				fmt.Fprintf(os.Stderr, "witself: finalize %s transaction: %v\n", integrationDisplayName(runtime), err)
				return 1
			}
		}
	}
	if runtime == transcriptcapture.RuntimeAntigravity && antigravityJournal != nil {
		if err := validateAntigravityInstalledArtifacts(cfg); err != nil {
			rollbackInstall(registerTouched, true)
			fmt.Fprintf(os.Stderr, "witself: finalize Antigravity topology: %v\n", err)
			return 1
		}
		if err := clearAntigravityTransaction(cfg.RuntimeConfigRoot, *antigravityJournal); err != nil {
			fmt.Fprintf(os.Stderr, "witself: finalize Antigravity transaction: %v\n", err)
			return 1
		}
	}
	if runtime == transcriptcapture.RuntimeOpenClaw {
		if err := validateOpenClawInstalledIntegration(cfg); err != nil {
			rollbackInstall(registerTouched, true)
			fmt.Fprintf(os.Stderr, "witself: finalize OpenClaw topology: %v\n", err)
			return 1
		}
		if openClawJournal == nil {
			fmt.Fprintln(os.Stderr, "witself: finalize OpenClaw transaction: journal is missing")
			return 1
		}
		configRoot, rootErr := openClawTransactionRootFromConfig(cfg)
		if rootErr != nil {
			fmt.Fprintf(os.Stderr, "witself: finalize OpenClaw transaction: %v\n", rootErr)
			return 1
		}
		if err := clearOpenClawTransaction(configRoot, *openClawJournal); err != nil {
			fmt.Fprintf(os.Stderr, "witself: finalize OpenClaw transaction: %v\n", err)
			return 1
		}
	}
	if runtime == transcriptcapture.RuntimeCopilot {
		if err := validateCopilotInstalledTopology(cfg); err != nil {
			rollbackInstall(registerTouched, true)
			fmt.Fprintf(os.Stderr, "witself: finalize GitHub Copilot topology: %v\n", err)
			return 1
		}
		if copilotJournal == nil {
			fmt.Fprintln(os.Stderr, "witself: finalize GitHub Copilot transaction: journal is missing")
			return 1
		}
		if err := clearCopilotTransaction(cfg.RuntimeConfigRoot, *copilotJournal); err != nil {
			fmt.Fprintf(os.Stderr, "witself: finalize GitHub Copilot transaction: %v\n", err)
			return 1
		}
	}
	if runtime == transcriptcapture.RuntimeAntigravity && previousBinding != nil &&
		previousBinding.RuntimePluginSource != cfg.RuntimePluginSource {
		if cleanupErr := removeAntigravitySourceBundle(*previousBinding); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "witself: warning: remove superseded Antigravity recovery bundle: %v\n", cleanupErr)
		}
	}

	if suppressIntegrationSuccessOutput {
		return 0
	}
	if loc.Name == "" {
		fmt.Printf("installed %s for agent %s\n", runtime, self.Identity.AgentName)
	} else {
		fmt.Printf("installed %s for agent %s at %s\n", runtime, self.Identity.AgentName, loc.Name)
	}
	if supportsTranscriptHooks(runtime) {
		fmt.Printf("hooks: %s (%s)\n", hookPath, hookMode)
	} else {
		fmt.Printf("transcript capture: unavailable (no supported %s hooks)\n", runtime)
	}
	if runtime == transcriptcapture.RuntimeCopilot {
		serverName, nameErr := copilotMCPServerName(cfg)
		if nameErr != nil {
			fmt.Println("mcp: Witself-managed server")
		} else {
			fmt.Printf("mcp: %s\n", serverName)
		}
	} else {
		fmt.Println("mcp: witself")
	}
	if runtime == transcriptcapture.RuntimeAntigravity {
		fmt.Printf("plugin: %s (managed)\n", cfg.RuntimePluginPath)
	}
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
	} else if runtime == transcriptcapture.RuntimeAntigravity {
		fmt.Println("next: refresh MCP servers in Antigravity (or run /mcp in agy), then start a new task to load the managed plugin rule and guided MCP fallback")
	} else if runtime == transcriptcapture.RuntimeCopilot {
		fmt.Println("next: start a new GitHub Copilot CLI session to load the managed instructions and guided MCP fallback")
	} else if memoryRouting.managed {
		fmt.Printf("next: restart %s and start a new task to load the managed memory-routing instructions; global user hooks require no project trust\n", memoryRouting.displayName)
	} else {
		fmt.Printf("next: restart %s; global user hooks require no project trust\n", runtime)
	}
	return 0
}

func validateIntegrationInstallPlatform(runtimeName, platform string) error {
	support := integrationPlatformSupport(runtimeName, platform)
	if support.SupportedOnPlatform {
		return nil
	}
	if support.Reason != "" {
		return errors.New(support.Reason)
	}
	return fmt.Errorf("%s integration is not supported on %s", runtimeName, platform)
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
	const groupUsage = "usage: witself uninstall RUNTIME[,RUNTIME...]|all [flags]"
	if commandHelpRequested(args) {
		printCommandGroupHelp(os.Stdout, groupUsage,
			"witself uninstall RUNTIME[,RUNTIME...] [--managed-hooks]",
			"witself uninstall all [--dry-run] [--json]",
		)
		return 0
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, groupUsage)
		return 2
	}
	if strings.EqualFold(strings.TrimSpace(args[0]), "all") {
		return uninstallAllCmd(args[1:])
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
	configureCommandUsage(fs, "usage: witself uninstall RUNTIME[,RUNTIME...] [--managed-hooks]")
	managedHooks := fs.Bool("managed-hooks", false, "also remove administrator-managed hooks")
	flagArgs := args[1:]
	if commandHelpRequested(flagArgs) {
		flagArgs = []string{"--help"}
	}
	if parsed, code := parseCommandFlags(fs, flagArgs); !parsed {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself uninstall RUNTIME[,RUNTIME...] [--managed-hooks]")
		return 2
	}
	if *managedHooks && !supportsManagedHooks(runtime) {
		fmt.Fprintf(os.Stderr, "witself: %s does not support administrator-managed hooks\n", runtime)
		return 2
	}
	releaseOperationLock, lockErr := acquireRuntimeIntegrationOperationLock(runtime)
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "witself: acquire %s integration lock: %v\n", integrationDisplayName(runtime), lockErr)
		return 1
	}
	defer releaseOperationLock()
	if isGenericProviderRuntime(runtime) {
		pendingOperation := ""
		if pending, pendingErr := loadGenericProviderTransactionJournal(runtime); pendingErr == nil {
			pendingOperation = pending.Operation
		} else if !errors.Is(pendingErr, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "witself: inspect interrupted %s transaction: %v\n", integrationDisplayName(runtime), pendingErr)
			return 1
		}
		if recoveryErr := recoverGenericProviderTransaction(runtime); recoveryErr != nil {
			fmt.Fprintf(os.Stderr, "witself: recover interrupted %s transaction: %v\n", integrationDisplayName(runtime), recoveryErr)
			return 1
		}
		if pendingOperation == genericProviderTransactionUninstall {
			if _, loadErr := transcriptcapture.LoadConfig(runtime); errors.Is(loadErr, os.ErrNotExist) {
				if !suppressIntegrationSuccessOutput {
					fmt.Printf("recovered and completed interrupted %s uninstall; tokens and pending transcript events were preserved\n", runtime)
				}
				return 0
			}
		}
	}
	if runtime == transcriptcapture.RuntimeOpenClaw {
		configRoot, rootErr := openClawOperationLockRoot()
		if rootErr != nil {
			fmt.Fprintf(os.Stderr, "witself: resolve OpenClaw transaction root: %v\n", rootErr)
			return 1
		}
		pendingOperation := ""
		if pending, pendingErr := loadOpenClawTransactionJournal(configRoot); pendingErr == nil {
			pendingOperation = pending.Operation
		} else if !errors.Is(pendingErr, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "witself: inspect interrupted OpenClaw transaction: %v\n", pendingErr)
			return 1
		}
		if recoveryErr := recoverOpenClawTransaction(configRoot); recoveryErr != nil {
			fmt.Fprintf(os.Stderr, "witself: recover interrupted OpenClaw transaction: %v\n", recoveryErr)
			return 1
		}
		if pendingOperation == openClawTransactionUninstall {
			if _, loadErr := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw); errors.Is(loadErr, os.ErrNotExist) {
				if !suppressIntegrationSuccessOutput {
					fmt.Println("recovered and completed interrupted openclaw uninstall; tokens and pending transcript events were preserved")
				}
				return 0
			}
		}
	}
	if runtime == transcriptcapture.RuntimeCopilot {
		configRoot, rootErr := copilotOperationLockRoot()
		if rootErr != nil {
			fmt.Fprintf(os.Stderr, "witself: resolve GitHub Copilot config root: %v\n", rootErr)
			return 1
		}
		pendingOperation := ""
		if pending, pendingErr := loadCopilotTransactionJournal(configRoot); pendingErr == nil {
			pendingOperation = pending.Operation
		} else if !errors.Is(pendingErr, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "witself: inspect interrupted GitHub Copilot transaction: %v\n", pendingErr)
			return 1
		}
		if recoveryErr := recoverCopilotTransaction(configRoot); recoveryErr != nil {
			fmt.Fprintf(os.Stderr, "witself: recover interrupted GitHub Copilot transaction: %v\n", recoveryErr)
			return 1
		}
		if pendingOperation == copilotTransactionUninstall {
			if _, loadErr := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCopilot); errors.Is(loadErr, os.ErrNotExist) {
				if !suppressIntegrationSuccessOutput {
					fmt.Println("recovered and completed interrupted copilot uninstall; tokens and pending transcript events were preserved")
				}
				return 0
			}
		}
	}
	antigravityCurrentConfigRoot := ""
	if runtime == transcriptcapture.RuntimeAntigravity {
		configRoot, rootErr := antigravityOperationLockRoot()
		if rootErr != nil {
			fmt.Fprintf(os.Stderr, "witself: resolve Antigravity config root: %v\n", rootErr)
			return 1
		}
		pendingOperation := ""
		if pending, pendingErr := loadAntigravityTransactionJournal(configRoot); pendingErr == nil {
			pendingOperation = pending.Operation
		} else if !errors.Is(pendingErr, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "witself: inspect interrupted Antigravity transaction: %v\n", pendingErr)
			return 1
		}
		if recoveryErr := recoverAntigravityTransaction(configRoot); recoveryErr != nil {
			fmt.Fprintf(os.Stderr, "witself: recover interrupted Antigravity transaction: %v\n", recoveryErr)
			return 1
		}
		if pendingOperation == antigravityTransactionUninstall {
			if _, loadErr := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity); errors.Is(loadErr, os.ErrNotExist) {
				if !suppressIntegrationSuccessOutput {
					fmt.Println("recovered and completed interrupted antigravity uninstall; tokens and pending transcript events were preserved")
				}
				return 0
			}
		}
		currentRoot, currentRootErr := currentAntigravityConfigRoot()
		if currentRootErr != nil {
			fmt.Fprintf(os.Stderr, "witself: resolve current Antigravity config root: %v\n", currentRootErr)
			return 1
		}
		antigravityCurrentConfigRoot = currentRoot
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
	if runtime == transcriptcapture.RuntimeAntigravity {
		if cfg.RuntimeConfigRoot != antigravityCurrentConfigRoot {
			fmt.Fprintf(os.Stderr, "witself: Antigravity config root changed from %s to %s; restore the HOME used for installation before uninstalling\n", cfg.RuntimeConfigRoot, antigravityCurrentConfigRoot)
			return 1
		}
		if err := validateAntigravityInstalledTopology(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "witself: preflight Antigravity integration: %v\n", err)
			return 1
		}
	}
	if runtime == transcriptcapture.RuntimeOpenClaw {
		currentEnvironment, environmentErr := captureOpenClawMCPEnvironment()
		if environmentErr != nil {
			fmt.Fprintf(os.Stderr, "witself: resolve OpenClaw MCP environment: %v\n", environmentErr)
			return 1
		}
		if !equalOpenClawMCPEnvironment(currentEnvironment, cfg.MCPEnvironment) {
			fmt.Fprintln(os.Stderr, "witself: OpenClaw selector environment changed from the installed namespace; restore OPENCLAW_STATE_DIR, OPENCLAW_CONFIG_PATH, and WITSELF_HOME before uninstalling")
			return 1
		}
	}
	if runtime == transcriptcapture.RuntimeCopilot {
		currentRoot, rootErr := currentCopilotConfigRoot()
		if rootErr != nil {
			fmt.Fprintf(os.Stderr, "witself: resolve GitHub Copilot config root: %v\n", rootErr)
			return 1
		}
		if currentRoot != cfg.RuntimeConfigRoot {
			fmt.Fprintf(os.Stderr, "witself: COPILOT_HOME changed from installed %s to %s; restore the installed value before uninstalling\n", cfg.RuntimeConfigRoot, currentRoot)
			return 1
		}
		currentEnvironment, environmentErr := captureCopilotMCPEnvironment()
		if environmentErr != nil {
			fmt.Fprintf(os.Stderr, "witself: resolve GitHub Copilot MCP environment: %v\n", environmentErr)
			return 1
		}
		if !equalCopilotEnvironment(currentEnvironment, cfg.MCPEnvironment) {
			fmt.Fprintln(os.Stderr, "witself: WITSELF_HOME changed from the installed GitHub Copilot binding; restore it before uninstalling")
			return 1
		}
	}
	witselfExecutable := ""
	var genericPersistedConfig *transcriptcapture.Config
	if isGenericProviderRuntime(runtime) {
		persisted := cfg
		genericPersistedConfig = &persisted
		witselfExecutable, err = currentExecutablePath()
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: locate current executable for %s ownership verification: %v\n", runtime, err)
			return 1
		}
		currentCLI, selectionErr := validateGenericProviderCurrentSelection(cfg)
		if selectionErr != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", selectionErr)
			return 1
		}
		cfg, err = hydrateLegacyGenericProviderConfig(cfg, currentCLI, witselfExecutable)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: reconstruct existing %s binding: %v\n", runtime, err)
			return 1
		}
		witselfExecutable = cfg.MCPCommand
		if err := validateGenericInstalledTopology(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "witself: preflight %s MCP ownership: %v\n", runtime, err)
			return 1
		}
	}
	if cfg.HookMode != transcriptcapture.HookModeNone {
		if err := hydrateLegacyRuntimeHookOwnership(&cfg, cfg.MCPCommand); err != nil {
			fmt.Fprintf(os.Stderr, "witself: inspect installed runtime hook ownership: %v\n", err)
			return 1
		}
	}
	if *managedHooks && cfg.HookMode != transcriptcapture.HookModeManaged {
		fmt.Fprintln(os.Stderr, "witself: no administrator-managed hooks are owned by this integration; refusing an unscoped managed-hook removal")
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
	var previousBinding *transcriptcapture.Config
	var antigravityJournal *antigravityTransactionJournal
	var openClawJournal *openClawTransactionJournal
	var copilotJournal *copilotTransactionJournal
	var genericProviderJournal *genericProviderTransactionJournal
	if cfgErr == nil {
		previous := cfg
		previousBinding = &previous
		if runtime == transcriptcapture.RuntimeOpenClaw {
			witselfExecutable = cfg.MCPCommand
		}
		if witselfExecutable == "" {
			witselfExecutable, err = currentExecutablePath()
			if err != nil {
				witselfExecutable, err = os.Executable()
				if err != nil {
					fmt.Fprintf(os.Stderr, "witself: locate current executable for rollback: %v\n", err)
					return 1
				}
			}
		}
	}
	runtimeCLI := ""
	var runtimeCLIErr error
	switch runtime {
	case transcriptcapture.RuntimeOpenClaw:
		// The OpenClaw namespace is selected by the installed CLI binding.
		// Uninstall through that exact persisted command instead of whichever
		// `openclaw` binary now happens to win PATH lookup.
		runtimeCLI = cfg.RuntimeCLICommand
		_, _, runtimeCLIErr = inspectOpenClawMCPWithEnvironment(runtimeCLI, cfg.MCPEnvironment)
	case transcriptcapture.RuntimeAntigravity:
		// The plugin is an exact-owned directory and does not require a provider
		// CLI mutation to remove. Keep using the persisted CLI path only as part
		// of the durable binding; uninstall remains possible if agy was removed.
		runtimeCLI = cfg.RuntimeCLICommand
	case transcriptcapture.RuntimeCopilot:
		// Use the exact CLI and COPILOT_HOME recorded at installation instead of
		// whichever binary or profile happens to win the current shell lookup.
		runtimeCLI = cfg.RuntimeCLICommand
		runtimeCLIErr = validateCopilotCLISelection(runtimeCLI, cfg)
	case transcriptcapture.RuntimeCodex,
		transcriptcapture.RuntimeClaudeCode,
		transcriptcapture.RuntimeGrokBuild,
		transcriptcapture.RuntimeCursor:
		runtimeCLI = cfg.RuntimeCLICommand
	default:
		runtimeCLI, runtimeCLIErr = findRuntimeCLI(runtime)
	}
	if runtime != transcriptcapture.RuntimeCursor && runtime != transcriptcapture.RuntimeAntigravity && runtimeCLIErr != nil {
		// Non-Cursor MCP registrations are owned by the runtime CLI. Preserve all
		// local state so a later retry can remove the complete integration.
		fmt.Fprintf(os.Stderr, "witself: cannot remove MCP registration: %v\n", runtimeCLIErr)
		return 1
	}
	if isGenericProviderRuntime(runtime) {
		if genericPersistedConfig == nil {
			fmt.Fprintln(os.Stderr, "witself: generic provider uninstall is missing its persisted integration preimage")
			return 1
		}
		_, exists, providerBefore, inspectErr := inspectGenericMCP(cfg)
		if inspectErr != nil || !exists {
			if inspectErr == nil {
				inspectErr = errors.New("exact-owned MCP registration disappeared before uninstall transaction")
			}
			fmt.Fprintf(os.Stderr, "witself: begin %s transaction: %v\n", integrationDisplayName(runtime), inspectErr)
			return 1
		}
		journal, journalErr := beginGenericProviderUninstallTransaction(*genericPersistedConfig, cfg, providerBefore)
		if journalErr != nil {
			fmt.Fprintf(os.Stderr, "witself: begin %s transaction: %v\n", integrationDisplayName(runtime), journalErr)
			return 1
		}
		genericProviderJournal = &journal
	}
	// OpenClaw and Copilot retain their static policy until the exact
	// credential-bound MCP registration is gone. Other runtimes retain the
	// existing routing-first teardown, whose snapshot supports rollback.
	var memoryRouting runtimeMemoryRoutingSnapshot
	if runtime != transcriptcapture.RuntimeOpenClaw && runtime != transcriptcapture.RuntimeCopilot {
		memoryRouting, err = removeRuntimeMemoryRoutingInstructionsAt(runtime, cfg.RuntimeWorkspace)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
	}
	cursorPermissionTouched := false
	rollbackUninstall := func(hooksTouched, mcpTouched bool) {
		if runtime == transcriptcapture.RuntimeAntigravity {
			if mcpTouched && previousBinding != nil {
				if rollbackErr := restoreAntigravityUninstall(previousBinding); rollbackErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: restore Antigravity plugin: %v; preserving the transaction journal for recovery\n", rollbackErr)
					return
				}
			}
			if antigravityJournal != nil {
				if clearErr := clearAntigravityTransaction(cfg.RuntimeConfigRoot, *antigravityJournal); clearErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: clear failed Antigravity transaction: %v\n", clearErr)
				}
			}
			return
		}
		routingHandled := false
		mcpRestoreAllowed := true
		rollbackComplete := true
		if (runtime == transcriptcapture.RuntimeOpenClaw || runtime == transcriptcapture.RuntimeCopilot) && memoryRouting.managed {
			routingHandled = true
			if rollbackErr := memoryRouting.restore(); rollbackErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: restore %s memory routing instructions: %v\n", memoryRouting.displayName, rollbackErr)
				mcpRestoreAllowed = false
				rollbackComplete = false
			}
		}
		if cursorPermissionTouched {
			if rollbackErr := cursorPermissionSnapshot.restore(); rollbackErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: restore Cursor CLI permissions: %v\n", rollbackErr)
				rollbackComplete = false
			}
		}
		if mcpTouched && mcpRestoreAllowed && previousBinding != nil && (runtimeCLIErr == nil || runtime == transcriptcapture.RuntimeCursor) {
			if rollbackErr := restoreRuntimeMCPBinding(runtime, runtimeCLI, witselfExecutable, previousBinding, previousBinding); rollbackErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: restore MCP registration: %v\n", rollbackErr)
				rollbackComplete = false
			}
		}
		if hooksTouched && previousBinding != nil {
			if rollbackErr := restoreRuntimeHooksOwned(nil, previousBinding); rollbackErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: restore runtime hooks: %v\n", rollbackErr)
				rollbackComplete = false
			}
		}
		if memoryRouting.managed && !routingHandled {
			if rollbackErr := memoryRouting.restore(); rollbackErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: restore %s memory routing instructions: %v\n", memoryRouting.displayName, rollbackErr)
				rollbackComplete = false
			}
		}
		if copilotJournal != nil && rollbackComplete {
			if clearErr := clearCopilotTransaction(cfg.RuntimeConfigRoot, *copilotJournal); clearErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: clear failed GitHub Copilot transaction: %v\n", clearErr)
			}
		}
		if openClawJournal != nil && rollbackComplete {
			configRoot, rootErr := openClawTransactionRootFromConfig(cfg)
			if rootErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: resolve failed OpenClaw transaction root: %v\n", rootErr)
			} else if clearErr := clearOpenClawTransaction(configRoot, *openClawJournal); clearErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: clear failed OpenClaw transaction: %v\n", clearErr)
			}
		}
		if genericProviderJournal != nil && rollbackComplete {
			if clearErr := clearGenericProviderTransaction(*genericProviderJournal); clearErr != nil {
				fmt.Fprintf(os.Stderr, "witself: warning: clear failed %s transaction: %v\n", integrationDisplayName(runtime), clearErr)
			}
		}
	}
	hooksTouched, err := removeRuntimeHooksOwned(cfg)
	if err != nil {
		rollbackUninstall(hooksTouched, false)
		fmt.Fprintf(os.Stderr, "witself: remove runtime hooks: %v\n", err)
		return 1
	}
	if runtime == transcriptcapture.RuntimeAntigravity {
		previous := cfg
		journal, journalErr := beginAntigravityTransaction(antigravityTransactionUninstall, &previous, nil)
		if journalErr != nil {
			fmt.Fprintf(os.Stderr, "witself: begin Antigravity transaction: %v\n", journalErr)
			return 1
		}
		antigravityJournal = &journal
	}
	if runtime == transcriptcapture.RuntimeOpenClaw {
		previous := cfg
		journal, journalErr := beginOpenClawTransaction(openClawTransactionUninstall, &previous, nil)
		if journalErr != nil {
			rollbackUninstall(hooksTouched, false)
			fmt.Fprintf(os.Stderr, "witself: begin OpenClaw transaction: %v\n", journalErr)
			return 1
		}
		openClawJournal = &journal
	}
	if runtime == transcriptcapture.RuntimeCopilot {
		previous := cfg
		journal, journalErr := beginCopilotTransaction(copilotTransactionUninstall, &previous, nil)
		if journalErr != nil {
			rollbackUninstall(hooksTouched, false)
			fmt.Fprintf(os.Stderr, "witself: begin GitHub Copilot transaction: %v\n", journalErr)
			return 1
		}
		copilotJournal = &journal
	}
	mcpMutationTouched := false
	if runtime == transcriptcapture.RuntimeAntigravity {
		if _, removeErr := removeAntigravityPlugin(cfg); removeErr != nil {
			rollbackUninstall(hooksTouched, true)
			fmt.Fprintf(os.Stderr, "witself: unregister MCP: %v\n", removeErr)
			return 1
		}
	} else if runtime == transcriptcapture.RuntimeOpenClaw {
		if openClawJournal == nil {
			fmt.Fprintln(os.Stderr, "witself: OpenClaw transaction journal is missing before MCP removal")
			return 1
		}
		if err := validateOpenClawTransactionProviderBefore(*openClawJournal); err != nil {
			rollbackUninstall(hooksTouched, false)
			fmt.Fprintf(os.Stderr, "witself: unregister MCP: %v\n", err)
			return 1
		}
		expected, expectedErr := openClawMCPBindingFromConfig(witselfExecutable, cfg)
		if expectedErr != nil {
			rollbackUninstall(hooksTouched, false)
			fmt.Fprintf(os.Stderr, "witself: resolve installed MCP binding: %v\n", expectedErr)
			return 1
		}
		_, _, removalSnapshot, inspectErr := inspectOpenClawMCPState(runtimeCLI, cfg.MCPEnvironment)
		if inspectErr != nil {
			rollbackUninstall(hooksTouched, false)
			fmt.Fprintf(os.Stderr, "witself: unregister MCP: %v\n", inspectErr)
			return 1
		}
		mcpTouched, removeErr := unregisterOpenClawMCPWithSnapshot(runtimeCLI, &expected, &removalSnapshot)
		if removeErr != nil {
			if providerMutationUncertain(removeErr) {
				fmt.Fprintf(os.Stderr, "witself: unregister MCP: %v; preserving OpenClaw routing and transaction journal\n", removeErr)
				return 1
			}
			rollbackUninstall(hooksTouched, mcpTouched)
			fmt.Fprintf(os.Stderr, "witself: unregister MCP: %v\n", removeErr)
			return 1
		}
		mcpMutationTouched = mcpTouched
		memoryRouting, err = removeRuntimeMemoryRoutingInstructionsAt(runtime, cfg.RuntimeWorkspace)
		if err != nil {
			// Preserve the integration record and transaction journal. A retry
			// can finish the exact removal without restoring credential-bound
			// tools into a partially removed policy state.
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
	} else if runtime == transcriptcapture.RuntimeCopilot {
		if copilotJournal == nil {
			fmt.Fprintln(os.Stderr, "witself: GitHub Copilot transaction journal is missing before MCP removal")
			return 1
		}
		if err := validateCopilotTransactionProviderBefore(*copilotJournal); err != nil {
			rollbackUninstall(hooksTouched, false)
			fmt.Fprintf(os.Stderr, "witself: unregister MCP: %v\n", err)
			return 1
		}
		_, _, removalSnapshot, inspectErr := inspectCopilotMCPState(runtimeCLI, cfg)
		if inspectErr != nil {
			rollbackUninstall(hooksTouched, false)
			fmt.Fprintf(os.Stderr, "witself: unregister MCP: %v\n", inspectErr)
			return 1
		}
		mcpTouched, removeErr := unregisterCopilotMCPWithSnapshot(runtimeCLI, &cfg, &removalSnapshot)
		if removeErr != nil {
			if providerMutationUncertain(removeErr) {
				fmt.Fprintf(os.Stderr, "witself: unregister MCP: %v; preserving GitHub Copilot routing and transaction journal\n", removeErr)
				return 1
			}
			rollbackUninstall(hooksTouched, mcpTouched)
			fmt.Fprintf(os.Stderr, "witself: unregister MCP: %v\n", removeErr)
			return 1
		}
		mcpMutationTouched = mcpTouched
		memoryRouting, err = removeRuntimeMemoryRoutingInstructionsAt(runtime, cfg.RuntimeWorkspace)
		if err != nil {
			// Preserve the integration record when exact policy removal cannot be
			// proven. MCP stays absent so no credential-bound tools lack policy.
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
	} else if isGenericProviderRuntime(runtime) {
		if err := unregisterGenericMCP(runtimeCLI, cfg); err != nil {
			rollbackUninstall(true, true)
			fmt.Fprintf(os.Stderr, "witself: unregister MCP: %v\n", err)
			return 1
		}
	} else {
		fmt.Fprintf(os.Stderr, "witself: warning: MCP registration was not removed: %v\n", runtimeCLIErr)
	}
	if runtime == transcriptcapture.RuntimeAntigravity {
		absent, stateErr := antigravitySharedMCPMatches(nil, &cfg)
		if stateErr != nil || !absent {
			rollbackUninstall(true, true)
			if stateErr != nil {
				fmt.Fprintf(os.Stderr, "witself: verify Antigravity shared MCP removal: %v\n", stateErr)
			} else {
				fmt.Fprintln(os.Stderr, "witself: Antigravity shared MCP entry reappeared during uninstall; restored the managed plugin and retained the integration")
			}
			return 1
		}
	}
	if cursorPermissionManaged && genericProviderJournal == nil {
		cursorPermissionTouched, err = cursorPermissionSnapshot.removeWitselfMCPPermission()
		if err != nil {
			mcpTouched := runtime == transcriptcapture.RuntimeCursor || runtimeCLIErr == nil
			if runtime == transcriptcapture.RuntimeOpenClaw || runtime == transcriptcapture.RuntimeCopilot {
				mcpTouched = mcpMutationTouched
			}
			rollbackUninstall(hooksTouched, mcpTouched)
			fmt.Fprintf(os.Stderr, "witself: remove Cursor CLI permissions: %v\n", err)
			return 1
		}
	}
	if err := removeRuntimeIntegrationConfig(runtime); err != nil {
		mcpTouched := runtime == transcriptcapture.RuntimeCursor || runtimeCLIErr == nil
		if runtime == transcriptcapture.RuntimeOpenClaw || runtime == transcriptcapture.RuntimeCopilot {
			mcpTouched = mcpMutationTouched
		}
		rollbackUninstall(hooksTouched, mcpTouched)
		fmt.Fprintf(os.Stderr, "witself: remove integration: %v\n", err)
		return 1
	}
	if runtime == transcriptcapture.RuntimeAntigravity {
		if antigravityJournal != nil {
			if recoveryErr := recoverAntigravityUninstallTransaction(cfg.RuntimeConfigRoot, *antigravityJournal); recoveryErr != nil {
				// The integration config was the uninstall commit marker. If final
				// convergence fails, make a best-effort exact restoration before
				// returning and retain the journal whenever restoration is incomplete.
				if saveErr := transcriptcapture.SaveConfig(cfg); saveErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: restore Antigravity integration config: %v\n", saveErr)
				} else if restoreErr := restoreAntigravityUninstall(&cfg); restoreErr != nil {
					fmt.Fprintf(os.Stderr, "witself: warning: restore Antigravity policy after finalization failure: %v\n", restoreErr)
				}
				fmt.Fprintf(os.Stderr, "witself: finalize Antigravity transaction: %v\n", recoveryErr)
				return 1
			}
		}
	}
	if runtime == transcriptcapture.RuntimeCopilot && copilotJournal != nil {
		if recoveryErr := recoverCopilotUninstallTransaction(*copilotJournal); recoveryErr != nil {
			fmt.Fprintf(os.Stderr, "witself: finalize GitHub Copilot transaction: %v\n", recoveryErr)
			return 1
		}
		if clearErr := clearCopilotTransaction(cfg.RuntimeConfigRoot, *copilotJournal); clearErr != nil {
			fmt.Fprintf(os.Stderr, "witself: finalize GitHub Copilot transaction: %v\n", clearErr)
			return 1
		}
	}
	if runtime == transcriptcapture.RuntimeOpenClaw && openClawJournal != nil {
		if recoveryErr := recoverOpenClawUninstallTransaction(*openClawJournal); recoveryErr != nil {
			fmt.Fprintf(os.Stderr, "witself: finalize OpenClaw transaction: %v\n", recoveryErr)
			return 1
		}
		configRoot, rootErr := openClawTransactionRootFromConfig(cfg)
		if rootErr != nil {
			fmt.Fprintf(os.Stderr, "witself: finalize OpenClaw transaction: %v\n", rootErr)
			return 1
		}
		if clearErr := clearOpenClawTransaction(configRoot, *openClawJournal); clearErr != nil {
			fmt.Fprintf(os.Stderr, "witself: finalize OpenClaw transaction: %v\n", clearErr)
			return 1
		}
	}
	if genericProviderJournal != nil {
		if recoveryErr := recoverGenericProviderTransaction(runtime); recoveryErr != nil {
			fmt.Fprintf(os.Stderr, "witself: finalize %s transaction: %v\n", integrationDisplayName(runtime), recoveryErr)
			return 1
		}
	}
	if !suppressIntegrationSuccessOutput {
		fmt.Printf("uninstalled %s integration; tokens and pending transcript events were preserved\n", runtime)
	}
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
	witselfHome := fs.String("witself-home", "", "installed Witself state root")
	if err := fs.Parse(args); err != nil {
		return 0
	}
	if foreignGrokCompatibilityHook(*runtime, os.Getenv(grokHookEventEnv)) {
		return 0
	}
	if strings.TrimSpace(*witselfHome) != "" {
		canonicalHome, homeErr := cleanCopilotAbsolutePath("hook WITSELF_HOME", *witselfHome)
		if homeErr != nil || canonicalHome != *witselfHome {
			if homeErr == nil {
				homeErr = errors.New("hook WITSELF_HOME must be canonical")
			}
			fmt.Fprintf(os.Stderr, "witself capture: %v\n", homeErr)
			return 0
		}
		priorHome, hadPriorHome := os.LookupEnv("WITSELF_HOME")
		if homeErr = os.Setenv("WITSELF_HOME", canonicalHome); homeErr != nil {
			fmt.Fprintf(os.Stderr, "witself capture: set WITSELF_HOME: %v\n", homeErr)
			return 0
		}
		defer func() {
			if hadPriorHome {
				_ = os.Setenv("WITSELF_HOME", priorHome)
			} else {
				_ = os.Unsetenv("WITSELF_HOME")
			}
		}()
		installed, loadErr := transcriptcapture.LoadConfig(*runtime)
		if loadErr != nil {
			fmt.Fprintf(os.Stderr, "witself capture: verify WITSELF_HOME binding: %v\n", loadErr)
			return 0
		}
		if isGenericProviderRuntime(installed.Runtime) && installed.MCPEnvironment["WITSELF_HOME"] != canonicalHome {
			fmt.Fprintln(os.Stderr, "witself capture: WITSELF_HOME does not match the installed provider binding")
			return 0
		}
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
	return supportsManagedHooksForPlatform(runtime, runtimepkg.GOOS)
}

func supportsManagedHooksForPlatform(runtime, platform string) bool {
	if platform != "darwin" && platform != "linux" {
		return false
	}
	return runtime == transcriptcapture.RuntimeCodex || runtime == transcriptcapture.RuntimeClaudeCode
}

func supportsTranscriptHooks(runtime string) bool {
	return supportsTranscriptHooksForPlatform(runtime, runtimepkg.GOOS)
}

func supportsTranscriptHooksForPlatform(runtime, platform string) bool {
	if platform != "darwin" && platform != "linux" && platform != "windows" {
		return false
	}
	switch runtime {
	case transcriptcapture.RuntimeCodex:
		return true
	case transcriptcapture.RuntimeClaudeCode,
		transcriptcapture.RuntimeGrokBuild,
		transcriptcapture.RuntimeCursor:
		// Codex has a dedicated commandWindows contract. The other providers'
		// hook command fields currently use POSIX shell quoting, so advertise
		// their hook surface only where that execution contract is tested.
		return platform == "darwin" || platform == "linux"
	default:
		return false
	}
}

var semanticVersionPattern = regexp.MustCompile(`(?i)\bv?([0-9]+\.[0-9]+(?:\.[0-9]+)?(?:[-+][0-9a-z][0-9a-z.-]*)?)\b`)

var (
	runtimeVersionProbeTimeout = 5 * time.Second
	runtimeVersionProbeWait    = 2 * time.Second
	errRuntimeCLIIncompatible  = errors.New("runtime CLI is incompatible with this operating-system boundary")
	errRuntimeCLICapability    = errors.New("runtime CLI was found but its required integration capability probe failed")
)

func detectRuntimeVersion(runtimeName, runtimeCLI string) string {
	return detectRuntimeVersionWithEnvironment(runtimeName, runtimeCLI, nil)
}

func detectRuntimeVersionWithEnvironment(runtimeName, runtimeCLI string, environment map[string]string) string {
	if runtimeName == transcriptcapture.RuntimeCopilot {
		configRoot, err := currentCopilotConfigRoot()
		if err != nil {
			return ""
		}
		raw, err := runCopilotCLI(runtimeCLI, configRoot, runtimeVersionProbeTimeout, "--version")
		if err != nil {
			return ""
		}
		return parseRuntimeVersionOutput(raw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), runtimeVersionProbeTimeout)
	defer cancel()
	// Version output is part of the executable's stdout contract. Keep stderr
	// separate: GUI-backed CLIs such as Cursor can emit unrelated diagnostics
	// there before printing their real semantic version on stdout.
	args := []string{"--version"}
	cmd := exec.CommandContext(ctx, runtimeCLI, args...)
	if runtimeName == transcriptcapture.RuntimeOpenClaw && environment != nil {
		cmd = openClawCommandContext(ctx, runtimeCLI, environment, args...)
	}
	cleanup, err := isolateProviderCLIWorkingDirectory(cmd, runtimeName)
	if err != nil {
		return ""
	}
	defer cleanup()
	output := &antigravityValidationOutput{limit: antigravityPluginValidationOutputLimit}
	cmd.Stdout = output
	cmd.WaitDelay = runtimeVersionProbeWait
	err = cmd.Run()
	if err != nil {
		return ""
	}
	return parseRuntimeVersionOutput([]byte(output.String()))
}

func parseRuntimeVersionOutput(raw []byte) string {
	if match := semanticVersionPattern.FindSubmatch(raw); len(match) == 2 {
		return string(match[1])
	}
	for _, line := range strings.Split(string(raw), "\n") {
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
		probeArgs = []string{"mcp", "add-json", "--help"}
	case transcriptcapture.RuntimeGrokBuild:
		candidates = append(candidates, strings.TrimSpace(os.Getenv("GROK_CLI_PATH")))
		if path, err := exec.LookPath("grok"); err == nil {
			candidates = append(candidates, path)
		}
		probeArgs = []string{"mcp", "add", "--help"}
	case transcriptcapture.RuntimeCursor:
		candidates = append(candidates, strings.TrimSpace(os.Getenv("CURSOR_CLI_PATH")))
		if path, err := exec.LookPath("cursor-agent"); err == nil {
			candidates = append(candidates, path)
		}
		if home, err := os.UserHomeDir(); err == nil {
			candidates = append(candidates, filepath.Join(home, ".local", "bin", "cursor-agent"))
		}
		// Some installations expose an MCP-capable binary as `cursor`. Keep it
		// as a compatibility candidate only after the exact capability output
		// below proves that it is the Agent CLI rather than the desktop launcher.
		if path, err := exec.LookPath("cursor"); err == nil {
			candidates = append(candidates, path)
		}
		probeArgs = []string{"mcp", "--help"}
	case transcriptcapture.RuntimeOpenClaw:
		candidates = append(candidates, strings.TrimSpace(os.Getenv("OPENCLAW_CLI_PATH")))
		if path, err := exec.LookPath("openclaw"); err == nil {
			candidates = append(candidates, path)
		}
		probeArgs = []string{"mcp", "add", "--help"}
	case transcriptcapture.RuntimeAntigravity:
		candidates = append(candidates, strings.TrimSpace(os.Getenv("ANTIGRAVITY_CLI_PATH")))
		if path, err := exec.LookPath("agy"); err == nil {
			candidates = append(candidates, path)
		}
		if home, err := os.UserHomeDir(); err == nil {
			candidates = append(candidates, filepath.Join(home, ".local", "bin", "agy"))
		}
		// Antigravity plugin subcommands do not implement conventional nested
		// --help handling; `plugin uninstall --help` targets a plugin literally
		// named --help. Probe only the read-only top-level version contract.
		probeArgs = []string{"--version"}
	case transcriptcapture.RuntimeCopilot:
		candidates = append(candidates, strings.TrimSpace(os.Getenv("COPILOT_CLI_PATH")))
		if path, err := exec.LookPath("copilot"); err == nil {
			candidates = append(candidates, path)
		}
		probeArgs = []string{"mcp", "add", "--help"}
	}
	seen := map[string]bool{}
	foundExistingCandidate := false
	foundIncompatibleCandidate := false
	probeTimeout := runtimeCLICapabilityProbeTimeout
	if runtime == transcriptcapture.RuntimeOpenClaw {
		probeTimeout = openClawCLIReadTimeout
	}
	for _, candidate := range candidates {
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		absolute, absoluteErr := filepath.Abs(candidate)
		if absoluteErr != nil {
			continue
		}
		candidatePath := filepath.Clean(absolute)
		info, statErr := os.Stat(candidatePath)
		if statErr != nil || !info.Mode().IsRegular() {
			continue
		}
		foundExistingCandidate = true
		if err := validateRuntimeCLIPlatformBoundary(candidatePath); err != nil {
			if errors.Is(err, errRuntimeCLIIncompatible) {
				foundIncompatibleCandidate = true
			}
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		cmd := exec.CommandContext(ctx, candidatePath, probeArgs...)
		if runtime == transcriptcapture.RuntimeOpenClaw && environment != nil {
			cmd = openClawCommandContext(ctx, candidatePath, environment, probeArgs...)
		}
		if runtime == transcriptcapture.RuntimeCopilot {
			cancel()
			configRoot, rootErr := currentCopilotConfigRoot()
			if rootErr != nil {
				continue
			}
			if _, probeErr := runCopilotCLI(candidatePath, configRoot, probeTimeout, probeArgs...); probeErr == nil {
				return candidatePath, nil
			}
			continue
		}
		cleanup, isolateErr := isolateProviderCLIWorkingDirectory(cmd, runtime)
		if isolateErr != nil {
			cancel()
			continue
		}
		var capabilityOutput *antigravityValidationOutput
		if runtime == transcriptcapture.RuntimeCursor {
			capabilityOutput = &antigravityValidationOutput{limit: antigravityPluginValidationOutputLimit}
			cmd.Stdout = capabilityOutput
			cmd.Stderr = capabilityOutput
		}
		err := cmd.Run()
		cleanup()
		cancel()
		if err == nil && (runtime != transcriptcapture.RuntimeCursor || strings.Contains(capabilityOutput.String(), "Manage MCP servers")) {
			return candidatePath, nil
		}
	}
	if foundIncompatibleCandidate {
		return "", fmt.Errorf("%w: %s resolved to a Windows executable; install and run both Witself and the provider CLI inside the same Windows or WSL environment", errRuntimeCLIIncompatible, runtime)
	}
	if foundExistingCandidate {
		return "", fmt.Errorf("%w: no %s executable passed its MCP capability probe", errRuntimeCLICapability, runtime)
	}
	return "", fmt.Errorf("no %s executable with MCP support was found", runtime)
}

func validateRuntimeCLIPlatformBoundary(path string) error {
	return validateRuntimeCLIPlatformBoundaryForPlatform(runtimepkg.GOOS, path)
}

func validateRuntimeCLIPlatformBoundaryForPlatform(platform, path string) error {
	if platform != "linux" {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	var magic [2]byte
	if _, err := io.ReadFull(file, magic[:]); err != nil {
		return nil
	}
	if magic == [2]byte{'M', 'Z'} {
		return errRuntimeCLIIncompatible
	}
	return nil
}

func registerMCP(runtime, runtimeCLI, witselfExecutable, account, realm, agent, location string) error {
	serveArgs := runtimeMCPServeArgs(runtime, witselfExecutable, account, realm, agent, location)
	if runtime == transcriptcapture.RuntimeCursor {
		if err := registerCursorMCP(serveArgs); err != nil {
			return err
		}
		output, err := runLegacyProviderCLI(runtimeCLI, 15*time.Second, "mcp", "enable", "witself")
		if err != nil {
			_ = unregisterCursorMCP()
			return fmt.Errorf("approve Cursor MCP: %w: %s", err, strings.TrimSpace(string(output)))
		}
		return nil
	}
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
	_, _ = runLegacyProviderCLI(runtimeCLI, 15*time.Second, removeArgs...)
	output, err := runLegacyProviderCLI(runtimeCLI, 15*time.Second, addArgs...)
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
			_, _ = runLegacyProviderCLI(runtimeCLI, 15*time.Second, "mcp", "disable", "witself")
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
	args := []string{"mcp", "remove", "witself"}
	if runtime == transcriptcapture.RuntimeClaudeCode || runtime == transcriptcapture.RuntimeGrokBuild {
		args = []string{"mcp", "remove", "--scope", "user", "witself"}
	}
	output, err := runLegacyProviderCLI(runtimeCLI, 15*time.Second, args...)
	if err != nil {
		if mcpRegistrationAlreadyMissing(output) {
			return nil
		}
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func runLegacyProviderCLI(runtimeCLI string, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, runtimeCLI, args...)
	cleanup, err := isolateProviderCLIWorkingDirectory(cmd, "provider")
	if err != nil {
		return nil, err
	}
	defer cleanup()
	cmd.WaitDelay = genericProviderCLIWaitDelay
	output := &antigravityValidationOutput{limit: genericProviderCLIOutputLimit}
	cmd.Stdout = output
	cmd.Stderr = output
	err = cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return []byte(output.String()), fmt.Errorf("provider CLI timed out after %s", timeout)
	}
	if err != nil {
		message := strings.TrimSpace(output.String())
		if message == "" {
			message = err.Error()
		}
		return []byte(output.String()), errors.New(message)
	}
	return []byte(output.String()), nil
}
