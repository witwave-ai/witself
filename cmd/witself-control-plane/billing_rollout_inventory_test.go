package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/billing/lifecycle"
	"github.com/witwave-ai/witself/internal/blob"
)

type billingRolloutNoCallReader struct{}

func (billingRolloutNoCallReader) ListComplete(
	context.Context, string, int,
) ([]blob.ObjectInfo, error) {
	panic("unexpected cloud listing")
}

func (billingRolloutNoCallReader) GetBounded(
	context.Context, string, int64,
) ([]byte, string, error) {
	panic("unexpected cloud read")
}

func billingRolloutTestSourceFence(observedAt string) billingRolloutSourceFence {
	disabledAt := "2026-08-17T22:00:00Z"
	priorInspectionSHA256 := strings.Repeat("9", 64)
	sourceFence := billingRolloutSourceFence{
		Schema:     billingRolloutSourceFenceSchema,
		ObservedAt: observedAt,
		Fence: billingRolloutSourceIdentity{
			BackendNamespaceID:                     strings.Repeat("b", 32),
			BindingInventorySHA256:                 strings.Repeat("c", 64),
			CloudflareAccountID:                    billingRolloutProductionCloudflareAccountID,
			ContainerApplicationID:                 "33333333-3333-4333-8333-333333333333",
			ContainerApplicationSHA256:             strings.Repeat("e", 64),
			ContainerApplicationVersion:            18,
			ExpectedTargetApplicationID:            "33333333-3333-4333-8333-333333333333",
			ExpectedTargetApplicationVersion:       18,
			ExpectedTargetImageDigest:              "sha256:" + strings.Repeat("6", 64),
			LifecycleDisabledObservedAt:            &disabledAt,
			PriorLifecycleDisabledInspectionSHA256: &priorInspectionSHA256,
			ReviewedConfigSHA256:                   strings.Repeat("7", 64),
			SecretNameInventorySHA256:              strings.Repeat("d", 64),
			SourceInstanceInventorySHA256:          strings.Repeat("f", 64),
			TargetApplicationCurrent:               true,
			TargetReleaseCommit:                    strings.Repeat("8", 40),
			TargetReleaseDate:                      "2026-08-17T21:00:00Z",
			TargetReleaseVersion:                   "0.0.255",
			WorkerDeploymentID:                     "11111111-1111-4111-8111-111111111111",
			WorkerScriptETag:                       strings.Repeat("a", 64),
			WorkerVersionID:                        "22222222-2222-4222-8222-222222222222",
		},
	}
	unsigned, err := canonicalBillingRolloutSourceFence(sourceFence, false)
	if err != nil {
		panic(err)
	}
	sourceFence.InspectionSHA256 = billingRolloutSHA256(unsigned)
	return sourceFence
}

func billingRolloutRefreshSourceFenceHash(sourceFence *billingRolloutSourceFence) {
	sourceFence.InspectionSHA256 = ""
	unsigned, err := canonicalBillingRolloutSourceFence(*sourceFence, false)
	if err != nil {
		panic(err)
	}
	sourceFence.InspectionSHA256 = billingRolloutSHA256(unsigned)
}

func billingRolloutWriteSourceFence(
	t *testing.T,
	filePath string,
	sourceFence billingRolloutSourceFence,
) {
	t.Helper()
	encoded, err := canonicalBillingRolloutSourceFence(sourceFence, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func billingRolloutTestEnvironment() map[string]string {
	return map[string]string{
		"WITSELF_BILLING_INVENTORY_R2_ENDPOINT":   billingRolloutProductionR2Endpoint,
		"WITSELF_BILLING_INVENTORY_R2_BUCKET":     "witself-control-plane",
		"WITSELF_BILLING_INVENTORY_R2_ACCESS_KEY": "dedicated-read-only-access",
		"WITSELF_BILLING_INVENTORY_R2_SECRET_KEY": "dedicated-read-only-secret",
		"WITSELF_BILLING_INVENTORY_R2_PREFIX":     "registry/",
	}
}

func billingRolloutLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func billingRolloutTempDir(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return directory
}

func billingRolloutTestCapture(
	beforeSHA256, authoritySHA256 string,
) *lifecycle.BillingRolloutRegistryCapture {
	return &lifecycle.BillingRolloutRegistryCapture{
		ScanStartedAt:                 time.Date(2026, 8, 17, 22, 10, 1, 0, time.UTC),
		ScanCompletedAt:               time.Date(2026, 8, 17, 22, 10, 3, 0, time.UTC),
		BeforeSourceInspectionSHA256:  beforeSHA256,
		RegistryAuthoritySHA256:       authoritySHA256,
		AccountObjectsScanned:         7,
		MutationReceiptObjectsScanned: 11,
		Records: lifecycle.BillingRolloutRegistryRecords{
			PreparedDowngrades:        1,
			TargetlessPendingChanges:  2,
			MalformedPendingChanges:   3,
			MalformedMutationReceipts: 4,
			PostRetryHorizonReceipts:  5,
		},
	}
}

func billingRolloutTestDependencies(
	t *testing.T,
	environment map[string]string,
	collect func(
		context.Context,
		lifecycle.BillingRolloutInventoryReader,
		lifecycle.BillingRolloutRegistryOptions,
	) (*lifecycle.BillingRolloutRegistryCapture, error),
) billingRolloutInventoryDependencies {
	t.Helper()
	if collect == nil {
		collect = func(
			context.Context,
			lifecycle.BillingRolloutInventoryReader,
			lifecycle.BillingRolloutRegistryOptions,
		) (*lifecycle.BillingRolloutRegistryCapture, error) {
			t.Fatal("unexpected billing rollout registry collection")
			return nil, nil
		}
	}
	return billingRolloutInventoryDependencies{
		lookupEnv: billingRolloutLookup(environment),
		now: func() time.Time {
			t.Fatal("CLI clock called outside lifecycle collector")
			return time.Time{}
		},
		newReader: func(config blob.Config) (lifecycle.BillingRolloutInventoryReader, error) {
			if config.Endpoint != environment["WITSELF_BILLING_INVENTORY_R2_ENDPOINT"] ||
				config.Bucket != environment["WITSELF_BILLING_INVENTORY_R2_BUCKET"] ||
				config.AccessKey != environment["WITSELF_BILLING_INVENTORY_R2_ACCESS_KEY"] ||
				config.SecretKey != environment["WITSELF_BILLING_INVENTORY_R2_SECRET_KEY"] {
				t.Fatal("dedicated blob config did not match the private test environment")
			}
			return billingRolloutNoCallReader{}, nil
		},
		collect: collect,
	}
}

func billingRolloutRunSuccessfulScan(
	t *testing.T,
	directory string,
	environment map[string]string,
) (beforePath, provisionalPath string, capture *lifecycle.BillingRolloutRegistryCapture) {
	t.Helper()
	before := billingRolloutTestSourceFence("2026-08-17T22:10:00Z")
	beforePath = filepath.Join(directory, "before.json")
	provisionalPath = filepath.Join(directory, "provisional.json")
	billingRolloutWriteSourceFence(t, beforePath, before)

	dependencies := billingRolloutTestDependencies(t, environment, func(
		_ context.Context,
		_ lifecycle.BillingRolloutInventoryReader,
		options lifecycle.BillingRolloutRegistryOptions,
	) (*lifecycle.BillingRolloutRegistryCapture, error) {
		if options.R2Prefix != "registry/" || options.Now == nil ||
			options.BeforeSourceInspectionSHA256 != before.InspectionSHA256 {
			t.Fatalf("collector options = %+v", options)
		}
		_, _, _, wantAuthority, err := billingRolloutInventoryR2Authority(
			billingRolloutLookup(environment))
		if err != nil {
			t.Fatal(err)
		}
		if options.RegistryAuthoritySHA256 != wantAuthority {
			t.Fatalf("authority = %q; want %q", options.RegistryAuthoritySHA256, wantAuthority)
		}
		capture = billingRolloutTestCapture(
			options.BeforeSourceInspectionSHA256,
			options.RegistryAuthoritySHA256,
		)
		return capture, nil
	})
	if err := runBillingRolloutInventoryWithDependencies(context.Background(), []string{
		"scan",
		"--source-fence-before", beforePath,
		"--provisional", provisionalPath,
	}, dependencies); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return beforePath, provisionalPath, capture
}

func TestBillingRolloutInventoryTwoPhaseSuccessIsPrivateAndCountOnly(t *testing.T) {
	directory := billingRolloutTempDir(t)
	environment := billingRolloutTestEnvironment()
	beforePath, provisionalPath, capture := billingRolloutRunSuccessfulScan(
		t, directory, environment)

	provisionalInfo, err := os.Stat(provisionalPath)
	if err != nil {
		t.Fatal(err)
	}
	if provisionalInfo.Mode().Perm() != 0o600 {
		t.Fatalf("provisional mode = %o; want 600", provisionalInfo.Mode().Perm())
	}
	provisionalRaw, err := os.ReadFile(provisionalPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(provisionalRaw), "11111111-1111-4111-8111-111111111111") ||
		strings.Contains(string(provisionalRaw), "worker_deployment_id") {
		t.Fatal("private provisional copied source identity instead of its inspection hash")
	}
	provisional, _, err := readBillingRolloutProvisional(provisionalPath)
	if err != nil {
		t.Fatalf("read provisional: %v", err)
	}
	if provisional.AccountObjectsScanned != 7 ||
		provisional.MutationReceiptObjectsScanned != 11 ||
		provisional.ScanStartedAt != capture.ScanStartedAt.Format(time.RFC3339) ||
		provisional.ScanCompletedAt != capture.ScanCompletedAt.Format(time.RFC3339) {
		t.Fatalf("provisional = %+v", provisional)
	}

	after := billingRolloutTestSourceFence("2026-08-17T22:11:00Z")
	// Inactive rows have null versions and may appear/disappear between the
	// two observations. They are not writers; both fences still attest zero
	// potential-writer rows and stable deployment/application identity.
	after.Fence.ContainerInstanceCount = 2
	after.Fence.SourceInstanceInventorySHA256 = strings.Repeat("0", 64)
	billingRolloutRefreshSourceFenceHash(&after)
	afterPath := filepath.Join(directory, "after.json")
	outputPath := filepath.Join(directory, "inventory.json")
	billingRolloutWriteSourceFence(t, afterPath, after)
	finalizeEnvironment := billingRolloutTestEnvironment()
	delete(finalizeEnvironment, "WITSELF_BILLING_INVENTORY_R2_ACCESS_KEY")
	delete(finalizeEnvironment, "WITSELF_BILLING_INVENTORY_R2_SECRET_KEY")
	dependencies := billingRolloutTestDependencies(t, finalizeEnvironment, func(
		context.Context,
		lifecycle.BillingRolloutInventoryReader,
		lifecycle.BillingRolloutRegistryOptions,
	) (*lifecycle.BillingRolloutRegistryCapture, error) {
		t.Fatal("finalize called the R2 collector")
		return nil, nil
	})
	dependencies.newReader = func(blob.Config) (lifecycle.BillingRolloutInventoryReader, error) {
		t.Fatal("finalize constructed an R2 client")
		return nil, nil
	}
	if err := runBillingRolloutInventoryWithDependencies(context.Background(), []string{
		"finalize",
		"--source-fence-before", beforePath,
		"--provisional", provisionalPath,
		"--source-fence-after", afterPath,
		"--output", outputPath,
	}, dependencies); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	outputInfo, err := os.Stat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if outputInfo.Mode().Perm() != 0o600 {
		t.Fatalf("output mode = %o; want 600", outputInfo.Mode().Perm())
	}
	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema":"witself.billing-rollout-inventory.v1","captured_at":"2026-08-17T22:10:03Z","billing_mutation_cohort_accounts":0,"source_fleet":{"api_replicas":0,"reconciler_replicas":0},"records":{"prepared_downgrades":1,"targetless_pending_changes":2,"malformed_pending_changes":3,"malformed_mutation_receipts":4,"post_retry_horizon_receipts":5}}` + "\n"
	if string(output) != want {
		t.Fatalf("public inventory = %s; want %s", output, want)
	}
	for _, forbidden := range []string{
		"scan_started_at", "scan_completed_at", "registry_authority_sha256",
		"before_source_inspection_sha256", "account_objects_scanned",
		"mutation_receipt_objects_scanned", "11111111-1111-4111-8111-111111111111",
	} {
		if strings.Contains(string(output), forbidden) {
			t.Fatalf("public inventory exposed %q", forbidden)
		}
	}
	if err := runBillingRolloutInventoryWithDependencies(context.Background(), []string{
		"finalize",
		"--source-fence-before", beforePath,
		"--provisional", provisionalPath,
		"--source-fence-after", afterPath,
		"--output", outputPath,
	}, dependencies); err == nil {
		t.Fatal("finalize overwrote an existing public inventory")
	}
	afterRefusal, err := os.ReadFile(outputPath)
	if err != nil || string(afterRefusal) != want {
		t.Fatalf("existing public inventory changed: %q, %v", afterRefusal, err)
	}
}

func TestBillingRolloutInventoryScanRejectsNonzeroSourceCountsBeforeR2(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*billingRolloutSourceFence)
	}{
		{name: "cohort", mutate: func(sourceFence *billingRolloutSourceFence) {
			sourceFence.BillingMutationCohortAccounts = 1
		}},
		{name: "api", mutate: func(sourceFence *billingRolloutSourceFence) {
			sourceFence.SourceFleet.APIReplicas = 1
		}},
		{name: "reconciler", mutate: func(sourceFence *billingRolloutSourceFence) {
			sourceFence.SourceFleet.ReconcilerReplicas = 1
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := billingRolloutTempDir(t)
			before := billingRolloutTestSourceFence("2026-08-17T22:10:00Z")
			test.mutate(&before)
			billingRolloutRefreshSourceFenceHash(&before)
			beforePath := filepath.Join(directory, "before.json")
			provisionalPath := filepath.Join(directory, "provisional.json")
			billingRolloutWriteSourceFence(t, beforePath, before)
			called := false
			dependencies := billingRolloutTestDependencies(
				t, billingRolloutTestEnvironment(), nil)
			dependencies.newReader = func(blob.Config) (lifecycle.BillingRolloutInventoryReader, error) {
				called = true
				return billingRolloutNoCallReader{}, nil
			}
			dependencies.collect = func(
				context.Context,
				lifecycle.BillingRolloutInventoryReader,
				lifecycle.BillingRolloutRegistryOptions,
			) (*lifecycle.BillingRolloutRegistryCapture, error) {
				called = true
				return nil, nil
			}
			err := runBillingRolloutInventoryWithDependencies(context.Background(), []string{
				"scan", "--source-fence-before", beforePath,
				"--provisional", provisionalPath,
			}, dependencies)
			if err == nil || called {
				t.Fatalf("scan error/called = %v/%t; want pre-R2 refusal", err, called)
			}
			if _, statErr := os.Lstat(provisionalPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("provisional exists after refusal: %v", statErr)
			}
		})
	}
}

func TestBillingRolloutInventoryRejectsStrictJSONViolationsAndSymlinks(t *testing.T) {
	valid := billingRolloutTestSourceFence("2026-08-17T22:10:00Z")
	canonical, err := canonicalBillingRolloutSourceFence(valid, true)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		raw  []byte
	}{
		{
			name: "extra field",
			raw:  bytesReplaceOnce(t, canonical, `{"billing_mutation_cohort_accounts":`, `{"extra":0,"billing_mutation_cohort_accounts":`),
		},
		{
			name: "duplicate field",
			raw:  bytesReplaceOnce(t, canonical, `{"billing_mutation_cohort_accounts":`, `{"billing_mutation_cohort_accounts":0,"billing_mutation_cohort_accounts":`),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := billingRolloutTempDir(t)
			beforePath := filepath.Join(directory, "before.json")
			if err := os.WriteFile(beforePath, append(test.raw, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			called := false
			dependencies := billingRolloutTestDependencies(t, billingRolloutTestEnvironment(), nil)
			dependencies.newReader = func(blob.Config) (lifecycle.BillingRolloutInventoryReader, error) {
				called = true
				return billingRolloutNoCallReader{}, nil
			}
			dependencies.collect = func(context.Context, lifecycle.BillingRolloutInventoryReader, lifecycle.BillingRolloutRegistryOptions) (*lifecycle.BillingRolloutRegistryCapture, error) {
				called = true
				return nil, nil
			}
			err := runBillingRolloutInventoryWithDependencies(context.Background(), []string{
				"scan", "--source-fence-before", beforePath,
				"--provisional", filepath.Join(directory, "provisional.json"),
			}, dependencies)
			if err == nil || called {
				t.Fatalf("strict JSON error/called = %v/%t", err, called)
			}
		})
	}

	directory := billingRolloutTempDir(t)
	realPath := filepath.Join(directory, "real.json")
	billingRolloutWriteSourceFence(t, realPath, valid)
	symlinkPath := filepath.Join(directory, "before-link.json")
	if err := os.Symlink(realPath, symlinkPath); err != nil {
		t.Fatal(err)
	}
	dependencies := billingRolloutTestDependencies(t, billingRolloutTestEnvironment(), func(context.Context, lifecycle.BillingRolloutInventoryReader, lifecycle.BillingRolloutRegistryOptions) (*lifecycle.BillingRolloutRegistryCapture, error) {
		t.Fatal("symlink input reached collector")
		return nil, nil
	})
	err = runBillingRolloutInventoryWithDependencies(context.Background(), []string{
		"scan", "--source-fence-before", symlinkPath,
		"--provisional", filepath.Join(directory, "provisional.json"),
	}, dependencies)
	if err == nil {
		t.Fatal("scan accepted symlink source fence")
	}

	realParent := filepath.Join(directory, "real-parent")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	parentLink := filepath.Join(directory, "parent-link")
	if err := os.Symlink(realParent, parentLink); err != nil {
		t.Fatal(err)
	}
	linkedSource := filepath.Join(parentLink, "before.json")
	billingRolloutWriteSourceFence(
		t, filepath.Join(realParent, "before.json"), valid)
	err = runBillingRolloutInventoryWithDependencies(context.Background(), []string{
		"scan", "--source-fence-before", linkedSource,
		"--provisional", filepath.Join(directory, "parent-source-provisional.json"),
	}, dependencies)
	if err == nil {
		t.Fatal("scan accepted a source fence through a symlink parent")
	}
	err = runBillingRolloutInventoryWithDependencies(context.Background(), []string{
		"scan", "--source-fence-before", realPath,
		"--provisional", filepath.Join(parentLink, "provisional.json"),
	}, dependencies)
	if err == nil {
		t.Fatal("scan accepted an output through a symlink parent")
	}
}

func bytesReplaceOnce(t *testing.T, data []byte, old, replacement string) []byte {
	t.Helper()
	result := strings.Replace(string(data), old, replacement, 1)
	if result == string(data) {
		t.Fatalf("fixture did not contain %q", old)
	}
	return []byte(result)
}

func TestBillingRolloutInventoryRefusesExistingOrRacingOutput(t *testing.T) {
	directory := billingRolloutTempDir(t)
	environment := billingRolloutTestEnvironment()
	before := billingRolloutTestSourceFence("2026-08-17T22:10:00Z")
	beforePath := filepath.Join(directory, "before.json")
	billingRolloutWriteSourceFence(t, beforePath, before)

	for _, test := range []struct {
		name       string
		seedOutput func(string) error
	}{
		{name: "file", seedOutput: func(filePath string) error {
			return os.WriteFile(filePath, []byte("keep"), 0o600)
		}},
		{name: "symlink", seedOutput: func(filePath string) error {
			target := filepath.Join(directory, "symlink-target")
			if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
				return err
			}
			return os.Symlink(target, filePath)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := filepath.Join(directory, "existing-"+test.name)
			if err := test.seedOutput(output); err != nil {
				t.Fatal(err)
			}
			called := false
			dependencies := billingRolloutTestDependencies(t, environment, nil)
			dependencies.newReader = func(blob.Config) (lifecycle.BillingRolloutInventoryReader, error) {
				called = true
				return billingRolloutNoCallReader{}, nil
			}
			dependencies.collect = func(context.Context, lifecycle.BillingRolloutInventoryReader, lifecycle.BillingRolloutRegistryOptions) (*lifecycle.BillingRolloutRegistryCapture, error) {
				called = true
				return nil, nil
			}
			err := runBillingRolloutInventoryWithDependencies(context.Background(), []string{
				"scan", "--source-fence-before", beforePath,
				"--provisional", output,
			}, dependencies)
			if err == nil || called {
				t.Fatalf("existing output error/called = %v/%t", err, called)
			}
		})
	}

	racingPath := filepath.Join(directory, "racing.json")
	dependencies := billingRolloutTestDependencies(t, environment, func(
		_ context.Context,
		_ lifecycle.BillingRolloutInventoryReader,
		options lifecycle.BillingRolloutRegistryOptions,
	) (*lifecycle.BillingRolloutRegistryCapture, error) {
		if err := os.WriteFile(racingPath, []byte("winner"), 0o600); err != nil {
			t.Fatal(err)
		}
		return billingRolloutTestCapture(
			options.BeforeSourceInspectionSHA256,
			options.RegistryAuthoritySHA256,
		), nil
	})
	err := runBillingRolloutInventoryWithDependencies(context.Background(), []string{
		"scan", "--source-fence-before", beforePath,
		"--provisional", racingPath,
	}, dependencies)
	if err == nil {
		t.Fatal("scan overwrote a path created during collection")
	}
	content, readErr := os.ReadFile(racingPath)
	if readErr != nil || string(content) != "winner" {
		t.Fatalf("racing output = %q, %v", content, readErr)
	}
}

func TestBillingRolloutInventoryCollectorFailureLeavesNoArtifact(t *testing.T) {
	directory := billingRolloutTempDir(t)
	environment := billingRolloutTestEnvironment()
	before := billingRolloutTestSourceFence("2026-08-17T22:10:00Z")
	beforePath := filepath.Join(directory, "before.json")
	provisionalPath := filepath.Join(directory, "provisional.json")
	billingRolloutWriteSourceFence(t, beforePath, before)
	dependencies := billingRolloutTestDependencies(t, environment, func(
		context.Context,
		lifecycle.BillingRolloutInventoryReader,
		lifecycle.BillingRolloutRegistryOptions,
	) (*lifecycle.BillingRolloutRegistryCapture, error) {
		return nil, lifecycle.ErrBillingRolloutInventoryIncomplete
	})
	err := runBillingRolloutInventoryWithDependencies(context.Background(), []string{
		"scan", "--source-fence-before", beforePath,
		"--provisional", provisionalPath,
	}, dependencies)
	if err == nil {
		t.Fatal("collector failure succeeded")
	}
	if _, statErr := os.Lstat(provisionalPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failure left provisional: %v", statErr)
	}
	matches, globErr := filepath.Glob(filepath.Join(directory, ".provisional.json.tmp-*"))
	if globErr != nil || len(matches) != 0 {
		t.Fatalf("failure left staging files: %v, %v", matches, globErr)
	}
}

func TestBillingRolloutInventoryScanRejectsInvalidCaptureTimeFence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*lifecycle.BillingRolloutRegistryCapture)
	}{
		{
			name: "scan starts at before observation",
			mutate: func(capture *lifecycle.BillingRolloutRegistryCapture) {
				capture.ScanStartedAt = time.Date(2026, 8, 17, 22, 10, 0, 0, time.UTC)
			},
		},
		{
			name: "non UTC",
			mutate: func(capture *lifecycle.BillingRolloutRegistryCapture) {
				capture.ScanStartedAt = time.Date(
					2026, 8, 17, 22, 10, 1, 0, time.FixedZone("test", 0))
			},
		},
		{
			name: "subsecond",
			mutate: func(capture *lifecycle.BillingRolloutRegistryCapture) {
				capture.ScanCompletedAt = capture.ScanCompletedAt.Add(time.Nanosecond)
			},
		},
		{
			name: "backwards",
			mutate: func(capture *lifecycle.BillingRolloutRegistryCapture) {
				capture.ScanCompletedAt = capture.ScanStartedAt.Add(-time.Second)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := billingRolloutTempDir(t)
			environment := billingRolloutTestEnvironment()
			before := billingRolloutTestSourceFence("2026-08-17T22:10:00Z")
			beforePath := filepath.Join(directory, "before.json")
			provisionalPath := filepath.Join(directory, "provisional.json")
			billingRolloutWriteSourceFence(t, beforePath, before)
			dependencies := billingRolloutTestDependencies(t, environment, func(
				_ context.Context,
				_ lifecycle.BillingRolloutInventoryReader,
				options lifecycle.BillingRolloutRegistryOptions,
			) (*lifecycle.BillingRolloutRegistryCapture, error) {
				capture := billingRolloutTestCapture(
					options.BeforeSourceInspectionSHA256,
					options.RegistryAuthoritySHA256,
				)
				test.mutate(capture)
				return capture, nil
			})
			err := runBillingRolloutInventoryWithDependencies(context.Background(), []string{
				"scan", "--source-fence-before", beforePath,
				"--provisional", provisionalPath,
			}, dependencies)
			if err == nil {
				t.Fatal("scan accepted an invalid private capture time fence")
			}
			if _, statErr := os.Lstat(provisionalPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("invalid capture left provisional: %v", statErr)
			}
		})
	}
}

func TestBillingRolloutInventoryFinalizeRefusesBrokenBracketIdentityCountsAndAuthority(t *testing.T) {
	tests := []struct {
		name              string
		mutateAfter       func(*billingRolloutSourceFence)
		mutateEnvironment func(map[string]string)
	}{
		{
			name: "same source timestamp",
			mutateAfter: func(sourceFence *billingRolloutSourceFence) {
				sourceFence.ObservedAt = "2026-08-17T22:10:00Z"
			},
		},
		{
			name: "changed source identity",
			mutateAfter: func(sourceFence *billingRolloutSourceFence) {
				sourceFence.Fence.WorkerDeploymentID = "44444444-4444-4444-8444-444444444444"
			},
		},
		{
			name: "changed source count",
			mutateAfter: func(sourceFence *billingRolloutSourceFence) {
				sourceFence.SourceFleet.ReconcilerReplicas = 1
			},
		},
		{
			name: "after before scan completion",
			mutateAfter: func(sourceFence *billingRolloutSourceFence) {
				sourceFence.ObservedAt = "2026-08-17T22:10:02Z"
			},
		},
		{
			name: "changed registry authority",
			mutateEnvironment: func(environment map[string]string) {
				environment["WITSELF_BILLING_INVENTORY_R2_PREFIX"] = "other-registry/"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := billingRolloutTempDir(t)
			environment := billingRolloutTestEnvironment()
			beforePath, provisionalPath, _ := billingRolloutRunSuccessfulScan(
				t, directory, environment)
			after := billingRolloutTestSourceFence("2026-08-17T22:11:00Z")
			if test.mutateAfter != nil {
				test.mutateAfter(&after)
				billingRolloutRefreshSourceFenceHash(&after)
			}
			if test.mutateEnvironment != nil {
				test.mutateEnvironment(environment)
			}
			afterPath := filepath.Join(directory, "after.json")
			outputPath := filepath.Join(directory, "inventory.json")
			billingRolloutWriteSourceFence(t, afterPath, after)
			dependencies := billingRolloutTestDependencies(t, environment, func(context.Context, lifecycle.BillingRolloutInventoryReader, lifecycle.BillingRolloutRegistryOptions) (*lifecycle.BillingRolloutRegistryCapture, error) {
				t.Fatal("finalize called collector")
				return nil, nil
			})
			err := runBillingRolloutInventoryWithDependencies(context.Background(), []string{
				"finalize",
				"--source-fence-before", beforePath,
				"--provisional", provisionalPath,
				"--source-fence-after", afterPath,
				"--output", outputPath,
			}, dependencies)
			if err == nil {
				t.Fatal("finalize accepted invalid bracket")
			}
			if _, statErr := os.Lstat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed finalize left public artifact: %v", statErr)
			}
		})
	}
}

func TestBillingRolloutInventoryProvisionalStrictShape(t *testing.T) {
	directory := billingRolloutTempDir(t)
	environment := billingRolloutTestEnvironment()
	beforePath, provisionalPath, _ := billingRolloutRunSuccessfulScan(
		t, directory, environment)
	raw, err := os.ReadFile(provisionalPath)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimSuffix(string(raw), "\n")
	body = strings.Replace(body, `{"account_objects_scanned":`, `{"unexpected":0,"account_objects_scanned":`, 1)
	if err := os.WriteFile(provisionalPath, []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	afterPath := filepath.Join(directory, "after.json")
	billingRolloutWriteSourceFence(
		t, afterPath, billingRolloutTestSourceFence("2026-08-17T22:11:00Z"))
	dependencies := billingRolloutTestDependencies(t, environment, func(context.Context, lifecycle.BillingRolloutInventoryReader, lifecycle.BillingRolloutRegistryOptions) (*lifecycle.BillingRolloutRegistryCapture, error) {
		t.Fatal("finalize called collector")
		return nil, nil
	})
	outputPath := filepath.Join(directory, "inventory.json")
	err = runBillingRolloutInventoryWithDependencies(context.Background(), []string{
		"finalize", "--source-fence-before", beforePath,
		"--provisional", provisionalPath,
		"--source-fence-after", afterPath,
		"--output", outputPath,
	}, dependencies)
	if err == nil {
		t.Fatal("finalize accepted provisional with extra field")
	}
}

func TestBillingRolloutInventoryFinalizeRejectsProvisionalHashMismatch(t *testing.T) {
	directory := billingRolloutTempDir(t)
	environment := billingRolloutTestEnvironment()
	beforePath, provisionalPath, _ := billingRolloutRunSuccessfulScan(
		t, directory, environment)
	raw, err := os.ReadFile(provisionalPath)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(
		strings.TrimSuffix(string(raw), "\n"),
		`"account_objects_scanned":7`,
		`"account_objects_scanned":8`,
		1,
	)
	if body == strings.TrimSuffix(string(raw), "\n") {
		t.Fatal("provisional fixture count was not found")
	}
	if err := os.WriteFile(provisionalPath, []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	afterPath := filepath.Join(directory, "after.json")
	billingRolloutWriteSourceFence(
		t, afterPath, billingRolloutTestSourceFence("2026-08-17T22:11:00Z"))
	outputPath := filepath.Join(directory, "inventory.json")
	dependencies := billingRolloutTestDependencies(t, environment, nil)
	err = runBillingRolloutInventoryWithDependencies(context.Background(), []string{
		"finalize", "--source-fence-before", beforePath,
		"--provisional", provisionalPath,
		"--source-fence-after", afterPath,
		"--output", outputPath,
	}, dependencies)
	if err == nil || !strings.Contains(err.Error(), "provisional_sha256") {
		t.Fatalf("tampered provisional error = %v", err)
	}
	if _, statErr := os.Lstat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("tampered provisional left output: %v", statErr)
	}
}

func TestBillingRolloutInventoryRejectsCanonicalProvisionalCountOverflow(t *testing.T) {
	directory := billingRolloutTempDir(t)
	environment := billingRolloutTestEnvironment()
	_, provisionalPath, _ := billingRolloutRunSuccessfulScan(
		t, directory, environment)
	provisional, _, err := readBillingRolloutProvisional(provisionalPath)
	if err != nil {
		t.Fatal(err)
	}
	provisional.MutationReceiptObjectsScanned = 0
	provisional.Records.MalformedMutationReceipts = math.MaxInt
	provisional.Records.PostRetryHorizonReceipts = math.MaxInt
	provisional.ProvisionalSHA256 = ""
	unsigned, err := canonicalBillingRolloutProvisional(*provisional, false)
	if err != nil {
		t.Fatal(err)
	}
	provisional.ProvisionalSHA256 = billingRolloutSHA256(unsigned)
	encoded, err := canonicalBillingRolloutProvisional(*provisional, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(provisionalPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readBillingRolloutProvisional(provisionalPath); err == nil {
		t.Fatal("strict provisional accepted overflowed record-count sums")
	}
}

func TestBillingRolloutInventoryArgumentsAndDedicatedEnvironmentAreStrict(t *testing.T) {
	dependencies := billingRolloutInventoryDependencies{
		lookupEnv: func(string) (string, bool) { return "", false },
		now:       time.Now,
		newReader: func(blob.Config) (lifecycle.BillingRolloutInventoryReader, error) {
			return billingRolloutNoCallReader{}, nil
		},
		collect: func(context.Context, lifecycle.BillingRolloutInventoryReader, lifecycle.BillingRolloutRegistryOptions) (*lifecycle.BillingRolloutRegistryCapture, error) {
			return nil, nil
		},
	}
	for _, args := range [][]string{
		nil,
		{"unknown"},
		{"scan", "--source-fence-before", "/tmp/a"},
		{"scan", "--source-fence-before", "/tmp/a", "--source-fence-before", "/tmp/b"},
		{"finalize", "--source-fence-before", "/tmp/a", "--provisional", "/tmp/b", "--source-fence-after", "/tmp/c"},
	} {
		if err := runBillingRolloutInventoryWithDependencies(context.Background(), args, dependencies); err == nil {
			t.Fatalf("arguments %#v succeeded", args)
		}
	}

	directory := billingRolloutTempDir(t)
	before := billingRolloutTestSourceFence("2026-08-17T22:10:00Z")
	beforePath := filepath.Join(directory, "before.json")
	billingRolloutWriteSourceFence(t, beforePath, before)
	environment := billingRolloutTestEnvironment()
	delete(environment, "WITSELF_BILLING_INVENTORY_R2_ACCESS_KEY")
	dependencies = billingRolloutTestDependencies(t, environment, func(context.Context, lifecycle.BillingRolloutInventoryReader, lifecycle.BillingRolloutRegistryOptions) (*lifecycle.BillingRolloutRegistryCapture, error) {
		t.Fatal("scan without dedicated credentials reached collector")
		return nil, nil
	})
	dependencies.newReader = func(blob.Config) (lifecycle.BillingRolloutInventoryReader, error) {
		t.Fatal("scan without dedicated credentials constructed a client")
		return nil, nil
	}
	err := runBillingRolloutInventoryWithDependencies(context.Background(), []string{
		"scan", "--source-fence-before", beforePath,
		"--provisional", filepath.Join(directory, "provisional.json"),
	}, dependencies)
	if err == nil || !strings.Contains(err.Error(), "WITSELF_BILLING_INVENTORY_R2_ACCESS_KEY") {
		t.Fatalf("missing dedicated credential error = %v", err)
	}

	for _, test := range []struct {
		name, variable, value string
	}{
		{
			name: "wrong production host", variable: "WITSELF_BILLING_INVENTORY_R2_ENDPOINT",
			value: "https://00000000000000000000000000000000.r2.cloudflarestorage.com",
		},
		{
			name: "empty lookalike bucket", variable: "WITSELF_BILLING_INVENTORY_R2_BUCKET",
			value: "witself-control-plane-empty",
		},
		{
			name: "empty lookalike prefix", variable: "WITSELF_BILLING_INVENTORY_R2_PREFIX",
			value: "empty-registry/",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			testEnvironment := billingRolloutTestEnvironment()
			testEnvironment[test.variable] = test.value
			called := false
			testDependencies := billingRolloutTestDependencies(t, testEnvironment, nil)
			testDependencies.newReader = func(blob.Config) (lifecycle.BillingRolloutInventoryReader, error) {
				called = true
				return billingRolloutNoCallReader{}, nil
			}
			testDependencies.collect = func(context.Context, lifecycle.BillingRolloutInventoryReader, lifecycle.BillingRolloutRegistryOptions) (*lifecycle.BillingRolloutRegistryCapture, error) {
				called = true
				return nil, nil
			}
			provisionalPath := filepath.Join(directory, "wrong-authority-"+strings.ReplaceAll(test.name, " ", "-")+".json")
			testErr := runBillingRolloutInventoryWithDependencies(context.Background(), []string{
				"scan", "--source-fence-before", beforePath,
				"--provisional", provisionalPath,
			}, testDependencies)
			if testErr == nil || called {
				t.Fatalf("wrong production authority error/called = %v/%t", testErr, called)
			}
		})
	}

	reusedEnvironment := billingRolloutTestEnvironment()
	reusedEnvironment["WITSELF_CP_R2_ACCESS_KEY"] =
		reusedEnvironment["WITSELF_BILLING_INVENTORY_R2_ACCESS_KEY"]
	reusedDependencies := billingRolloutTestDependencies(t, reusedEnvironment, nil)
	reusedDependencies.newReader = func(blob.Config) (lifecycle.BillingRolloutInventoryReader, error) {
		t.Fatal("reused ordinary credential constructed a client")
		return nil, nil
	}
	err = runBillingRolloutInventoryWithDependencies(context.Background(), []string{
		"scan", "--source-fence-before", beforePath,
		"--provisional", filepath.Join(directory, "reused-credential.json"),
	}, reusedDependencies)
	if err == nil {
		t.Fatal("scan accepted an ordinary control-plane R2 credential")
	}
}

func TestBillingRolloutSourceFenceFixtureMatchesStrictCanonicalHash(t *testing.T) {
	directory := billingRolloutTempDir(t)
	filePath := filepath.Join(directory, "source.json")
	sourceFence := billingRolloutTestSourceFence("2026-08-17T22:10:00Z")
	billingRolloutWriteSourceFence(t, filePath, sourceFence)
	decoded, _, err := readBillingRolloutSourceFence(filePath)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil || len(encoded) == 0 {
		t.Fatalf("decoded source fence = %s, %v", encoded, err)
	}
}
