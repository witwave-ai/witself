package trust

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"
)

// Options has no ambient authority. Roots must be supplied explicitly, and the
// caller must explicitly attest whether Clock is trustworthy. AllowRollback is
// intent only: the current high-water release must also authorize that target.
type Options struct {
	Roots         []PublicKey
	State         TrustState
	Clock         Clock
	ExpectedTag   string
	OS            string
	Arch          string
	AllowRollback bool
}

// Verifier authenticates exact signed documents against explicit roots and state.
type Verifier struct {
	options   Options
	state     TrustState
	rootError Reason
}

// ProductionRoots remains empty until the separate S7-F2 custody decision.
func ProductionRoots() []PublicKey { return nil }

// NewVerifier constructs a verifier without ambient roots, time trust, or state.
// Invalid or absent roots are reported by its verification methods as refusals.
func NewVerifier(options Options) *Verifier {
	v := &Verifier{options: options, state: options.State}
	v.options.Roots = append([]PublicKey(nil), options.Roots...)
	if v.state == nil {
		v.state = NewMemoryState()
	}
	if len(options.Roots) == 0 {
		v.rootError = ReasonNoTrustRoot
	}
	seen := map[string]bool{}
	for _, key := range options.Roots {
		if _, err := ParsePublicKey(key); err != nil {
			v.rootError = reasonOf(err)
			break
		}
		if seen[key.KeyID] {
			v.rootError = ReasonInvalidKey
			break
		}
		seen[key.KeyID] = true
	}
	return v
}

func reasonOf(err error) Reason {
	if err == nil {
		return ReasonAccepted
	}
	var reason Reason
	if errors.As(err, &reason) {
		return reason
	}
	return ReasonInvalidDocument
}

func refusal(reason Reason) Outcome { return Outcome{Reason: reason, Freshness: UnknownClock} }
func failed(out Outcome, err error) Outcome {
	out.Accepted = false
	out.Reason = reasonOf(err)
	return out
}
func accepted(out Outcome) Outcome { out.Accepted = true; out.Reason = ReasonAccepted; return out }

func digestBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (v *Verifier) freshness(issued, expires string) Freshness {
	if !v.options.Clock.Trusted {
		return UnknownClock
	}
	start, err := time.Parse(time.RFC3339, issued)
	if err != nil {
		return UnknownClock
	}
	end, err := time.Parse(time.RFC3339, expires)
	if err != nil {
		return UnknownClock
	}
	if v.options.Clock.Now.Before(start) {
		return NotYetValid
	}
	if !v.options.Clock.Now.Before(end) {
		return Expired
	}
	return Fresh
}

func freshnessReason(f Freshness) Reason {
	switch f {
	case Fresh:
		return ReasonAccepted
	case Expired:
		return ReasonExpired
	case NotYetValid:
		return ReasonNotYetValid
	default:
		return ReasonUnknownClock
	}
}

func (v *Verifier) validateState(s *StateSnapshot) error {
	if s.FormatVersion != 1 || s.Releases == nil || s.Installed == nil {
		return ReasonStateInvalid
	}
	if s.RootSequence == 0 {
		if s.InitialRootID != "" || s.Root.KeyID != "" || s.DelegationSequence != 0 || s.Delegation != nil || len(s.Releases) != 0 || len(s.Installed) != 0 || len(s.RootHistory) != 0 {
			return ReasonStateInvalid
		}
		return nil
	}
	found := false
	for _, root := range v.options.Roots {
		if root.KeyID == s.InitialRootID {
			found = true
		}
	}
	if !found {
		return ReasonStateInvalid
	}
	if _, err := ParsePublicKey(s.Root); err != nil {
		return ReasonStateInvalid
	}
	if s.RootSequence == 1 && s.Root.KeyID != s.InitialRootID {
		return ReasonStateInvalid
	}
	rootIDs := map[string]bool{}
	for _, id := range s.RootHistory {
		if !digestPattern.MatchString(id) || rootIDs[id] {
			return ReasonStateInvalid
		}
		rootIDs[id] = true
	}
	if !rootIDs[s.Root.KeyID] || !rootIDs[s.InitialRootID] || uint64(len(s.RootHistory)) != s.RootSequence || s.RootHistory[0] != s.InitialRootID || s.RootHistory[len(s.RootHistory)-1] != s.Root.KeyID {
		return ReasonStateInvalid
	}
	if s.Delegation != nil {
		if s.Delegation.RootSequence != s.RootSequence || s.Delegation.DelegationSequence != s.DelegationSequence || !digestPattern.MatchString(s.DelegationDigest) {
			return ReasonStateInvalid
		}
		raw, err := json.Marshal(s.Delegation)
		if err != nil {
			return ReasonStateInvalid
		}
		if _, err := ParseDelegation(raw); err != nil {
			return ReasonStateInvalid
		}
		for _, key := range s.Delegation.Keys {
			if v.isRoot(key.KeyID, s) {
				return ReasonStateInvalid
			}
		}
	} else if s.DelegationDigest != "" {
		return ReasonStateInvalid
	}
	for channel, release := range s.Releases {
		if channel != "stable" || release.Sequence == 0 || !releaseTag.MatchString(release.Tag) || !digestPattern.MatchString(release.Digest) {
			return ReasonStateInvalid
		}
		seen := map[uint64]bool{}
		for _, target := range release.AllowedTargets {
			if target == 0 || target >= release.Sequence || seen[target] {
				return ReasonStateInvalid
			}
			seen[target] = true
		}
	}
	for digest, installed := range s.Installed {
		if !digestPattern.MatchString(digest) || !digestPattern.MatchString(installed.EnvelopeDigest) || !digestPattern.MatchString(installed.KeyID) || installed.RootSequence == 0 || installed.RootSequence > s.RootSequence || installed.DelegationSequence == 0 || installed.DelegationSequence > s.DelegationSequence || installed.ReleaseSequence == 0 || installed.ReleaseSequence > s.Releases["stable"].Sequence {
			return ReasonStateInvalid
		}
		if !releaseTag.MatchString(installed.ReleaseTag) || (installed.OS != "linux" && installed.OS != "darwin" && installed.OS != "windows") || (installed.Arch != "amd64" && installed.Arch != "arm64") {
			return ReasonStateInvalid
		}
		if err := validateCommon(1, "component_set", "component_set", installed.IssuedAt, installed.ExpiresAt); err != nil {
			return ReasonStateInvalid
		}
	}
	return nil
}

func parseSignedEnvelope(doc, rawEnvelope []byte, kind string, maxBytes, count int) (Envelope, error) {
	if len(doc) > maxBytes {
		return Envelope{}, ReasonDocumentTooLarge
	}
	envelope, err := ParseEnvelope(rawEnvelope)
	if err != nil {
		return Envelope{}, err
	}
	if envelope.Kind != kind {
		return Envelope{}, ReasonWrongKind
	}
	if len(envelope.Signatures) != count {
		return Envelope{}, ReasonSignatureCount
	}
	seen := map[string]bool{}
	for _, signature := range envelope.Signatures {
		if seen[signature.KeyID] {
			return Envelope{}, ReasonSignatureCount
		}
		seen[signature.KeyID] = true
	}
	return envelope, nil
}

func (v *Verifier) isRoot(keyID string, s *StateSnapshot) bool {
	if keyID == s.Root.KeyID {
		return true
	}
	for _, id := range s.RootHistory {
		if keyID == id {
			return true
		}
	}
	for _, key := range v.options.Roots {
		if key.KeyID == keyID {
			return true
		}
	}
	return false
}

func (v *Verifier) rootSigner(s *StateSnapshot, signatures []Signature) (PublicKey, Signature, error) {
	if s.RootSequence != 0 {
		for _, signature := range signatures {
			if signature.KeyID == s.Root.KeyID {
				return s.Root, signature, nil
			}
		}
	} else {
		for _, signature := range signatures {
			for _, root := range v.options.Roots {
				if signature.KeyID == root.KeyID {
					return root, signature, nil
				}
			}
		}
	}
	if s.Delegation != nil {
		for _, signature := range signatures {
			for _, key := range s.Delegation.Keys {
				if key.KeyID == signature.KeyID {
					return PublicKey{}, Signature{}, ReasonWrongSigner
				}
			}
		}
	}
	return PublicKey{}, Signature{}, ReasonUnknownKey
}

func initializeRoot(s *StateSnapshot, key PublicKey) {
	if s.RootSequence == 0 {
		s.RootSequence = 1
		s.Root = key
		s.InitialRootID = key.KeyID
		s.RootHistory = []string{key.KeyID}
	}
}

// VerifyRotation authenticates the current-root signature before reading NewRoot;
// only then does it verify new-root possession and commit the complete rotation.
func (v *Verifier) VerifyRotation(doc, rawEnvelope []byte) Outcome {
	if v.rootError != "" {
		return refusal(v.rootError)
	}
	out := refusal(ReasonInvalidDocument)
	err := v.state.Update(func(s *StateSnapshot) error {
		if err := v.validateState(s); err != nil {
			return err
		}
		envelope, err := parseSignedEnvelope(doc, rawEnvelope, "root_rotation", MaxAuthorityBytes, 2)
		if err != nil {
			return err
		}
		root, signature, err := v.rootSigner(s, envelope.Signatures)
		if err != nil {
			return err
		}
		if err := VerifySignature(doc, root, signature); err != nil {
			return err
		}
		rotation, err := ParseRootRotation(doc)
		if err != nil {
			return err
		}
		// With several explicit bootstrap roots, envelope ordering can select the
		// new key first. Its trusted signature authenticated the parsed document;
		// now require the other signer to be an explicit old authority as well.
		if s.RootSequence == 0 && root.KeyID == rotation.NewRoot.KeyID {
			var other []Signature
			for _, candidate := range envelope.Signatures {
				if candidate.KeyID != rotation.NewRoot.KeyID {
					other = append(other, candidate)
				}
			}
			root, signature, err = v.rootSigner(s, other)
			if err != nil {
				return err
			}
			if err := VerifySignature(doc, root, signature); err != nil {
				return err
			}
		}
		out.KeyID = root.KeyID
		out.RootSequence = rotation.NewSequence
		oldSequence := s.RootSequence
		if oldSequence == 0 {
			oldSequence = 1
		}
		if oldSequence == math.MaxUint64 || rotation.RootSequence != oldSequence || rotation.NewSequence != oldSequence+1 || rotation.NewRoot.KeyID == root.KeyID {
			return ReasonSequence
		}
		for _, id := range s.RootHistory {
			if id == rotation.NewRoot.KeyID {
				return ReasonSequence
			}
		}
		var newSignature *Signature
		for i := range envelope.Signatures {
			if envelope.Signatures[i].KeyID == rotation.NewRoot.KeyID {
				newSignature = &envelope.Signatures[i]
			}
		}
		if newSignature == nil {
			return ReasonUnknownKey
		}
		if err := VerifySignature(doc, rotation.NewRoot, *newSignature); err != nil {
			return err
		}
		out.Freshness = v.freshness(rotation.IssuedAt, rotation.ExpiresAt)
		if out.Freshness != Fresh {
			return freshnessReason(out.Freshness)
		}
		initializeRoot(s, root)
		s.Root = rotation.NewRoot
		s.RootSequence = rotation.NewSequence
		s.RootHistory = append(s.RootHistory, rotation.NewRoot.KeyID)
		s.Delegation = nil
		s.DelegationDigest = ""
		return nil
	})
	if err != nil {
		return failed(out, err)
	}
	return accepted(out)
}

// VerifyDelegation installs only a fresh root-authorized delegation. Equal
// sequences are idempotent only for exactly the same signed document bytes.
func (v *Verifier) VerifyDelegation(doc, rawEnvelope []byte) Outcome {
	if v.rootError != "" {
		return refusal(v.rootError)
	}
	out := refusal(ReasonInvalidDocument)
	err := v.state.Update(func(s *StateSnapshot) error {
		if err := v.validateState(s); err != nil {
			return err
		}
		envelope, err := parseSignedEnvelope(doc, rawEnvelope, "delegation", MaxAuthorityBytes, 1)
		if err != nil {
			return err
		}
		root, signature, err := v.rootSigner(s, envelope.Signatures)
		if err != nil {
			return err
		}
		if err := VerifySignature(doc, root, signature); err != nil {
			return err
		}
		delegation, err := ParseDelegation(doc)
		if err != nil {
			return err
		}
		out.KeyID = root.KeyID
		out.RootSequence = delegation.RootSequence
		out.DelegationSequence = delegation.DelegationSequence
		rootSequence := s.RootSequence
		if rootSequence == 0 {
			rootSequence = 1
		}
		if delegation.RootSequence != rootSequence || delegation.DelegationSequence == 0 {
			return ReasonSequence
		}
		if delegation.DelegationSequence < s.DelegationSequence {
			return ReasonReplay
		}
		if delegation.DelegationSequence == s.DelegationSequence && digestBytes(doc) != s.DelegationDigest {
			return ReasonReplay
		}
		for _, key := range delegation.Keys {
			if v.isRoot(key.KeyID, s) {
				return ReasonWrongSigner
			}
		}
		out.Freshness = v.freshness(delegation.IssuedAt, delegation.ExpiresAt)
		if out.Freshness != Fresh {
			return freshnessReason(out.Freshness)
		}
		initializeRoot(s, root)
		s.DelegationSequence = delegation.DelegationSequence
		s.DelegationDigest = digestBytes(doc)
		s.Delegation = &delegation
		return nil
	})
	if err != nil {
		return failed(out, err)
	}
	return accepted(out)
}

func (v *Verifier) verifyComponentSet(s *StateSnapshot, doc, rawEnvelope []byte) (ComponentSet, Outcome) {
	out := refusal(ReasonInvalidDocument)
	if err := v.validateState(s); err != nil {
		return ComponentSet{}, failed(out, err)
	}
	envelope, err := parseSignedEnvelope(doc, rawEnvelope, "component_set", MaxComponentSetBytes, 1)
	if err != nil {
		return ComponentSet{}, failed(out, err)
	}
	signature := envelope.Signatures[0]
	if v.isRoot(signature.KeyID, s) {
		return ComponentSet{}, failed(out, ReasonWrongSigner)
	}
	if s.Delegation == nil {
		return ComponentSet{}, failed(out, ReasonUnknownKey)
	}
	for _, revoked := range s.Delegation.RevokedKeyIDs {
		if signature.KeyID == revoked {
			return ComponentSet{}, failed(out, ReasonRevokedKey)
		}
	}
	var key *PublicKey
	for i := range s.Delegation.Keys {
		if signature.KeyID == s.Delegation.Keys[i].KeyID {
			key = &s.Delegation.Keys[i]
			break
		}
	}
	if key == nil {
		return ComponentSet{}, failed(out, ReasonUnknownKey)
	}
	if err := VerifySignature(doc, *key, signature); err != nil {
		return ComponentSet{}, failed(out, err)
	}
	set, err := ParseComponentSet(doc)
	if err != nil {
		return ComponentSet{}, failed(out, err)
	}
	out.KeyID = key.KeyID
	out.RootSequence = set.RootSequence
	out.DelegationSequence = set.DelegationSequence
	out.ReleaseSequence = set.ReleaseSequence
	if set.RootSequence != s.RootSequence || set.DelegationSequence != s.DelegationSequence {
		return set, failed(out, ReasonSequence)
	}
	if v.options.ExpectedTag != "" && set.ReleaseTag != v.options.ExpectedTag {
		return set, failed(out, ReasonWrongTag)
	}
	if (v.options.OS == "") != (v.options.Arch == "") {
		return set, failed(out, ReasonWrongPlatform)
	}
	if v.options.OS != "" {
		found := false
		for _, platform := range set.Platforms {
			if platform.OS == v.options.OS && platform.Arch == v.options.Arch {
				found = true
			}
		}
		if !found {
			return set, failed(out, ReasonWrongPlatform)
		}
	}
	out.Freshness = v.freshness(set.IssuedAt, set.ExpiresAt)
	if out.Freshness != Fresh {
		return set, failed(out, freshnessReason(out.Freshness))
	}
	parentFreshness := v.freshness(s.Delegation.IssuedAt, s.Delegation.ExpiresAt)
	if parentFreshness != Fresh {
		out.Freshness = parentFreshness
		return set, failed(out, freshnessReason(parentFreshness))
	}
	// UTC second-resolution strings are already validated by the strict parser,
	// so lexical ordering is chronological. Child authority cannot outlive its parent.
	if set.IssuedAt < s.Delegation.IssuedAt || set.ExpiresAt > s.Delegation.ExpiresAt {
		return set, failed(out, ReasonInvalidDocument)
	}
	current, exists := s.Releases[set.Channel]
	if exists {
		if set.ReleaseSequence < current.Sequence {
			if !v.options.AllowRollback {
				return set, failed(out, ReasonReplay)
			}
			allowed := false
			for _, target := range current.AllowedTargets {
				if target == set.ReleaseSequence {
					allowed = true
				}
			}
			if !allowed {
				return set, failed(out, ReasonRollbackDenied)
			}
		} else if set.ReleaseSequence == current.Sequence && (set.ReleaseTag != current.Tag || digestBytes(doc) != current.Digest) {
			return set, failed(out, ReasonReplay)
		}
	}
	return set, accepted(out)
}

// VerifyComponentSet verifies acquisition eligibility without recording an
// acquisition or an installed generation.
func (v *Verifier) VerifyComponentSet(doc, rawEnvelope []byte) Outcome {
	if v.rootError != "" {
		return refusal(v.rootError)
	}
	s, err := v.state.Load()
	if err != nil {
		return failed(refusal(ReasonStateIO), err)
	}
	_, out := v.verifyComponentSet(&s, doc, rawEnvelope)
	return out
}

// AcceptForAcquisition advances only the security high-water record. It does
// not assert that payload bytes were installed or permit offline startup.
func (v *Verifier) AcceptForAcquisition(doc, rawEnvelope []byte) Outcome {
	return v.accept(doc, rawEnvelope, nil, false)
}

// AcceptSet commits one complete platform's components from this exact signed
// document. Callers must ExtractVerified and RecheckInventory before activation;
// this metadata binding does not perform extraction or execute a component.
func (v *Verifier) AcceptSet(doc, rawEnvelope []byte, components []Component) Outcome {
	return v.accept(doc, rawEnvelope, components, true)
}

func (v *Verifier) accept(doc, rawEnvelope []byte, components []Component, installed bool) Outcome {
	if v.rootError != "" {
		return refusal(v.rootError)
	}
	out := refusal(ReasonInvalidDocument)
	err := v.state.Update(func(s *StateSnapshot) error {
		var set ComponentSet
		set, out = v.verifyComponentSet(s, doc, rawEnvelope)
		if !out.Accepted {
			return out.Reason
		}
		var selectedOS, selectedArch string
		if installed {
			var ok bool
			selectedOS, selectedArch, ok = v.componentsMatch(set, components, digestBytes(doc))
			if !ok {
				return ReasonMixedSet
			}
		}
		current, exists := s.Releases[set.Channel]
		if !exists || set.ReleaseSequence > current.Sequence {
			s.Releases[set.Channel] = ReleaseRecord{Sequence: set.ReleaseSequence, Tag: set.ReleaseTag, Digest: digestBytes(doc), AllowedTargets: append([]uint64(nil), set.Rollback.AllowedTargets...)}
		}
		if installed {
			s.Installed[digestBytes(doc)] = InstalledRecord{EnvelopeDigest: digestBytes(rawEnvelope), KeyID: out.KeyID, RootSequence: set.RootSequence, DelegationSequence: set.DelegationSequence, ReleaseSequence: set.ReleaseSequence, ReleaseTag: set.ReleaseTag, OS: selectedOS, Arch: selectedArch, IssuedAt: set.IssuedAt, ExpiresAt: set.ExpiresAt}
		}
		return nil
	})
	if err != nil {
		return failed(out, err)
	}
	return accepted(out)
}

func (v *Verifier) componentsMatch(set ComponentSet, components []Component, manifestDigest string) (string, string, bool) {
	if len(components) == 0 {
		return "", "", false
	}
	for _, component := range components {
		if component.manifestDigest != manifestDigest {
			return "", "", false
		}
	}
	for _, platform := range set.Platforms {
		if v.options.OS != "" && (platform.OS != v.options.OS || platform.Arch != v.options.Arch) {
			continue
		}
		if len(platform.Components) != len(components) {
			continue
		}
		used := make([]bool, len(platform.Components))
		matched := true
		for _, component := range components {
			encoded, _ := json.Marshal(component)
			found := false
			for i, candidate := range platform.Components {
				if used[i] {
					continue
				}
				other, _ := json.Marshal(candidate)
				if bytes.Equal(encoded, other) {
					used[i] = true
					found = true
					break
				}
			}
			if !found {
				matched = false
				break
			}
		}
		if matched {
			return platform.OS, platform.Arch, true
		}
	}
	return "", "", false
}

// AcceptForOfflineStart recognizes only bytes previously committed by AcceptSet.
// Expiry, an untrusted clock, and a subsequent revocation do not erase that receipt.
func (v *Verifier) AcceptForOfflineStart(doc, rawEnvelope []byte) Outcome {
	if v.rootError != "" {
		return refusal(v.rootError)
	}
	if len(doc) > MaxComponentSetBytes {
		return refusal(ReasonDocumentTooLarge)
	}
	if len(rawEnvelope) > MaxEnvelopeBytes {
		return refusal(ReasonEnvelopeTooLarge)
	}
	s, err := v.state.Load()
	if err != nil {
		return failed(refusal(ReasonStateIO), err)
	}
	if err := v.validateState(&s); err != nil {
		return failed(refusal(ReasonStateInvalid), err)
	}
	record, ok := s.Installed[digestBytes(doc)]
	if !ok || record.EnvelopeDigest != digestBytes(rawEnvelope) {
		return refusal(ReasonNotInstalled)
	}
	if v.options.ExpectedTag != "" && v.options.ExpectedTag != record.ReleaseTag {
		return refusal(ReasonWrongTag)
	}
	if (v.options.OS == "") != (v.options.Arch == "") {
		return refusal(ReasonWrongPlatform)
	}
	if v.options.OS != "" && (v.options.OS != record.OS || v.options.Arch != record.Arch) {
		return refusal(ReasonWrongPlatform)
	}
	return accepted(Outcome{Freshness: v.freshness(record.IssuedAt, record.ExpiresAt), KeyID: record.KeyID, RootSequence: record.RootSequence, DelegationSequence: record.DelegationSequence, ReleaseSequence: record.ReleaseSequence})
}
