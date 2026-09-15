package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/signuplegal"
)

func legalClientRefusal() signuplegal.Refusal {
	return signuplegal.Refusal{SchemaVersion: signuplegal.RefusalSchema, Code: signuplegal.RefusalCode,
		Error: signuplegal.RefusalError, ProvisionID: "prv_original", RequestFingerprint: strings.Repeat("a", 64),
		ConsentTermsVersion: "terms-old", ConsentPrivacyVersion: "privacy-old", RefusalID: "ref_1", RefusalRevision: 1,
		RequiredTermsVersion: "terms-current", RequiredPrivacyVersion: "privacy-current"}
}

func legalClientRequest() signuplegal.ReconsentRequest {
	return signuplegal.ReconsentRequest{SchemaVersion: signuplegal.ReconsentSchema, Refusal: legalClientRefusal(),
		TransitionID: "trn_1", Candidate: signuplegal.Candidate{ProvisionID: "prv_successor",
			ConsentTermsVersion: "terms-newer", ConsentPrivacyVersion: "privacy-newer"},
		Email: "owner@example.com", Invite: "", DisplayName: "Owner"}
}

func legalClientAck() signuplegal.Ack {
	r := legalClientRequest()
	return signuplegal.Ack{SchemaVersion: signuplegal.AckSchema, Status: "registered", Refusal: r.Refusal,
		TransitionID: r.TransitionID, Candidate: r.Candidate, CandidateRequestFingerprint: strings.Repeat("b", 64)}
}

func legalClientJSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func legalClientCreate(ctx context.Context, endpoint string) error {
	_, err := CreateAccountExact(ctx, endpoint, "owner@example.com", "", "Owner", "prv_original", "", "terms-old", "privacy-old")
	return err
}

func TestCreateAccountExactValidatesDurableLegalRefusal(t *testing.T) {
	want := legalClientRefusal()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/accounts" {
			t.Error("wrong signup request")
		}
		var sent accountCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Error(err)
		}
		if sent.ProvisionID != want.ProvisionID || sent.ConsentTermsVersion != want.ConsentTermsVersion || sent.ConsentPrivacyVersion != want.ConsentPrivacyVersion {
			t.Error("wrong outstanding signup")
		}
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()
	err := legalClientCreate(context.Background(), srv.URL)
	var refusal *SignupLegalRefusalError
	if !errors.As(err, &refusal) || !errors.Is(err, ErrConflict) || refusal.Refusal != want {
		t.Fatalf("typed refusal = %T %v", err, err)
	}
	if requests.Load() != 1 {
		t.Fatalf("refusal retried %d times", requests.Load())
	}
	if err.Error() != signuplegal.RefusalError {
		t.Fatalf("message = %q", err.Error())
	}
}

func TestCreateAccountExactRejectsUnprovenLegalRefusal(t *testing.T) {
	valid := legalClientJSON(t, legalClientRefusal())
	for _, test := range []struct {
		name, body string
		status     int
		short      bool
	}{
		{"generic conflict", `{"error":"already exists"}`, 409, false},
		{"wrong id", strings.Replace(valid, "prv_original", "prv_other", 1), 409, false},
		{"wrong terms", strings.Replace(valid, "terms-old", "terms-other", 1), 409, false},
		{"wrong privacy", strings.Replace(valid, "privacy-old", "privacy-other", 1), 409, false},
		{"duplicate alias", strings.Replace(valid, `"provision_id":`, `"provision\u005fid":"prv_other","provision_id":`, 1), 409, false},
		{"unknown field", strings.Replace(valid, `{`, `{"extra":false,`, 1), 409, false},
		{"trailing JSON", valid + `{}`, 409, false},
		{"truncated JSON", valid[:len(valid)-1], 409, false},
		{"truncated transport after valid JSON", valid, 409, true},
		{"oversize", valid + strings.Repeat(" ", signuplegal.MaxProtocolBytes), 409, false},
		{"typed body on unavailable status", valid, 503, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				if test.short {
					w.Header().Set("Content-Length", strconv.Itoa(len(test.body)+10))
				}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer srv.Close()
			err := legalClientCreate(context.Background(), srv.URL)
			var refusal *SignupLegalRefusalError
			if err == nil || errors.As(err, &refusal) {
				t.Fatalf("unproven refusal = %T %v", err, err)
			}
			if test.status == 409 && requests.Load() != 1 {
				t.Fatal("conflict must stop the command")
			}
		})
	}
}

func TestRegisterAccountReconsentExactEchoAndStableRetry(t *testing.T) {
	want := legalClientRequest()
	wantAck := legalClientAck()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/account-signups/prv_original:reconsent" {
			t.Error("wrong reconsent endpoint")
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Error("wrong content type")
		}
		var sent signuplegal.ReconsentRequest
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil || sent != want {
			t.Errorf("changed persisted request: %v", err)
		}
		if attempt == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(wantAck)
	}))
	defer srv.Close()
	if ack, err := RegisterAccountReconsent(context.Background(), srv.URL, want); err == nil || ack != nil || requests.Load() != 1 {
		t.Fatal("ambiguous registration must leave the same transition pending")
	}
	ack, err := RegisterAccountReconsent(context.Background(), srv.URL, want)
	if err != nil || ack == nil || *ack != wantAck || requests.Load() != 2 {
		t.Fatalf("exact replay = %+v, %v", ack, err)
	}
	// The CP candidate hash is opaque. It intentionally differs from the local
	// request fingerprint and the original CP fingerprint.
	local, err := AccountCreateRequestFingerprint(srv.URL, "local", want.Email, want.Invite, want.DisplayName, want.Candidate.ConsentTermsVersion, want.Candidate.ConsentPrivacyVersion)
	if err != nil || local == ack.CandidateRequestFingerprint || local == ack.Refusal.RequestFingerprint {
		t.Fatal("fixture conflates fingerprint domains")
	}
}

func TestRegisterAccountReconsentRejectsUnprovenAck(t *testing.T) {
	valid := legalClientJSON(t, legalClientAck())
	for _, test := range []struct {
		name, body string
		status     int
		short      bool
	}{
		{"wrong predecessor", strings.Replace(valid, "prv_original", "prv_other", 1), 200, false},
		{"wrong refusal", strings.Replace(valid, "ref_1", "ref_other", 1), 200, false},
		{"wrong old pair", strings.Replace(valid, "privacy-old", "privacy-other", 1), 200, false},
		{"wrong candidate", strings.Replace(valid, "prv_successor", "prv_other", 1), 200, false},
		{"wrong new pair", strings.Replace(valid, "terms-newer", "terms-other", 1), 200, false},
		{"wrong transition", strings.Replace(valid, "trn_1", "trn_other", 1), 200, false},
		{"wrong status", strings.Replace(valid, "registered", "prepared", 1), 200, false},
		{"nested duplicate alias", strings.Replace(valid, `"candidate":{`, `"candidate":{"provision\u005fid":"prv_other",`, 1), 200, false},
		{"nested unknown", strings.Replace(valid, `"candidate":{`, `"candidate":{"extra":true,`, 1), 200, false},
		{"trailing JSON", valid + `null`, 200, false},
		{"truncated transport after valid JSON", valid, 200, true},
		{"oversize", valid + strings.Repeat(" ", signuplegal.MaxProtocolBytes), 200, false},
		{"wrong HTTP status", valid, 201, false},
		{"conflict", valid, 409, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.short {
					w.Header().Set("Content-Length", strconv.Itoa(len(test.body)+1))
				}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer srv.Close()
			ack, err := RegisterAccountReconsent(context.Background(), srv.URL, legalClientRequest())
			if err == nil || ack != nil {
				t.Fatal("accepted unproven acknowledgement")
			}
		})
	}
}

func TestSignupMutationsNeverFollowRedirects(t *testing.T) {
	for _, reconsent := range []bool{false, true} {
		for _, status := range []int{301, 302, 303, 307, 308} {
			for _, destination := range []string{"same", "foreign", "userinfo", "downgrade"} {
				t.Run(strconv.FormatBool(reconsent)+"/"+strconv.Itoa(status)+"/"+destination, func(t *testing.T) {
					var followed atomic.Int32
					target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { followed.Add(1); w.WriteHeader(200) }))
					defer target.Close()
					cleartext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { followed.Add(1); w.WriteHeader(200) }))
					defer cleartext.Close()
					srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/redirected" {
							followed.Add(1)
							return
						}
						location := "/redirected"
						switch destination {
						case "foreign":
							location = target.URL + "/redirected"
						case "userinfo":
							location = "https://private:credential@" + strings.TrimPrefix(target.URL, "https://") + "/redirected"
						case "downgrade":
							location = cleartext.URL + "/redirected"
						}
						w.Header().Set("Location", location)
						w.WriteHeader(status)
					}))
					defer srv.Close()
					previous := http.DefaultTransport
					http.DefaultTransport = srv.Client().Transport
					defer func() { http.DefaultTransport = previous }()
					var err error
					if reconsent {
						_, err = RegisterAccountReconsent(context.Background(), srv.URL, legalClientRequest())
					} else {
						err = legalClientCreate(context.Background(), srv.URL)
					}
					if err == nil || followed.Load() != 0 {
						t.Fatalf("mutation redirect followed=%d err=%v", followed.Load(), err)
					}
					if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "credential") {
						t.Fatal("redirect credentials leaked into error")
					}
				})
			}
		}
	}
}

type legalRoundTripFunc func(*http.Request) (*http.Response, error)

func (f legalRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSignupMutationsRejectInvalidSelectedEndpointsBeforeNetwork(t *testing.T) {
	var calls int
	previous := http.DefaultTransport
	http.DefaultTransport = legalRoundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected request") })
	defer func() { http.DefaultTransport = previous }()
	for _, endpoint := range []string{"https://user:private@cp.example", "https://cp.example?token=private", "https://cp.example?", "https://cp.example#private", "ftp://cp.example", "https:///missing", "//cp.example", "https://cp.example:99999", "https://cp.example:"} {
		if err := legalClientCreate(context.Background(), endpoint); err == nil {
			t.Fatal("accepted invalid signup endpoint")
		}
		if ack, err := RegisterAccountReconsent(context.Background(), endpoint, legalClientRequest()); err == nil || ack != nil {
			t.Fatal("accepted invalid reconsent endpoint")
		}
	}
	bad := legalClientRequest()
	bad.Candidate.ProvisionID = bad.Refusal.ProvisionID
	if ack, err := RegisterAccountReconsent(context.Background(), "https://cp.example", bad); err == nil || ack != nil {
		t.Fatal("accepted self-target before network")
	}
	if calls != 0 {
		t.Fatalf("invalid request reached network %d times", calls)
	}
}

type legalInterruptedBody struct {
	ctx       context.Context
	prefix    []byte
	started   chan struct{}
	signalled bool
	closed    *atomic.Bool
}

func (b *legalInterruptedBody) Read(p []byte) (int, error) {
	if len(b.prefix) != 0 {
		n := copy(p, b.prefix)
		b.prefix = b.prefix[n:]
		return n, nil
	}
	if !b.signalled {
		close(b.started)
		b.signalled = true
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (b *legalInterruptedBody) Close() error { b.closed.Store(true); return nil }

func TestSignupLegalTransportRequiresCompleteBodyWithinContext(t *testing.T) {
	for _, reconsent := range []bool{false, true} {
		for _, stage := range []string{"headers", "body"} {
			t.Run(strconv.FormatBool(reconsent)+"/"+stage, func(t *testing.T) {
				started := make(chan struct{})
				var closed atomic.Bool
				previous := http.DefaultTransport
				http.DefaultTransport = legalRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					if stage == "headers" {
						close(started)
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					status, body := 409, legalClientJSON(t, legalClientRefusal())
					if reconsent {
						status, body = 200, legalClientJSON(t, legalClientAck())
					}
					return &http.Response{StatusCode: status, Request: r, Header: make(http.Header), Body: &legalInterruptedBody{ctx: r.Context(), prefix: []byte(body), started: started, closed: &closed}}, nil
				})
				defer func() { http.DefaultTransport = previous }()
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				var err error
				if reconsent {
					var ack *signuplegal.Ack
					ack, err = RegisterAccountReconsent(ctx, "https://cp.example", legalClientRequest())
					if ack != nil {
						t.Fatal("promoted incomplete body")
					}
				} else {
					err = legalClientCreate(ctx, "https://cp.example")
				}
				select {
				case <-started:
				default:
					t.Fatal("deadline test did not enter intended I/O stage")
				}
				var refusal *SignupLegalRefusalError
				if err == nil || errors.As(err, &refusal) || ctx.Err() != context.DeadlineExceeded {
					t.Fatalf("incomplete transport = %T %v", err, err)
				}
				if stage == "body" && !closed.Load() {
					t.Fatal("body was not closed")
				}
			})
		}
	}
}

func TestSignupLegalResponseMustRemainAtSelectedAuthority(t *testing.T) {
	for _, changed := range []string{"https://other.example/v1/accounts", "http://cp.example/v1/accounts", "https://cp.example:444/v1/accounts", "https://private:credential@cp.example/v1/accounts", "https://cp.example/other"} {
		t.Run(changed, func(t *testing.T) {
			previous := http.DefaultTransport
			http.DefaultTransport = legalRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				copied := r.Clone(r.Context())
				copied.URL, _ = url.Parse(changed)
				return &http.Response{StatusCode: 409, Request: copied, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(legalClientJSON(t, legalClientRefusal())))}, nil
			})
			defer func() { http.DefaultTransport = previous }()
			err := legalClientCreate(context.Background(), "https://cp.example")
			var refusal *SignupLegalRefusalError
			if err == nil || errors.As(err, &refusal) {
				t.Fatal("accepted refusal outside selected endpoint")
			}
			if strings.Contains(err.Error(), "credential") {
				t.Fatal("private endpoint leaked")
			}
		})
	}
}
