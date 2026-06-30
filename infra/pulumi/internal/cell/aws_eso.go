package cell

import (
	"fmt"

	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/eks"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/iam"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// The External Secrets Operator (installed via GitOps, see .gitops/bootstrap)
// authenticates to AWS with EKS Pod Identity — preferred over IRSA: no OIDC
// provider, no ServiceAccount annotations, and EKS Auto Mode ships the Pod
// Identity Agent built-in, so there is nothing extra to install. The association
// binds ESO's ServiceAccount to an IAM role directly.
const (
	esoNamespace      = "external-secrets"
	esoServiceAccount = "external-secrets"

	// Pod Identity trust: the EKS Pod Identity service assumes the role for the
	// associated ServiceAccount's pods.
	esoPodIdentityTrust = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"pods.eks.amazonaws.com"},"Action":["sts:AssumeRole","sts:TagSession"]}]}`
)

// provisionAWSESOPodIdentity creates the IAM role the External Secrets Operator
// assumes — scoped to read only THIS cell's secrets under witself/<cell>/* — and
// binds it to ESO's ServiceAccount via an EKS Pod Identity association, so ESO
// pulls from AWS Secrets Manager with no static credentials.
//
// The association is just a mapping registered with EKS; it does NOT require ESO
// to be installed yet (Argo installs ESO asynchronously from .gitops), and the
// cluster need only exist. A pod picks up the credentials when it starts, so an
// already-running ESO must be restarted once after the association is created.
func provisionAWSESOPodIdentity(ctx *pulumi.Context, c awsCell, cluster *awsEKS, prov *aws.Provider) error {
	caller, err := aws.GetCallerIdentity(ctx, nil, pulumi.Provider(prov))
	if err != nil {
		return err
	}

	// Least privilege: only this cell's secret prefix. Secrets Manager appends a
	// random 6-char suffix to secret ARNs, which the trailing * also covers.
	secretArn := fmt.Sprintf("arn:aws:secretsmanager:%s:%s:secret:witself/%s/*", c.region, caller.AccountId, c.name)
	policyDoc := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["secretsmanager:GetSecretValue","secretsmanager:DescribeSecret","secretsmanager:ListSecretVersionIds"],"Resource":"%s"}]}`, secretArn)

	role, err := iam.NewRole(ctx, "cell-eso", &iam.RoleArgs{
		Name:             pulumi.String(rname(c.name, "eso")),
		AssumeRolePolicy: pulumi.String(esoPodIdentityTrust),
		Tags:             resourceTags(rname(c.name, "eso"), "external-secrets"),
	}, pulumi.Provider(prov))
	if err != nil {
		return err
	}

	if _, err := iam.NewRolePolicy(ctx, "cell-eso", &iam.RolePolicyArgs{
		Name:   pulumi.String(rname(c.name, "eso-secrets")),
		Role:   role.ID(),
		Policy: pulumi.String(policyDoc),
	}, pulumi.Provider(prov)); err != nil {
		return err
	}

	if _, err := eks.NewPodIdentityAssociation(ctx, "cell-eso", &eks.PodIdentityAssociationArgs{
		ClusterName:    cluster.name,
		Namespace:      pulumi.String(esoNamespace),
		ServiceAccount: pulumi.String(esoServiceAccount),
		RoleArn:        role.Arn,
		Tags:           resourceTags(rname(c.name, "eso"), "external-secrets"),
	}, pulumi.Provider(prov)); err != nil {
		return err
	}

	ctx.Export("esoRole", role.Arn)
	ctx.Export("esoSecretsPrefix", pulumi.String(fmt.Sprintf("witself/%s/", c.name)))
	return nil
}
