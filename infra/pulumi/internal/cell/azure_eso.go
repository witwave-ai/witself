package cell

import (
	"fmt"

	authorization "github.com/pulumi/pulumi-azure-native-sdk/authorization/v3"
	keyvault "github.com/pulumi/pulumi-azure-native-sdk/keyvault/v3"
	managedidentity "github.com/pulumi/pulumi-azure-native-sdk/managedidentity/v3"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	azureESONamespace                = "external-secrets"
	azureESOServiceAccount           = "external-secrets"
	azureWorkloadIdentityAudience    = "api://AzureADTokenExchange"
	azureESOFederatedCredentialName  = "external-secrets"
	azureESOKubernetesServiceAccount = azureESONamespace + "/" + azureESOServiceAccount
)

func provisionAzureESOWorkloadIdentity(ctx *pulumi.Context, c azureCell, net *azureNetwork, aks *azureKubernetes, secrets *azureSecrets) (*azureESO, error) {
	client, err := authorization.GetClientConfig(ctx)
	if err != nil {
		return nil, err
	}

	identityName := rname(c.name, "eso")
	identity, err := managedidentity.NewUserAssignedIdentity(ctx, "cell-eso", &managedidentity.UserAssignedIdentityArgs{
		ResourceGroupName: net.resourceGroupName,
		ResourceName:      pulumi.String(identityName),
		Location:          pulumi.String(c.region),
		Tags:              azureResourceTags(c, identityName, "external-secrets"),
	})
	if err != nil {
		return nil, err
	}

	policy, err := keyvault.NewAccessPolicy(ctx, "cell-eso-secrets", &keyvault.AccessPolicyArgs{
		ResourceGroupName: net.resourceGroupName,
		VaultName:         secrets.vaultName,
		Policy: keyvault.AccessPolicyEntryArgs{
			TenantId: pulumi.String(client.TenantId),
			ObjectId: identity.PrincipalId,
			Permissions: keyvault.PermissionsArgs{
				Secrets: pulumi.StringArray{
					pulumi.String("get"),
					pulumi.String("list"),
				},
			},
		},
	}, pulumi.DependsOn([]pulumi.Resource{secrets.vault, identity}))
	if err != nil {
		return nil, err
	}

	issuer := aks.oidcIssuerURL.ApplyT(func(v *string) (string, error) {
		if v == nil || *v == "" {
			return "", fmt.Errorf("AKS OIDC issuer URL is empty; workload identity must be enabled")
		}
		return *v, nil
	}).(pulumi.StringOutput)

	credential, err := managedidentity.NewFederatedIdentityCredential(ctx, "cell-eso-federated", &managedidentity.FederatedIdentityCredentialArgs{
		ResourceGroupName:                       net.resourceGroupName,
		ResourceName:                            identity.Name,
		FederatedIdentityCredentialResourceName: pulumi.String(azureESOFederatedCredentialName),
		Issuer:                                  issuer,
		Subject:                                 pulumi.String("system:serviceaccount:" + azureESONamespace + ":" + azureESOServiceAccount),
		Audiences:                               pulumi.StringArray{pulumi.String(azureWorkloadIdentityAudience)},
	}, pulumi.DependsOn([]pulumi.Resource{aks.cluster, identity}))
	if err != nil {
		return nil, err
	}

	return &azureESO{
		identityName:             identity.Name,
		identityID:               identity.ID(),
		clientID:                 identity.ClientId,
		principalID:              identity.PrincipalId,
		tenantID:                 pulumi.String(client.TenantId).ToStringOutput(),
		federatedCredentialName:  credential.Name,
		kubernetesServiceAccount: azureESOKubernetesServiceAccount,
		dependencies:             []pulumi.Resource{policy, credential},
	}, nil
}
