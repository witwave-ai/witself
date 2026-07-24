package cell

import (
	"fmt"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const cloudflareDelegationTTL = 300
const cloudflareProviderVersion = "6.17.0"

type cloudflareProvider struct {
	pulumi.ProviderResourceState
}

func newCloudflareProvider(ctx *pulumi.Context) (*cloudflareProvider, error) {
	var prov cloudflareProvider
	err := ctx.RegisterResource("pulumi:providers:cloudflare", "cloudflare", nil, &prov, pulumi.Version(cloudflareProviderVersion))
	if err != nil {
		return nil, err
	}
	return &prov, nil
}

type cloudflareDNSRecord struct {
	pulumi.CustomResourceState
}

func newCloudflareDNSRecord(ctx *pulumi.Context, name string, args pulumi.Map, opts ...pulumi.ResourceOption) (*cloudflareDNSRecord, error) {
	var record cloudflareDNSRecord
	opts = append(opts, pulumi.Version(cloudflareProviderVersion))
	err := ctx.RegisterResource("cloudflare:index/dnsRecord:DnsRecord", name, args, &record, opts...)
	if err != nil {
		return nil, err
	}
	return &record, nil
}

type cloudflareDelegation struct {
	enabled      bool
	cellName     string
	parentDomain string
}

func provisionCloudflareDNSDelegation(ctx *pulumi.Context, d cloudflareDelegation, zoneName string, nameServers pulumi.StringArrayOutput) ([]pulumi.Resource, error) {
	if !d.enabled || zoneName == "" {
		ctx.Export("cloudflareDNSDelegation", pulumi.String("disabled"))
		return nil, nil
	}

	prov, err := newCloudflareProvider(ctx)
	if err != nil {
		return nil, err
	}

	parentZone, err := lookupCloudflareZone(ctx, d.parentDomain, prov)
	if err != nil {
		return nil, err
	}
	recordName := cloudflareDelegationRecordName(zoneName, parentZone.name)

	records := make([]pulumi.Resource, 0, 4)
	for i := 0; i < 4; i++ {
		record, err := newCloudflareDNSRecord(ctx, fmt.Sprintf("cell-dns-delegation-%d", i), pulumi.Map{
			"zoneId":  pulumi.String(parentZone.zoneID),
			"name":    pulumi.String(recordName),
			"type":    pulumi.String("NS"),
			"content": nameServers.Index(pulumi.Int(i)),
			"ttl":     pulumi.Float64(cloudflareDelegationTTL),
			"comment": pulumi.String("Witself cell DNS delegation for " + d.cellName),
		}, pulumi.Provider(prov))
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	ctx.Export("cloudflareDNSDelegation", pulumi.String("enabled"))
	ctx.Export("cloudflareDNSZone", pulumi.String(parentZone.name))
	ctx.Export("cloudflareDNSDelegationName", pulumi.String(recordName))
	return records, nil
}

type cloudflareZone struct {
	name   string
	zoneID string
}

type cloudflareLookupZonesArgs struct {
	Name     *string `pulumi:"name"`
	Match    *string `pulumi:"match"`
	MaxItems *int    `pulumi:"maxItems"`
}

type cloudflareLookupZonesResult struct {
	Results []cloudflareLookupZoneResult `pulumi:"results"`
}

type cloudflareLookupZoneResult struct {
	ID   string `pulumi:"id"`
	Name string `pulumi:"name"`
}

func lookupCloudflareZone(ctx *pulumi.Context, parentDomain string, prov *cloudflareProvider) (cloudflareZone, error) {
	for _, candidate := range cloudflareZoneCandidates(parentDomain) {
		match := "all"
		maxItems := 1
		var result cloudflareLookupZonesResult
		if err := ctx.Invoke("cloudflare:index/getZones:getZones", &cloudflareLookupZonesArgs{
			Name:     &candidate,
			Match:    &match,
			MaxItems: &maxItems,
		}, &result, pulumi.Provider(prov), pulumi.Version(cloudflareProviderVersion)); err != nil {
			return cloudflareZone{}, fmt.Errorf("lookup Cloudflare zone %s: %w", candidate, err)
		}
		if len(result.Results) > 0 {
			return cloudflareZone{
				name:   result.Results[0].Name,
				zoneID: result.Results[0].ID,
			}, nil
		}
	}
	return cloudflareZone{}, fmt.Errorf("no matching Cloudflare zone found for parent domain %q", parentDomain)
}

func cloudflareZoneCandidates(parentDomain string) []string {
	labels := strings.Split(normalizeZoneName(parentDomain), ".")
	candidates := make([]string, 0, len(labels)-1)
	for i := 0; i <= len(labels)-2; i++ {
		candidates = append(candidates, strings.Join(labels[i:], "."))
	}
	return candidates
}

func cloudflareDelegationRecordName(cellZoneName, cloudflareZoneName string) string {
	return strings.TrimSuffix(strings.TrimSuffix(cellZoneName, cloudflareZoneName), ".")
}

func provisionCivoDNS(ctx *pulumi.Context, c civoCell, address pulumi.StringInput) (string, pulumi.Resource, error) {
	if !c.cloudflareDNS {
		return "", nil, fmt.Errorf("Civo control-plane cells require CLOUDFLARE_API_TOKEN for their DNS-only A record")
	}
	if c.domain == "" {
		return "", nil, fmt.Errorf("Civo control-plane cells require witself:domain")
	}
	apiHost := "api." + c.name + "." + normalizeZoneName(c.domain)
	prov, err := newCloudflareProvider(ctx)
	if err != nil {
		return "", nil, err
	}
	zone, err := lookupCloudflareZone(ctx, c.domain, prov)
	if err != nil {
		return "", nil, err
	}
	recordName := cloudflareDelegationRecordName(apiHost, zone.name)
	record, err := newCloudflareDNSRecord(ctx, "cell-api-dns", pulumi.Map{
		"zoneId":  pulumi.String(zone.zoneID),
		"name":    pulumi.String(recordName),
		"type":    pulumi.String("A"),
		"content": address,
		"proxied": pulumi.Bool(false),
		"ttl":     pulumi.Float64(cloudflareDelegationTTL),
		"comment": pulumi.String("Witself Civo development cell " + c.name),
	}, pulumi.Provider(prov))
	if err != nil {
		return "", nil, err
	}
	ctx.Export("cloudflareDNSZone", pulumi.String(zone.name))
	ctx.Export("cloudflareDNSRecord", pulumi.String(apiHost))
	ctx.Export("cloudflareDNSProxy", pulumi.Bool(false))
	return apiHost, record, nil
}
