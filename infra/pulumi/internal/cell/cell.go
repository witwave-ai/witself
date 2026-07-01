// Package cell is the Witself cell: the inline Pulumi program that
// witself-infra provisions. A cell is one complete, isolated Witself stack in a
// single cloud account/region (witself-server + Postgres + pgvector, the
// sealed-plane KMS, object storage, and ingress). The same program provisions a
// self-hoster's single cell and each cell in the Witself Cloud fleet; what
// differs is who runs it (a human vs CI) and the stack config, never the code.
//
// The cell's identity is its Pulumi stack name (ctx.Stack()), composed by the CLI
// as <cloud>-<account-alias>-<region-code>-<role> (e.g. aws-sandbox-usw2-dev).
// That name is threaded into every resource as witself-<cell>-* plus a set of
// witself:* tags applied fleet-wide via the provider defaultTags.
//
// Build order, one slice at a time:
//
//	slice 1 — [done] module + CLI + Automation API loop
//	slice 2 — [done] AWS substrate: dedicated VPC + RDS Postgres (private subnets)
//	slice 3 — install the OCI chart (oci://ghcr.io/witwave-ai/charts/witself-server)
//	slice 4 — ingress modes: cloudflare-tunnel | alb | none
//	slice 5 — sealed-plane KMS (prod profile), IRSA, NAT/egress, GCP
package cell

import (
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// awsCell carries the cell's identity + placement into the AWS provisioning code,
// where it becomes the provider defaultTags and the resource name prefix.
type awsCell struct {
	name              string // composed cell name (= ctx.Stack())
	profile           string // minimal | prod
	cidr              string // VPC CIDR (/16)
	accountAlias      string // free-text account label
	region            string // real region, e.g. us-west-2
	role              string // dev | prod | canary | ordinal
	k8sVersion        string // EKS Kubernetes version
	dbVersion         string // RDS PostgreSQL major version
	argocd            bool   // install Argo CD (GitOps control plane) into the cluster
	gitopsRepo        string // GitOps repo URL Argo's root app reconciles
	gitopsPath        string // path in the repo for the root bootstrap chart
	gitopsValuesPath  string // path in the repo for this cell's bootstrap values
	gitopsRevision    string // repo revision (branch/tag)
	bootstrapToken    pulumi.StringOutput
	bootstrapTokenSet bool
}

// Program is the inline Pulumi program — the embedded Automation API engine runs
// this closure, so the cell definition is compiled into the witself-infra binary.
func Program(ctx *pulumi.Context) error {
	cellName := ctx.Stack() // the composed cell name: cloud-account-region-role

	w := config.New(ctx, "witself")
	a := config.New(ctx, "aws")

	cloud := w.Get("cloud")     // aws | gcp | azure
	profile := w.Get("profile") // minimal | prod
	ingress := w.Get("ingress") // cloudflare-tunnel | alb | none
	cidr := w.Get("cidr")
	if cidr == "" {
		cidr = "10.20.0.0/16"
	}
	k8sVersion := w.Get("k8sVersion")
	if k8sVersion == "" {
		k8sVersion = "1.36"
	}
	dbVersion := w.Get("dbVersion")
	if dbVersion == "" {
		dbVersion = "18"
	}
	argocd := w.GetBool("argocd")
	gitopsRepo := w.Get("gitopsRepo")
	if gitopsRepo == "" {
		gitopsRepo = DefaultGitopsRepo
	}
	gitopsPath := w.Get("gitopsPath")
	if gitopsPath == "" {
		gitopsPath = DefaultGitopsPath
	}
	gitopsValuesPath := w.Get("gitopsValuesPath")
	if gitopsValuesPath == "" {
		gitopsValuesPath = DefaultGitopsValuesPath(cellName)
	}
	gitopsRevision := w.Get("gitopsRevision")
	if gitopsRevision == "" {
		gitopsRevision = DefaultGitopsRevision
	}
	_, bootstrapTokenErr := w.Try("bootstrapToken")
	bootstrapTokenSet := bootstrapTokenErr == nil

	ctx.Export("cell", pulumi.String(cellName))
	ctx.Export("cloud", pulumi.String(cloud))
	ctx.Export("profile", pulumi.String(profile))
	ctx.Export("ingress", pulumi.String(ingress))

	switch cloud {
	case "", "aws":
		return provisionAWS(ctx, awsCell{
			name:              cellName,
			profile:           profile,
			cidr:              cidr,
			accountAlias:      w.Get("accountAlias"),
			region:            a.Get("region"),
			role:              w.Get("role"),
			k8sVersion:        k8sVersion,
			dbVersion:         dbVersion,
			argocd:            argocd,
			gitopsRepo:        gitopsRepo,
			gitopsPath:        gitopsPath,
			gitopsValuesPath:  gitopsValuesPath,
			gitopsRevision:    gitopsRevision,
			bootstrapToken:    w.GetSecret("bootstrapToken"),
			bootstrapTokenSet: bootstrapTokenSet,
		})
	default:
		ctx.Export("status", pulumi.String("cloud "+cloud+" not implemented yet — no resources"))
		return nil
	}
}
