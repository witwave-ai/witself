package cell

import (
	"fmt"

	authorization "github.com/pulumi/pulumi-azure-native-sdk/authorization/v3"
	dns "github.com/pulumi/pulumi-azure-native-sdk/dns/v3"
	managedidentity "github.com/pulumi/pulumi-azure-native-sdk/managedidentity/v3"
	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	azureDNSZoneContributorRoleID       = "befefa01-2a29-4197-83a8-272ff33ce314"
	azureReaderRoleID                   = "acdd72a7-3385-48ef-bd42-f606fba81ae7"
	azureExternalDNSNamespace           = "external-dns"
	azureExternalDNSServiceAccount      = "external-dns"
	azureExternalDNSFederatedCredential = "external-dns"
	azureExternalDNSConfigSecret        = "external-dns-azure"
)

type azureDNS struct {
	zoneName          string
	apiHost           string
	resourceGroupName pulumi.StringOutput
	zone              pulumi.Resource
	zoneID            pulumi.IDOutput
	nameServers       pulumi.StringArrayOutput
	identityName      pulumi.StringOutput
	identityID        pulumi.IDOutput
	clientID          pulumi.StringOutput
	principalID       pulumi.StringOutput
	tenantID          pulumi.StringOutput
	subscriptionID    string
	dependencies      []pulumi.Resource
}

func provisionAzureDNS(ctx *pulumi.Context, c azureCell, net *azureNetwork, aks *azureKubernetes) (*azureDNS, error) {
	parentDomain := normalizeZoneName(c.domain)
	if parentDomain == "" {
		return nil, nil
	}

	client, err := authorization.GetClientConfig(ctx)
	if err != nil {
		return nil, err
	}

	zoneName := c.name + "." + parentDomain
	apiHost := "api." + zoneName

	zone, err := dns.NewZone(ctx, "cell-dns", &dns.ZoneArgs{
		ResourceGroupName: net.resourceGroupName,
		ZoneName:          pulumi.String(zoneName),
		ZoneType:          dns.ZoneTypePtr(string(dns.ZoneTypePublic)),
		Location:          pulumi.String("global"),
		Tags:              azureResourceTags(c, rname(c.name, "dns"), "dns"),
	}, pulumi.DependsOn([]pulumi.Resource{aks.cluster}))
	if err != nil {
		return nil, err
	}

	delegationRecords, err := provisionCloudflareDNSDelegation(ctx, cloudflareDelegation{
		enabled:      c.cloudflareDNS,
		cellName:     c.name,
		parentDomain: c.domain,
	}, zoneName, zone.NameServers)
	if err != nil {
		return nil, err
	}

	identityName := rname(c.name, "external-dns")
	identity, err := managedidentity.NewUserAssignedIdentity(ctx, "cell-external-dns", &managedidentity.UserAssignedIdentityArgs{
		ResourceGroupName: net.resourceGroupName,
		ResourceName:      pulumi.String(identityName),
		Location:          pulumi.String(c.region),
		Tags:              azureResourceTags(c, identityName, "dns"),
	})
	if err != nil {
		return nil, err
	}

	readerRole, err := azureRoleAssignment(ctx, "cell-external-dns-rg-reader", identity.PrincipalId, net.resourceGroupID.ToStringOutput(), client.SubscriptionId, azureReaderRoleID)
	if err != nil {
		return nil, err
	}
	zoneContributorRole, err := azureRoleAssignment(ctx, "cell-external-dns-zone", identity.PrincipalId, zone.ID().ToStringOutput(), client.SubscriptionId, azureDNSZoneContributorRoleID)
	if err != nil {
		return nil, err
	}

	issuer := aks.oidcIssuerURL.ApplyT(func(v *string) (string, error) {
		if v == nil || *v == "" {
			return "", fmt.Errorf("AKS OIDC issuer URL is empty; workload identity must be enabled")
		}
		return *v, nil
	}).(pulumi.StringOutput)

	credential, err := managedidentity.NewFederatedIdentityCredential(ctx, "cell-external-dns-federated", &managedidentity.FederatedIdentityCredentialArgs{
		ResourceGroupName:                       net.resourceGroupName,
		ResourceName:                            identity.Name,
		FederatedIdentityCredentialResourceName: pulumi.String(azureExternalDNSFederatedCredential),
		Issuer:                                  issuer,
		Subject:                                 pulumi.String("system:serviceaccount:" + azureExternalDNSNamespace + ":" + azureExternalDNSServiceAccount),
		Audiences:                               pulumi.StringArray{pulumi.String(azureWorkloadIdentityAudience)},
	}, pulumi.DependsOn([]pulumi.Resource{aks.cluster, identity}))
	if err != nil {
		return nil, err
	}

	deps := []pulumi.Resource{identity, readerRole, zoneContributorRole, credential}
	deps = append(deps, delegationRecords...)

	ctx.Export("cellDomain", pulumi.String(zoneName))
	ctx.Export("apiHost", pulumi.String(apiHost))
	ctx.Export("dnsZoneName", pulumi.String(zoneName))
	ctx.Export("dnsManagedZone", zone.Name)
	ctx.Export("dnsZoneID", zone.ID())
	ctx.Export("dnsZoneNameServers", zone.NameServers)
	ctx.Export("dnsDelegationRecordName", pulumi.String(zoneName))
	ctx.Export("dnsDelegationRecordType", pulumi.String("NS"))
	ctx.Export("externalDNSIdentityName", identity.Name)
	ctx.Export("externalDNSIdentityID", identity.ID())
	ctx.Export("externalDNSClientID", identity.ClientId)
	ctx.Export("externalDNSPrincipalID", identity.PrincipalId)
	ctx.Export("externalDNSKubernetesServiceAccount", pulumi.String(azureExternalDNSNamespace+"/"+azureExternalDNSServiceAccount))
	ctx.Export("externalDNSZone", pulumi.String(zoneName))

	return &azureDNS{
		zoneName:          zoneName,
		apiHost:           apiHost,
		resourceGroupName: net.resourceGroupName,
		zone:              zone,
		zoneID:            zone.ID(),
		nameServers:       zone.NameServers,
		identityName:      identity.Name,
		identityID:        identity.ID(),
		clientID:          identity.ClientId,
		principalID:       identity.PrincipalId,
		tenantID:          pulumi.String(client.TenantId).ToStringOutput(),
		subscriptionID:    client.SubscriptionId,
		dependencies:      deps,
	}, nil
}

func azureRoleAssignment(ctx *pulumi.Context, name string, principalID pulumi.StringOutput, scope pulumi.StringOutput, subscriptionID, roleID string) (*authorization.RoleAssignment, error) {
	roleAssignmentName, err := random.NewRandomUuid(ctx, name, &random.RandomUuidArgs{
		Keepers: pulumi.StringMap{
			"principal": principalID,
			"scope":     scope,
			"role":      pulumi.String(roleID),
		},
	})
	if err != nil {
		return nil, err
	}

	return authorization.NewRoleAssignment(ctx, name, &authorization.RoleAssignmentArgs{
		RoleAssignmentName: roleAssignmentName.Result,
		PrincipalId:        principalID,
		PrincipalType:      authorization.PrincipalTypeServicePrincipal,
		RoleDefinitionId:   pulumi.Sprintf("/subscriptions/%s/providers/Microsoft.Authorization/roleDefinitions/%s", subscriptionID, roleID),
		Scope:              scope,
	})
}
