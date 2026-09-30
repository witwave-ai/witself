package cell

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/pulumi/pulumi-civo/sdk/v2/go/civo"
	"github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes"
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	civoIngressNodePort     = "nodeport"
	civoIngressLoadBalancer = "loadbalancer"
	civoDNSNone             = "none"
	civoDNSCloudflare       = "cloudflare"

	civoLoadBalancerResource  = "civo-ingress-lb"
	civoLoadBalancerNamespace = "kube-system"
	civoLoadBalancerName      = "witself-lb"
	civoFirewallAnnotation    = "kubernetes.civo.com/firewall-id"
	civoAPIRecordResource     = "cell-api-record"
	civoAPIRecordTTL          = 300
)

func validateCivoIngress(c civoCell) error {
	if c.ingress != "" && c.ingress != civoIngressNodePort && c.ingress != civoIngressLoadBalancer {
		return fmt.Errorf("witself:civoIngress %q is not supported (want nodeport or loadbalancer)", c.ingress)
	}
	if c.dns != "" && c.dns != civoDNSNone && c.dns != civoDNSCloudflare {
		return fmt.Errorf("witself:civoDNS %q is not supported (want none or cloudflare)", c.dns)
	}
	if c.dns == civoDNSCloudflare && c.ingress != civoIngressLoadBalancer {
		return fmt.Errorf("witself:civoDNS cloudflare requires witself:civoIngress loadbalancer")
	}
	if c.ingress == civoIngressLoadBalancer && normalizeZoneName(c.domain) == "" {
		return fmt.Errorf("witself:domain is required with witself:civoIngress loadbalancer")
	}
	return nil
}

func civoLoadBalancerAddress(status *corev1.ServiceStatus) (string, error) {
	if status != nil && status.LoadBalancer != nil {
		for _, ingress := range status.LoadBalancer.Ingress {
			if ingress.Ip == nil {
				continue
			}
			address, err := netip.ParseAddr(*ingress.Ip)
			if err == nil && address.Is4() {
				return address.String(), nil
			}
		}
	}
	return "", fmt.Errorf("load balancer Service kube-system/witself-lb reports no IPv4 address in status.loadBalancer.ingress; the host name in that status is never used")
}

func provisionCivoLoadBalancer(ctx *pulumi.Context, c civoCell, k8s *kubernetes.Provider, cluster *civo.KubernetesCluster, firewall *civo.Firewall, apiHost string) ([]pulumi.Resource, error) {
	service, err := corev1.NewService(ctx, civoLoadBalancerResource, &corev1.ServiceArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String(civoLoadBalancerName),
			Namespace: pulumi.String(civoLoadBalancerNamespace),
			Annotations: pulumi.StringMap{
				civoFirewallAnnotation: firewall.ID().ToStringOutput(),
			},
		},
		Spec: &corev1.ServiceSpecArgs{
			Type: pulumi.String("LoadBalancer"),
			Selector: pulumi.StringMap{
				"app.kubernetes.io/instance": pulumi.String("traefik-kube-system"),
				"app.kubernetes.io/name":     pulumi.String("traefik"),
			},
			Ports: corev1.ServicePortArray{
				&corev1.ServicePortArgs{
					Name: pulumi.String("http"), Protocol: pulumi.String("TCP"),
					Port: pulumi.Int(80), TargetPort: pulumi.Int(80),
				},
				&corev1.ServicePortArgs{
					Name: pulumi.String("https"), Protocol: pulumi.String("TCP"),
					Port: pulumi.Int(443), TargetPort: pulumi.Int(443),
				},
			},
		},
	}, pulumi.Provider(k8s), pulumi.DependsOn([]pulumi.Resource{cluster, firewall}))
	if err != nil {
		return nil, err
	}
	address := service.Status.ApplyT(civoLoadBalancerAddress).(pulumi.StringOutput)
	ctx.Export("loadBalancerIP", address)
	ctx.Export("civoIngress", pulumi.String(civoIngressLoadBalancer))
	dns := civoDNSNone
	if c.dns == civoDNSCloudflare {
		dns = civoDNSCloudflare
	}
	ctx.Export("civoDNS", pulumi.String(dns))
	if c.dns != civoDNSCloudflare {
		return []pulumi.Resource{service}, nil
	}

	provider, err := newCloudflareProvider(ctx)
	if err != nil {
		return nil, err
	}
	zone, err := lookupCloudflareZone(ctx, normalizeZoneName(c.domain), provider)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(apiHost, "."+zone.name) {
		return nil, fmt.Errorf("zone %q found in Cloudflare does not contain the host %q", zone.name, apiHost)
	}
	record, err := newCloudflareDNSRecord(ctx, civoAPIRecordResource, pulumi.Map{
		"zoneId":  pulumi.String(zone.zoneID),
		"name":    pulumi.String(apiHost),
		"type":    pulumi.String("A"),
		"content": address,
		"ttl":     pulumi.Float64(civoAPIRecordTTL),
		"proxied": pulumi.Bool(false),
		"comment": pulumi.String("Witself cell API: " + c.name),
	}, pulumi.Provider(provider), pulumi.DependsOn([]pulumi.Resource{service}))
	if err != nil {
		return nil, err
	}
	ctx.Export("cloudflareDNSZone", pulumi.String(zone.name))
	return []pulumi.Resource{service, record}, nil
}
