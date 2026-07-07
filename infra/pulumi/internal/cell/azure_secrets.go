package cell

import (
	"encoding/json"

	authorization "github.com/pulumi/pulumi-azure-native-sdk/authorization/v3"
	keyvault "github.com/pulumi/pulumi-azure-native-sdk/keyvault/v3"
	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	azureDBSecretName        = "db"
	azureBootstrapSecretName = "bootstrap-operator-token"
	azureProvisionSecretName = "provision-token"
)

type azureSecrets struct {
	vaultName           pulumi.StringOutput
	vaultID             pulumi.IDOutput
	vaultURL            pulumi.StringOutput
	vault               pulumi.Resource
	dbSecretID          pulumi.IDOutput
	bootstrapSecretID   pulumi.IDOutput
	provisionSecretID   pulumi.IDOutput
	provisionToken      pulumi.StringOutput
	bootstrapTokenTTL   string
	dbSecretName        string
	bootstrapSecretName string
	provisionSecretName string
}

func provisionAzureSecrets(ctx *pulumi.Context, c azureCell, net *azureNetwork, db *azureDatabase) (*azureSecrets, error) {
	client, err := authorization.GetClientConfig(ctx)
	if err != nil {
		return nil, err
	}

	suffix, err := random.NewRandomId(ctx, "cell-keyvault-suffix", &random.RandomIdArgs{
		ByteLength: pulumi.Int(3),
	})
	if err != nil {
		return nil, err
	}

	vaultName := pulumi.Sprintf("witself-%s-kv", suffix.Hex)
	vault, err := keyvault.NewVault(ctx, "cell", &keyvault.VaultArgs{
		ResourceGroupName: net.resourceGroupName,
		VaultName:         vaultName,
		Location:          pulumi.String(c.region),
		Properties: keyvault.VaultPropertiesArgs{
			TenantId:                  pulumi.String(client.TenantId),
			EnableRbacAuthorization:   pulumi.Bool(false),
			EnableSoftDelete:          pulumi.Bool(true),
			SoftDeleteRetentionInDays: pulumi.Int(7),
			Sku: keyvault.SkuArgs{
				Family: pulumi.String("A"),
				Name:   keyvault.SkuNameStandard,
			},
			AccessPolicies: keyvault.AccessPolicyEntryArray{
				keyvault.AccessPolicyEntryArgs{
					TenantId: pulumi.String(client.TenantId),
					ObjectId: pulumi.String(client.ObjectId),
					Permissions: keyvault.PermissionsArgs{
						Secrets: pulumi.StringArray{
							pulumi.String("get"),
							pulumi.String("list"),
							pulumi.String("set"),
							pulumi.String("delete"),
							pulumi.String("recover"),
							pulumi.String("purge"),
						},
					},
				},
			},
		},
		Tags: azureResourceTags(c, rname(c.name, "secrets"), "secrets"),
	},
		pulumi.DependsOn([]pulumi.Resource{db.database}),
		pulumi.IgnoreChanges([]string{"properties.accessPolicies"}),
	)
	if err != nil {
		return nil, err
	}

	dbSecret, err := provisionAzureSecret(ctx, "witself-db", c, net, vault.Name, azureDBSecretName, "database", azureDBPayload(db), vault)
	if err != nil {
		return nil, err
	}

	bootstrapPayload, err := azureBootstrapPayload(ctx, c)
	if err != nil {
		return nil, err
	}
	bootstrapSecret, err := provisionAzureSecret(ctx, "witself-bootstrap-token", c, net, vault.Name, azureBootstrapSecretName, "bootstrap", bootstrapPayload, vault)
	if err != nil {
		return nil, err
	}

	provisionPayload, provisionToken, err := azureProvisionPayload(ctx)
	if err != nil {
		return nil, err
	}
	provisionSecret, err := provisionAzureSecret(ctx, "witself-provision-token", c, net, vault.Name, azureProvisionSecretName, "provision", provisionPayload, vault)
	if err != nil {
		return nil, err
	}

	return &azureSecrets{
		vaultName:           vault.Name,
		vaultID:             vault.ID(),
		vaultURL:            vault.Properties.VaultUri(),
		vault:               vault,
		dbSecretID:          dbSecret.ID(),
		bootstrapSecretID:   bootstrapSecret.ID(),
		provisionSecretID:   provisionSecret.ID(),
		provisionToken:      provisionToken,
		bootstrapTokenTTL:   bootstrapTokenTTL,
		dbSecretName:        azureDBSecretName,
		bootstrapSecretName: azureBootstrapSecretName,
		provisionSecretName: azureProvisionSecretName,
	}, nil
}

func provisionAzureSecret(ctx *pulumi.Context, resourceName string, c azureCell, net *azureNetwork, vaultName pulumi.StringOutput, secretName, component string, value pulumi.StringOutput, vault pulumi.Resource) (*keyvault.Secret, error) {
	return keyvault.NewSecret(ctx, resourceName, &keyvault.SecretArgs{
		ResourceGroupName: net.resourceGroupName,
		VaultName:         vaultName,
		SecretName:        pulumi.String(secretName),
		Properties: keyvault.SecretPropertiesArgs{
			ContentType: pulumi.String("application/json"),
			Value:       value,
		},
		Tags: azureResourceTags(c, secretName, component),
	}, pulumi.DependsOn([]pulumi.Resource{vault}))
}

func azureDBPayload(db *azureDatabase) pulumi.StringOutput {
	return pulumi.All(db.fqdn, db.password, db.dsn).ApplyT(func(a []interface{}) (string, error) {
		b, err := json.Marshal(map[string]interface{}{
			"host":     a[0].(string),
			"port":     5432,
			"username": "witself",
			"password": a[1].(string),
			"dbname":   "witself",
			"dsn":      a[2].(string),
		})
		return string(b), err
	}).(pulumi.StringOutput)
}

func azureBootstrapPayload(ctx *pulumi.Context, c azureCell) (pulumi.StringOutput, error) {
	var token pulumi.StringOutput
	if c.bootstrapTokenSet {
		token = c.bootstrapToken
	} else {
		tokenBody, err := random.NewRandomString(ctx, "witself-bootstrap-token", &random.RandomStringArgs{
			Length:  pulumi.Int(43),
			Special: pulumi.Bool(false),
			Upper:   pulumi.Bool(true),
			Lower:   pulumi.Bool(true),
			Numeric: pulumi.Bool(true),
		})
		if err != nil {
			return pulumi.StringOutput{}, err
		}
		token = tokenBody.Result.ApplyT(func(body string) string {
			return "witself_boot_" + body
		}).(pulumi.StringOutput)
	}

	return token.ApplyT(func(tok string) (string, error) {
		b, err := json.Marshal(map[string]string{
			"token": tok,
			"ttl":   bootstrapTokenTTL,
		})
		return string(b), err
	}).(pulumi.StringOutput), nil
}

func azureProvisionPayload(ctx *pulumi.Context) (pulumi.StringOutput, pulumi.StringOutput, error) {
	tokenBody, err := random.NewRandomString(ctx, "witself-provision-token", &random.RandomStringArgs{
		Length:  pulumi.Int(43),
		Special: pulumi.Bool(false),
		Upper:   pulumi.Bool(true),
		Lower:   pulumi.Bool(true),
		Numeric: pulumi.Bool(true),
	})
	if err != nil {
		return pulumi.StringOutput{}, pulumi.StringOutput{}, err
	}
	token := tokenBody.Result.ApplyT(func(body string) string {
		return "witself_prv_" + body
	}).(pulumi.StringOutput)

	payload := token.ApplyT(func(tok string) (string, error) {
		b, err := json.Marshal(map[string]string{"token": tok})
		return string(b), err
	}).(pulumi.StringOutput)

	return payload, token, nil
}
