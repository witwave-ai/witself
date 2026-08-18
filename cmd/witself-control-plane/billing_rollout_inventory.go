package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/billing/lifecycle"
	"github.com/witwave-ai/witself/internal/blob"
)

const (
	billingRolloutSourceFenceSchema    = "witself.billing-rollout-source-fence.v1"
	billingRolloutProvisionalSchema    = "witself.billing-rollout-inventory-provisional.v1"
	billingRolloutPublicSchema         = "witself.billing-rollout-inventory.v1"
	billingRolloutProductionR2Endpoint = "https://8f0bf04a4e7aab3a8cc60f02cc8c8fdb.r2.cloudflarestorage.com"
	billingRolloutProductionR2Bucket   = "witself-control-plane"
	billingRolloutProductionR2Prefix   = "registry/"

	billingRolloutPrivateArtifactMaxBytes       = 128 << 10
	billingRolloutMaxSafeJSONInteger            = int64(1<<53 - 1)
	billingRolloutProductionCloudflareAccountID = "8f0bf04a4e7aab3a8cc60f02cc8c8fdb"
	billingRolloutMaxAccountObjects             = 1_000_000
	billingRolloutMaxMutationReceiptObjects     = 1_000_000
)

var (
	billingRolloutUUIDPattern           = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	billingRolloutHex32Pattern          = regexp.MustCompile(`^[0-9a-f]{32}$`)
	billingRolloutSHA256Pattern         = regexp.MustCompile(`^[0-9a-f]{64}$`)
	billingRolloutImageDigestPattern    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	billingRolloutReleaseVersionPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)$`)
	billingRolloutReleaseCommitPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	billingRolloutUTCSecondPattern      = regexp.MustCompile(
		`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)
)

type billingRolloutSourceFleet struct {
	APIReplicas        int64 `json:"api_replicas"`
	ReconcilerReplicas int64 `json:"reconciler_replicas"`
}

type billingRolloutSourceIdentity struct {
	BackendNamespaceID                     string  `json:"backend_namespace_id"`
	BindingInventorySHA256                 string  `json:"binding_inventory_sha256"`
	CloudflareAccountID                    string  `json:"cloudflare_account_id"`
	ContainerApplicationID                 string  `json:"container_application_id"`
	ContainerApplicationSHA256             string  `json:"container_application_sha256"`
	ContainerApplicationVersion            int64   `json:"container_application_version"`
	ContainerInstanceCount                 int64   `json:"container_instance_count"`
	ExpectedTargetApplicationID            string  `json:"expected_target_application_id"`
	ExpectedTargetApplicationVersion       int64   `json:"expected_target_application_version"`
	ExpectedTargetImageDigest              string  `json:"expected_target_image_digest"`
	IncompatibleInstanceCount              int64   `json:"incompatible_instance_count"`
	LifecycleDisabledObservedAt            *string `json:"lifecycle_disabled_observed_at"`
	LifecycleGatePresent                   bool    `json:"lifecycle_gate_present"`
	PotentialWriterInstanceCount           int64   `json:"potential_writer_instance_count"`
	PriorLifecycleDisabledInspectionSHA256 *string `json:"prior_lifecycle_disabled_inspection_sha256"`
	ReviewedConfigSHA256                   string  `json:"reviewed_config_sha256"`
	SecretNameInventorySHA256              string  `json:"secret_name_inventory_sha256"`
	SourceInstanceInventorySHA256          string  `json:"source_instance_inventory_sha256"`
	TargetApplicationCurrent               bool    `json:"target_application_current"`
	TargetReleaseCommit                    string  `json:"target_release_commit"`
	TargetReleaseDate                      string  `json:"target_release_date"`
	TargetReleaseVersion                   string  `json:"target_release_version"`
	TargetVersionInstanceCount             int64   `json:"target_version_instance_count"`
	WorkerDeploymentID                     string  `json:"worker_deployment_id"`
	WorkerScriptETag                       string  `json:"worker_script_etag"`
	WorkerVersionID                        string  `json:"worker_version_id"`
}

type billingRolloutSourceFence struct {
	Schema                        string                       `json:"schema"`
	ObservedAt                    string                       `json:"observed_at"`
	BillingMutationCohortAccounts int64                        `json:"billing_mutation_cohort_accounts"`
	SourceFleet                   billingRolloutSourceFleet    `json:"source_fleet"`
	Fence                         billingRolloutSourceIdentity `json:"fence"`
	InspectionSHA256              string                       `json:"inspection_sha256"`
}

type billingRolloutProvisional struct {
	Schema                        string                                  `json:"schema"`
	ScanStartedAt                 string                                  `json:"scan_started_at"`
	ScanCompletedAt               string                                  `json:"scan_completed_at"`
	BeforeSourceInspectionSHA256  string                                  `json:"before_source_inspection_sha256"`
	RegistryAuthoritySHA256       string                                  `json:"registry_authority_sha256"`
	AccountObjectsScanned         int                                     `json:"account_objects_scanned"`
	MutationReceiptObjectsScanned int                                     `json:"mutation_receipt_objects_scanned"`
	Records                       lifecycle.BillingRolloutRegistryRecords `json:"records"`
	ProvisionalSHA256             string                                  `json:"provisional_sha256"`
}

type billingRolloutPublicInventory struct {
	Schema                        string                          `json:"schema"`
	CapturedAt                    string                          `json:"captured_at"`
	BillingMutationCohortAccounts int                             `json:"billing_mutation_cohort_accounts"`
	SourceFleet                   billingRolloutPublicSourceFleet `json:"source_fleet"`
	Records                       billingRolloutPublicRecords     `json:"records"`
}

type billingRolloutPublicSourceFleet struct {
	APIReplicas        int `json:"api_replicas"`
	ReconcilerReplicas int `json:"reconciler_replicas"`
}

type billingRolloutPublicRecords struct {
	PreparedDowngrades        int `json:"prepared_downgrades"`
	TargetlessPendingChanges  int `json:"targetless_pending_changes"`
	MalformedPendingChanges   int `json:"malformed_pending_changes"`
	MalformedMutationReceipts int `json:"malformed_mutation_receipts"`
	PostRetryHorizonReceipts  int `json:"post_retry_horizon_receipts"`
}

type billingRolloutInventoryDependencies struct {
	lookupEnv func(string) (string, bool)
	now       func() time.Time
	newReader func(blob.Config) (lifecycle.BillingRolloutInventoryReader, error)
	collect   func(
		context.Context,
		lifecycle.BillingRolloutInventoryReader,
		lifecycle.BillingRolloutRegistryOptions,
	) (*lifecycle.BillingRolloutRegistryCapture, error)
}

func productionBillingRolloutInventoryDependencies() billingRolloutInventoryDependencies {
	return billingRolloutInventoryDependencies{
		lookupEnv: os.LookupEnv,
		now:       time.Now,
		newReader: func(config blob.Config) (lifecycle.BillingRolloutInventoryReader, error) {
			return blob.New(config)
		},
		collect: lifecycle.CollectBillingRolloutRegistry,
	}
}

func runBillingRolloutInventory(ctx context.Context, args []string) error {
	return runBillingRolloutInventoryWithDependencies(
		ctx, args, productionBillingRolloutInventoryDependencies())
}

func runBillingRolloutInventoryWithDependencies(
	ctx context.Context,
	args []string,
	dependencies billingRolloutInventoryDependencies,
) error {
	if ctx == nil || dependencies.lookupEnv == nil || dependencies.now == nil ||
		dependencies.newReader == nil || dependencies.collect == nil {
		return errors.New("billing rollout inventory dependencies are invalid")
	}
	if len(args) == 0 {
		return errors.New("billing-rollout-inventory requires scan or finalize")
	}
	switch args[0] {
	case "scan":
		options, err := parseBillingRolloutScanArgs(args[1:])
		if err != nil {
			return err
		}
		return runBillingRolloutInventoryScan(ctx, options, dependencies)
	case "finalize":
		options, err := parseBillingRolloutFinalizeArgs(args[1:])
		if err != nil {
			return err
		}
		return runBillingRolloutInventoryFinalize(options, dependencies)
	default:
		return errors.New("unknown billing-rollout-inventory phase (have: scan, finalize)")
	}
}

type billingRolloutScanOptions struct {
	sourceFenceBefore string
	provisional       string
}

func parseBillingRolloutScanArgs(args []string) (billingRolloutScanOptions, error) {
	values, err := parseExactBillingRolloutFlags(args, []string{
		"--source-fence-before", "--provisional",
	})
	if err != nil {
		return billingRolloutScanOptions{}, err
	}
	return billingRolloutScanOptions{
		sourceFenceBefore: values["--source-fence-before"],
		provisional:       values["--provisional"],
	}, nil
}

type billingRolloutFinalizeOptions struct {
	sourceFenceBefore string
	provisional       string
	sourceFenceAfter  string
	output            string
}

func parseBillingRolloutFinalizeArgs(args []string) (billingRolloutFinalizeOptions, error) {
	values, err := parseExactBillingRolloutFlags(args, []string{
		"--source-fence-before", "--provisional", "--source-fence-after", "--output",
	})
	if err != nil {
		return billingRolloutFinalizeOptions{}, err
	}
	return billingRolloutFinalizeOptions{
		sourceFenceBefore: values["--source-fence-before"],
		provisional:       values["--provisional"],
		sourceFenceAfter:  values["--source-fence-after"],
		output:            values["--output"],
	}, nil
}

func parseExactBillingRolloutFlags(
	args []string,
	required []string,
) (map[string]string, error) {
	allowed := make(map[string]struct{}, len(required))
	for _, name := range required {
		allowed[name] = struct{}{}
	}
	if len(args) != len(required)*2 {
		return nil, errors.New("billing rollout inventory arguments are incomplete")
	}
	values := make(map[string]string, len(required))
	for index := 0; index < len(args); index += 2 {
		name, value := args[index], args[index+1]
		if _, ok := allowed[name]; !ok {
			return nil, errors.New("unknown billing rollout inventory argument")
		}
		if _, duplicate := values[name]; duplicate {
			return nil, fmt.Errorf("duplicate billing rollout inventory argument %q", name)
		}
		if value == "" {
			return nil, fmt.Errorf("%s requires a value", name)
		}
		values[name] = value
	}
	for _, name := range required {
		if values[name] == "" {
			return nil, fmt.Errorf("%s is required", name)
		}
	}
	return values, nil
}

func runBillingRolloutInventoryScan(
	ctx context.Context,
	options billingRolloutScanOptions,
	dependencies billingRolloutInventoryDependencies,
) error {
	beforePath, err := normalizedAbsoluteBillingRolloutPath(
		options.sourceFenceBefore, "--source-fence-before")
	if err != nil {
		return err
	}
	provisionalPath, err := normalizedAbsoluteBillingRolloutPath(
		options.provisional, "--provisional")
	if err != nil {
		return err
	}
	if beforePath == provisionalPath {
		return errors.New("--source-fence-before and --provisional must be different files")
	}
	if err := refuseExistingBillingRolloutOutput(provisionalPath); err != nil {
		return fmt.Errorf("--provisional: %w", err)
	}

	before, _, err := readBillingRolloutSourceFence(beforePath)
	if err != nil {
		return fmt.Errorf("--source-fence-before: %w", err)
	}
	if err := validateZeroBillingRolloutSourceCounts(before); err != nil {
		return fmt.Errorf("--source-fence-before: %w", err)
	}

	config, prefix, authoritySHA256, err := billingRolloutInventoryR2Config(
		dependencies.lookupEnv)
	if err != nil {
		return err
	}
	reader, err := dependencies.newReader(config)
	if err != nil {
		return errors.New("dedicated billing inventory R2 configuration is invalid")
	}
	capture, err := dependencies.collect(ctx, reader, lifecycle.BillingRolloutRegistryOptions{
		R2Prefix:                     prefix,
		BeforeSourceInspectionSHA256: before.InspectionSHA256,
		RegistryAuthoritySHA256:      authoritySHA256,
		Now:                          dependencies.now,
	})
	if err != nil {
		return fmt.Errorf("billing rollout inventory scan failed: %w", err)
	}
	if err := validateBillingRolloutRegistryCapture(
		capture, before.InspectionSHA256, authoritySHA256); err != nil {
		return err
	}
	beforeObservedAt, _ := parseCanonicalBillingRolloutUTCSecond(before.ObservedAt)
	if !capture.ScanStartedAt.After(beforeObservedAt) {
		return errors.New("registry scan must start after the BEFORE source-fence observation")
	}
	publicCapturedAt := capture.ScanCompletedAt
	if !publicCapturedAt.After(beforeObservedAt) {
		return errors.New("registry scan completion does not produce a whole-second artifact fence after BEFORE")
	}

	provisional := billingRolloutProvisional{
		Schema:                        billingRolloutProvisionalSchema,
		ScanStartedAt:                 capture.ScanStartedAt.Format(time.RFC3339),
		ScanCompletedAt:               capture.ScanCompletedAt.Format(time.RFC3339),
		BeforeSourceInspectionSHA256:  capture.BeforeSourceInspectionSHA256,
		RegistryAuthoritySHA256:       capture.RegistryAuthoritySHA256,
		AccountObjectsScanned:         capture.AccountObjectsScanned,
		MutationReceiptObjectsScanned: capture.MutationReceiptObjectsScanned,
		Records:                       capture.Records,
	}
	unsigned, err := canonicalBillingRolloutProvisional(provisional, false)
	if err != nil {
		return errors.New("private provisional inventory could not be encoded")
	}
	provisional.ProvisionalSHA256 = billingRolloutSHA256(unsigned)
	encoded, err := canonicalBillingRolloutProvisional(provisional, true)
	if err != nil {
		return errors.New("private provisional inventory could not be encoded")
	}
	return writeAtomicPrivateBillingRolloutFile(provisionalPath, append(encoded, '\n'))
}

func runBillingRolloutInventoryFinalize(
	options billingRolloutFinalizeOptions,
	dependencies billingRolloutInventoryDependencies,
) error {
	beforePath, err := normalizedAbsoluteBillingRolloutPath(
		options.sourceFenceBefore, "--source-fence-before")
	if err != nil {
		return err
	}
	provisionalPath, err := normalizedAbsoluteBillingRolloutPath(
		options.provisional, "--provisional")
	if err != nil {
		return err
	}
	afterPath, err := normalizedAbsoluteBillingRolloutPath(
		options.sourceFenceAfter, "--source-fence-after")
	if err != nil {
		return err
	}
	outputPath, err := normalizedAbsoluteBillingRolloutPath(options.output, "--output")
	if err != nil {
		return err
	}
	if beforePath == provisionalPath || beforePath == afterPath || beforePath == outputPath ||
		provisionalPath == afterPath || provisionalPath == outputPath || afterPath == outputPath {
		return errors.New("billing rollout inventory inputs and output must be different files")
	}
	if err := refuseExistingBillingRolloutOutput(outputPath); err != nil {
		return fmt.Errorf("--output: %w", err)
	}

	before, beforeInfo, err := readBillingRolloutSourceFence(beforePath)
	if err != nil {
		return fmt.Errorf("--source-fence-before: %w", err)
	}
	provisional, provisionalInfo, err := readBillingRolloutProvisional(provisionalPath)
	if err != nil {
		return fmt.Errorf("--provisional: %w", err)
	}
	after, afterInfo, err := readBillingRolloutSourceFence(afterPath)
	if err != nil {
		return fmt.Errorf("--source-fence-after: %w", err)
	}
	if os.SameFile(beforeInfo, provisionalInfo) || os.SameFile(beforeInfo, afterInfo) ||
		os.SameFile(provisionalInfo, afterInfo) {
		return errors.New("BEFORE, provisional, and AFTER must be independent files")
	}
	if err := validateZeroBillingRolloutSourceCounts(before); err != nil {
		return fmt.Errorf("--source-fence-before: %w", err)
	}
	if err := validateZeroBillingRolloutSourceCounts(after); err != nil {
		return fmt.Errorf("--source-fence-after: %w", err)
	}
	if before.SourceFleet != after.SourceFleet ||
		before.BillingMutationCohortAccounts != after.BillingMutationCohortAccounts {
		return errors.New("BEFORE and AFTER source-fence counts changed")
	}
	beforeIdentity, _ := canonicalBillingRolloutStableSourceIdentity(before.Fence)
	afterIdentity, _ := canonicalBillingRolloutStableSourceIdentity(after.Fence)
	if !bytes.Equal(beforeIdentity, afterIdentity) {
		return errors.New("BEFORE and AFTER source-fence identities changed")
	}
	if subtle.ConstantTimeCompare(
		[]byte(provisional.BeforeSourceInspectionSHA256),
		[]byte(before.InspectionSHA256),
	) != 1 {
		return errors.New("provisional inventory is not bound to the supplied BEFORE source fence")
	}
	_, _, _, authoritySHA256, err := billingRolloutInventoryR2Authority(
		dependencies.lookupEnv)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(
		[]byte(provisional.RegistryAuthoritySHA256),
		[]byte(authoritySHA256),
	) != 1 {
		return errors.New("provisional inventory is not bound to the configured registry authority")
	}

	beforeObservedAt, _ := parseCanonicalBillingRolloutUTCSecond(before.ObservedAt)
	afterObservedAt, _ := parseCanonicalBillingRolloutUTCSecond(after.ObservedAt)
	scanStartedAt, _ := parseCanonicalBillingRolloutUTCSecond(provisional.ScanStartedAt)
	scanCompletedAt, _ := parseCanonicalBillingRolloutUTCSecond(provisional.ScanCompletedAt)
	if !afterObservedAt.After(beforeObservedAt) {
		return errors.New("AFTER source-fence observation must be after BEFORE")
	}
	if !scanStartedAt.After(beforeObservedAt) || scanStartedAt.After(scanCompletedAt) ||
		scanCompletedAt.After(afterObservedAt) {
		return errors.New("source-fence and registry scan timestamps do not form a valid BEFORE-scan-AFTER bracket")
	}
	publicCapturedAt := scanCompletedAt
	if !publicCapturedAt.After(beforeObservedAt) || publicCapturedAt.After(afterObservedAt) {
		return errors.New("registry scan completion cannot be represented by a valid public whole-second fence")
	}

	publicInventory := billingRolloutPublicInventory{
		Schema:      billingRolloutPublicSchema,
		CapturedAt:  publicCapturedAt.Format(time.RFC3339),
		SourceFleet: billingRolloutPublicSourceFleet{},
		Records: billingRolloutPublicRecords{
			PreparedDowngrades:        provisional.Records.PreparedDowngrades,
			TargetlessPendingChanges:  provisional.Records.TargetlessPendingChanges,
			MalformedPendingChanges:   provisional.Records.MalformedPendingChanges,
			MalformedMutationReceipts: provisional.Records.MalformedMutationReceipts,
			PostRetryHorizonReceipts:  provisional.Records.PostRetryHorizonReceipts,
		},
	}
	encoded, err := json.Marshal(&publicInventory)
	if err != nil {
		return errors.New("billing rollout inventory output could not be encoded")
	}
	return writeAtomicPrivateBillingRolloutFile(outputPath, append(encoded, '\n'))
}

func billingRolloutInventoryR2Config(
	lookupEnv func(string) (string, bool),
) (blob.Config, string, string, error) {
	endpoint, bucket, prefix, authoritySHA256, err :=
		billingRolloutInventoryR2Authority(lookupEnv)
	if err != nil {
		return blob.Config{}, "", "", err
	}
	accessKey, err := requiredCanonicalBillingRolloutEnv(
		lookupEnv, "WITSELF_BILLING_INVENTORY_R2_ACCESS_KEY")
	if err != nil {
		return blob.Config{}, "", "", err
	}
	secretKey, err := requiredCanonicalBillingRolloutEnv(
		lookupEnv, "WITSELF_BILLING_INVENTORY_R2_SECRET_KEY")
	if err != nil {
		return blob.Config{}, "", "", err
	}
	if ordinaryAccessKey, ok := lookupEnv("WITSELF_CP_R2_ACCESS_KEY"); ok &&
		subtle.ConstantTimeCompare([]byte(accessKey), []byte(ordinaryAccessKey)) == 1 {
		return blob.Config{}, "", "", errors.New("dedicated billing inventory R2 access key must not reuse WITSELF_CP_R2_ACCESS_KEY")
	}
	if ordinarySecretKey, ok := lookupEnv("WITSELF_CP_R2_SECRET_KEY"); ok &&
		subtle.ConstantTimeCompare([]byte(secretKey), []byte(ordinarySecretKey)) == 1 {
		return blob.Config{}, "", "", errors.New("dedicated billing inventory R2 secret key must not reuse WITSELF_CP_R2_SECRET_KEY")
	}
	return blob.Config{
		Endpoint: endpoint, Bucket: bucket,
		AccessKey: accessKey, SecretKey: secretKey,
	}, prefix, authoritySHA256, nil
}

func billingRolloutInventoryR2Authority(
	lookupEnv func(string) (string, bool),
) (endpoint, bucket, prefix, authoritySHA256 string, err error) {
	values := map[string]string{}
	for _, name := range []string{
		"WITSELF_BILLING_INVENTORY_R2_ENDPOINT",
		"WITSELF_BILLING_INVENTORY_R2_BUCKET",
		"WITSELF_BILLING_INVENTORY_R2_PREFIX",
	} {
		value, envErr := requiredCanonicalBillingRolloutEnv(lookupEnv, name)
		if envErr != nil {
			return "", "", "", "", envErr
		}
		values[name] = value
	}

	endpoint = values["WITSELF_BILLING_INVENTORY_R2_ENDPOINT"]
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil || parsedEndpoint.Scheme != "https" || parsedEndpoint.Host == "" ||
		parsedEndpoint.User != nil || parsedEndpoint.Path != "" ||
		parsedEndpoint.RawQuery != "" || parsedEndpoint.Fragment != "" ||
		parsedEndpoint.Port() != "" || parsedEndpoint.Host != parsedEndpoint.Hostname() ||
		strings.ToLower(parsedEndpoint.Host) != parsedEndpoint.Host ||
		parsedEndpoint.String() != endpoint {
		return "", "", "", "", errors.New("WITSELF_BILLING_INVENTORY_R2_ENDPOINT must be a canonical HTTPS origin")
	}
	if endpoint != billingRolloutProductionR2Endpoint {
		return "", "", "", "", errors.New("WITSELF_BILLING_INVENTORY_R2_ENDPOINT must be the exact reviewed production R2 origin")
	}
	bucket = values["WITSELF_BILLING_INVENTORY_R2_BUCKET"]
	if len(bucket) > 255 || strings.ContainsAny(bucket, `/\\`) {
		return "", "", "", "", errors.New("WITSELF_BILLING_INVENTORY_R2_BUCKET is invalid")
	}
	if bucket != billingRolloutProductionR2Bucket {
		return "", "", "", "", errors.New("WITSELF_BILLING_INVENTORY_R2_BUCKET must be the exact reviewed production bucket")
	}
	prefix = values["WITSELF_BILLING_INVENTORY_R2_PREFIX"]
	if len(prefix) > 1024 || strings.HasPrefix(prefix, "/") ||
		!strings.HasSuffix(prefix, "/") || strings.ContainsRune(prefix, '\\') ||
		path.Clean(strings.TrimSuffix(prefix, "/"))+"/" != prefix {
		return "", "", "", "", errors.New("WITSELF_BILLING_INVENTORY_R2_PREFIX must be an explicit canonical non-root prefix ending in slash")
	}
	if prefix != billingRolloutProductionR2Prefix {
		return "", "", "", "", errors.New("WITSELF_BILLING_INVENTORY_R2_PREFIX must be the exact reviewed production registry prefix")
	}
	authority, marshalErr := json.Marshal(map[string]string{
		"bucket": bucket, "endpoint_host": parsedEndpoint.Host, "prefix": prefix,
	})
	if marshalErr != nil {
		return "", "", "", "", errors.New("registry authority could not be canonicalized")
	}
	return endpoint, bucket, prefix, billingRolloutSHA256(authority), nil
}

func requiredCanonicalBillingRolloutEnv(
	lookupEnv func(string) (string, bool),
	name string,
) (string, error) {
	value, ok := lookupEnv(name)
	if !ok || value == "" || strings.TrimSpace(value) != value ||
		!utf8.ValidString(value) || strings.IndexFunc(value, func(r rune) bool {
		return r == 0 || r == '\x7f' || r < ' '
	}) >= 0 {
		return "", fmt.Errorf("%s must be explicitly set to a canonical dedicated inventory value", name)
	}
	return value, nil
}

func readBillingRolloutSourceFence(
	filePath string,
) (*billingRolloutSourceFence, os.FileInfo, error) {
	raw, info, err := readPrivateBillingRolloutFile(filePath)
	if err != nil {
		return nil, nil, err
	}
	var sourceFence billingRolloutSourceFence
	if err := decodeStrictCanonicalBillingRolloutJSON(raw, &sourceFence, func() ([]byte, error) {
		return canonicalBillingRolloutSourceFence(sourceFence, true)
	}); err != nil {
		return nil, nil, errors.New("source fence was not strict canonical JSON")
	}
	if err := validateBillingRolloutSourceFence(&sourceFence); err != nil {
		return nil, nil, err
	}
	unsigned, err := canonicalBillingRolloutSourceFence(sourceFence, false)
	if err != nil {
		return nil, nil, errors.New("source fence could not be canonicalized")
	}
	wantHash := billingRolloutSHA256(unsigned)
	if subtle.ConstantTimeCompare([]byte(sourceFence.InspectionSHA256), []byte(wantHash)) != 1 {
		return nil, nil, errors.New("source fence inspection_sha256 did not match its canonical content")
	}
	return &sourceFence, info, nil
}

func validateBillingRolloutSourceFence(sourceFence *billingRolloutSourceFence) error {
	if sourceFence == nil || sourceFence.Schema != billingRolloutSourceFenceSchema ||
		!billingRolloutSHA256Pattern.MatchString(sourceFence.InspectionSHA256) {
		return errors.New("source fence schema or hash was invalid")
	}
	observedAt, err := parseCanonicalBillingRolloutUTCSecond(sourceFence.ObservedAt)
	if err != nil {
		return errors.New("source fence observed_at was invalid")
	}
	if sourceFence.BillingMutationCohortAccounts != 0 ||
		!billingRolloutNonnegativeSafeInteger(sourceFence.SourceFleet.APIReplicas) ||
		!billingRolloutNonnegativeSafeInteger(sourceFence.SourceFleet.ReconcilerReplicas) {
		return errors.New("source fence counts were invalid")
	}
	fence := sourceFence.Fence
	if fence.CloudflareAccountID != billingRolloutProductionCloudflareAccountID ||
		!billingRolloutUUIDPattern.MatchString(fence.WorkerDeploymentID) ||
		!billingRolloutUUIDPattern.MatchString(fence.WorkerVersionID) ||
		!billingRolloutSHA256Pattern.MatchString(fence.WorkerScriptETag) ||
		!billingRolloutHex32Pattern.MatchString(fence.BackendNamespaceID) ||
		!billingRolloutSHA256Pattern.MatchString(fence.BindingInventorySHA256) ||
		!billingRolloutSHA256Pattern.MatchString(fence.SecretNameInventorySHA256) ||
		!billingRolloutUUIDPattern.MatchString(fence.ContainerApplicationID) ||
		!billingRolloutPositiveSafeInteger(fence.ContainerApplicationVersion) ||
		!billingRolloutSHA256Pattern.MatchString(fence.ContainerApplicationSHA256) ||
		!billingRolloutSHA256Pattern.MatchString(fence.SourceInstanceInventorySHA256) ||
		!billingRolloutNonnegativeSafeInteger(fence.ContainerInstanceCount) ||
		!billingRolloutUUIDPattern.MatchString(fence.ExpectedTargetApplicationID) ||
		fence.ExpectedTargetApplicationID != fence.ContainerApplicationID ||
		!billingRolloutPositiveSafeInteger(fence.ExpectedTargetApplicationVersion) ||
		!billingRolloutImageDigestPattern.MatchString(fence.ExpectedTargetImageDigest) ||
		!billingRolloutNonnegativeSafeInteger(fence.IncompatibleInstanceCount) ||
		!billingRolloutNonnegativeSafeInteger(fence.PotentialWriterInstanceCount) ||
		!billingRolloutSHA256Pattern.MatchString(fence.ReviewedConfigSHA256) ||
		!billingRolloutReleaseCommitPattern.MatchString(fence.TargetReleaseCommit) ||
		!billingRolloutReleaseVersionPattern.MatchString(fence.TargetReleaseVersion) ||
		!billingRolloutNonnegativeSafeInteger(fence.TargetVersionInstanceCount) ||
		(fence.TargetApplicationCurrent &&
			fence.ContainerApplicationVersion != fence.ExpectedTargetApplicationVersion) {
		return errors.New("source fence identity was invalid")
	}
	if _, err := parseCanonicalBillingRolloutUTCSecond(fence.TargetReleaseDate); err != nil {
		return errors.New("source fence target release date was invalid")
	}
	if (fence.PriorLifecycleDisabledInspectionSHA256 == nil) !=
		(fence.LifecycleDisabledObservedAt == nil) ||
		(fence.PriorLifecycleDisabledInspectionSHA256 != nil &&
			!billingRolloutSHA256Pattern.MatchString(
				*fence.PriorLifecycleDisabledInspectionSHA256)) {
		return errors.New("source fence lifecycle-disabled identity was invalid")
	}
	outsideInFlightBound := false
	if fence.LifecycleDisabledObservedAt != nil {
		disabledAt, err := parseCanonicalBillingRolloutUTCSecond(*fence.LifecycleDisabledObservedAt)
		if err != nil || disabledAt.After(observedAt) {
			return errors.New("source fence lifecycle-disabled observation was invalid")
		}
		outsideInFlightBound = observedAt.Sub(disabledAt) >= 4*time.Minute
	}
	if fence.TargetVersionInstanceCount+fence.IncompatibleInstanceCount !=
		fence.PotentialWriterInstanceCount ||
		fence.PotentialWriterInstanceCount > fence.ContainerInstanceCount {
		return errors.New("source fence instance counts were inconsistent")
	}
	wantAPIReplicas := fence.PotentialWriterInstanceCount
	if !fence.TargetApplicationCurrent && wantAPIReplicas < 1 {
		wantAPIReplicas = 1
	}
	if sourceFence.SourceFleet.APIReplicas != wantAPIReplicas {
		return errors.New("source fence API count was inconsistent")
	}
	wantReconcilers := sourceFence.SourceFleet.APIReplicas
	if !fence.LifecycleGatePresent && sourceFence.SourceFleet.APIReplicas == 0 &&
		outsideInFlightBound {
		wantReconcilers = 0
	} else if wantReconcilers < 1 {
		wantReconcilers = 1
	}
	if sourceFence.SourceFleet.ReconcilerReplicas != wantReconcilers {
		return errors.New("source fence reconciler count was inconsistent")
	}
	return nil
}

func validateZeroBillingRolloutSourceCounts(sourceFence *billingRolloutSourceFence) error {
	if sourceFence.BillingMutationCohortAccounts != 0 ||
		sourceFence.SourceFleet.APIReplicas != 0 ||
		sourceFence.SourceFleet.ReconcilerReplicas != 0 ||
		sourceFence.Fence.LifecycleGatePresent ||
		!sourceFence.Fence.TargetApplicationCurrent ||
		sourceFence.Fence.PotentialWriterInstanceCount != 0 ||
		sourceFence.Fence.TargetVersionInstanceCount != 0 ||
		sourceFence.Fence.IncompatibleInstanceCount != 0 ||
		sourceFence.Fence.LifecycleDisabledObservedAt == nil ||
		sourceFence.Fence.PriorLifecycleDisabledInspectionSHA256 == nil {
		return errors.New("source fence must attest zero billing cohort, API replicas, and reconciler replicas")
	}
	observedAt, _ := parseCanonicalBillingRolloutUTCSecond(sourceFence.ObservedAt)
	disabledAt, _ := parseCanonicalBillingRolloutUTCSecond(
		*sourceFence.Fence.LifecycleDisabledObservedAt)
	if observedAt.Sub(disabledAt) < 4*time.Minute {
		return errors.New("source fence does not prove the lifecycle in-flight bound elapsed")
	}
	return nil
}

func readBillingRolloutProvisional(
	filePath string,
) (*billingRolloutProvisional, os.FileInfo, error) {
	raw, info, err := readPrivateBillingRolloutFile(filePath)
	if err != nil {
		return nil, nil, err
	}
	var provisional billingRolloutProvisional
	if err := decodeStrictCanonicalBillingRolloutJSON(raw, &provisional, func() ([]byte, error) {
		return canonicalBillingRolloutProvisional(provisional, true)
	}); err != nil {
		return nil, nil, errors.New("provisional artifact was not strict canonical JSON")
	}
	if provisional.Schema != billingRolloutProvisionalSchema ||
		!billingRolloutSHA256Pattern.MatchString(provisional.BeforeSourceInspectionSHA256) ||
		!billingRolloutSHA256Pattern.MatchString(provisional.RegistryAuthoritySHA256) ||
		!billingRolloutSHA256Pattern.MatchString(provisional.ProvisionalSHA256) ||
		!validBillingRolloutRegistryRecords(provisional.Records) ||
		!validBillingRolloutPrivateCounts(
			provisional.AccountObjectsScanned,
			provisional.MutationReceiptObjectsScanned,
			provisional.Records) {
		return nil, nil, errors.New("provisional artifact fields were invalid")
	}
	scanStartedAt, err := parseCanonicalBillingRolloutUTCSecond(provisional.ScanStartedAt)
	if err != nil {
		return nil, nil, errors.New("provisional scan_started_at was invalid")
	}
	scanCompletedAt, err := parseCanonicalBillingRolloutUTCSecond(provisional.ScanCompletedAt)
	if err != nil || scanCompletedAt.Before(scanStartedAt) {
		return nil, nil, errors.New("provisional scan_completed_at was invalid")
	}
	unsigned, err := canonicalBillingRolloutProvisional(provisional, false)
	if err != nil || subtle.ConstantTimeCompare(
		[]byte(provisional.ProvisionalSHA256),
		[]byte(billingRolloutSHA256(unsigned)),
	) != 1 {
		return nil, nil, errors.New("provisional_sha256 did not match its canonical content")
	}
	return &provisional, info, nil
}

func validateBillingRolloutRegistryCapture(
	capture *lifecycle.BillingRolloutRegistryCapture,
	beforeSourceInspectionSHA256 string,
	registryAuthoritySHA256 string,
) error {
	if capture == nil || capture.ScanStartedAt.IsZero() || capture.ScanCompletedAt.IsZero() ||
		capture.ScanStartedAt.Location() != time.UTC || capture.ScanCompletedAt.Location() != time.UTC ||
		capture.ScanStartedAt.Year() < 1 || capture.ScanStartedAt.Year() > 9999 ||
		capture.ScanCompletedAt.Year() < 1 || capture.ScanCompletedAt.Year() > 9999 ||
		capture.ScanStartedAt.Nanosecond() != 0 || capture.ScanCompletedAt.Nanosecond() != 0 ||
		capture.ScanCompletedAt.Before(capture.ScanStartedAt) ||
		capture.BeforeSourceInspectionSHA256 != beforeSourceInspectionSHA256 ||
		capture.RegistryAuthoritySHA256 != registryAuthoritySHA256 ||
		!validBillingRolloutRegistryRecords(capture.Records) ||
		!validBillingRolloutPrivateCounts(
			capture.AccountObjectsScanned,
			capture.MutationReceiptObjectsScanned,
			capture.Records) {
		return errors.New("billing rollout inventory collector returned an invalid private capture")
	}
	return nil
}

func validBillingRolloutRegistryRecords(records lifecycle.BillingRolloutRegistryRecords) bool {
	return records.PreparedDowngrades >= 0 &&
		records.TargetlessPendingChanges >= 0 &&
		records.MalformedPendingChanges >= 0 &&
		records.MalformedMutationReceipts >= 0 &&
		records.PostRetryHorizonReceipts >= 0
}

func validBillingRolloutPrivateCounts(
	accountObjects int,
	mutationReceiptObjects int,
	records lifecycle.BillingRolloutRegistryRecords,
) bool {
	if accountObjects < 0 || accountObjects > billingRolloutMaxAccountObjects ||
		mutationReceiptObjects < 0 ||
		mutationReceiptObjects > billingRolloutMaxMutationReceiptObjects {
		return false
	}
	remainingAccounts := accountObjects
	for _, count := range []int{
		records.PreparedDowngrades,
		records.TargetlessPendingChanges,
		records.MalformedPendingChanges,
	} {
		if count < 0 || count > remainingAccounts {
			return false
		}
		remainingAccounts -= count
	}
	remainingReceipts := mutationReceiptObjects
	for _, count := range []int{
		records.MalformedMutationReceipts,
		records.PostRetryHorizonReceipts,
	} {
		if count < 0 || count > remainingReceipts {
			return false
		}
		remainingReceipts -= count
	}
	return true
}

func decodeStrictCanonicalBillingRolloutJSON(
	raw []byte,
	destination any,
	canonical func() ([]byte, error),
) error {
	body := raw
	if bytes.HasSuffix(body, []byte{'\n'}) {
		body = body[:len(body)-1]
	}
	if len(body) == 0 || bytes.ContainsAny(body, "\r\n") {
		return errors.New("artifact must contain one canonical JSON line")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("artifact contained trailing JSON")
	}
	want, err := canonical()
	if err != nil {
		return err
	}
	if !bytes.Equal(body, want) {
		return errors.New("artifact was not canonical")
	}
	return nil
}

func readPrivateBillingRolloutFile(filePath string) ([]byte, os.FileInfo, error) {
	root, base, err := openBillingRolloutParentRoot(filePath)
	if err != nil {
		return nil, nil, errors.New("private artifact could not be opened")
	}
	defer func() { _ = root.Close() }()
	before, err := root.Lstat(base)
	if err != nil {
		return nil, nil, errors.New("private artifact could not be opened")
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 ||
		before.Mode().Perm() != 0o600 || before.Size() < 1 ||
		before.Size() > billingRolloutPrivateArtifactMaxBytes {
		return nil, nil, errors.New("artifact must be a private regular file with exact mode 0600")
	}
	file, err := root.Open(base)
	if err != nil {
		return nil, nil, errors.New("private artifact could not be opened")
	}
	defer func() { _ = file.Close() }()
	afterOpen, err := file.Stat()
	if err != nil || !os.SameFile(before, afterOpen) ||
		afterOpen.Mode().Perm() != 0o600 || afterOpen.Size() != before.Size() ||
		!afterOpen.ModTime().Equal(before.ModTime()) {
		return nil, nil, errors.New("private artifact identity changed while opening")
	}
	afterPath, err := root.Lstat(base)
	if err != nil || !afterPath.Mode().IsRegular() || afterPath.Mode()&os.ModeSymlink != 0 ||
		afterPath.Mode().Perm() != 0o600 || !os.SameFile(afterOpen, afterPath) ||
		afterPath.Size() != afterOpen.Size() ||
		!afterPath.ModTime().Equal(afterOpen.ModTime()) {
		return nil, nil, errors.New("private artifact identity changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(file, billingRolloutPrivateArtifactMaxBytes+1))
	finalDescriptor, finalStatErr := file.Stat()
	finalPath, finalPathErr := root.Lstat(base)
	if err != nil || len(raw) > billingRolloutPrivateArtifactMaxBytes ||
		int64(len(raw)) != afterOpen.Size() || finalStatErr != nil ||
		!os.SameFile(afterOpen, finalDescriptor) ||
		finalDescriptor.Size() != afterOpen.Size() ||
		!finalDescriptor.ModTime().Equal(afterOpen.ModTime()) ||
		finalDescriptor.Mode().Perm() != 0o600 || finalPathErr != nil ||
		!finalPath.Mode().IsRegular() || finalPath.Mode()&os.ModeSymlink != 0 ||
		finalPath.Mode().Perm() != 0o600 ||
		!os.SameFile(finalDescriptor, finalPath) ||
		finalPath.Size() != finalDescriptor.Size() ||
		!finalPath.ModTime().Equal(finalDescriptor.ModTime()) {
		return nil, nil, errors.New("private artifact could not be read within its bound")
	}
	return raw, afterOpen, nil
}

func normalizedAbsoluteBillingRolloutPath(value, label string) (string, error) {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value ||
		strings.IndexByte(value, 0) >= 0 || filepath.Base(value) == string(os.PathSeparator) ||
		filepath.Base(value) == "." {
		return "", fmt.Errorf("%s must be a normalized absolute path", label)
	}
	return value, nil
}

func refuseExistingBillingRolloutOutput(filePath string) error {
	root, base, err := openBillingRolloutParentRoot(filePath)
	if err != nil {
		return errors.New("output parent must contain no symlinks and must be a directory")
	}
	defer func() { _ = root.Close() }()
	_, err = root.Lstat(base)
	switch {
	case err == nil:
		return errors.New("refusing to overwrite an existing path")
	case errors.Is(err, os.ErrNotExist):
		return nil
	default:
		return errors.New("output path could not be checked safely")
	}
}

func writeAtomicPrivateBillingRolloutFile(filePath string, data []byte) (err error) {
	root, base, err := openBillingRolloutParentRoot(filePath)
	if err != nil {
		return errors.New("output parent must contain no symlinks and must be a directory")
	}
	defer func() { _ = root.Close() }()
	if _, statErr := root.Lstat(base); statErr == nil {
		return errors.New("refusing to overwrite an existing path")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return errors.New("output path could not be checked safely")
	}
	temporary, temporaryName, err := createPrivateBillingRolloutStagingFile(root, base)
	if err != nil {
		return err
	}
	defer func() {
		_ = temporary.Close()
		if temporaryName != "" {
			_ = root.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return errors.New("private output permissions could not be set")
	}
	if _, err := temporary.Write(data); err != nil {
		return errors.New("private output could not be written")
	}
	if err := temporary.Sync(); err != nil {
		return errors.New("private output could not be synchronized")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("private output could not be closed")
	}
	// A same-directory hard link publishes the complete staged inode
	// atomically and, unlike Rename, has create-only/no-overwrite semantics.
	if err := root.Link(temporaryName, base); err != nil {
		if _, statErr := root.Lstat(base); statErr == nil {
			return errors.New("refusing to overwrite an existing path")
		}
		return errors.New("private output could not be published atomically")
	}
	if err := root.Remove(temporaryName); err != nil {
		_ = root.Remove(base)
		return errors.New("private output staging link could not be removed")
	}
	temporaryName = ""
	directory, err := root.Open(".")
	if err != nil {
		_ = root.Remove(base)
		return errors.New("private output directory could not be opened for synchronization")
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		_ = root.Remove(base)
		if retryDirectory, retryErr := root.Open("."); retryErr == nil {
			_ = retryDirectory.Sync()
			_ = retryDirectory.Close()
		}
		return errors.New("private output directory could not be synchronized")
	}
	return nil
}

func openBillingRolloutParentRoot(filePath string) (*os.Root, string, error) {
	parent := filepath.Dir(filePath)
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil || filepath.Clean(resolved) != parent {
		return nil, "", errors.New("parent path contains a symlink or does not exist")
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return nil, "", errors.New("parent path is not a real directory")
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, "", errors.New("parent directory could not be opened safely")
	}
	rootInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(parentInfo, rootInfo) {
		_ = root.Close()
		return nil, "", errors.New("parent directory identity changed while opening")
	}
	return root, filepath.Base(filePath), nil
}

func createPrivateBillingRolloutStagingFile(
	root *os.Root,
	base string,
) (*os.File, string, error) {
	for attempt := 0; attempt < 16; attempt++ {
		var randomBytes [16]byte
		if _, err := rand.Read(randomBytes[:]); err != nil {
			return nil, "", errors.New("private output staging name could not be generated")
		}
		name := "." + base + ".tmp-" + hex.EncodeToString(randomBytes[:])
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", errors.New("private output staging file could not be created")
		}
		return file, name, nil
	}
	return nil, "", errors.New("private output staging name was exhausted")
}

func parseCanonicalBillingRolloutUTCSecond(value string) (time.Time, error) {
	if !billingRolloutUTCSecondPattern.MatchString(value) {
		return time.Time{}, errors.New("timestamp is not a canonical UTC whole second")
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil || parsed.Format(time.RFC3339) != value || parsed.Nanosecond() != 0 {
		return time.Time{}, errors.New("timestamp is not a real canonical UTC whole second")
	}
	return parsed, nil
}

func billingRolloutNonnegativeSafeInteger(value int64) bool {
	return value >= 0 && value <= billingRolloutMaxSafeJSONInteger &&
		value <= int64(math.MaxInt)
}

func billingRolloutPositiveSafeInteger(value int64) bool {
	return value > 0 && billingRolloutNonnegativeSafeInteger(value)
}

func billingRolloutSHA256(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func canonicalBillingRolloutSourceIdentity(
	identity billingRolloutSourceIdentity,
) ([]byte, error) {
	return json.Marshal(billingRolloutSourceIdentityValue(identity, false))
}

func canonicalBillingRolloutStableSourceIdentity(
	identity billingRolloutSourceIdentity,
) ([]byte, error) {
	return json.Marshal(billingRolloutSourceIdentityValue(identity, true))
}

func billingRolloutSourceIdentityValue(
	identity billingRolloutSourceIdentity,
	stableOnly bool,
) map[string]any {
	value := map[string]any{
		"backend_namespace_id":                       identity.BackendNamespaceID,
		"binding_inventory_sha256":                   identity.BindingInventorySHA256,
		"cloudflare_account_id":                      identity.CloudflareAccountID,
		"container_application_id":                   identity.ContainerApplicationID,
		"container_application_sha256":               identity.ContainerApplicationSHA256,
		"container_application_version":              identity.ContainerApplicationVersion,
		"container_instance_count":                   identity.ContainerInstanceCount,
		"expected_target_application_id":             identity.ExpectedTargetApplicationID,
		"expected_target_application_version":        identity.ExpectedTargetApplicationVersion,
		"expected_target_image_digest":               identity.ExpectedTargetImageDigest,
		"incompatible_instance_count":                identity.IncompatibleInstanceCount,
		"lifecycle_disabled_observed_at":             identity.LifecycleDisabledObservedAt,
		"lifecycle_gate_present":                     identity.LifecycleGatePresent,
		"potential_writer_instance_count":            identity.PotentialWriterInstanceCount,
		"prior_lifecycle_disabled_inspection_sha256": identity.PriorLifecycleDisabledInspectionSHA256,
		"reviewed_config_sha256":                     identity.ReviewedConfigSHA256,
		"secret_name_inventory_sha256":               identity.SecretNameInventorySHA256,
		"source_instance_inventory_sha256":           identity.SourceInstanceInventorySHA256,
		"target_application_current":                 identity.TargetApplicationCurrent,
		"target_release_commit":                      identity.TargetReleaseCommit,
		"target_release_date":                        identity.TargetReleaseDate,
		"target_release_version":                     identity.TargetReleaseVersion,
		"target_version_instance_count":              identity.TargetVersionInstanceCount,
		"worker_deployment_id":                       identity.WorkerDeploymentID,
		"worker_script_etag":                         identity.WorkerScriptETag,
		"worker_version_id":                          identity.WorkerVersionID,
	}
	if stableOnly {
		delete(value, "container_instance_count")
		delete(value, "source_instance_inventory_sha256")
		delete(value, "target_version_instance_count")
		delete(value, "incompatible_instance_count")
		delete(value, "potential_writer_instance_count")
	}
	return value
}

func canonicalBillingRolloutSourceFence(
	sourceFence billingRolloutSourceFence,
	includeHash bool,
) ([]byte, error) {
	identity, err := canonicalBillingRolloutSourceIdentity(sourceFence.Fence)
	if err != nil {
		return nil, err
	}
	var identityValue any
	if err := json.Unmarshal(identity, &identityValue); err != nil {
		return nil, err
	}
	value := map[string]any{
		"billing_mutation_cohort_accounts": sourceFence.BillingMutationCohortAccounts,
		"fence":                            identityValue,
		"observed_at":                      sourceFence.ObservedAt,
		"schema":                           sourceFence.Schema,
		"source_fleet": map[string]any{
			"api_replicas":        sourceFence.SourceFleet.APIReplicas,
			"reconciler_replicas": sourceFence.SourceFleet.ReconcilerReplicas,
		},
	}
	if includeHash {
		value["inspection_sha256"] = sourceFence.InspectionSHA256
	}
	return json.Marshal(value)
}

func canonicalBillingRolloutProvisional(
	provisional billingRolloutProvisional,
	includeHash bool,
) ([]byte, error) {
	value := map[string]any{
		"account_objects_scanned":          provisional.AccountObjectsScanned,
		"before_source_inspection_sha256":  provisional.BeforeSourceInspectionSHA256,
		"mutation_receipt_objects_scanned": provisional.MutationReceiptObjectsScanned,
		"records": map[string]int{
			"malformed_mutation_receipts": provisional.Records.MalformedMutationReceipts,
			"malformed_pending_changes":   provisional.Records.MalformedPendingChanges,
			"post_retry_horizon_receipts": provisional.Records.PostRetryHorizonReceipts,
			"prepared_downgrades":         provisional.Records.PreparedDowngrades,
			"targetless_pending_changes":  provisional.Records.TargetlessPendingChanges,
		},
		"registry_authority_sha256": provisional.RegistryAuthoritySHA256,
		"scan_completed_at":         provisional.ScanCompletedAt,
		"scan_started_at":           provisional.ScanStartedAt,
		"schema":                    provisional.Schema,
	}
	if includeHash {
		value["provisional_sha256"] = provisional.ProvisionalSHA256
	}
	return json.Marshal(value)
}
