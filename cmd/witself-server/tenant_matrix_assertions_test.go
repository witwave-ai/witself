package main

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
)

type capturedResponse struct {
	Status  int
	Headers http.Header
	Body    []byte
	// RequestPath distinguishes a ServeMux subtree redirect from an application
	// redirect. Capture the path of the request that produced this response.
	RequestPath string
}

func tenantRequestContains(req tenantRequest, value string) bool {
	if value == "" {
		return false
	}
	for _, s := range []string{req.Path, req.RawQuery, string(req.Body)} {
		if strings.Contains(s, value) {
			return true
		}
	}
	// Escaped query/path values still count as carried by the request.
	if decoded, err := url.QueryUnescape(req.RawQuery); err == nil && strings.Contains(decoded, value) {
		return true
	}
	if decoded, err := url.PathUnescape(req.Path); err == nil && strings.Contains(decoded, value) {
		return true
	}
	for _, values := range req.Headers {
		for _, s := range values {
			if strings.Contains(s, value) {
				return true
			}
		}
	}
	return false
}

// leaks returns values, not a printable diagnostic. Runtime callers must report
// only the count or fixture key/kind, never a token or secret value.
func leaks(resp *capturedResponse, req tenantRequest, values []string) []string {
	var found []string
	seen := map[string]bool{}
	for _, value := range values {
		if value == "" || seen[value] || tenantRequestContains(req, value) {
			continue
		}
		seen[value] = true
		present := strings.Contains(string(resp.Body), value)
		for key, headerValues := range resp.Headers {
			if strings.EqualFold(key, "Date") {
				continue
			}
			for _, header := range headerValues {
				present = present || strings.Contains(header, value)
			}
		}
		if present {
			found = append(found, value)
		}
	}
	return found
}

// sameRefusal compares only the observable refusal contract; it deliberately
// never includes bodies or substituted values in errors. Sort longest first so
// an ID that prefixes another ID cannot swallow the latter's replacement.
func sameRefusal(owner, missing *capturedResponse, subst map[string]string) error {
	if owner.Status != missing.Status {
		return fmt.Errorf("refusal status differs: owner=%d missing=%d", owner.Status, missing.Status)
	}
	if owner.Headers.Get("Content-Type") != missing.Headers.Get("Content-Type") {
		return fmt.Errorf("refusal Content-Type differs")
	}
	keys := make([]string, 0, len(subst))
	for key, value := range subst {
		if key == "" || value == "" {
			return fmt.Errorf("empty refusal substitution")
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	ownerPairs, missingPairs := map[string]string{}, map[string]string{}
	for i, key := range keys {
		placeholder := fmt.Sprintf("<v%d>", i+1)
		ownerPairs[key] = placeholder
		if _, duplicate := missingPairs[subst[key]]; duplicate {
			return fmt.Errorf("ambiguous refusal substitution")
		}
		missingPairs[subst[key]] = placeholder
	}
	if tenantNormalize(string(owner.Body), ownerPairs) != tenantNormalize(string(missing.Body), missingPairs) {
		return fmt.Errorf("refusal body differs after ID normalization")
	}
	return nil
}

func tenantNormalize(body string, replacements map[string]string) string {
	keys := make([]string, 0, len(replacements))
	for key := range replacements {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	pairs := make([]string, 0, 2*len(keys))
	for _, key := range keys {
		pairs = append(pairs, key, replacements[key])
	}
	return strings.NewReplacer(pairs...).Replace(body)
}

func isMuxFallback(resp *capturedResponse) bool {
	mediaType, _, _ := mime.ParseMediaType(resp.Headers.Get("Content-Type"))
	if mediaType == "application/json" || json.Valid(resp.Body) {
		return false
	}
	switch resp.Status {
	case http.StatusNotFound:
		return mediaType == "text/plain" && string(resp.Body) == "404 page not found\n"
	case http.StatusMethodNotAllowed:
		return resp.Headers.Get("Allow") != ""
	case http.StatusMovedPermanently, http.StatusTemporaryRedirect:
		location, err := url.Parse(resp.Headers.Get("Location"))
		return err == nil && location.Host == "" && location.Scheme == "" && resp.RequestPath != "" && !strings.HasSuffix(resp.RequestPath, "/") && location.Path == resp.RequestPath+"/"
	default:
		return false
	}
}

func TestTenantMatrixAssertionHelpers(t *testing.T) {
	const canary = "tcb1234567890abcdef"
	const carried = "mem_1234567890abcdef"
	response := func(status int, body string) *capturedResponse {
		return &capturedResponse{Status: status, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}
	}
	t.Run("fixture values exclude local sequences", func(t *testing.T) {
		f := syntheticTenantFixture()
		for _, key := range []string{"avatar.version", "avatar.profile.revision", "avatar.style.version", "enrollment_expiry"} {
			resp := response(200, f.value(sideB, key))
			if len(leaks(resp, tenantRequest{}, f.values(sideB))) != 0 {
				t.Errorf("non-identity fixture slot %q counted as a B value", key)
			}
		}
		if len(leaks(response(200, f.value(sideB, "agent")), tenantRequest{}, f.values(sideB))) != 1 {
			t.Error("B agent ID no longer participates in leak detection")
		}
		for _, value := range f.ownValues {
			if len(leaks(response(200, value), tenantRequest{}, f.values(sideB))) != 0 {
				t.Error("A-own fixture value included in B values")
			}
		}
	})
	t.Run("own probes require dedicated missing slots", func(t *testing.T) {
		f := syntheticTenantFixture()
		for _, build := range []func(*tenantFixture, tenantSide) tenantRequest{
			tenantBuild("POST", "/v1/transcripts", "", `{"title":"${canary.transcript.title}"}`),
			func(f *tenantFixture, _ tenantSide) tenantRequest {
				return tenantRequest{Path: f.value(sideMissing, "transcript")}
			},
			func(f *tenantFixture, _ tenantSide) tenantRequest { return tenantRequest{Path: f.own("transcript")} },
		} {
			if tenantOwnProbeError(f, tenantProbe{Build: build}) == nil {
				t.Error("own probe accepted shared or side-dependent fixture slots")
			}
		}
		for _, row := range tenantRows() {
			for _, probe := range row.Probes {
				if probe.Kind == own {
					if err := tenantOwnProbeError(f, probe); err != nil {
						t.Errorf("declared own probe %q/%q: %v", row.Pattern, probe.Name, err)
					}
				}
			}
		}
	})
	t.Run("namespace challenges vary the requested key", func(t *testing.T) {
		f := syntheticTenantFixture()
		for _, row := range tenantRows() {
			for _, probe := range row.Probes {
				if !probe.CallerNamespaced {
					continue
				}
				if err := tenantNamespaceError(f, row, probe); err != nil {
					t.Errorf("namespace probe %q: %v", row.Pattern, err)
				}
				original := probe.NamespaceBuild
				probe.NamespaceBuild = func(f *tenantFixture, _ tenantSide) tenantRequest { return original(f, sideMissing) }
				if tenantNamespaceError(f, row, probe) == nil {
					t.Error("namespace challenge accepted a constant missing-side key")
				}
			}
		}
	})
	t.Run("ShowsOwn is confined to reads", func(t *testing.T) {
		for _, pattern := range []string{"POST /v1/memory-vector-profiles", "POST /v1/email:send", "POST /v1/message-requests", "PUT /v1/fact-subjects/{subject}", "PATCH /v1/account/placement-policy"} {
			method, _ := tenantPatternParts(pattern)
			if tenantOwnRead(pattern, method) {
				t.Errorf("write pattern %q accepted as own read", pattern)
			}
		}
		for _, pattern := range []string{"GET /v1/memories", "POST /v1/memories:recall", "POST /v1/messages:listen", "POST /v1/email:listen"} {
			method, _ := tenantPatternParts(pattern)
			if !tenantOwnRead(pattern, method) {
				t.Errorf("read pattern %q rejected", pattern)
			}
		}
	})

	t.Run("body reference isolates one field", func(t *testing.T) {
		f := syntheticTenantFixture()
		cases := []struct {
			name, path, body, ref string
			valid                 bool
		}{
			{"valid", "/v1/memories/${own.memory}/supersede", `{"target":"${memory}","content":"${own.adjust_content}"}`, "target", true},
			{"nested", "/v1/memories/${own.memory}/supersede", `{"refs":[{"id":"${memory}"}]}`, "refs.0.id", true},
			{"foreign path", "/v1/memories/${memory}/supersede", `{"target":"${memory}"}`, "target", false},
			{"second field", "/v1/memories/${own.memory}/supersede", `{"target":"${memory}","other":"${transcript}"}`, "target", false},
			{"duplicate value", "/v1/memories/${own.memory}/supersede", `{"target":"${memory}","other":"${memory}"}`, "target", false},
			{"wrong field", "/v1/memories/${own.memory}/supersede", `{"target":"${memory}"}`, "other", false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				probe := tenantProbe{BodyRef: tc.ref, Build: tenantBuild("POST", tc.path, "", tc.body)}
				if (tenantBodyRefError(f, probe) == nil) != tc.valid {
					t.Error("BodyRef validation result differs")
				}
			})
		}
	})
	t.Run("credentials are supplied by runner", func(t *testing.T) {
		f := syntheticTenantFixture()
		const credential = "synthetic\"credential&/with space"
		build := tenantBuild("POST", "/credential/${cred}", "credential=${cred}", `{"bootstrap_token":"${cred}"}`)
		for _, side := range []tenantSide{sideB, sideMissing} {
			unresolved := build(f, side)
			unresolved.Headers.Set("X-Test", "${cred}")
			resolved := tenantResolveCredential(unresolved, credential)
			var body map[string]string
			query, err := url.ParseQuery(resolved.RawQuery)
			if err != nil || query.Get("credential") != credential {
				t.Error("credential query escaping failed")
			}
			if json.Unmarshal(resolved.Body, &body) != nil || body["bootstrap_token"] != credential {
				t.Error("credential JSON escaping failed")
			}
			if resolved.Path != "/credential/"+url.PathEscape(credential) || resolved.Headers.Get("X-Test") != credential {
				t.Error("credential path/header escaping failed")
			}
			if unresolved.Headers.Get("X-Test") != "${cred}" || !strings.Contains(string(unresolved.Body), "${cred}") {
				t.Error("credential resolution mutated the reusable template")
			}
		}
	})

	t.Run("leaks", func(t *testing.T) {
		cases := []struct {
			name   string
			resp   *capturedResponse
			req    tenantRequest
			values []string
			want   int
		}{
			{"body", response(200, `{"content":"`+canary+`"}`), tenantRequest{}, []string{canary}, 1},
			{"Location", &capturedResponse{Headers: http.Header{"Location": {"/" + canary}}}, tenantRequest{}, []string{canary}, 1},
			{"request path", response(404, carried), tenantRequest{Path: "/v1/memories/" + carried}, []string{carried}, 0},
			{"request query", response(404, carried), tenantRequest{RawQuery: "id=" + carried}, []string{carried}, 0},
			{"request escaped query", response(404, "b@example.invalid"), tenantRequest{RawQuery: "email=b%40example.invalid"}, []string{"b@example.invalid"}, 0},
			{"request body", response(404, carried), tenantRequest{Body: []byte(carried)}, []string{carried}, 0},
			{"request header", response(404, carried), tenantRequest{Headers: http.Header{"X-Test": {carried}}}, []string{carried}, 0},
			{"Date", &capturedResponse{Headers: http.Header{"dAtE": {canary}}}, tenantRequest{}, []string{canary}, 0},
			{"duplicate and empty", response(200, canary), tenantRequest{}, []string{"", canary, canary}, 1},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := len(leaks(tc.resp, tc.req, tc.values)); got != tc.want {
					t.Fatalf("leak count=%d want=%d", got, tc.want)
				}
			})
		}
	})
	t.Run("sameRefusal", func(t *testing.T) {
		owner := response(404, `{"error":"realm rlm_b agent agt_b"}`)
		missing := response(404, `{"error":"realm rlm_m agent agt_m"}`)
		subst := map[string]string{"rlm_b": "rlm_m", "agt_b": "agt_m"}
		if err := sameRefusal(owner, missing, subst); err != nil {
			t.Fatal(err)
		}
		missing.Status = 403
		if sameRefusal(owner, missing, subst) == nil {
			t.Error("different status accepted")
		}
		missing.Status = 404
		missing.Body = []byte(`{"error":"deleted realm rlm_m agent agt_m"}`)
		if sameRefusal(owner, missing, subst) == nil {
			t.Error("different normalized body accepted")
		}
		missing.Body = owner.Body
		missing.Headers.Set("Content-Type", "text/plain")
		if sameRefusal(owner, missing, nil) == nil {
			t.Error("different Content-Type accepted")
		}
		if err := sameRefusal(response(404, "id_bb id_b"), response(404, "id_mm id_m"), map[string]string{"id_b": "id_m", "id_bb": "id_mm"}); err != nil {
			t.Fatalf("overlapping IDs: %v", err)
		}
	})
	t.Run("muxFallback", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /known", func(http.ResponseWriter, *http.Request) {})
		mux.HandleFunc("/subtree/", func(http.ResponseWriter, *http.Request) {})
		for _, tc := range []struct {
			method, path string
			status       int
		}{{"GET", "/missing", 404}, {"POST", "/known", 405}, {"GET", "/subtree", 301}} {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
			resp := &capturedResponse{Status: rr.Code, Headers: rr.Header(), Body: rr.Body.Bytes(), RequestPath: tc.path}
			wantStatus := rr.Code == tc.status || (tc.status == 301 && rr.Code == 307)
			if !wantStatus || !isMuxFallback(resp) {
				t.Errorf("real mux fallback not recognized: got status %d, expected %d (or 307 for subtree redirect)", rr.Code, tc.status)
			}
			if rr.Code == 301 || rr.Code == 307 {
				resp.Status = 301
				if !isMuxFallback(resp) {
					t.Error("301 subtree redirect not recognized")
				}
			}
		}
		if isMuxFallback(response(404, `{"schema_version":"witself.v0","error":"not found"}`+"\n")) {
			t.Error("JSON refusal treated as mux fallback")
		}
		if isMuxFallback(&capturedResponse{Status: 301, Headers: http.Header{"Location": {"/elsewhere/"}}, RequestPath: "/subtree"}) {
			t.Error("unrelated redirect treated as subtree fallback")
		}
		if isMuxFallback(&capturedResponse{Status: 405, Headers: http.Header{}}) {
			t.Error("405 without Allow treated as mux fallback")
		}
	})
}
