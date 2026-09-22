package agenttui

import (
	"context"
	"errors"
)

// SecretSource optionally transfers ownership of one field's plaintext to the
// caller, including bytes returned with an error. Implementations must refuse
// TOTP seeds and must not initialize or enroll keys. Calls require a deliberate
// reveal or copy key; inventory refresh never invokes this interface.
type SecretSource interface {
	RevealSecret(context.Context, string, string) ([]byte, error)
}

type privateExpiredMsg struct{ generation uint64 }

// RevealSecret serves authored demo values only; nothing is read from a vault.
func (d *demoSource) RevealSecret(ctx context.Context, secretID, fieldID string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if secretID == "secret_preview" {
		switch fieldID {
		case "field_user":
			return []byte("atlas-preview@example.test"), nil
		case "field_password":
			return []byte("DEMO-only-Copper-Finch-42!"), nil
		}
	}
	return nil, errors.New("demo field unavailable")
}

var _ SecretSource = (*demoSource)(nil)
