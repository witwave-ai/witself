package cell

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/internals"
)

type civoShapeMocks struct {
	deletionProtectionMocks
	shapeMu        sync.Mutex
	zoneName       string
	zoneID         string
	zoneAnswerName string
	zoneLookups    []string
	serviceStatus  *resource.PropertyValue
}

func (m *civoShapeMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	id, outputs, err := m.deletionProtectionMocks.NewResource(args)
	if err != nil {
		return "", nil, err
	}
	m.shapeMu.Lock()
	defer m.shapeMu.Unlock()
	switch args.TypeToken {
	case "civo:index/kubernetesCluster:KubernetesCluster":
		outputs["dnsEntry"] = resource.NewStringProperty("fixture-cluster.k8s.civo.test.")
	case "kubernetes:core/v1:Service":
		status := resource.NewObjectProperty(resource.NewPropertyMapFromMap(map[string]interface{}{
			"loadBalancer": map[string]interface{}{"ingress": []interface{}{map[string]interface{}{"ip": "203.0.113.20", "hostname": "fixture.lb.civo.test"}}},
		}))
		if m.serviceStatus != nil {
			status = *m.serviceStatus
		}
		outputs["status"] = status
	}
	return id, outputs, nil
}

func (m *civoShapeMocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	if args.Token != "cloudflare:index/getZones:getZones" {
		return m.deletionProtectionMocks.Call(args)
	}
	m.shapeMu.Lock()
	defer m.shapeMu.Unlock()
	name := args.Args["name"].StringValue()
	m.zoneLookups = append(m.zoneLookups, name)
	results := []resource.PropertyValue{}
	if name == m.zoneName {
		answer := m.zoneName
		if m.zoneAnswerName != "" {
			answer = m.zoneAnswerName
		}
		results = append(results, resource.NewObjectProperty(resource.PropertyMap{"id": resource.NewStringProperty(m.zoneID), "name": resource.NewStringProperty(answer)}))
	}
	return resource.PropertyMap{"results": resource.NewArrayProperty(results)}, nil
}

type civoShapeRecord struct {
	Type                       string                 `json:"type"`
	Name                       string                 `json:"name"`
	Custom                     bool                   `json:"custom"`
	Parent                     string                 `json:"parent"`
	Provider                   string                 `json:"provider"`
	Providers                  map[string]string      `json:"providers"`
	ImportID                   string                 `json:"importId"`
	DeletedWith                string                 `json:"deletedWith"`
	PackageRef                 string                 `json:"packageRef"`
	Protect                    *bool                  `json:"protect"`
	RetainOnDelete             *bool                  `json:"retainOnDelete"`
	DeleteBeforeReplace        bool                   `json:"deleteBeforeReplace"`
	DeleteBeforeReplaceDefined bool                   `json:"deleteBeforeReplaceDefined"`
	IgnoreChanges              []string               `json:"ignoreChanges"`
	ReplaceOnChanges           []string               `json:"replaceOnChanges"`
	AdditionalSecretOutputs    []string               `json:"additionalSecretOutputs"`
	HideDiffs                  []string               `json:"hideDiffs"`
	ReplaceWith                []string               `json:"replaceWith"`
	AliasURNs                  []string               `json:"aliasURNs"`
	AliasCount                 int                    `json:"aliasCount"`
	AliasSpecs                 bool                   `json:"aliasSpecs"`
	TransformCount             int                    `json:"transformCount"`
	ReplacementTriggerSet      bool                   `json:"replacementTriggerSet"`
	EnvVarMappings             map[string]string      `json:"envVarMappings"`
	Hooks                      map[string][]string    `json:"hooks"`
	CustomTimeouts             map[string]string      `json:"customTimeouts"`
	Dependencies               []string               `json:"dependencies"`
	PropertyDependencies       map[string][]string    `json:"propertyDependencies"`
	Inputs                     map[string]interface{} `json:"inputs"`
}

type civoShape struct {
	Exports   []string          `json:"exports"`
	Resources []civoShapeRecord `json:"resources"`
	// Resolved exports are available to ingress assertions but not part of the golden shape.
	Values map[string]interface{} `json:"-"`
}

func shapeStrings(values []string) []string { return append([]string{}, values...) }
func shapeStringMap(values map[string]string) map[string]string {
	result := map[string]string{}
	for key, value := range values {
		result[key] = value
	}
	return result
}

func recordCivoShape(t *testing.T, args pulumi.MockResourceArgs) civoShapeRecord {
	t.Helper()
	r := args.RegisterRPC
	if r == nil {
		t.Fatalf("registration %s %s has no register request", args.TypeToken, args.Name)
	}
	deps := shapeStrings(r.GetDependencies())
	sort.Strings(deps)
	propertyDeps := map[string][]string{}
	for key, dep := range r.GetPropertyDependencies() {
		urns := shapeStrings(dep.GetUrns())
		sort.Strings(urns)
		propertyDeps[key] = urns
	}
	hooks := r.GetHooks()
	timeouts := r.GetCustomTimeouts()
	inputs := r.GetObject().AsMap()
	if inputs == nil {
		inputs = map[string]interface{}{}
	}
	return civoShapeRecord{
		Type: r.GetType(), Name: r.GetName(), Custom: r.GetCustom(), Parent: r.GetParent(), Provider: r.GetProvider(),
		Providers: shapeStringMap(r.GetProviders()), ImportID: r.GetImportId(), DeletedWith: r.GetDeletedWith(), PackageRef: r.GetPackageRef(),
		Protect: r.Protect, RetainOnDelete: r.RetainOnDelete,
		DeleteBeforeReplace: r.GetDeleteBeforeReplace(), DeleteBeforeReplaceDefined: r.GetDeleteBeforeReplaceDefined(),
		IgnoreChanges: shapeStrings(r.GetIgnoreChanges()), ReplaceOnChanges: shapeStrings(r.GetReplaceOnChanges()),
		AdditionalSecretOutputs: shapeStrings(r.GetAdditionalSecretOutputs()), HideDiffs: shapeStrings(r.GetHideDiffs()), ReplaceWith: shapeStrings(r.GetReplaceWith()), AliasURNs: shapeStrings(r.GetAliasURNs()),
		AliasCount: len(r.GetAliases()), AliasSpecs: r.GetAliasSpecs(), TransformCount: len(r.GetTransforms()), ReplacementTriggerSet: r.GetReplacementTrigger() != nil,
		EnvVarMappings: shapeStringMap(r.GetEnvVarMappings()),
		Hooks:          map[string][]string{"beforeCreate": shapeStrings(hooks.GetBeforeCreate()), "afterCreate": shapeStrings(hooks.GetAfterCreate()), "beforeUpdate": shapeStrings(hooks.GetBeforeUpdate()), "afterUpdate": shapeStrings(hooks.GetAfterUpdate()), "beforeDelete": shapeStrings(hooks.GetBeforeDelete()), "afterDelete": shapeStrings(hooks.GetAfterDelete()), "onError": shapeStrings(hooks.GetOnError())},
		CustomTimeouts: map[string]string{"create": timeouts.GetCreate(), "update": timeouts.GetUpdate(), "delete": timeouts.GetDelete()},
		Dependencies:   deps, PropertyDependencies: propertyDeps, Inputs: inputs,
	}
}

func runCivoShape(t *testing.T, stack string, config map[string]string, mocks *civoShapeMocks) (civoShape, error) {
	t.Helper()
	for _, name := range []string{"PULUMI_K8S_DELETE_UNREACHABLE", "PULUMI_K8S_ENABLE_CONFIGMAP_MUTABLE", "PULUMI_K8S_ENABLE_PATCH_FORCE", "PULUMI_K8S_ENABLE_SECRET_MUTABLE", "PULUMI_K8S_ENABLE_SERVER_SIDE_APPLY", "PULUMI_K8S_SKIP_UPDATE_UNREACHABLE", "PULUMI_K8S_SUPPRESS_DEPRECATION_WARNINGS", "PULUMI_K8S_SUPPRESS_HELM_HOOK_WARNINGS", "PULUMI_K8S_UPSERT_EXISTING_OBJECTS"} {
		t.Setenv(name, "")
	}
	shape := civoShape{Exports: []string{}, Resources: []civoShapeRecord{}, Values: map[string]interface{}{}}
	outputs := map[string]pulumi.Output{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		if err := Program(ctx); err != nil {
			return err
		}
		for key, value := range ctx.GetCurrentExportMap() {
			shape.Exports = append(shape.Exports, key)
			if key != "apiHost" && key != "loadBalancerIP" && key != "civoDNSEntry" && key != "cloudflareDNSZone" {
				continue
			}
			outputs[key] = pulumi.ToOutput(value)
		}
		return nil
	}, pulumi.WithMocks("witself-infra", stack, mocks), func(info *pulumi.RunInfo) { info.Config = config })
	// RunErr waits for resource RPCs, but a detached ApplyT on an already-known
	// export can still be running. Await these public outputs explicitly.
	if err == nil {
		for key, output := range outputs {
			resolved, awaitErr := internals.UnsafeAwaitOutput(t.Context(), output)
			if awaitErr != nil {
				err = awaitErr
				break
			}
			if !resolved.Known {
				t.Fatalf("public export %s is unknown under mocks", key)
			}
			shape.Values[key] = resolved.Value
		}
	}
	mocks.registrationMu.Lock()
	defer mocks.registrationMu.Unlock()
	for _, args := range mocks.registrations {
		shape.Resources = append(shape.Resources, recordCivoShape(t, args))
	}
	sort.Strings(shape.Exports)
	sort.Slice(shape.Resources, func(i, j int) bool {
		if shape.Resources[i].Type != shape.Resources[j].Type {
			return shape.Resources[i].Type < shape.Resources[j].Type
		}
		return shape.Resources[i].Name < shape.Resources[j].Name
	})
	return shape, err
}

type civoShapeCase struct {
	name               string
	config             map[string]string
	resources, exports int
}

func civoShapeCases() []civoShapeCase {
	cases := []civoShapeCase{
		{name: "argocd-prod-pinned-token", resources: 15, exports: 21},
		{name: "argocd-minimal-generated", resources: 16, exports: 21},
		{name: "plain-minimal", resources: 3, exports: 16},
	}
	for i := range cases {
		cases[i].config = map[string]string{
			"witself:cloud": "civo", "witself:deletionProtection": "true", "witself:accountAlias": "fixture", "witself:role": "serving", "witself:channel": "experimental",
			"witself:gitopsRepo": "https://github.com/witwave-ai/witself", "witself:gitopsPath": ".gitops/charts/bootstrap", "witself:gitopsValuesPath": ".gitops/cells/civo-fixture-use1-serving/values.yaml", "witself:gitopsRevision": "main",
			"witself:civoNodeSize": "g4s.kube.medium", "witself:civoAdminCIDR": "203.0.113.7/32", "civo:region": "nyc1",
			"witself:profile": "minimal", "witself:argocd": "true", "witself:k8sVersion": "",
		}
	}
	cases[0].config["witself:profile"] = "prod"
	cases[0].config["witself:k8sVersion"] = "1.35.0-k3s1"
	cases[0].config["witself:bootstrapToken"] = "fixture-bootstrap-token-not-real"
	cases[2].config["witself:argocd"] = "false"
	return cases
}

func marshalCivoShape(t *testing.T, shape civoShape) []byte {
	t.Helper()
	data, err := json.MarshalIndent(shape, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func compareCivoShape(t *testing.T, got civoShape, expected []byte) {
	t.Helper()
	if bytes.Equal(marshalCivoShape(t, got), expected) {
		return
	}
	var want civoShape
	if err := json.Unmarshal(expected, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Exports, want.Exports) {
		t.Errorf("export keys differ: got %v, want %v", got.Exports, want.Exports)
	}
	records := map[string]civoShapeRecord{}
	for _, item := range want.Resources {
		records[item.Type+" / "+item.Name] = item
	}
	for _, item := range got.Resources {
		key := item.Type + " / " + item.Name
		if old, ok := records[key]; !ok || !reflect.DeepEqual(item, old) {
			t.Errorf("record differs: %s", key)
		}
		delete(records, key)
	}
	for key := range records {
		t.Errorf("record missing: %s", key)
	}
	t.Error("default Civo shape differs from base")
}

// These files describe the program of the base commit of slice 64. Regenerate
// only for a deliberate change of the default Civo shape, or after an upgrade
// of the Pulumi SDK or a provider SDK that changes a recorded field, and review
// the regenerated diff line by line. Provider versions are not recorded:
// TestCivoDefaultShapeVersionsFollowGoMod compares them with go.mod. The nine
// PULUMI_K8S_* variables are emptied because the Kubernetes SDK copies them into
// the provider's inputs.
func TestCivoDefaultShapeMatchesBase(t *testing.T) {
	update := os.Getenv("WITSELF_UPDATE_CIVO_SHAPE") == "1"
	for _, tc := range civoShapeCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLOUDFLARE_API_TOKEN", "")
			shape, err := runCivoShape(t, "civo-fixture-use1-serving", tc.config, &civoShapeMocks{})
			if err != nil {
				t.Fatal(err)
			}
			if len(shape.Resources) != tc.resources || len(shape.Exports) != tc.exports {
				t.Errorf("counts resources=%d exports=%d, want %d and %d", len(shape.Resources), len(shape.Exports), tc.resources, tc.exports)
			}
			path := filepath.Join("testdata", "civo-default-shape", tc.name+".json")
			if update {
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, marshalCivoShape(t, shape), 0644); err != nil {
					t.Fatal(err)
				}
				return
			}
			expected, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			compareCivoShape(t, shape, expected)
			t.Setenv("CLOUDFLARE_API_TOKEN", "test-cloudflare-token-not-real")
			withToken, err := runCivoShape(t, "civo-fixture-use1-serving", tc.config, &civoShapeMocks{})
			if err != nil {
				t.Fatal(err)
			}
			compareCivoShape(t, withToken, expected)
		})
	}
	if update {
		t.Fatal("golden files written; unset WITSELF_UPDATE_CIVO_SHAPE and run the test again")
	}
}

func TestCivoDefaultShapeVersionsFollowGoMod(t *testing.T) {
	data, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	versions := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			versions[fields[0]] = strings.TrimPrefix(fields[1], "v")
		}
	}
	for _, tc := range civoShapeCases() {
		t.Run(tc.name, func(t *testing.T) {
			mocks := &civoShapeMocks{}
			if _, err := runCivoShape(t, "civo-fixture-use1-serving", tc.config, mocks); err != nil {
				t.Fatal(err)
			}
			for _, args := range mocks.registrations {
				var module string
				switch {
				case strings.HasPrefix(args.TypeToken, "civo:"):
					module = "github.com/pulumi/pulumi-civo/sdk/v2"
				case strings.HasPrefix(args.TypeToken, "kubernetes:"), args.TypeToken == "pulumi:providers:kubernetes":
					module = "github.com/pulumi/pulumi-kubernetes/sdk/v4"
				case strings.HasPrefix(args.TypeToken, "random:"):
					module = "github.com/pulumi/pulumi-random/sdk/v4"
				default:
					t.Errorf("unmapped type %s %s", args.TypeToken, args.Name)
					continue
				}
				want := versions[module]
				if want == "" {
					t.Fatalf("module %s is not pinned", module)
				}
				// The untyped CustomResource constructor forwards options unchanged;
				// unlike generated typed constructors, it adds no SDK version.
				if args.TypeToken == "kubernetes:argoproj.io/v1alpha1:Application" && args.Name == "argocd-root" {
					want = ""
				}
				if got := args.RegisterRPC.GetVersion(); got != want {
					t.Errorf("%s %s version %q, want %q", args.TypeToken, args.Name, got, want)
				}
				if args.RegisterRPC.GetPluginDownloadURL() != "" {
					t.Errorf("%s %s has a plugin download URL", args.TypeToken, args.Name)
				}
			}
		})
	}
}
