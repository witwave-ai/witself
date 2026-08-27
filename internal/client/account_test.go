package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestValidateAccountConsentVersions(t *testing.T) {
	for _, versions := range [][2]string{
		{"draft-2026-08-22", "privacy_2026.08"},
		{strings.Repeat("a", 64), "v1"},
		{"", ""},
	} {
		if err := validateAccountConsentVersions(
			versions[0], versions[1],
		); err != nil {
			t.Errorf("valid consent %q/%q = %v",
				versions[0], versions[1], err)
		}
	}
	for _, versions := range [][2]string{
		{"owner@example.com", "privacy-2026.08"},
		{strings.Repeat("a", 65), "privacy-2026.08"},
	} {
		if err := validateAccountConsentVersions(
			versions[0], versions[1],
		); err == nil || err.Error() != consentVersionValidationError {
			t.Errorf("invalid consent %q/%q = %v, want %q",
				versions[0], versions[1], err,
				consentVersionValidationError)
		}
	}
}

func TestCreateAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/accounts" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			ProvisionID string `json:"provision_id"`
			Email       string `json:"email"`
			Invite      string `json:"invite"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		switch body.Invite {
		case "good-code":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","provision_id":"` + body.ProvisionID + `","account_id":"acc_1","operator_id":"opr_1",
				"email":"` + body.Email + `","status":"active","bootstrap_token":"witself_boot_x",
				"cell":{"name":"aws-prod-usw2-1","endpoint":"https://api.example.com"}}`))
		case "spent-code":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","error":"invalid invite: fully used"}`))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","error":"no capacity: no accepting cells"}`))
		}
	}))
	defer srv.Close()

	acct, err := CreateAccount(context.Background(), srv.URL, "amy@co.com", "good-code", "Amy")
	if err != nil {
		t.Fatal(err)
	}
	if acct.AccountID != "acc_1" || acct.Cell.Endpoint != "https://api.example.com" || acct.BootstrapToken == "" {
		t.Errorf("account = %+v", acct)
	}
	if !strings.HasPrefix(acct.ProvisionID, "prv_") {
		t.Errorf("provision id = %q", acct.ProvisionID)
	}

	_, err = CreateAccount(context.Background(), srv.URL, "amy@co.com", "spent-code", "")
	if err == nil || !strings.Contains(err.Error(), "fully used") {
		t.Errorf("spent invite error = %v, want server message surfaced", err)
	}

	_, err = CreateAccount(context.Background(), srv.URL, "amy@co.com", "any-other", "")
	if err == nil || !strings.Contains(err.Error(), "no capacity") {
		t.Errorf("no-capacity error = %v, want server message surfaced", err)
	}
}

func TestCreateAccountRetriesWithStableProvisionID(t *testing.T) {
	var attempts int
	var provisionIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		attempts++
		var body struct {
			ProvisionID string `json:"provision_id"`
			Email       string `json:"email"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		provisionIDs = append(provisionIDs, body.ProvisionID)
		switch attempts {
		case 1:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(
				`{"schema_version":"witself.v0","error":"ambiguous cell response"}`,
			))
		case 2:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"schema_version":`))
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(
				`{"schema_version":"witself.v0","provision_id":"` +
					body.ProvisionID +
					`","account_id":"acc_retry","operator_id":"opr_retry",` +
					`"email":"` + body.Email +
					`","status":"pending","bootstrap_token":"witself_boot_retry",` +
					`"cell":{"name":"cell-retry","endpoint":"https://cell.example"}}`,
			))
		}
	}))
	defer srv.Close()

	account, err := CreateAccount(
		context.Background(), srv.URL,
		"retry@witwave.ai", "invite-retry", "Retry",
	)
	if err != nil {
		t.Fatal(err)
	}
	if account.AccountID != "acc_retry" || attempts != 3 {
		t.Fatalf("account = %+v; attempts = %d", account, attempts)
	}
	if len(provisionIDs) != 3 || provisionIDs[0] == "" ||
		provisionIDs[1] != provisionIDs[0] ||
		provisionIDs[2] != provisionIDs[0] {
		t.Fatalf("retry provision ids = %#v", provisionIDs)
	}
}

func TestCreateAccountExactUsesCallerProvisionID(t *testing.T) {
	var received []string
	srv := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		var body struct {
			ProvisionID    string  `json:"provision_id"`
			Email          string  `json:"email"`
			Invite         string  `json:"invite"`
			DisplayName    string  `json:"display_name"`
			TurnstileToken *string `json:"turnstile_token"`
			ConsentTerms   *string `json:"consent_terms_version"`
			ConsentPrivacy *string `json:"consent_privacy_version"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		received = []string{
			body.ProvisionID, body.Email, body.Invite, body.DisplayName,
		}
		if body.TurnstileToken != nil {
			t.Errorf("empty turnstile token was not omitted")
		}
		if body.ConsentTerms != nil || body.ConsentPrivacy != nil {
			t.Errorf("empty consent versions were not omitted")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(
			`{"schema_version":"witself.v0","provision_id":"` +
				body.ProvisionID +
				`","account_id":"acc_exact","operator_id":"opr_exact",` +
				`"email":"` + body.Email +
				`","status":"pending","bootstrap_token":"witself_boot_exact",` +
				`"cell":{"name":"cell-exact","endpoint":"https://cell.example"}}`,
		))
	}))
	defer srv.Close()

	account, err := CreateAccountExact(
		context.Background(), srv.URL+"/",
		" Owner@Example.COM ", " invite-exact ", "",
		"prv_durableRequest1", "", "", "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if account.ProvisionID != "prv_durableRequest1" {
		t.Fatalf("account = %+v", account)
	}
	if want := []string{
		"prv_durableRequest1", "owner@example.com", "invite-exact",
		"owner@example.com",
	}; !reflect.DeepEqual(received, want) {
		t.Fatalf("request = %#v, want %#v", received, want)
	}
	if _, err := CreateAccountExact(
		context.Background(), srv.URL, "a@b.c", "invite", "", "bad id", "",
		"", "",
	); err == nil || !strings.Contains(err.Error(), "invalid provision id") {
		t.Fatalf("invalid provision id error = %v", err)
	}
}

func TestCreateAccountExactSurfacesSignupChallenge(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		attempts++
		var body struct {
			TurnstileToken string `json:"turnstile_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.TurnstileToken != "challenge-response" {
			t.Errorf("turnstile token = %q", body.TurnstileToken)
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(
			`{"schema_version":"witself.v0","error":"turnstile challenge required",` +
				`"challenge_url":"https://control.example/signup/challenge"}`,
		))
	}))
	defer srv.Close()

	_, err := CreateAccountExact(
		context.Background(), srv.URL,
		"owner@example.com", "invite", "Owner", "prv_challenge",
		"challenge-response", "", "",
	)
	var challengeErr *SignupChallengeError
	if !errors.As(err, &challengeErr) {
		t.Fatalf("error = %T %v, want *SignupChallengeError", err, err)
	}
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("error = %v, want ErrForbidden classification", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want fail-fast 1", attempts)
	}
	if challengeErr.ChallengeURL != "https://control.example/signup/challenge" ||
		err.Error() != "turnstile challenge required" {
		t.Fatalf("challenge error = %+v, message = %q", challengeErr, err)
	}
}

func TestAccountCreateRequestOmitsEmptyTurnstileToken(t *testing.T) {
	request := accountCreateRequest{
		DisplayName: "Owner",
		Email:       "owner@example.com",
		Invite:      "invite",
		ProvisionID: "prv_test",
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	const darkBody = `{"display_name":"Owner","email":"owner@example.com",` +
		`"invite":"invite","provision_id":"prv_test"}`
	if string(body) != darkBody {
		t.Fatalf("dark-default body = %s, want %s", body, darkBody)
	}

	request.TurnstileToken = "challenge-response"
	body, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	const challengedBody = `{"display_name":"Owner","email":"owner@example.com",` +
		`"invite":"invite","provision_id":"prv_test",` +
		`"turnstile_token":"challenge-response"}`
	if string(body) != challengedBody {
		t.Fatalf("challenged body = %s, want %s", body, challengedBody)
	}
}

func TestAccountCreateResponseErrorChallenge(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusForbidden,
		Body: io.NopCloser(strings.NewReader(
			`{"error":"turnstile challenge required",` +
				`"challenge_url":"https://control.example/signup/challenge"}`,
		)),
	}
	err := accountCreateResponseError(response, "account creation failed")
	var challengeErr *SignupChallengeError
	if !errors.As(err, &challengeErr) || !errors.Is(err, ErrForbidden) {
		t.Fatalf("error = %T %v, want forbidden signup challenge", err, err)
	}
	if challengeErr.ChallengeURL != "https://control.example/signup/challenge" ||
		challengeErr.Message != "turnstile challenge required" {
		t.Fatalf("challenge error = %+v", challengeErr)
	}
}

func TestAccountCreateRequestFingerprintCanonicalScope(t *testing.T) {
	base, err := AccountCreateRequestFingerprint(
		"https://control.example/", "default",
		" Owner@Example.COM ", " invite-code ", "", "", "",
	)
	if err != nil {
		t.Fatal(err)
	}
	equivalent, err := AccountCreateRequestFingerprint(
		" https://control.example ", "default",
		"owner@example.com", "invite-code", " owner@example.com ", "", "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if base != equivalent || len(base) != 64 {
		t.Fatalf("fingerprints = %q, %q", base, equivalent)
	}
	variants := [][]string{
		{"https://other.example", "default", "owner@example.com", "invite-code", ""},
		{"https://control.example", "other", "owner@example.com", "invite-code", ""},
		{"https://control.example", "default", "other@example.com", "invite-code", ""},
		{"https://control.example", "default", "owner@example.com", "other-invite", ""},
		{"https://control.example", "default", "owner@example.com", "invite-code", "Other"},
	}
	for _, variant := range variants {
		got, err := AccountCreateRequestFingerprint(
			variant[0], variant[1], variant[2], variant[3], variant[4],
			"", "",
		)
		if err != nil {
			t.Fatal(err)
		}
		if got == base {
			t.Fatalf("variant %#v did not change fingerprint", variant)
		}
	}
}

// TestAccountCreateRequestFingerprintGolden pins the consent-less fingerprint
// to the exact value HEAD's algorithm produced before consent capture
// existed. This is the dark contract: journals begun by older CLIs must
// resume under the same fingerprint after an upgrade.
func TestAccountCreateRequestFingerprintGolden(t *testing.T) {
	const golden = "fd32232792508d2ec5746c1a5cca6214f7d8f117b892215cc31d69362610db9a"
	got, err := AccountCreateRequestFingerprint(
		"https://control.example/", "default",
		" Owner@Example.COM ", " invite-code ", "", "", "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != golden {
		t.Fatalf("consent-less fingerprint = %s, want pinned %s", got, golden)
	}

	withConsent, err := AccountCreateRequestFingerprint(
		"https://control.example/", "default",
		"owner@example.com", "invite-code", "",
		"draft-2026-08-22", "draft-2026-08-23",
	)
	if err != nil {
		t.Fatal(err)
	}
	if withConsent == golden {
		t.Fatal("consent did not change the fingerprint")
	}
	swapped, err := AccountCreateRequestFingerprint(
		"https://control.example/", "default",
		"owner@example.com", "invite-code", "",
		"draft-2026-08-23", "draft-2026-08-22",
	)
	if err != nil {
		t.Fatal(err)
	}
	if swapped == withConsent {
		t.Fatal("terms/privacy positions are not domain-separated")
	}
}

func TestCreateAccountExactSendsConsentWhenPresent(t *testing.T) {
	var gotTerms, gotPrivacy *string
	srv := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		var body struct {
			ProvisionID    string  `json:"provision_id"`
			Email          string  `json:"email"`
			ConsentTerms   *string `json:"consent_terms_version"`
			ConsentPrivacy *string `json:"consent_privacy_version"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		gotTerms, gotPrivacy = body.ConsentTerms, body.ConsentPrivacy
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(
			`{"schema_version":"witself.v0","provision_id":"` +
				body.ProvisionID +
				`","account_id":"acc_consent","operator_id":"opr_consent",` +
				`"email":"` + body.Email +
				`","status":"pending","bootstrap_token":"witself_boot_consent",` +
				`"cell":{"name":"cell-consent","endpoint":"https://cell.example"}}`,
		))
	}))
	defer srv.Close()

	if _, err := CreateAccountExact(
		context.Background(), srv.URL,
		"owner@example.com", "invite-consent", "", "prv_consentRequest1", "",
		"draft-2026-08-22", "draft-2026-08-23",
	); err != nil {
		t.Fatal(err)
	}
	if gotTerms == nil || *gotTerms != "draft-2026-08-22" ||
		gotPrivacy == nil || *gotPrivacy != "draft-2026-08-23" {
		t.Fatalf("consent body = %v/%v", gotTerms, gotPrivacy)
	}

	if _, err := CreateAccountExact(
		context.Background(), srv.URL,
		"owner@example.com", "invite-consent", "", "prv_consentRequest2", "",
		"draft-2026-08-22", "",
	); err == nil || !strings.Contains(
		err.Error(), "must be provided together",
	) {
		t.Fatalf("one-of-two consent error = %v", err)
	}

	for _, versions := range [][2]string{
		{"owner@example.com", "draft-2026-08-23"},
		{strings.Repeat("a", 65), "draft-2026-08-23"},
	} {
		if _, err := CreateAccountExact(
			context.Background(), srv.URL,
			"owner@example.com", "invite-consent", "",
			"prv_invalidConsent", "", versions[0], versions[1],
		); err == nil || err.Error() != consentVersionValidationError {
			t.Fatalf("invalid consent %q/%q error = %v, want %q",
				versions[0], versions[1], err,
				consentVersionValidationError)
		}
		if _, err := AccountCreateRequestFingerprint(
			srv.URL, "default", "owner@example.com", "invite-consent", "",
			versions[0], versions[1],
		); err == nil || err.Error() != consentVersionValidationError {
			t.Fatalf("invalid consent fingerprint %q/%q error = %v, want %q",
				versions[0], versions[1], err,
				consentVersionValidationError)
		}
	}
}
