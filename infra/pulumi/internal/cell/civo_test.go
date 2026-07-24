package cell

import (
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func TestProvisionCivoRegistersOnlyCivoSubstrate(t *testing.T) {
	mocks := &civoResourceMocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		return provisionCivo(ctx, civoCell{
			name:      "civo-sandbox-use1-dev",
			region:    "nyc1",
			profile:   "minimal",
			nodeSize:  "g4s.kube.medium",
			adminCIDR: "203.0.113.7/32",
		})
	}, pulumi.WithMocks("witself-infra", "civo-sandbox-use1-dev", mocks))
	if err != nil {
		t.Fatalf("provision Civo: %v", err)
	}

	for _, want := range []string{
		"civo:index/network:Network",
		"civo:index/firewall:Firewall",
		"civo:index/kubernetesCluster:KubernetesCluster",
	} {
		if !mocks.types[want] {
			t.Errorf("resource %s was not registered", want)
		}
	}
	for typ := range mocks.types {
		if typ == "pulumi:pulumi:Stack" {
			continue
		}
		if len(typ) >= 4 && (typ[:4] == "aws:" || typ[:4] == "gcp:") {
			t.Errorf("Civo provisioning registered another cloud's resource: %s", typ)
		}
		if len(typ) >= 13 && typ[:13] == "azure-native:" {
			t.Errorf("Civo provisioning registered another cloud's resource: %s", typ)
		}
	}

	cluster := mocks.inputs["civo:index/kubernetesCluster:KubernetesCluster"]
	if got := cluster["region"]; !got.IsString() || got.StringValue() != "nyc1" {
		t.Errorf("cluster region = %v, want nyc1", got)
	}
	if got := cluster["name"]; !got.IsString() || got.StringValue() != "witself-civo-sandbox-use1-dev" {
		t.Errorf("cluster name = %v, want witself-civo-sandbox-use1-dev", got)
	}
	if got := cluster["applications"]; !got.IsString() || got.StringValue() != "traefik2-nodeport" {
		t.Errorf("cluster applications = %v, want traefik2-nodeport", got)
	}
	pools := cluster["pools"].ObjectValue()
	if got := pools["nodeCount"]; !got.IsNumber() || got.NumberValue() != 1 {
		t.Errorf("node count = %v, want 1", got)
	}
	if got := pools["size"]; !got.IsString() || got.StringValue() != "g4s.kube.medium" {
		t.Errorf("node size = %v, want g4s.kube.medium", got)
	}
}

func TestProvisionCivoRequiresValidAdminCIDR(t *testing.T) {
	for _, cidr := range []string{"", "0.0.0.0"} {
		err := pulumi.RunErr(func(ctx *pulumi.Context) error {
			return provisionCivo(ctx, civoCell{
				name: "civo-sandbox-use1-dev", region: "nyc1",
				nodeSize: "g4s.kube.medium", adminCIDR: cidr,
			})
		}, pulumi.WithMocks("witself-infra", "civo-sandbox-use1-dev", &civoResourceMocks{}))
		if err == nil {
			t.Errorf("admin CIDR %q unexpectedly accepted", cidr)
		}
	}
}

type civoResourceMocks struct {
	types  map[string]bool
	inputs map[string]resource.PropertyMap
}

func (m *civoResourceMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	if m.types == nil {
		m.types = map[string]bool{}
		m.inputs = map[string]resource.PropertyMap{}
	}
	m.types[args.TypeToken] = true
	m.inputs[args.TypeToken] = args.Inputs
	return args.Name + "-id", args.Inputs, nil
}

func (*civoResourceMocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return args.Args, nil
}
