package localcell

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const drillFormat, drillMaxBytes = 1, 16 << 10

type drillReason string

type drillError struct{ reason drillReason }

func (drillError) Error() string { return "drill request refused" }

const (
	drillTooLarge          drillReason = "too_large"
	drillMalformed         drillReason = "malformed"
	drillUnknownVersion    drillReason = "unknown_version"
	drillIdentityInvalid   drillReason = "identity_invalid"
	drillBindingInvalid    drillReason = "binding_invalid"
	drillStateInvalid      drillReason = "state_invalid"
	drillOutcomeBeforeStop drillReason = "outcome_before_stop"
	drillStopRequired      drillReason = "stop_required"
	drillTransitionRefused drillReason = "transition_refused"
	drillStaleResume       drillReason = "stale_resume"
	drillReceiptBinding    drillReason = "receipt_binding"
	drillReceiptStale      drillReason = "receipt_stale"
)

type drillBinding struct {
	InstallationID         string `json:"installation_id"`
	CheckpointID           string `json:"checkpoint_id"`
	InventorySHA256        string `json:"inventory_sha256"`
	LayoutVersion          int    `json:"layout_version"`
	PrincipalLayoutVersion int    `json:"principal_layout_version"`
	SchemaVersion          int    `json:"schema_version"`
	PostgresMajor          string `json:"postgres_major"`
}
type drillRecord struct {
	Format      int          `json:"format"`
	DrillID     string       `json:"drill_id"`
	OperationID string       `json:"operation_id"`
	Binding     drillBinding `json:"binding"`
	State       string       `json:"state"`
	Outcome     string       `json:"outcome"`
	CleanupDue  bool         `json:"cleanup_due"`
}
type drillReceipt struct {
	Format     int          `json:"format"`
	DrillID    string       `json:"drill_id"`
	Binding    drillBinding `json:"binding"`
	Outcome    string       `json:"outcome"`
	CleanupDue bool         `json:"cleanup_due"`
}

func drillStateIndex(state string) int {
	for i, candidate := range []string{"reserved", "copied", "verified", "launched", "probed", "stopped"} {
		if state == candidate {
			return i
		}
	}
	return -1
}
func drillValidOperationID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}
func drillValidInstallationID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}
func drillValidCheckpointID(id string) bool {
	if len(id) != 21 || id[:5] != "ckpt_" {
		return false
	}
	for i := 5; i < len(id); i++ {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}
func drillValidID(id string) bool {
	if len(id) != 22 || id[:6] != "drill_" {
		return false
	}
	for i := 6; i < len(id); i++ {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}
func drillValidBinding(b drillBinding) bool {
	return drillValidInstallationID(b.InstallationID) &&
		drillValidCheckpointID(b.CheckpointID) && len(b.InventorySHA256) == 64 &&
		strings.Trim(b.InventorySHA256, "0123456789abcdef") == "" && b.LayoutVersion >= 1 &&
		b.PrincipalLayoutVersion >= 0 && b.SchemaVersion >= 1 && b.PostgresMajor == "16"
}
func drillScanObject(d *json.Decoder, binding, receipt bool) (bool, bool) {
	fields := map[string]string{"format": "number", "drill_id": "string", "operation_id": "string", "binding": "object", "state": "string", "outcome": "string", "cleanup_due": "bool"}
	if receipt {
		delete(fields, "operation_id")
		delete(fields, "state")
	}
	if binding {
		fields = map[string]string{"installation_id": "string", "checkpoint_id": "string", "inventory_sha256": "string", "layout_version": "number", "principal_layout_version": "number", "schema_version": "number", "postgres_major": "string"}
	}
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return false, false
	}
	seen, incomplete := map[string]bool{}, false
	for d.More() {
		keyToken, err := d.Token()
		key, isKey := keyToken.(string)
		kind, known := fields[key]
		if err != nil || !isKey || !known || seen[key] {
			return false, false
		}
		seen[key] = true
		if kind == "object" {
			var ok bool
			incomplete, ok = drillScanObject(d, true, false)
			if !ok {
				return false, false
			}
			continue
		}
		value, err := d.Token()
		if err != nil {
			return false, false
		}
		var ok bool
		switch kind {
		case "string":
			_, ok = value.(string)
		case "bool":
			_, ok = value.(bool)
		case "number":
			var number json.Number
			number, ok = value.(json.Number)
			if ok {
				_, err = number.Int64()
				ok = err == nil
			}
		}
		if !ok {
			return false, false
		}
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') {
		return false, false
	}
	for key := range fields {
		if !seen[key] {
			if binding || key == "binding" {
				incomplete = true
			} else {
				return false, false
			}
		}
	}
	return incomplete, true
}
func drillValidate(r drillRecord) error {
	switch {
	case r.Format != drillFormat:
		return drillError{reason: drillUnknownVersion}
	case !drillValidID(r.DrillID) || !drillValidOperationID(r.OperationID):
		return drillError{reason: drillIdentityInvalid}
	case !drillValidBinding(r.Binding):
		return drillError{reason: drillBindingInvalid}
	case drillStateIndex(r.State) < 0 || (r.Outcome != "" && r.Outcome != "passed" && r.Outcome != "failed"):
		return drillError{reason: drillStateInvalid}
	case r.Outcome == "passed" && r.State != "stopped":
		return drillError{reason: drillOutcomeBeforeStop}
	case r.Outcome != "" && (r.State == "launched" || r.State == "probed"):
		return drillError{reason: drillStopRequired}
	}
	return nil
}
func drillDecode(data []byte, receipt bool, dst any) error {
	if len(data) > drillMaxBytes {
		return drillError{reason: drillTooLarge}
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	incomplete, ok := drillScanObject(d, false, receipt)
	_, tail := d.Token()
	if !ok || !errors.Is(tail, io.EOF) || json.Unmarshal(data, dst) != nil {
		return drillError{reason: drillMalformed}
	}
	var version struct{ Format int }
	if json.Unmarshal(data, &version) != nil {
		return drillError{reason: drillMalformed}
	}
	if version.Format != drillFormat {
		return drillError{reason: drillUnknownVersion}
	}
	if incomplete {
		return drillError{reason: drillBindingInvalid}
	}
	return nil
}
func drillValidateReceipt(r drillReceipt) error {
	if err := drillValidate(drillRecord{r.Format, r.DrillID, "receipt", r.Binding, "stopped", r.Outcome, r.CleanupDue}); err != nil {
		return err
	}
	if r.Outcome == "" {
		return drillError{reason: drillTransitionRefused}
	}
	return nil
}
func drillCaller(r drillRecord, op string) error {
	if err := drillValidate(r); err != nil {
		return err
	}
	if !drillValidOperationID(op) {
		return drillError{reason: drillIdentityInvalid}
	}
	return nil
}
func drillParseRecord(data []byte) (drillRecord, error) {
	var r drillRecord
	if err := drillDecode(data, false, &r); err != nil {
		return drillRecord{}, err
	}
	if err := drillValidate(r); err != nil {
		return drillRecord{}, err
	}
	return r, nil
}
func drillParseReceipt(data []byte) (drillReceipt, error) {
	var r drillReceipt
	if err := drillDecode(data, true, &r); err != nil {
		return drillReceipt{}, err
	}
	if err := drillValidateReceipt(r); err != nil {
		return drillReceipt{}, err
	}
	return r, nil
}
func drillAdvance(r drillRecord, op, state string) (drillRecord, error) {
	if err := drillCaller(r, op); err != nil {
		return drillRecord{}, err
	}
	if r.Outcome != "" || drillStateIndex(state) != drillStateIndex(r.State)+1 {
		return drillRecord{}, drillError{reason: drillTransitionRefused}
	}
	if op != r.OperationID && (r.State != "probed" || state != "stopped") {
		return drillRecord{}, drillError{reason: drillStaleResume}
	}
	r.State = state
	return r, nil
}
func drillStop(r drillRecord, op string) (drillRecord, error) {
	if err := drillCaller(r, op); err != nil {
		return drillRecord{}, err
	}
	if r.Outcome != "" || (r.State != "launched" && r.State != "probed") {
		return drillRecord{}, drillError{reason: drillTransitionRefused}
	}
	r.State = "stopped"
	return r, nil
}
func drillFinish(r drillRecord, op, outcome string) (drillRecord, error) {
	if err := drillCaller(r, op); err != nil {
		return drillRecord{}, err
	}
	if r.Outcome != "" || (outcome != "passed" && outcome != "failed") {
		return drillRecord{}, drillError{reason: drillTransitionRefused}
	}
	if outcome == "passed" && op != r.OperationID {
		return drillRecord{}, drillError{reason: drillStaleResume}
	}
	r.Outcome = outcome
	if err := drillValidate(r); err != nil {
		return drillRecord{}, err
	}
	return r, nil
}
func drillCleanup(r drillRecord, due bool) (drillRecord, error) {
	if err := drillValidate(r); err != nil {
		return drillRecord{}, err
	}
	if due && !r.CleanupDue {
		return drillRecord{}, drillError{reason: drillTransitionRefused}
	}
	r.CleanupDue = due
	return r, nil
}
func drillResume(r drillRecord, op string) (string, error) {
	if err := drillCaller(r, op); err != nil {
		return "", err
	}
	switch {
	case r.Outcome != "":
		return "none", nil
	case r.State == "launched" || r.State == "probed":
		return "stop_child", nil
	case op != r.OperationID:
		return "record_failed", nil
	default:
		return "continue", nil
	}
}
func drillMakeReceipt(r drillRecord) (drillReceipt, error) {
	if err := drillValidate(r); err != nil {
		return drillReceipt{}, err
	}
	if r.Outcome == "" {
		return drillReceipt{}, drillError{reason: drillTransitionRefused}
	}
	return drillReceipt{r.Format, r.DrillID, r.Binding, r.Outcome, r.CleanupDue}, nil
}
func drillCheckReceipt(receipt drillReceipt, r drillRecord, present drillBinding) error {
	if err := drillValidate(r); err != nil {
		return err
	}
	if receipt.DrillID != r.DrillID || receipt.Binding != r.Binding || receipt.Outcome != r.Outcome {
		return drillError{reason: drillReceiptBinding}
	}
	if err := drillValidateReceipt(receipt); err != nil {
		return err
	}
	if receipt.Binding != present {
		return drillError{reason: drillReceiptStale}
	}
	return nil
}
