package cell

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/secretmanager"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	gcpESONamespace      = "external-secrets"
	gcpESOServiceAccount = "external-secrets"
)

func gcpESOAccountID(c gcpCell) string {
	base := "ws-eso-" + c.name
	if len(base) <= 30 {
		return base
	}
	sum := sha1.Sum([]byte(c.name))
	return "ws-eso-" + hex.EncodeToString(sum[:])[:12]
}

func provisionGCPESOWorkloadIdentity(ctx *pulumi.Context, c gcpCell, secrets []gcpSecretAccess, prov *gcp.Provider, iamCredentialsAPI pulumi.Resource) error {
	gsa, err := serviceaccount.NewAccount(ctx, "cell-eso", &serviceaccount.AccountArgs{
		AccountId:   pulumi.String(gcpESOAccountID(c)),
		DisplayName: pulumi.String("Witself ESO for " + c.name),
		Description: pulumi.String("External Secrets Operator identity for " + c.name),
		Project:     pulumi.String(c.project),
	}, pulumi.Provider(prov), pulumi.DependsOn([]pulumi.Resource{iamCredentialsAPI}))
	if err != nil {
		return err
	}

	if _, err := serviceaccount.NewIAMMember(ctx, "cell-eso-workload-identity", &serviceaccount.IAMMemberArgs{
		ServiceAccountId: gsa.Name,
		Role:             pulumi.String("roles/iam.workloadIdentityUser"),
		Member:           pulumi.String(fmt.Sprintf("serviceAccount:%s.svc.id.goog[%s/%s]", c.project, gcpESONamespace, gcpESOServiceAccount)),
	}, pulumi.Provider(prov)); err != nil {
		return err
	}

	for _, secret := range secrets {
		if _, err := secretmanager.NewSecretIamMember(ctx, "cell-eso-"+secret.resourceName+"-secret", &secretmanager.SecretIamMemberArgs{
			SecretId: secret.secretID,
			Project:  pulumi.String(c.project),
			Role:     pulumi.String("roles/secretmanager.secretAccessor"),
			Member:   pulumi.Sprintf("serviceAccount:%s", gsa.Email),
		}, pulumi.Provider(prov)); err != nil {
			return err
		}
	}

	ctx.Export("esoServiceAccountEmail", gsa.Email)
	ctx.Export("esoKubernetesServiceAccount", pulumi.String(gcpESONamespace+"/"+gcpESOServiceAccount))
	return nil
}
