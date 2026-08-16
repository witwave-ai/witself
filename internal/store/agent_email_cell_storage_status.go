package store

import (
	"context"
	"fmt"
)

// AgentEmailCellStorageStatus is the value-free, cell-local logical email
// storage projection used by readiness checks and Prometheus collection. It is
// platform safety state, never account-plan usage.
type AgentEmailCellStorageStatus struct {
	RetainedBytes     int64
	RootRows          int64
	CountedRows       int64
	AdmissionBytes    int64
	AdmissionRootRows int64
	HardBytes         int64
	HardCountedRows   int64
}

// ReadAgentEmailCellStorageStatus reads schema 91's singleton without locking
// it. Scrapes must never delay writers or expose tenant identifiers.
func (s *Store) ReadAgentEmailCellStorageStatus(
	ctx context.Context,
) (AgentEmailCellStorageStatus, error) {
	var status AgentEmailCellStorageStatus
	err := s.pool.QueryRow(ctx, `
		SELECT retained_bytes,root_rows,counted_rows,
		       admission_bytes,admission_root_rows,
		       hard_bytes,hard_counted_rows
		  FROM agent_email_cell_storage_capacity
		 WHERE singleton=1`).Scan(
		&status.RetainedBytes,
		&status.RootRows,
		&status.CountedRows,
		&status.AdmissionBytes,
		&status.AdmissionRootRows,
		&status.HardBytes,
		&status.HardCountedRows,
	)
	if err != nil {
		return AgentEmailCellStorageStatus{}, fmt.Errorf(
			"read agent-email cell storage status: %w", err,
		)
	}
	if err := validateAgentEmailCellStorageStatus(status); err != nil {
		return AgentEmailCellStorageStatus{}, err
	}
	return status, nil
}

func validateAgentEmailCellStorageStatus(status AgentEmailCellStorageStatus) error {
	if status.RetainedBytes < 0 || status.RootRows < 0 || status.CountedRows < 0 ||
		status.RootRows > status.CountedRows {
		return fmt.Errorf("agent-email cell storage status has invalid usage counters")
	}
	if err := validateAgentEmailCellStorageLimits(
		status.AdmissionBytes,
		status.AdmissionRootRows,
		status.HardBytes,
		status.HardCountedRows,
	); err != nil {
		return fmt.Errorf("agent-email cell storage status has invalid limits: %w", err)
	}
	return nil
}
