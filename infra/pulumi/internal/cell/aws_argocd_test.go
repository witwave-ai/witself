package cell

import (
	"testing"

	"github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/helm/v3"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func TestArgoCDReleaseValuesObserveApplicationStatusUpdates(t *testing.T) {
	values := argocdReleaseValues()
	configs, ok := values["configs"].(pulumi.Map)
	if !ok {
		t.Fatal("Argo CD values contain no configs map")
	}
	cm, ok := configs["cm"].(pulumi.Map)
	if !ok {
		t.Fatal("Argo CD values contain no configs.cm map")
	}
	if got := cm["resource.ignoreResourceUpdatesEnabled"]; got != pulumi.String("false") {
		t.Fatalf("resource.ignoreResourceUpdatesEnabled = %v, want false", got)
	}
}

func TestArgoCDReleaseSerializesStatusUpdateSetting(t *testing.T) {
	mocks := &captureArgoReleaseMocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		_, err := helm.NewRelease(ctx, "argocd", &helm.ReleaseArgs{
			Chart:  pulumi.String(argocdChart),
			Values: argocdReleaseValues(),
		})
		return err
	}, pulumi.WithMocks("witself-infra", "test", mocks))
	if err != nil {
		t.Fatalf("register Argo CD release: %v", err)
	}
	if mocks.releaseInputs == nil {
		t.Fatal("Argo CD release was not registered")
	}

	values := mocks.releaseInputs["values"].ObjectValue()
	configs := values["configs"].ObjectValue()
	cm := configs["cm"].ObjectValue()
	key := resource.PropertyKey("resource.ignoreResourceUpdatesEnabled")
	if got := cm[key]; !got.IsString() || got.StringValue() != "false" {
		t.Fatalf("serialized %s = %v, want false", key, got)
	}
}

type captureArgoReleaseMocks struct {
	releaseInputs resource.PropertyMap
}

func (m *captureArgoReleaseMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	if args.TypeToken == "kubernetes:helm.sh/v3:Release" && args.Name == "argocd" {
		m.releaseInputs = args.Inputs
	}
	return args.Name + "-id", args.Inputs, nil
}

func (*captureArgoReleaseMocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return args.Args, nil
}
