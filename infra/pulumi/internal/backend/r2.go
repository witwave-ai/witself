package backend

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// R2AccessKeyIDEnv and the related constants define the dedicated R2 inputs and probe namespace.
const (
	R2AccessKeyIDEnv      = "WITSELF_INFRA_R2_ACCESS_KEY_ID"
	R2SecretAccessKeyEnv  = "WITSELF_INFRA_R2_SECRET_ACCESS_KEY"
	R2StatePassphraseEnv  = "WITSELF_INFRA_STATE_PASSPHRASE"
	R2MinKeyLength        = 16
	R2MinPassphraseLength = 20
	R2ProbePrefix         = ".pulumi/locks/witself-infra-state-check/"
)

// R2Settings identifies an owner-created state bucket.
type R2Settings struct{ Bucket, Endpoint string }

var r2BucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
var r2EndpointPattern = regexp.MustCompile(`^https://[0-9a-f]{32}(\.(eu|us|fedramp))?\.r2\.cloudflarestorage\.com$`)

// ValidateR2Settings checks addressing without disclosing rejected values.
func ValidateR2Settings(s R2Settings) error {
	switch {
	case s.Bucket == "":
		return errors.New("-backend r2 requires -r2-bucket (inventory key r2_bucket)")
	case s.Endpoint == "":
		return errors.New("-backend r2 requires -r2-endpoint (inventory key r2_endpoint)")
	case !r2BucketPattern.MatchString(s.Bucket):
		return errors.New("-r2-bucket must be 3-63 characters of lowercase letters, digits and hyphens, and must not begin or end with a hyphen")
	case !r2EndpointPattern.MatchString(s.Endpoint):
		return errors.New("-r2-endpoint must be https://<account-id>.r2.cloudflarestorage.com, or the eu, us or fedramp form of that host, with no path, query, port or credentials")
	}
	return nil
}

// R2StateURL builds the credential-free Pulumi backend URL.
func R2StateURL(s R2Settings) string {
	return "s3://" + s.Bucket + "?" + (url.Values{"endpoint": {s.Endpoint}, "region": {"auto"}, "request_checksum_calculation": {"when_required"}, "use_path_style": {"true"}}).Encode()
}

// R2Secrets holds values that must never be formatted or serialized.
type R2Secrets struct{ accessKeyID, secretAccessKey, passphrase string }

// String redacts the complete value.
func (R2Secrets) String() string { return "R2Secrets{redacted}" }

// GoString redacts Go-syntax formatting.
func (R2Secrets) GoString() string { return "R2Secrets{redacted}" }

// Format redacts every formatting verb, including pointer formatting.
func (R2Secrets) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, "R2Secrets{redacted}") }

func r2Names() []string {
	return []string{R2AccessKeyIDEnv, R2SecretAccessKeyEnv, R2StatePassphraseEnv}
}

// R2SecretsFromEnv validates the three dedicated values supplied by the caller.
func R2SecretsFromEnv(getenv func(string) string) (R2Secrets, error) {
	names := r2Names()
	values := make([]string, len(names))
	var missing []string
	for i, name := range names {
		values[i] = getenv(name)
		if values[i] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return R2Secrets{}, fmt.Errorf("state backend r2: missing environment variable(s): %s; export them in the shell that runs witself-infra. They are never read from infra.yaml, a flag or a file, and the passphrase is never generated", strings.Join(missing, ", "))
	}
	for i, value := range values {
		if value != strings.TrimSpace(value) {
			return R2Secrets{}, fmt.Errorf("state backend r2: environment variable %s has leading or trailing whitespace; export the exact value", names[i])
		}
	}
	for i := 0; i < 2; i++ {
		if len(values[i]) < R2MinKeyLength {
			return R2Secrets{}, fmt.Errorf("state backend r2: environment variable %s is shorter than 16 characters; it does not look like an R2 key", names[i])
		}
	}
	if len(values[2]) < R2MinPassphraseLength {
		return R2Secrets{}, errors.New("state backend r2: WITSELF_INFRA_STATE_PASSPHRASE is shorter than 20 characters")
	}
	return R2Secrets{values[0], values[1], values[2]}, nil
}

// R2SettingsContainSecret rejects a credential pasted into public addressing.
func R2SettingsContainSecret(s R2Settings, getenv func(string) string) error {
	for _, name := range r2Names() {
		value := getenv(name)
		if len(value) >= 8 && (strings.Contains(s.Bucket, value) || strings.Contains(s.Endpoint, value)) {
			return fmt.Errorf("state backend r2: -r2-bucket or -r2-endpoint contains the value of %s; the inventory and the flags hold names and endpoints, never credential values", name)
		}
	}
	return nil
}

// R2WorkspaceEnv overrides ambient AWS session, profile and endpoint settings,
// and pins the project layout and checkpoint backups. A Civo cell program uses no AWS provider.
func R2WorkspaceEnv(s R2Settings, secrets R2Secrets) map[string]string {
	return map[string]string{
		"PULUMI_BACKEND_URL": R2StateURL(s), "PULUMI_CONFIG_PASSPHRASE": secrets.passphrase,
		"PULUMI_CONFIG_PASSPHRASE_FILE": "", "PULUMI_DIY_BACKEND_DISABLE_CHECKPOINT_BACKUPS": "false", "PULUMI_DIY_BACKEND_LEGACY_LAYOUT": "false",
		"AWS_ACCESS_KEY_ID": secrets.accessKeyID, "AWS_SECRET_ACCESS_KEY": secrets.secretAccessKey,
		"AWS_SESSION_TOKEN": "", "AWS_PROFILE": "", "AWS_DEFAULT_PROFILE": "", "AWS_ENDPOINT_URL": "", "AWS_ENDPOINT_URL_S3": "", "AWS_REQUEST_CHECKSUM_CALCULATION": "", "AWS_RESPONSE_CHECKSUM_VALIDATION": "",
	}
}

// RedactR2Secrets replaces dedicated values wherever they occur in diagnostic text.
func RedactR2Secrets(text string, getenv func(string) string) string {
	for _, name := range []string{R2SecretAccessKeyEnv, R2AccessKeyIDEnv, R2StatePassphraseEnv} {
		if value := getenv(name); len(value) >= 8 {
			text = strings.ReplaceAll(text, value, "[redacted "+name+"]")
		}
	}
	return text
}

// R2CheckOptions selects the stack inventory, when requested.
type R2CheckOptions struct {
	Project, Stack string
	Inventory      bool
}

// R2CheckReport describes the probe and one page of each inventory prefix.
type R2CheckReport struct {
	ProbeKey                                                  string
	StackFile                                                 bool
	StackLocks, HistoryObjects, BackupObjects, ProbeLeftovers string
}

// CheckR2State checks the object operations needed by Pulumi's lock.
func CheckR2State(ctx context.Context, s R2Settings, secrets R2Secrets, opts R2CheckOptions) (R2CheckReport, error) {
	return checkR2State(ctx, s, secrets, opts, s.Endpoint, 3)
}

func checkR2State(ctx context.Context, s R2Settings, secrets R2Secrets, opts R2CheckOptions, endpoint string, attempts int) (R2CheckReport, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client := s3.NewFromConfig(aws.Config{Region: "auto", Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: secrets.accessKeyID, SecretAccessKey: secrets.secretAccessKey}, nil
	})}, func(o *s3.Options) {
		o.BaseEndpoint = &endpoint
		o.UsePathStyle = true
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.DisableLogOutputChecksumValidationSkipped = true
		o.RetryMaxAttempts = attempts
	})
	report := R2CheckReport{}
	list := func(prefix string, maxKeys int32) (*s3.ListObjectsV2Output, error) {
		out, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &s.Bucket, Prefix: &prefix, MaxKeys: &maxKeys})
		if err != nil {
			return nil, fmt.Errorf("state backend r2: list objects in bucket %q failed: %s", s.Bucket, r2ErrorClass(err))
		}
		return out, nil
	}
	if _, err := list(".pulumi/", 1); err != nil {
		return report, err
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return report, errors.New("state backend r2: could not generate probe object name")
	}
	report.ProbeKey = fmt.Sprintf("%s%x.json", R2ProbePrefix, nonce)
	body, _ := json.Marshal(struct {
		Tool    string `json:"tool"`
		Purpose string `json:"purpose"`
		Created string `json:"created"`
	}{"witself-infra", "state-check probe; safe to delete", time.Now().UTC().Format(time.RFC3339)})
	body = append(body, '\n')
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: &s.Bucket, Key: &report.ProbeKey, Body: bytes.NewReader(body)}); err != nil {
		return report, fmt.Errorf("state backend r2: write probe object %q failed: %s", report.ProbeKey, r2ErrorClass(err))
	}
	deleteProbe := func() error {
		_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.Bucket, Key: &report.ProbeKey})
		return err
	}
	cleanup := func(err error) (R2CheckReport, error) {
		if deleteProbe() != nil {
			err = fmt.Errorf("%s; the probe object was left in the bucket", err)
		}
		return report, err
	}
	got, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.Bucket, Key: &report.ProbeKey})
	if err != nil {
		return cleanup(fmt.Errorf("state backend r2: read probe object %q failed: %s", report.ProbeKey, r2ErrorClass(err)))
	}
	readBody, readErr := io.ReadAll(io.LimitReader(got.Body, int64(len(body)+1)))
	_ = got.Body.Close()
	if readErr != nil {
		return cleanup(fmt.Errorf("state backend r2: read probe object %q failed: %s", report.ProbeKey, r2ErrorClass(readErr)))
	}
	if !bytes.Equal(body, readBody) {
		return cleanup(fmt.Errorf("state backend r2: probe object %q read back with different content", report.ProbeKey))
	}
	contains := func(out *s3.ListObjectsV2Output) bool {
		for _, obj := range out.Contents {
			if aws.ToString(obj.Key) == report.ProbeKey {
				return true
			}
		}
		return false
	}
	out, err := list(R2ProbePrefix, 1000)
	if err != nil {
		return cleanup(err)
	}
	if !contains(out) {
		return cleanup(fmt.Errorf("state backend r2: probe object %q was written but a listing did not show it; the Pulumi lock needs a listing that is consistent immediately after a write", report.ProbeKey))
	}
	if err := deleteProbe(); err != nil {
		return report, fmt.Errorf("state backend r2: delete probe object %q failed: %s; Pulumi could not release its lock either. Check for a bucket lock rule that covers .pulumi/locks/ and delete the probe object by hand", report.ProbeKey, r2ErrorClass(err))
	}
	out, err = list(R2ProbePrefix, 1000)
	if err != nil {
		return report, err
	}
	if contains(out) {
		return report, fmt.Errorf("state backend r2: probe object %q was deleted but a listing still shows it; the Pulumi lock needs a listing that is consistent immediately after a delete", report.ProbeKey)
	}
	if opts.Inventory {
		stackFile := ".pulumi/stacks/" + opts.Project + "/" + opts.Stack + ".json"
		prefixes := []string{stackFile, ".pulumi/locks/organization/" + opts.Project + "/" + opts.Stack + "/", ".pulumi/history/" + opts.Project + "/" + opts.Stack + "/", ".pulumi/backups/" + opts.Project + "/" + opts.Stack + "/", R2ProbePrefix}
		counts := []*string{nil, &report.StackLocks, &report.HistoryObjects, &report.BackupObjects, &report.ProbeLeftovers}
		for i, prefix := range prefixes {
			out, err := list(prefix, 1000)
			if err != nil {
				return report, err
			}
			if i == 0 {
				for _, obj := range out.Contents {
					key := aws.ToString(obj.Key)
					if key == stackFile || key == stackFile+".gz" || key == stackFile+".zst" {
						report.StackFile = true
					}
				}
			} else {
				*counts[i] = strconv.Itoa(len(out.Contents))
				if aws.ToBool(out.IsTruncated) {
					*counts[i] = "1000+"
				}
			}
		}
	}
	return report, nil
}

type r2CodedError interface{ ErrorCode() string }
type r2StatusError interface{ HTTPStatusCode() int }

func r2ErrorClass(err error) string {
	var coded r2CodedError
	var statusError r2StatusError
	var code string
	var status int
	if errors.As(err, &coded) {
		code = coded.ErrorCode()
	}
	if errors.As(err, &statusError) {
		status = statusError.HTTPStatusCode()
	}
	var class string
	switch {
	case status != 0 && code != "":
		class = fmt.Sprintf("HTTP %d %s", status, code)
	case code != "":
		class = code
	case status != 0:
		class = fmt.Sprintf("HTTP %d", status)
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		class = "timed out after 30s"
	default:
		class = "network error (DNS, TLS or connection); the endpoint host could not be reached"
	}
	if status == 401 || status == 403 {
		class += "; check the two R2 key variables and that the token has Object Read & Write on this bucket; a new token can take up to a minute to work"
	} else if code == "NoSuchBucket" {
		class += "; the bucket does not exist at this endpoint; the owner creates it, witself-infra never does"
	}
	return class
}
