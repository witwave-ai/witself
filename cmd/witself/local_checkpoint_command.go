package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// This disconnected command accepts only adapter-owned projections. F4 owns
// construction and released dispatch. A timeout budgets context cooperation;
// it cannot promise cancellation of filesystem work or binary authenticity.
type localCheckpointRequest struct {
	operation, root, pgbin, installationID, checkpointID string
	timeout                                              time.Duration
}
type localCheckpointFailure struct {
	err           error
	class, reason string
	cleanup       bool
}
type localCheckpointSummary struct {
	id, installationID string
	created            time.Time
	bytes              int64
	complete           bool
}
type localCheckpointObservation struct {
	status, installationID string
	installationPresent    bool
	unknown                int
	entries                []localCheckpointSummary
}
type localCheckpointCallbacks struct {
	create func(context.Context, localCheckpointRequest) localCheckpointFailure
	read   func(context.Context, localCheckpointRequest) (localCheckpointObservation, localCheckpointFailure)
	delete func(context.Context, localCheckpointRequest) localCheckpointFailure
}
type localCheckpointDiagnostic struct {
	Class             string `json:"class"`
	Reason            string `json:"reason,omitempty"`
	CleanupIncomplete bool   `json:"cleanup_incomplete"`
}
type localCheckpointEntry struct {
	ID             string `json:"id"`
	InstallationID string `json:"installation_id,omitempty"`
	Complete       bool   `json:"complete"`
	Verification   string `json:"verification"`
}
type localCheckpointSnapshot struct {
	Status         string                     `json:"status"`
	Failure        *localCheckpointDiagnostic `json:"failure,omitempty"`
	InstallationID string                     `json:"installation_id,omitempty"`
	Count          *int                       `json:"count,omitempty"`
	UnknownCount   *int                       `json:"unknown_count,omitempty"`
	Entries        []localCheckpointEntry     `json:"entries,omitempty"`
}
type localCheckpointReceipt struct {
	Operation    string                     `json:"operation"`
	Outcome      string                     `json:"outcome"`
	Failure      *localCheckpointDiagnostic `json:"failure,omitempty"`
	Before       *localCheckpointSnapshot   `json:"before,omitempty"`
	After        *localCheckpointSnapshot   `json:"after,omitempty"`
	Target       string                     `json:"target,omitempty"`
	Presence     string                     `json:"presence,omitempty"`
	Verification string                     `json:"verification"`
	Notice       string                     `json:"notice"`
}

const localCheckpointNotice = "Checkpoint journal history may remain, including after purged-root delete; another purge does not prune it. Older readers may refuse that history; downgrade compatibility is unsupported."

var localCheckpointID = regexp.MustCompile(`^ckpt_[a-z2-7]{16}$`)

func localCheckpointIdentity(s string) bool {
	return len(s) <= 64 && strings.Trim(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-") == ""
}

func localCheckpointParse(args []string) (localCheckpointRequest, string) {
	r := localCheckpointRequest{timeout: 30 * time.Second}
	if len(args) == 0 {
		return r, "local_checkpoint_command_disabled"
	}
	r.operation = args[0]
	f := flag.NewFlagSet("checkpoint", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	mode, timeout := "", "30s"
	register := func(target *string, name string) {
		seen := false
		f.Func(name, "", func(value string) error {
			if seen {
				return errors.New("repeated flag")
			}
			seen, *target = true, value
			return nil
		})
	}
	register(&mode, "checkpoint-mode")
	register(&r.root, "root")
	register(&timeout, "timeout")
	switch r.operation {
	case "create":
		register(&r.pgbin, "pgbin")
		register(&r.installationID, "installation-id")
	case "delete":
		register(&r.checkpointID, "checkpoint-id")
	}
	err := f.Parse(args[1:])
	if mode != "cold-v1" {
		return r, "local_checkpoint_command_disabled"
	}
	if err != nil || f.NArg() != 0 || (r.operation != "create" && r.operation != "inspect" && r.operation != "delete") {
		return r, "local_checkpoint_arguments_invalid"
	}
	r.timeout, err = time.ParseDuration(timeout)
	if err != nil || r.timeout <= 0 || !filepath.IsAbs(r.root) || (r.operation == "create" && !filepath.IsAbs(r.pgbin)) {
		return r, "local_checkpoint_arguments_invalid"
	}
	if (r.operation == "create" && r.installationID == "") || (r.operation == "delete" && r.checkpointID == "") {
		return r, "local_checkpoint_confirmation_required"
	}
	if !localCheckpointIdentity(r.installationID) || (r.operation == "create" && (r.installationID == "latest" || r.installationID == "oldest" || r.installationID == "all")) || (r.operation == "delete" && !localCheckpointID.MatchString(r.checkpointID)) {
		return r, "local_checkpoint_arguments_invalid"
	}
	return r, ""
}

func localCheckpointSafeFailure(f localCheckpointFailure) *localCheckpointDiagnostic {
	d := &localCheckpointDiagnostic{Class: "local_checkpoint_operation_failed", CleanupIncomplete: f.cleanup}
	switch f.class {
	case "local_checkpoint_state_unsupported", "local_checkpoint_identity_mismatch", "local_checkpoint_source_unsupported", "local_checkpoint_storage_insufficient", "local_checkpoint_limit_reached", "local_checkpoint_incomplete", "local_checkpoint_witness_mismatch", "local_checkpoint_copy_failed", "local_maintenance_fence_active", "local_cell_locked", "local_checkpoint_interrupted", "local_cell_layout_refused":
		d.Class = f.class
	}
	switch f.reason {
	case "boundary_unreadable", "boundary_noncanonical", "boundary_not_private", "descendant_refused", "tree_unreadable", "unknown_entries":
		d.Reason = f.reason
	}
	return d
}

func localCheckpointRead(ctx context.Context, r localCheckpointRequest, cb localCheckpointCallbacks) *localCheckpointSnapshot {
	s := &localCheckpointSnapshot{Status: "unavailable"}
	if cb.read == nil {
		s.Failure = localCheckpointSafeFailure(localCheckpointFailure{})
		return s
	}
	o, f := cb.read(ctx, r)
	if f.err != nil {
		s.Failure = localCheckpointSafeFailure(f)
		return s
	}
	if o.status == "busy" || o.status == "unowned" {
		s.Status = o.status
		return s
	}
	valid := o.status == "listed" && o.unknown >= 0 && localCheckpointIdentity(o.installationID) && (o.installationID != "" || !o.installationPresent)
	for _, e := range o.entries {
		valid = valid && localCheckpointID.MatchString(e.id) && localCheckpointIdentity(e.installationID) && e.bytes >= 0 && e.created.Year() >= 1 && e.created.Year() <= 9999 && (!e.complete || (e.installationID != "" && !e.created.IsZero()))
	}
	if !valid {
		s.Failure = &localCheckpointDiagnostic{Class: "local_checkpoint_metadata_unsupported"}
		return s
	}
	s.Status, s.InstallationID, s.UnknownCount = "listed", o.installationID, &o.unknown
	n := len(o.entries)
	s.Count = &n
	for _, e := range o.entries {
		s.Entries = append(s.Entries, localCheckpointEntry{e.id, e.installationID, e.complete, "not_performed"})
	}
	return s
}

func localCheckpointCommand(ctx context.Context, args []string, cb localCheckpointCallbacks, stdout, stderr io.Writer) int {
	r, refusal := localCheckpointParse(args)
	receipt := localCheckpointReceipt{Operation: "unknown", Outcome: "refused", Verification: "not_performed", Notice: localCheckpointNotice}
	finish := func(code int) int {
		if json.NewEncoder(stdout).Encode(receipt) != nil {
			_, _ = io.WriteString(stderr, "local_checkpoint_operation_failed\n")
			return 1
		}
		return code
	}
	if r.operation == "create" || r.operation == "inspect" || r.operation == "delete" {
		receipt.Operation = r.operation
	}
	if refusal != "" {
		receipt.Failure = &localCheckpointDiagnostic{Class: refusal}
		return finish(2)
	}
	if r.operation == "delete" {
		receipt.Target, receipt.Presence = r.checkpointID, "unknown"
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	if r.operation == "inspect" {
		receipt.After = localCheckpointRead(ctx, r, cb)
		if receipt.After.Status != "listed" {
			receipt.Failure = receipt.After.Failure
			return finish(1)
		}
		receipt.Outcome = "succeeded"
		return finish(0)
	}
	receipt.Before = localCheckpointRead(ctx, r, cb)
	if receipt.Before.Status != "listed" {
		receipt.Failure = receipt.Before.Failure
		return finish(1)
	}
	if r.operation == "create" && r.installationID != receipt.Before.InstallationID {
		receipt.Failure = &localCheckpointDiagnostic{Class: "local_checkpoint_identity_mismatch"}
		return finish(2)
	}
	mutate := cb.create
	if r.operation == "delete" {
		mutate = cb.delete
	}
	if mutate == nil {
		receipt.Failure = localCheckpointSafeFailure(localCheckpointFailure{})
		return finish(1)
	}
	failure := mutate(ctx, r)
	receipt.Outcome = "succeeded"
	code := 0
	if failure.err != nil {
		receipt.Outcome, receipt.Failure, code = "failed", localCheckpointSafeFailure(failure), 1
	}
	// Separate ownership acquisitions make this an observation, never attribution.
	receipt.After = localCheckpointRead(ctx, r, cb)
	if receipt.After.Status == "busy" || receipt.After.Status == "unowned" || (receipt.After.Failure != nil && receipt.After.Failure.Class == "local_checkpoint_metadata_unsupported") {
		code = 1
	}
	if r.operation == "delete" && receipt.After.Status == "listed" {
		receipt.Presence = "absent"
		for _, e := range receipt.After.Entries {
			if e.ID == r.checkpointID {
				receipt.Presence = "present"
			}
		}
	}
	return finish(code)
}
