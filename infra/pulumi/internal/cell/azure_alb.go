package cell

import (
	"fmt"

	authorization "github.com/pulumi/pulumi-azure-native-sdk/authorization/v3"
	managedidentity "github.com/pulumi/pulumi-azure-native-sdk/managedidentity/v3"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	azureAppGwForContainersConfigManagerRoleID = "fbc52c3f-28ad-4303-a892-8a056630b8f1"
	azureALBControllerNamespace                = "azure-alb-system"
	azureALBControllerServiceAccount           = "alb-controller-sa"
	azureALBControllerFederatedCredential      = "alb-controller"
	azureALBControllerKubernetesServiceAccount = azureALBControllerNamespace + "/" + azureALBControllerServiceAccount
)

func provisionAzureALBControllerIdentity(ctx *pulumi.Context, c azureCell, net *azureNetwork, aks *azureKubernetes) (*azureALBController, error) {
	client, err := authorization.GetClientConfig(ctx)
	if err != nil {
		return nil, err
	}

	identityName := rname(c.name, "alb-controller")
	identity, err := managedidentity.NewUserAssignedIdentity(ctx, "cell-alb-controller", &managedidentity.UserAssignedIdentityArgs{
		ResourceGroupName: net.resourceGroupName,
		ResourceName:      pulumi.String(identityName),
		Location:          pulumi.String(c.region),
		Tags:              azureResourceTags(c, identityName, "ingress"),
	})
	if err != nil {
		return nil, err
	}

	nodeResourceGroupScope := aks.nodeResourceGroup.ApplyT(func(v *string) (string, error) {
		if v == nil || *v == "" {
			return "", fmt.Errorf("AKS node resource group is empty")
		}
		return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s", client.SubscriptionId, *v), nil
	}).(pulumi.StringOutput)

	readerRole, err := azureRoleAssignment(ctx, "cell-alb-controller-node-rg-reader", identity.PrincipalId, nodeResourceGroupScope, client.SubscriptionId, azureReaderRoleID)
	if err != nil {
		return nil, err
	}
	configManagerRole, err := azureRoleAssignment(ctx, "cell-alb-controller-config-manager", identity.PrincipalId, nodeResourceGroupScope, client.SubscriptionId, azureAppGwForContainersConfigManagerRoleID)
	if err != nil {
		return nil, err
	}
	subnetContributorRole, err := azureRoleAssignment(ctx, "cell-alb-controller-subnet", identity.PrincipalId, net.albSubnetID.ToStringOutput(), client.SubscriptionId, azureNetworkContributorRoleID)
	if err != nil {
		return nil, err
	}

	issuer := aks.oidcIssuerURL.ApplyT(func(v *string) (string, error) {
		if v == nil || *v == "" {
			return "", fmt.Errorf("AKS OIDC issuer URL is empty; workload identity must be enabled")
		}
		return *v, nil
	}).(pulumi.StringOutput)

	credential, err := managedidentity.NewFederatedIdentityCredential(ctx, "cell-alb-controller-federated", &managedidentity.FederatedIdentityCredentialArgs{
		ResourceGroupName:                       net.resourceGroupName,
		ResourceName:                            identity.Name,
		FederatedIdentityCredentialResourceName: pulumi.String(azureALBControllerFederatedCredential),
		Issuer:                                  issuer,
		Subject:                                 pulumi.String("system:serviceaccount:" + azureALBControllerNamespace + ":" + azureALBControllerServiceAccount),
		Audiences:                               pulumi.StringArray{pulumi.String(azureWorkloadIdentityAudience)},
	}, pulumi.DependsOn([]pulumi.Resource{aks.cluster, identity}))
	if err != nil {
		return nil, err
	}

	ctx.Export("azureALBControllerIdentityName", identity.Name)
	ctx.Export("azureALBControllerIdentityID", identity.ID())
	ctx.Export("azureALBControllerClientID", identity.ClientId)
	ctx.Export("azureALBControllerPrincipalID", identity.PrincipalId)
	ctx.Export("azureALBControllerKubernetesServiceAccount", pulumi.String(azureALBControllerKubernetesServiceAccount))

	return &azureALBController{
		identityName:             identity.Name,
		identityID:               identity.ID(),
		clientID:                 identity.ClientId,
		principalID:              identity.PrincipalId,
		tenantID:                 pulumi.String(client.TenantId).ToStringOutput(),
		federatedCredentialName:  credential.Name,
		kubernetesServiceAccount: azureALBControllerKubernetesServiceAccount,
		dependencies:             []pulumi.Resource{readerRole, configManagerRole, subnetContributorRole, credential},
	}, nil
}
