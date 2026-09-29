package backend

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const testR2Endpoint = "https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com"

var testR2Settings = R2Settings{"witself-state-test", testR2Endpoint}

func testR2Values() map[string]string {
	return map[string]string{R2AccessKeyIDEnv: "test-access-key-id-not-real", R2SecretAccessKeyEnv: "test-secret-access-key-not-real", R2StatePassphraseEnv: "test-passphrase-not-real-0001"}
}
func testR2Secrets(t *testing.T) R2Secrets {
	t.Helper()
	values := testR2Values()
	s, err := R2SecretsFromEnv(func(k string) string { return values[k] })
	if err != nil {
		t.Fatal("fake secret validation failed")
	}
	return s
}
func assertNoR2Values(t *testing.T, text string) {
	t.Helper()
	for _, v := range testR2Values() {
		if strings.Contains(text, v) {
			t.Fatal("diagnostic disclosed a fake secret")
		}
	}
}
func assertR2Error(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error")
	}
	assertNoR2Values(t, err.Error())
	if err.Error() != want {
		t.Fatalf("message mismatch; expected %q", want)
	}
}

func TestValidateR2Settings(t *testing.T) {
	bucketErr := "-r2-bucket must be 3-63 characters of lowercase letters, digits and hyphens, and must not begin or end with a hyphen"
	endpointErr := "-r2-endpoint must be https://<account-id>.r2.cloudflarestorage.com, or the eu, us or fedramp form of that host, with no path, query, port or credentials"
	for _, jurisdiction := range []string{"", ".eu", ".us", ".fedramp"} {
		for _, n := range []int{3, 63} {
			s := R2Settings{strings.Repeat("a", n), strings.Replace(testR2Endpoint, ".r2.", jurisdiction+".r2.", 1)}
			if ValidateR2Settings(s) != nil {
				t.Fatal("valid settings rejected")
			}
		}
	}
	for i, bucket := range []string{"aa", strings.Repeat("a", 64), "Abc", "-abc", "abc-", "a_b", "a.b"} {
		t.Run(fmt.Sprint("bucket", i), func(t *testing.T) {
			assertR2Error(t, ValidateR2Settings(R2Settings{bucket, testR2Endpoint}), bucketErr)
		})
	}
	for i, endpoint := range []string{strings.Replace(testR2Endpoint, "https:", "http:", 1), testR2Endpoint + "/", testR2Endpoint + ":443", testR2Endpoint + "?a=b", strings.Replace(testR2Endpoint, "https://", "https://user@", 1), strings.Replace(testR2Endpoint, "0123", "123", 1), strings.Replace(testR2Endpoint, "abcdef", "ABCDEF", 1), "https://example.com", testR2Endpoint + ".example.com", testR2Endpoint + "\n"} {
		t.Run(fmt.Sprint("endpoint", i), func(t *testing.T) { assertR2Error(t, ValidateR2Settings(R2Settings{"abc", endpoint}), endpointErr) })
	}
	assertR2Error(t, ValidateR2Settings(R2Settings{}), "-backend r2 requires -r2-bucket (inventory key r2_bucket)")
	assertR2Error(t, ValidateR2Settings(R2Settings{Bucket: "abc"}), "-backend r2 requires -r2-endpoint (inventory key r2_endpoint)")
}

func TestR2StateURL(t *testing.T) {
	want := "s3://witself-state-test?endpoint=https%3A%2F%2F0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com&region=auto&request_checksum_calculation=when_required&use_path_style=true"
	got := R2StateURL(testR2Settings)
	if got != want {
		t.Fatal("state URL differs")
	}
	u, err := url.Parse(got)
	if err != nil || u.User != nil || len(u.Query()) != 4 {
		t.Fatal("invalid state URL structure")
	}
}

func TestR2SecretsFromEnv(t *testing.T) {
	names := r2Names()
	for _, missing := range [][]string{{names[0]}, {names[1]}, {names[2]}, names} {
		v := testR2Values()
		for _, n := range missing {
			delete(v, n)
		}
		_, err := R2SecretsFromEnv(func(k string) string { return v[k] })
		assertR2Error(t, err, "state backend r2: missing environment variable(s): "+strings.Join(missing, ", ")+"; export them in the shell that runs witself-infra. They are never read from infra.yaml, a flag or a file, and the passphrase is never generated")
	}
	for _, n := range names {
		for _, side := range []bool{false, true} {
			v := testR2Values()
			if side {
				v[n] = " " + v[n]
			} else {
				v[n] += "\n"
			}
			_, err := R2SecretsFromEnv(func(k string) string { return v[k] })
			assertR2Error(t, err, "state backend r2: environment variable "+n+" has leading or trailing whitespace; export the exact value")
		}
	}
	for i, n := range names {
		minLen := 16
		if i == 2 {
			minLen = 20
		}
		for _, size := range []int{minLen - 1, minLen} {
			v := testR2Values()
			v[n] = strings.Repeat("x", size)
			_, err := R2SecretsFromEnv(func(k string) string { return v[k] })
			if size == minLen {
				if err != nil {
					t.Fatal("minimum length rejected")
				}
				continue
			}
			want := "state backend r2: environment variable " + n + " is shorter than 16 characters; it does not look like an R2 key"
			if i == 2 {
				want = "state backend r2: WITSELF_INFRA_STATE_PASSPHRASE is shorter than 20 characters"
			}
			assertR2Error(t, err, want)
		}
	}
}

func TestR2SecretsNeverFormat(t *testing.T) {
	s := testR2Secrets(t)
	for _, v := range []any{s, &s} {
		for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
			got := fmt.Sprintf(format, v)
			assertNoR2Values(t, got)
			if got != "R2Secrets{redacted}" {
				t.Fatal("format was not fully redacted")
			}
		}
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal("marshal failed")
		}
		assertNoR2Values(t, string(raw))
		if string(raw) != "{}" {
			t.Fatal("JSON exposed fields")
		}
	}
}
func TestR2SettingsContainSecret(t *testing.T) {
	for _, n := range r2Names() {
		v := testR2Values()
		for _, s := range []R2Settings{{Bucket: v[n]}, {Endpoint: "https://" + v[n] + "/"}} {
			assertR2Error(t, R2SettingsContainSecret(s, func(k string) string { return v[k] }), "state backend r2: -r2-bucket or -r2-endpoint contains the value of "+n+"; the inventory and the flags hold names and endpoints, never credential values")
		}
	}
	for _, value := range []string{"1234567", ""} {
		if R2SettingsContainSecret(R2Settings{Bucket: value}, func(string) string { return value }) != nil {
			t.Fatal("short or unset value rejected")
		}
	}
}
func TestR2WorkspaceEnv(t *testing.T) {
	v := testR2Values()
	want := map[string]string{"PULUMI_BACKEND_URL": R2StateURL(testR2Settings), "PULUMI_CONFIG_PASSPHRASE": v[R2StatePassphraseEnv], "PULUMI_CONFIG_PASSPHRASE_FILE": "", "PULUMI_DIY_BACKEND_DISABLE_CHECKPOINT_BACKUPS": "false", "PULUMI_DIY_BACKEND_LEGACY_LAYOUT": "false", "AWS_ACCESS_KEY_ID": v[R2AccessKeyIDEnv], "AWS_SECRET_ACCESS_KEY": v[R2SecretAccessKeyEnv], "AWS_SESSION_TOKEN": "", "AWS_PROFILE": "", "AWS_DEFAULT_PROFILE": "", "AWS_ENDPOINT_URL": "", "AWS_ENDPOINT_URL_S3": "", "AWS_REQUEST_CHECKSUM_CALCULATION": "", "AWS_RESPONSE_CHECKSUM_VALIDATION": ""}
	if !reflect.DeepEqual(R2WorkspaceEnv(testR2Settings, testR2Secrets(t)), want) {
		t.Fatal("workspace environment differs")
	}
}
func TestRedactR2Secrets(t *testing.T) {
	v := testR2Values()
	get := func(k string) string { return v[k] }
	for n, value := range v {
		if RedactR2Secrets(value+value, get) != strings.Repeat("[redacted "+n+"]", 2) {
			t.Fatal("replacement mismatch")
		}
	}
	for _, getenv := range []func(string) string{get, func(string) string { return "" }, func(string) string { return "1234567" }} {
		if RedactR2Secrets("unchanged 1234567", getenv) != "unchanged 1234567" {
			t.Fatal("unrelated text changed")
		}
	}
}

type r2Request struct {
	method, path string
	query        url.Values
	headers      http.Header
}
type r2Fake struct {
	server                       *httptest.Server
	objects                      map[string][]byte
	requests                     []r2Request
	failAt                       map[int]int
	errorBody                    string
	hide, keep, wrong, truncated bool
	deleted                      string
}

func newR2Fake(t *testing.T) *r2Fake {
	t.Helper()
	f := &r2Fake{objects: map[string][]byte{}, failAt: map[int]int{}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r2Request{r.Method, r.URL.Path, r.URL.Query(), r.Header.Clone()})
		if status := f.failAt[len(f.requests)]; status != 0 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, f.errorBody)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/"+testR2Settings.Bucket+"/")
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
			prefix := r.URL.Query().Get("prefix")
			keys := []string{}
			for k := range f.objects {
				if strings.HasPrefix(k, prefix) && (!f.hide || !strings.HasPrefix(k, R2ProbePrefix)) {
					keys = append(keys, k)
				}
			}
			if f.keep && f.deleted != "" && strings.HasPrefix(f.deleted, prefix) {
				keys = append(keys, f.deleted)
			}
			sort.Strings(keys)
			type object struct {
				Key string `xml:"Key"`
			}
			result := struct {
				XMLName   xml.Name `xml:"ListBucketResult"`
				Truncated bool     `xml:"IsTruncated"`
				Objects   []object `xml:"Contents"`
			}{Truncated: f.truncated}
			for _, k := range keys {
				result.Objects = append(result.Objects, object{k})
			}
			w.Header().Set("Content-Type", "application/xml")
			_ = xml.NewEncoder(w).Encode(result)
		case r.Method == http.MethodPut:
			data, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(500)
				return
			}
			f.objects[key] = data
		case r.Method == http.MethodGet:
			if f.wrong {
				_, _ = io.WriteString(w, "different")
			} else {
				_, _ = w.Write(f.objects[key])
			}
		case r.Method == http.MethodDelete:
			delete(f.objects, key)
			f.deleted = key
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}
func (f *r2Fake) check(t *testing.T, inventory bool) (R2CheckReport, error) {
	t.Helper()
	return checkR2State(context.Background(), testR2Settings, testR2Secrets(t), R2CheckOptions{Project: "witself-infra", Stack: "test-cell", Inventory: inventory}, f.server.URL, 1)
}
func checkR2Requests(t *testing.T, f *r2Fake) {
	t.Helper()
	values := testR2Values()
	for _, r := range f.requests {
		if !strings.HasPrefix(r.path, "/"+testR2Settings.Bucket) {
			t.Fatal("request is not path style")
		}
		auth := r.headers.Get("Authorization")
		if !strings.Contains(auth, "Credential="+values[R2AccessKeyIDEnv]+"/") || !strings.Contains(auth, "/auto/s3/aws4_request") {
			t.Fatal("request signing identity or region differs")
		}
		if r.headers.Get("X-Amz-Security-Token") != "" {
			t.Fatal("ambient session token sent")
		}
		if r.method == http.MethodPut {
			for k := range r.headers {
				if strings.HasPrefix(k, "X-Amz-Checksum-") || k == "X-Amz-Sdk-Checksum-Algorithm" || k == "X-Amz-Trailer" {
					t.Fatalf("unexpected header %s", k)
				}
			}
			if strings.Contains(r.headers.Get("Content-Encoding"), "aws-chunked") {
				t.Fatal("unexpected chunked signing")
			}
		}
	}
}
func TestCheckR2StateHappyPath(t *testing.T) {
	f := newR2Fake(t)
	report, err := f.check(t, false)
	if err != nil {
		t.Fatal("probe failed")
	}
	methods := []string{}
	for _, r := range f.requests {
		methods = append(methods, r.method)
	}
	if !reflect.DeepEqual(methods, []string{"GET", "PUT", "GET", "GET", "DELETE", "GET"}) {
		t.Fatalf("request methods: %v", methods)
	}
	checkR2Requests(t, f)
	if !strings.HasPrefix(report.ProbeKey, R2ProbePrefix) || !strings.HasSuffix(report.ProbeKey, ".json") || len(report.ProbeKey) != len(R2ProbePrefix)+21 {
		t.Fatal("invalid probe key")
	}
	if len(f.objects) != 0 {
		t.Fatal("probe was not deleted")
	}
	if f.requests[0].query.Get("max-keys") != "1" || f.requests[0].query.Get("prefix") != ".pulumi/" {
		t.Fatal("initial list bounds differ")
	}
}
func TestCheckR2StateInventory(t *testing.T) {
	for _, suffix := range []string{"", ".gz", ".zst", ".bak"} {
		f := newR2Fake(t)
		f.objects[".pulumi/stacks/witself-infra/test-cell.json"+suffix] = nil
		f.objects[".pulumi/locks/organization/witself-infra/test-cell/one"] = nil
		f.objects[".pulumi/locks/organization/witself-infra/other/one"] = nil
		for i := 0; i < 2; i++ {
			f.objects[fmt.Sprint(".pulumi/history/witself-infra/test-cell/", i)] = nil
		}
		for i := 0; i < 3; i++ {
			f.objects[fmt.Sprint(".pulumi/backups/witself-infra/test-cell/", i)] = nil
		}
		f.objects[R2ProbePrefix+"old.json"] = nil
		r, err := f.check(t, true)
		if err != nil || r.StackFile != (suffix != ".bak") || r.StackLocks != "1" || r.HistoryObjects != "2" || r.BackupObjects != "3" || r.ProbeLeftovers != "1" {
			t.Fatal("inventory differs")
		}
		for _, req := range f.requests[6:] {
			if req.query.Get("max-keys") != "1000" {
				t.Fatal("inventory unbounded")
			}
		}
		f.truncated = true
		r, err = f.check(t, true)
		if err != nil || r.StackLocks != "1000+" || r.HistoryObjects != "1000+" || r.BackupObjects != "1000+" || r.ProbeLeftovers != "1000+" {
			t.Fatal("truncated count differs")
		}
	}
}

func TestCheckR2StateFailures(t *testing.T) {
	hint := "; check the two R2 key variables and that the token has Object Read & Write on this bucket; a new token can take up to a minute to work"
	for _, tc := range []struct {
		name              string
		at, status        int
		body, class, kind string
		calls             int
	}{
		{"list denied", 1, 403, "<Error><Code>AccessDenied</Code></Error>", "HTTP 403 AccessDenied" + hint, "list", 1},
		{"missing bucket", 1, 404, "<Error><Code>NoSuchBucket</Code></Error>", "HTTP 404 NoSuchBucket; the bucket does not exist at this endpoint; the owner creates it, witself-infra never does", "list", 1},
		{"malformed unavailable", 1, 503, "<Error><Code>", "HTTP 503", "list", 1},
		{"plain unavailable", 1, 503, "upstream unavailable", "HTTP 503 ServiceUnavailable", "list", 1},
		{"malformed denied", 1, 403, "<Error><Code>", "HTTP 403" + hint, "list", 1},
		{"put denied", 2, 403, "<Error><Code>AccessDenied</Code></Error>", "HTTP 403 AccessDenied" + hint, "write", 2},
		{"get denied", 3, 403, "<Error><Code>AccessDenied</Code></Error>", "HTTP 403 AccessDenied" + hint, "read", 4},
		{"different read", 0, 0, "", "", "different", 4},
		{"hidden write", 0, 0, "", "", "hidden", 5},
		{"delete denied", 5, 403, "<Error><Code>AccessDenied</Code></Error>", "HTTP 403 AccessDenied" + hint, "delete", 5},
		{"kept delete", 0, 0, "", "", "kept", 6},
		{"list after write denied", 4, 403, "<Error><Code>AccessDenied</Code></Error>", "HTTP 403 AccessDenied" + hint, "list", 5},
		{"list after delete denied", 6, 403, "<Error><Code>AccessDenied</Code></Error>", "HTTP 403 AccessDenied" + hint, "list", 6},
		{"inventory denied", 7, 403, "<Error><Code>AccessDenied</Code></Error>", "HTTP 403 AccessDenied" + hint, "list", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newR2Fake(t)
			f.failAt[tc.at] = tc.status
			f.errorBody = tc.body
			f.wrong = tc.kind == "different"
			f.hide = tc.kind == "hidden"
			f.keep = tc.kind == "kept"
			r, err := f.check(t, tc.at == 7)
			want := ""
			switch tc.kind {
			case "list":
				want = fmt.Sprintf("state backend r2: list objects in bucket %q failed: %s", testR2Settings.Bucket, tc.class)
			case "write", "read":
				want = fmt.Sprintf("state backend r2: %s probe object %q failed: %s", tc.kind, r.ProbeKey, tc.class)
			case "delete":
				want = fmt.Sprintf("state backend r2: delete probe object %q failed: %s; Pulumi could not release its lock either. Check for a bucket lock rule that covers .pulumi/locks/ and delete the probe object by hand", r.ProbeKey, tc.class)
			case "different":
				want = fmt.Sprintf("state backend r2: probe object %q read back with different content", r.ProbeKey)
			case "hidden":
				want = fmt.Sprintf("state backend r2: probe object %q was written but a listing did not show it; the Pulumi lock needs a listing that is consistent immediately after a write", r.ProbeKey)
			case "kept":
				want = fmt.Sprintf("state backend r2: probe object %q was deleted but a listing still shows it; the Pulumi lock needs a listing that is consistent immediately after a delete", r.ProbeKey)
			}
			assertR2Error(t, err, want)
			if len(f.requests) != tc.calls {
				t.Fatalf("request count: got %d want %d", len(f.requests), tc.calls)
			}
			wantObjects := 0
			if tc.kind == "delete" {
				wantObjects = 1
			}
			if len(f.objects) != wantObjects {
				t.Fatal("cleanup object count differs")
			}
		})
	}
	t.Run("closed endpoint", func(t *testing.T) {
		f := newR2Fake(t)
		f.server.Close()
		_, err := f.check(t, false)
		assertR2Error(t, err, fmt.Sprintf("state backend r2: list objects in bucket %q failed: network error (DNS, TLS or connection); the endpoint host could not be reached", testR2Settings.Bucket))
	})
	t.Run("cleanup denied", func(t *testing.T) {
		f := newR2Fake(t)
		f.wrong = true
		f.failAt[4] = 403
		r, err := f.check(t, false)
		assertR2Error(t, err, fmt.Sprintf("state backend r2: probe object %q read back with different content; the probe object was left in the bucket", r.ProbeKey))
		if len(f.objects) != 1 {
			t.Fatal("leftover missing")
		}
	})
}
func TestCheckR2StateIgnoresAmbientAWSEnvironment(t *testing.T) {
	for _, n := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE"} {
		t.Setenv(n, "ambient-test-value-not-real")
	}
	f := newR2Fake(t)
	if _, err := f.check(t, false); err != nil {
		t.Fatal("probe failed")
	}
	checkR2Requests(t, f)
}
func TestR2ErrorClassContext(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		if r2ErrorClass(fmt.Errorf("wrapped: %w", err)) != "timed out after 30s" {
			t.Fatal("context classification differs")
		}
	}
	if r2ErrorClass(errors.New("private network diagnostic")) != "network error (DNS, TLS or connection); the endpoint host could not be reached" {
		t.Fatal("network classification differs")
	}
}

type r2TestCodeError struct{ code string }

func (r2TestCodeError) Error() string       { return "private diagnostic" }
func (e r2TestCodeError) ErrorCode() string { return e.code }

type r2TestHTTPError struct {
	status int
	err    error
}

func (r2TestHTTPError) Error() string         { return "private HTTP diagnostic" }
func (e r2TestHTTPError) HTTPStatusCode() int { return e.status }
func (e r2TestHTTPError) Unwrap() error       { return e.err }

func TestR2ErrorClassPriority(t *testing.T) {
	hint := "; check the two R2 key variables and that the token has Object Read & Write on this bucket; a new token can take up to a minute to work"
	for _, tc := range []struct {
		err  error
		want string
	}{
		{r2TestCodeError{"AccessDenied"}, "AccessDenied"},
		{r2TestHTTPError{503, context.DeadlineExceeded}, "HTTP 503"},
		{r2TestHTTPError{401, r2TestCodeError{"AccessDenied"}}, "HTTP 401 AccessDenied" + hint},
		{r2TestHTTPError{401, context.Canceled}, "HTTP 401" + hint},
		{r2TestCodeError{"NoSuchBucket"}, "NoSuchBucket; the bucket does not exist at this endpoint; the owner creates it, witself-infra never does"},
	} {
		if got := r2ErrorClass(tc.err); got != tc.want {
			t.Fatalf("class mismatch; expected %q", tc.want)
		}
	}
}
