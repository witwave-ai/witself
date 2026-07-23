package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	runtimepkg "runtime"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const integrationsSchemaVersion = "witself.integrations.v2"

const bulkIntegrationSchemaVersion = "witself.integration-operation.v1"

const (
	integrationDetectionAvailable = "available"
	integrationDetectionNotFound  = "not_found"
	integrationDetectionError     = "error"
	integrationDetectionSkipped   = "skipped"

	integrationStateInstalled    = "installed"
	integrationStateNotInstalled = "not_installed"
	integrationStateError        = "error"
)

const (
	integrationVerificationHealthy      = "healthy"
	integrationVerificationDrifted      = "drifted"
	integrationVerificationIncomplete   = "incomplete"
	integrationVerificationUnavailable  = "unavailable"
	integrationVerificationUnsupported  = "unsupported"
	integrationVerificationNotInstalled = "not_installed"
)

const (
	integrationPlatformNative      = "native"
	integrationPlatformWSLOnly     = "wsl_only"
	integrationPlatformUnsupported = "unsupported"
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

type integrationPlatformStatus struct {
	SupportedOnPlatform bool   `json:"supported_on_platform"`
	SupportLevel        string `json:"support_level"`
	Reason              string `json:"reason,omitempty"`
}

type integrationVerificationStatus struct {
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

type integrationTopologyClassError struct {
	class string
	err   error
}

func (e *integrationTopologyClassError) Error() string {
	return e.err.Error()
}

func (e *integrationTopologyClassError) Unwrap() error {
	return e.err
}

func incompleteIntegrationTopology(err error) error {
	return &integrationTopologyClassError{class: integrationVerificationIncomplete, err: err}
}

func unavailableIntegrationTopology(err error) error {
	return &integrationTopologyClassError{class: integrationVerificationUnavailable, err: err}
}

type integrationRuntimeStatus struct {
	Runtime      string                         `json:"runtime"`
	DisplayName  string                         `json:"display_name"`
	Detection    integrationDetection           `json:"detection"`
	Integration  integrationBindingStatus       `json:"integration"`
	Capabilities integrationCapabilities        `json:"capabilities"`
	Platform     integrationPlatformStatus      `json:"platform"`
	Verification *integrationVerificationStatus `json:"verification,omitempty"`
}

type integrationsSummary struct {
	Supported int `json:"supported"`
	Detected  int `json:"detected"`
	Installed int `json:"installed"`
	Attention int `json:"attention"`
}

type integrationsVerificationSummary struct {
	Healthy      int `json:"healthy"`
	Drifted      int `json:"drifted"`
	Incomplete   int `json:"incomplete"`
	Unavailable  int `json:"unavailable"`
	Unsupported  int `json:"unsupported"`
	NotInstalled int `json:"not_installed"`
}

type integrationsReport struct {
	SchemaVersion       string                           `json:"schema_version"`
	Runtimes            []integrationRuntimeStatus       `json:"runtimes"`
	Summary             integrationsSummary              `json:"summary"`
	VerificationSummary *integrationsVerificationSummary `json:"verification_summary,omitempty"`
}

type bulkIntegrationResult struct {
	Runtime string `json:"runtime"`
	Result  string `json:"result"`
	Detail  string `json:"detail,omitempty"`
}

type bulkIntegrationSummary struct {
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

type bulkIntegrationReport struct {
	SchemaVersion string                  `json:"schema_version"`
	Operation     string                  `json:"operation"`
	DryRun        bool                    `json:"dry_run"`
	Results       []bulkIntegrationResult `json:"results"`
	Summary       bulkIntegrationSummary  `json:"summary"`
}

var (
	probeRuntimeForIntegrationCatalog = probeIntegrationRuntime
	loadIntegrationForCatalog         = transcriptcapture.LoadConfig
	validateIntegrationForCatalog     = validateInstalledIntegrationTopology
	statIntegrationCLIForCatalog      = os.Stat
	pendingTransactionForCatalog      = pendingIntegrationTransaction
	installOneIntegration             func([]string) int
	uninstallOneIntegration           func([]string) int
	suppressIntegrationSuccessOutput  bool
	integrationsOutput                io.Writer
	integrationCatalogGOOS            = runtimepkg.GOOS
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
	configureCommandUsage(fs, "usage: witself integrations [--verify] [--json]")
	jsonOutput := fs.Bool("json", false, "emit a stable machine-readable inventory")
	verify := fs.Bool("verify", false, "verify each installed runtime against its persisted exact binding")
	if commandHelpRequested(args) {
		args = []string{"--help"}
	}
	if parsed, code := parseCommandFlags(fs, args); !parsed {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself integrations [--verify] [--json]")
		return 2
	}
	report := collectIntegrationsReportWithVerification(*verify)
	if *jsonOutput {
		encoder := json.NewEncoder(integrationsWriter())
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			fmt.Fprintf(os.Stderr, "witself: encode integrations inventory: %v\n", err)
			return 1
		}
		if *verify && integrationsVerificationFailed(report) {
			return 1
		}
		return 0
	}
	writeIntegrationsTable(integrationsWriter(), report)
	if *verify && integrationsVerificationFailed(report) {
		return 1
	}
	return 0
}

func collectIntegrationsReport() integrationsReport {
	return collectIntegrationsReportWithVerification(false)
}

func collectIntegrationsReportWithVerification(verify bool) integrationsReport {
	runtimes := transcriptcapture.SupportedRuntimes()
	statuses := make([]integrationRuntimeStatus, len(runtimes))
	var wait sync.WaitGroup
	for index, runtimeName := range runtimes {
		index, runtimeName := index, runtimeName
		wait.Add(1)
		go func() {
			defer wait.Done()
			statuses[index] = inspectIntegrationRuntime(runtimeName, verify)
		}()
	}
	wait.Wait()
	report := integrationsReport{
		SchemaVersion: integrationsSchemaVersion,
		Runtimes:      statuses,
		Summary:       integrationsSummary{},
	}
	if verify {
		report.VerificationSummary = &integrationsVerificationSummary{}
	}
	for _, status := range statuses {
		if status.Platform.SupportedOnPlatform {
			report.Summary.Supported++
		}
		if status.Detection.State == integrationDetectionAvailable {
			report.Summary.Detected++
		}
		if status.Integration.State == integrationStateInstalled {
			report.Summary.Installed++
		}
		if status.Detection.State == integrationDetectionError || status.Integration.State == integrationStateError ||
			(status.Integration.State == integrationStateInstalled && !status.Platform.SupportedOnPlatform) ||
			verificationNeedsAttention(status.Verification) {
			report.Summary.Attention++
		}
		if report.VerificationSummary != nil && status.Verification != nil {
			switch status.Verification.State {
			case integrationVerificationHealthy:
				report.VerificationSummary.Healthy++
			case integrationVerificationDrifted:
				report.VerificationSummary.Drifted++
			case integrationVerificationIncomplete:
				report.VerificationSummary.Incomplete++
			case integrationVerificationUnavailable:
				report.VerificationSummary.Unavailable++
			case integrationVerificationUnsupported:
				report.VerificationSummary.Unsupported++
			case integrationVerificationNotInstalled:
				report.VerificationSummary.NotInstalled++
			}
		}
	}
	return report
}

func inspectIntegrationRuntime(runtimeName string, verify bool) integrationRuntimeStatus {
	platform := integrationPlatformSupport(runtimeName, integrationCatalogGOOS)
	status := integrationRuntimeStatus{
		Runtime:     runtimeName,
		DisplayName: integrationDisplayName(runtimeName),
		Platform:    platform,
		Integration: integrationBindingStatus{State: integrationStateNotInstalled},
		Capabilities: integrationCapabilities{
			MCP:                true,
			ManagedRouting:     true,
			TranscriptHooks:    supportsTranscriptHooksForPlatform(runtimeName, integrationCatalogGOOS),
			AdministratorHooks: supportsManagedHooksForPlatform(runtimeName, integrationCatalogGOOS),
		},
	}
	if platform.SupportedOnPlatform {
		status.Detection = probeRuntimeForIntegrationCatalog(runtimeName)
	} else {
		status.Detection = integrationDetection{State: integrationDetectionSkipped, Message: platform.Reason}
	}
	cfg, err := loadIntegrationForCatalog(runtimeName)
	if verify && platform.SupportedOnPlatform {
		var persisted *transcriptcapture.Config
		if err == nil {
			persisted = &cfg
		}
		pendingOperation, pending, pendingErr := pendingTransactionForCatalog(runtimeName, persisted)
		if pendingErr != nil {
			status.Integration = integrationBindingStatus{State: integrationStateError, Message: pendingErr.Error()}
			status.Verification = &integrationVerificationStatus{
				State:   integrationVerificationIncomplete,
				Message: "inspect interrupted integration transaction: " + pendingErr.Error(),
			}
			return status
		}
		if pending {
			command := fmt.Sprintf("witself %s %s", pendingOperation, runtimeName)
			message := fmt.Sprintf("interrupted %s transaction is pending; rerun `%s` to recover it", pendingOperation, command)
			if pendingOperation == genericProviderTransactionInstall {
				message = fmt.Sprintf(
					"interrupted install transaction is pending; rerun `%s` with the same identity and connection selectors to recover it",
					command,
				)
			}
			status.Integration = integrationBindingStatus{State: integrationStateError, Message: message}
			status.Verification = &integrationVerificationStatus{State: integrationVerificationIncomplete, Message: message}
			return status
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		if verify {
			verification := integrationVerificationStatus{State: integrationVerificationNotInstalled}
			if !platform.SupportedOnPlatform {
				verification = integrationVerificationStatus{
					State:   integrationVerificationUnsupported,
					Message: platform.Reason,
				}
			}
			status.Verification = &verification
		}
		return status
	}
	if err != nil {
		status.Integration = integrationBindingStatus{State: integrationStateError, Message: err.Error()}
		if verify {
			status.Verification = &integrationVerificationStatus{
				State:   integrationVerificationIncomplete,
				Message: "persisted integration record is unreadable or invalid: " + err.Error(),
			}
		}
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
	if verify {
		verification := verifyInstalledIntegration(runtimeName, platform, cfg)
		status.Verification = &verification
	}
	return status
}

func pendingIntegrationTransaction(runtimeName string, persisted *transcriptcapture.Config) (string, bool, error) {
	var operation string
	switch runtimeName {
	case transcriptcapture.RuntimeCodex,
		transcriptcapture.RuntimeClaudeCode,
		transcriptcapture.RuntimeGrokBuild,
		transcriptcapture.RuntimeCursor:
		journal, err := loadGenericProviderTransactionJournal(runtimeName)
		if err == nil {
			operation = journal.Operation
		} else if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		} else {
			return "", false, err
		}
	case transcriptcapture.RuntimeOpenClaw:
		configRoot := ""
		var err error
		if persisted != nil {
			configRoot, err = openClawTransactionRootFromConfig(*persisted)
		} else {
			var environment map[string]string
			environment, err = captureOpenClawMCPEnvironment()
			if err == nil {
				configRoot, err = openClawTransactionRootFromEnvironment(environment)
			}
		}
		if err != nil {
			return "", false, err
		}
		journal, err := loadOpenClawTransactionJournal(configRoot)
		if err == nil {
			operation = journal.Operation
		} else if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		} else {
			return "", false, err
		}
	case transcriptcapture.RuntimeAntigravity:
		configRoot := ""
		var err error
		if persisted != nil {
			configRoot = persisted.RuntimeConfigRoot
			if configRoot == "" {
				err = errors.New("persisted Antigravity integration does not pin its config root")
			}
		} else {
			configRoot, err = currentAntigravityConfigRoot()
		}
		if err != nil {
			return "", false, err
		}
		journal, err := loadAntigravityTransactionJournal(configRoot)
		if err == nil {
			operation = journal.Operation
		} else if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		} else {
			return "", false, err
		}
	case transcriptcapture.RuntimeCopilot:
		configRoot := ""
		var err error
		if persisted != nil {
			configRoot = persisted.RuntimeConfigRoot
			if configRoot == "" {
				err = errors.New("persisted GitHub Copilot integration does not pin its config root")
			}
		} else {
			configRoot, err = currentCopilotConfigRoot()
		}
		if err != nil {
			return "", false, err
		}
		journal, err := loadCopilotTransactionJournal(configRoot)
		if err == nil {
			operation = journal.Operation
		} else if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		} else {
			return "", false, err
		}
	default:
		return "", false, fmt.Errorf("unsupported integration runtime %q", runtimeName)
	}
	return operation, true, nil
}

func verifyInstalledIntegration(
	runtimeName string,
	platform integrationPlatformStatus,
	cfg transcriptcapture.Config,
) integrationVerificationStatus {
	if !platform.SupportedOnPlatform {
		return integrationVerificationStatus{
			State:   integrationVerificationUnsupported,
			Message: platform.Reason,
		}
	}
	if issue := incompletePersistedIntegrationBinding(runtimeName, cfg); issue != "" {
		return integrationVerificationStatus{
			State:   integrationVerificationIncomplete,
			Message: issue,
		}
	}
	for _, command := range []struct {
		label string
		path  string
	}{
		{label: "runtime CLI", path: cfg.RuntimeCLICommand},
		{label: "Witself MCP command", path: cfg.MCPCommand},
	} {
		info, err := statIntegrationCLIForCatalog(command.path)
		if err != nil {
			return integrationVerificationStatus{
				State:   integrationVerificationUnavailable,
				Message: fmt.Sprintf("persisted %s %s is unavailable: %v", command.label, command.path, err),
			}
		}
		if !info.Mode().IsRegular() {
			return integrationVerificationStatus{
				State:   integrationVerificationUnavailable,
				Message: fmt.Sprintf("persisted %s %s is not a regular file", command.label, command.path),
			}
		}
		if integrationCatalogGOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
			return integrationVerificationStatus{
				State:   integrationVerificationUnavailable,
				Message: fmt.Sprintf("persisted %s %s is not executable", command.label, command.path),
			}
		}
	}
	if err := validateIntegrationForCatalog(runtimeName, cfg); err != nil {
		var classified *integrationTopologyClassError
		if errors.As(err, &classified) {
			return integrationVerificationStatus{
				State:   classified.class,
				Message: classified.Error(),
			}
		}
		if errors.Is(err, os.ErrPermission) {
			return integrationVerificationStatus{
				State:   integrationVerificationUnavailable,
				Message: err.Error(),
			}
		}
		if errors.Is(err, os.ErrNotExist) {
			return integrationVerificationStatus{
				State:   integrationVerificationIncomplete,
				Message: err.Error(),
			}
		}
		return integrationVerificationStatus{
			State:   integrationVerificationDrifted,
			Message: err.Error(),
		}
	}
	return integrationVerificationStatus{State: integrationVerificationHealthy}
}

func incompletePersistedIntegrationBinding(runtimeName string, cfg transcriptcapture.Config) string {
	if strings.TrimSpace(cfg.RuntimeCLICommand) == "" {
		return "persisted integration record does not pin the runtime CLI; reinstall this integration"
	}
	if strings.TrimSpace(cfg.MCPCommand) == "" {
		return "persisted integration record does not pin the Witself MCP command; reinstall this integration"
	}
	if isGenericProviderRuntime(runtimeName) &&
		(strings.TrimSpace(cfg.MCPCommand) == "" ||
			strings.TrimSpace(cfg.RuntimeConfigRoot) == "" ||
			strings.TrimSpace(cfg.RuntimeMCPConfigPath) == "" ||
			strings.TrimSpace(cfg.MCPEnvironment["WITSELF_HOME"]) == "") {
		return "persisted integration record predates exact provider binding ownership; reinstall this integration"
	}
	if supportsTranscriptHooks(runtimeName) {
		switch cfg.HookMode {
		case transcriptcapture.HookModeUser:
			if strings.TrimSpace(cfg.HookConfigPath) == "" {
				return "persisted integration record does not pin the exact user hook config path; reinstall this integration"
			}
		case transcriptcapture.HookModeManaged:
			if strings.TrimSpace(cfg.HookConfigPath) == "" ||
				strings.TrimSpace(cfg.HookManagedDir) == "" ||
				strings.TrimSpace(cfg.HookRunnerPath) == "" ||
				strings.TrimSpace(cfg.HookRunnerDigest) == "" ||
				strings.TrimSpace(cfg.HookPolicyDigest) == "" {
				return "persisted integration record does not pin the exact managed hook policy and runner; reinstall this integration"
			}
		}
	}
	if runtimeName == transcriptcapture.RuntimeOpenClaw &&
		(strings.TrimSpace(cfg.MCPCommand) == "" ||
			strings.TrimSpace(cfg.MCPEnvironment["WITSELF_HOME"]) == "" ||
			strings.TrimSpace(cfg.MCPEnvironment["OPENCLAW_STATE_DIR"]) == "" ||
			strings.TrimSpace(cfg.MCPEnvironment["OPENCLAW_CONFIG_PATH"]) == "" ||
			strings.TrimSpace(cfg.RuntimeWorkspace) == "" ||
			strings.TrimSpace(cfg.RuntimeAgentID) == "") {
		return "persisted OpenClaw integration does not pin its exact CLI, namespace, agent, workspace, and MCP binding; reinstall this integration"
	}
	return ""
}

func validateInstalledIntegrationTopology(runtimeName string, cfg transcriptcapture.Config) error {
	var providerErr error
	switch runtimeName {
	case transcriptcapture.RuntimeCodex,
		transcriptcapture.RuntimeClaudeCode,
		transcriptcapture.RuntimeGrokBuild,
		transcriptcapture.RuntimeCursor:
		if err := validateGenericProviderCurrentRoots(cfg); err != nil {
			return err
		}
		providerErr = validateGenericInstalledTopology(cfg)
	case transcriptcapture.RuntimeOpenClaw:
		currentEnvironment, err := captureOpenClawMCPEnvironment()
		if err != nil {
			return err
		}
		if !equalOpenClawMCPEnvironment(currentEnvironment, cfg.MCPEnvironment) {
			return errors.New("OpenClaw selector environment differs from the persisted exact namespace")
		}
		providerErr = validateOpenClawInstalledIntegration(cfg)
	case transcriptcapture.RuntimeAntigravity:
		currentRoot, err := currentAntigravityConfigRoot()
		if err != nil {
			return err
		}
		if currentRoot != cfg.RuntimeConfigRoot {
			return fmt.Errorf("the Antigravity config root changed from installed %s to %s", cfg.RuntimeConfigRoot, currentRoot)
		}
		currentWitselfHome, err := local.Home()
		if err != nil {
			return err
		}
		currentWitselfHome, err = cleanAntigravityAbsolutePath("WITSELF_HOME", currentWitselfHome)
		if err != nil {
			return err
		}
		if currentWitselfHome != cfg.MCPEnvironment["WITSELF_HOME"] {
			return errors.New("WITSELF_HOME changed from the installed Antigravity binding")
		}
		providerErr = validateAntigravityInstalledTopology(cfg)
	case transcriptcapture.RuntimeCopilot:
		currentRoot, err := currentCopilotConfigRoot()
		if err != nil {
			return err
		}
		if currentRoot != cfg.RuntimeConfigRoot {
			return fmt.Errorf("COPILOT_HOME changed from installed %s to %s", cfg.RuntimeConfigRoot, currentRoot)
		}
		currentEnvironment, err := captureCopilotMCPEnvironment()
		if err != nil {
			return err
		}
		if !equalCopilotEnvironment(currentEnvironment, cfg.MCPEnvironment) {
			return errors.New("WITSELF_HOME changed from the installed GitHub Copilot binding")
		}
		providerErr = validateCopilotInstalledTopology(cfg)
	default:
		return fmt.Errorf("unsupported integration runtime %q", runtimeName)
	}
	if providerErr != nil {
		return providerErr
	}
	if supportsTranscriptHooks(runtimeName) {
		if err := verifyRuntimeHooksOwned(cfg); err != nil {
			return incompleteIntegrationTopology(fmt.Errorf("installed %s hooks are missing or stale: %w", integrationDisplayName(runtimeName), err))
		}
	}
	routingCurrent, err := runtimeMemoryRoutingCurrentAt(runtimeName, cfg.RuntimeWorkspace)
	if err != nil {
		return fmt.Errorf("verify installed %s static routing: %w", integrationDisplayName(runtimeName), err)
	}
	if !routingCurrent {
		return incompleteIntegrationTopology(fmt.Errorf("installed %s static routing is missing or stale", integrationDisplayName(runtimeName)))
	}
	return nil
}

func verificationNeedsAttention(verification *integrationVerificationStatus) bool {
	if verification == nil {
		return false
	}
	switch verification.State {
	case integrationVerificationDrifted,
		integrationVerificationIncomplete,
		integrationVerificationUnavailable:
		return true
	default:
		return false
	}
}

func integrationsVerificationFailed(report integrationsReport) bool {
	for _, status := range report.Runtimes {
		if verificationNeedsAttention(status.Verification) ||
			(status.Integration.State == integrationStateInstalled &&
				status.Verification != nil &&
				status.Verification.State == integrationVerificationUnsupported) {
			return true
		}
	}
	return false
}

func integrationPlatformSupport(runtimeName, platform string) integrationPlatformStatus {
	switch platform {
	case "darwin", "linux":
		return integrationPlatformStatus{SupportedOnPlatform: true, SupportLevel: integrationPlatformNative}
	case "windows":
		if runtimeName == transcriptcapture.RuntimeCursor {
			return integrationPlatformStatus{
				SupportedOnPlatform: false,
				SupportLevel:        integrationPlatformWSLOnly,
				Reason:              "Cursor Agent's published CLI contract supports Windows through WSL; install Witself and Cursor inside the same WSL distribution",
			}
		}
		return integrationPlatformStatus{SupportedOnPlatform: true, SupportLevel: integrationPlatformNative}
	default:
		return integrationPlatformStatus{
			SupportedOnPlatform: false,
			SupportLevel:        integrationPlatformUnsupported,
			Reason:              fmt.Sprintf("%s integration is not supported on %s", runtimeName, platform),
		}
	}
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
		if errors.Is(err, errRuntimeCLIIncompatible) || errors.Is(err, errRuntimeCLICapability) {
			return integrationDetection{State: integrationDetectionError, Message: err.Error()}
		}
		return integrationDetection{State: integrationDetectionNotFound, Message: err.Error()}
	}
	version := detectRuntimeVersionWithEnvironment(runtimeName, executable, environment)
	if runtimeName == transcriptcapture.RuntimeCopilot {
		if err := validateCopilotRuntimeVersion(version); err != nil {
			return integrationDetection{
				State:      integrationDetectionError,
				Executable: executable,
				Version:    version,
				Message:    err.Error(),
			}
		}
	}
	return integrationDetection{
		State:      integrationDetectionAvailable,
		Executable: executable,
		Version:    version,
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
	case transcriptcapture.RuntimeCopilot:
		return "GitHub Copilot"
	default:
		return runtimeName
	}
}

func writeIntegrationsTable(w io.Writer, report integrationsReport) {
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if report.VerificationSummary != nil {
		_, _ = fmt.Fprintln(table, "RUNTIME\tAPPLICATION\tPLATFORM\tCLI\tVERSION\tWITSELF\tHEALTH\tAGENT\tLOCATION\tHOOKS")
	} else {
		_, _ = fmt.Fprintln(table, "RUNTIME\tAPPLICATION\tPLATFORM\tCLI\tVERSION\tWITSELF\tAGENT\tLOCATION\tHOOKS")
	}
	for _, status := range report.Runtimes {
		cli := "error"
		switch status.Detection.State {
		case integrationDetectionAvailable:
			cli = "found"
		case integrationDetectionNotFound:
			cli = "not found"
		case integrationDetectionSkipped:
			cli = "not applicable"
		}
		if report.VerificationSummary != nil {
			health := "-"
			if status.Verification != nil {
				health = strings.ReplaceAll(status.Verification.State, "_", " ")
			}
			_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				status.Runtime,
				status.DisplayName,
				strings.ReplaceAll(status.Platform.SupportLevel, "_", " "),
				cli,
				valueOrDash(status.Detection.Version),
				strings.ReplaceAll(status.Integration.State, "_", " "),
				health,
				valueOrDash(status.Integration.Agent),
				valueOrDash(status.Integration.Location),
				valueOrDash(status.Integration.HookMode),
			)
		} else {
			_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				status.Runtime,
				status.DisplayName,
				strings.ReplaceAll(status.Platform.SupportLevel, "_", " "),
				cli,
				valueOrDash(status.Detection.Version),
				strings.ReplaceAll(status.Integration.State, "_", " "),
				valueOrDash(status.Integration.Agent),
				valueOrDash(status.Integration.Location),
				valueOrDash(status.Integration.HookMode),
			)
		}
	}
	_ = table.Flush()
	for _, status := range report.Runtimes {
		if status.Detection.State == integrationDetectionError {
			_, _ = fmt.Fprintf(w, "\n%s detection error: %s\n", status.Runtime, status.Detection.Message)
		}
		if status.Integration.State == integrationStateError {
			_, _ = fmt.Fprintf(w, "\n%s integration error: %s\n", status.Runtime, status.Integration.Message)
		}
		if status.Verification != nil && status.Verification.Message != "" &&
			status.Verification.State != integrationVerificationUnsupported {
			_, _ = fmt.Fprintf(w, "\n%s verification %s: %s\n",
				status.Runtime, status.Verification.State, status.Verification.Message)
		}
		if !status.Platform.SupportedOnPlatform {
			_, _ = fmt.Fprintf(w, "\n%s platform note: %s\n", status.Runtime, status.Platform.Reason)
		}
	}
	_, _ = fmt.Fprintf(w, "\n%d supported, %d detected, %d installed, %d need attention\n",
		report.Summary.Supported,
		report.Summary.Detected,
		report.Summary.Installed,
		report.Summary.Attention,
	)
	if summary := report.VerificationSummary; summary != nil {
		_, _ = fmt.Fprintf(w, "%d healthy, %d drifted, %d incomplete, %d unavailable, %d unsupported, %d not installed\n",
			summary.Healthy,
			summary.Drifted,
			summary.Incomplete,
			summary.Unavailable,
			summary.Unsupported,
			summary.NotInstalled,
		)
	}
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
	account, realm, agent, location, endpoint, tokenFile string,
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
		{name: "location", flag: location, env: os.Getenv("WITSELF_LOCATION"), current: status.Integration.Location},
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
	configureCommandUsage(fs, "usage: witself install all [--account NAME] [--realm NAME] [--agent NAME] [--location NAME] [--endpoint URL] [--token-file FILE] [--managed-hooks|--user-hooks] [--dry-run] [--json]")
	account := accountFlag(fs)
	realm := fs.String("realm", "", `local realm name (default: WITSELF_REALM or "default")`)
	agent := fs.String("agent", "", "local agent name")
	location := fs.String("location", strings.TrimSpace(os.Getenv("WITSELF_LOCATION")), "stable human label for this machine")
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing an agent token")
	dryRun := fs.Bool("dry-run", false, "show the plan without changing any runtime")
	jsonOutput := fs.Bool("json", false, "emit stable machine-readable results")
	managedHooks := fs.Bool("managed-hooks", false, "use administrator-managed hooks where supported")
	userHooks := fs.Bool("user-hooks", false, "use user-scoped hooks where supported")
	_ = fs.String("capture", transcriptcapture.ModeRaw, "not supported with install all")
	_ = fs.Bool("routing-only", false, "not supported with install all")
	if commandHelpRequested(args) {
		args = []string{"--help"}
	}
	if parsed, code := parseCommandFlags(fs, args); !parsed {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself install all [--account NAME] [--realm NAME] [--agent NAME] [--location NAME] [--endpoint URL] [--token-file FILE] [--managed-hooks|--user-hooks] [--dry-run] [--json]")
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
		if !status.Platform.SupportedOnPlatform {
			results = append(results, bulkIntegrationResult{Runtime: status.Runtime, Result: "unsupported", Detail: status.Platform.Reason})
			continue
		}
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
		action, detail := bulkInstallPreview(status, setFlags, *account, *realm, *agent, *location, *endpoint, *tokenFile)
		hookArgs, hookDetail := bulkInstallHookArgs(status, setFlags, *managedHooks, *userHooks)
		detail = joinBulkDetails(detail, hookDetail)
		if action == "blocked" {
			results = append(results, bulkIntegrationResult{Runtime: status.Runtime, Result: action, Detail: detail})
			exitCode = 1
			continue
		}
		if *dryRun {
			results = append(results, bulkIntegrationResult{Runtime: status.Runtime, Result: action, Detail: detail})
			continue
		}
		if !*jsonOutput {
			_, _ = fmt.Fprintf(integrationsWriter(), "installing %s...\n", status.Runtime)
		}
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
	if err := writeBulkIntegrationOutput(integrationsWriter(), "install", results, *dryRun, *jsonOutput); err != nil {
		fmt.Fprintf(os.Stderr, "witself: encode bulk installation results: %v\n", err)
		return 1
	}
	if attempted == 0 {
		fmt.Fprintln(os.Stderr, "witself: no supported AI runtime with MCP capability was detected")
		return 1
	}
	return exitCode
}

func uninstallAllCmd(args []string) int {
	fs := flag.NewFlagSet("uninstall all", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configureCommandUsage(fs, "usage: witself uninstall all [--dry-run] [--json]")
	dryRun := fs.Bool("dry-run", false, "show the plan without changing any runtime")
	jsonOutput := fs.Bool("json", false, "emit stable machine-readable results")
	_ = fs.Bool("managed-hooks", false, "not supported with uninstall all")
	if commandHelpRequested(args) {
		args = []string{"--help"}
	}
	if parsed, code := parseCommandFlags(fs, args); !parsed {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself uninstall all [--dry-run] [--json]")
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
			pendingOperation, pending, pendingErr := pendingTransactionForCatalog(runtimeName, nil)
			if pendingErr != nil {
				results = append(results, bulkIntegrationResult{
					Runtime: runtimeName,
					Result:  "failed",
					Detail:  "inspect interrupted integration transaction: " + pendingErr.Error(),
				})
				if exitCode == 0 {
					exitCode = 1
				}
				continue
			}
			if !pending || pendingOperation != genericProviderTransactionUninstall {
				results = append(results, bulkIntegrationResult{Runtime: runtimeName, Result: "not installed"})
				continue
			}
			err = nil
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
		if !*jsonOutput {
			_, _ = fmt.Fprintf(integrationsWriter(), "uninstalling %s...\n", runtimeName)
		}
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
	if err := writeBulkIntegrationOutput(integrationsWriter(), "uninstall", results, *dryRun, *jsonOutput); err != nil {
		fmt.Fprintf(os.Stderr, "witself: encode bulk uninstallation results: %v\n", err)
		return 1
	}
	if installed == 0 && exitCode == 0 {
		if !*jsonOutput {
			_, _ = fmt.Fprintln(integrationsWriter(), "No Witself runtime integrations are installed.")
		}
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

func writeBulkIntegrationOutput(w io.Writer, operation string, results []bulkIntegrationResult, dryRun, jsonOutput bool) error {
	if !jsonOutput {
		writeBulkIntegrationResults(w, results, dryRun)
		return nil
	}
	report := bulkIntegrationReport{
		SchemaVersion: bulkIntegrationSchemaVersion,
		Operation:     operation,
		DryRun:        dryRun,
		Results:       results,
		Summary:       summarizeBulkIntegrationResults(results),
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func summarizeBulkIntegrationResults(results []bulkIntegrationResult) bulkIntegrationSummary {
	var summary bulkIntegrationSummary
	for _, result := range results {
		switch result.Result {
		case "installed", "refreshed", "rebound", "updated", "uninstalled", "would install", "would refresh", "would rebind", "would refresh/rebind", "would uninstall":
			summary.Succeeded++
		case "failed", "blocked":
			summary.Failed++
		default:
			summary.Skipped++
		}
	}
	return summary
}
