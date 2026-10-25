package localcell

// ProcessRecord binds a PID to its native process creation identity and cell.
// A PID alone is never sufficient authority to signal a process.
type ProcessRecord struct {
	// LaunchPending is Windows recovery authority for a suspended or not-yet-confirmed launch.
	LaunchPending bool   `json:"launch_pending,omitempty"`
	PID           int    `json:"pid"`
	StartTime     string `json:"start_time"`
	Root          string `json:"root"`
	Component     string `json:"component"`
}
