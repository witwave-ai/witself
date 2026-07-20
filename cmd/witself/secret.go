package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/sealed"
	"github.com/witwave-ai/witself/internal/secretclient"
)

const maxSecretCreateDocumentBytes = 2 << 20

type secretCLIContext struct {
	connection agentConnection
	service    *secretclient.Service
}

type secretCreateDocument struct {
	Name        string                      `json:"name"`
	Description string                      `json:"description,omitempty"`
	Template    string                      `json:"template,omitempty"`
	Tags        []string                    `json:"tags,omitempty"`
	Fields      []secretCreateFieldDocument `json:"fields"`
}

type secretCreateFieldDocument struct {
	Name             string                        `json:"name"`
	Kind             string                        `json:"kind,omitempty"`
	Sensitive        *bool                         `json:"sensitive,omitempty"`
	Encoding         string                        `json:"encoding,omitempty"`
	Value            *string                       `json:"value,omitempty"`
	ValueBase64      *string                       `json:"value_base64,omitempty"`
	OTPAuthURI       *string                       `json:"otpauth_uri,omitempty"`
	GeneratePassword bool                          `json:"generate_password,omitempty"`
	PasswordPolicy   *secretPasswordPolicyDocument `json:"password_policy,omitempty"`
}

type secretPasswordPolicyDocument struct {
	Length           int   `json:"length,omitempty"`
	Lowercase        *bool `json:"lowercase,omitempty"`
	Uppercase        *bool `json:"uppercase,omitempty"`
	Digits           *bool `json:"digits,omitempty"`
	Symbols          *bool `json:"symbols,omitempty"`
	ExcludeAmbiguous bool  `json:"exclude_ambiguous,omitempty"`
}

func secretCmd(args []string) int {
	if len(args) == 0 || commandHelpRequested(args) {
		printCommandGroupHelp(os.Stderr,
			"usage: witself secret create|list|search|show|reveal|archive|restore ...",
			"create   Create a structured secret from strict JSON",
			"list     List redacted secret inventory",
			"search   Search public secret metadata",
			"show     Show one redacted secret and its field inventory",
			"reveal   Return exactly one non-TOTP field",
			"archive  Archive one active secret",
			"restore  Restore one archived secret",
		)
		if commandHelpRequested(args) {
			return 0
		}
		return 2
	}
	switch args[0] {
	case "create":
		return secretCreate(args[1:])
	case "list", "search":
		return secretList(args[0], args[1:])
	case "show":
		return secretShow(args[1:])
	case "reveal":
		return secretReveal(args[1:])
	case "archive", "restore":
		return secretLifecycle(args[0], args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself: unknown secret command %q\n", args[0])
		return 2
	}
}

func vaultCmd(args []string) int {
	keyHelp := len(args) == 2 && args[0] == "key" && commandHelpRequested(args[1:])
	if commandHelpRequested(args) || keyHelp {
		printCommandGroupHelp(os.Stderr,
			"usage: witself vault key init|status|enroll|recovery|rotate|rotation ...",
			"key init    Reconcile or initialize this installation's agent vault key",
			"key status  Compare the local key with the backend's public binding",
			"key enroll  Enroll another installation without exposing the key to Witself",
			"key recovery  Export, inspect, or import an offline encrypted recovery artifact",
			"key rotate  Rotate every sensitive field wrapper to a new client-held key epoch",
			"key rotation  Inspect or cancel an in-progress rotation",
		)
		return 0
	}
	if len(args) < 2 || args[0] != "key" {
		fmt.Fprintln(os.Stderr, "usage: witself vault key init|status|enroll|recovery|rotate|rotation ...")
		return 2
	}
	switch args[1] {
	case "init", "status":
		return vaultKey(args[1], args[2:])
	case "enroll":
		return vaultKeyEnroll(args[2:])
	case "recovery":
		return vaultKeyRecovery(args[2:])
	case "rotate", "rotation":
		return vaultKeyRotation(args[1], args[2:])
	default:
		fmt.Fprintf(os.Stderr, "witself: unknown vault key command %q\n", args[1])
		return 2
	}
}

func vaultKey(operation string, args []string) int {
	fs := flag.NewFlagSet("vault key "+operation, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configureCommandUsage(fs, "usage: witself vault key "+operation+" [agent connection flags]")
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	jsonOut := jsonFlag(fs)
	if parsed, exitCode := parseCommandFlags(fs, args); !parsed {
		return exitCode
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "witself: vault key accepts no positional arguments")
		return 2
	}
	cli, err := connectSecretCLI(context.Background(), *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		return printSecretCLIError("connect vault", err)
	}
	if operation == "status" {
		status, err := cli.service.VaultKeyStatus(context.Background())
		if err != nil {
			return printSecretCLIError("read vault key status", err)
		}
		if *jsonOut {
			return printJSON(status)
		}
		fmt.Printf("state:\t%s\nlocal key:\t%t\nbackend binding:\t%t\nmatch:\t%t\n",
			status.State, status.LocalPresent, status.BackendPresent, status.Match)
		if status.LocalMetadata != nil {
			fmt.Printf("key:\t%s (version %d)\nfingerprint:\t%s\n",
				status.LocalMetadata.ID, status.LocalMetadata.Version, status.LocalMetadata.Fingerprint)
		} else if status.BackendBinding != nil {
			fmt.Printf("key:\t%s (version %d)\nfingerprint:\t%s\n",
				status.BackendBinding.ID, status.BackendBinding.KeyVersion, status.BackendBinding.Fingerprint)
		}
		return 0
	}
	key, err := cli.service.ReconcileVaultKey(context.Background())
	if err != nil {
		return printSecretCLIError("initialize vault key", err)
	}
	defer key.Clear()
	metadata := key.Metadata()
	if *jsonOut {
		return printJSON(map[string]any{"state": "match", "key": metadata})
	}
	fmt.Printf("state:\tmatch\nkey:\t%s (version %d)\nfingerprint:\t%s\n",
		metadata.ID, metadata.Version, metadata.Fingerprint)
	return 0
}

func secretCreate(args []string) int {
	fs := flag.NewFlagSet("secret create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configureCommandUsage(fs, "usage: witself secret create (--file FILE|--stdin) --idempotency-key KEY [agent connection flags]")
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	file := fs.String("file", "", "read the plaintext secret document from FILE")
	stdin := fs.Bool("stdin", false, "read the plaintext secret document from stdin")
	idempotencyKey := fs.String("idempotency-key", "", "required retry key; reuse only for this exact logical create")
	jsonOut := jsonFlag(fs)
	if parsed, exitCode := parseCommandFlags(fs, args); !parsed {
		return exitCode
	}
	if fs.NArg() != 0 || ((*file == "") == !*stdin) || strings.TrimSpace(*idempotencyKey) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself secret create (--file FILE|--stdin) --idempotency-key KEY [agent connection flags]")
		return 2
	}
	document, raw, err := readSecretCreateDocument(*file, *stdin)
	if raw != nil {
		defer clear(raw)
	}
	if err != nil {
		return printSecretCLIError("read secret document", err)
	}
	fields, err := toSecretClientFields(document.Fields)
	if err != nil {
		clearSecretClientFields(fields)
		return printSecretCLIError("validate secret document", err)
	}
	defer clearSecretClientFields(fields)
	retryKey := strings.TrimSpace(*idempotencyKey)
	cli, err := connectSecretCLI(context.Background(), *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		return printSecretCLIError("connect secret vault", err)
	}
	result, err := cli.service.Create(context.Background(), secretclient.CreateInput{
		Name: document.Name, Description: document.Description, Template: document.Template,
		Tags: document.Tags, Fields: fields, IdempotencyKey: retryKey,
	})
	if err != nil {
		return printSecretCLIError("create secret", err)
	}
	if *jsonOut {
		return printJSON(result)
	}
	fmt.Printf("created\t%s\t%s\n", result.Secret.ID, safeText(result.Secret.Name))
	return 0
}

func secretList(operation string, args []string) int {
	fs := flag.NewFlagSet("secret "+operation, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	usage := "usage: witself secret list [filters] [agent connection flags]"
	if operation == "search" {
		usage = "usage: witself secret search QUERY [filters] [agent connection flags]"
	}
	configureCommandUsage(fs, usage)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	query := fs.String("query", "", "public metadata search query")
	lifecycle := fs.String("lifecycle", "active", "active or archived")
	template := fs.String("template", "", "exact template filter")
	var tags csvListFlag
	fs.Var(&tags, "tag", "required tag (repeat or comma-separate)")
	limit := fs.Int("limit", 25, "maximum results (1-100)")
	cursor := fs.String("cursor", "", "opaque next-page cursor")
	includeFields := fs.Bool("fields", false, "include redacted field inventory")
	jsonOut := jsonFlag(fs)
	parseArgs := args
	if operation == "search" {
		parseArgs = secretFlagParseOrder(args, 1)
	}
	if parsed, exitCode := parseCommandFlags(fs, parseArgs); !parsed {
		return exitCode
	}
	if operation == "search" {
		if fs.NArg() == 1 && strings.TrimSpace(*query) == "" {
			*query = fs.Arg(0)
		} else if fs.NArg() != 0 || strings.TrimSpace(*query) == "" {
			fmt.Fprintln(os.Stderr, "usage: witself secret search QUERY [filters] [agent connection flags]")
			return 2
		}
	} else if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "witself: secret list accepts no positional arguments")
		return 2
	}
	if *limit < 1 || *limit > 100 || (*lifecycle != "active" && *lifecycle != "archived") {
		fmt.Fprintln(os.Stderr, "witself: limit must be 1-100 and lifecycle must be active or archived")
		return 2
	}
	cli, err := connectSecretCLI(context.Background(), *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		return printSecretCLIError("connect secret vault", err)
	}
	page, err := cli.service.List(context.Background(), client.SecretListOptions{
		Query: strings.TrimSpace(*query), Lifecycle: *lifecycle, Template: *template,
		Tags: []string(tags), Limit: *limit, Cursor: *cursor, IncludeFields: *includeFields,
	})
	if err != nil {
		return printSecretCLIError("list secrets", err)
	}
	if *jsonOut {
		return printJSON(page)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tTEMPLATE\tSTATE\tSENSITIVE\tID")
	for _, value := range page.Items {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", tabSafe(safeText(value.Name)),
			tabSafe(safeText(value.Template)), value.Lifecycle, value.SensitiveCount, value.ID)
	}
	_ = w.Flush()
	if page.NextCursor != "" {
		fmt.Fprintf(os.Stderr, "next cursor: %s\n", page.NextCursor)
	}
	return 0
}

func secretShow(args []string) int {
	fs := flag.NewFlagSet("secret show", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configureCommandUsage(fs, "usage: witself secret show [--lifecycle active|archived] SECRET [agent connection flags]")
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	lifecycle := fs.String("lifecycle", "active", "active or archived")
	jsonOut := jsonFlag(fs)
	if parsed, exitCode := parseCommandFlags(fs, secretFlagParseOrder(args, 1)); !parsed {
		return exitCode
	}
	if fs.NArg() != 1 || (*lifecycle != "active" && *lifecycle != "archived") {
		fmt.Fprintln(os.Stderr, "usage: witself secret show [--lifecycle active|archived] SECRET [agent connection flags]")
		return 2
	}
	cli, err := connectSecretCLI(context.Background(), *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		return printSecretCLIError("connect secret vault", err)
	}
	value, err := resolveSecret(context.Background(), cli.service, fs.Arg(0), *lifecycle)
	if err != nil {
		return printSecretCLIError("show secret", err)
	}
	if *jsonOut {
		return printJSON(map[string]any{"secret": value})
	}
	printSecret(value)
	return 0
}

func secretReveal(args []string) int {
	fs := flag.NewFlagSet("secret reveal", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configureCommandUsage(fs, "usage: witself secret reveal SECRET FIELD [--idempotency-key KEY] [agent connection flags]")
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	idempotencyKey := fs.String("idempotency-key", "", "retry key for this exact field access")
	jsonOut := jsonFlag(fs)
	if parsed, exitCode := parseCommandFlags(fs, secretFlagParseOrder(args, 2)); !parsed {
		return exitCode
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: witself secret reveal SECRET FIELD [--idempotency-key KEY] [agent connection flags]")
		return 2
	}
	cli, err := connectSecretCLI(context.Background(), *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		return printSecretCLIError("connect secret vault", err)
	}
	secret, err := resolveSecret(context.Background(), cli.service, fs.Arg(0), "active")
	if err != nil {
		return printSecretCLIError("resolve secret", err)
	}
	field, err := resolveSecretField(secret, fs.Arg(1))
	if err != nil {
		return printSecretCLIError("resolve secret field", err)
	}
	if field.Kind == "totp" {
		fmt.Fprintln(os.Stderr, "witself: TOTP enrollment material cannot be revealed; use `witself totp show` or `witself totp code`")
		return 2
	}
	var plaintext []byte
	if field.Sensitive {
		retryKey, err := secretIdempotencyKey(*idempotencyKey)
		if err != nil {
			return printSecretCLIError("create access retry key", err)
		}
		plaintext, err = cli.service.RevealField(context.Background(), secret.ID, field.ID, retryKey)
		if err != nil {
			return printSecretCLIError("reveal secret field", err)
		}
		defer clear(plaintext)
	} else if field.PublicValue != nil {
		plaintext = []byte(*field.PublicValue)
		defer clear(plaintext)
	} else {
		return printSecretCLIError("reveal secret field", errors.New("public value is unavailable"))
	}
	return printRevealedSecretField(secret.ID, field, plaintext, *jsonOut)
}

func secretLifecycle(operation string, args []string) int {
	fs := flag.NewFlagSet("secret "+operation, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configureCommandUsage(fs, fmt.Sprintf("usage: witself secret %s SECRET [--expected-row-version N] [--idempotency-key KEY] [agent connection flags]", operation))
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	expectedRevision := fs.Int64("expected-row-version", 0, "exact current secret row version (default: resolve current)")
	idempotencyKey := fs.String("idempotency-key", "", "retry key for this exact lifecycle change")
	jsonOut := jsonFlag(fs)
	if parsed, exitCode := parseCommandFlags(fs, secretFlagParseOrder(args, 1)); !parsed {
		return exitCode
	}
	if fs.NArg() != 1 || *expectedRevision < 0 {
		fmt.Fprintf(os.Stderr, "usage: witself secret %s SECRET [--expected-row-version N] [--idempotency-key KEY] [agent connection flags]\n", operation)
		return 2
	}
	cli, err := connectSecretCLI(context.Background(), *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		return printSecretCLIError("connect secret vault", err)
	}
	lifecycle := "active"
	if operation == "restore" {
		lifecycle = "archived"
	}
	value, err := resolveSecret(context.Background(), cli.service, fs.Arg(0), lifecycle)
	if err != nil {
		return printSecretCLIError(operation+" secret", err)
	}
	if *expectedRevision == 0 {
		*expectedRevision = value.RowVersion
	}
	retryKey, err := secretIdempotencyKey(*idempotencyKey)
	if err != nil {
		return printSecretCLIError("create lifecycle retry key", err)
	}
	input := client.SecretLifecycleInput{ExpectedRowVersion: *expectedRevision, IdempotencyKey: retryKey}
	var result *client.SecretMutationResult
	if operation == "archive" {
		result, err = client.ArchiveSecret(context.Background(), cli.connection.Endpoint, cli.connection.Token, value.ID, input)
	} else {
		result, err = client.RestoreSecret(context.Background(), cli.connection.Endpoint, cli.connection.Token, value.ID, input)
	}
	if err != nil {
		return printSecretCLIError(operation+" secret", err)
	}
	if *jsonOut {
		return printJSON(result)
	}
	fmt.Printf("%sd\t%s\t%s\n", operation, result.Secret.ID, safeText(result.Secret.Name))
	return 0
}

func connectSecretCLI(ctx context.Context, accountName, realmName, agentName, endpoint, tokenFile string) (secretCLIContext, error) {
	accountName, realmName, agentName = secretLocalSelectors(accountName, realmName, agentName)
	if agentName == "" {
		return secretCLIContext{}, errors.New("an agent selector is required")
	}
	connection, err := connectAgent(ctx, accountName, realmName, agentName, endpoint, tokenFile)
	if err != nil {
		return secretCLIContext{}, err
	}
	resolvedAccountName, account, err := local.ResolveAccount(accountName)
	if err != nil {
		return secretCLIContext{}, errors.New("the selected local account binding is required for secret custody")
	}
	if connection.AccountID != "" && connection.AccountID != account.ID {
		return secretCLIContext{}, errors.New("the selected local account binding changed during secret connection")
	}
	service, err := secretclient.New(secretclient.Config{
		Endpoint: connection.Endpoint, Token: connection.Token,
		AccountID: account.ID, AccountName: resolvedAccountName,
		RealmName: realmName, AgentName: agentName,
	})
	if err != nil {
		return secretCLIContext{}, err
	}
	return secretCLIContext{connection: connection, service: service}, nil
}

func secretLocalSelectors(accountName, realmName, agentName string) (string, string, string) {
	accountName = strings.TrimSpace(accountName)
	if accountName == "" {
		accountName = strings.TrimSpace(os.Getenv("WITSELF_ACCOUNT"))
	}
	if accountName == "" {
		accountName = "default"
	}
	realmName = strings.TrimSpace(realmName)
	if realmName == "" {
		realmName = strings.TrimSpace(os.Getenv("WITSELF_REALM"))
	}
	if realmName == "" {
		realmName = "default"
	}
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		agentName = strings.TrimSpace(os.Getenv("WITSELF_AGENT"))
	}
	return accountName, realmName, agentName
}

// secretFlagParseOrder permits the documented positional-first spellings such
// as `secret reveal NAME FIELD --json` while retaining flag.FlagSet for shared
// connection flags. Flags-first input is returned unchanged.
func secretFlagParseOrder(args []string, positionalCount int) []string {
	if positionalCount < 1 || len(args) < positionalCount {
		return args
	}
	for index := 0; index < positionalCount; index++ {
		if strings.HasPrefix(args[index], "-") {
			return args
		}
	}
	ordered := make([]string, 0, len(args))
	ordered = append(ordered, args[positionalCount:]...)
	ordered = append(ordered, args[:positionalCount]...)
	return ordered
}

func readSecretCreateDocument(path string, fromStdin bool) (secretCreateDocument, []byte, error) {
	var reader io.Reader
	var file *os.File
	if fromStdin {
		reader = os.Stdin
	} else {
		opened, err := os.Open(path)
		if err != nil {
			return secretCreateDocument{}, nil, errors.New("could not open secret document")
		}
		file = opened
		defer func() { _ = file.Close() }()
		reader = file
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maxSecretCreateDocumentBytes+1))
	if err != nil || len(raw) == 0 || len(raw) > maxSecretCreateDocumentBytes {
		clear(raw)
		return secretCreateDocument{}, nil, errors.New("secret document is empty, unreadable, or too large")
	}
	if err := rejectDuplicateJSONFields(raw); err != nil {
		return secretCreateDocument{}, raw, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var document secretCreateDocument
	if err := decoder.Decode(&document); err != nil {
		return secretCreateDocument{}, raw, errors.New("secret document is not valid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return secretCreateDocument{}, raw, errors.New("secret document has trailing data")
	}
	return document, raw, nil
}

func rejectDuplicateJSONFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("invalid object key")
				}
				if _, duplicate := seen[key]; duplicate {
					return errors.New("secret document contains a duplicate field")
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return errors.New("invalid JSON delimiter")
		}
	}
	if err := walk(); err != nil {
		return errors.New("secret document is not strict JSON")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("secret document has trailing data")
	}
	return nil
}

func toSecretClientFields(documents []secretCreateFieldDocument) ([]secretclient.FieldInput, error) {
	if len(documents) == 0 {
		return nil, errors.New("at least one field is required")
	}
	fields := make([]secretclient.FieldInput, 0, len(documents))
	for _, document := range documents {
		field, err := toSecretClientField(document)
		if err != nil {
			clearSecretClientFields(fields)
			return nil, err
		}
		fields = append(fields, field)
	}
	return fields, nil
}

func toSecretClientField(document secretCreateFieldDocument) (secretclient.FieldInput, error) {
	kind := strings.TrimSpace(document.Kind)
	if kind == "" {
		kind = "text"
	}
	encoding := strings.TrimSpace(document.Encoding)
	if encoding == "" {
		encoding = sealed.ValueEncodingUTF8
	}
	sensitive := true
	if document.Sensitive != nil {
		sensitive = *document.Sensitive
	}
	sources := 0
	if document.Value != nil {
		sources++
	}
	if document.ValueBase64 != nil {
		sources++
	}
	if document.OTPAuthURI != nil {
		sources++
	}
	if document.GeneratePassword {
		sources++
	}
	if sources != 1 || strings.TrimSpace(document.Name) == "" {
		return secretclient.FieldInput{}, errors.New("each secret field requires one value source")
	}
	if document.PasswordPolicy != nil && !document.GeneratePassword {
		return secretclient.FieldInput{}, errors.New("password_policy requires generate_password")
	}
	var value []byte
	switch {
	case document.Value != nil:
		value = []byte(*document.Value)
	case document.ValueBase64 != nil:
		decoded, err := base64.StdEncoding.Strict().DecodeString(*document.ValueBase64)
		if err != nil {
			return secretclient.FieldInput{}, errors.New("a secret field has invalid base64")
		}
		value = decoded
	case document.OTPAuthURI != nil:
		if kind != "totp" || !sensitive {
			return secretclient.FieldInput{}, errors.New("otpauth_uri requires a sensitive totp field")
		}
		if document.Encoding != "" && encoding != sealed.ValueEncodingJSON {
			return secretclient.FieldInput{}, errors.New("otpauth_uri requires JSON encoding")
		}
		payload, err := sealed.ParseOTPAuthTOTP(*document.OTPAuthURI)
		if err != nil {
			return secretclient.FieldInput{}, errors.New("a TOTP enrollment is invalid")
		}
		defer payload.Clear()
		value, err = sealed.EncodeTOTPPayload(payload)
		if err != nil {
			return secretclient.FieldInput{}, errors.New("a TOTP enrollment is invalid")
		}
		encoding = sealed.ValueEncodingJSON
	case document.GeneratePassword:
		if kind != "password" || !sensitive {
			return secretclient.FieldInput{}, errors.New("generate_password requires a sensitive password field")
		}
		if document.Encoding != "" && encoding != sealed.ValueEncodingUTF8 {
			return secretclient.FieldInput{}, errors.New("generate_password requires UTF-8 encoding")
		}
		policy := sealed.DefaultPasswordPolicy()
		if document.PasswordPolicy != nil {
			applySecretPasswordPolicy(&policy, *document.PasswordPolicy)
		}
		password, err := sealed.GeneratePasswordBytes(policy)
		if err != nil {
			return secretclient.FieldInput{}, errors.New("a generated-password policy is invalid")
		}
		value = password
		encoding = sealed.ValueEncodingUTF8
	}
	if len(value) == 0 {
		clear(value)
		return secretclient.FieldInput{}, errors.New("secret field values must not be empty")
	}
	if document.ValueBase64 != nil && encoding != sealed.ValueEncodingBinary {
		clear(value)
		return secretclient.FieldInput{}, errors.New("value_base64 requires binary encoding")
	}
	if encoding == sealed.ValueEncodingUTF8 && !utf8.Valid(value) {
		clear(value)
		return secretclient.FieldInput{}, errors.New("a UTF-8 field is invalid")
	}
	if encoding == sealed.ValueEncodingJSON && (!json.Valid(value) || rejectDuplicateJSONFields(value) != nil) {
		clear(value)
		return secretclient.FieldInput{}, errors.New("a JSON field is invalid")
	}
	return secretclient.FieldInput{
		Name: strings.TrimSpace(document.Name), Kind: kind, Encoding: encoding,
		Sensitive: sensitive, Value: value,
	}, nil
}

func applySecretPasswordPolicy(policy *sealed.PasswordPolicy, document secretPasswordPolicyDocument) {
	if document.Length != 0 {
		policy.Length = document.Length
	}
	if document.Lowercase != nil {
		policy.Lowercase = *document.Lowercase
	}
	if document.Uppercase != nil {
		policy.Uppercase = *document.Uppercase
	}
	if document.Digits != nil {
		policy.Digits = *document.Digits
	}
	if document.Symbols != nil {
		policy.Symbols = *document.Symbols
	}
	policy.ExcludeAmbiguous = document.ExcludeAmbiguous
}

func clearSecretClientFields(fields []secretclient.FieldInput) {
	for index := range fields {
		clear(fields[index].Value)
		fields[index].Value = nil
	}
}

func resolveSecret(ctx context.Context, service *secretclient.Service, selector, lifecycle string) (*client.Secret, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil, errors.New("secret selector is required")
	}
	if lifecycle == "active" && strings.HasPrefix(selector, "sec_") {
		return service.Get(ctx, selector)
	}
	cursor := ""
	var match *client.Secret
	for pageNumber := 0; pageNumber < 100; pageNumber++ {
		query := selector
		if strings.HasPrefix(selector, "sec_") {
			query = ""
		}
		page, err := service.List(ctx, client.SecretListOptions{
			Query: query, Lifecycle: lifecycle, Limit: 100, Cursor: cursor, IncludeFields: true,
		})
		if err != nil {
			return nil, err
		}
		for index := range page.Items {
			if page.Items[index].ID == selector || page.Items[index].Name == selector {
				if match != nil {
					return nil, errors.New("secret selector is ambiguous")
				}
				candidate := page.Items[index]
				match = &candidate
			}
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if match != nil {
		return match, nil
	}
	return nil, errors.New("secret was not found")
}

func resolveSecretField(secret *client.Secret, selector string) (client.SecretField, error) {
	selector = strings.TrimSpace(selector)
	var match *client.SecretField
	for index := range secret.Fields {
		field := &secret.Fields[index]
		if field.ID == selector || field.Name == selector {
			if match != nil {
				return client.SecretField{}, errors.New("secret field selector is ambiguous")
			}
			match = field
		}
	}
	if match == nil {
		return client.SecretField{}, errors.New("secret field was not found")
	}
	return *match, nil
}

func printSecret(value *client.Secret) {
	fmt.Printf("name:\t%s\nid:\t%s\ntemplate:\t%s\nstate:\t%s\nrevision:\t%d\n",
		safeText(value.Name), value.ID, safeText(value.Template), value.Lifecycle, value.RowVersion)
	if value.Description != "" {
		fmt.Printf("description:\t%s\n", safeText(value.Description))
	}
	if len(value.Tags) != 0 {
		fmt.Printf("tags:\t%s\n", strings.Join(value.Tags, ","))
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "\nFIELD\tKIND\tSENSITIVE\tVALUE\tID")
	for _, field := range value.Fields {
		display := "[redacted]"
		if !field.Sensitive && field.PublicValue != nil {
			display = tabSafe(safeText(*field.PublicValue))
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%t\t%s\t%s\n", field.Name, field.Kind,
			field.Sensitive, display, field.ID)
	}
	_ = w.Flush()
}

func printRevealedSecretField(secretID string, field client.SecretField, plaintext []byte, jsonOut bool) int {
	if field.Encoding == sealed.ValueEncodingUTF8 || field.Encoding == sealed.ValueEncodingJSON {
		if !utf8.Valid(plaintext) || (field.Encoding == sealed.ValueEncodingJSON && !json.Valid(plaintext)) {
			return printSecretCLIError("render secret field", errors.New("decrypted value encoding is invalid"))
		}
		if jsonOut {
			return printJSON(map[string]any{
				"secret_id": secretID, "field_id": field.ID, "field_name": field.Name,
				"encoding": field.Encoding, "value": string(plaintext),
			})
		}
		_, _ = os.Stdout.Write(append(append([]byte(nil), plaintext...), '\n'))
		return 0
	}
	encoded := base64.StdEncoding.EncodeToString(plaintext)
	if jsonOut {
		return printJSON(map[string]any{
			"secret_id": secretID, "field_id": field.ID, "field_name": field.Name,
			"encoding": field.Encoding, "value_base64": encoded,
		})
	}
	_, _ = fmt.Fprintln(os.Stdout, encoded)
	return 0
}

func secretIdempotencyKey(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value != "" {
		return value, nil
	}
	return id.New("retry")
}

func printSecretCLIError(operation string, err error) int {
	fmt.Fprintf(os.Stderr, "witself: %s: %v\n", operation, err)
	return 1
}
