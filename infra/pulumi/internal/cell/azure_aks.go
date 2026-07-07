package cell

import (
	authorization "github.com/pulumi/pulumi-azure-native-sdk/authorization/v3"
	containerservice "github.com/pulumi/pulumi-azure-native-sdk/containerservice/v3"
	managedidentity "github.com/pulumi/pulumi-azure-native-sdk/managedidentity/v3"
	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const azureNetworkContributorRoleID = "4d97b98b-1d4f-4787-a291-c67834d212e7"

func provisionAzureAKS(ctx *pulumi.Context, c azureCell, net *azureNetwork) (*azureKubernetes, error) {
	client, err := authorization.GetClientConfig(ctx)
	if err != nil {
		return nil, err
	}

	nodeProfile := azureAKSNodeProfileFor(c.profile)
	clusterName := rname(c.name, "")
	identityName := rname(c.name, "aks")
	nodeResourceGroup := rname(c.name, "aks-nodes-rg")

	identity, err := managedidentity.NewUserAssignedIdentity(ctx, "cell-aks", &managedidentity.UserAssignedIdentityArgs{
		ResourceGroupName: net.resourceGroupName,
		ResourceName:      pulumi.String(identityName),
		Location:          pulumi.String(c.region),
		Tags:              azureResourceTags(c, identityName, "kubernetes"),
	})
	if err != nil {
		return nil, err
	}

	roleAssignmentName, err := random.NewRandomUuid(ctx, "cell-aks-network-contributor", &random.RandomUuidArgs{
		Keepers: pulumi.StringMap{
			"identity": identity.ID().ToStringOutput(),
			"scope":    net.vnetID.ToStringOutput(),
			"role":     pulumi.String(azureNetworkContributorRoleID),
		},
	})
	if err != nil {
		return nil, err
	}

	networkContributor, err := authorization.NewRoleAssignment(ctx, "cell-aks-network-contributor", &authorization.RoleAssignmentArgs{
		RoleAssignmentName: roleAssignmentName.Result,
		PrincipalId:        identity.PrincipalId,
		PrincipalType:      authorization.PrincipalTypeServicePrincipal,
		RoleDefinitionId:   pulumi.Sprintf("/subscriptions/%s/providers/Microsoft.Authorization/roleDefinitions/%s", client.SubscriptionId, azureNetworkContributorRoleID),
		Scope:              net.vnetID.ToStringOutput(),
	})
	if err != nil {
		return nil, err
	}

	cluster, err := containerservice.NewManagedCluster(ctx, "cell", &containerservice.ManagedClusterArgs{
		ResourceGroupName: net.resourceGroupName,
		ResourceName:      pulumi.String(clusterName),
		Location:          pulumi.String(c.region),
		DnsPrefix:         pulumi.String(clusterName),
		KubernetesVersion: pulumi.String(c.k8sVersion),
		EnableRBAC:        pulumi.Bool(true),
		Identity: containerservice.ManagedClusterIdentityArgs{
			Type: containerservice.ResourceIdentityTypeUserAssigned,
			UserAssignedIdentities: pulumi.StringArray{
				identity.ID().ToStringOutput(),
			},
		},
		NodeResourceGroup: pulumi.String(nodeResourceGroup),
		OidcIssuerProfile: containerservice.ManagedClusterOIDCIssuerProfileArgs{
			Enabled: pulumi.Bool(true),
		},
		SecurityProfile: containerservice.ManagedClusterSecurityProfileArgs{
			WorkloadIdentity: containerservice.ManagedClusterSecurityProfileWorkloadIdentityArgs{
				Enabled: pulumi.Bool(true),
			},
		},
		AgentPoolProfiles: containerservice.ManagedClusterAgentPoolProfileArray{
			containerservice.ManagedClusterAgentPoolProfileArgs{
				Name:                pulumi.String("system"),
				Count:               pulumi.Int(nodeProfile.minCount),
				Mode:                containerservice.AgentPoolModeSystem,
				Type:                containerservice.AgentPoolTypeVirtualMachineScaleSets,
				OsType:              pulumi.String("Linux"),
				OsDiskSizeGB:        pulumi.Int(64),
				VmSize:              pulumi.String(nodeProfile.vmSize),
				OrchestratorVersion: pulumi.String(c.k8sVersion),
				VnetSubnetID:        net.workloadSubnetID.ToStringOutput(),
				EnableAutoScaling:   pulumi.Bool(true),
				MinCount:            pulumi.Int(nodeProfile.minCount),
				MaxCount:            pulumi.Int(nodeProfile.maxCount),
				MaxPods:             pulumi.Int(110),
				NodeLabels: pulumi.StringMap{
					"witself.io/cell": pulumi.String(c.name),
					"witself.io/pool": pulumi.String("system"),
				},
				Tags: azureResourceTags(c, rname(c.name, "aks-system"), "kubernetes"),
			},
		},
		NetworkProfile: containerservice.ContainerServiceNetworkProfileArgs{
			NetworkPlugin:     pulumi.String("azure"),
			NetworkPluginMode: containerservice.NetworkPluginModeOverlay,
			NetworkPolicy:     pulumi.String("azure"),
			LoadBalancerSku:   pulumi.String("standard"),
			OutboundType:      pulumi.String("userAssignedNATGateway"),
			PodCidr:           pulumi.String("10.244.0.0/16"),
			ServiceCidr:       pulumi.String("10.21.0.0/16"),
			DnsServiceIP:      pulumi.String("10.21.0.10"),
		},
		PublicNetworkAccess: pulumi.String("Enabled"),
		Tags:                azureResourceTags(c, clusterName, "kubernetes"),
	}, pulumi.DependsOn([]pulumi.Resource{networkContributor}))
	if err != nil {
		return nil, err
	}

	security := cluster.SecurityProfile.WorkloadIdentity()
	network := cluster.NetworkProfile

	return &azureKubernetes{
		name:                    cluster.Name,
		fqdn:                    cluster.Fqdn,
		kubernetesVersion:       cluster.KubernetesVersion,
		nodeResourceGroup:       cluster.NodeResourceGroup,
		identityName:            identity.Name,
		identityID:              identity.ID(),
		oidcIssuerURL:           cluster.OidcIssuerProfile.IssuerURL(),
		workloadIdentityEnabled: security.Enabled(),
		networkPlugin:           network.NetworkPlugin(),
		networkPluginMode:       network.NetworkPluginMode(),
		outboundType:            network.OutboundType(),
		nodePoolAutoScaling:     true,
		nodePoolMinCount:        nodeProfile.minCount,
		nodePoolMaxCount:        nodeProfile.maxCount,
		cluster:                 cluster,
	}, nil
}

type azureAKSNodeProfile struct {
	vmSize   string
	minCount int
	maxCount int
}

func azureAKSNodeProfileFor(profile string) azureAKSNodeProfile {
	if profile == "prod" {
		return azureAKSNodeProfile{
			vmSize:   "Standard_D2s_v4",
			minCount: 2,
			maxCount: 20,
		}
	}
	return azureAKSNodeProfile{
		vmSize:   "Standard_D2s_v4",
		minCount: 1,
		maxCount: 20,
	}
}
