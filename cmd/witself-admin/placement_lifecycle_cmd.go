package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/tokenfile"
)

func placementLifecycleCmd(args []string) int {
	fs := flag.NewFlagSet("placement lifecycle", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "control-plane URL")
	accountID := fs.String("account-id", "", "account ID")
	tokenPath := fs.String("token-file", "", "fleet token file")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || !cellMoveAccountID.MatchString(*accountID) {
		fmt.Fprintln(os.Stderr, "usage: witself-admin placement lifecycle --account-id ID [--endpoint URL] [--token-file PATH] [--json]")
		return 2
	}
	var token string
	var err error
	explicitFile := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "token-file" {
			explicitFile = true
		}
	})
	if explicitFile {
		token, err = tokenfile.Read(*tokenPath, tokenfile.Options{Description: "fleet token file"})
	} else {
		token, err = resolveFleetToken("")
	}
	if err != nil || strings.TrimSpace(token) == "" {
		fmt.Fprintln(os.Stderr, "fleet token unavailable; use --token-file or the managed fleet token")
		return 2
	}
	result, err := client.GetFleetAccountLifecycle(context.Background(), cpEndpoint(*endpoint), token, *accountID)
	if err != nil {
		message := "lifecycle status request failed; inspect control-plane diagnostics"
		if errors.Is(err, client.ErrUnauthorized) {
			message = "not authorized (check the fleet token)"
		}
		if errors.Is(err, client.ErrNotFound) {
			message = "account not found, or the control plane predates the lifecycle status route"
		}
		fmt.Fprintln(os.Stderr, message)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"account_lifecycle": result})
	}
	printLifecycleTable(result)
	return 0
}

func lifecycleScalar[T ~string | ~bool | ~int64](value *T) string {
	if value == nil {
		return "-"
	}
	return fmt.Sprint(*value)
}
func lifecycleTime(value *time.Time) string {
	if value == nil {
		return "-"
	}
	return value.UTC().Format(time.RFC3339)
}
func printLifecycleTable(r *client.FleetAccountLifecycle) {
	w, flush := tableWriter("field\tvalue")
	defer flush()
	row := func(field, value string) { _, _ = fmt.Fprintf(w, "%s\t%s\n", tabSafe(field), tabSafe(value)) }
	row("account", r.AccountID)
	row("initialized", lifecycleScalar(r.Initialized))
	location := "-"
	if r.Location != nil {
		location = r.Location.Kind
		if r.Location.Cell != nil {
			location += " " + *r.Location.Cell
		}
	}
	row("location", location)
	directory := "unavailable"
	if r.Directory != nil {
		parts := []string{}
		if r.Directory.LiveCell != nil {
			parts = append(parts, "live "+*r.Directory.LiveCell)
		}
		if r.Directory.ArchivedCell != nil {
			parts = append(parts, "archived "+*r.Directory.ArchivedCell)
		}
		directory = strings.Join(parts, "; ")
		if directory == "" {
			directory = "none"
		}
	}
	row("directory", directory)
	row("epoch", lifecycleScalar(r.Epoch))
	row("revision", lifecycleScalar(r.Revision))
	driver := "idle"
	if r.DriverActive != nil && *r.DriverActive {
		driver = "active"
	}
	row("driver", driver)
	row("alarm", lifecycleTime(r.AlarmAt))
	op := r.Operation
	if op == nil {
		row("operation", "none")
	} else {
		row("operation", op.Kind+" "+op.OperationID)
		row("evacuation", lifecycleScalar(op.EvacuationID))
		row("phase", op.Phase)
		row("cells", op.SourceCell+" -> "+lifecycleScalar(op.TargetCell))
		row("target protocol", lifecycleScalar(op.TargetProtocol))
		step := "-"
		if op.NextStep != nil {
			step = lifecycleScalar(op.NextStep.Type) + " " + lifecycleScalar(op.NextStep.Action)
			if op.NextStep.Target != nil {
				step += " " + *op.NextStep.Target
			}
		}
		row("next step", step)
		row("retryable", lifecycleScalar(op.Retryable))
		row("last error", lifecycleScalar(op.LastError))
	}
	export := "none"
	if op != nil && op.ExportJob != nil {
		j := op.ExportJob
		streamed := "no"
		if j.Streamed {
			streamed = "yes"
		}
		export = "stream attempt " + lifecycleScalar(j.StreamAttempts) + "; verify attempt " + lifecycleScalar(j.VerifyAttempts) + "; streamed " + streamed + " at " + lifecycleTime(j.StreamedAt) + "; stream ms " + lifecycleScalar(j.StreamMS)
	}
	row("export job", export)
	job := "none"
	if op != nil && op.ImportJob != nil {
		j := op.ImportJob
		job = "attempt " + lifecycleScalar(j.Attempts) + "; next alarm action " + j.NextAlarmAction + "; retry at " + lifecycleTime(j.RetryAt) + "; first started " + lifecycleTime(j.FirstStartedAt) + "; started " + lifecycleTime(j.StartedAt) + "; last polled " + lifecycleTime(j.LastPolledAt)
	}
	row("import job", job)
	completed := "none"
	if c := r.LastCompleted; c != nil {
		completed = c.Kind + " " + c.Outcome + " " + c.OperationID + " at revision " + lifecycleScalar(c.CompletedRevision)
	}
	row("last completed", completed)
	quarantine := "none"
	if q := r.RestoreQuarantine; q != nil {
		quarantine = q.Reason + " at " + lifecycleTime(q.QuarantinedAt) + "; matches operation " + strconv.FormatBool(q.MatchesOperation) + "; matches location " + strconv.FormatBool(q.MatchesLocation)
	}
	row("quarantine", quarantine)
	row("observed", lifecycleTime(r.ObservedAt))
	if op != nil && op.Retryable != nil && !*op.Retryable && op.LastError != nil {
		row("attention", "needs operator")
	}
}
