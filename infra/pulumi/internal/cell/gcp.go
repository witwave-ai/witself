package cell

import (
	"fmt"
	"net/netip"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/compute"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/projects"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/servicenetworking"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

type gcpNetwork struct {
	networkName       pulumi.StringOutput
	networkSelfLink   pulumi.StringOutput
	subnetName        pulumi.StringOutput
	subnetSelfLink    pulumi.StringOutput
	subnetCIDR        string
	podsRangeName     string
	podsRangeCIDR     string
	servicesRangeName string
	servicesRangeCIDR string
	privateRangeName  pulumi.StringOutput
	privateRangeCIDR  string
	privatePeering    pulumi.StringOutput
}

func gcpDefaultLabels(c gcpCell) pulumi.StringMap {
	return pulumi.StringMap{
		"app":                   pulumi.String("witself"),
		"witself_cell":          pulumi.String(c.name),
		"witself_cloud":         pulumi.String("gcp"),
		"witself_account_alias": pulumi.String(c.accountAlias),
		"witself_region":        pulumi.String(c.region),
		"witself_role":          pulumi.String(c.role),
		"witself_profile":       pulumi.String(c.profile),
		"witself_managed_by":    pulumi.String("pulumi"),
	}
}

// provisionGCP is the first GCP slice: a real Pulumi stack with no workload
// substrate. The state backend and secrets provider are prepared outside the
// resource graph by internal/backend/gcp.go.
func provisionGCP(ctx *pulumi.Context, c gcpCell) error {
	prov, err := gcp.NewProvider(ctx, "gcp", &gcp.ProviderArgs{
		Project:             pulumi.String(c.project),
		Region:              pulumi.String(c.region),
		BillingProject:      pulumi.String(c.project),
		UserProjectOverride: pulumi.Bool(true),
		DefaultLabels:       gcpDefaultLabels(c),
	})
	if err != nil {
		return err
	}

	computeAPI, err := projects.NewService(ctx, "gcp-compute-api", &projects.ServiceArgs{
		Project:          pulumi.String(c.project),
		Service:          pulumi.String("compute.googleapis.com"),
		DisableOnDestroy: pulumi.Bool(false),
	}, pulumi.Provider(prov))
	if err != nil {
		return err
	}

	serviceNetworkingAPI, err := projects.NewService(ctx, "gcp-service-networking-api", &projects.ServiceArgs{
		Project:          pulumi.String(c.project),
		Service:          pulumi.String("servicenetworking.googleapis.com"),
		DisableOnDestroy: pulumi.Bool(false),
	}, pulumi.Provider(prov))
	if err != nil {
		return err
	}

	net, err := provisionGCPNetwork(ctx, c, prov, computeAPI, serviceNetworkingAPI)
	if err != nil {
		return err
	}

	ctx.Export("status", pulumi.String("gcp: vpc network + private services access provisioned"))
	ctx.Export("gcpProject", pulumi.String(c.project))
	ctx.Export("gcpRegion", pulumi.String(c.region))
	ctx.Export("accountAlias", pulumi.String(c.accountAlias))
	ctx.Export("role", pulumi.String(c.role))
	ctx.Export("vpcName", net.networkName)
	ctx.Export("vpcSelfLink", net.networkSelfLink)
	ctx.Export("subnetName", net.subnetName)
	ctx.Export("subnetSelfLink", net.subnetSelfLink)
	ctx.Export("subnetCIDR", pulumi.String(net.subnetCIDR))
	ctx.Export("podsRangeName", pulumi.String(net.podsRangeName))
	ctx.Export("podsRangeCIDR", pulumi.String(net.podsRangeCIDR))
	ctx.Export("servicesRangeName", pulumi.String(net.servicesRangeName))
	ctx.Export("servicesRangeCIDR", pulumi.String(net.servicesRangeCIDR))
	ctx.Export("privateServiceRangeName", net.privateRangeName)
	ctx.Export("privateServiceRangeCIDR", pulumi.String(net.privateRangeCIDR))
	ctx.Export("privateServicePeering", net.privatePeering)
	return nil
}

func provisionGCPNetwork(ctx *pulumi.Context, c gcpCell, prov *gcp.Provider, computeAPI, serviceNetworkingAPI pulumi.Resource) (*gcpNetwork, error) {
	prefix := cidrPrefix(c.cidr)
	subnetCIDR := fmt.Sprintf("%s.0.0/20", prefix)
	podsRangeName := "pods"
	podsRangeCIDR := fmt.Sprintf("%s.16.0/20", prefix)
	servicesRangeName := "services"
	servicesRangeCIDR := fmt.Sprintf("%s.32.0/22", prefix)
	privateRangeAddress, privateRangeCIDR := privateServicesRange(c.cidr)

	network, err := compute.NewNetwork(ctx, "cell", &compute.NetworkArgs{
		Name:                        pulumi.String(rname(c.name, "vpc")),
		Description:                 pulumi.String("Witself cell VPC for " + c.name),
		AutoCreateSubnetworks:       pulumi.Bool(false),
		RoutingMode:                 pulumi.String("REGIONAL"),
		DeleteDefaultRoutesOnCreate: pulumi.Bool(false),
	}, pulumi.Provider(prov), pulumi.DependsOn([]pulumi.Resource{computeAPI}))
	if err != nil {
		return nil, err
	}

	subnet, err := compute.NewSubnetwork(ctx, "cell", &compute.SubnetworkArgs{
		Name:                  pulumi.String(rname(c.name, "subnet")),
		Description:           pulumi.String("Witself cell regional subnet for " + c.name),
		IpCidrRange:           pulumi.String(subnetCIDR),
		Region:                pulumi.String(c.region),
		Network:               network.ID(),
		PrivateIpGoogleAccess: pulumi.Bool(true),
		SecondaryIpRanges: compute.SubnetworkSecondaryIpRangeArray{
			&compute.SubnetworkSecondaryIpRangeArgs{
				RangeName:   pulumi.String(podsRangeName),
				IpCidrRange: pulumi.String(podsRangeCIDR),
			},
			&compute.SubnetworkSecondaryIpRangeArgs{
				RangeName:   pulumi.String(servicesRangeName),
				IpCidrRange: pulumi.String(servicesRangeCIDR),
			},
		},
	}, pulumi.Provider(prov), pulumi.DependsOn([]pulumi.Resource{network}))
	if err != nil {
		return nil, err
	}

	if _, err := compute.NewFirewall(ctx, "cell-internal", &compute.FirewallArgs{
		Name:        pulumi.String(rname(c.name, "allow-internal")),
		Description: pulumi.String("Allow internal traffic inside the Witself cell VPC"),
		Network:     network.SelfLink,
		Direction:   pulumi.String("INGRESS"),
		Priority:    pulumi.Int(1000),
		SourceRanges: pulumi.StringArray{
			pulumi.String(c.cidr),
		},
		Allows: compute.FirewallAllowArray{
			&compute.FirewallAllowArgs{Protocol: pulumi.String("icmp")},
			&compute.FirewallAllowArgs{Protocol: pulumi.String("tcp")},
			&compute.FirewallAllowArgs{Protocol: pulumi.String("udp")},
		},
	}, pulumi.Provider(prov), pulumi.DependsOn([]pulumi.Resource{network})); err != nil {
		return nil, err
	}

	privateRange, err := compute.NewGlobalAddress(ctx, "cell-private-services", &compute.GlobalAddressArgs{
		Name:         pulumi.String(rname(c.name, "private-services")),
		Description:  pulumi.String("Private services access range for " + c.name),
		Purpose:      pulumi.String("VPC_PEERING"),
		AddressType:  pulumi.String("INTERNAL"),
		Address:      pulumi.String(privateRangeAddress),
		PrefixLength: pulumi.Int(20),
		Network:      network.ID(),
	}, pulumi.Provider(prov), pulumi.DependsOn([]pulumi.Resource{network, serviceNetworkingAPI}))
	if err != nil {
		return nil, err
	}

	privateConnection, err := servicenetworking.NewConnection(ctx, "cell-private-services", &servicenetworking.ConnectionArgs{
		Network: pulumi.Sprintf("projects/%s/global/networks/%s", c.project, network.Name),
		Service: pulumi.String("servicenetworking.googleapis.com"),
		ReservedPeeringRanges: pulumi.StringArray{
			privateRange.Name,
		},
	}, pulumi.Provider(prov), pulumi.DependsOn([]pulumi.Resource{privateRange, serviceNetworkingAPI}))
	if err != nil {
		return nil, err
	}

	return &gcpNetwork{
		networkName:       network.Name,
		networkSelfLink:   network.SelfLink,
		subnetName:        subnet.Name,
		subnetSelfLink:    subnet.SelfLink,
		subnetCIDR:        subnetCIDR,
		podsRangeName:     podsRangeName,
		podsRangeCIDR:     podsRangeCIDR,
		servicesRangeName: servicesRangeName,
		servicesRangeCIDR: servicesRangeCIDR,
		privateRangeName:  privateRange.Name,
		privateRangeCIDR:  privateRangeCIDR,
		privatePeering:    privateConnection.Peering,
	}, nil
}

func privateServicesRange(cidr string) (address string, block string) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "10.21.0.0", "10.21.0.0/20"
	}
	p = p.Masked()
	a := p.Addr()
	if !a.Is4() || p.Bits() >= 32 {
		return "10.21.0.0", "10.21.0.0/20"
	}
	ip := a.As4()
	base := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
	next := base + (uint32(1) << uint(32-p.Bits()))
	nextIP := netip.AddrFrom4([4]byte{byte(next >> 24), byte(next >> 16), byte(next >> 8), byte(next)})
	if !nextIP.IsPrivate() {
		return "10.21.0.0", "10.21.0.0/20"
	}
	address = nextIP.String()
	return address, address + "/20"
}
