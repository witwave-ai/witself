package cell

import (
	"fmt"

	authorization "github.com/pulumi/pulumi-azure-native-sdk/authorization/v3"
	managedidentity "github.com/pulumi/pulumi-azure-native-sdk/managedidentity/v3"
	resources "github.com/pulumi/pulumi-azure-native-sdk/resources/v3"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	azureALBAddonAPIVersion               = "2025-09-02-preview"
	azureALBAddonServiceAccount           = "alb-controller-sa"
	azureALBAddonNamespace                = "kube-system"
	azureALBAddonKubernetesServiceAccount = azureALBAddonNamespace + "/" + azureALBAddonServiceAccount
)

func provisionAzureALBAddon(ctx *pulumi.Context, c azureCell, net *azureNetwork, aks *azureKubernetes) (*azureALBController, error) {
	client, err := authorization.GetClientConfig(ctx)
	if err != nil {
		return nil, err
	}

	addon, err := resources.NewDeployment(ctx, "cell-alb-addon", &resources.DeploymentArgs{
		DeploymentName:    pulumi.StringPtr(rname(c.name, "alb-addon")),
		ResourceGroupName: net.resourceGroupName,
		Properties: resources.DeploymentPropertiesArgs{
			Mode: resources.DeploymentModeIncremental,
			Template: pulumi.Map{
				"$schema":        pulumi.String("https://schema.management.azure.com/schemas/2019-04-01/deploymentTemplate.json#"),
				"contentVersion": pulumi.String("1.0.0.0"),
				"resources": pulumi.Array{
					pulumi.Map{
						"type":       pulumi.String("Microsoft.ContainerService/managedClusters"),
						"apiVersion": pulumi.String(azureALBAddonAPIVersion),
						"name":       aks.name,
						"location":   pulumi.String(c.region),
						"properties": pulumi.Map{
							"ingressProfile": pulumi.Map{
								"applicationLoadBalancer": pulumi.Map{
									"enabled": pulumi.Bool(true),
								},
								"gatewayAPI": pulumi.Map{
									"installation": pulumi.String("Standard"),
								},
							},
						},
					},
				},
			},
		},
		Tags: azureResourceTags(c, rname(c.name, "alb-addon"), "ingress"),
	}, pulumi.DependsOn([]pulumi.Resource{aks.cluster}))
	if err != nil {
		return nil, err
	}

	nodeResourceGroup := aks.nodeResourceGroup.ApplyT(func(v *string) (string, error) {
		if v == nil || *v == "" {
			return "", fmt.Errorf("AKS node resource group is empty")
		}
		return *v, nil
	}).(pulumi.StringOutput)

	identityName := pulumi.Sprintf("applicationloadbalancer-%s", aks.name)
	identity := managedidentity.LookupUserAssignedIdentityOutput(ctx, managedidentity.LookupUserAssignedIdentityOutputArgs{
		ResourceGroupName: nodeResourceGroup,
		ResourceName:      identityName,
	}, pulumi.DependsOn([]pulumi.Resource{addon}))

	subnetContributorRole, err := azureRoleAssignment(ctx, "cell-alb-controller-subnet", identity.PrincipalId(), net.albSubnetID.ToStringOutput(), client.SubscriptionId, azureNetworkContributorRoleID, pulumi.DependsOn([]pulumi.Resource{addon}))
	if err != nil {
		return nil, err
	}

	ctx.Export("azureALBControllerAddOn", pulumi.String("enabled"))
	ctx.Export("azureALBControllerIdentityName", identity.Name())
	ctx.Export("azureALBControllerIdentityID", identity.Id())
	ctx.Export("azureALBControllerClientID", identity.ClientId())
	ctx.Export("azureALBControllerPrincipalID", identity.PrincipalId())
	ctx.Export("azureALBControllerKubernetesServiceAccount", pulumi.String(azureALBAddonKubernetesServiceAccount))

	return &azureALBController{
		identityName:             identity.Name(),
		identityID:               identity.Id(),
		clientID:                 identity.ClientId(),
		principalID:              identity.PrincipalId(),
		tenantID:                 identity.TenantId(),
		kubernetesServiceAccount: azureALBAddonKubernetesServiceAccount,
		dependencies:             []pulumi.Resource{addon, subnetContributorRole},
	}, nil
}
