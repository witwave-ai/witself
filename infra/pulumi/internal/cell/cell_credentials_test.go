package cell

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws"
	"github.com/pulumi/pulumi-civo/sdk/v2/go/civo"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func TestAWSProvisionSecretContainsIndependentBackupCredential(t *testing.T) {
	mocks := &credentialResourceMocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		provider, err := aws.NewProvider(ctx, "aws-test", &aws.ProviderArgs{
			Region: pulumi.String("us-west-2"),
		})
		if err != nil {
			return err
		}
		return provisionAWSProvisionSecret(ctx, awsCell{
			name: "aws-sandbox-usw2-dev",
		}, provider)
	}, pulumi.WithMocks("witself-infra", "aws-sandbox-usw2-dev", mocks))
	if err != nil {
		t.Fatalf("provision AWS credentials: %v", err)
	}

	inputs := mocks.resourceInputs(
		t, "aws:secretsmanager/secretVersion:SecretVersion",
		"witself-provision-token",
	)
	assertJSONCredentialPayload(t, inputs["secretString"])
	mocks.assertCredentialRandomInputs(t, "witself-provision-token")
	mocks.assertCredentialRandomInputs(t, "witself-backup-token")
	mocks.assertNoProviderBackupSecret(t)
}

func TestGCPProvisionSecretContainsIndependentBackupCredential(t *testing.T) {
	mocks := &credentialResourceMocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		provider, err := gcp.NewProvider(ctx, "gcp-test", &gcp.ProviderArgs{
			Project: pulumi.String("witself-test"),
			Region:  pulumi.String("us-west2"),
		})
		if err != nil {
			return err
		}
		var dependency credentialTestDependency
		if err := ctx.RegisterComponentResource(
			"witself:test:Dependency", "secret-manager-api", &dependency,
		); err != nil {
			return err
		}
		_, err = provisionGCPProvisionSecret(ctx, gcpCell{
			name:    "gcp-sandbox-usw2-dev",
			project: "witself-test",
			region:  "us-west2",
		}, provider, &dependency)
		return err
	}, pulumi.WithMocks("witself-infra", "gcp-sandbox-usw2-dev", mocks))
	if err != nil {
		t.Fatalf("provision GCP credentials: %v", err)
	}

	inputs := mocks.resourceInputs(
		t, "gcp:secretmanager/secretVersion:SecretVersion",
		"witself-provision-token",
	)
	assertJSONCredentialPayload(t, inputs["secretData"])
	mocks.assertNoProviderBackupSecret(t)
}

func TestAzureProvisionSecretContainsIndependentBackupCredential(t *testing.T) {
	mocks := &credentialResourceMocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		var database credentialTestDependency
		if err := ctx.RegisterComponentResource(
			"witself:test:Dependency", "database", &database,
		); err != nil {
			return err
		}
		_, err := provisionAzureSecrets(
			ctx,
			azureCell{
				name:    "azure-sandbox-usw2-dev",
				region:  "westus2",
				profile: "minimal",
			},
			&azureNetwork{
				resourceGroupName: pulumi.String("witself-test-rg").ToStringOutput(),
			},
			&azureDatabase{
				fqdn:     pulumi.String("db.private.example").ToStringOutput(),
				password: pulumi.String("test-password").ToStringOutput(),
				dsn:      pulumi.String("postgres://test").ToStringOutput(),
				database: &database,
			},
		)
		return err
	}, pulumi.WithMocks("witself-infra", "azure-sandbox-usw2-dev", mocks))
	if err != nil {
		t.Fatalf("provision Azure credentials: %v", err)
	}

	inputs := mocks.resourceInputs(
		t, "azure-native:keyvault:Secret", "witself-provision-token",
	)
	properties := plainProperty(t, inputs["properties"]).ObjectValue()
	assertJSONCredentialPayload(t, properties["value"])
	mocks.assertNoProviderBackupSecret(t)
}

func TestCivoProvisionSecretContainsIndependentBackupCredential(t *testing.T) {
	mocks := &credentialResourceMocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		cluster, err := civo.NewKubernetesCluster(
			ctx, "test-cluster", &civo.KubernetesClusterArgs{
				Name:        pulumi.String("witself-civo-sandbox-usw2-dev"),
				Region:      pulumi.String("PHX1"),
				NetworkId:   pulumi.String("network-id"),
				FirewallId:  pulumi.String("firewall-id"),
				ClusterType: pulumi.String("k3s"),
				Cni:         pulumi.String("cilium"),
				Pools: civo.KubernetesClusterPoolsArgs{
					Label:     pulumi.String("development"),
					NodeCount: pulumi.Int(1),
					Size:      pulumi.String("g4s.kube.medium"),
				},
			},
		)
		if err != nil {
			return err
		}
		return provisionCivoArgoCD(
			ctx,
			civoCell{
				name:             "civo-sandbox-usw2-dev",
				region:           "PHX1",
				gitopsRepo:       "https://github.com/witwave-ai/witself",
				gitopsPath:       ".gitops/charts/bootstrap",
				gitopsValuesPath: ".gitops/cells/civo-sandbox-usw2-dev/values.yaml",
				gitopsRevision:   "main",
			},
			cluster,
			pulumi.String("civo-sandbox-usw2-dev.civo.com"),
			pulumi.String("civo-sandbox-usw2-dev.civo.com"),
		)
	}, pulumi.WithMocks("witself-infra", "civo-sandbox-usw2-dev", mocks))
	if err != nil {
		t.Fatalf("provision Civo credentials: %v", err)
	}

	inputs := mocks.resourceInputs(
		t, "kubernetes:core/v1:Secret", "witself-provision",
	)
	stringData := plainProperty(t, inputs["stringData"]).ObjectValue()
	assertCredentialPair(
		t,
		plainString(t, stringData["token"]),
		plainString(t, stringData["backup_token"]),
	)
	if !inputs["stringData"].ContainsSecrets() {
		t.Fatal("Civo provision Secret stringData is not marked secret")
	}
	mocks.assertNoProviderBackupSecret(t)
}

type credentialTestDependency struct {
	pulumi.ResourceState
}

type capturedCredentialResource struct {
	typ    string
	name   string
	inputs resource.PropertyMap
}

type credentialResourceMocks struct {
	mu        sync.Mutex
	resources []capturedCredentialResource
}

func (m *credentialResourceMocks) NewResource(
	args pulumi.MockResourceArgs,
) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	m.resources = append(m.resources, capturedCredentialResource{
		typ: args.TypeToken, name: args.Name, inputs: args.Inputs.Copy(),
	})
	m.mu.Unlock()

	outputs := args.Inputs.Copy()
	switch args.TypeToken {
	case "random:index/randomString:RandomString":
		body := strings.Repeat("R", 43)
		switch args.Name {
		case "witself-provision-token":
			body = strings.Repeat("P", 43)
		case "witself-backup-token":
			body = strings.Repeat("B", 43)
		case "witself-bootstrap-token":
			body = strings.Repeat("T", 43)
		}
		// Keep the provider result deliberately plain. The resource's
		// AdditionalSecretOutputs option, not a helpful mock provider, must
		// protect the generated token and propagate secrecy to every consumer.
		outputs["result"] = resource.NewStringProperty(body)
	case "random:index/randomPassword:RandomPassword":
		outputs["result"] = resource.MakeSecret(
			resource.NewStringProperty("test-database-password"),
		)
	case "random:index/randomId:RandomId":
		outputs["hex"] = resource.NewStringProperty("abcdef")
	case "civo:index/kubernetesCluster:KubernetesCluster":
		outputs["kubeconfig"] = resource.MakeSecret(
			resource.NewStringProperty(
				"apiVersion: v1\nclusters: []\ncontexts: []\n",
			),
		)
		outputs["apiEndpoint"] = resource.NewStringProperty("https://cluster.example")
		outputs["ready"] = resource.NewBoolProperty(true)
		outputs["kubernetesVersion"] = resource.NewStringProperty("1.35.0-k3s1")
	case "azure-native:keyvault:Vault":
		if properties, ok := outputs["properties"]; ok && properties.IsObject() {
			value := properties.ObjectValue().Copy()
			value["vaultUri"] = resource.NewStringProperty(
				"https://witself-test.vault.azure.net/",
			)
			outputs["properties"] = resource.NewObjectProperty(value)
		}
	}
	return args.Name + "-id", outputs, nil
}

func (*credentialResourceMocks) Call(
	args pulumi.MockCallArgs,
) (resource.PropertyMap, error) {
	if args.Token == "azure-native:authorization:getClientConfig" {
		return resource.PropertyMap{
			"clientId":       resource.NewStringProperty("client-id"),
			"objectId":       resource.NewStringProperty("object-id"),
			"subscriptionId": resource.NewStringProperty("subscription-id"),
			"tenantId":       resource.NewStringProperty("tenant-id"),
		}, nil
	}
	return args.Args, nil
}

func (m *credentialResourceMocks) resourceInputs(
	t *testing.T, typ, name string,
) resource.PropertyMap {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, item := range m.resources {
		if item.typ == typ && item.name == name {
			return item.inputs
		}
	}
	t.Fatalf("resource %s %q was not registered; got %#v", typ, name, m.resources)
	return nil
}

func (m *credentialResourceMocks) assertNoProviderBackupSecret(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, item := range m.resources {
		if strings.Contains(strings.ToLower(item.name), "backup") &&
			strings.Contains(strings.ToLower(item.typ), "secret") &&
			item.typ != "random:index/randomString:RandomString" {
			t.Fatalf(
				"backup credential unexpectedly created a separate provider secret: %s %q",
				item.typ, item.name,
			)
		}
	}
}

func (m *credentialResourceMocks) assertCredentialRandomInputs(
	t *testing.T, name string,
) {
	t.Helper()
	inputs := m.resourceInputs(
		t, "random:index/randomString:RandomString", name,
	)
	if got := inputs["length"]; !got.IsNumber() || got.NumberValue() != 43 {
		t.Fatalf("%s length = %v, want 43", name, got)
	}
	for _, key := range []resource.PropertyKey{
		"lower", "numeric", "upper",
	} {
		if got := inputs[key]; !got.IsBool() || !got.BoolValue() {
			t.Fatalf("%s %s = %v, want true", name, key, got)
		}
	}
	if got := inputs["special"]; !got.IsBool() || got.BoolValue() {
		t.Fatalf("%s special = %v, want false", name, got)
	}
}

func assertJSONCredentialPayload(t *testing.T, input resource.PropertyValue) {
	t.Helper()
	if !input.ContainsSecrets() {
		t.Fatal("provider provision secret payload is not marked secret")
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(plainString(t, input)), &payload); err != nil {
		t.Fatalf("decode credential payload: %v", err)
	}
	if len(payload) != 2 {
		t.Fatalf("credential payload fields = %#v, want only token and backup_token", payload)
	}
	assertCredentialPair(t, payload["token"], payload["backup_token"])
}

func assertCredentialPair(t *testing.T, provisionToken, backupToken string) {
	t.Helper()
	if !strings.HasPrefix(provisionToken, "witself_prv_") {
		t.Fatalf("provision token prefix = %q", provisionToken)
	}
	if !strings.HasPrefix(backupToken, "witself_bak_") {
		t.Fatalf("backup token prefix = %q", backupToken)
	}
	if provisionToken == backupToken {
		t.Fatal("provision and backup credentials are identical")
	}
}

func plainString(t *testing.T, value resource.PropertyValue) string {
	t.Helper()
	value = plainProperty(t, value)
	if !value.IsString() {
		t.Fatalf("property = %v, want string", value)
	}
	return value.StringValue()
}

func plainProperty(t *testing.T, value resource.PropertyValue) resource.PropertyValue {
	t.Helper()
	for {
		switch {
		case value.IsSecret():
			value = value.SecretValue().Element
		case value.IsOutput():
			output := value.OutputValue()
			if !output.Known {
				t.Fatal("property output is unknown")
			}
			value = output.Element
		default:
			return value
		}
	}
}
