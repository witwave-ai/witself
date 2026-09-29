package client

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"time"
)

// FleetAccountLifecycle is the closed, read-only operator lifecycle view.
type FleetAccountLifecycle struct {
	SchemaVersion     string                     `json:"schema_version"`
	AccountID         string                     `json:"account_id"`
	ObservedAt        *time.Time                 `json:"observed_at"`
	Initialized       *bool                      `json:"initialized"`
	Revision          *int64                     `json:"revision"`
	Epoch             *int64                     `json:"epoch"`
	Location          *FleetLifecycleLocation    `json:"location"`
	DriverActive      *bool                      `json:"driver_active"`
	AlarmAt           *time.Time                 `json:"alarm_at"`
	Operation         *FleetLifecycleOperation   `json:"operation"`
	Projections       *FleetLifecycleProjections `json:"projections"`
	LastCompleted     *FleetLifecycleCompleted   `json:"last_completed"`
	RestoreQuarantine *FleetLifecycleQuarantine  `json:"restore_quarantine"`
	Directory         *FleetLifecycleDirectory   `json:"directory"`
}

// FleetLifecycleLocation excludes archive objects and route endpoints.
type FleetLifecycleLocation struct {
	Kind string  `json:"kind"`
	Cell *string `json:"cell"`
}

// FleetLifecycleOperation contains only operator-safe progress fields.
type FleetLifecycleOperation struct {
	OperationID                string                   `json:"operation_id"`
	EvacuationID               *string                  `json:"evacuation_id"`
	Kind                       string                   `json:"kind"`
	Phase                      string                   `json:"phase"`
	Epoch                      *int64                   `json:"epoch"`
	RequestEpoch               *int64                   `json:"request_epoch"`
	SourceCell                 string                   `json:"source_cell"`
	TargetCell                 *string                  `json:"target_cell"`
	SourceRegistrationID       *string                  `json:"source_registration_id"`
	TargetRegistrationID       *string                  `json:"target_registration_id"`
	TargetProtocol             *int64                   `json:"target_protocol"`
	HasArchive                 bool                     `json:"has_archive"`
	HasArchiveOrigin           bool                     `json:"has_archive_origin"`
	NextStep                   *FleetLifecycleStep      `json:"next_step"`
	ImportedStatus             *string                  `json:"imported_status"`
	RestoredStatus             *string                  `json:"restored_status"`
	SourceFinalization         *string                  `json:"source_finalization"`
	TargetReservationExpiresAt *time.Time               `json:"target_reservation_expires_at"`
	Retryable                  *bool                    `json:"retryable"`
	LastError                  *string                  `json:"last_error"`
	ExportJob                  *FleetLifecycleExportJob `json:"export_job"`
	ImportJob                  *FleetLifecycleImportJob `json:"import_job"`
}

// FleetLifecycleStep excludes step payloads.
type FleetLifecycleStep struct {
	Type   *string `json:"type"`
	Action *string `json:"action"`
	Target *string `json:"target"`
}

// FleetLifecycleExportJob reports export progress without archive metadata.
type FleetLifecycleExportJob struct {
	StreamAttempts  *int64     `json:"stream_attempts"`
	VerifyAttempts  *int64     `json:"verify_attempts"`
	StreamStartedAt *time.Time `json:"stream_started_at"`
	Streamed        bool       `json:"streamed"`
	StreamedAt      *time.Time `json:"streamed_at"`
	StreamMS        *int64     `json:"stream_ms"`
}

// FleetLifecycleImportJob excludes receipt etags.
type FleetLifecycleImportJob struct {
	Attempts              *int64     `json:"attempts"`
	FirstStartedAt        *time.Time `json:"first_started_at"`
	StartedAt             *time.Time `json:"started_at"`
	LastPolledAt          *time.Time `json:"last_polled_at"`
	RetryAt               *time.Time `json:"retry_at"`
	Validated             bool       `json:"validated"`
	ManifestSchemaVersion *int64     `json:"manifest_schema_version"`
	NextAlarmAction       string     `json:"next_alarm_action"`
}

// FleetLifecycleProjections reports reconciliation without projected values.
type FleetLifecycleProjections struct {
	Route   *FleetLifecycleProjection `json:"route"`
	Archive *FleetLifecycleProjection `json:"archive"`
	Cleanup *FleetLifecycleProjection `json:"cleanup"`
}

// FleetLifecycleProjection describes one reconciliation action.
type FleetLifecycleProjection struct {
	Action string `json:"action"`
	Status string `json:"status"`
}

// FleetLifecycleCompleted reports the last retired operation.
type FleetLifecycleCompleted struct {
	OperationID       string                  `json:"operation_id"`
	EvacuationID      *string                 `json:"evacuation_id"`
	Kind              string                  `json:"kind"`
	Outcome           string                  `json:"outcome"`
	Epoch             *int64                  `json:"epoch"`
	SourceCell        string                  `json:"source_cell"`
	TargetCell        *string                 `json:"target_cell"`
	CompletedRevision *int64                  `json:"completed_revision"`
	FinalLocation     *FleetLifecycleLocation `json:"final_location"`
}

// FleetLifecycleQuarantine compares archive identity without disclosing keys.
type FleetLifecycleQuarantine struct {
	Reason           string     `json:"reason"`
	QuarantinedAt    *time.Time `json:"quarantined_at"`
	MatchesOperation bool       `json:"matches_operation"`
	MatchesLocation  bool       `json:"matches_location"`
}

// FleetLifecycleDirectory is an eventually consistent routing projection.
type FleetLifecycleDirectory struct {
	LiveCell     *string `json:"live_cell"`
	ArchivedCell *string `json:"archived_cell"`
}

var lifecycleIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,256}$`)
var lifecycleWordPattern = regexp.MustCompile(`^[a-z_]{1,64}$`)
var lifecycleErrors = []string{
	"archive import above 90 MiB requires target account evacuation protocol 2",
	"target cell schema is older than the archive; upgrade the target cell first",
	"archive import failed; retry required",
	"target cell attests protocol 2 while an import job is in flight; waiting for the roll",
	"import job ended without a receipt; retry required",
	"import job exhausted its attempts; operator attention required",
	"import capacity exhausted; retry scheduled",
	"import job exceeded its age limit; operator attention required",
	"account exists under a different evacuation",
	"archive schema is newer than this cell — upgrade the cell first",
	"invalid or corrupt archive",
}

const lifecycleUnrecognizedError = "unrecognized lifecycle error"

func (*FleetAccountLifecycle) maxResponseBytes() int64 { return 64 * 1024 }

// GetFleetAccountLifecycle reads lifecycle authority without driving it.
func GetFleetAccountLifecycle(ctx context.Context, endpoint, token, accountID string) (*FleetAccountLifecycle, error) {
	if !fleetAccountIDPattern.MatchString(accountID) {
		return nil, errors.New("invalid account_id")
	}
	u, err := fleetRequestURL(endpoint, "/v1/placement/accounts/"+accountID+"/lifecycle")
	if err != nil {
		return nil, err
	}
	var result FleetAccountLifecycle
	if err := doJSON(ctx, http.MethodGet, u, token, nil, &result); err != nil {
		return nil, err
	}
	if !result.normalize(accountID) {
		return nil, errors.New("invalid account lifecycle status")
	}
	return &result, nil
}

func lifecycleKnown(value *string, known ...string) {
	if value != nil && !slices.Contains(known, *value) {
		*value = "unknown"
	}
}
func lifecycleWord(value *string) {
	if value != nil && !lifecycleWordPattern.MatchString(*value) {
		*value = "unknown"
	}
}
func lifecycleOptionalID(value *string, pattern *regexp.Regexp) bool {
	return value == nil || pattern.MatchString(*value)
}
func lifecycleCounters(values ...*int64) bool {
	for _, value := range values {
		if value != nil && *value < 0 {
			return false
		}
	}
	return true
}
func lifecycleLocation(value *FleetLifecycleLocation) bool {
	if value == nil {
		return true
	}
	lifecycleKnown(&value.Kind, "live", "archived", "closed_archived", "closed")
	return lifecycleOptionalID(value.Cell, fleetCellNamePattern)
}
func lifecycleKind(value *string) { lifecycleKnown(value, "evacuate", "move", "restore", "close") }
func (r *FleetAccountLifecycle) normalize(accountID string) bool {
	if r.SchemaVersion != "witself.v0" || r.AccountID != accountID || r.Initialized == nil || r.DriverActive == nil || r.ObservedAt == nil {
		return false
	}
	if *r.Initialized && (r.Revision == nil || r.Epoch == nil || r.Location == nil) {
		return false
	}
	if !lifecycleCounters(r.Revision, r.Epoch) || !lifecycleLocation(r.Location) {
		return false
	}
	if d := r.Directory; d != nil && (!lifecycleOptionalID(d.LiveCell, fleetCellNamePattern) || !lifecycleOptionalID(d.ArchivedCell, fleetCellNamePattern)) {
		return false
	}
	if op := r.Operation; op != nil {
		if !lifecycleIDPattern.MatchString(op.OperationID) || op.Kind == "" || op.Phase == "" || !fleetCellNamePattern.MatchString(op.SourceCell) || !lifecycleOptionalID(op.TargetCell, fleetCellNamePattern) || !lifecycleOptionalID(op.EvacuationID, fleetAccountIDPattern) || !lifecycleOptionalID(op.SourceRegistrationID, lifecycleIDPattern) || !lifecycleOptionalID(op.TargetRegistrationID, lifecycleIDPattern) || !lifecycleCounters(op.Epoch, op.RequestEpoch) {
			return false
		}
		lifecycleKind(&op.Kind)
		lifecycleWord(&op.Phase)
		if op.TargetProtocol != nil && (*op.TargetProtocol < 1 || *op.TargetProtocol > 99) {
			op.TargetProtocol = nil
		}
		if step := op.NextStep; step != nil {
			lifecycleWord(step.Type)
			lifecycleWord(step.Action)
			lifecycleWord(step.Target)
		}
		lifecycleKnown(op.ImportedStatus, "active", "suspended", "closed")
		lifecycleKnown(op.RestoredStatus, "active", "suspended", "closed")
		lifecycleKnown(op.SourceFinalization, "same_cell_target", "legacy_unfenced_source", "source_cell_unregistered", "source_cell_replaced", "cell_receipt")
		if op.LastError != nil && !slices.Contains(lifecycleErrors, *op.LastError) && *op.LastError != lifecycleUnrecognizedError {
			*op.LastError = lifecycleUnrecognizedError
		}
		if job := op.ExportJob; job != nil && !lifecycleCounters(job.StreamAttempts, job.VerifyAttempts, job.StreamMS) {
			return false
		}
		if job := op.ImportJob; job != nil {
			if !lifecycleCounters(job.Attempts, job.ManifestSchemaVersion) {
				return false
			}
			lifecycleKnown(&job.NextAlarmAction, "start", "wait", "poll")
		}
	}
	if c := r.LastCompleted; c != nil {
		if !lifecycleIDPattern.MatchString(c.OperationID) || c.Kind == "" || !fleetCellNamePattern.MatchString(c.SourceCell) || !lifecycleOptionalID(c.TargetCell, fleetCellNamePattern) || !lifecycleOptionalID(c.EvacuationID, fleetAccountIDPattern) || !lifecycleCounters(c.Epoch, c.CompletedRevision) || !lifecycleLocation(c.FinalLocation) {
			return false
		}
		lifecycleKind(&c.Kind)
		lifecycleKnown(&c.Outcome, "completed", "aborted", "reaped", "cancelled", "quarantined")
	}
	if p := r.Projections; p != nil {
		for _, v := range []*FleetLifecycleProjection{p.Route, p.Archive, p.Cleanup} {
			if v != nil {
				lifecycleKnown(&v.Action, "put", "delete")
				lifecycleKnown(&v.Status, "pending", "applied")
			}
		}
	}
	if q := r.RestoreQuarantine; q != nil {
		lifecycleKnown(&q.Reason, "archive_integrity", "unreadable")
	}
	return true
}
