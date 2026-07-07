package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ErrAccountArchived is returned by LookupAccount when the account is not
// currently placed on a live cell — it was evacuated and is awaiting
// placement or restore. The archive metadata is preserved on the error so
// the CLI can report it.
type ErrAccountArchived struct {
	Cell       string
	Region     string
	RegionCode string
	Object     string
	ExportedAt time.Time
}

func (e *ErrAccountArchived) Error() string {
	return fmt.Sprintf("account is archived (was on cell %s) — awaiting placement", e.Cell)
}

// LookupAccount asks the control plane directory which cell currently hosts
// an account. The CLI calls this fresh on every command rather than caching
// an endpoint locally: accounts can move between cells, and the directory is
// the projection of where they live right now. Returns *ErrAccountArchived
// when the account is preserved in R2 but not placed anywhere.
func LookupAccount(ctx context.Context, controlPlane, accountID string) (cellName, endpoint string, err error) {
	var out struct {
		Cell struct {
			Cell     string `json:"cell"`
			Endpoint string `json:"endpoint"`
		} `json:"cell"`
		Archived *struct {
			Cell       string    `json:"cell"`
			Region     string    `json:"region"`
			RegionCode string    `json:"region_code"`
			Object     string    `json:"object"`
			ExportedAt time.Time `json:"exported_at"`
		} `json:"archived,omitempty"`
	}
	url := strings.TrimRight(controlPlane, "/") + "/v1/directory/" + accountID
	if err := doJSON(ctx, http.MethodGet, url, "", nil, &out); err != nil {
		return "", "", err
	}
	if out.Archived != nil {
		return "", "", &ErrAccountArchived{
			Cell:       out.Archived.Cell,
			Region:     out.Archived.Region,
			RegionCode: out.Archived.RegionCode,
			Object:     out.Archived.Object,
			ExportedAt: out.Archived.ExportedAt,
		}
	}
	if out.Cell.Endpoint == "" {
		return "", "", errors.New("directory returned no endpoint")
	}
	return out.Cell.Cell, out.Cell.Endpoint, nil
}
