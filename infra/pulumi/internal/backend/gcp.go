package backend

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/storage"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	serviceusage "google.golang.org/api/serviceusage/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const gcpCloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// EnsureGCPADC verifies the credentials path Pulumi's GCS backend and gcpkms
// secrets provider use. `gcloud auth login` is not enough; these use Application
// Default Credentials.
func EnsureGCPADC(ctx context.Context, project string) error {
	ts, err := google.DefaultTokenSource(ctx, gcpCloudPlatformScope)
	if err != nil {
		return gcpADCError(project, err)
	}
	if _, err := ts.Token(); err != nil {
		return gcpADCError(project, err)
	}
	return nil
}

func gcpADCError(project string, err error) error {
	cmd := "gcloud auth application-default login"
	if project != "" {
		cmd += " --project " + project
	}
	return fmt.Errorf("GCP Application Default Credentials are required for the GCS state backend and gcpkms secrets provider: %w\nrun: %s", err, cmd)
}

func gcpNames(project, region, regionCode string) *Info {
	bucket := fmt.Sprintf("witself-state-%s-%s", project, regionCode)
	keyRing := "witself-state-" + regionCode
	key := "pulumi-secrets"
	keyName := fmt.Sprintf("projects/%s/locations/%s/keyRings/%s/cryptoKeys/%s", project, region, keyRing, key)
	return &Info{
		Bucket:          bucket,
		BackendURL:      "gs://" + bucket,
		KeyAlias:        keyName,
		SecretsProvider: "gcpkms://" + keyName,
	}
}

// ResolveGCP computes the GCS/KMS backend names and reports whether the state
// bucket already exists. A single project+region backend can hold many cell
// stacks; the stack name, not the project, remains the cell boundary.
func ResolveGCP(ctx context.Context, project, region, regionCode string) (*Info, bool, error) {
	info := gcpNames(project, region, regionCode)
	sc, err := storage.NewClient(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("create storage client: %w", err)
	}
	defer sc.Close()

	_, err = sc.Bucket(info.Bucket).Attrs(ctx)
	if err == nil {
		return info, true, nil
	}
	if errors.Is(err, storage.ErrBucketNotExist) {
		return info, false, nil
	}
	return nil, false, fmt.Errorf("check GCS bucket %s: %w", info.Bucket, err)
}

// BootstrapGCP idempotently ensures the GCP state backend exists: a versioned,
// public-access-prevented GCS bucket plus a Cloud KMS key for Pulumi's gcpkms
// secrets provider. Safe to re-run.
func BootstrapGCP(ctx context.Context, project, region, regionCode string, log func(string)) (*Info, error) {
	if err := EnsureGCPADC(ctx, project); err != nil {
		return nil, err
	}
	info := gcpNames(project, region, regionCode)
	if err := EnsureGCPServices(ctx, project, log, "storage.googleapis.com", "cloudkms.googleapis.com"); err != nil {
		return nil, err
	}
	if err := ensureGCPKMSKey(ctx, project, region, regionCode, log); err != nil {
		return nil, err
	}
	if err := ensureGCSStateBucket(ctx, project, region, info.Bucket, log); err != nil {
		return nil, err
	}
	return info, nil
}

func EnsureGCPServices(ctx context.Context, project string, log func(string), services ...string) error {
	svc, err := serviceusage.NewService(ctx, option.WithScopes(gcpCloudPlatformScope))
	if err != nil {
		return fmt.Errorf("create serviceusage client: %w", err)
	}
	for _, name := range services {
		resource := fmt.Sprintf("projects/%s/services/%s", project, name)
		op, err := svc.Services.Enable(resource, &serviceusage.EnableServiceRequest{}).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("enable GCP API %s: %w", name, err)
		}
		if err := waitGCPServiceUsageOperation(ctx, svc, op); err != nil {
			return fmt.Errorf("enable GCP API %s: %w", name, err)
		}
		log("gcp: ensured API " + name)
	}
	return nil
}

func waitGCPServiceUsageOperation(ctx context.Context, svc *serviceusage.Service, op *serviceusage.Operation) error {
	if op == nil || op.Name == "" {
		return nil
	}
	for {
		if op.Done {
			if op.Error != nil {
				return fmt.Errorf("%d: %s", op.Error.Code, op.Error.Message)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
		next, err := svc.Operations.Get(op.Name).Context(ctx).Do()
		if err != nil {
			return err
		}
		op = next
	}
}

func ensureGCPKMSKey(ctx context.Context, project, region, regionCode string, log func(string)) error {
	kc, err := kms.NewKeyManagementClient(ctx)
	if err != nil {
		return fmt.Errorf("create kms client: %w", err)
	}
	defer kc.Close()

	parent := fmt.Sprintf("projects/%s/locations/%s", project, region)
	keyRingID := "witself-state-" + regionCode
	keyRingName := parent + "/keyRings/" + keyRingID
	if _, err := kc.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: keyRingName}); err == nil {
		log("kms: reusing " + keyRingName)
	} else if status.Code(err) == codes.NotFound {
		if _, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{
			Parent:    parent,
			KeyRingId: keyRingID,
			KeyRing:   &kmspb.KeyRing{},
		}); err != nil {
			return fmt.Errorf("create kms key ring: %w", err)
		}
		log("kms: created " + keyRingName)
	} else {
		return fmt.Errorf("get kms key ring: %w", err)
	}

	keyID := "pulumi-secrets"
	keyName := keyRingName + "/cryptoKeys/" + keyID
	if _, err := kc.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: keyName}); err == nil {
		log("kms: reusing " + keyName)
		return nil
	} else if status.Code(err) != codes.NotFound {
		return fmt.Errorf("get kms crypto key: %w", err)
	}

	if _, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{
		Parent:      keyRingName,
		CryptoKeyId: keyID,
		CryptoKey: &kmspb.CryptoKey{
			Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT,
			VersionTemplate: &kmspb.CryptoKeyVersionTemplate{
				ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE,
				Algorithm:       kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION,
			},
			NextRotationTime: timestamppb.New(time.Now().Add(90 * 24 * time.Hour)),
			RotationSchedule: &kmspb.CryptoKey_RotationPeriod{
				RotationPeriod: durationpb.New(90 * 24 * time.Hour),
			},
			Labels: gcpLabels("state"),
		},
	}); err != nil {
		return fmt.Errorf("create kms crypto key: %w", err)
	}
	log("kms: created " + keyName)
	return nil
}

func ensureGCSStateBucket(ctx context.Context, project, region, bucket string, log func(string)) error {
	sc, err := storage.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("create storage client: %w", err)
	}
	defer sc.Close()

	bh := sc.Bucket(bucket)
	attrs := &storage.BucketAttrs{
		Location:                 strings.ToUpper(region),
		StorageClass:             "STANDARD",
		VersioningEnabled:        true,
		UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true},
		PublicAccessPrevention:   storage.PublicAccessPreventionEnforced,
		Labels:                   gcpLabels("state"),
	}
	if _, err := bh.Attrs(ctx); err != nil {
		if !errors.Is(err, storage.ErrBucketNotExist) {
			return fmt.Errorf("check GCS bucket %s: %w", bucket, err)
		}
		if err := bh.Create(ctx, project, attrs); err != nil {
			return fmt.Errorf("create GCS bucket %s: %w", bucket, err)
		}
		log("gcs: created " + bucket)
		return nil
	}

	update := storage.BucketAttrsToUpdate{
		VersioningEnabled:        true,
		UniformBucketLevelAccess: &storage.UniformBucketLevelAccess{Enabled: true},
		PublicAccessPrevention:   storage.PublicAccessPreventionEnforced,
		StorageClass:             "STANDARD",
	}
	for k, v := range gcpLabels("state") {
		update.SetLabel(k, v)
	}
	if _, err := bh.Update(ctx, update); err != nil {
		return fmt.Errorf("update GCS bucket %s: %w", bucket, err)
	}
	log("gcs: reusing " + bucket)
	return nil
}

func gcpLabels(component string) map[string]string {
	return map[string]string{
		"app":                "witself",
		"witself_component":  component,
		"witself_managed_by": "pulumi",
	}
}
