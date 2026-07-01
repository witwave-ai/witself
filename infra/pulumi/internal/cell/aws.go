package cell

import (
	"fmt"

	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/rds"
	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// rname builds a witself-prefixed AWS resource name: witself-<cell>-<kind> (or
// witself-<cell> when kind is empty). <cell> is the composed cell name.
func rname(cell, kind string) string {
	if kind == "" {
		return "witself-" + cell
	}
	return "witself-" + cell + "-" + kind
}

// defaultTags are applied to EVERY cell resource through the AWS provider, so the
// cell's identity and placement are tagged uniformly without repeating them on
// each resource. Mutable, high-churn values (versions, shas, timestamps) are
// deliberately NOT here — they belong in stack outputs / the registry.
func defaultTags(c awsCell) pulumi.StringMap {
	return pulumi.StringMap{
		"witself:cell":          pulumi.String(c.name),
		"witself:cloud":         pulumi.String("aws"),
		"witself:account-alias": pulumi.String(c.accountAlias),
		"witself:region":        pulumi.String(c.region),
		"witself:role":          pulumi.String(c.role),
		"witself:profile":       pulumi.String(c.profile),
		"witself:managed-by":    pulumi.String("pulumi"),
		"app":                   pulumi.String("witself"),
	}
}

// resourceTags layer on top of the provider defaultTags: a human Name and the
// component kind. Provider defaultTags + these merge, with these winning ties.
func resourceTags(name, component string) pulumi.StringMap {
	return pulumi.StringMap{
		"Name":              pulumi.String(name),
		"witself:component": pulumi.String(component),
	}
}

// dbInstanceClass sizes the RDS instance for the profile. The minimal profile is
// the cheap, single-AZ dev box (~$0.016/hr); prod sizing is a later slice.
func dbInstanceClass(profile string) string {
	if profile == "prod" {
		return "db.t4g.small"
	}
	return "db.t4g.micro"
}

// provisionAWS provisions the cell's AWS substrate: its dedicated VPC (see
// aws_vpc.go) and an RDS PostgreSQL instance on a pgvector-capable engine, sitting
// privately in the cell's private subnets. Every resource is created through an
// explicit provider that stamps the cell's defaultTags. EKS, KMS, object storage,
// and ingress come in later slices.
func provisionAWS(ctx *pulumi.Context, c awsCell) error {
	minimal := c.profile != "prod"

	// Explicit provider carrying defaultTags so every resource is tagged uniformly.
	prov, err := aws.NewProvider(ctx, "aws", &aws.ProviderArgs{
		Region:      pulumi.String(c.region),
		DefaultTags: &aws.ProviderDefaultTagsArgs{Tags: defaultTags(c)},
	})
	if err != nil {
		return err
	}

	dns, err := provisionAWSDNS(ctx, c, prov)
	if err != nil {
		return err
	}
	if dns != nil {
		c.apiHost = dns.apiHost
		c.tlsCertificateARN = dns.ingressCertificateARN
	} else {
		c.tlsCertificateARN = pulumi.String("").ToStringOutput()
	}

	// The cell owns its network: a dedicated VPC with private subnets for the
	// database, so it never depends on a region's default VPC.
	net, err := provisionAWSNetwork(ctx, c, minimal, prov)
	if err != nil {
		return err
	}

	// EKS Auto Mode cluster: AWS manages the nodes, core add-ons, scaling, and
	// patching, so the cell's Kubernetes layer is low-maintenance.
	eksCluster, err := provisionAWSEKS(ctx, c, net, prov)
	if err != nil {
		return err
	}

	// GitOps control plane (opt-in): install Argo CD via Helm + wire it to the
	// cell bootstrap, AND create ESO's Pod Identity IAM so ESO can read this
	// cell's secrets — all on `up`. Universal: the upstream charts, not the
	// AWS-only managed capability.
	if c.argocd {
		if err := provisionAWSArgoCD(ctx, c, eksCluster); err != nil {
			return err
		}
		if err := provisionAWSESOPodIdentity(ctx, c, eksCluster, prov); err != nil {
			return err
		}
		if err := provisionAWSExternalDNSPodIdentity(ctx, c, eksCluster, dns, prov); err != nil {
			return err
		}
	}

	// Master password: generated, never hard-coded. RDS disallows /, @, ", and
	// spaces, so keep it alphanumeric. RandomPassword.Result is a secret output.
	pw, err := random.NewRandomPassword(ctx, "witself-db", &random.RandomPasswordArgs{
		Length:  pulumi.Int(24),
		Special: pulumi.Bool(false),
	})
	if err != nil {
		return err
	}

	db, err := rds.NewInstance(ctx, "witself", &rds.InstanceArgs{
		Identifier:          pulumi.String(rname(c.name, "db")),
		Engine:              pulumi.String("postgres"),
		EngineVersion:       pulumi.String(c.dbVersion), // pgvector-capable; RDS picks the latest minor
		InstanceClass:       pulumi.String(dbInstanceClass(c.profile)),
		AllocatedStorage:    pulumi.Int(20),
		StorageType:         pulumi.String("gp3"),
		StorageEncrypted:    pulumi.Bool(true),        // KMS-encrypted at rest (AWS-managed RDS key)
		DbName:              pulumi.String("witself"), // logical app DB stays "witself"
		Username:            pulumi.String("witself"),
		Password:            pw.Result,
		DbSubnetGroupName:   net.dbSubnetGroup,
		VpcSecurityGroupIds: pulumi.StringArray{net.dbSecurityGrp},
		MultiAz:             pulumi.Bool(!minimal),
		// Dev-friendly lifecycle so `destroy` is clean and leaves no billed
		// snapshot. The prod profile will flip these in a later slice.
		SkipFinalSnapshot:  pulumi.Bool(true),
		DeletionProtection: pulumi.Bool(false),
		PubliclyAccessible: pulumi.Bool(false),
		Tags:               resourceTags(rname(c.name, "db"), "database"),
		// The DB has an explicit Identifier, so a replacement (e.g. enabling
		// storage encryption) must delete the old instance before creating the new
		// one — otherwise the two collide on the name (DBInstanceAlreadyExists).
	}, pulumi.Provider(prov), pulumi.DeleteBeforeReplace(true))
	if err != nil {
		return err
	}

	// Convenience DSN; secret because it embeds the password.
	dsn := pulumi.All(db.Endpoint, pw.Result).ApplyT(func(a []interface{}) string {
		return fmt.Sprintf("postgres://witself:%s@%s/witself?sslmode=require", a[1], a[0])
	}).(pulumi.StringOutput)

	// Publish the DB connection to Secrets Manager as <cell>/db — the canonical
	// source ESO syncs into the cluster (ESO's Pod Identity role reads <cell>/*).
	if err := provisionAWSDBSecret(ctx, c, db, pw, prov); err != nil {
		return err
	}
	if err := provisionAWSBootstrapSecret(ctx, c, prov); err != nil {
		return err
	}

	ctx.Export("status", pulumi.String("aws: cell vpc + eks (auto mode) + rds postgres provisioned"))
	ctx.Export("vpcId", net.vpcID)
	ctx.Export("eksCluster", eksCluster.name)
	ctx.Export("eksEndpoint", eksCluster.endpoint)
	ctx.Export("dbEndpoint", db.Endpoint)
	ctx.Export("dbName", pulumi.String("witself"))
	ctx.Export("dbUsername", pulumi.String("witself"))
	ctx.Export("dbPassword", pw.Result) // secret
	ctx.Export("dbDSN", dsn)            // secret
	return nil
}
