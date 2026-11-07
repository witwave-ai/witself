package localcell

// ProcessRecord binds a PID to its native process creation identity and cell.
// A PID alone is never sufficient authority to signal a process.
type ProcessRecord struct {
	LaunchKind         string `json:"launch_kind,omitempty"`
	ShutdownEndpoint   string `json:"shutdown_endpoint,omitempty"`
	ShutdownDescriptor string `json:"shutdown_descriptor,omitempty"`
	InstallationID     string `json:"installation_id,omitempty"`
	Generation         string `json:"generation,omitempty"`
	// LaunchPending is Windows recovery authority for a suspended or not-yet-confirmed launch.
	LaunchPending bool   `json:"launch_pending,omitempty"`
	PID           int    `json:"pid"`
	StartTime     string `json:"start_time"`
	BootID        string `json:"boot_id,omitempty"`
	Root          string `json:"root"`
	Component     string `json:"component"`
}
