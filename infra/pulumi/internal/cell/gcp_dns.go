package cell

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/compute"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/dns"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/projects"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	gcpExternalDNSNamespace      = "external-dns"
	gcpExternalDNSServiceAccount = "external-dns"
)

type gcpDNS struct {
	zoneName       string
	apiHost        string
	managedZone    pulumi.StringOutput
	nameServers    pulumi.StringArrayOutput
	apiAddressName pulumi.StringOutput
	apiAddress     pulumi.StringOutput
}

func gcpExternalDNSAccountID(c gcpCell) string {
	base := "ws-dns-" + c.name
	if len(base) <= 30 {
		return base
	}
	sum := sha1.Sum([]byte(c.name))
	return "ws-dns-" + hex.EncodeToString(sum[:])[:12]
}

func provisionGCPDNS(ctx *pulumi.Context, c gcpCell, prov *gcp.Provider, dnsAPI, computeAPI pulumi.Resource) (*gcpDNS, error) {
	parentDomain := normalizeZoneName(c.domain)
	if parentDomain == "" {
		return nil, nil
	}
	zoneName := c.name + "." + parentDomain
	apiHost := "api." + zoneName

	zone, err := dns.NewManagedZone(ctx, "cell-dns", &dns.ManagedZoneArgs{
		Name:         pulumi.String(rname(c.name, "dns")),
		DnsName:      pulumi.String(zoneName + "."),
		Description:  pulumi.String("Witself cell public DNS zone for " + c.name),
		Visibility:   pulumi.String("public"),
		ForceDestroy: pulumi.BoolPtr(true),
		Labels:       gcpDefaultLabels(c),
	}, pulumi.Provider(prov), pulumi.DependsOn([]pulumi.Resource{dnsAPI}))
	if err != nil {
		return nil, err
	}

	address, err := compute.NewGlobalAddress(ctx, "cell-api", &compute.GlobalAddressArgs{
		Name:        pulumi.String(rname(c.name, "api")),
		Description: pulumi.String("Witself API ingress IP for " + c.name),
		AddressType: pulumi.String("EXTERNAL"),
		IpVersion:   pulumi.String("IPV4"),
		Labels:      gcpDefaultLabels(c),
	}, pulumi.Provider(prov), pulumi.DependsOn([]pulumi.Resource{computeAPI}))
	if err != nil {
		return nil, err
	}

	if _, err := provisionCloudflareDNSDelegation(ctx, cloudflareDelegation{
		enabled:      c.cloudflareDNS,
		cellName:     c.name,
		parentDomain: c.domain,
	}, zoneName, zone.NameServers); err != nil {
		return nil, err
	}

	ctx.Export("cellDomain", pulumi.String(zoneName))
	ctx.Export("apiHost", pulumi.String(apiHost))
	ctx.Export("dnsZoneName", pulumi.String(zoneName))
	ctx.Export("dnsManagedZone", zone.Name)
	ctx.Export("dnsZoneNameServers", zone.NameServers)
	ctx.Export("dnsDelegationRecordName", pulumi.String(zoneName))
	ctx.Export("dnsDelegationRecordType", pulumi.String("NS"))
	ctx.Export("ingressGlobalAddressName", address.Name)
	ctx.Export("ingressGlobalAddress", address.Address)

	return &gcpDNS{
		zoneName:       zoneName,
		apiHost:        apiHost,
		managedZone:    zone.Name,
		nameServers:    zone.NameServers,
		apiAddressName: address.Name,
		apiAddress:     address.Address,
	}, nil
}

func provisionGCPExternalDNSWorkloadIdentity(ctx *pulumi.Context, c gcpCell, gcpDNS *gcpDNS, prov *gcp.Provider, iamCredentialsAPI pulumi.Resource) (pulumi.StringOutput, error) {
	if gcpDNS == nil {
		return pulumi.String("").ToStringOutput(), nil
	}

	gsa, err := serviceaccount.NewAccount(ctx, "cell-external-dns", &serviceaccount.AccountArgs{
		AccountId:   pulumi.String(gcpExternalDNSAccountID(c)),
		DisplayName: pulumi.String("Witself ExternalDNS for " + c.name),
		Description: pulumi.String("ExternalDNS identity for " + c.name),
		Project:     pulumi.String(c.project),
	}, pulumi.Provider(prov), pulumi.DependsOn([]pulumi.Resource{iamCredentialsAPI}))
	if err != nil {
		return pulumi.String("").ToStringOutput(), err
	}

	if _, err := serviceaccount.NewIAMMember(ctx, "cell-external-dns-workload-identity", &serviceaccount.IAMMemberArgs{
		ServiceAccountId: gsa.Name,
		Role:             pulumi.String("roles/iam.workloadIdentityUser"),
		Member:           pulumi.String(fmt.Sprintf("serviceAccount:%s.svc.id.goog[%s/%s]", c.project, gcpExternalDNSNamespace, gcpExternalDNSServiceAccount)),
	}, pulumi.Provider(prov)); err != nil {
		return pulumi.String("").ToStringOutput(), err
	}

	if _, err := projects.NewIAMMember(ctx, "cell-external-dns-project-reader", &projects.IAMMemberArgs{
		Project: pulumi.String(c.project),
		Role:    pulumi.String("roles/dns.reader"),
		Member:  pulumi.Sprintf("serviceAccount:%s", gsa.Email),
	}, pulumi.Provider(prov)); err != nil {
		return pulumi.String("").ToStringOutput(), err
	}

	if _, err := dns.NewDnsManagedZoneIamMember(ctx, "cell-external-dns-zone", &dns.DnsManagedZoneIamMemberArgs{
		Project:     pulumi.String(c.project),
		ManagedZone: gcpDNS.managedZone,
		Role:        pulumi.String("roles/dns.admin"),
		Member:      pulumi.Sprintf("serviceAccount:%s", gsa.Email),
	}, pulumi.Provider(prov)); err != nil {
		return pulumi.String("").ToStringOutput(), err
	}

	ctx.Export("externalDNSServiceAccountEmail", gsa.Email)
	ctx.Export("externalDNSKubernetesServiceAccount", pulumi.String(gcpExternalDNSNamespace+"/"+gcpExternalDNSServiceAccount))
	ctx.Export("externalDNSZone", pulumi.String(gcpDNS.zoneName))
	return gsa.Email, nil
}
