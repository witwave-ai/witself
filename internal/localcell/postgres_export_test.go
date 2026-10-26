package localcell

import "os/exec"

// ConfigurePostgresProofCommand lets the external native proof apply exactly
// the product's PostgreSQL token policy without exporting a production API.
func ConfigurePostgresProofCommand(cmd *exec.Cmd) (func(), error) {
	return configurePostgresCommand(cmd)
}
