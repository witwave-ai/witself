package cell

import (
	"fmt"

	network "github.com/pulumi/pulumi-azure-native-sdk/network/v3"
	resources "github.com/pulumi/pulumi-azure-native-sdk/resources/v3"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

type azureNetwork struct {
	resourceGroupName pulumi.StringOutput
	vnetName          pulumi.StringOutput
	vnetID            pulumi.IDOutput
	workloadSubnetID  pulumi.IDOutput
	dbSubnetID        pulumi.IDOutput
	workloadSubnet    string
	workloadCIDR      string
	dbSubnet          string
	dbCIDR            string
	natGatewayName    pulumi.StringOutput
	natGatewayID      pulumi.IDOutput
	natPublicIPName   pulumi.StringOutput
	natPublicIP       pulumi.StringPtrOutput
}

type azureDatabase struct {
	serverName     pulumi.StringOutput
	fqdn           pulumi.StringOutput
	databaseName   pulumi.StringOutput
	username       pulumi.StringOutput
	password       pulumi.StringOutput
	dsn            pulumi.StringOutput
	privateDNSZone pulumi.StringOutput
	privateDNSLink pulumi.StringOutput
	version        string
	database       pulumi.Resource
}

type azureKubernetes struct {
	name                    pulumi.StringOutput
	fqdn                    pulumi.StringOutput
	kubernetesVersion       pulumi.StringPtrOutput
	nodeResourceGroup       pulumi.StringPtrOutput
	identityName            pulumi.StringOutput
	identityID              pulumi.IDOutput
	oidcIssuerURL           pulumi.StringPtrOutput
	workloadIdentityEnabled pulumi.BoolPtrOutput
	networkPlugin           pulumi.StringPtrOutput
	networkPluginMode       pulumi.StringPtrOutput
	outboundType            pulumi.StringPtrOutput
	cluster                 pulumi.Resource
}

type azureESO struct {
	identityName             pulumi.StringOutput
	identityID               pulumi.IDOutput
	clientID                 pulumi.StringOutput
	principalID              pulumi.StringOutput
	tenantID                 pulumi.StringOutput
	federatedCredentialName  pulumi.StringOutput
	kubernetesServiceAccount string
	dependencies             []pulumi.Resource
}

func azureDefaultTags(c azureCell) pulumi.StringMap {
	return pulumi.StringMap{
		"app":                   pulumi.String("witself"),
		"witself:cell":          pulumi.String(c.name),
		"witself:cloud":         pulumi.String("azure"),
		"witself:account-alias": pulumi.String(c.accountAlias),
		"witself:region":        pulumi.String(c.region),
		"witself:role":          pulumi.String(c.role),
		"witself:profile":       pulumi.String(c.profile),
		"witself:managed-by":    pulumi.String("pulumi"),
	}
}

func azureResourceTags(c azureCell, name, component string) pulumi.StringMap {
	tags := azureDefaultTags(c)
	tags["Name"] = pulumi.String(name)
	tags["witself:component"] = pulumi.String(component)
	return tags
}

func provisionAzure(ctx *pulumi.Context, c azureCell) error {
	net, err := provisionAzureNetwork(ctx, c)
	if err != nil {
		return err
	}

	db, err := provisionAzurePostgres(ctx, c, net)
	if err != nil {
		return err
	}

	aks, err := provisionAzureAKS(ctx, c, net)
	if err != nil {
		return err
	}

	secrets, err := provisionAzureSecrets(ctx, c, net, db)
	if err != nil {
		return err
	}

	eso, err := provisionAzureESOWorkloadIdentity(ctx, c, net, aks, secrets)
	if err != nil {
		return err
	}

	if c.argocd {
		if err := provisionAzureArgoCD(ctx, c, net, aks, secrets, eso); err != nil {
			return err
		}
	}

	ctx.Export("status", pulumi.String("azure: resource group + vnet + controlled egress + postgres flexible server + key vault secrets + aks + eso workload identity provisioned"))
	ctx.Export("azureRegion", pulumi.String(c.region))
	ctx.Export("accountAlias", pulumi.String(c.accountAlias))
	ctx.Export("role", pulumi.String(c.role))
	ctx.Export("resourceGroup", net.resourceGroupName)
	ctx.Export("vnetName", net.vnetName)
	ctx.Export("vnetID", net.vnetID)
	ctx.Export("workloadSubnetName", pulumi.String(net.workloadSubnet))
	ctx.Export("workloadSubnetID", net.workloadSubnetID)
	ctx.Export("workloadSubnetCIDR", pulumi.String(net.workloadCIDR))
	ctx.Export("dbSubnetName", pulumi.String(net.dbSubnet))
	ctx.Export("dbSubnetID", net.dbSubnetID)
	ctx.Export("dbSubnetCIDR", pulumi.String(net.dbCIDR))
	ctx.Export("natGatewayName", net.natGatewayName)
	ctx.Export("natGatewayID", net.natGatewayID)
	ctx.Export("natPublicIPName", net.natPublicIPName)
	ctx.Export("natPublicIP", net.natPublicIP)
	ctx.Export("dbInstance", db.serverName)
	ctx.Export("dbEndpoint", db.fqdn)
	ctx.Export("dbName", db.databaseName)
	ctx.Export("dbUsername", db.username)
	ctx.Export("dbPassword", db.password)
	ctx.Export("dbDSN", db.dsn)
	ctx.Export("dbVersion", pulumi.String(db.version))
	ctx.Export("dbPrivateDNSZone", db.privateDNSZone)
	ctx.Export("dbPrivateDNSLink", db.privateDNSLink)
	ctx.Export("secretVaultName", secrets.vaultName)
	ctx.Export("secretVaultID", secrets.vaultID)
	ctx.Export("secretVaultURL", secrets.vaultURL)
	ctx.Export("dbSecretName", pulumi.String(secrets.dbSecretName))
	ctx.Export("dbSecretID", secrets.dbSecretID)
	ctx.Export("bootstrapSecretName", pulumi.String(secrets.bootstrapSecretName))
	ctx.Export("bootstrapSecretID", secrets.bootstrapSecretID)
	ctx.Export("bootstrapTokenTTL", pulumi.String(secrets.bootstrapTokenTTL))
	ctx.Export("provisionSecretName", pulumi.String(secrets.provisionSecretName))
	ctx.Export("provisionSecretID", secrets.provisionSecretID)
	ctx.Export("provisionToken", pulumi.ToSecret(secrets.provisionToken))
	ctx.Export("aksCluster", aks.name)
	ctx.Export("aksFQDN", aks.fqdn)
	ctx.Export("aksKubernetesVersion", aks.kubernetesVersion)
	ctx.Export("aksNodeResourceGroup", aks.nodeResourceGroup)
	ctx.Export("aksIdentityName", aks.identityName)
	ctx.Export("aksIdentityID", aks.identityID)
	ctx.Export("aksOIDCIssuerURL", aks.oidcIssuerURL)
	ctx.Export("aksWorkloadIdentityEnabled", aks.workloadIdentityEnabled)
	ctx.Export("aksNetworkPlugin", aks.networkPlugin)
	ctx.Export("aksNetworkPluginMode", aks.networkPluginMode)
	ctx.Export("aksOutboundType", aks.outboundType)
	ctx.Export("esoIdentityName", eso.identityName)
	ctx.Export("esoIdentityID", eso.identityID)
	ctx.Export("esoClientID", eso.clientID)
	ctx.Export("esoPrincipalID", eso.principalID)
	ctx.Export("esoTenantID", eso.tenantID)
	ctx.Export("esoFederatedCredential", eso.federatedCredentialName)
	ctx.Export("esoKubernetesServiceAccount", pulumi.String(eso.kubernetesServiceAccount))
	return nil
}

func provisionAzureNetwork(ctx *pulumi.Context, c azureCell) (*azureNetwork, error) {
	prefix := cidrPrefix(c.cidr)
	workloadCIDR := fmt.Sprintf("%s.0.0/20", prefix)
	dbCIDR := fmt.Sprintf("%s.32.0/24", prefix)

	resourceGroupName := rname(c.name, "rg")
	vnetName := rname(c.name, "vnet")
	workloadSubnetName := rname(c.name, "workload")
	dbSubnetName := rname(c.name, "db")
	natName := rname(c.name, "nat")

	rg, err := resources.NewResourceGroup(ctx, "cell", &resources.ResourceGroupArgs{
		ResourceGroupName: pulumi.String(resourceGroupName),
		Location:          pulumi.String(c.region),
		Tags:              azureResourceTags(c, resourceGroupName, "cell"),
	})
	if err != nil {
		return nil, err
	}

	vnet, err := network.NewVirtualNetwork(ctx, "cell", &network.VirtualNetworkArgs{
		ResourceGroupName:  rg.Name,
		VirtualNetworkName: pulumi.String(vnetName),
		Location:           rg.Location,
		AddressSpace: network.AddressSpaceArgs{
			AddressPrefixes: pulumi.StringArray{pulumi.String(c.cidr)},
		},
		Tags: azureResourceTags(c, vnetName, "network"),
	}, pulumi.DependsOn([]pulumi.Resource{rg}))
	if err != nil {
		return nil, err
	}

	publicIP, err := network.NewPublicIPAddress(ctx, "cell-nat", &network.PublicIPAddressArgs{
		ResourceGroupName:        rg.Name,
		PublicIpAddressName:      pulumi.String(natName),
		Location:                 rg.Location,
		PublicIPAllocationMethod: pulumi.String(string(network.IPAllocationMethodStatic)),
		Sku: network.PublicIPAddressSkuArgs{
			Name: pulumi.String(string(network.PublicIPAddressSkuNameStandard)),
		},
		Tags: azureResourceTags(c, natName, "network"),
	}, pulumi.DependsOn([]pulumi.Resource{rg}))
	if err != nil {
		return nil, err
	}

	nat, err := network.NewNatGateway(ctx, "cell", &network.NatGatewayArgs{
		ResourceGroupName: rg.Name,
		NatGatewayName:    pulumi.String(natName),
		Location:          rg.Location,
		Sku: network.NatGatewaySkuArgs{
			Name: pulumi.String(string(network.NatGatewaySkuNameStandard)),
		},
		PublicIpAddresses: network.SubResourceArray{
			network.SubResourceArgs{Id: publicIP.ID()},
		},
		Tags: azureResourceTags(c, natName, "network"),
	}, pulumi.DependsOn([]pulumi.Resource{publicIP}))
	if err != nil {
		return nil, err
	}

	workloadSubnet, err := network.NewSubnet(ctx, "cell-workload", &network.SubnetArgs{
		ResourceGroupName:     rg.Name,
		VirtualNetworkName:    vnet.Name,
		SubnetName:            pulumi.String(workloadSubnetName),
		AddressPrefix:         pulumi.String(workloadCIDR),
		DefaultOutboundAccess: pulumi.Bool(false),
		NatGateway:            network.SubResourceArgs{Id: nat.ID()},
	}, pulumi.DependsOn([]pulumi.Resource{vnet, nat}))
	if err != nil {
		return nil, err
	}

	dbSubnet, err := network.NewSubnet(ctx, "cell-db", &network.SubnetArgs{
		ResourceGroupName:     rg.Name,
		VirtualNetworkName:    vnet.Name,
		SubnetName:            pulumi.String(dbSubnetName),
		AddressPrefix:         pulumi.String(dbCIDR),
		DefaultOutboundAccess: pulumi.Bool(false),
		Delegations: network.DelegationArray{
			network.DelegationArgs{
				Name:        pulumi.String("postgres-flexible-server"),
				ServiceName: pulumi.String("Microsoft.DBforPostgreSQL/flexibleServers"),
				Actions: pulumi.StringArray{
					pulumi.String("Microsoft.Network/virtualNetworks/subnets/join/action"),
				},
			},
		},
	}, pulumi.DependsOn([]pulumi.Resource{vnet}))
	if err != nil {
		return nil, err
	}

	return &azureNetwork{
		resourceGroupName: rg.Name,
		vnetName:          vnet.Name,
		vnetID:            vnet.ID(),
		workloadSubnetID:  workloadSubnet.ID(),
		dbSubnetID:        dbSubnet.ID(),
		workloadSubnet:    workloadSubnetName,
		workloadCIDR:      workloadCIDR,
		dbSubnet:          dbSubnetName,
		dbCIDR:            dbCIDR,
		natGatewayName:    nat.Name,
		natGatewayID:      nat.ID(),
		natPublicIPName:   publicIP.Name,
		natPublicIP:       publicIP.IpAddress,
	}, nil
}
