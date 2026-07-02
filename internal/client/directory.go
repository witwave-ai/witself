package client

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// LookupAccount asks the control plane directory which cell currently hosts
// an account. The CLI calls this fresh on every command rather than caching
// an endpoint locally: accounts can move between cells, and the directory is
// the projection of where they live right now.
func LookupAccount(ctx context.Context, controlPlane, accountID string) (cellName, endpoint string, err error) {
	var out struct {
		Cell struct {
			Cell     string `json:"cell"`
			Endpoint string `json:"endpoint"`
		} `json:"cell"`
	}
	url := strings.TrimRight(controlPlane, "/") + "/v1/directory/" + accountID
	if err := doJSON(ctx, http.MethodGet, url, "", nil, &out); err != nil {
		return "", "", err
	}
	if out.Cell.Endpoint == "" {
		return "", "", fmt.Errorf("directory returned no endpoint for %s", accountID)
	}
	return out.Cell.Cell, out.Cell.Endpoint, nil
}
