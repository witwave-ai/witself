package main

// `witself-infra cell-health -cell X` — a read-only health probe the
// dashboard's Health tab runs as a subprocess. It reuses the same
// stack-outputs + Argo lister machinery the post-up convergence wait
// uses, but takes a single reading (not a poll loop) and prints a JSON
// report of each subsystem's level + detail.
//
// Scope today: Kubernetes (apiserver /readyz) and Workloads (Argo CD)
// for GCP and Azure, whose cluster credentials are exported as stack
// outputs. AWS EKS (no CA output — needs eks get-token) and Database
// status (per-cloud provider APIs) report "unknown" until wired.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
)

// healthNames is the wire form of each level — the JSON the CLI emits
// and the dashboard parses uses these strings, not the int values.
var healthNames = map[healthLevel]string{
	healthUnknown:  "unknown",
	healthGood:     "good",
	healthWarn:     "warn",
	healthDegraded: "degraded",
	healthBad:      "bad",
}

func healthLevelByName(s string) healthLevel {
	for lvl, name := range healthNames {
		if name == s {
			return lvl
		}
	}
	return healthUnknown
}

// MarshalJSON / UnmarshalJSON let a healthLevel ride the JSON report as
// its stable string name in either direction.
func (l healthLevel) MarshalJSON() ([]byte, error) {
	name, ok := healthNames[l]
	if !ok {
		name = "unknown"
	}
	return json.Marshal(name)
}

func (l *healthLevel) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*l = healthLevelByName(s)
	return nil
}

// subsystemHealth is one line of the report.
type subsystemHealth struct {
	Level  healthLevel `json:"level"`
	Detail string      `json:"detail"`
}

// cellHealthReport is the whole probe result — one entry per Health-tab
// line that needs a real probe (the credential and fleet lines are
// computed in-dashboard from data every refresh already carries).
type cellHealthReport struct {
	Kubernetes subsystemHealth `json:"kubernetes"`
	Database   subsystemHealth `json:"database"`
	Argo       subsystemHealth `json:"argo"`
}

// argoClusterProber is what both the GCP and Azure listers satisfy: an
// Argo application list plus a kube-apiserver readiness check over the
// same authenticated client.
type argoClusterProber interface {
	argoApplicationLister
	clusterReadyz(ctx context.Context) (bool, string)
}

// printCellHealth reads the stack's outputs, probes each subsystem it
// can reach, and prints the JSON report to stdout. It never fails the
// process for a probe that couldn't run — an unreachable subsystem is
// data (level bad/unknown), not an error — so the dashboard always
// gets a parseable report.
func printCellHealth(ctx context.Context, stack auto.Stack, cloud string, argocd bool) error {
	report := cellHealthReport{
		Kubernetes: subsystemHealth{healthUnknown, "not probed"},
		Database:   subsystemHealth{healthUnknown, "status probe not yet wired"},
		Argo:       subsystemHealth{healthUnknown, "argocd not enabled for this cell"},
	}

	outs, err := stack.Outputs(ctx)
	if err != nil {
		report.Kubernetes = subsystemHealth{healthBad, "read stack outputs: " + oneLine(err.Error())}
		return emitHealth(report)
	}

	var prober argoClusterProber
	var namespace string
	switch cloud {
	case "gcp":
		prober, namespace, err = newGCPArgoListerFromOutputs(outs)
	case "azure":
		prober, namespace, err = newAzureArgoListerFromOutputs(ctx, outs)
	default:
		// AWS EKS exports no CA/token for a direct apiserver call — that
		// path needs `aws eks get-token` and is a follow-up.
		report.Kubernetes = subsystemHealth{healthUnknown, "cluster probe not yet wired for " + cloud}
		return emitHealth(report)
	}
	if err != nil {
		report.Kubernetes = subsystemHealth{healthBad, "build cluster client: " + oneLine(err.Error())}
		return emitHealth(report)
	}

	// Kubernetes: apiserver readiness.
	if ready, why := prober.clusterReadyz(ctx); ready {
		report.Kubernetes = subsystemHealth{healthGood, "apiserver ready"}
	} else if why == "" {
		report.Kubernetes = subsystemHealth{healthBad, "apiserver unreachable"}
	} else {
		report.Kubernetes = subsystemHealth{healthDegraded, oneLine(why)}
	}

	// Workloads: Argo application health (only when the cell runs Argo).
	if argocd {
		apps, aerr := prober.ListArgoApplications(ctx, namespace)
		if aerr != nil {
			report.Argo = subsystemHealth{healthBad, oneLine(aerr.Error())}
		} else {
			lvl, detail := argoHealthLevel(apps)
			report.Argo = subsystemHealth{lvl, detail}
		}
	}

	return emitHealth(report)
}

func emitHealth(r cellHealthReport) error {
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(r)
}

// argoHealthLevel collapses the per-application Argo status into one
// level. A Degraded or Missing app is bad; anything merely in flight
// (Progressing, OutOfSync) is a warning; all Synced+Healthy is good.
// Worst status wins, and the detail names the offenders.
func argoHealthLevel(apps []argoApplication) (healthLevel, string) {
	if len(apps) == 0 {
		return healthUnknown, "no Argo applications reported"
	}
	worst := healthGood
	var notes []string
	for _, app := range apps {
		sync := app.Status.Sync.Status
		health := app.Status.Health.Status
		if sync == "Synced" && health == "Healthy" {
			continue
		}
		name := app.Metadata.Name
		if name == "" {
			name = "<unknown>"
		}
		if sync == "" {
			sync = "Unknown"
		}
		if health == "" {
			health = "Unknown"
		}
		notes = append(notes, fmt.Sprintf("%s %s/%s", name, sync, health))
		switch health {
		case "Degraded", "Missing":
			if worst < healthBad {
				worst = healthBad
			}
		default:
			if worst < healthDegraded {
				worst = healthDegraded
			}
		}
	}
	if worst == healthGood {
		return healthGood, fmt.Sprintf("%d applications Synced/Healthy", len(apps))
	}
	return worst, strings.Join(notes, "; ")
}
