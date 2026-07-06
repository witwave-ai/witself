package cell

import (
	"encoding/json"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/secretmanager"
	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const bootstrapTokenTTL = "24h"

type gcpSecret struct {
	name pulumi.StringOutput
	id   pulumi.IDOutput
}

func gcpBootstrapSecretName(c gcpCell) string {
	return rname(c.name, "bootstrap-operator-token")
}

func gcpProvisionSecretName(c gcpCell) string {
	return rname(c.name, "provision-token")
}

func provisionGCPJSONSecret(ctx *pulumi.Context, resourceName string, c gcpCell, secretID, description string, payload pulumi.StringOutput, prov *gcp.Provider, secretManagerAPI pulumi.Resource) (*gcpSecret, error) {
	secret, err := secretmanager.NewSecret(ctx, resourceName, &secretmanager.SecretArgs{
		SecretId:           pulumi.String(secretID),
		DeletionPolicy:     pulumi.String("DELETE"),
		DeletionProtection: pulumi.Bool(false),
		Labels:             gcpDefaultLabels(c),
		Annotations: pulumi.StringMap{
			"description": pulumi.String(description),
		},
		Replication: &secretmanager.SecretReplicationArgs{
			Auto: &secretmanager.SecretReplicationAutoArgs{},
		},
	}, pulumi.Provider(prov), pulumi.DependsOn([]pulumi.Resource{secretManagerAPI}))
	if err != nil {
		return nil, err
	}

	if _, err := secretmanager.NewSecretVersion(ctx, resourceName, &secretmanager.SecretVersionArgs{
		Secret:     secret.ID(),
		SecretData: payload,
	}, pulumi.Provider(prov), pulumi.DependsOn([]pulumi.Resource{secret})); err != nil {
		return nil, err
	}

	return &gcpSecret{
		name: pulumi.String(secretID).ToStringOutput(),
		id:   secret.ID(),
	}, nil
}

// provisionGCPBootstrapSecret mirrors AWS's bootstrap secret in GCP Secret
// Manager, using a GCP-safe flat Secret ID instead of the AWS slash namespace.
func provisionGCPBootstrapSecret(ctx *pulumi.Context, c gcpCell, prov *gcp.Provider, secretManagerAPI pulumi.Resource) (*gcpSecret, error) {
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
			return nil, err
		}
		token = tokenBody.Result.ApplyT(func(body string) string {
			return "witself_boot_" + body
		}).(pulumi.StringOutput)
	}

	payload := token.ApplyT(func(tok string) (string, error) {
		b, err := json.Marshal(map[string]string{
			"token": tok,
			"ttl":   bootstrapTokenTTL,
		})
		return string(b), err
	}).(pulumi.StringOutput)

	secret, err := provisionGCPJSONSecret(ctx, "witself-bootstrap-token", c, gcpBootstrapSecretName(c), "Witself first-operator bootstrap token (managed by witself-infra)", payload, prov, secretManagerAPI)
	if err != nil {
		return nil, err
	}

	ctx.Export("bootstrapSecretName", secret.name)
	ctx.Export("bootstrapSecretID", secret.id)
	ctx.Export("bootstrapTokenTTL", pulumi.String(bootstrapTokenTTL))
	return secret, nil
}

// provisionGCPProvisionSecret mints the per-cell account-provisioning token and
// publishes it where ESO can sync it into witself-server as WITSELF_PROVISION_TOKEN.
func provisionGCPProvisionSecret(ctx *pulumi.Context, c gcpCell, prov *gcp.Provider, secretManagerAPI pulumi.Resource) (*gcpSecret, error) {
	tokenBody, err := random.NewRandomString(ctx, "witself-provision-token", &random.RandomStringArgs{
		Length:  pulumi.Int(43),
		Special: pulumi.Bool(false),
		Upper:   pulumi.Bool(true),
		Lower:   pulumi.Bool(true),
		Numeric: pulumi.Bool(true),
	})
	if err != nil {
		return nil, err
	}
	token := tokenBody.Result.ApplyT(func(body string) string {
		return "witself_prv_" + body
	}).(pulumi.StringOutput)

	payload := token.ApplyT(func(tok string) (string, error) {
		b, err := json.Marshal(map[string]string{"token": tok})
		return string(b), err
	}).(pulumi.StringOutput)

	secret, err := provisionGCPJSONSecret(ctx, "witself-provision-token", c, gcpProvisionSecretName(c), "Witself cell account-provisioning token (managed by witself-infra)", payload, prov, secretManagerAPI)
	if err != nil {
		return nil, err
	}

	ctx.Export("provisionSecretName", secret.name)
	ctx.Export("provisionSecretID", secret.id)
	ctx.Export("provisionToken", pulumi.ToSecret(token))
	return secret, nil
}
