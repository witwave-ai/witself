// Package backend initializes and resolves the Pulumi state backend for a cell's
// cloud account+region. It is deliberately NOT Pulumi — just direct, idempotent
// AWS SDK calls — which is what lets witself-infra create its own state backend
// without a chicken-and-egg (the backend itself has no state to store).
//
// One state bucket and one KMS key PER region in an account: the bucket carries
// the account id (S3 names are global) and region code; the alias carries only
// the region (KMS aliases are already account+region scoped). Both say "state" to
// stay distinct from the future sealed-plane app key.
package backend

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Info describes the state backend for one cloud account/project + region.
type Info struct {
	Bucket          string // object-store backend root: S3 bucket, GCS bucket, or Azure storage account
	BackendURL      string // PULUMI_BACKEND_URL, e.g. s3://witself-state-<account>-<region-code>
	KeyAlias        string // cloud KMS key name/alias used by the secrets provider
	SecretsProvider string // Pulumi secrets provider, e.g. awskms://… or gcpkms://…
	StorageKey      string // Azure storage account key for azblob backends; secret, never print
	SubscriptionID  string // Azure subscription ID selected by the CLI/backend resolver
}

func names(account, region, regionCode string) *Info {
	bucket := fmt.Sprintf("witself-state-%s-%s", account, regionCode)
	alias := "alias/witself-state-" + regionCode
	return &Info{
		Bucket: bucket,
		// The ?region is required: without it Pulumi's S3 backend (gocloud) hits
		// the us-east-1 endpoint and a bucket in another region returns a 301
		// PermanentRedirect.
		BackendURL:      fmt.Sprintf("s3://%s?region=%s", bucket, region),
		KeyAlias:        alias,
		SecretsProvider: fmt.Sprintf("awskms://%s?region=%s", alias, region),
	}
}

func loadConfig(ctx context.Context, region, profile string) (aws.Config, error) {
	opts := []func(*awscfg.LoadOptions) error{awscfg.WithRegion(region)}
	if profile != "" {
		opts = append(opts, awscfg.WithSharedConfigProfile(profile))
	}
	return awscfg.LoadDefaultConfig(ctx, opts...)
}

func accountID(ctx context.Context, cfg aws.Config) (string, error) {
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("get caller identity (check creds / -aws-profile): %w", err)
	}
	return aws.ToString(out.Account), nil
}

// ResolveAWS computes the backend names for an account+region and reports whether
// the state bucket already exists (so callers can require bootstrap first).
func ResolveAWS(ctx context.Context, region, regionCode, profile string) (*Info, bool, error) {
	cfg, err := loadConfig(ctx, region, profile)
	if err != nil {
		return nil, false, fmt.Errorf("load aws config: %w", err)
	}
	acct, err := accountID(ctx, cfg)
	if err != nil {
		return nil, false, err
	}
	info := names(acct, region, regionCode)
	_, headErr := s3.NewFromConfig(cfg).HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(info.Bucket)})
	return info, headErr == nil, nil
}

// BootstrapAWS idempotently ensures the state backend exists: a versioned,
// KMS-encrypted, public-access-blocked, TLS-only S3 bucket plus a rotated KMS key
// (aliased) for Pulumi's secrets provider. Safe to re-run.
func BootstrapAWS(ctx context.Context, region, regionCode, profile string, log func(string)) (*Info, error) {
	cfg, err := loadConfig(ctx, region, profile)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	acct, err := accountID(ctx, cfg)
	if err != nil {
		return nil, err
	}
	info := names(acct, region, regionCode)

	keyArn, err := ensureKMSKey(ctx, kms.NewFromConfig(cfg), info.KeyAlias, log)
	if err != nil {
		return nil, err
	}
	if err := ensureStateBucket(ctx, s3.NewFromConfig(cfg), info.Bucket, region, keyArn, log); err != nil {
		return nil, err
	}
	return info, nil
}

func ensureKMSKey(ctx context.Context, kc *kms.Client, alias string, log func(string)) (string, error) {
	out, err := kc.DescribeKey(ctx, &kms.DescribeKeyInput{KeyId: aws.String(alias)})
	if err == nil {
		log("kms: reusing " + alias)
		return aws.ToString(out.KeyMetadata.Arn), nil
	}
	var nf *kmstypes.NotFoundException
	if !errors.As(err, &nf) {
		return "", fmt.Errorf("describe kms key: %w", err)
	}

	created, err := kc.CreateKey(ctx, &kms.CreateKeyInput{
		Description: aws.String("witself cell state secrets (Pulumi secrets provider)"),
		Tags: []kmstypes.Tag{
			{TagKey: aws.String("app"), TagValue: aws.String("witself")},
			{TagKey: aws.String("witself:component"), TagValue: aws.String("state")},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create kms key: %w", err)
	}
	keyID := aws.ToString(created.KeyMetadata.KeyId)
	if _, err := kc.EnableKeyRotation(ctx, &kms.EnableKeyRotationInput{KeyId: aws.String(keyID)}); err != nil {
		return "", fmt.Errorf("enable key rotation: %w", err)
	}
	if _, err := kc.CreateAlias(ctx, &kms.CreateAliasInput{
		AliasName:   aws.String(alias),
		TargetKeyId: aws.String(keyID),
	}); err != nil {
		return "", fmt.Errorf("create kms alias: %w", err)
	}
	log("kms: created key + " + alias)
	return aws.ToString(created.KeyMetadata.Arn), nil
}

func ensureStateBucket(ctx context.Context, sc *s3.Client, bucket, region, keyArn string, log func(string)) error {
	if _, err := sc.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
		in := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
		if region != "us-east-1" { // us-east-1 must NOT set a LocationConstraint
			in.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
				LocationConstraint: s3types.BucketLocationConstraint(region),
			}
		}
		if _, err := sc.CreateBucket(ctx, in); err != nil {
			var owned *s3types.BucketAlreadyOwnedByYou
			if !errors.As(err, &owned) {
				return fmt.Errorf("create bucket %s: %w", bucket, err)
			}
		}
		log("s3: created " + bucket)
	} else {
		log("s3: reusing " + bucket)
	}

	// Apply hardening idempotently (these PUTs overwrite), new or existing.
	if _, err := sc.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusEnabled},
	}); err != nil {
		return fmt.Errorf("enable versioning: %w", err)
	}
	if _, err := sc.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{
		Bucket: aws.String(bucket),
		ServerSideEncryptionConfiguration: &s3types.ServerSideEncryptionConfiguration{
			Rules: []s3types.ServerSideEncryptionRule{{
				ApplyServerSideEncryptionByDefault: &s3types.ServerSideEncryptionByDefault{
					SSEAlgorithm:   s3types.ServerSideEncryptionAwsKms,
					KMSMasterKeyID: aws.String(keyArn),
				},
				BucketKeyEnabled: aws.Bool(true),
			}},
		},
	}); err != nil {
		return fmt.Errorf("set bucket encryption: %w", err)
	}
	if _, err := sc.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
		Bucket: aws.String(bucket),
		PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{
			BlockPublicAcls:       aws.Bool(true),
			IgnorePublicAcls:      aws.Bool(true),
			BlockPublicPolicy:     aws.Bool(true),
			RestrictPublicBuckets: aws.Bool(true),
		},
	}); err != nil {
		return fmt.Errorf("set public access block: %w", err)
	}
	policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Sid":"DenyInsecureTransport","Effect":"Deny","Principal":"*","Action":"s3:*","Resource":["arn:aws:s3:::%s","arn:aws:s3:::%s/*"],"Condition":{"Bool":{"aws:SecureTransport":"false"}}}]}`, bucket, bucket)
	if _, err := sc.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String(bucket),
		Policy: aws.String(policy),
	}); err != nil {
		return fmt.Errorf("set bucket policy: %w", err)
	}
	return nil
}
