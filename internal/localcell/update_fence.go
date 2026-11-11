package localcell

import (
	"errors"
	"os"
	"time"
)

// maintenanceFence is separate from desired service state and the derived
// update record. Install, Start, Repair, and Remove check it under their
// operation lock; other entry points do not yet enforce maintenance admission.
type maintenanceFence struct {
	Generation  int       `json:"generation"`
	OperationID string    `json:"operation_id"`
	Since       time.Time `json:"since"`
}

func (f maintenanceFence) valid() bool {
	return f.Generation > 0 && validUpdateOperationID(f.OperationID) && !f.Since.IsZero()
}

func (c *Cell) readMaintenanceFence() (*maintenanceFence, error) {
	return c.readMaintenanceFenceWith(readJSON)
}

// readMaintenanceFenceWith separates absent, validated present, and refused
// fences. An invalid parent without an observed leaf returns the original parent
// error separately, so callers can preserve their legacy error ordering.
// Once observed, a fence disappearing during validation is always refused.
func (c *Cell) readMaintenanceFenceWith(read func(string, any) error) (*maintenanceFence, error) {
	path := c.path("journal", "maintenance.json")
	info, leafErr := os.Lstat(path)
	observed := leafErr == nil
	if _, err := os.Lstat(c.path("journal")); errors.Is(err, os.ErrNotExist) {
		if observed {
			return nil, errMaintenanceFenceActive
		}
		return nil, nil
	}
	if err := privateDir(c.path("journal"), false); err != nil {
		if !observed {
			return nil, err
		}
		return nil, errMaintenanceFenceActive
	}
	if errors.Is(leafErr, os.ErrNotExist) {
		return nil, nil
	}
	if leafErr != nil || !info.Mode().IsRegular() || !privatePassword(path, info) {
		return nil, errMaintenanceFenceActive
	}
	var fence maintenanceFence
	if err := read(path, &fence); err != nil || !fence.valid() {
		return nil, errMaintenanceFenceActive
	}
	return &fence, nil
}

// maintenanceFenceAllows admits the four core lifecycle operations above.
// It is not an independent boundary for lower-level launch or service helpers.
// Zero denotes an ordinary caller; a positive generation requires an exact
// durable fence match, including when no fence exists anymore.
func (c *Cell) maintenanceFenceAllows(generation int) error {
	fence, err := c.readMaintenanceFence()
	if err != nil {
		return err
	}
	if fence == nil && generation == 0 {
		return nil
	}
	if fence != nil && generation > 0 && generation == fence.Generation {
		return nil
	}
	return errMaintenanceFenceActive
}
