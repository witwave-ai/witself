package cell

import (
	"fmt"
	"net"

	"github.com/pulumi/pulumi-civo/sdk/v2/go/civo"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// provisionCivo is the inexpensive development substrate. It is deliberately
// separate from the AWS/GCP/Azure programs: Civo supplies the network and K3s
// cluster, while portable application services are reconciled through GitOps.
func provisionCivo(ctx *pulumi.Context, c civoCell) error {
	if c.region == "" {
		return fmt.Errorf("civo:region is required")
	}
	if c.adminCIDR == "" {
		return fmt.Errorf("witself:civoAdminCIDR is required; use the operator's public IP as a /32")
	}
	if _, _, err := net.ParseCIDR(c.adminCIDR); err != nil {
		return fmt.Errorf("witself:civoAdminCIDR %q is not a valid CIDR: %w", c.adminCIDR, err)
	}

	network, err := civo.NewNetwork(ctx, "cell-network", &civo.NetworkArgs{
		Label:  pulumi.String(c.name + "-network"),
		Region: pulumi.String(c.region),
	})
	if err != nil {
		return err
	}

	firewall, err := civo.NewFirewall(ctx, "cell-firewall", &civo.FirewallArgs{
		Name:               pulumi.String(c.name + "-firewall"),
		NetworkId:          network.ID().ToStringOutput(),
		Region:             pulumi.String(c.region),
		CreateDefaultRules: pulumi.Bool(false),
		IngressRules: civo.FirewallIngressRuleArray{
			civo.FirewallIngressRuleArgs{
				Action:    pulumi.String("allow"),
				Cidrs:     pulumi.StringArray{pulumi.String(c.adminCIDR)},
				Label:     pulumi.String("kubernetes-api"),
				PortRange: pulumi.String("6443"),
				Protocol:  pulumi.String("tcp"),
			},
			civo.FirewallIngressRuleArgs{
				Action:    pulumi.String("allow"),
				Cidrs:     pulumi.StringArray{pulumi.String("0.0.0.0/0")},
				Label:     pulumi.String("http-acme"),
				PortRange: pulumi.String("80"),
				Protocol:  pulumi.String("tcp"),
			},
			civo.FirewallIngressRuleArgs{
				Action:    pulumi.String("allow"),
				Cidrs:     pulumi.StringArray{pulumi.String("0.0.0.0/0")},
				Label:     pulumi.String("https-api"),
				PortRange: pulumi.String("443"),
				Protocol:  pulumi.String("tcp"),
			},
		},
		EgressRules: civo.FirewallEgressRuleArray{
			civo.FirewallEgressRuleArgs{
				Action:    pulumi.String("allow"),
				Cidrs:     pulumi.StringArray{pulumi.String("0.0.0.0/0")},
				Label:     pulumi.String("outbound-tcp"),
				PortRange: pulumi.String("1-65535"),
				Protocol:  pulumi.String("tcp"),
			},
			civo.FirewallEgressRuleArgs{
				Action:    pulumi.String("allow"),
				Cidrs:     pulumi.StringArray{pulumi.String("0.0.0.0/0")},
				Label:     pulumi.String("outbound-udp"),
				PortRange: pulumi.String("1-65535"),
				Protocol:  pulumi.String("udp"),
			},
		},
	})
	if err != nil {
		return err
	}

	cluster, err := civo.NewKubernetesCluster(ctx, "cluster", &civo.KubernetesClusterArgs{
		Name:        pulumi.String(c.name),
		Region:      pulumi.String(c.region),
		NetworkId:   network.ID().ToStringOutput(),
		FirewallId:  firewall.ID().ToStringOutput(),
		ClusterType: pulumi.String("k3s"),
		Cni:         pulumi.String("cilium"),
		Pools: civo.KubernetesClusterPoolsArgs{
			Label:            pulumi.String("development"),
			NodeCount:        pulumi.Int(1),
			PublicIpNodePool: pulumi.Bool(true),
			Size:             pulumi.String(c.nodeSize),
		},
		Tags:            pulumi.String("witself " + c.name + " development"),
		WriteKubeconfig: pulumi.Bool(c.argocd),
	}, pulumi.DependsOn([]pulumi.Resource{firewall}))
	if err != nil {
		return err
	}

	ctx.Export("clusterName", cluster.Name)
	ctx.Export("clusterEndpoint", cluster.ApiEndpoint)
	ctx.Export("clusterReady", cluster.Ready)
	ctx.Export("civoRegion", pulumi.String(c.region))
	ctx.Export("civoNodeSize", pulumi.String(c.nodeSize))
	ctx.Export("network", network.ID())
	ctx.Export("firewall", firewall.ID())
	ctx.Export("status", pulumi.String("Civo development substrate provisioned"))

	apiHost := ""
	var dnsRecord pulumi.Resource
	if c.argocd {
		apiHost, dnsRecord, err = provisionCivoDNS(ctx, c, cluster.MasterIp)
		if err != nil {
			return err
		}
	}
	ctx.Export("apiHost", pulumi.String(apiHost))

	if c.argocd {
		deps := []pulumi.Resource{cluster}
		if dnsRecord != nil {
			deps = append(deps, dnsRecord)
		}
		return provisionCivoArgoCD(ctx, c, cluster, apiHost, deps...)
	}
	return nil
}
