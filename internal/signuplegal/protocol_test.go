package signuplegal

import (
	"encoding/json"
	"strings"
	"testing"
)

func testRefusal() Refusal {
	return Refusal{SchemaVersion: RefusalSchema, Code: RefusalCode, Error: RefusalError,
		ProvisionID: "prv_original", RequestFingerprint: strings.Repeat("a", 64),
		ConsentTermsVersion: "terms-old", ConsentPrivacyVersion: "privacy-old",
		RefusalID: "ref_1", RefusalRevision: 1, RequiredTermsVersion: "terms-current", RequiredPrivacyVersion: "privacy-current"}
}

func testRequest() ReconsentRequest {
	return ReconsentRequest{SchemaVersion: ReconsentSchema, Refusal: testRefusal(), TransitionID: "trn_1",
		Candidate: Candidate{ProvisionID: "prv_successor", ConsentTermsVersion: "terms-newer", ConsentPrivacyVersion: "privacy-newer"},
		Email:     "owner@example.com", Invite: "", DisplayName: "Owner"}
}

func testAck() Ack {
	r := testRequest()
	return Ack{SchemaVersion: AckSchema, Status: "registered", Refusal: r.Refusal,
		TransitionID: r.TransitionID, Candidate: r.Candidate, CandidateRequestFingerprint: strings.Repeat("b", 64)}
}

func encoded(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestLegalProtocolExactIdentity(t *testing.T) {
	r := testRequest()
	a := testAck()
	if err := a.ValidateFor(r); err != nil {
		t.Fatal(err)
	}
	if r.Candidate.ConsentTermsVersion == r.Refusal.RequiredTermsVersion {
		t.Fatal("fixture must cover a manifest change after refusal")
	}
	for name, mutate := range map[string]func(*Ack){
		"original id":       func(a *Ack) { a.Refusal.ProvisionID = "prv_other" },
		"old terms":         func(a *Ack) { a.Refusal.ConsentTermsVersion = "other" },
		"old privacy":       func(a *Ack) { a.Refusal.ConsentPrivacyVersion = "other" },
		"required labels":   func(a *Ack) { a.Refusal.RequiredTermsVersion = "other" },
		"refusal id":        func(a *Ack) { a.Refusal.RefusalID = "ref_other" },
		"refusal revision":  func(a *Ack) { a.Refusal.RefusalRevision++ },
		"CP fingerprint":    func(a *Ack) { a.Refusal.RequestFingerprint = strings.Repeat("c", 64) },
		"candidate id":      func(a *Ack) { a.Candidate.ProvisionID = "prv_other" },
		"candidate terms":   func(a *Ack) { a.Candidate.ConsentTermsVersion = "other" },
		"candidate privacy": func(a *Ack) { a.Candidate.ConsentPrivacyVersion = "other" },
		"transition":        func(a *Ack) { a.TransitionID = "trn_other" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := a
			mutate(&changed)
			if err := changed.ValidateFor(r); err == nil {
				t.Fatal("accepted a different transition")
			}
		})
	}
	p := Pending{Candidate: r.Candidate, TransitionID: r.TransitionID, RequestFingerprint: strings.Repeat("d", 64)}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.RequestFingerprint == a.CandidateRequestFingerprint {
		t.Fatal("fixture conflates local and CP fingerprints")
	}
	// All three hashes are valid and deliberately different: local, original CP,
	// and candidate CP. The acknowledgement is still an exact valid echo.
	if err := a.ValidateFor(r); err != nil {
		t.Fatal(err)
	}
}

func TestLegalRefusalFences(t *testing.T) {
	r := testRefusal()
	if err := r.ValidateFor(r.ProvisionID, r.ConsentTermsVersion, r.ConsentPrivacyVersion); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Refusal){
		"zero revision":   func(r *Refusal) { r.RefusalRevision = 0 },
		"unsafe revision": func(r *Refusal) { r.RefusalRevision = 1 << 53 },
		"missing id":      func(r *Refusal) { r.RefusalID = "" },
		"long id":         func(r *Refusal) { r.RefusalID = strings.Repeat("a", 129) },
		"path id":         func(r *Refusal) { r.ProvisionID = "prv/x" },
		"upper hash":      func(r *Refusal) { r.RequestFingerprint = strings.Repeat("A", 64) },
		"short hash":      func(r *Refusal) { r.RequestFingerprint = "a" },
		"unknown schema":  func(r *Refusal) { r.SchemaVersion = "v2" },
		"unknown code":    func(r *Refusal) { r.Code = "other" },
		"arbitrary error": func(r *Refusal) { r.Error = "other" },
		"empty consent":   func(r *Refusal) { r.ConsentTermsVersion = "" },
		"empty required":  func(r *Refusal) { r.RequiredPrivacyVersion = "" },
		"long label":      func(r *Refusal) { r.RequiredTermsVersion = strings.Repeat("v", 65) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := r
			mutate(&bad)
			if bad.Validate() == nil {
				t.Fatal("accepted invalid refusal")
			}
		})
	}
	r.RefusalRevision = maxSafeRevision
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, fields := range [][3]string{{"prv_other", r.ConsentTermsVersion, r.ConsentPrivacyVersion},
		{r.ProvisionID, "other", r.ConsentPrivacyVersion}, {r.ProvisionID, r.ConsentTermsVersion, "other"},
		{r.ProvisionID, "", ""}} {
		if r.ValidateFor(fields[0], fields[1], fields[2]) == nil {
			t.Fatal("accepted refusal for another or consentless request")
		}
	}
}

func TestLegalProtocolStrictJSON(t *testing.T) {
	refusal, ack := encoded(t, testRefusal()), encoded(t, testAck())
	for _, test := range []struct {
		name, body string
		ack        bool
	}{
		{"refusal duplicate", strings.Replace(refusal, `"refusal_id":`, `"refusal_id":"ref_other","refusal_id":`, 1), false},
		{"refusal escaped duplicate", strings.Replace(refusal, `"refusal_id":`, `"refusal\u005fid":"ref_other","refusal_id":`, 1), false},
		{"refusal casing alias", strings.Replace(refusal, `"refusal_id"`, `"Refusal_ID"`, 1), false},
		{"refusal unknown", strings.Replace(refusal, `{`, `{"extra":false,`, 1), false},
		{"refusal null", strings.Replace(refusal, `"refusal_revision":1`, `"refusal_revision":null`, 1), false},
		{"refusal decimal", strings.Replace(refusal, `"refusal_revision":1`, `"refusal_revision":1.5`, 1), false},
		{"refusal negative", strings.Replace(refusal, `"refusal_revision":1`, `"refusal_revision":-1`, 1), false},
		{"refusal string number", strings.Replace(refusal, `"refusal_revision":1`, `"refusal_revision":"1"`, 1), false},
		{"refusal bool", strings.Replace(refusal, `"refusal_revision":1`, `"refusal_revision":true`, 1), false},
		{"refusal trailing object", refusal + `{}`, false},
		{"refusal trailing null", refusal + `null`, false},
		{"refusal short", refusal[:len(refusal)-1], false},
		{"refusal invalid utf8", refusal + string([]byte{0xff}), false},
		{"refusal too large", refusal + strings.Repeat(" ", MaxProtocolBytes), false},
		{"refusal null object", `null`, false},
		{"refusal array", `[]`, false},
		{"ack nested duplicate", strings.Replace(ack, `"refusal_id":`, `"refusal_id":"ref_other","refusal_id":`, 1), true},
		{"ack candidate escaped duplicate", strings.Replace(ack, `"candidate":{`, `"candidate":{"provision\u005fid":"prv_other",`, 1), true},
		{"ack candidate casing alias", strings.Replace(ack, `"candidate":{"provision_id"`, `"candidate":{"Provision_ID"`, 1), true},
		{"ack nested unknown", strings.Replace(ack, `"candidate":{`, `"candidate":{"extra":null,`, 1), true},
		{"ack missing candidate", strings.Replace(ack, `"candidate":`, `"missing":`, 1), true},
		{"ack null field", strings.Replace(ack, `"transition_id":"trn_1"`, `"transition_id":null`, 1), true},
		{"ack trailing", ack + `{}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var err error
			if test.ack {
				_, err = DecodeAck([]byte(test.body))
			} else {
				_, err = DecodeRefusal([]byte(test.body))
			}
			if err == nil {
				t.Fatal("accepted malformed protocol JSON")
			}
		})
	}
	// Valid escapes have identical decoded meaning. Whitespace up to the exact
	// body limit is permitted, but one extra byte must fail.
	escaped := strings.Replace(refusal, `"refusal_id"`, `"refusal\u005fid"`, 1)
	padded := escaped + strings.Repeat(" ", MaxProtocolBytes-len(escaped))
	if got, err := DecodeRefusal([]byte(padded)); err != nil || got != testRefusal() {
		t.Fatalf("valid escaped boundary: %v", err)
	}
	if _, err := DecodeRefusal([]byte(padded + " ")); err == nil {
		t.Fatal("accepted oversized boundary")
	}
	if got, err := DecodeAck([]byte(ack)); err != nil || got != testAck() {
		t.Fatalf("valid ack: %v", err)
	}
}

func TestReconsentRequestAndPendingValidation(t *testing.T) {
	r := testRequest()
	for name, mutate := range map[string]func(*ReconsentRequest){
		"self target":        func(r *ReconsentRequest) { r.Candidate.ProvisionID = r.Refusal.ProvisionID },
		"missing transition": func(r *ReconsentRequest) { r.TransitionID = "" },
		"upper email":        func(r *ReconsentRequest) { r.Email = "Owner@example.com" },
		"spaced email":       func(r *ReconsentRequest) { r.Email = " owner@example.com" },
		"invalid email":      func(r *ReconsentRequest) { r.Email = "owner" },
		"invalid utf8 email": func(r *ReconsentRequest) { r.Email = "owner" + string([]byte{0xff}) + "@example.com" },
		"surrogate email":    func(r *ReconsentRequest) { r.Email = "owner" + string([]byte{0xed, 0xa0, 0x80}) + "@example.com" },
		"spaced name":        func(r *ReconsentRequest) { r.DisplayName = "Owner " },
		"invalid utf8 name":  func(r *ReconsentRequest) { r.DisplayName = "Owner" + string([]byte{0xff}) },
		"long name":          func(r *ReconsentRequest) { r.DisplayName = strings.Repeat("界", 201) },
		"long astral name":   func(r *ReconsentRequest) { r.DisplayName = strings.Repeat("😀", 101) },
		"invalid invite":     func(r *ReconsentRequest) { r.Invite = " AbC " },
		"oversized core":     func(r *ReconsentRequest) { r.Email = strings.Repeat("a", MaxProtocolBytes) + "@example.com" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := r
			mutate(&bad)
			if bad.Validate() == nil {
				t.Fatal("accepted invalid request")
			}
		})
	}
	r.DisplayName = strings.Repeat("😀", 100)
	r.Invite = "valid-code"
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	p := Pending{Candidate: r.Candidate, TransitionID: r.TransitionID, RequestFingerprint: strings.Repeat("d", 64)}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.RequestFingerprint = ""
	if p.Validate() == nil {
		t.Fatal("accepted pending transition without a local hash")
	}
}

func TestReconsentStableCoreBudget(t *testing.T) {
	// The literal includes the JSON field syntax and the fixed account text.
	// Each test fills the remaining budget using the wire cost of its character.
	const fixedCoreBytes = len(`{"email":"a@example.com","display_name":"Owner","invite":""}`)
	for _, test := range []struct {
		name, character string
		wireBytes       int
	}{
		{"ascii", "a", 1},
		{"less than", "<", 6},
		{"greater than", ">", 6},
		{"ampersand", "&", 6},
		{"line separator", "\u2028", 6},
		{"paragraph separator", "\u2029", 6},
		{"multibyte", "界", 3},
		{"astral", "😀", 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := testRequest()
			available := MaxCoreBytes - fixedCoreBytes
			r.Email = "a" + strings.Repeat(test.character, available/test.wireBytes) +
				strings.Repeat("a", available%test.wireBytes) + "@example.com"
			if err := r.Validate(); err != nil {
				t.Fatalf("exact 32-KiB core: %v", err)
			}
			// The same core remains valid with every variable envelope field at
			// its maximum size, including later, longer consent version labels.
			r.Refusal.ProvisionID = strings.Repeat("o", 128)
			r.Refusal.RefusalID = strings.Repeat("r", 128)
			r.Refusal.RefusalRevision = maxSafeRevision
			r.Refusal.ConsentTermsVersion = strings.Repeat("t", 64)
			r.Refusal.ConsentPrivacyVersion = strings.Repeat("p", 64)
			r.Refusal.RequiredTermsVersion = strings.Repeat("u", 64)
			r.Refusal.RequiredPrivacyVersion = strings.Repeat("q", 64)
			r.Candidate = Candidate{ProvisionID: strings.Repeat("c", 128),
				ConsentTermsVersion: strings.Repeat("v", 64), ConsentPrivacyVersion: strings.Repeat("w", 64)}
			r.TransitionID = strings.Repeat("s", 128)
			if err := r.Validate(); err != nil {
				t.Fatalf("unchanged core with maximum IDs and labels: %v", err)
			}
			// This exact wire ceiling includes Go's HTML and separator escaping;
			// it must remain below the existing 64-KiB transport bound.
			wire := encoded(t, r)
			if len(wire) != 34231 || len(wire) > MaxProtocolBytes {
				t.Fatalf("maximum reconsent request bytes = %d, want 34231", len(wire))
			}
			r.Email = "a" + r.Email
			if len(encoded(t, r)) >= MaxProtocolBytes {
				t.Fatal("over-core fixture must remain below the transport limit")
			}
			if err := r.Validate(); err == nil {
				t.Fatal("accepted 32769-byte core below the transport limit")
			}
		})
	}
}
