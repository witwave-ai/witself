package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/activity"
	"github.com/witwave-ai/witself/internal/agenttui"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/sealed"
	"github.com/witwave-ai/witself/internal/secretclient"
)

// tuiSecretSource adds only an explicit, exact-field reveal to the passive
// projection reader. It does not widen that reader or the browser console.
type tuiSecretSource struct {
	agenttui.Source
	connection agentConnection
	identity   client.SelfIdentity
}

func (s *tuiSecretSource) RevealSecret(ctx context.Context, secretID, fieldID string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(activity.WithDeliberate(ctx), 10*time.Second)
	defer cancel()
	accountName, account, err := local.ResolveAccount(s.connection.AccountName)
	if err != nil || account.ID != s.identity.AccountID {
		return nil, errors.New("secret access needs the matching local account and enrolled vault key")
	}
	service, err := secretclient.New(secretclient.Config{
		Endpoint: s.connection.Endpoint, Token: s.connection.Token,
		AccountID: s.identity.AccountID, AccountName: accountName,
		RealmName: s.identity.RealmName, AgentName: s.identity.AgentName,
	})
	if err != nil {
		return nil, errors.New("local secret custody is unavailable")
	}
	return revealTUISecret(ctx, service, secretID, fieldID)
}

type tuiSecretReader interface {
	Get(context.Context, string) (*client.Secret, error)
	RevealExistingField(context.Context, string, string, string) ([]byte, error)
}

func revealTUISecret(ctx context.Context, service tuiSecretReader, secretID, fieldID string) ([]byte, error) {
	secret, err := service.Get(ctx, secretID)
	if err != nil || secret == nil || secret.ID != secretID || secret.Lifecycle != "active" {
		return nil, errors.New("selected secret is unavailable")
	}
	var field *client.SecretField
	for i := range secret.Fields {
		if secret.Fields[i].ID == fieldID {
			field = &secret.Fields[i]
			break
		}
	}
	if field == nil {
		return nil, errors.New("selected field is unavailable")
	}
	if field.Kind == "totp" {
		return nil, errors.New("TOTP seed cannot be revealed; use witself totp code for a current code")
	}
	var value []byte
	if field.Sensitive {
		key, keyErr := secretIdempotencyKey("")
		if keyErr != nil {
			return nil, errors.New("could not start field access")
		}
		value, err = service.RevealExistingField(ctx, secret.ID, field.ID, key)
		if err != nil {
			clear(value)
			switch {
			case errors.Is(err, secretclient.ErrKeyUnavailable):
				return nil, errors.New("vault key unavailable here; enroll this installation before revealing")
			case errors.Is(err, secretclient.ErrKeyMismatch):
				return nil, errors.New("local vault key does not match; reveal refused")
			default:
				return nil, errors.New("field access failed; no value was displayed")
			}
		}
	} else if field.PublicValue != nil {
		value = []byte(*field.PublicValue)
	} else {
		return nil, errors.New("selected field value is unavailable")
	}
	if ctx.Err() != nil {
		clear(value)
		return nil, errors.New("field access canceled")
	}
	if len(value) > 64<<10 {
		clear(value)
		return nil, errors.New("field is too large for terminal reveal")
	}
	switch field.Encoding {
	case sealed.ValueEncodingUTF8, sealed.ValueEncodingJSON:
		if !utf8.Valid(value) || (field.Encoding == sealed.ValueEncodingJSON && !json.Valid(value)) {
			clear(value)
			return nil, errors.New("field encoding is invalid")
		}
	case sealed.ValueEncodingBinary:
		encoded := make([]byte, base64.StdEncoding.EncodedLen(len(value)))
		base64.StdEncoding.Encode(encoded, value)
		clear(value)
		value = encoded
	default:
		clear(value)
		return nil, errors.New("field encoding is unsupported")
	}
	return value, nil
}
