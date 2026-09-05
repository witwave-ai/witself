package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
	"github.com/witwave-ai/witself/internal/version"
)

const (
	transcriptReleaseUploaderCapabilityFlag = "--uploader-capability"
	transcriptReleaseUploaderCapability     = "witself.transcript-release.uploader.v1"
	transcriptReleaseUploaderProbeTimeout   = 5 * time.Second
)

// verifyTranscriptReleaseUploaders checks every installed integration in this
// Witself home: another runtime's pinned binary can also flush this outbox.
// Ambiguous legacy bindings are never reconstructed here. No hook or integration
// file is changed here.
func verifyTranscriptReleaseUploaders() error {
	verified := make(map[string]bool)
	for _, runtimeName := range transcriptcapture.SupportedRuntimes() {
		cfg, err := transcriptcapture.LoadConfig(runtimeName)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return transcriptReleaseUploaderError(runtimeName, err)
		}
		if err := verifyTranscriptReleaseHookExecutable(cfg); err != nil {
			return transcriptReleaseUploaderError(runtimeName, err)
		}
		if verified[cfg.MCPCommand] {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), transcriptReleaseUploaderProbeTimeout)
		err = probeTranscriptReleaseUploader(ctx, cfg.MCPCommand)
		cancel()
		if err != nil {
			return transcriptReleaseUploaderError(runtimeName, err)
		}
		verified[cfg.MCPCommand] = true
	}
	return nil
}

type transcriptReleaseUploaderVerificationError struct {
	Runtime string
	Err     error
}

func (err *transcriptReleaseUploaderVerificationError) Error() string {
	return fmt.Sprintf("%s installed uploader is not verified for operator release: %v; run witself install --runtime %s (re-registers hooks with the current binary), then retry", err.Runtime, err.Err, err.Runtime)
}

func (err *transcriptReleaseUploaderVerificationError) Unwrap() error { return err.Err }

func transcriptReleaseUploaderError(runtimeName string, err error) error {
	return &transcriptReleaseUploaderVerificationError{Runtime: runtimeName, Err: err}
}

func verifyTranscriptReleaseHookExecutable(cfg transcriptcapture.Config) error {
	if cfg.MCPCommand == "" {
		return errors.New("installed hook executable is missing")
	}
	if cfg.HookMode != transcriptcapture.HookModeNone && (cfg.HookConfigPath == "" || cfg.HookRunnerPath == "") {
		return errors.New("installed hook registration is incomplete: mcp_command, hook_config_path, and hook_runner_path are required")
	}
	switch cfg.HookMode {
	case transcriptcapture.HookModeNone:
		return nil
	case transcriptcapture.HookModeUser:
		opts, err := userHooksOptionsFromConfig(cfg, cfg.MCPCommand)
		if err != nil {
			return err
		}
		return transcriptcapture.VerifyOwnedHooks(opts)
	case transcriptcapture.HookModeManaged:
		ownership, ok, err := managedHookOwnershipFromConfig(cfg)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("managed hook ownership is missing")
		}
		return transcriptcapture.VerifyManagedHookExecutable(cfg.Runtime, ownership, cfg.MCPCommand)
	default:
		return fmt.Errorf("unsupported transcript hook mode %q", cfg.HookMode)
	}
}

func probeTranscriptReleaseUploader(ctx context.Context, executable string) error {
	if !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		return errors.New("uploader executable must be a clean absolute path")
	}
	info, err := os.Stat(executable)
	if err != nil {
		return fmt.Errorf("inspect uploader executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("uploader executable must be a regular file")
	}
	// Old binaries perform startup maintenance even for an unknown command.
	// Give the child empty application, user, and service-config homes so that
	// maintenance cannot discover live state or retired service definitions.
	probeHome, err := os.MkdirTemp("", "witself-release-uploader-")
	if err != nil {
		return fmt.Errorf("create isolated uploader probe home: %w", err)
	}
	defer os.RemoveAll(probeHome)
	env := append([]string{}, os.Environ()...)
	for i := len(env) - 1; i >= 0; i-- {
		key, _, _ := strings.Cut(env[i], "=")
		switch strings.ToUpper(key) {
		case "WITSELF_HOME", "HOME", "USERPROFILE", "XDG_CONFIG_HOME":
			env = append(env[:i], env[i+1:]...)
		}
	}
	env = append(env, "WITSELF_HOME="+probeHome, "HOME="+probeHome, "USERPROFILE="+probeHome, "XDG_CONFIG_HOME="+probeHome)
	versionOutput, err := runTranscriptReleaseUploaderProbe(ctx, executable, probeHome, env, "--version")
	if err != nil {
		return fmt.Errorf("uploader version probe failed: %w", err)
	}
	if strings.TrimSpace(versionOutput) != version.String("witself") {
		return errors.New("uploader --version does not match this release")
	}
	output, err := runTranscriptReleaseUploaderProbe(ctx, executable, probeHome, env, "transcript", "release", transcriptReleaseUploaderCapabilityFlag)
	if err != nil {
		return fmt.Errorf("uploader capability probe failed: %w", err)
	}
	if strings.TrimSpace(output) != transcriptReleaseUploaderCapability {
		return errors.New("uploader does not advertise operator-release support")
	}
	return nil
}

func runTranscriptReleaseUploaderProbe(ctx context.Context, executable, probeHome string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Dir = probeHome
	cmd.Env = env
	var output transcriptReleaseCapabilityOutput
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return output.String(), nil
}

type transcriptReleaseCapabilityOutput struct{ buffer bytes.Buffer }

func (output *transcriptReleaseCapabilityOutput) String() string { return output.buffer.String() }

func (output *transcriptReleaseCapabilityOutput) Write(raw []byte) (int, error) {
	if len(raw) > 256-output.buffer.Len() {
		return 0, errors.New("uploader capability response exceeds 256 bytes")
	}
	return output.buffer.Write(raw)
}
