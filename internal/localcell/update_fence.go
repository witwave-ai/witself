package localcell

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
)

const maintenanceFenceMaxBytes = 4096

// maintenanceFence is separate from desired service state and the derived
// update record. Install, Start, Repair, and Remove check it under their
// operation lock. PrepareService checks new publication, and reconcileServiceBoot
// checks boot publication and stale-record cleanup under their existing locks.
// Other service entry points do not yet enforce maintenance admission.
type maintenanceFence struct {
	Generation  int       `json:"generation"`
	OperationID string    `json:"operation_id"`
	Since       time.Time `json:"since"`
}

func (f maintenanceFence) valid() bool {
	return f.Generation > 0 && validUpdateOperationID(f.OperationID) && !f.Since.IsZero()
}

func (c *Cell) readMaintenanceFence() (*maintenanceFence, error) {
	return c.readMaintenanceFenceWith(readMaintenanceFenceJSON)
}

func readMaintenanceFenceJSON(path string, dst any) error {
	f, err := openPrivateFile(path, os.O_RDONLY, 0)
	if err != nil {
		return errors.New("cannot open local cell record")
	}
	defer func() { _ = f.Close() }()
	return decodeMaintenanceFence(f, dst)
}

func decodeMaintenanceFence(r io.Reader, dst any) error {
	// Read one extra byte so a valid prefix cannot hide an oversized fence.
	data, err := io.ReadAll(io.LimitReader(r, maintenanceFenceMaxBytes+1))
	if err != nil {
		return errors.New("cannot read local cell record")
	}
	if len(data) > maintenanceFenceMaxBytes {
		return errors.New("local cell record exceeds size limit")
	}
	if err := checkMaintenanceFenceShape(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return errors.New("invalid local cell record")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid trailing local cell record")
	}
	return nil
}

func checkMaintenanceFenceShape(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("invalid local cell record")
	}
	seen := make(map[string]bool, 3)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return errors.New("invalid local cell record")
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("invalid local cell record")
		}
		switch key {
		case "generation", "operation_id", "since":
		default:
			return errors.New("invalid local cell record")
		}
		if seen[key] {
			return errors.New("invalid local cell record")
		}
		seen[key] = true
		value, err := decoder.Token()
		if err != nil || value == nil {
			return errors.New("invalid local cell record")
		}
		if _, nested := value.(json.Delim); nested {
			return errors.New("invalid local cell record")
		}
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') || len(seen) < 3 {
		return errors.New("invalid local cell record")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("invalid local cell record")
	}
	return nil
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

// maintenanceFenceAllows admits Install, Start, Repair, Remove, PrepareService,
// reconcileServiceBoot, setServiceDesiredStateLocked (running), and RepairService.
// It is not a service-wide maintenance policy or
// an independent boundary for lower-level launch helpers.
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
