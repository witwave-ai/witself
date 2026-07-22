package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const integrationsSchemaVersion = "witself.integrations.v1"

const (
	integrationDetectionAvailable = "available"
	integrationDetectionNotFound  = "not_found"
	integrationDetectionError     = "error"

	integrationStateInstalled    = "installed"
	integrationStateNotInstalled = "not_installed"
	integrationStateError        = "error"
)

type integrationDetection struct {
	State      string `json:"state"`
	Executable string `json:"executable,omitempty"`
	Version    string `json:"version,omitempty"`
	Message    string `json:"message,omitempty"`
}

type integrationBindingStatus struct {
	State    string `json:"state"`
	Account  string `json:"account,omitempty"`
	Realm    string `json:"realm,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Location string `json:"location,omitempty"`
	HookMode string `json:"hook_mode,omitempty"`
	Message  string `json:"message,omitempty"`
}

type integrationCapabilities struct {
	MCP                bool `json:"mcp"`
	ManagedRouting     bool `json:"managed_routing"`
	TranscriptHooks    bool `json:"transcript_hooks"`
	AdministratorHooks bool `json:"administrator_hooks"`
}

type integrationRuntimeStatus struct {
	Runtime      string                   `json:"runtime"`
	DisplayName  string                   `json:"display_name"`
	Detection    integrationDetection     `json:"detection"`
	Integration  integrationBindingStatus `json:"integration"`
	Capabilities integrationCapabilities  `json:"capabilities"`
}

type integrationsSummary struct {
	Supported int `json:"supported"`
	Detected  int `json:"detected"`
	Installed int `json:"installed"`
	Attention int `json:"attention"`
}

type integrationsReport struct {
	SchemaVersion string                     `json:"schema_version"`
	Runtimes      []integrationRuntimeStatus `json:"runtimes"`
	Summary       integrationsSummary        `json:"summary"`
}

type bulkIntegrationResult struct {
	Runtime string
	Result  string
	Detail  string
}

var (
	probeRuntimeForIntegrationCatalog = probeIntegrationRuntime
	loadIntegrationForCatalog         = transcriptcapture.LoadConfig
	installOneIntegration             func([]string) int
	uninstallOneIntegration           func([]string) int
	suppressIntegrationSuccessOutput  bool
	integrationsOutput                io.Writer
)

func integrationsWriter() io.Writer {
	if integrationsOutput != nil {
		return integrationsOutput
	}
	return os.Stdout
}

func callInstallOneIntegration(args []string) int {
	if installOneIntegration != nil {
		return installOneIntegration(args)
	}
	return installCmd(args)
}

func callUninstallOneIntegration(args []string) int {
	if uninstallOneIntegration != nil {
		return uninstallOneIntegration(args)
	}
	return uninstallCmd(args)
}

func integrationsCmd(args []string) int {
	fs := flag.NewFlagSet("integrations", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOutput := fs.Bool("json", false, "emit a stable machine-readable inventory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself integrations [--json]")
		return 2
	}
	report := collectIntegrationsReport()
	if *jsonOutput {
		encoder := json.NewEncoder(integrationsWriter())
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			fmt.Fprintf(os.Stderr, "witself: encode integrations inventory: %v\n", err)
			return 1
		}
		return 0
	}
	writeIntegrationsTable(integrationsWriter(), report)
	return 0
}

func collectIntegrationsReport() integrationsReport {
	runtimes := transcriptcapture.SupportedRuntimes()
	statuses := make([]integrationRuntimeStatus, len(runtimes))
	var wait sync.WaitGroup
	for index, runtimeName := range runtimes {
		index, runtimeName := index, runtimeName
		wait.Add(1)
		go func() {
			defer wait.Done()
			statuses[index] = inspectIntegrationRuntime(runtimeName)
		}()
	}
	wait.Wait()
	report := integrationsReport{
		SchemaVersion: integrationsSchemaVersion,
		Runtimes:      statuses,
		Summary:       integrationsSummary{Supported: len(statuses)},
	}
	for _, status := range statuses {
		if status.Detection.State == integrationDetectionAvailable {
			report.Summary.Detected++
		}
		if status.Integration.State == integrationStateInstalled {
			report.Summary.Installed++
		}
		if status.Detection.State == integrationDetectionError || status.Integration.State == integrationStateError {
			report.Summary.Attention++
		}
	}
	return report
}

func inspectIntegrationRuntime(runtimeName string) integrationRuntimeStatus {
	status := integrationRuntimeStatus{
		Runtime:     runtimeName,
		DisplayName: integrationDisplayName(runtimeName),
		Detection:   probeRuntimeForIntegrationCatalog(runtimeName),
		Integration: integrationBindingStatus{State: integrationStateNotInstalled},
		Capabilities: integrationCapabilities{
			MCP:                true,
			ManagedRouting:     true,
			TranscriptHooks:    supportsTranscriptHooks(runtimeName),
			AdministratorHooks: supportsManagedHooks(runtimeName),
		},
	}
	cfg, err := loadIntegrationForCatalog(runtimeName)
	if errors.Is(err, os.ErrNotExist) {
		return status
	}
	if err != nil {
		status.Integration = integrationBindingStatus{State: integrationStateError, Message: err.Error()}
		return status
	}
	status.Integration = integrationBindingStatus{
		State:    integrationStateInstalled,
		Account:  defaultString(cfg.Account, "default"),
		Realm:    defaultString(cfg.Realm, "default"),
		Agent:    defaultString(cfg.Agent, cfg.AgentName),
		Location: cfg.Location.Name,
		HookMode: cfg.HookMode,
	}
	return status
}

func probeIntegrationRuntime(runtimeName string) integrationDetection {
	var environment map[string]string
	if runtimeName == transcriptcapture.RuntimeOpenClaw {
		var err error
		environment, err = captureOpenClawMCPEnvironment()
		if err != nil {
			return integrationDetection{State: integrationDetectionError, Message: err.Error()}
		}
	}
	executable, err := findRuntimeCLIWithEnvironment(runtimeName, environment)
	if err != nil {
		return integrationDetection{State: integrationDetectionNotFound, Message: err.Error()}
	}
	return integrationDetection{
		State:      integrationDetectionAvailable,
		Executable: executable,
		Version:    detectRuntimeVersionWithEnvironment(runtimeName, executable, environment),
	}
}

func integrationDisplayName(runtimeName string) string {
	switch runtimeName {
	case transcriptcapture.RuntimeCodex:
		return "Codex"
	case transcriptcapture.RuntimeClaudeCode:
		return "Claude Code"
	case transcriptcapture.RuntimeGrokBuild:
		return "Grok Build"
	case transcriptcapture.RuntimeCursor:
		return "Cursor"
	case transcriptcapture.RuntimeOpenClaw:
		return "OpenClaw"
	case transcriptcapture.RuntimeAntigravity:
		return "Antigravity"
	default:
		return runtimeName
	}
}

func writeIntegrationsTable(w io.Writer, report integrationsReport) {
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "RUNTIME\tAPPLICATION\tCLI\tVERSION\tWITSELF\tAGENT\tLOCATION\tHOOKS")
	for _, status := range report.Runtimes {
		cli := "error"
		switch status.Detection.State {
		case integrationDetectionAvailable:
			cli = "found"
		case integrationDetectionNotFound:
			cli = "not found"
		}
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			status.Runtime,
			status.DisplayName,
			cli,
			valueOrDash(status.Detection.Version),
			strings.ReplaceAll(status.Integration.State, "_", " "),
			valueOrDash(status.Integration.Agent),
			valueOrDash(status.Integration.Location),
			valueOrDash(status.Integration.HookMode),
		)
	}
	_ = table.Flush()
	for _, status := range report.Runtimes {
		if status.Detection.State == integrationDetectionError {
			_, _ = fmt.Fprintf(w, "\n%s detection error: %s\n", status.Runtime, status.Detection.Message)
		}
		if status.Integration.State == integrationStateError {
			_, _ = fmt.Fprintf(w, "\n%s integration error: %s\n", status.Runtime, status.Integration.Message)
		}
	}
	_, _ = fmt.Fprintf(w, "\n%d supported, %d detected, %d installed, %d need attention\n",
		report.Summary.Supported,
		report.Summary.Detected,
		report.Summary.Installed,
		report.Summary.Attention,
	)
}

func valueOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func bulkInstallPreview(
	status integrationRuntimeStatus,
	setFlags map[string]bool,
	account, realm, agent, endpoint, tokenFile string,
) (string, string) {
	switch status.Integration.State {
	case integrationStateError:
		return "blocked", status.Integration.Message
	case integrationStateNotInstalled:
		return "would install", ""
	}

	changed := make([]string, 0, 3)
	unknownIdentity := false
	for _, selector := range []struct {
		name     string
		flag     string
		env      string
		fallback string
		current  string
	}{
		{name: "account", flag: account, env: os.Getenv("WITSELF_ACCOUNT"), fallback: "default", current: status.Integration.Account},
		{name: "realm", flag: realm, env: os.Getenv("WITSELF_REALM"), fallback: "default", current: status.Integration.Realm},
		{name: "agent", flag: agent, env: os.Getenv("WITSELF_AGENT"), current: status.Integration.Agent},
	} {
		selected := setFlags[selector.name] || strings.TrimSpace(selector.env) != ""
		if !selected {
			continue
		}
		value := strings.TrimSpace(selector.flag)
		if value == "" {
			value = strings.TrimSpace(selector.env)
		}
		if value == "" {
			value = selector.fallback
		}
		if value == "" {
			unknownIdentity = true
			continue
		}
		if value != selector.current {
			changed = append(changed, selector.name)
		}
	}
	if len(changed) != 0 {
		return "would rebind", "identity change: " + strings.Join(changed, ", ")
	}
	if setFlags["endpoint"] || setFlags["token-file"] ||
		strings.TrimSpace(endpoint) != "" || strings.TrimSpace(tokenFile) != "" || unknownIdentity {
		return "would refresh/rebind", "explicit connection or unresolved identity; authenticated identity decides"
	}
	return "would refresh", ""
}

func bulkInstallHookArgs(
	status integrationRuntimeStatus,
	setFlags map[string]bool,
	managedHooks, userHooks bool,
) ([]string, string) {
	if !status.Capabilities.TranscriptHooks {
		return nil, ""
	}
	if setFlags["user-hooks"] && userHooks {
		return []string{"--user-hooks"}, "user hooks"
	}
	if setFlags["managed-hooks"] {
		if managedHooks && status.Capabilities.AdministratorHooks {
			return []string{"--managed-hooks"}, "administrator-managed hooks"
		}
		return []string{"--managed-hooks=false"}, "user hooks"
	}
	if status.Integration.State == integrationStateInstalled {
		switch status.Integration.HookMode {
		case transcriptcapture.HookModeManaged:
			if status.Capabilities.AdministratorHooks {
				return []string{"--managed-hooks"}, "preserve administrator-managed hooks"
			}
		case transcriptcapture.HookModeUser:
			return []string{"--user-hooks"}, "preserve user hooks"
		}
	}
	if status.Capabilities.AdministratorHooks {
		return nil, "administrator-managed hooks by default; access may be requested"
	}
	return nil, "user hooks"
}

func joinBulkDetails(parts ...string) string {
	nonempty := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			nonempty = append(nonempty, part)
		}
	}
	return strings.Join(nonempty, "; ")
}

func installAllCmd(args []string) int {
	fs := flag.NewFlagSet("install all", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	realm := fs.String("realm", "", `local realm name (default: WITSELF_REALM or "default")`)
	agent := fs.String("agent", "", "local agent name")
	location := fs.String("location", strings.TrimSpace(os.Getenv("WITSELF_LOCATION")), "stable human label for this machine")
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing an agent token")
	dryRun := fs.Bool("dry-run", false, "show the plan without changing any runtime")
	managedHooks := fs.Bool("managed-hooks", false, "use administrator-managed hooks where supported")
	userHooks := fs.Bool("user-hooks", false, "use user-scoped hooks where supported")
	_ = fs.String("capture", transcriptcapture.ModeRaw, "not supported with install all")
	_ = fs.Bool("routing-only", false, "not supported with install all")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself install all [--account NAME] [--realm NAME] [--agent NAME] [--location NAME] [--endpoint URL] [--token-file FILE] [--managed-hooks|--user-hooks] [--dry-run]")
		return 2
	}
	setFlags := map[string]bool{}
	fs.Visit(func(current *flag.Flag) { setFlags[current.Name] = true })
	for _, unsupported := range []string{"capture", "routing-only"} {
		if setFlags[unsupported] {
			fmt.Fprintf(os.Stderr, "witself: --%s is provider-specific and is not supported with `install all`; use an explicit runtime list\n", unsupported)
			return 2
		}
	}
	if *userHooks && setFlags["managed-hooks"] && *managedHooks {
		fmt.Fprintln(os.Stderr, "witself: --user-hooks conflicts with --managed-hooks")
		return 2
	}
	forward := make([]string, 0, 12)
	for _, option := range []struct {
		name  string
		value string
	}{
		{"account", *account},
		{"realm", *realm},
		{"agent", *agent},
		{"location", *location},
		{"endpoint", *endpoint},
		{"token-file", *tokenFile},
	} {
		if setFlags[option.name] {
			forward = append(forward, "--"+option.name, option.value)
		}
	}
	report := collectIntegrationsReport()
	results := make([]bulkIntegrationResult, 0, len(report.Runtimes))
	attempted := 0
	exitCode := 0
	for _, status := range report.Runtimes {
		switch status.Detection.State {
		case integrationDetectionNotFound:
			results = append(results, bulkIntegrationResult{Runtime: status.Runtime, Result: "not detected"})
			continue
		case integrationDetectionError:
			results = append(results, bulkIntegrationResult{Runtime: status.Runtime, Result: "failed", Detail: status.Detection.Message})
			if exitCode == 0 {
				exitCode = 1
			}
			continue
		}
		attempted++
		action, detail := bulkInstallPreview(status, setFlags, *account, *realm, *agent, *endpoint, *tokenFile)
		hookArgs, hookDetail := bulkInstallHookArgs(status, setFlags, *managedHooks, *userHooks)
		detail = joinBulkDetails(detail, hookDetail)
		if *dryRun {
			if action == "blocked" {
				exitCode = 1
			}
			results = append(results, bulkIntegrationResult{Runtime: status.Runtime, Result: action, Detail: detail})
			continue
		}
		_, _ = fmt.Fprintf(integrationsWriter(), "installing %s...\n", status.Runtime)
		runtimeArgs := append([]string{status.Runtime}, forward...)
		runtimeArgs = append(runtimeArgs, hookArgs...)
		code := runBulkIntegrationCommand(func() int {
			return callInstallOneIntegration(runtimeArgs)
		})
		if code != 0 {
			results = append(results, bulkIntegrationResult{Runtime: status.Runtime, Result: "failed", Detail: fmt.Sprintf("exit code %d", code)})
			if code == 2 {
				exitCode = 2
			} else if exitCode == 0 {
				exitCode = 1
			}
			continue
		}
		outcome := "installed"
		if status.Integration.State == integrationStateInstalled {
			outcome = "refreshed"
			switch action {
			case "would rebind":
				outcome = "rebound"
			case "would refresh/rebind":
				outcome = "updated"
			}
		}
		results = append(results, bulkIntegrationResult{Runtime: status.Runtime, Result: outcome, Detail: hookDetail})
	}
	writeBulkIntegrationResults(integrationsWriter(), results, *dryRun)
	if attempted == 0 {
		fmt.Fprintln(os.Stderr, "witself: no supported AI runtime with MCP capability was detected")
		return 1
	}
	return exitCode
}

func uninstallAllCmd(args []string) int {
	fs := flag.NewFlagSet("uninstall all", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dryRun := fs.Bool("dry-run", false, "show the plan without changing any runtime")
	_ = fs.Bool("managed-hooks", false, "not supported with uninstall all")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself uninstall all [--dry-run]")
		return 2
	}
	setFlags := map[string]bool{}
	fs.Visit(func(current *flag.Flag) { setFlags[current.Name] = true })
	if setFlags["managed-hooks"] {
		fmt.Fprintln(os.Stderr, "witself: --managed-hooks is provider-specific and is not supported with `uninstall all`; installed hook ownership is removed automatically")
		return 2
	}
	results := make([]bulkIntegrationResult, 0, len(transcriptcapture.SupportedRuntimes()))
	exitCode := 0
	installed := 0
	for _, runtimeName := range transcriptcapture.SupportedRuntimes() {
		_, err := loadIntegrationForCatalog(runtimeName)
		if errors.Is(err, os.ErrNotExist) {
			results = append(results, bulkIntegrationResult{Runtime: runtimeName, Result: "not installed"})
			continue
		}
		if err != nil {
			results = append(results, bulkIntegrationResult{Runtime: runtimeName, Result: "failed", Detail: err.Error()})
			if exitCode == 0 {
				exitCode = 1
			}
			continue
		}
		installed++
		if *dryRun {
			results = append(results, bulkIntegrationResult{Runtime: runtimeName, Result: "would uninstall"})
			continue
		}
		_, _ = fmt.Fprintf(integrationsWriter(), "uninstalling %s...\n", runtimeName)
		code := runBulkIntegrationCommand(func() int {
			return callUninstallOneIntegration([]string{runtimeName})
		})
		if code != 0 {
			results = append(results, bulkIntegrationResult{Runtime: runtimeName, Result: "failed", Detail: fmt.Sprintf("exit code %d", code)})
			if code == 2 {
				exitCode = 2
			} else if exitCode == 0 {
				exitCode = 1
			}
			continue
		}
		results = append(results, bulkIntegrationResult{Runtime: runtimeName, Result: "uninstalled"})
	}
	writeBulkIntegrationResults(integrationsWriter(), results, *dryRun)
	if installed == 0 && exitCode == 0 {
		_, _ = fmt.Fprintln(integrationsWriter(), "No Witself runtime integrations are installed.")
		return 0
	}
	return exitCode
}

func runBulkIntegrationCommand(command func() int) int {
	previous := suppressIntegrationSuccessOutput
	suppressIntegrationSuccessOutput = true
	defer func() { suppressIntegrationSuccessOutput = previous }()
	return command()
}

func writeBulkIntegrationResults(w io.Writer, results []bulkIntegrationResult, dryRun bool) {
	_, _ = fmt.Fprintln(w)
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "RUNTIME\tRESULT\tDETAIL")
	succeeded := 0
	failed := 0
	skipped := 0
	for _, result := range results {
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\n", result.Runtime, result.Result, valueOrDash(result.Detail))
		switch result.Result {
		case "installed", "refreshed", "rebound", "updated", "uninstalled", "would install", "would refresh", "would rebind", "would refresh/rebind", "would uninstall":
			succeeded++
		case "failed", "blocked":
			failed++
		default:
			skipped++
		}
	}
	_ = table.Flush()
	label := "planned"
	if !dryRun {
		label = "succeeded"
	}
	_, _ = fmt.Fprintf(w, "\n%d %s, %d failed, %d skipped\n", succeeded, label, failed, skipped)
	if !dryRun && succeeded > 0 {
		_, _ = fmt.Fprintln(w, "Next: restart updated AI runtimes or start new tasks so they load the refreshed Witself integration.")
	}
}
