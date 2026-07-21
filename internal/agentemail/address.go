package agentemail

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

const (
	maximumAgentSegmentBytes = 47
	maximumLocalPartBytes    = 64
)

// ErrAddressInvalid reports an address outside the canonical pilot grammar.
var ErrAddressInvalid = errors.New("invalid agent-email address")

// AddressParts is one canonical pilot recipient. Address is the full envelope
// value including a plus tag when present; BaseAddress is the exact enrolled
// address used for routing and mailbox lookup.
type AddressParts struct {
	Address       string
	BaseAddress   string
	Domain        string
	LocalPart     string
	AgentSegment  string
	RealmLabel    string
	SubaddressTag string
}

// RealmLabelFromID derives the settled 16-character routing label.
func RealmLabelFromID(realmID string) (string, error) {
	if len(realmID) != len("realm_")+16 || !strings.HasPrefix(realmID, "realm_") {
		return "", fmt.Errorf("%w: realm id is invalid", ErrAddressInvalid)
	}
	label := strings.TrimPrefix(realmID, "realm_")
	if !validRealmLabel(label) {
		return "", fmt.Errorf("%w: realm id body is invalid", ErrAddressInvalid)
	}
	return label, nil
}

// DeriveAgentSegment applies the settled NFKC/name sanitization pipeline. It
// fails closed on empty, reserved, or over-budget results; it never truncates
// or auto-suffixes.
func DeriveAgentSegment(name string) (string, error) {
	name = strings.ToLower(norm.NFKC.String(name))
	var b strings.Builder
	lastHyphen := false
	for _, r := range name {
		if unicode.IsSpace(r) || r == '_' || r == '.' {
			r = '-'
		}
		allowed := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !allowed {
			continue
		}
		if r == '-' {
			if b.Len() == 0 || lastHyphen {
				continue
			}
			lastHyphen = true
			b.WriteByte('-')
			continue
		}
		lastHyphen = false
		b.WriteRune(r)
	}
	segment := strings.Trim(b.String(), "-")
	return ValidateAgentSegment(segment)
}

// ValidateAgentSegment validates an explicit operator override or a derived
// segment against the same address and reserved-role policy.
func ValidateAgentSegment(segment string) (string, error) {
	segment = strings.ToLower(strings.TrimSpace(segment))
	if len(segment) < 1 || len(segment) > maximumAgentSegmentBytes ||
		segment[0] == '-' || segment[len(segment)-1] == '-' {
		return "", fmt.Errorf("%w: agent segment is empty or exceeds its 47-byte budget", ErrAddressInvalid)
	}
	for _, c := range []byte(segment) {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return "", fmt.Errorf("%w: agent segment must use lowercase ASCII letters, digits, and hyphens", ErrAddressInvalid)
		}
	}
	if reservedAgentSegments[segment] {
		return "", fmt.Errorf("%w: agent segment is reserved", ErrAddressInvalid)
	}
	return segment, nil
}

// ValidateDomain accepts only canonical lowercase ASCII DNS names. The pilot
// deliberately does not perform implicit IDNA conversion.
func ValidateDomain(domain string) (string, error) {
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if len(domain) < 1 || len(domain) > 253 || strings.Contains(domain, "..") {
		return "", fmt.Errorf("%w: domain is invalid", ErrAddressInvalid)
	}
	labels := strings.Split(domain, ".")
	for _, label := range labels {
		if len(label) < 1 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("%w: domain label is invalid", ErrAddressInvalid)
		}
		for _, c := range []byte(label) {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return "", fmt.Errorf("%w: domain must be lowercase ASCII DNS", ErrAddressInvalid)
			}
		}
	}
	return domain, nil
}

// ComposeAddress creates one enrolled base address from validated components.
func ComposeAddress(segment, realmID, domain string) (AddressParts, error) {
	segment, err := ValidateAgentSegment(segment)
	if err != nil {
		return AddressParts{}, err
	}
	label, err := RealmLabelFromID(realmID)
	if err != nil {
		return AddressParts{}, err
	}
	domain, err = ValidateDomain(domain)
	if err != nil {
		return AddressParts{}, err
	}
	localPart := segment + "." + label
	if len(localPart) > maximumLocalPartBytes {
		return AddressParts{}, fmt.Errorf("%w: local part exceeds its 64-byte SMTP limit", ErrAddressInvalid)
	}
	base := localPart + "@" + domain
	return AddressParts{
		Address: base, BaseAddress: base, Domain: domain, LocalPart: localPart,
		AgentSegment: segment, RealmLabel: label,
	}, nil
}

// ParseRecipient validates the receive grammar, preserving one bounded plus
// tag while returning the exact untagged enrollment key.
func ParseRecipient(recipient, expectedDomain string) (AddressParts, error) {
	recipient = strings.ToLower(strings.TrimSpace(recipient))
	if strings.HasPrefix(recipient, "<") && strings.HasSuffix(recipient, ">") {
		recipient = strings.TrimSpace(recipient[1 : len(recipient)-1])
	}
	if len(recipient) < 3 || len(recipient) > 320 || strings.Count(recipient, "@") != 1 {
		return AddressParts{}, fmt.Errorf("%w: recipient is malformed", ErrAddressInvalid)
	}
	local, domain, _ := strings.Cut(recipient, "@")
	if len(local) < 1 || len(local) > maximumLocalPartBytes {
		return AddressParts{}, fmt.Errorf("%w: local part exceeds its 64-byte SMTP limit", ErrAddressInvalid)
	}
	domain, err := ValidateDomain(domain)
	if err != nil {
		return AddressParts{}, err
	}
	if expectedDomain != "" {
		expectedDomain, err = ValidateDomain(expectedDomain)
		if err != nil || domain != expectedDomain {
			return AddressParts{}, fmt.Errorf("%w: recipient domain is not enrolled", ErrAddressInvalid)
		}
	}
	tag := ""
	if i := strings.IndexByte(local, '+'); i >= 0 {
		tag = local[i+1:]
		local = local[:i]
		if !validSubaddressTag(tag) {
			return AddressParts{}, fmt.Errorf("%w: subaddress tag is invalid", ErrAddressInvalid)
		}
	}
	if strings.Count(local, ".") != 1 {
		return AddressParts{}, fmt.Errorf("%w: recipient local part is malformed", ErrAddressInvalid)
	}
	segment, label, _ := strings.Cut(local, ".")
	segment, err = ValidateAgentSegment(segment)
	if err != nil || !validRealmLabel(label) {
		return AddressParts{}, fmt.Errorf("%w: recipient components are invalid", ErrAddressInvalid)
	}
	base := local + "@" + domain
	return AddressParts{
		Address: recipient, BaseAddress: base, Domain: domain, LocalPart: local,
		AgentSegment: segment, RealmLabel: label, SubaddressTag: tag,
	}, nil
}

func validRealmLabel(label string) bool {
	if len(label) != 16 {
		return false
	}
	for _, c := range []byte(label) {
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

func validSubaddressTag(tag string) bool {
	if len(tag) < 1 || len(tag) > 64 {
		return false
	}
	for _, c := range []byte(tag) {
		if c < 0x21 || c > 0x7e || c == '@' || c == '+' {
			return false
		}
	}
	return true
}

var reservedAgentSegments = map[string]bool{
	"abuse": true, "admin": true, "administrator": true,
	"billing": true, "hostmaster": true, "info": true,
	"mailer-daemon": true, "no-reply": true, "noc": true,
	"noreply": true, "postmaster": true, "root": true,
	"sales": true, "security": true, "support": true, "webmaster": true,
}
