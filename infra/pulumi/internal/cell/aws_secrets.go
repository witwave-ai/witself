package cell

import (
	"encoding/json"
	"fmt"

	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/rds"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/secretsmanager"
	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// dbSecretName is the cell's DB-credentials secret in AWS Secrets Manager. It is
// named <cell>/db — prefixed with the cell alias (the composed cell name) and a
// slash, so secrets are namespaced per cell within the account and ESO's Pod
// Identity role (scoped to <cell>/*) can read only this cell's secrets.
func dbSecretName(c awsCell) string { return c.name + "/db" }

// bootstrapSecretName is the cell's first-operator bootstrap token secret. ESO's
// Pod Identity role can read it because the IAM policy allows <cell>/*.
func bootstrapSecretName(c awsCell) string { return c.name + "/bootstrap/operator-token" }

// provisionAWSDBSecret writes the cell's Postgres connection (host, port, user,
// password, dbname, and a ready-to-use DSN) into AWS Secrets Manager as
// <cell>/db — the canonical source the External Secrets Operator syncs into the
// cluster. The value is a JSON object so an ExternalSecret can pull individual
// keys.
func provisionAWSDBSecret(ctx *pulumi.Context, c awsCell, db *rds.Instance, pw *random.RandomPassword, prov *aws.Provider) error {
	payload := pulumi.All(db.Address, db.Port, pw.Result).ApplyT(func(a []interface{}) (string, error) {
		host, port, password := a[0].(string), a[1].(int), a[2].(string)
		b, err := json.Marshal(map[string]interface{}{
			"host":     host,
			"port":     port,
			"username": "witself",
			"password": password,
			"dbname":   "witself",
			"dsn":      fmt.Sprintf("postgres://witself:%s@%s:%d/witself?sslmode=require", password, host, port),
		})
		return string(b), err
	}).(pulumi.StringOutput)

	// Dev cells force-delete the secret on destroy (no recovery window) so a
	// teardown leaves nothing billed; prod keeps the default 30-day recovery.
	recovery := 0
	if c.profile == "prod" {
		recovery = 30
	}

	secret, err := secretsmanager.NewSecret(ctx, "witself-db", &secretsmanager.SecretArgs{
		Name:                 pulumi.String(dbSecretName(c)),
		Description:          pulumi.String("Witself cell Postgres connection (managed by witself-infra)"),
		RecoveryWindowInDays: pulumi.Int(recovery),
		Tags:                 resourceTags(dbSecretName(c), "database"),
	}, pulumi.Provider(prov))
	if err != nil {
		return err
	}

	if _, err := secretsmanager.NewSecretVersion(ctx, "witself-db", &secretsmanager.SecretVersionArgs{
		SecretId:     secret.ID(),
		SecretString: payload, // secret — embeds the password
	}, pulumi.Provider(prov)); err != nil {
		return err
	}

	ctx.Export("dbSecretName", pulumi.String(dbSecretName(c)))
	ctx.Export("dbSecretArn", secret.Arn)
	return nil
}

// provisionAWSBootstrapSecret writes a short-lived first-operator bootstrap token
// into AWS Secrets Manager. The token is generated once per stack by Pulumi's
// random provider and delivered to the cluster by ESO as a mounted file.
func provisionAWSBootstrapSecret(ctx *pulumi.Context, c awsCell, prov *aws.Provider) error {
	var token pulumi.StringOutput
	if c.bootstrapTokenSet {
		token = c.bootstrapToken
	} else {
		tokenBody, err := random.NewRandomString(ctx, "witself-bootstrap-token", &random.RandomStringArgs{
			Length:  pulumi.Int(43), // 256-ish bits with base62 chars.
			Special: pulumi.Bool(false),
			Upper:   pulumi.Bool(true),
			Lower:   pulumi.Bool(true),
			Numeric: pulumi.Bool(true),
		})
		if err != nil {
			return err
		}
		token = tokenBody.Result.ApplyT(func(body string) string {
			return "witself_boot_" + body
		}).(pulumi.StringOutput)
	}
	payload := token.ApplyT(func(tok string) (string, error) {
		b, err := json.Marshal(map[string]string{
			"token": tok,
			"ttl":   "24h",
		})
		return string(b), err
	}).(pulumi.StringOutput)

	// Dev cells force-delete the secret on destroy (no recovery window) so a
	// teardown leaves no bootstrap material behind; prod keeps the default
	// recovery window.
	recovery := 0
	if c.profile == "prod" {
		recovery = 30
	}

	secret, err := secretsmanager.NewSecret(ctx, "witself-bootstrap-token", &secretsmanager.SecretArgs{
		Name:                 pulumi.String(bootstrapSecretName(c)),
		Description:          pulumi.String("Witself first-operator bootstrap token (managed by witself-infra)"),
		RecoveryWindowInDays: pulumi.Int(recovery),
		Tags:                 resourceTags(bootstrapSecretName(c), "bootstrap"),
	}, pulumi.Provider(prov))
	if err != nil {
		return err
	}

	if _, err := secretsmanager.NewSecretVersion(ctx, "witself-bootstrap-token", &secretsmanager.SecretVersionArgs{
		SecretId:     secret.ID(),
		SecretString: payload,
	}, pulumi.Provider(prov)); err != nil {
		return err
	}

	ctx.Export("bootstrapSecretName", pulumi.String(bootstrapSecretName(c)))
	ctx.Export("bootstrapSecretArn", secret.Arn)
	ctx.Export("bootstrapTokenTTL", pulumi.String("24h"))
	return nil
}
