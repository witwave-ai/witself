// Package signuplegal defines the bounded signup refusal and explicit reconsent
// protocol shared by the HTTP client and its durable local journal.
package signuplegal

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/jsonstrict"
)

// Protocol schemas, refusal values and byte limits define the shared wire contract.
const (
	RefusalSchema    = "witself.signup-legal-refusal.v1"
	ReconsentSchema  = "witself.signup-reconsent.v1"
	AckSchema        = "witself.signup-reconsent-ack.v1"
	RefusalCode      = "signup_legal_stale"
	RefusalError     = "signup legal acceptance is out of date"
	MaxCoreBytes     = 32 << 10
	MaxProtocolBytes = 64 << 10
	maxSafeRevision  = 1<<53 - 1
)

var (
	idPattern      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	hashPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	invitePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,63}$`)
)

// Refusal is durable evidence that this exact request was not admitted. Its
// RequestFingerprint is a CP identifier, not the client's local fingerprint.
type Refusal struct {
	SchemaVersion          string `json:"schema_version"`
	Code                   string `json:"code"`
	Error                  string `json:"error"`
	ProvisionID            string `json:"provision_id"`
	RequestFingerprint     string `json:"request_fingerprint"`
	ConsentTermsVersion    string `json:"consent_terms_version"`
	ConsentPrivacyVersion  string `json:"consent_privacy_version"`
	RefusalID              string `json:"refusal_id"`
	RefusalRevision        uint64 `json:"refusal_revision"`
	RequiredTermsVersion   string `json:"required_terms_version"`
	RequiredPrivacyVersion string `json:"required_privacy_version"`
}

// Validate checks the refusal schema, bounded fields and revision.
func (r Refusal) Validate() error {
	if r.SchemaVersion != RefusalSchema || r.Code != RefusalCode || r.Error != RefusalError ||
		!idPattern.MatchString(r.ProvisionID) || !hashPattern.MatchString(r.RequestFingerprint) ||
		!idPattern.MatchString(r.RefusalID) || r.RefusalRevision == 0 || r.RefusalRevision > maxSafeRevision ||
		!versionPattern.MatchString(r.ConsentTermsVersion) || !versionPattern.MatchString(r.ConsentPrivacyVersion) ||
		!versionPattern.MatchString(r.RequiredTermsVersion) || !versionPattern.MatchString(r.RequiredPrivacyVersion) {
		return errors.New("invalid signup legal refusal")
	}
	return nil
}

// ValidateFor binds the refusal to the outstanding signup. The CP fingerprint
// remains opaque; it must never be compared with a locally derived hash.
func (r Refusal) ValidateFor(provisionID, terms, privacy string) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.ProvisionID != provisionID || r.ConsentTermsVersion != terms || r.ConsentPrivacyVersion != privacy {
		return errors.New("signup legal refusal does not match the outstanding request")
	}
	return nil
}

// Candidate identifies one successor signup and its selected legal versions.
type Candidate struct {
	ProvisionID           string `json:"provision_id"`
	ConsentTermsVersion   string `json:"consent_terms_version"`
	ConsentPrivacyVersion string `json:"consent_privacy_version"`
}

// Validate checks the candidate identifier and legal version labels.
func (c Candidate) Validate() error {
	if !idPattern.MatchString(c.ProvisionID) || !versionPattern.MatchString(c.ConsentTermsVersion) ||
		!versionPattern.MatchString(c.ConsentPrivacyVersion) {
		return errors.New("invalid signup reconsent candidate")
	}
	return nil
}

// ReconsentRequest binds a saved refusal and candidate to the original signup core.
type ReconsentRequest struct {
	SchemaVersion string    `json:"schema_version"`
	Refusal       Refusal   `json:"refusal"`
	TransitionID  string    `json:"transition_id"`
	Candidate     Candidate `json:"candidate"`
	Email         string    `json:"email"`
	Invite        string    `json:"invite"`
	DisplayName   string    `json:"display_name"`
}

// Validate checks the transition, normalized core and complete request size.
func (r ReconsentRequest) Validate() error {
	if r.SchemaVersion != ReconsentSchema || r.Refusal.Validate() != nil || r.Candidate.Validate() != nil ||
		!idPattern.MatchString(r.TransitionID) || r.Candidate.ProvisionID == r.Refusal.ProvisionID ||
		!utf8.ValidString(r.Email) || !strings.Contains(r.Email, "@") || r.Email != strings.ToLower(strings.TrimSpace(r.Email)) ||
		!utf8.ValidString(r.DisplayName) || r.DisplayName != strings.TrimSpace(r.DisplayName) ||
		len(utf16.Encode([]rune(r.DisplayName))) > 200 || (r.Invite != "" && !invitePattern.MatchString(r.Invite)) {
		return errors.New("invalid signup reconsent request")
	}
	// IDs and legal labels may grow during reconsent. Budget only the invariant
	// normalized core, using the same HTML and U+2028/U+2029 escaping as the wire.
	core, err := json.Marshal(struct {
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
		Invite      string `json:"invite"`
	}{r.Email, r.DisplayName, r.Invite})
	if err != nil || len(core) > MaxCoreBytes {
		return errors.New("signup reconsent core exceeds the size limit")
	}
	// The complete transport envelope has a separate, larger bound.
	body, err := json.Marshal(r)
	if err != nil || len(body) > MaxProtocolBytes {
		return errors.New("signup reconsent request exceeds the size limit")
	}
	return nil
}

// Ack confirms registration of one exact refusal, transition and candidate.
type Ack struct {
	SchemaVersion               string    `json:"schema_version"`
	Status                      string    `json:"status"`
	Refusal                     Refusal   `json:"refusal"`
	TransitionID                string    `json:"transition_id"`
	Candidate                   Candidate `json:"candidate"`
	CandidateRequestFingerprint string    `json:"candidate_request_fingerprint"`
}

// Validate checks the acknowledgement schema and its bounded registration fields.
func (a Ack) Validate() error {
	if a.SchemaVersion != AckSchema || a.Status != "registered" || a.Refusal.Validate() != nil ||
		a.Candidate.Validate() != nil || !idPattern.MatchString(a.TransitionID) ||
		a.Candidate.ProvisionID == a.Refusal.ProvisionID || !hashPattern.MatchString(a.CandidateRequestFingerprint) {
		return errors.New("invalid signup reconsent acknowledgement")
	}
	return nil
}

// ValidateFor binds a valid acknowledgement to the exact reconsent request.
func (a Ack) ValidateFor(r ReconsentRequest) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if err := a.Validate(); err != nil {
		return err
	}
	if a.Refusal != r.Refusal || a.TransitionID != r.TransitionID || a.Candidate != r.Candidate {
		return errors.New("signup reconsent acknowledgement does not match the pending transition")
	}
	return nil
}

// Pending is local-only. RequestFingerprint guards local journal promotion and
// is deliberately separate from the fingerprints returned by the CP.
type Pending struct {
	Candidate          Candidate `json:"candidate"`
	TransitionID       string    `json:"transition_id"`
	RequestFingerprint string    `json:"request_fingerprint"`
}

// Validate checks the saved candidate, transition and local request fingerprint.
func (p Pending) Validate() error {
	if p.Candidate.Validate() != nil || !idPattern.MatchString(p.TransitionID) || !hashPattern.MatchString(p.RequestFingerprint) {
		return errors.New("invalid pending signup reconsent")
	}
	return nil
}

var refusalKeys = []string{"schema_version", "code", "error", "provision_id", "request_fingerprint",
	"consent_terms_version", "consent_privacy_version", "refusal_id", "refusal_revision",
	"required_terms_version", "required_privacy_version"}
var candidateKeys = []string{"provision_id", "consent_terms_version", "consent_privacy_version"}
var ackKeys = []string{"schema_version", "status", "refusal", "transition_id", "candidate", "candidate_request_fingerprint"}

// DecodeRefusal accepts one complete, uniquely keyed, exactly shaped object.
func DecodeRefusal(body []byte) (Refusal, error) {
	var out Refusal
	if err := uniqueJSON(body); err != nil {
		return out, err
	}
	if _, err := exactObject(body, refusalKeys); err != nil {
		return out, err
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Refusal{}, errors.New("invalid signup legal refusal fields")
	}
	if err := out.Validate(); err != nil {
		return Refusal{}, err
	}
	return out, nil
}

// DecodeAck validates nested shapes too. encoding/json's case-insensitive
// struct matching alone would accept aliases of protocol field names.
func DecodeAck(body []byte) (Ack, error) {
	var out Ack
	if err := uniqueJSON(body); err != nil {
		return out, err
	}
	fields, err := exactObject(body, ackKeys)
	if err != nil {
		return out, err
	}
	if _, err := exactObject(fields["refusal"], refusalKeys); err != nil {
		return out, err
	}
	if _, err := exactObject(fields["candidate"], candidateKeys); err != nil {
		return out, err
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Ack{}, errors.New("invalid signup reconsent acknowledgement fields")
	}
	if err := out.Validate(); err != nil {
		return Ack{}, err
	}
	return out, nil
}

func uniqueJSON(body []byte) error {
	if len(body) > MaxProtocolBytes || !utf8.Valid(body) {
		return errors.New("invalid signup legal response encoding or size")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if jsonstrict.ConsumeUniqueValue(decoder) != nil || jsonstrict.RequireEOF(decoder) != nil {
		return errors.New("signup legal response must contain one complete uniquely keyed JSON object")
	}
	return nil
}

func exactObject(body []byte, keys []string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || len(fields) != len(keys) {
		return nil, errors.New("invalid signup legal response fields")
	}
	for _, key := range keys {
		value, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, errors.New("invalid signup legal response fields")
		}
	}
	return fields, nil
}
