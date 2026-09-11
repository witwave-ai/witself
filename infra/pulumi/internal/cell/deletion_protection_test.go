package cell

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

var cellSecretResourceNames = []string{"witself-db", "witself-bootstrap-token", "witself-provision-token"}

func TestProgramDeletionProtectionConfiguration(t *testing.T) {
	for _, cloud := range []string{"aws", "gcp", "azure"} {
		for _, configured := range []string{"absent", "true", "false", "invalid"} {
			t.Run(cloud+"/"+configured, func(t *testing.T) {
				mocks := &deletionProtectionMocks{}
				config := map[string]string{
					"witself:cloud": cloud, "witself:profile": "minimal",
					"aws:region": "us-west-2", "gcp:project": "witself-test",
					"gcp:region": "us-west2", "azure-native:location": "westus2",
				}
				if configured != "absent" {
					config["witself:deletionProtection"] = configured
				}
				err := pulumi.RunErr(Program, pulumi.WithMocks("witself-infra", cloud+"-test", mocks), func(info *pulumi.RunInfo) {
					info.Config = config
				})
				if configured == "invalid" {
					if err == nil || !strings.Contains(err.Error(), "witself:deletionProtection must be a boolean") {
						t.Fatalf("invalid configuration error = %v", err)
					}
					for _, args := range mocks.registrations {
						if args.Custom {
							t.Fatalf("invalid configuration registered %s %s", args.TypeToken, args.Name)
						}
					}
					return
				}
				if err != nil {
					t.Fatalf("run %s cell program: %v", cloud, err)
				}
				protected := configured != "false"
				switch cloud {
				case "aws":
					mocks.registered(t, "aws:rds/instance:Instance", "witself", protected)
					for _, name := range cellSecretResourceNames {
						mocks.registered(t, "aws:secretsmanager/secret:Secret", name, protected)
					}
				case "gcp":
					mocks.registered(t, "gcp:sql/databaseInstance:DatabaseInstance", "witself", protected)
					for _, name := range cellSecretResourceNames {
						mocks.registered(t, "gcp:secretmanager/secret:Secret", name, protected)
					}
				case "azure":
					mocks.registered(t, "azure-native:dbforpostgresql:Server", "witself", protected)
					mocks.registered(t, "azure-native:keyvault:Vault", "cell", protected)
				}
			})
		}
	}
}

func TestAWSDeletionProtectionResources(t *testing.T) {
	for _, profile := range []string{"minimal", "prod"} {
		for _, protected := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/protected=%t", profile, protected), func(t *testing.T) {
				mocks := &deletionProtectionMocks{}
				err := pulumi.RunErr(func(ctx *pulumi.Context) error {
					return provisionAWS(ctx, awsCell{
						name: "aws-sandbox-usw2-dev", region: "us-west-2", profile: profile,
						cidr: "10.20.0.0/16", dbVersion: "18", k8sVersion: "1.35",
						deletionProtection: protected,
					})
				}, pulumi.WithMocks("witself-infra", "aws-test", mocks))
				if err != nil {
					t.Fatalf("render AWS resources: %v", err)
				}
				db := mocks.registered(t, "aws:rds/instance:Instance", "witself", protected)
				assertProtectionBool(t, db.Inputs, "deletionProtection", protected)
				assertProtectionBool(t, db.Inputs, "skipFinalSnapshot", true)
				recovery := 0
				if protected || profile == "prod" {
					recovery = 30
				}
				for _, name := range cellSecretResourceNames {
					secret := mocks.registered(t, "aws:secretsmanager/secret:Secret", name, protected)
					got := secret.Inputs["recoveryWindowInDays"]
					if !got.IsNumber() || got.NumberValue() != float64(recovery) {
						t.Fatalf("%s recovery window = %v, want %d", name, got, recovery)
					}
					if got, ok := secret.Inputs["forceDeleteWithoutRecovery"]; ok && (!got.IsBool() || got.BoolValue()) {
						t.Fatalf("%s forceDeleteWithoutRecovery = %v", name, got)
					}
					// Version replacement remains available for normal rotation.
					mocks.registered(t, "aws:secretsmanager/secretVersion:SecretVersion", name, false)
				}
				mocks.assertCount(t, "aws:secretsmanager/secret:Secret", 3)
				mocks.assertCount(t, "aws:secretsmanager/secretVersion:SecretVersion", 3)
			})
		}
	}
}

func TestGCPDeletionProtectionResources(t *testing.T) {
	for _, profile := range []string{"minimal", "prod"} {
		for _, protected := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/protected=%t", profile, protected), func(t *testing.T) {
				mocks := &deletionProtectionMocks{}
				err := pulumi.RunErr(func(ctx *pulumi.Context) error {
					provider, err := gcp.NewProvider(ctx, "gcp-test", &gcp.ProviderArgs{Project: pulumi.String("witself-test")})
					if err != nil {
						return err
					}
					var dependency credentialTestDependency
					if err := ctx.RegisterComponentResource("witself:test:Dependency", "apis-and-network", &dependency); err != nil {
						return err
					}
					c := gcpCell{
						name: "gcp-sandbox-usw2-dev", project: "witself-test", region: "us-west2",
						profile: profile, dbVersion: "18", deletionProtection: protected,
					}
					_, err = provisionGCPCloudSQL(ctx, c, &gcpNetwork{
						networkSelfLink:  pulumi.String("projects/witself-test/global/networks/cell").ToStringOutput(),
						privateRangeName: pulumi.String("cell-private").ToStringOutput(), privateConnection: &dependency,
					}, provider, &dependency, &dependency)
					if err != nil {
						return err
					}
					if _, err := provisionGCPBootstrapSecret(ctx, c, provider, &dependency); err != nil {
						return err
					}
					_, err = provisionGCPProvisionSecret(ctx, c, provider, &dependency)
					return err
				}, pulumi.WithMocks("witself-infra", "gcp-test", mocks))
				if err != nil {
					t.Fatalf("render GCP resources: %v", err)
				}
				db := mocks.registered(t, "gcp:sql/databaseInstance:DatabaseInstance", "witself", protected)
				assertProtectionBool(t, db.Inputs, "deletionProtection", protected)
				assertProtectionBool(t, db.Inputs["settings"].ObjectValue(), "deletionProtectionEnabled", protected)
				mocks.registered(t, "gcp:sql/database:Database", "witself", protected)
				for _, name := range cellSecretResourceNames {
					secret := mocks.registered(t, "gcp:secretmanager/secret:Secret", name, protected)
					assertProtectionBool(t, secret.Inputs, "deletionProtection", protected)
					if got := secret.Inputs["deletionPolicy"]; !got.IsString() || got.StringValue() != "DELETE" {
						t.Fatalf("%s deletion policy = %v, want DELETE", name, got)
					}
					if got := secret.Inputs["versionDestroyTtl"]; protected {
						if !got.IsString() || got.StringValue() != "2592000s" {
							t.Fatalf("%s version destruction delay = %v, want 2592000s", name, got)
						}
					} else if !got.IsNull() {
						t.Fatalf("%s unprotected version destruction delay = %v, want absent", name, got)
					}
					mocks.registered(t, "gcp:secretmanager/secretVersion:SecretVersion", name, false)
				}
				mocks.assertCount(t, "gcp:secretmanager/secret:Secret", 3)
				mocks.assertCount(t, "gcp:secretmanager/secretVersion:SecretVersion", 3)
			})
		}
	}
}

func TestAzureDeletionProtectionResources(t *testing.T) {
	for _, profile := range []string{"minimal", "prod"} {
		for _, protected := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/protected=%t", profile, protected), func(t *testing.T) {
				mocks := &deletionProtectionMocks{}
				err := pulumi.RunErr(func(ctx *pulumi.Context) error {
					c := azureCell{
						name: "azure-sandbox-usw2-dev", region: "westus2", profile: profile,
						deletionProtection: protected,
					}
					net := &azureNetwork{
						resourceGroupName: pulumi.String("witself-test-rg").ToStringOutput(),
						vnetID:            pulumi.ID("/test/vnet").ToIDOutput(), dbSubnetID: pulumi.ID("/test/subnet").ToIDOutput(),
					}
					db, err := provisionAzurePostgres(ctx, c, net)
					if err != nil {
						return err
					}
					_, err = provisionAzureSecrets(ctx, c, net, db)
					return err
				}, pulumi.WithMocks("witself-infra", "azure-test", mocks))
				if err != nil {
					t.Fatalf("render Azure resources: %v", err)
				}
				server := mocks.registered(t, "azure-native:dbforpostgresql:Server", "witself", protected)
				mocks.registered(t, "azure-native:dbforpostgresql:Database", "witself", protected)
				const lockType = "azure-native:authorization:ManagementLockAtResourceLevel"
				if protected {
					lock := mocks.registered(t, lockType, "witself-db-deletion-protection", false)
					for key, want := range map[resource.PropertyKey]string{
						"level": "CanNotDelete", "resourceProviderNamespace": "Microsoft.DBforPostgreSQL",
						"resourceType": "flexibleServers", "resourceGroupName": "witself-test-rg",
						"resourceName": plainString(t, server.Inputs["serverName"]), "parentResourcePath": "",
					} {
						if got := plainString(t, lock.Inputs[key]); got != want {
							t.Fatalf("database lock %s = %q, want %q", key, got, want)
						}
					}
					mocks.assertCount(t, lockType, 1)
				} else {
					mocks.assertCount(t, lockType, 0)
				}
				vault := mocks.registered(t, "azure-native:keyvault:Vault", "cell", protected)
				properties := vault.Inputs["properties"].ObjectValue()
				assertProtectionBool(t, properties, "enableSoftDelete", true)
				if got := properties["softDeleteRetentionInDays"]; !got.IsNumber() || got.NumberValue() != 7 {
					t.Fatalf("vault retention = %v, want existing immutable 7 days", got)
				}
				if protected {
					assertProtectionBool(t, properties, "enablePurgeProtection", true)
				} else if got := properties["enablePurgeProtection"]; !got.IsNull() {
					t.Fatalf("unprotected purge protection = %v, want absent because Azure cannot disable it", got)
				}
				ignoresPurgeProtection := false
				for _, path := range vault.RegisterRPC.GetIgnoreChanges() {
					if path == "properties.enablePurgeProtection" {
						ignoresPurgeProtection = true
					}
				}
				if ignoresPurgeProtection == protected {
					t.Fatalf("vault ignores purge protection = %t, want %t", ignoresPurgeProtection, !protected)
				}
				for _, name := range cellSecretResourceNames {
					mocks.registered(t, "azure-native:keyvault:Secret", name, protected)
				}
				mocks.assertCount(t, "azure-native:keyvault:Vault", 1)
				mocks.assertCount(t, "azure-native:keyvault:Secret", 3)
			})
		}
	}
}

type deletionProtectionMocks struct {
	credentialResourceMocks
	registrationMu sync.Mutex
	registrations  []pulumi.MockResourceArgs
}

func (m *deletionProtectionMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.registrationMu.Lock()
	m.registrations = append(m.registrations, args)
	m.registrationMu.Unlock()
	id, outputs, err := m.credentialResourceMocks.NewResource(args)
	if err != nil {
		return "", nil, err
	}
	switch args.TypeToken {
	case "aws:rds/instance:Instance":
		outputs["address"] = resource.NewStringProperty("db.private.example")
		outputs["endpoint"] = resource.NewStringProperty("db.private.example:5432")
		outputs["port"] = resource.NewNumberProperty(5432)
	case "aws:eks/cluster:Cluster":
		outputs["certificateAuthority"] = resource.NewObjectProperty(resource.PropertyMap{
			"data": resource.NewStringProperty("test-ca"),
		})
	case "gcp:sql/databaseInstance:DatabaseInstance":
		outputs["privateIpAddress"] = resource.NewStringProperty("10.20.0.10")
		outputs["connectionName"] = resource.NewStringProperty("witself-test:us-west2:cell")
	case "gcp:container/cluster:Cluster":
		outputs["masterAuth"] = resource.NewObjectProperty(resource.PropertyMap{
			"clusterCaCertificate": resource.NewStringProperty("test-ca"),
		})
	case "azure-native:containerservice:ManagedCluster":
		issuer := outputs["oidcIssuerProfile"].ObjectValue().Copy()
		issuer["issuerURL"] = resource.NewStringProperty("https://oidc.example/")
		outputs["oidcIssuerProfile"] = resource.NewObjectProperty(issuer)
	case "azure-native:dbforpostgresql:Server":
		outputs["name"] = outputs["serverName"]
		outputs["fullyQualifiedDomainName"] = resource.NewStringProperty("db.private.example")
	case "azure-native:keyvault:Vault":
		outputs["name"] = outputs["vaultName"]
	case "azure-native:privatedns:PrivateZone":
		outputs["name"] = outputs["privateZoneName"]
	}
	return id, outputs, nil
}

func (m *deletionProtectionMocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	if args.Token == "aws:index/getAvailabilityZones:getAvailabilityZones" {
		return resource.PropertyMap{
			"names": resource.NewArrayProperty([]resource.PropertyValue{
				resource.NewStringProperty("us-west-2a"), resource.NewStringProperty("us-west-2b"), resource.NewStringProperty("us-west-2c"),
			}),
		}, nil
	}
	return m.credentialResourceMocks.Call(args)
}

func (m *deletionProtectionMocks) registered(t *testing.T, typ, name string, protected bool) pulumi.MockResourceArgs {
	t.Helper()
	m.registrationMu.Lock()
	defer m.registrationMu.Unlock()
	for _, args := range m.registrations {
		if args.TypeToken == typ && args.Name == name {
			if args.RegisterRPC == nil || args.RegisterRPC.GetProtect() != protected {
				t.Fatalf("%s %s Pulumi Protect = %v, want %t", typ, name, args.RegisterRPC.GetProtect(), protected)
			}
			return args
		}
	}
	t.Fatalf("resource %s %s was not registered", typ, name)
	return pulumi.MockResourceArgs{}
}

func (m *deletionProtectionMocks) assertCount(t *testing.T, typ string, want int) {
	t.Helper()
	m.registrationMu.Lock()
	defer m.registrationMu.Unlock()
	var count int
	for _, args := range m.registrations {
		if args.TypeToken == typ {
			count++
		}
	}
	if count != want {
		t.Fatalf("%s resource count = %d, want %d", typ, count, want)
	}
}

func assertProtectionBool(t *testing.T, inputs resource.PropertyMap, key resource.PropertyKey, want bool) {
	t.Helper()
	if got := inputs[key]; !got.IsBool() || got.BoolValue() != want {
		t.Fatalf("%s = %v, want %t", strings.TrimSpace(string(key)), got, want)
	}
}
