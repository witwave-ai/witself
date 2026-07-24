package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/sealed"
	"github.com/witwave-ai/witself/internal/secretclient"
)

const (
	maxMCPSecretNameBytes        = 256
	maxMCPSecretDescriptionBytes = 4096
	maxMCPSecretTemplateBytes    = 128
	maxMCPSecretTags             = 64
	maxMCPSecretTagBytes         = 64
	maxMCPSecretFields           = 64
	maxMCPSecretFieldNameBytes   = 128
	maxMCPSecretKindBytes        = 64
	maxMCPSecretEncodingBytes    = 16
	maxMCPSecretQueryBytes       = 512
	maxMCPSecretCursorBytes      = 1024
	maxMCPSecretRetryKeyBytes    = 512
	maxMCPSecretMutationBytes    = 1 << 20
)

// mcpSecretBackend is the optional client-custodied secret extension. Keeping
// it separate from witselfMCPBackend preserves focused fake and custom
// backends that do not hold an agent vault key. The configured backend below
// implements the complete extension after pinning every operation to the
// installed integration identity.
type mcpSecretBackend interface {
	SecretLimitStatus(context.Context) (*client.SecretLimitStatus, error)
	SearchSecrets(context.Context, client.SecretListOptions) (*client.SecretPage, error)
	ShowSecret(context.Context, string) (*client.Secret, error)
	CreateSealedSecret(context.Context, secretclient.CreateInput) (*client.SecretMutationResult, error)
	DeleteSecret(context.Context, string, client.SecretLifecycleInput) (*client.SecretMutationResult, error)
	RevealSealedSecretField(context.Context, string, string, string) ([]byte, error)
}

type mcpSecretStatusInput struct{}

type mcpSecretSearchInput struct {
	Query         string   `json:"query,omitempty" jsonschema:"public metadata or explicitly non-sensitive value search query; omit to list inventory"`
	Lifecycle     string   `json:"lifecycle,omitempty" jsonschema:"active or archived; defaults to active"`
	Template      string   `json:"template,omitempty" jsonschema:"optional exact public template filter"`
	Tags          []string `json:"tags,omitempty" jsonschema:"optional public tags all results must contain"`
	Limit         int      `json:"limit,omitempty" jsonschema:"maximum results from 1 to 100; defaults to 25"`
	Cursor        string   `json:"cursor,omitempty" jsonschema:"opaque continuation cursor from a prior search"`
	IncludeFields bool     `json:"include_fields,omitempty" jsonschema:"include field inventory; sensitive values remain redacted"`
}

type mcpSecretShowInput struct {
	SecretID string `json:"secret_id" jsonschema:"exact active Witself secret id beginning with sec_"`
}

type mcpSecretCreateInput struct {
	Name           string                      `json:"name" jsonschema:"human-readable secret name unique among this agent's active secrets"`
	Description    string                      `json:"description,omitempty" jsonschema:"optional public description; never put a credential value here"`
	Template       string                      `json:"template,omitempty" jsonschema:"public lowercase template; defaults to generic"`
	Tags           []string                    `json:"tags,omitempty" jsonschema:"optional public lowercase search tags"`
	Fields         []mcpSecretCreateFieldInput `json:"fields" jsonschema:"one to 64 structured fields"`
	IdempotencyKey string                      `json:"idempotency_key" jsonschema:"required fresh retry key for this exact logical create"`
}

type mcpSecretCreateFieldInput struct {
	Name             string                   `json:"name" jsonschema:"stable lowercase field name"`
	Kind             string                   `json:"kind,omitempty" jsonschema:"text, username, password, url, api_key, token, private_key, totp, recovery_code, or note"`
	Sensitive        *bool                    `json:"sensitive,omitempty" jsonschema:"defaults true; protection-required kinds cannot be false"`
	Encoding         string                   `json:"encoding,omitempty" jsonschema:"utf8 or json; defaults to utf8"`
	Value            *string                  `json:"value,omitempty" jsonschema:"one explicit field value; mutually exclusive with generate_password and otpauth_uri"`
	GeneratePassword bool                     `json:"generate_password,omitempty" jsonschema:"generate and immediately seal a local password; the created-secret result remains redacted"`
	PasswordPolicy   *mcpSecretPasswordPolicy `json:"password_policy,omitempty" jsonschema:"optional local generated-password policy"`
	OTPAuthURI       *string                  `json:"otpauth_uri,omitempty" jsonschema:"one otpauth TOTP enrollment URI accepted only for immediate local canonicalization and sealing; never returned"`
}

type mcpSecretPasswordPolicy struct {
	Length           int   `json:"length,omitempty" jsonschema:"password length from 1 to 4096; defaults to 32"`
	Lowercase        *bool `json:"lowercase,omitempty" jsonschema:"include lowercase letters; defaults true"`
	Uppercase        *bool `json:"uppercase,omitempty" jsonschema:"include uppercase letters; defaults true"`
	Digits           *bool `json:"digits,omitempty" jsonschema:"include digits; defaults true"`
	Symbols          *bool `json:"symbols,omitempty" jsonschema:"include symbols; defaults true"`
	ExcludeAmbiguous bool  `json:"exclude_ambiguous,omitempty" jsonschema:"exclude visually ambiguous characters"`
}

type mcpSecretRevealInput struct {
	SecretID       string `json:"secret_id" jsonschema:"exact active Witself secret id beginning with sec_"`
	FieldID        string `json:"field_id" jsonschema:"exact field id beginning with fld_; TOTP fields are refused"`
	IdempotencyKey string `json:"idempotency_key" jsonschema:"required fresh retry key for this exact sensitive-field access"`
}

type mcpSecretDeleteInput struct {
	SecretID           string `json:"secret_id" jsonschema:"exact active or archived Witself secret id beginning with sec_"`
	ExpectedRowVersion int64  `json:"expected_row_version" jsonschema:"exact positive row version last observed"`
	IdempotencyKey     string `json:"idempotency_key" jsonschema:"required retry key for this exact tombstone operation"`
}

type mcpPasswordGenerateInput struct {
	Length           int   `json:"length,omitempty" jsonschema:"password length from 1 to 4096; defaults to 32"`
	Lowercase        *bool `json:"lowercase,omitempty" jsonschema:"include lowercase letters; defaults true"`
	Uppercase        *bool `json:"uppercase,omitempty" jsonschema:"include uppercase letters; defaults true"`
	Digits           *bool `json:"digits,omitempty" jsonschema:"include digits; defaults true"`
	Symbols          *bool `json:"symbols,omitempty" jsonschema:"include symbols; defaults true"`
	ExcludeAmbiguous bool  `json:"exclude_ambiguous,omitempty" jsonschema:"exclude visually ambiguous characters"`
}

type mcpTOTPCodeInput struct {
	SecretID       string `json:"secret_id" jsonschema:"exact active Witself secret id beginning with sec_"`
	FieldID        string `json:"field_id" jsonschema:"exact sensitive TOTP field id beginning with fld_"`
	IdempotencyKey string `json:"idempotency_key" jsonschema:"required fresh retry key for this exact encrypted TOTP-field access"`
}

type mcpSecretSearchOutput struct {
	Secrets    []client.Secret `json:"secrets"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

type mcpSecretOutput struct {
	Secret client.Secret `json:"secret"`
}

type mcpSecretStatusOutput struct {
	Limit client.SecretLimitStatus `json:"limit"`
}

type mcpSecretCreateOutput struct {
	Secret  client.Secret                `json:"secret"`
	Receipt client.SecretMutationReceipt `json:"receipt"`
}

type mcpSecretRevealOutput struct {
	SecretID    string `json:"secret_id"`
	FieldID     string `json:"field_id"`
	FieldName   string `json:"field_name"`
	FieldKind   string `json:"field_kind"`
	Encoding    string `json:"encoding"`
	Value       string `json:"value,omitempty"`
	ValueBase64 string `json:"value_base64,omitempty"`
}

type mcpPasswordGenerateOutput struct {
	Password string `json:"password"`
	Length   int    `json:"length"`
}

type mcpTOTPCodeOutput struct {
	SecretID         string    `json:"secret_id"`
	FieldID          string    `json:"field_id"`
	Code             string    `json:"code"`
	Digits           int       `json:"digits"`
	PeriodSeconds    uint64    `json:"period_seconds"`
	RemainingSeconds uint64    `json:"remaining_seconds"`
	ExpiresAt        time.Time `json:"expires_at"`
}

var mcpSecretNow = time.Now

var _ mcpSecretBackend = configuredMCPBackend{}

func (b configuredMCPBackend) configuredSecretService(ctx context.Context) (*secretclient.Service, error) {
	conn, _, err := b.connectAndVerify(ctx, false)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(b.cfg.AccountID) == "" {
		return nil, errors.New("installed MCP binding has no account id; reinstall the integration before using agent secrets")
	}
	account, realm, agent := secretLocalSelectors(b.cfg.Account, b.cfg.Realm, b.cfg.Agent)
	service, err := secretclient.New(secretclient.Config{
		Endpoint: conn.Endpoint, Token: conn.Token,
		AccountID: b.cfg.AccountID, AccountName: account,
		RealmName: realm, AgentName: agent,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize installed MCP secret client: %w", err)
	}
	return service, nil
}

func (b configuredMCPBackend) SearchSecrets(ctx context.Context, options client.SecretListOptions) (*client.SecretPage, error) {
	service, err := b.configuredSecretService(ctx)
	if err != nil {
		return nil, err
	}
	return service.List(ctx, options)
}

func (b configuredMCPBackend) SecretLimitStatus(ctx context.Context) (*client.SecretLimitStatus, error) {
	conn, _, err := b.connectAndVerify(ctx, false)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(b.cfg.AccountID) == "" {
		return nil, errors.New("installed MCP binding has no account id; reinstall the integration before using agent secrets")
	}
	return client.GetSecretLimitStatus(ctx, conn.Endpoint, conn.Token)
}

func (b configuredMCPBackend) ShowSecret(ctx context.Context, secretID string) (*client.Secret, error) {
	service, err := b.configuredSecretService(ctx)
	if err != nil {
		return nil, err
	}
	return service.Get(ctx, secretID)
}

func (b configuredMCPBackend) CreateSealedSecret(ctx context.Context, input secretclient.CreateInput) (*client.SecretMutationResult, error) {
	service, err := b.configuredSecretService(ctx)
	if err != nil {
		return nil, err
	}
	return service.Create(ctx, input)
}

func (b configuredMCPBackend) DeleteSecret(ctx context.Context, secretID string, input client.SecretLifecycleInput) (*client.SecretMutationResult, error) {
	conn, _, err := b.connectAndVerify(ctx, false)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(b.cfg.AccountID) == "" {
		return nil, errors.New("installed MCP binding has no account id; reinstall the integration before using agent secrets")
	}
	return client.DeleteSecret(ctx, conn.Endpoint, conn.Token, secretID, input)
}

func (b configuredMCPBackend) RevealSealedSecretField(ctx context.Context, secretID, fieldID, idempotencyKey string) ([]byte, error) {
	service, err := b.configuredSecretService(ctx)
	if err != nil {
		return nil, err
	}
	return service.RevealField(ctx, secretID, fieldID, idempotencyKey)
}

func registerSecretMCPTools(server *mcp.Server, runtimeName string, backend mcpSecretBackend) {
	createTool := mcpToolName(runtimeName, "witself.secret.create")
	totpCodeTool := mcpToolName(runtimeName, "witself.totp.code")
	mcp.AddTool(server, &mcp.Tool{
		Name:        mcpToolName(runtimeName, "witself.secret.status"),
		Description: "Show this authenticated agent's value-free retained-secret capacity: used, maximum, remaining, unlimited, and over-limit state. It never returns secret metadata or values.",
		Annotations: mcpReadOnlyClosedWorldAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ mcpSecretStatusInput) (*mcp.CallToolResult, mcpSecretStatusOutput, error) {
		status, err := backend.SecretLimitStatus(ctx)
		if err != nil {
			return nil, mcpSecretStatusOutput{}, err
		}
		if status == nil {
			return nil, mcpSecretStatusOutput{}, errors.New("secret limit status returned no result")
		}
		return nil, mcpSecretStatusOutput{Limit: *status}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        mcpToolName(runtimeName, "witself.secret.search"),
		Description: "Search this authenticated agent's secret inventory using public metadata and explicitly non-sensitive field values. This is a redacted inventory operation: it never returns a sensitive value, ciphertext, wrapped key, AVK material, TOTP enrollment URI, or TOTP seed.",
		Annotations: mcpReadOnlyClosedWorldAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpSecretSearchInput) (*mcp.CallToolResult, mcpSecretSearchOutput, error) {
		lifecycle, err := normalizeMCPSecretLifecycle(in.Lifecycle)
		if err != nil {
			return nil, mcpSecretSearchOutput{}, err
		}
		if in.Limit == 0 {
			in.Limit = 25
		}
		if in.Limit < 1 || in.Limit > 100 {
			return nil, mcpSecretSearchOutput{}, fmt.Errorf("limit must be between 1 and 100")
		}
		if len(in.Tags) > maxMCPSecretTags {
			return nil, mcpSecretSearchOutput{}, errors.New("secret search input exceeds the supported limit")
		}
		tags := normalizedMCPSecretTags(in.Tags)
		if err := validateMCPSecretSearchBounds(in.Query, in.Template, in.Cursor, tags); err != nil {
			return nil, mcpSecretSearchOutput{}, err
		}
		page, err := backend.SearchSecrets(ctx, client.SecretListOptions{
			Query: strings.TrimSpace(in.Query), Lifecycle: lifecycle,
			Template: strings.TrimSpace(in.Template), Tags: tags,
			Limit: in.Limit, Cursor: strings.TrimSpace(in.Cursor), IncludeFields: in.IncludeFields,
		})
		if err != nil {
			return nil, mcpSecretSearchOutput{}, err
		}
		if page == nil {
			return nil, mcpSecretSearchOutput{}, errors.New("secret search returned no result")
		}
		secrets := make([]client.Secret, len(page.Items))
		for index := range page.Items {
			secrets[index] = redactMCPSecret(page.Items[index])
		}
		return nil, mcpSecretSearchOutput{Secrets: secrets, NextCursor: page.NextCursor}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        mcpToolName(runtimeName, "witself.secret.show"),
		Description: "Show one exact active secret's public metadata and field inventory for this authenticated agent. The result is redacted and never contains a sensitive value, ciphertext, wrapped key, AVK material, TOTP enrollment URI, or TOTP seed.",
		Annotations: mcpReadOnlyClosedWorldAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpSecretShowInput) (*mcp.CallToolResult, mcpSecretOutput, error) {
		if !validMCPSecretResourceID(strings.TrimSpace(in.SecretID), "sec") {
			return nil, mcpSecretOutput{}, fmt.Errorf("secret_id is invalid")
		}
		secret, err := backend.ShowSecret(ctx, strings.TrimSpace(in.SecretID))
		if err != nil {
			return nil, mcpSecretOutput{}, err
		}
		if secret == nil {
			return nil, mcpSecretOutput{}, errors.New("secret show returned no result")
		}
		return nil, mcpSecretOutput{Secret: redactMCPSecret(*secret)}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        createTool,
		Description: "Create one structured secret for this authenticated agent. Sensitive values are encrypted in this active client under its local agent vault key; only ciphertext and public metadata reach Witself. A password policy generates and immediately seals a password locally, and otpauth_uri is parsed into a canonical encrypted TOTP payload locally. The result is always redacted and does not return a generated password, enrollment URI, TOTP seed, AVK bytes, or any other sensitive field value; use one exact authorized value-returning tool later when needed.",
		Annotations: mcpWriteClosedWorldAnnotations(false, true),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpSecretCreateInput) (*mcp.CallToolResult, mcpSecretCreateOutput, error) {
		if err := validateMCPSecretCreateBounds(in); err != nil {
			return nil, mcpSecretCreateOutput{}, err
		}
		documents := make([]secretCreateFieldDocument, len(in.Fields))
		for index := range in.Fields {
			documents[index] = in.Fields[index].document()
		}
		fields, err := toSecretClientFields(documents)
		if err != nil {
			clearSecretClientFields(fields)
			return nil, mcpSecretCreateOutput{}, fmt.Errorf("invalid secret fields: %w", err)
		}
		defer clearSecretClientFields(fields)
		result, err := backend.CreateSealedSecret(ctx, secretclient.CreateInput{
			Name: in.Name, Description: in.Description, Template: in.Template,
			Tags: append([]string(nil), in.Tags...), Fields: fields,
			IdempotencyKey: strings.TrimSpace(in.IdempotencyKey),
		})
		if err != nil {
			return nil, mcpSecretCreateOutput{}, err
		}
		if result == nil {
			return nil, mcpSecretCreateOutput{}, errors.New("secret create returned no result")
		}
		return nil, mcpSecretCreateOutput{
			Secret: redactMCPSecret(result.Secret), Receipt: result.Receipt,
		}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        mcpToolName(runtimeName, "witself.secret.delete"),
		Description: "Tombstone one exact active or archived secret for this authenticated agent using an optimistic revision fence and durable retry key. The redacted tombstone and value-free receipt are returned; retained capacity is released.",
		Annotations: mcpWriteClosedWorldAnnotations(true, true),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpSecretDeleteInput) (*mcp.CallToolResult, mcpSecretCreateOutput, error) {
		in.SecretID = strings.TrimSpace(in.SecretID)
		if !validMCPSecretResourceID(in.SecretID, "sec") ||
			in.ExpectedRowVersion < 1 || !validMCPSecretRetryKey(in.IdempotencyKey) {
			return nil, mcpSecretCreateOutput{}, errors.New("secret_id, expected_row_version, or idempotency_key is invalid")
		}
		result, err := backend.DeleteSecret(ctx, in.SecretID, client.SecretLifecycleInput{
			ExpectedRowVersion: in.ExpectedRowVersion,
			IdempotencyKey:     strings.TrimSpace(in.IdempotencyKey),
		})
		if err != nil {
			return nil, mcpSecretCreateOutput{}, err
		}
		if result == nil {
			return nil, mcpSecretCreateOutput{}, errors.New("secret delete returned no result")
		}
		return nil, mcpSecretCreateOutput{
			Secret: redactMCPSecret(result.Secret), Receipt: result.Receipt,
		}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        mcpToolName(runtimeName, "witself.secret.reveal"),
		Description: fmt.Sprintf("Explicit value-returning operation. Reveal exactly one field from one active secret for the current authorized task. Sensitive material is decrypted only in this active client and the backend receives only an audited ciphertext-access request. The returned value is private. Raw TOTP enrollment material is categorically refused; use %s for a short-lived code.", totpCodeTool),
		Annotations: mcpWriteClosedWorldAnnotations(false, true),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpSecretRevealInput) (*mcp.CallToolResult, mcpSecretRevealOutput, error) {
		if !validMCPSecretResourceID(strings.TrimSpace(in.SecretID), "sec") ||
			!validMCPSecretResourceID(strings.TrimSpace(in.FieldID), "fld") ||
			!validMCPSecretRetryKey(in.IdempotencyKey) {
			return nil, mcpSecretRevealOutput{}, fmt.Errorf("secret_id, field_id, or idempotency_key is invalid")
		}
		secret, field, err := exactMCPSecretField(ctx, backend, in.SecretID, in.FieldID)
		if err != nil {
			return nil, mcpSecretRevealOutput{}, err
		}
		if field.Kind == "totp" {
			return nil, mcpSecretRevealOutput{}, fmt.Errorf("TOTP enrollment material cannot be revealed; use %s", totpCodeTool)
		}
		var plaintext []byte
		if field.Sensitive {
			plaintext, err = backend.RevealSealedSecretField(ctx, secret.ID, field.ID, strings.TrimSpace(in.IdempotencyKey))
			if err != nil {
				clear(plaintext)
				return nil, mcpSecretRevealOutput{}, err
			}
		} else if field.PublicValue != nil {
			plaintext = []byte(*field.PublicValue)
		} else {
			return nil, mcpSecretRevealOutput{}, errors.New("the selected public field has no value")
		}
		defer clear(plaintext)
		value, valueBase64, err := encodeMCPRevealedValue(field.Encoding, plaintext)
		if err != nil {
			return nil, mcpSecretRevealOutput{}, err
		}
		return nil, mcpSecretRevealOutput{
			SecretID: secret.ID, FieldID: field.ID, FieldName: field.Name,
			FieldKind: field.Kind, Encoding: field.Encoding, Value: value, ValueBase64: valueBase64,
		}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        mcpToolName(runtimeName, "witself.password.generate"),
		Description: fmt.Sprintf("Explicit value-returning local operation. Generate one cryptographically random password in this active client and return it without storing it or contacting Witself. The returned password is private; store it with %s when the account workflow requires durable custody.", createTool),
		Annotations: mcpWriteClosedWorldAnnotations(false, false),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in mcpPasswordGenerateInput) (*mcp.CallToolResult, mcpPasswordGenerateOutput, error) {
		policy := sealed.DefaultPasswordPolicy()
		applySecretPasswordPolicy(&policy, in.document())
		password, err := sealed.GeneratePassword(policy)
		if err != nil {
			return nil, mcpPasswordGenerateOutput{}, errors.New("invalid password policy")
		}
		return nil, mcpPasswordGenerateOutput{Password: password, Length: len(password)}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        totpCodeTool,
		Description: "Explicit value-returning operation. Retrieve exactly one encrypted TOTP field, decrypt and parse it in this active client, and return only the short-lived code and expiry timing. The backend never receives plaintext, and this tool never returns the enrollment URI, TOTP seed, decrypted payload, AVK bytes, or another field value.",
		Annotations: mcpWriteClosedWorldAnnotations(false, false),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpTOTPCodeInput) (*mcp.CallToolResult, mcpTOTPCodeOutput, error) {
		if !validMCPSecretResourceID(strings.TrimSpace(in.SecretID), "sec") ||
			!validMCPSecretResourceID(strings.TrimSpace(in.FieldID), "fld") ||
			!validMCPSecretRetryKey(in.IdempotencyKey) {
			return nil, mcpTOTPCodeOutput{}, fmt.Errorf("secret_id, field_id, or idempotency_key is invalid")
		}
		secret, field, err := exactMCPSecretField(ctx, backend, in.SecretID, in.FieldID)
		if err != nil {
			return nil, mcpTOTPCodeOutput{}, err
		}
		if field.Kind != "totp" || !field.Sensitive {
			return nil, mcpTOTPCodeOutput{}, errors.New("the selected field is not a sensitive TOTP enrollment")
		}
		plaintext, err := backend.RevealSealedSecretField(ctx, secret.ID, field.ID, strings.TrimSpace(in.IdempotencyKey))
		if err != nil {
			clear(plaintext)
			return nil, mcpTOTPCodeOutput{}, err
		}
		payload, parseErr := sealed.ParseTOTPPayload(plaintext)
		clear(plaintext)
		if parseErr != nil {
			return nil, mcpTOTPCodeOutput{}, errors.New("the selected TOTP enrollment failed local validation")
		}
		defer payload.Clear()
		code, err := sealed.GenerateTOTPCode(payload, mcpSecretNow())
		if err != nil {
			return nil, mcpTOTPCodeOutput{}, errors.New("the selected TOTP enrollment could not produce a code")
		}
		return nil, mcpTOTPCodeOutput{
			SecretID: secret.ID, FieldID: field.ID, Code: code.Code, Digits: code.Digits,
			PeriodSeconds: code.PeriodSeconds, RemainingSeconds: code.RemainingSeconds,
			ExpiresAt: code.ExpiresAt,
		}, nil
	})
}

func (in mcpSecretCreateFieldInput) document() secretCreateFieldDocument {
	var policy *secretPasswordPolicyDocument
	if in.PasswordPolicy != nil {
		value := in.PasswordPolicy.document()
		policy = &value
	}
	return secretCreateFieldDocument{
		Name: in.Name, Kind: in.Kind, Sensitive: in.Sensitive, Encoding: in.Encoding,
		Value: in.Value, OTPAuthURI: in.OTPAuthURI, GeneratePassword: in.GeneratePassword,
		PasswordPolicy: policy,
	}
}

func (in mcpSecretPasswordPolicy) document() secretPasswordPolicyDocument {
	return secretPasswordPolicyDocument(in)
}

func (in mcpPasswordGenerateInput) document() secretPasswordPolicyDocument {
	return secretPasswordPolicyDocument(in)
}

func validateMCPSecretSearchBounds(query, template, cursor string, tags []string) error {
	if len(strings.TrimSpace(query)) > maxMCPSecretQueryBytes ||
		len(strings.TrimSpace(template)) > maxMCPSecretTemplateBytes ||
		len(strings.TrimSpace(cursor)) > maxMCPSecretCursorBytes || len(tags) > maxMCPSecretTags {
		return errors.New("secret search input exceeds the supported limit")
	}
	for _, tag := range tags {
		if len(tag) > maxMCPSecretTagBytes {
			return errors.New("secret search input exceeds the supported limit")
		}
	}
	return nil
}

func validateMCPSecretCreateBounds(in mcpSecretCreateInput) error {
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > maxMCPSecretNameBytes ||
		len(in.Description) > maxMCPSecretDescriptionBytes || len(in.Template) > maxMCPSecretTemplateBytes ||
		len(in.Tags) > maxMCPSecretTags || len(in.Fields) < 1 || len(in.Fields) > maxMCPSecretFields ||
		!validMCPSecretRetryKey(in.IdempotencyKey) {
		return errors.New("secret create input is missing or exceeds the supported limit")
	}
	totalBytes := len(in.Name) + len(in.Description) + len(in.Template)
	for _, tag := range in.Tags {
		if len(tag) > maxMCPSecretTagBytes {
			return errors.New("secret create input is missing or exceeds the supported limit")
		}
		totalBytes += len(tag)
	}
	for _, field := range in.Fields {
		if len(field.Name) < 1 || len(field.Name) > maxMCPSecretFieldNameBytes ||
			len(field.Kind) > maxMCPSecretKindBytes || len(field.Encoding) > maxMCPSecretEncodingBytes {
			return errors.New("secret create input is missing or exceeds the supported limit")
		}
		totalBytes += len(field.Name) + len(field.Kind) + len(field.Encoding)
		if field.Value != nil {
			if len(*field.Value) > sealed.MaxSensitiveValueBytes {
				return errors.New("secret create input is missing or exceeds the supported limit")
			}
			totalBytes += len(*field.Value)
		}
		if field.OTPAuthURI != nil {
			if len(*field.OTPAuthURI) > sealed.MaxOTPAuthURIBytes {
				return errors.New("secret create input is missing or exceeds the supported limit")
			}
			totalBytes += len(*field.OTPAuthURI)
		}
		if field.PasswordPolicy != nil && (field.PasswordPolicy.Length < 0 || field.PasswordPolicy.Length > sealed.MaxPasswordLength) {
			return errors.New("secret create input is missing or exceeds the supported limit")
		}
	}
	if totalBytes > maxMCPSecretMutationBytes {
		return errors.New("secret create input is missing or exceeds the supported limit")
	}
	return nil
}

func validMCPSecretRetryKey(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxMCPSecretRetryKeyBytes || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validMCPSecretResourceID(value, prefix string) bool {
	body := strings.TrimPrefix(value, prefix+"_")
	if body == value || len(body) != 16 {
		return false
	}
	for _, character := range body {
		if (character < 'a' || character > 'z') && (character < '2' || character > '7') {
			return false
		}
	}
	return true
}

func normalizeMCPSecretLifecycle(raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return "active", nil
	}
	if value != "active" && value != "archived" {
		return "", fmt.Errorf("lifecycle must be active or archived")
	}
	return value, nil
}

func normalizedMCPSecretTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		if value := strings.TrimSpace(tag); value != "" {
			out = append(out, value)
		}
	}
	if out == nil {
		return []string{}
	}
	return out
}

func redactMCPSecret(value client.Secret) client.Secret {
	value.Tags = append([]string(nil), value.Tags...)
	if value.Tags == nil {
		value.Tags = []string{}
	}
	value.Fields = append([]client.SecretField(nil), value.Fields...)
	for index := range value.Fields {
		if value.Fields[index].Sensitive {
			value.Fields[index].PublicValue = nil
			value.Fields[index].Redacted = true
		}
	}
	return value
}

func exactMCPSecretField(ctx context.Context, backend mcpSecretBackend, secretID, fieldID string) (*client.Secret, client.SecretField, error) {
	secretID = strings.TrimSpace(secretID)
	fieldID = strings.TrimSpace(fieldID)
	secret, err := backend.ShowSecret(ctx, secretID)
	if err != nil {
		return nil, client.SecretField{}, err
	}
	if secret == nil || secret.ID != secretID || secret.Lifecycle != "active" {
		return nil, client.SecretField{}, errors.New("the selected active secret is unavailable")
	}
	for _, field := range secret.Fields {
		if field.ID == fieldID {
			return secret, field, nil
		}
	}
	return nil, client.SecretField{}, errors.New("the selected field is unavailable")
}

func encodeMCPRevealedValue(encoding string, plaintext []byte) (string, string, error) {
	switch encoding {
	case sealed.ValueEncodingUTF8:
		if !utf8.Valid(plaintext) {
			return "", "", errors.New("the revealed UTF-8 field failed local validation")
		}
		return string(plaintext), "", nil
	case sealed.ValueEncodingJSON:
		if !utf8.Valid(plaintext) || !json.Valid(plaintext) {
			return "", "", errors.New("the revealed JSON field failed local validation")
		}
		return string(plaintext), "", nil
	case sealed.ValueEncodingBinary:
		return "", base64.StdEncoding.EncodeToString(plaintext), nil
	default:
		return "", "", errors.New("the revealed field has an unsupported encoding")
	}
}
