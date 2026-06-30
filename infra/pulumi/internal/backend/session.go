package backend

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// EnsureAWSSession refreshes an expired AWS SSO session BEFORE an operation runs,
// so a command doesn't fail on credentials mid-flight. It acts only when a profile
// is given AND the terminal is interactive — a browser login would hang in CI,
// where credentials come from OIDC and need no login. It checks the session with
// STS and, if invalid, shells out to `aws sso login --profile <p>`.
//
// Best-effort: if the aws CLI is missing or the login doesn't complete, it logs
// and returns, letting the operation surface the real credential error.
func EnsureAWSSession(ctx context.Context, profile string) {
	if profile == "" || !interactiveTTY() {
		return
	}
	if awsSessionValid(ctx, profile) {
		return
	}
	fmt.Fprintf(os.Stderr, "witself-infra: AWS session for profile %q is not valid — running `aws sso login --profile %s`...\n", profile, profile)
	cmd := exec.CommandContext(ctx, "aws", "sso", "login", "--profile", profile)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "witself-infra: `aws sso login` did not complete (%v); continuing\n", err)
	}
}

// interactiveTTY reports whether stdin is a terminal (so a browser login is safe).
func interactiveTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func awsSessionValid(ctx context.Context, profile string) bool {
	cfg, err := awscfg.LoadDefaultConfig(ctx, awscfg.WithSharedConfigProfile(profile))
	if err != nil {
		return false
	}
	_, err = sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	return err == nil
}
