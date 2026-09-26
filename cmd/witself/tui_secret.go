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
	"github.com/witwave-ai/witself/internal/dashboard"
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
		return nil, agenttui.SecretUnavailable
	}
	service, err := secretclient.New(secretclient.Config{
		Endpoint: s.connection.Endpoint, Token: s.connection.Token,
		AccountID: s.identity.AccountID, AccountName: accountName,
		RealmName: s.identity.RealmName, AgentName: s.identity.AgentName,
	})
	if err != nil {
		return nil, agenttui.SecretUnavailable
	}
	return revealTUISecret(ctx, service, secretID, fieldID)
}

type tuiSecretReader interface {
	Get(context.Context, string) (*client.Secret, error)
	RevealExistingField(context.Context, string, string, string) ([]byte, error)
}

func revealTUISecret(ctx context.Context, service tuiSecretReader, secretID, fieldID string) ([]byte, error) {
	secret, err := service.Get(ctx, secretID)
	if ctx.Err() != nil {
		return nil, agenttui.SecretCanceled
	}
	if err != nil || secret == nil || secret.ID != secretID || secret.Lifecycle != "active" {
		return nil, agenttui.SecretUnavailable
	}
	var field *client.SecretField
	for i := range secret.Fields {
		if secret.Fields[i].ID == fieldID {
			field = &secret.Fields[i]
			break
		}
	}
	if field == nil {
		return nil, agenttui.SecretUnavailable
	}
	if field.Kind == "totp" {
		return nil, agenttui.SecretUnavailable
	}
	var value []byte
	if field.Sensitive {
		key, keyErr := secretIdempotencyKey("")
		if keyErr != nil {
			return nil, agenttui.SecretUnavailable
		}
		value, err = service.RevealExistingField(ctx, secret.ID, field.ID, key)
		if err != nil {
			clear(value)
			switch {
			case ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
				return nil, agenttui.SecretCanceled
			case errors.Is(err, secretclient.ErrKeyUnavailable):
				return nil, agenttui.SecretUnenrolled
			case errors.Is(err, secretclient.ErrKeyMismatch):
				return nil, agenttui.SecretMismatch
			default:
				return nil, agenttui.SecretUnavailable
			}
		}
	} else if field.PublicValue != nil {
		value = []byte(*field.PublicValue)
	} else {
		return nil, agenttui.SecretUnavailable
	}
	if ctx.Err() != nil {
		clear(value)
		return nil, agenttui.SecretCanceled
	}
	if len(value) > 64<<10 {
		clear(value)
		return nil, agenttui.SecretUnavailable
	}
	switch field.Encoding {
	case sealed.ValueEncodingUTF8, sealed.ValueEncodingJSON:
		if !utf8.Valid(value) || (field.Encoding == sealed.ValueEncodingJSON && !json.Valid(value)) {
			clear(value)
			return nil, agenttui.SecretUnavailable
		}
	case sealed.ValueEncodingBinary:
		encoded := make([]byte, base64.StdEncoding.EncodedLen(len(value)))
		base64.StdEncoding.Encode(encoded, value)
		clear(value)
		value = encoded
	default:
		clear(value)
		return nil, agenttui.SecretUnavailable
	}
	return value, nil
}

// Forward only the private principal check, never manager credentials.
func (s *tuiSecretSource) VerifyAccountConsoleAuthority(ctx context.Context, expected dashboard.AccountManagerIdentity) error {
	if source, ok := s.Source.(interface {
		VerifyAccountConsoleAuthority(context.Context, dashboard.AccountManagerIdentity) error
	}); ok {
		return source.VerifyAccountConsoleAuthority(ctx, expected)
	}
	return client.ErrAccountConsoleUnavailable
}
