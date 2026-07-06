package cell

import "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

// provisionGCP is the first GCP slice: a real Pulumi stack with no workload
// resources yet. The state backend and secrets provider are prepared outside the
// resource graph by internal/backend/gcp.go, so this gives us a safe lifecycle
// checkpoint before adding GKE, Cloud SQL, DNS, and GitOps.
func provisionGCP(ctx *pulumi.Context, c gcpCell) error {
	ctx.Export("status", pulumi.String("gcp: empty cell stack initialized"))
	ctx.Export("gcpProject", pulumi.String(c.project))
	ctx.Export("gcpRegion", pulumi.String(c.region))
	ctx.Export("accountAlias", pulumi.String(c.accountAlias))
	ctx.Export("role", pulumi.String(c.role))
	return nil
}
