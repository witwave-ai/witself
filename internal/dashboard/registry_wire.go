package dashboard

import (
	"encoding/json"
	"errors"
)

// Keep the legacy credential field empty for manager sessions. A schema bump
// alone would not help: old status readers ignore schema and unknown fields.
// This wire projection is deliberately confined to durable registry I/O; public
// output still uses PublicRegistryEntry and never serializes the private field.
func privateRegistryRecord(entry RegistryEntry) any {
	if entry.Manager == nil {
		return entry
	}
	return struct {
		RegistryEntry
		AccessURL        string `json:"access_url"`
		ManagerAccessURL string `json:"manager_access_url"`
	}{RegistryEntry: entry, ManagerAccessURL: entry.AccessURL}
}

func decodeRegistryRecord(raw []byte, entry *RegistryEntry) error {
	var record struct {
		RegistryEntry
		ManagerAccessURL string `json:"manager_access_url"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return err
	}
	if record.ManagerAccessURL != "" {
		if record.Manager == nil || record.AccessURL != "" {
			return errors.New("dashboard registry credential binding is invalid")
		}
		record.AccessURL = record.ManagerAccessURL
	}
	*entry = record.RegistryEntry
	return nil
}
