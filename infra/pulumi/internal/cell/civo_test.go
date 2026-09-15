package cell

import (
	"fmt"
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
			nodeCount: civoNodeProfileFor("minimal"),
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
	if got, ok := cluster["kubernetesVersion"]; ok {
		t.Errorf("unconfigured Kubernetes version = %v, want omitted for Civo latest stable", got)
	}
	pools := cluster["pools"].ObjectValue()
	if got := pools["nodeCount"]; !got.IsNumber() || got.NumberValue() != 1 {
		t.Errorf("node count = %v, want 1", got)
	}
	if got := pools["size"]; !got.IsString() || got.StringValue() != "g4s.kube.medium" {
		t.Errorf("node size = %v, want g4s.kube.medium", got)
	}
	if got := pools["label"]; !got.IsString() || got.StringValue() != "development" {
		t.Errorf("pool label = %v, want development", got)
	}
}

func TestProvisionCivoProdProfileOnlyChangesNodeCount(t *testing.T) {
	minimalPool := civoPoolForProfile(t, "minimal")
	prodPool := civoPoolForProfile(t, "prod")

	if got := minimalPool["nodeCount"]; !got.IsNumber() || got.NumberValue() != 1 {
		t.Errorf("minimal node count = %v, want 1", got)
	}
	if got := prodPool["nodeCount"]; !got.IsNumber() || got.NumberValue() != 2 {
		t.Errorf("prod node count = %v, want 2", got)
	}
	for _, key := range []resource.PropertyKey{"label", "size"} {
		minimal := minimalPool[key]
		prod := prodPool[key]
		if !minimal.IsString() || !prod.IsString() || prod.StringValue() != minimal.StringValue() {
			t.Errorf("prod pool %s = %v, want unchanged from minimal (%v)", key, prod, minimal)
		}
	}
	minimalPoolShape := minimalPool.Copy()
	prodPoolShape := prodPool.Copy()
	delete(minimalPoolShape, "nodeCount")
	delete(prodPoolShape, "nodeCount")
	if !resource.NewObjectProperty(prodPoolShape).DeepEquals(resource.NewObjectProperty(minimalPoolShape)) {
		t.Errorf("prod pool properties except nodeCount = %v, want unchanged from minimal (%v)", prodPoolShape, minimalPoolShape)
	}
}

func civoPoolForProfile(t *testing.T, profile string) resource.PropertyMap {
	t.Helper()
	mocks := &civoResourceMocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		return provisionCivo(ctx, civoCell{
			name:      "civo-sandbox-use1-dev",
			region:    "nyc1",
			profile:   profile,
			nodeSize:  "g4s.kube.medium",
			nodeCount: civoNodeProfileFor(profile),
			adminCIDR: "203.0.113.7/32",
		})
	}, pulumi.WithMocks("witself-infra", "civo-sandbox-use1-dev", mocks))
	if err != nil {
		t.Fatalf("provision Civo with profile %q: %v", profile, err)
	}
	return mocks.inputs["civo:index/kubernetesCluster:KubernetesCluster"]["pools"].ObjectValue()
}

func TestCivoNodeProfileFor(t *testing.T) {
	tests := []struct {
		name    string
		profile string
		want    int
	}{
		{name: "prod", profile: "prod", want: 2},
		{name: "minimal", profile: "minimal", want: 1},
		{name: "empty", profile: "", want: 1},
		{name: "other", profile: "other", want: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := civoNodeProfileFor(test.profile); got != test.want {
				t.Errorf("civoNodeProfileFor(%q) = %d, want %d", test.profile, got, test.want)
			}
		})
	}
}

func TestProvisionCivoPinsExplicitKubernetesVersion(t *testing.T) {
	unpinned := civoClusterForVersion(t, "")
	pinned := civoClusterForVersion(t, "1.35.0-k3s1")
	if got, ok := unpinned["kubernetesVersion"]; ok {
		t.Errorf("unconfigured Kubernetes version = %v, want omitted for Civo latest stable", got)
	}
	if got := pinned["kubernetesVersion"]; !got.IsString() || got.StringValue() != "1.35.0-k3s1" {
		t.Errorf("kubernetesVersion = %v, want 1.35.0-k3s1", got)
	}
	pinnedShape := pinned.Copy()
	delete(pinnedShape, "kubernetesVersion")
	if !resource.NewObjectProperty(pinnedShape).DeepEquals(resource.NewObjectProperty(unpinned)) {
		t.Errorf("pinned cluster properties except kubernetesVersion = %v, want unchanged from unpinned (%v)", pinnedShape, unpinned)
	}
}

func civoClusterForVersion(t *testing.T, version string) resource.PropertyMap {
	t.Helper()
	// Deliberately differ from the requested pin to prove health's export comes
	// from provider state, including when no version was configured.
	const providerVersion = "1.35.1-k3s1"
	mocks := &civoResourceMocks{kubernetesVersion: providerVersion}
	var exportedVersion string
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		if err := Program(ctx); err != nil {
			return err
		}
		output, ok := ctx.GetCurrentExportMap()["kubernetesVersion"].(pulumi.StringOutput)
		if !ok {
			return fmt.Errorf("kubernetesVersion export is missing or is not a string output")
		}
		output.ApplyT(func(value string) string {
			exportedVersion = value
			return value
		})
		return nil
	}, pulumi.WithMocks("witself-infra", "civo-sandbox-use1-dev", mocks), func(info *pulumi.RunInfo) {
		info.Config = map[string]string{
			"witself:cloud":         "civo",
			"witself:profile":       "minimal",
			"witself:civoNodeSize":  "g4s.kube.medium",
			"witself:civoAdminCIDR": "203.0.113.7/32",
			"civo:region":           "nyc1",
		}
		if version != "" {
			info.Config["witself:k8sVersion"] = version
		}
	})
	if err != nil {
		t.Fatalf("Civo program with version %q: %v", version, err)
	}
	if exportedVersion != providerVersion {
		t.Errorf("kubernetesVersion export with configured version %q = %q, want provider version %q", version, exportedVersion, providerVersion)
	}
	return mocks.inputs["civo:index/kubernetesCluster:KubernetesCluster"]
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
	types             map[string]bool
	inputs            map[string]resource.PropertyMap
	kubernetesVersion string
}

func (m *civoResourceMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	if m.types == nil {
		m.types = map[string]bool{}
		m.inputs = map[string]resource.PropertyMap{}
	}
	m.types[args.TypeToken] = true
	m.inputs[args.TypeToken] = args.Inputs
	outputs := args.Inputs.Copy()
	if args.TypeToken == "civo:index/kubernetesCluster:KubernetesCluster" && m.kubernetesVersion != "" {
		outputs["kubernetesVersion"] = resource.NewStringProperty(m.kubernetesVersion)
	}
	return args.Name + "-id", outputs, nil
}

func (*civoResourceMocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return args.Args, nil
}
