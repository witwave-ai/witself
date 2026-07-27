package cell

import (
	"encoding/json"

	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// cellCredentials are two independent machine credentials. ESO-backed clouds
// store them in one provider JSON secret and extract both keys through the
// existing access boundary. Civo projects them into separate Kubernetes
// Secrets so adding backup authority never replaces its live provisioning
// Secret.
type cellCredentials struct {
	provisionToken pulumi.StringOutput
	backupToken    pulumi.StringOutput
	payload        pulumi.StringOutput
}

func newCellCredentials(ctx *pulumi.Context) (*cellCredentials, error) {
	provisionToken, err := newCellCredentialToken(
		ctx, "witself-provision-token", "witself_prv_",
	)
	if err != nil {
		return nil, err
	}
	backupToken, err := newCellCredentialToken(
		ctx, "witself-backup-token", "witself_bak_",
	)
	if err != nil {
		return nil, err
	}

	payload := pulumi.All(provisionToken, backupToken).ApplyT(
		func(values []interface{}) (string, error) {
			body, err := json.Marshal(map[string]string{
				"token":        values[0].(string),
				"backup_token": values[1].(string),
			})
			return string(body), err
		},
	).(pulumi.StringOutput)

	return &cellCredentials{
		provisionToken: provisionToken,
		backupToken:    backupToken,
		payload:        payload,
	}, nil
}

func newCellCredentialToken(
	ctx *pulumi.Context,
	resourceName, prefix string,
) (pulumi.StringOutput, error) {
	body, err := random.NewRandomString(ctx, resourceName, &random.RandomStringArgs{
		Length:  pulumi.Int(43), // 256-ish bits with base62 chars.
		Special: pulumi.Bool(false),
		Upper:   pulumi.Bool(true),
		Lower:   pulumi.Bool(true),
		Numeric: pulumi.Bool(true),
	}, pulumi.AdditionalSecretOutputs([]string{"result"}))
	if err != nil {
		return pulumi.StringOutput{}, err
	}
	// AdditionalSecretOutputs keeps the provider's raw result encrypted in
	// checkpoint state. ToSecret also marks the SDK value explicitly so
	// secrecy propagates through Apply/All and into every provider payload,
	// including providers whose input schema does not itself mark the field.
	secretBody := pulumi.ToSecret(body.Result).(pulumi.StringOutput)
	return secretBody.ApplyT(func(value string) string {
		return prefix + value
	}).(pulumi.StringOutput), nil
}
