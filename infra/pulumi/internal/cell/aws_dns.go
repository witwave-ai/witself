package cell

import (
	"strings"

	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/route53"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func normalizeZoneName(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

func provisionAWSDNS(ctx *pulumi.Context, c awsCell, prov *aws.Provider) error {
	zoneName := normalizeZoneName(c.dnsZone)
	if zoneName == "" {
		return nil
	}

	zone, err := route53.NewZone(ctx, "cell-dns", &route53.ZoneArgs{
		Name:         pulumi.StringPtr(zoneName),
		Comment:      pulumi.StringPtr("Witself cell public DNS zone for " + c.name),
		ForceDestroy: pulumi.BoolPtr(true),
		Tags:         resourceTags(rname(c.name, "dns"), "dns"),
	}, pulumi.Provider(prov))
	if err != nil {
		return err
	}

	ctx.Export("dnsZoneName", pulumi.String(zoneName))
	ctx.Export("dnsZoneID", zone.ZoneId)
	ctx.Export("dnsZoneNameServers", zone.NameServers)
	ctx.Export("dnsDelegationRecordName", pulumi.String(zoneName))
	ctx.Export("dnsDelegationRecordType", pulumi.String("NS"))
	return nil
}
