package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/sealed"
	"github.com/witwave-ai/witself/internal/secretclient"
)

const totpCommandUsage = "usage: witself totp show|code [agent connection flags] [--idempotency-key KEY] [--json] SECRET FIELD"

type totpFieldRevealer interface {
	ResolveField(context.Context, string, string) (string, string, error)
	RevealField(context.Context, string, string, string) ([]byte, error)
}

type secretClientTOTPFieldRevealer struct {
	service *secretclient.Service
}

func (r secretClientTOTPFieldRevealer) ResolveField(ctx context.Context, secretSelector, fieldSelector string) (string, string, error) {
	secret, err := resolveSecret(ctx, r.service, secretSelector, "active")
	if err != nil {
		return "", "", err
	}
	field, err := resolveSecretField(secret, fieldSelector)
	if err != nil {
		return "", "", err
	}
	if field.Kind != "totp" || !field.Sensitive {
		return "", "", errors.New("the selected field is not a sensitive TOTP enrollment")
	}
	return secret.ID, field.ID, nil
}

func (r secretClientTOTPFieldRevealer) RevealField(ctx context.Context, secretID, fieldID, retryKey string) ([]byte, error) {
	return r.service.RevealField(ctx, secretID, fieldID, retryKey)
}

type totpRevealerConnector func(context.Context, string, string, string, string, string) (totpFieldRevealer, error)

var (
	totpNow                                        = time.Now
	connectTOTPFieldRevealer totpRevealerConnector = connectSecretClientTOTPFieldRevealer
)

func connectSecretClientTOTPFieldRevealer(ctx context.Context, account, realm, agent, endpoint, tokenFile string) (totpFieldRevealer, error) {
	cli, err := connectSecretCLI(ctx, account, realm, agent, endpoint, tokenFile)
	if err != nil {
		return nil, err
	}
	return secretClientTOTPFieldRevealer{service: cli.service}, nil
}

func totpCmd(args []string) int {
	if len(args) == 0 || commandHelpRequested(args) {
		printCommandGroupHelp(os.Stderr, totpCommandUsage,
			"show  Show seed-free metadata for a TOTP field",
			"code  Generate the current TOTP code locally",
		)
		if commandHelpRequested(args) {
			return 0
		}
		return 2
	}
	switch args[0] {
	case "show":
		return totpValueCommand(args[1:], false)
	case "code":
		return totpValueCommand(args[1:], true)
	default:
		fmt.Fprintf(os.Stderr, "witself totp: unknown subcommand %q\n", args[0])
		return 2
	}
}

func totpValueCommand(args []string, generateCode bool) int {
	command := "show"
	if generateCode {
		command = "code"
	}
	fs := flag.NewFlagSet("totp "+command, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configureCommandUsage(fs, "usage: witself totp "+command+" SECRET FIELD [agent connection flags] [--idempotency-key KEY] [--json]")
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	idempotencyKey := fs.String("idempotency-key", "", "retry key for this one encrypted field access")
	jsonOut := jsonFlag(fs)
	if parsed, exitCode := parseCommandFlags(fs, totpFlagParseOrder(args)); !parsed {
		return exitCode
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, totpCommandUsage)
		return 2
	}
	secretSelector := strings.TrimSpace(fs.Arg(0))
	fieldSelector := strings.TrimSpace(fs.Arg(1))
	if secretSelector == "" || fieldSelector == "" {
		fmt.Fprintln(os.Stderr, totpCommandUsage)
		return 2
	}

	retryKey := strings.TrimSpace(*idempotencyKey)
	if retryKey == "" {
		var err error
		retryKey, err = id.New("secret_access")
		if err != nil {
			fmt.Fprintln(os.Stderr, "witself: generate TOTP access retry key: unavailable")
			return 1
		}
	}

	ctx := context.Background()
	accountName, realmName, agentName := secretLocalSelectors(*account, *realm, *agent)
	if agentName == "" {
		fmt.Fprintln(os.Stderr, "witself: --agent (or WITSELF_AGENT) is required to select the local TOTP vault")
		return 2
	}
	revealer, err := connectTOTPFieldRevealer(ctx, accountName, realmName, agentName, *endpoint, *tokenFile)
	if err != nil {
		return printTOTPValueError("initialize local TOTP vault", err)
	}
	secretID, fieldID, err := revealer.ResolveField(ctx, secretSelector, fieldSelector)
	if err != nil {
		return printTOTPValueError("resolve local TOTP field", err)
	}
	plaintext, err := revealer.RevealField(ctx, secretID, fieldID, retryKey)
	if err != nil {
		clear(plaintext)
		return printTOTPValueError("reveal local TOTP field", err)
	}
	defer clear(plaintext)
	payload, err := sealed.ParseTOTPPayload(plaintext)
	if err != nil {
		fmt.Fprintln(os.Stderr, "witself: revealed field is not a valid TOTP enrollment")
		return 1
	}
	defer payload.Clear()

	if !generateCode {
		return printTOTPPayloadMetadata(payload.Metadata(), *jsonOut)
	}
	result, err := sealed.GenerateTOTPCode(payload, totpNow())
	if err != nil {
		fmt.Fprintln(os.Stderr, "witself: generate local TOTP code: unavailable")
		return 1
	}
	return printTOTPCode(result, *jsonOut)
}

func printTOTPValueError(operation string, err error) int {
	detail := "unavailable"
	switch {
	case errors.Is(err, secretclient.ErrKeyUnavailable):
		detail = "the local agent vault key is unavailable; enroll this installation with the existing key"
	case errors.Is(err, secretclient.ErrKeyMismatch):
		detail = "the local agent vault key does not match the backend binding"
	case errors.Is(err, secretclient.ErrIdentityMismatch):
		detail = "the authenticated agent does not match the local vault selectors"
	case errors.Is(err, secretclient.ErrIntegrity):
		detail = "encrypted TOTP material failed integrity verification"
	case errors.Is(err, secretclient.ErrInvalidInput):
		detail = "the secret or field identifier is invalid"
	case err != nil && err.Error() == "secret selector is ambiguous":
		detail = "the secret name is ambiguous; use an exact secret id"
	case err != nil && err.Error() == "secret was not found":
		detail = "the secret was not found"
	case err != nil && err.Error() == "secret field selector is ambiguous":
		detail = "the field name is ambiguous; use an exact field id"
	case err != nil && err.Error() == "secret field was not found":
		detail = "the secret field was not found"
	case err != nil && err.Error() == "the selected field is not a sensitive TOTP enrollment":
		detail = "the selected field is not a sensitive TOTP enrollment"
	}
	fmt.Fprintf(os.Stderr, "witself: %s: %s\n", operation, detail)
	return 1
}

// totpFlagParseOrder also permits the natural
// `totp show SECRET_ID FIELD_ID --json` spelling while retaining flag.FlagSet
// for the shared connection flags.
func totpFlagParseOrder(args []string) []string {
	if len(args) >= 2 && !strings.HasPrefix(args[0], "-") && !strings.HasPrefix(args[1], "-") {
		ordered := make([]string, 0, len(args))
		ordered = append(ordered, args[2:]...)
		ordered = append(ordered, args[:2]...)
		return ordered
	}
	return args
}

func printTOTPPayloadMetadata(metadata sealed.TOTPPayloadMetadata, jsonOut bool) int {
	if jsonOut {
		return printJSON(metadata)
	}
	_, _ = fmt.Fprintf(os.Stdout, "issuer\t%s\n", metadata.Issuer)
	_, _ = fmt.Fprintf(os.Stdout, "account\t%s\n", metadata.Account)
	_, _ = fmt.Fprintf(os.Stdout, "algorithm\t%s\n", metadata.Algorithm)
	_, _ = fmt.Fprintf(os.Stdout, "digits\t%d\n", metadata.Digits)
	_, _ = fmt.Fprintf(os.Stdout, "period_seconds\t%d\n", metadata.PeriodSeconds)
	return 0
}

func printTOTPCode(result sealed.TOTPCode, jsonOut bool) int {
	if jsonOut {
		return printJSON(result)
	}
	_, _ = fmt.Fprintf(os.Stdout, "code\t%s\n", result.Code)
	_, _ = fmt.Fprintf(os.Stdout, "digits\t%d\n", result.Digits)
	_, _ = fmt.Fprintf(os.Stdout, "period_seconds\t%d\n", result.PeriodSeconds)
	_, _ = fmt.Fprintf(os.Stdout, "remaining_seconds\t%d\n", result.RemainingSeconds)
	_, _ = fmt.Fprintf(os.Stdout, "expires_at\t%s\n", result.ExpiresAt.UTC().Format(time.RFC3339))
	return 0
}
