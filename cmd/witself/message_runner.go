package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/messagerunner"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const messageRunnerUsage = "usage: witself message runner enable|disable|status|notifications|run|serve|start ..."

var messageRunnerModelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+@-]{0,127}$`)

type messageRunnerStatus struct {
	Configured        bool                          `json:"configured"`
	Config            messagerunner.PersistedConfig `json:"config,omitempty"`
	Service           messagerunner.ServiceStatus   `json:"service"`
	NotificationCount int                           `json:"notification_count"`
	Health            messagerunner.Health          `json:"health"`
}

func messageRunnerCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, messageRunnerUsage)
		return 2
	}
	switch args[0] {
	case "enable":
		return messageRunnerEnable(args[1:])
	case "disable":
		return messageRunnerDisable(args[1:])
	case "status":
		return messageRunnerStatusCmd(args[1:])
	case "notifications":
		return messageRunnerNotificationsCmd(args[1:])
	case "run":
		return messageRunnerRun(args[1:], false)
	case "serve":
		return messageRunnerRun(args[1:], true)
	case "start":
		return messageRunnerStart(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself message runner: unknown subcommand %q\n", args[0])
		return 2
	}
}

func messageRunnerEnable(args []string) int {
	fs := flag.NewFlagSet("message runner enable", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	runtimeFlag := fs.String("runtime", "", "installed runtime binding")
	providerFlag := fs.String("provider", "", "native text provider (claude-code or grok-build)")
	providerPathFlag := fs.String("provider-path", "", "optional provider executable path")
	model := fs.String("model", "", "optional provider model")
	maximumTurns := fs.Int("max-turns", 12, "maximum automated turns in one conversation (1-64)")
	replaceBinding := fs.Bool("replace-binding", false, "replace a runner pinned to a different installed identity")
	noService := fs.Bool("no-service", false, "configure without installing the per-user background service")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*runtimeFlag) == "" || *maximumTurns < 1 || *maximumTurns > 64 {
		fmt.Fprintln(os.Stderr, "usage: witself message runner enable --runtime RUNTIME [--provider PROVIDER] [--max-turns 1-64]")
		return 2
	}
	runtimeName, err := transcriptcapture.NormalizeRuntime(*runtimeFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	providerName := strings.TrimSpace(*providerFlag)
	if providerName == "" {
		providerName = runtimeName
	}
	provider, err := parseMessageNativeProvider(providerName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	if strings.TrimSpace(*model) != "" && !messageRunnerModelPattern.MatchString(strings.TrimSpace(*model)) {
		fmt.Fprintln(os.Stderr, "witself: --model is not a valid native provider model name")
		return 2
	}
	providerPath := strings.TrimSpace(*providerPathFlag)
	if providerPath != "" {
		providerPath, err = filepath.Abs(providerPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: resolve provider path: %v\n", err)
			return 2
		}
	}

	binding, err := transcriptcapture.LoadConfig(runtimeName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: load %s integration: %v\n", runtimeName, err)
		return 1
	}
	if err := validateMessageRunnerBinding(binding); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 15*time.Second)
	capability, err := (messagerunner.NativeTextProvider{
		Provider: provider, Path: providerPath, Model: strings.TrimSpace(*model),
	}).Probe(probeCtx)
	cancelProbe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: probe message runner provider: %v\n", err)
		return 1
	}
	if !capability.Supported {
		fmt.Fprintf(os.Stderr, "witself: native provider %s is unavailable: %s\n", provider, capability.UnsupportedReason)
		return 1
	}

	preflightCtx, cancelPreflight := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelPreflight()
	conn, err := connectAgent(preflightCtx, binding.Account, binding.Realm, binding.Agent, binding.Endpoint, binding.TokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: connect message runner: %v\n", err)
		return 1
	}
	self, err := client.GetSelf(preflightCtx, conn.Endpoint, conn.Token, client.SelfOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: verify message runner identity: %v\n", err)
		return 1
	}
	if !messageRunnerIdentityMatches(binding, self.Identity) {
		fmt.Fprintln(os.Stderr, "witself: authenticated token does not match the installed runtime binding")
		return 1
	}

	store, err := messagerunner.DefaultConfigStore(runtimeName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: initialize message runner: %v\n", err)
		return 1
	}
	releaseRunner, acquired, err := store.Acquire()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: acquire message runner configuration: %v\n", err)
		return 1
	}
	if !acquired {
		fmt.Fprintln(os.Stderr, "witself: the message runner is active; disable it before changing its binding or provider")
		return 1
	}
	releasedRunner := false
	defer func() {
		if !releasedRunner {
			_ = releaseRunner()
		}
	}()
	config, err := store.Enable(messagerunner.Settings{
		Runtime: runtimeName, AccountID: binding.AccountID, RealmID: binding.RealmID,
		AgentID: binding.AgentID, AgentName: binding.AgentName,
		Provider: string(provider), ProviderPath: capability.Executable,
		Model: strings.TrimSpace(*model), MaximumTurns: *maximumTurns,
		ReplaceBinding: *replaceBinding,
	})
	if err != nil {
		if errors.Is(err, messagerunner.ErrRunnerBindingConflict) {
			fmt.Fprintln(os.Stderr, "witself: message runner is pinned to another identity; use --replace-binding after verifying the runtime binding")
		} else {
			fmt.Fprintf(os.Stderr, "witself: configure message runner: %v\n", err)
		}
		return 1
	}
	if err := store.CaptureProviderCredentials(config.Provider, os.Environ()); err != nil {
		_, _ = store.Disable()
		fmt.Fprintf(os.Stderr, "witself: capture message runner provider credentials: %v\n", err)
		return 1
	}
	if err := releaseRunner(); err != nil {
		_, _ = store.Disable()
		fmt.Fprintf(os.Stderr, "witself: release message runner configuration: %v\n", err)
		return 1
	}
	releasedRunner = true

	status := messageRunnerStatus{Configured: true, Config: config}
	if !*noService {
		executable, executableErr := os.Executable()
		if executableErr != nil {
			_, _ = store.Disable()
			fmt.Fprintf(os.Stderr, "witself: resolve executable for message runner service: %v\n", executableErr)
			return 1
		}
		manager, managerErr := messagerunner.DefaultServiceManager(executable)
		if managerErr != nil {
			_, _ = store.Disable()
			fmt.Fprintf(os.Stderr, "witself: initialize message runner service: %v\n", managerErr)
			return 1
		}
		status.Service, err = manager.Install(preflightCtx, runtimeName)
		if err != nil {
			_, _ = store.Disable()
			fmt.Fprintf(os.Stderr, "witself: install message runner service: %v\n", err)
			return 1
		}
	}
	if *jsonOut {
		return printJSON(status)
	}
	fmt.Printf("enabled\truntime=%s\tagent=%s\tprovider=%s\tservice=%t\n",
		tabSafe(safeText(runtimeName)), tabSafe(safeText(config.AgentName)),
		tabSafe(safeText(config.Provider)), status.Service.Installed)
	return 0
}

func messageRunnerDisable(args []string) int {
	runtimeName, jsonOut, code := parseMessageRunnerRuntime("disable", args)
	if code != 0 {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: resolve executable: %v\n", err)
		return 1
	}
	manager, err := messagerunner.DefaultServiceManager(executable)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: initialize message runner service: %v\n", err)
		return 1
	}
	serviceStatus, err := manager.Uninstall(ctx, runtimeName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: uninstall message runner service: %v\n", err)
		return 1
	}
	store, err := messagerunner.DefaultConfigStore(runtimeName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: initialize message runner: %v\n", err)
		return 1
	}
	releaseRunner, acquired, err := store.Acquire()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: acquire message runner for disable: %v\n", err)
		return 1
	}
	if !acquired {
		fmt.Fprintln(os.Stderr, "witself: the message runner is still active; stop the foreground runner and retry disable")
		return 1
	}
	defer func() { _ = releaseRunner() }()
	config, err := store.Disable()
	if errors.Is(err, messagerunner.ErrRunnerNotConfigured) {
		status := messageRunnerStatus{Service: serviceStatus}
		if jsonOut {
			return printJSON(status)
		}
		fmt.Printf("disabled\truntime=%s\n", runtimeName)
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: disable message runner: %v\n", err)
		return 1
	}
	status := messageRunnerStatus{Configured: true, Config: config, Service: serviceStatus}
	if jsonOut {
		return printJSON(status)
	}
	fmt.Printf("disabled\truntime=%s\tagent=%s\n",
		tabSafe(safeText(runtimeName)), tabSafe(safeText(config.AgentName)))
	return 0
}

func messageRunnerStatusCmd(args []string) int {
	runtimeName, jsonOut, code := parseMessageRunnerRuntime("status", args)
	if code != 0 {
		return code
	}
	store, err := messagerunner.DefaultConfigStore(runtimeName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: initialize message runner: %v\n", err)
		return 1
	}
	status := messageRunnerStatus{}
	status.Config, err = store.Load()
	if err == nil {
		status.Configured = true
	} else if !errors.Is(err, messagerunner.ErrRunnerNotConfigured) {
		fmt.Fprintf(os.Stderr, "witself: inspect message runner: %v\n", err)
		return 1
	}
	notifications, err := store.Notifications(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: inspect message runner notifications: %v\n", err)
		return 1
	}
	status.NotificationCount = len(notifications)
	status.Health, err = store.RunnerHealth(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: inspect message runner health: %v\n", err)
		return 1
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: resolve executable: %v\n", err)
		return 1
	}
	manager, err := messagerunner.DefaultServiceManager(executable)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: initialize message runner service: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	status.Service, err = manager.Status(ctx, runtimeName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: inspect message runner service: %v\n", err)
		return 1
	}
	if jsonOut {
		return printJSON(status)
	}
	state := "unconfigured"
	if status.Configured {
		state = map[bool]string{true: "enabled", false: "disabled"}[status.Config.Enabled]
	}
	fmt.Printf("%s\truntime=%s\tagent=%s\tprovider=%s\tservice-installed=%t\tservice-active=%t\tnotifications=%d\tlast=%s\tfailures=%d\terror-class=%s\n",
		tabSafe(safeText(state)), tabSafe(safeText(runtimeName)),
		tabSafe(safeText(status.Config.AgentName)), tabSafe(safeText(status.Config.Provider)),
		status.Service.Installed, status.Service.Active, status.NotificationCount,
		tabSafe(safeText(status.Health.LastStatus)), status.Health.ConsecutiveFailures,
		tabSafe(safeText(status.Health.LastErrorClass)))
	return 0
}

func messageRunnerNotificationsCmd(args []string) int {
	fs := flag.NewFlagSet("message runner notifications", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	runtimeFlag := fs.String("runtime", "", "installed runtime binding")
	clearAll := fs.Bool("clear-all", false, "clear every recorded notification pointer")
	var clearIDs csvListFlag
	fs.Var(&clearIDs, "clear", "clear exact message id(s), repeatable or comma-separated")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*runtimeFlag) == "" || (*clearAll && len(clearIDs) != 0) {
		fmt.Fprintln(os.Stderr, "usage: witself message runner notifications --runtime RUNTIME [--clear MSG_ID|--clear-all] [--json]")
		return 2
	}
	runtimeName, err := transcriptcapture.NormalizeRuntime(*runtimeFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	store, err := messagerunner.DefaultConfigStore(runtimeName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: initialize message runner: %v\n", err)
		return 1
	}
	removed := 0
	if *clearAll || len(clearIDs) != 0 {
		ids := []string(clearIDs)
		if *clearAll {
			ids = nil
		}
		removed, err = store.ClearNotifications(context.Background(), ids)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: clear message runner notifications: %v\n", err)
			return 1
		}
	}
	notifications, err := store.Notifications(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: inspect message runner notifications: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"notifications": notifications, "cleared": removed})
	}
	if *clearAll || len(clearIDs) != 0 {
		fmt.Printf("cleared\t%d\n", removed)
	}
	for _, notification := range notifications {
		fmt.Printf("%s\t%s\t%s\t%s\t%s\t%s\n",
			notification.RecordedAt.Format(time.RFC3339), notification.MessageID,
			notification.ThreadID, tabSafe(safeText(notification.Kind)),
			notification.FromAgentID, tabSafe(safeText(notification.FromAgentName)))
	}
	return 0
}

func messageRunnerStart(args []string) int {
	runtimeName, jsonOut, code := parseMessageRunnerRuntime("start", args)
	if code != 0 {
		return code
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: resolve executable: %v\n", err)
		return 1
	}
	manager, err := messagerunner.DefaultServiceManager(executable)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: initialize message runner service: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	status, err := manager.Start(ctx, runtimeName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: start message runner service: %v\n", err)
		return 1
	}
	if jsonOut {
		return printJSON(status)
	}
	fmt.Printf("started\truntime=%s\tactive=%t\n", runtimeName, status.Active)
	return 0
}

func messageRunnerRun(args []string, service bool) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return messageRunnerRunContext(ctx, args, service)
}

func messageRunnerRunContext(ctx context.Context, args []string, service bool) int {
	command := "run"
	if service {
		command = "serve"
	}
	fs := flag.NewFlagSet("message runner "+command, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	runtimeFlag := fs.String("runtime", "", "installed runtime binding")
	once := fs.Bool("once", false, "process at most one oldest unacknowledged message")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*runtimeFlag) == "" || (service && (*once || *jsonOut)) {
		fmt.Fprintf(os.Stderr, "usage: witself message runner %s --runtime RUNTIME", command)
		if !service {
			fmt.Fprintln(os.Stderr, " [--once] [--json]")
		} else {
			fmt.Fprintln(os.Stderr)
		}
		return 2
	}
	runtimeName, err := transcriptcapture.NormalizeRuntime(*runtimeFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	binding, err := transcriptcapture.LoadConfig(runtimeName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: load %s integration: %v\n", runtimeName, err)
		return 1
	}
	store, err := messagerunner.DefaultConfigStore(runtimeName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: initialize message runner: %v\n", err)
		return 1
	}
	config, err := store.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: load message runner configuration: %v\n", err)
		return 1
	}
	if !config.Enabled {
		fmt.Fprintln(os.Stderr, "witself: message runner is disabled")
		return 1
	}
	if config.Runtime != runtimeName || config.AccountID != binding.AccountID || config.RealmID != binding.RealmID ||
		config.AgentID != binding.AgentID || config.AgentName != binding.AgentName {
		fmt.Fprintln(os.Stderr, "witself: message runner configuration does not match the installed runtime binding")
		return 1
	}
	provider, err := parseMessageNativeProvider(config.Provider)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	providerEnvironment, err := store.ProviderEnvironment(config.Provider)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: load message runner provider credentials: %v\n", err)
		return 1
	}
	providerImpl := messagerunner.NativeTextProvider{
		Provider: provider, Path: config.ProviderPath, Model: config.Model, Env: providerEnvironment,
	}
	capability, err := providerImpl.Probe(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: probe message runner provider: %v\n", err)
		return 1
	}
	if !capability.Supported {
		fmt.Fprintf(os.Stderr, "witself: message runner provider is unavailable: %s\n", capability.UnsupportedReason)
		return 1
	}
	conn, err := connectAgent(ctx, binding.Account, binding.Realm, binding.Agent, binding.Endpoint, binding.TokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: connect message runner: %v\n", err)
		return 1
	}
	release, acquired, err := store.Acquire()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: acquire message runner: %v\n", err)
		return 1
	}
	if !acquired {
		fmt.Fprintln(os.Stderr, "witself: a message runner is already active for this runtime")
		return 1
	}
	defer func() { _ = release() }()
	runner := messagerunner.Runner{
		API:      messagerunner.HTTPAPI{Endpoint: conn.Endpoint, Token: conn.Token},
		Provider: providerImpl,
		State:    store,
		Config: messagerunner.Config{
			RunnerID: config.RunnerID,
			ExpectedIdentity: client.SelfIdentity{
				AccountID: config.AccountID, RealmID: config.RealmID,
				AgentID: config.AgentID, AgentName: config.AgentName,
			},
			MaximumTurns: config.MaximumTurns,
		},
	}
	recordCycle := func(result messagerunner.RunResult, cycleErr error) {
		if err := store.RecordCycle(context.Background(), result, cycleErr); err != nil {
			fmt.Fprintln(os.Stderr, "witself: message runner could not update its content-free health record")
		}
	}
	if *once {
		result, err := runner.RunOnce(ctx)
		recordCycle(result, err)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: run message runner: %v\n", err)
			return 1
		}
		if *jsonOut {
			return printJSON(result)
		}
		fmt.Printf("%s\tmessage=%s\tresult=%s\n", result.Status, result.MessageID, result.ResultMessageID)
		return 0
	}
	if err := runner.Serve(ctx, messagerunner.LoopOptions{Observe: recordCycle}); err != nil {
		fmt.Fprintf(os.Stderr, "witself: message runner stopped: %v\n", err)
		return 1
	}
	return 0
}

func parseMessageRunnerRuntime(command string, args []string) (string, bool, int) {
	fs := flag.NewFlagSet("message runner "+command, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	runtimeFlag := fs.String("runtime", "", "installed runtime binding")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return "", false, 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*runtimeFlag) == "" {
		fmt.Fprintf(os.Stderr, "usage: witself message runner %s --runtime RUNTIME\n", command)
		return "", false, 2
	}
	runtimeName, err := transcriptcapture.NormalizeRuntime(*runtimeFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return "", false, 2
	}
	return runtimeName, *jsonOut, 0
}

func parseMessageNativeProvider(value string) (messagerunner.NativeProvider, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "codex":
		return messagerunner.ProviderCodex, nil
	case "claude", "claude-code":
		return messagerunner.ProviderClaudeCode, nil
	case "grok", "grok-build":
		return messagerunner.ProviderGrokBuild, nil
	case "cursor":
		return messagerunner.ProviderCursor, nil
	default:
		return "", errors.New("message runner provider must be codex, claude-code, grok-build, or cursor")
	}
}

func validateMessageRunnerBinding(binding transcriptcapture.Config) error {
	if strings.TrimSpace(binding.AccountID) == "" || strings.TrimSpace(binding.RealmID) == "" ||
		strings.TrimSpace(binding.AgentID) == "" || strings.TrimSpace(binding.AgentName) == "" {
		return errors.New("message runner requires a current installed account, realm, agent id, and agent name; reinstall the runtime binding")
	}
	return nil
}

func messageRunnerIdentityMatches(binding transcriptcapture.Config, identity client.SelfIdentity) bool {
	return identity.AccountID == binding.AccountID && identity.RealmID == binding.RealmID &&
		identity.AgentID == binding.AgentID && identity.AgentName == binding.AgentName
}
