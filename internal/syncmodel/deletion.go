package syncmodel

import (
	"errors"
	"time"
)

// DefaultOfflineGrant and DeletionSafetyMargin are the O-8 fixture horizon.
const (
	DefaultOfflineGrant  = 30 * 24 * time.Hour
	DeletionSafetyMargin = 7 * 24 * time.Hour
)

// DeletionKnowledge preserves payload-free generation identity and proof
// references after payload retirement. It contains no payload or unkeyed hash.
type DeletionKnowledge struct {
	AccountID          string
	Family             string
	ObjectID           string
	Generation         GenerationID
	DeletionGeneration GenerationID
	AcceptedDelete     OperationID
	IssuerStream       string
	Frontier           Ordinal
	EvidenceRef        string
	DeletedAt          time.Time
	RetainUntil        time.Time
	UnresolvedPins     uint64
}

// Validate requires the configured longest grant plus seven days of coverage.
func (d DeletionKnowledge) Validate(longestOfflineGrant time.Duration) error {
	if longestOfflineGrant < 0 || !validLabel(d.AccountID) || !validFamily(d.Family) || !validLabel(d.ObjectID) || !validLabel(d.IssuerStream) || !validLabel(d.EvidenceRef) || d.DeletedAt.IsZero() || d.Frontier == 0 || d.Frontier.Validate() != nil {
		return errors.New("invalid deletion knowledge")
	}
	if longestOfflineGrant > time.Duration(1<<63-1)-DeletionSafetyMargin || d.RetainUntil.Before(d.DeletedAt.Add(longestOfflineGrant+DeletionSafetyMargin)) {
		return errors.New("deletion retention below grant floor")
	}
	return nil
}

// CanCollect checks time and unresolved references independently. Expiry never
// implies nonacceptance or authorizes resurrection.
func (d DeletionKnowledge) CanCollect(now time.Time) bool {
	return d.Validate(DefaultOfflineGrant) == nil && d.UnresolvedPins == 0 && !now.Before(d.RetainUntil)
}

// RetentionFloor is the later of settlement+37d and data retirement+37d.
func RetentionFloor(settledAt, retiredAt time.Time) time.Time {
	base := settledAt
	if retiredAt.After(base) {
		base = retiredAt
	}
	return base.Add(DefaultOfflineGrant + DeletionSafetyMargin)
}

type evidenceIdentity struct{ account, kind, id string }

// EvidencePins tracks unresolved identity, evidence and key dependencies apart
// from payload lifetime. Releasing one owner never releases another owner.
type EvidencePins struct {
	pins map[evidenceIdentity]map[string]bool
}

// Pin preserves one referenced item for an unresolved operation or recovery.
func (p *EvidencePins) Pin(account, kind, id, owner string) error {
	if !validLabel(account) || !validLabel(id) || !validLabel(owner) {
		return errors.New("invalid evidence pin")
	}
	switch kind {
	case "identity", "evidence", "comparison_key", "signing_key", "deletion":
	default:
		return errors.New("invalid evidence kind")
	}
	if p.pins == nil {
		p.pins = make(map[evidenceIdentity]map[string]bool)
	}
	k := evidenceIdentity{account, kind, id}
	if p.pins[k] == nil {
		p.pins[k] = make(map[string]bool)
	}
	p.pins[k][owner] = true
	return nil
}

// Release removes only the named resolved reference.
func (p *EvidencePins) Release(account, kind, id, owner string) {
	delete(p.pins[evidenceIdentity{account, kind, id}], owner)
}

// CanCollect requires both the retention floor and absence of every owner pin.
func (p *EvidencePins) CanCollect(account, kind, id string, now, retainUntil time.Time) bool {
	return !retainUntil.IsZero() && !now.Before(retainUntil) && len(p.pins[evidenceIdentity{account, kind, id}]) == 0
}
