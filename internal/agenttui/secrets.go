package agenttui

import (
	"context"
	"errors"
)

// SecretSource optionally transfers ownership of one field's plaintext to the
// caller, including bytes returned with an error. Implementations must refuse
// TOTP seeds and must not initialize or enroll keys. Calls require a deliberate
// reveal or copy key; inventory refresh never invokes this interface.
// Failures should use SecretRevealReason; arbitrary errors are never displayed.
type SecretSource interface {
	RevealSecret(context.Context, string, string) ([]byte, error)
}

// SecretRevealReason is the closed, value-free failure vocabulary of SecretSource.
// Unknown errors and out-of-range values are always rendered as unavailable.
type SecretRevealReason uint8

const (
	// SecretUnavailable covers all failures without a more specific safe reason.
	SecretUnavailable SecretRevealReason = iota
	// SecretUnenrolled means this installation has no enrolled vault key.
	SecretUnenrolled
	// SecretMismatch means the local key does not match the vault binding.
	SecretMismatch
	// SecretCanceled means the reveal was canceled or its deadline expired.
	SecretCanceled
)

func (r SecretRevealReason) Error() string {
	switch r {
	case SecretUnenrolled:
		return "unenrolled"
	case SecretMismatch:
		return "mismatch"
	case SecretCanceled:
		return "canceled"
	default:
		return "unavailable"
	}
}

func secretRevealReason(err error) SecretRevealReason {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return SecretCanceled
	}
	var reason SecretRevealReason
	if errors.As(err, &reason) && reason <= SecretCanceled {
		return reason
	}
	return SecretUnavailable
}

func secretStatusText(status string) string {
	switch status {
	case "unenrolled":
		return "Vault key is not enrolled here"
	case "mismatch":
		return "Local vault key does not match"
	case "canceled":
		return "Reveal canceled"
	case "loading", "copied", "copy unavailable", "copy failed":
		return status
	default:
		return "Reveal unavailable"
	}
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
