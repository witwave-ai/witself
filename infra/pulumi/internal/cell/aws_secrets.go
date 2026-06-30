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
