package localcell

import (
	"strings"

	"github.com/witwave-ai/witself/internal/id"
)

// Task names require an alphanumeric installation ID. Change only allocation
// for new Windows installations, keeping the existing 80-bit random body and
// every persisted identity unchanged. Never normalize a manifest on read.
func newLocalCellInstallationID(goos string) (string, error) {
	value, err := id.New("lc")
	if err != nil {
		return "", err
	}
	if goos == "windows" {
		return "lc" + strings.TrimPrefix(value, "lc_"), nil
	}
	return value, nil
}
