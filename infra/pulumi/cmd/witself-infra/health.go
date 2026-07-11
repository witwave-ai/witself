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
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/rds"
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

// subsystemHealth is one line of the report. Have/Total are optional
// counts (ready nodes / total nodes, healthy apps / total apps) that
// the dashboard renders as a gauge bar; both zero means "no gauge".
type subsystemHealth struct {
	Level  healthLevel `json:"level"`
	Detail string      `json:"detail"`
	Have   int         `json:"have,omitempty"`
	Total  int         `json:"total,omitempty"`
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
	clusterNodes(ctx context.Context) (total, ready int, ok bool)
}

// printCellHealth reads the stack's outputs, probes each subsystem it
// can reach, and prints the JSON report to stdout. It never fails the
// process for a probe that couldn't run — an unreachable subsystem is
// data (level bad/unknown), not an error — so the dashboard always
// gets a parseable report.
func printCellHealth(ctx context.Context, stack auto.Stack, cloud, region, awsProfile string, argocd bool) error {
	report := cellHealthReport{
		Kubernetes: sh(healthUnknown, "not probed"),
		Database:   sh(healthUnknown, "not probed"),
		Argo:       sh(healthUnknown, "argocd not enabled for this cell"),
	}

	outs, err := stack.Outputs(ctx)
	if err != nil {
		report.Kubernetes = sh(healthBad, "read stack outputs: "+oneLine(err.Error()))
		return emitHealth(report)
	}

	// Database status — available for every cloud (provider status API),
	// independent of the cluster path.
	report.Database = probeDatabase(ctx, cloud, region, awsProfile, outs)

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
		report.Kubernetes = sh(healthUnknown, "cluster probe not yet wired for "+cloud)
		return emitHealth(report)
	}
	if err != nil {
		report.Kubernetes = sh(healthBad, "build cluster client: "+oneLine(err.Error()))
		return emitHealth(report)
	}

	// Kubernetes: apiserver readiness, with a node ready/total gauge.
	k8s := sh(healthGood, "apiserver ready")
	if ready, why := prober.clusterReadyz(ctx); !ready {
		if why == "" {
			k8s = sh(healthBad, "apiserver unreachable")
		} else {
			k8s = sh(healthDegraded, oneLine(why))
		}
	}
	if total, nready, ok := prober.clusterNodes(ctx); ok {
		k8s.Have, k8s.Total = nready, total
		if k8s.Level == healthGood {
			// Cluster is up but a node is down → degrade and say so. The
			// gauge already carries the ratio, so the detail is
			// complementary, not a repeat of the count.
			if nready < total {
				k8s.Level = healthDegraded
				k8s.Detail = fmt.Sprintf("%d node(s) not Ready", total-nready)
			} else {
				k8s.Detail = "apiserver ready · all nodes up"
			}
		}
	}
	report.Kubernetes = k8s

	// Workloads: Argo application health (only when the cell runs Argo).
	if argocd {
		apps, aerr := prober.ListArgoApplications(ctx, namespace)
		if aerr != nil {
			report.Argo = sh(healthBad, oneLine(aerr.Error()))
		} else {
			report.Argo = argoHealth(apps)
		}
	}

	return emitHealth(report)
}

// sh is a keyed-literal shorthand for a countless subsystem line.
func sh(l healthLevel, detail string) subsystemHealth {
	return subsystemHealth{Level: l, Detail: detail}
}

func emitHealth(r cellHealthReport) error {
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(r)
}

// probeDatabase queries the managed-database provider for its instance
// status via the cloud CLI (the same tools whoami / provisioning
// already rely on for auth), then maps the provider's status string to
// a health level. The raw status rides along as the detail.
func probeDatabase(ctx context.Context, cloud, region, awsProfile string, outs auto.OutputMap) subsystemHealth {
	id := outputString(outs, "dbInstance")
	if id == "" {
		return sh(healthUnknown, "no dbInstance output — cannot query status")
	}
	switch cloud {
	case "aws":
		// Use the RDS SDK with the cell's profile — the same credential
		// path whoami uses (and which works) — NOT the `aws` CLI, which
		// read the ambient environment without the cell's profile and hit
		// an InvalidClientTokenId on the wrong/absent credentials.
		status, err := awsRDSStatus(ctx, region, awsProfile, id)
		if err != nil {
			return sh(healthBad, "rds status query failed: "+oneLine(err.Error()))
		}
		return dbLevel(status, awsDBLevel)
	case "gcp":
		// Use the Cloud SQL Admin REST API with the ADC token — the same
		// credential the cluster probe uses — NOT `gcloud sql describe`,
		// which authenticates with gcloud's separate user login that goes
		// stale independently (and did: "problem refreshing auth tokens").
		project := outputString(outs, "gcpProject")
		state, err := gcpCloudSQLState(ctx, project, id)
		if err != nil {
			return sh(healthBad, "cloud sql status query failed: "+oneLine(err.Error()))
		}
		return dbLevel(state, gcpDBLevel)
	case "azure":
		rg := outputString(outs, "resourceGroup")
		status, err := dbStatusCmd(ctx, "az", "postgres", "flexible-server", "show",
			"--name", id, "--resource-group", rg, "--query", "state", "-o", "tsv")
		if err != nil {
			return sh(healthBad, "postgres status query failed: "+err.Error())
		}
		return dbLevel(status, azureDBLevel)
	}
	return sh(healthUnknown, "database status not wired for "+cloud)
}

// awsRDSStatus reads one RDS instance's DBInstanceStatus via the SDK,
// loading config with the cell's profile and region — mirroring the
// working whoami path. Threading the profile is the whole point: the
// old CLI call inherited the ambient environment (no profile) and
// failed with InvalidClientTokenId.
func awsRDSStatus(ctx context.Context, region, profile, id string) (string, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	if profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return "", fmt.Errorf("load AWS config: %w", err)
	}
	out, err := rds.NewFromConfig(cfg).DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: &id,
	})
	if err != nil {
		return "", err
	}
	if len(out.DBInstances) == 0 || out.DBInstances[0].DBInstanceStatus == nil {
		return "", fmt.Errorf("no status reported for instance %q", id)
	}
	return *out.DBInstances[0].DBInstanceStatus, nil
}

// gcpCloudSQLState queries the Cloud SQL Admin API for one instance's
// state (RUNNABLE / MAINTENANCE / SUSPENDED / …) using the ADC token —
// the application-default credential, which is the one the cluster
// probe already authenticates with and which survives independently of
// `gcloud auth login`. Reusing it avoids the CLI's stale-user-token
// failure mode.
func gcpCloudSQLState(ctx context.Context, project, instance string) (string, error) {
	if project == "" {
		return "", fmt.Errorf("no gcpProject output — cannot address the instance")
	}
	token, err := gcpADCAccessToken(ctx, project)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("https://sqladmin.googleapis.com/v1/projects/%s/instances/%s?fields=state", project, instance)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cloud SQL Admin API HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode Cloud SQL response: %w", err)
	}
	return out.State, nil
}

// dbStatusCmd runs a cloud-CLI status query and returns the trimmed
// stdout. On failure it returns the trimmed stderr (or the error) so
// the Health line can show something actionable.
func dbStatusCmd(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = os.Environ()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return "", fmt.Errorf("%s", oneLine(detail))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// dbLevel wraps a per-cloud status→level mapper: an empty status is
// unknown, otherwise the mapper decides and the raw status is the
// detail.
func dbLevel(status string, mapper func(string) healthLevel) subsystemHealth {
	if status == "" {
		return sh(healthUnknown, "empty status")
	}
	return subsystemHealth{Level: mapper(status), Detail: status}
}

// awsDBLevel maps an RDS DBInstanceStatus to a level.
// https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/accessing-monitoring.html#Overview.DBInstance.Status
func awsDBLevel(status string) healthLevel {
	switch strings.ToLower(status) {
	case "available":
		return healthGood
	case "backing-up", "maintenance", "modifying", "upgrading", "starting",
		"configuring-enhanced-monitoring", "configuring-iam-database-auth",
		"configuring-log-exports", "converting-to-vpc", "renaming",
		"resetting-master-credentials", "storage-optimization", "rebooting":
		return healthWarn
	case "storage-full", "insufficient-capacity", "incompatible-parameters",
		"incompatible-option-group", "incompatible-restore":
		return healthDegraded
	case "stopped", "stopping", "failed", "deleting",
		"inaccessible-encryption-credentials", "restore-error":
		return healthBad
	default:
		return healthUnknown
	}
}

// gcpDBLevel maps a Cloud SQL instance state to a level.
func gcpDBLevel(status string) healthLevel {
	switch strings.ToUpper(status) {
	case "RUNNABLE":
		return healthGood
	case "MAINTENANCE", "PENDING_CREATE", "PENDING_DELETE":
		return healthWarn
	case "SUSPENDED", "FAILED":
		return healthBad
	default:
		return healthUnknown
	}
}

// azureDBLevel maps a Postgres Flexible Server state to a level.
func azureDBLevel(status string) healthLevel {
	switch strings.ToLower(status) {
	case "ready":
		return healthGood
	case "updating", "starting", "provisioning", "restarting":
		return healthWarn
	case "stopped", "stopping":
		return healthDegraded
	case "disabled", "dropping", "inaccessible":
		return healthBad
	default:
		return healthUnknown
	}
}

// argoHealth collapses the per-application Argo status into one line,
// with a healthy/total gauge. A Degraded or Missing app is bad;
// anything merely in flight (Progressing, OutOfSync) is a warning; all
// Synced+Healthy is good. Worst status wins, and the detail names the
// offenders.
func argoHealth(apps []argoApplication) subsystemHealth {
	if len(apps) == 0 {
		return sh(healthUnknown, "no Argo applications reported")
	}
	worst := healthGood
	healthy := 0
	var notes []string
	for _, app := range apps {
		sync := app.Status.Sync.Status
		health := app.Status.Health.Status
		if sync == "Synced" && health == "Healthy" {
			healthy++
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
	out := subsystemHealth{Level: worst, Have: healthy, Total: len(apps)}
	if worst == healthGood {
		// Gauge shows N/N; detail stays complementary.
		out.Detail = "all Synced/Healthy"
	} else {
		out.Detail = strings.Join(notes, "; ")
	}
	return out
}
