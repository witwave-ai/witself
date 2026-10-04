package main

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/witwave-ai/witself/infra/pulumi/internal/backend"
	"github.com/witwave-ai/witself/infra/pulumi/internal/cell"
)

var (
	r2CheckState = backend.CheckR2State
	r2PulumiCLI  = func() (command auto.PulumiCommand, major, minor, patch uint64, err error) {
		command, err = auto.NewPulumiCommand(nil)
		if err != nil {
			return
		}
		version := command.Version()
		return command, version.Major, version.Minor, version.Patch, nil
	}
)

func r2PulumiVersion() (auto.PulumiCommand, uint64, uint64, uint64, error) {
	command, major, minor, patch, err := r2PulumiCLI()
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("state backend r2: cannot run the Pulumi CLI: %w", err)
	}
	return command, major, minor, patch, nil
}

func checkR2PulumiVersion(major, minor, patch uint64) error {
	if major < 3 || (major == 3 && minor < 263) {
		return fmt.Errorf("state backend r2 needs Pulumi CLI 3.263.0 or newer on PATH; found %d.%d.%d", major, minor, patch)
	}
	return nil
}

func resolveR2Backend(backendKind, cloud, bucket, endpoint, stateDir string, getenv func(string) string) (backend.R2Settings, backend.R2Secrets, error) {
	var secrets backend.R2Secrets
	s := backend.R2Settings{}
	if backendKind != "r2" {
		if bucket != "" || endpoint != "" {
			return s, secrets, fmt.Errorf("-r2-bucket and -r2-endpoint apply only to -backend r2")
		}
		return s, secrets, nil
	}
	if cloud != "civo" {
		return s, secrets, fmt.Errorf("-backend r2 is only implemented for -cloud civo")
	}
	s = backend.R2Settings{Bucket: bucket, Endpoint: endpoint}
	if err := backend.ValidateR2Settings(s); err != nil {
		return s, secrets, err
	}
	if stateDir != defaultStateDir() {
		return s, secrets, fmt.Errorf("-state-dir does not apply to -backend r2 (inventory key state_dir)")
	}
	secrets, err := backend.R2SecretsFromEnv(getenv)
	if err != nil {
		return s, secrets, err
	}
	return s, secrets, backend.R2SettingsContainSecret(s, getenv)
}

func prepareR2Workspace(ctx context.Context, cmd string, s backend.R2Settings, secrets backend.R2Secrets, cellName string, errOut io.Writer) (map[string]string, auto.PulumiCommand, error) {
	command, major, minor, patch, err := r2PulumiVersion()
	if err != nil {
		return nil, nil, err
	}
	if err := checkR2PulumiVersion(major, minor, patch); err != nil {
		return nil, nil, err
	}
	switch cmd {
	case "up", "preview", "refresh", "destroy":
		if _, err := r2CheckState(ctx, s, secrets, backend.R2CheckOptions{Project: projectName, Stack: cellName}); err != nil {
			return nil, nil, err
		}
		_, _ = fmt.Fprintf(errOut, "state backend r2: bucket %s verified (list, write, read, delete)\n", s.Bucket)
	}
	return backend.R2WorkspaceEnv(s, secrets), command, nil
}

func openCellStack(ctx context.Context, backendKind, cmd, cellName, bucket string, opts ...auto.LocalWorkspaceOption) (auto.Stack, error) {
	var stack auto.Stack
	var err error
	if backendKind == "r2" && cmd != "up" && cmd != "preview" {
		stack, err = auto.SelectStackInlineSource(ctx, cellName, projectName, cell.Program, opts...)
		if auto.IsSelectStack404Error(err) {
			return stack, fmt.Errorf("state backend r2: cell %q has no stack in bucket %q; %s never creates one. Check r2_bucket and r2_endpoint, or run preview first for a new cell", cellName, bucket, cmd)
		}
	} else {
		stack, err = auto.UpsertStackInlineSource(ctx, cellName, projectName, cell.Program, opts...)
	}
	if err != nil {
		return stack, fmt.Errorf("create/select cell %q: %w", cellName, err)
	}
	return stack, nil
}

func runR2Bootstrap(ctx context.Context, out io.Writer, s backend.R2Settings, secrets backend.R2Secrets, cellName string) error {
	if _, err := r2CheckState(ctx, s, secrets, backend.R2CheckOptions{Project: projectName, Stack: cellName}); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Civo R2 state backend ready: bucket %s at %s (the owner creates the bucket; witself-infra wrote and deleted one probe object)\n", s.Bucket, s.Endpoint)
	return nil
}

func runStateCheck(ctx context.Context, out io.Writer, s backend.R2Settings, secrets backend.R2Secrets, cellName string) error {
	_, major, minor, patch, err := r2PulumiVersion()
	if err != nil {
		return err
	}
	line := func(key, value string) { _, _ = fmt.Fprintf(out, "%-19s%s\n", key, value) }
	line("cell:", cellName)
	line("backend:", "r2")
	line("bucket:", s.Bucket)
	line("endpoint:", s.Endpoint)
	line("variables:", "3 of 3 present")
	line("pulumi cli:", fmt.Sprintf("%d.%d.%d", major, minor, patch))
	if err := checkR2PulumiVersion(major, minor, patch); err != nil {
		return err
	}
	report, err := r2CheckState(ctx, s, secrets, backend.R2CheckOptions{Project: projectName, Stack: cellName, Inventory: true})
	if err != nil {
		return err
	}
	for _, key := range []string{"list:", "write probe:", "read probe:", "list after write:", "delete probe:", "list after delete:"} {
		line(key, "ok")
	}
	present := "absent"
	if report.StackFile {
		present = "present"
	}
	line("stack file:", present)
	locks := report.StackLocks
	if locks != "0" {
		locks += " (an operation is running, or a crashed run left a lock; see the README)"
	}
	line("stack locks:", locks)
	line("history objects:", report.HistoryObjects)
	line("backup objects:", report.BackupObjects)
	line("probe leftovers:", report.ProbeLeftovers)
	_, _ = fmt.Fprintln(out, "ok")
	return nil
}

// cloudflareSecretEnv lists the Cloudflare credentials that the Cloudflare
// provider reads from the environment of the Pulumi process.
var cloudflareSecretEnv = []string{"CLOUDFLARE_API_TOKEN", "CLOUDFLARE_API_KEY", "CLOUDFLARE_API_USER_SERVICE_KEY"}

// cloudflareTokenShape matches the prefixed Cloudflare credential formats:
// user API tokens, account API tokens and global API keys.
var cloudflareTokenShape = regexp.MustCompile(`cf(?:ut|at|k)_[A-Za-z0-9_-]{8,}`)

// resolvedCivoTokens records each Civo API token that resolveCivoToken has
// returned in this process, from a token file or from CIVO_TOKEN, so that
// redactDiagnostic can replace a token that is not in the environment.
var resolvedCivoTokens struct {
	sync.Mutex
	values []string
}

// resolvedLocalPassphrases records successfully resolved local-state
// passphrases in memory so diagnostics can redact values read from disk.
var resolvedLocalPassphrases struct {
	sync.Mutex
	values []string
}

func rememberLocalPassphrase(value string) {
	if value == "" {
		return
	}
	resolvedLocalPassphrases.Lock()
	defer resolvedLocalPassphrases.Unlock()
	if !slices.Contains(resolvedLocalPassphrases.values, value) {
		resolvedLocalPassphrases.values = append(resolvedLocalPassphrases.values, value)
	}
}

func localPassphraseValues(getenv func(string) string) []string {
	value := getenv("PULUMI_CONFIG_PASSPHRASE")
	resolvedLocalPassphrases.Lock()
	defer resolvedLocalPassphrases.Unlock()
	return append([]string{value, strings.TrimSpace(value)}, resolvedLocalPassphrases.values...)
}

// rememberCivoToken adds token to resolvedCivoTokens unless it is already there.
func rememberCivoToken(token string) {
	resolvedCivoTokens.Lock()
	defer resolvedCivoTokens.Unlock()
	if slices.Contains(resolvedCivoTokens.values, token) {
		return
	}
	resolvedCivoTokens.values = append(resolvedCivoTokens.values, token)
}

// civoTokenValues returns CIVO_TOKEN as exported, its trimmed form and every
// token in resolvedCivoTokens.
func civoTokenValues(getenv func(string) string) []string {
	value := getenv("CIVO_TOKEN")
	resolvedCivoTokens.Lock()
	defer resolvedCivoTokens.Unlock()
	return append([]string{value, strings.TrimSpace(value)}, resolvedCivoTokens.values...)
}

// redactDiagnostic replaces credential values in diagnostic text: the three
// R2 values, each Cloudflare credential of cloudflareSecretEnv as exported and
// without surrounding whitespace, the Civo API token of civoTokenValues,
// PULUMI_CONFIG_PASSPHRASE as exported and trimmed plus recorded local-state
// passphrases, and any text shaped like a prefixed Cloudflare credential.
// A value shorter than 8 characters is never replaced.
func redactDiagnostic(text string, getenv func(string) string) string {
	text = backend.RedactR2Secrets(text, getenv)
	for _, name := range cloudflareSecretEnv {
		value := getenv(name)
		for _, v := range []string{value, strings.TrimSpace(value)} {
			if len(v) >= 8 {
				text = strings.ReplaceAll(text, v, "[redacted "+name+"]")
			}
		}
	}
	for _, v := range civoTokenValues(getenv) {
		if len(v) >= 8 {
			text = strings.ReplaceAll(text, v, "[redacted CIVO_TOKEN]")
		}
	}
	for _, v := range localPassphraseValues(getenv) {
		if len(v) >= 8 {
			text = strings.ReplaceAll(text, v, "[redacted PULUMI_CONFIG_PASSPHRASE]")
		}
	}
	return cloudflareTokenShape.ReplaceAllString(text, "[redacted Cloudflare token]")
}

func fatalMessage(err error, getenv func(string) string) string {
	return "witself-infra: " + redactDiagnostic(err.Error(), getenv)
}
