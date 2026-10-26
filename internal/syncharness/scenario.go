package syncharness

import (
	"errors"
	"regexp"
	"slices"
	"time"
)

// NodeID names one synthetic topology participant.
type NodeID string

// The topology has four stores, one witness, and an ordinary remote client.
const (
	L1 NodeID = "L1"
	L2 NodeID = "L2"
	R  NodeID = "R"
	D  NodeID = "D"
	W  NodeID = "W"
	C  NodeID = "C"
)

// EvidenceStatus distinguishes observed behavior from unimplemented obligations.
type EvidenceStatus string

// Evidence classes must remain explicit in catalog and runner results.
const (
	ModelPass    EvidenceStatus = "MODEL_PASS"
	PostgresPass EvidenceStatus = "POSTGRES_PASS"
	ScaffoldOnly EvidenceStatus = "SCAFFOLD_ONLY"
	NotRun       EvidenceStatus = "NOT_RUN"
	Fail         EvidenceStatus = "FAIL"
)

// Result keeps missing capabilities visible. Model and database observations are
// separate evidence classes; neither alone certifies full synchronization.
type Result struct {
	Obligation string         `json:"obligation"`
	Status     EvidenceStatus `json:"status"`
	Missing    []string       `json:"missing_capabilities,omitempty"`
}

// Passed reports an executed observation, never a missing capability or skip.
func (r Result) Passed() bool { return r.Status == ModelPass || r.Status == PostgresPass }

// RequireCapabilities never manufactures a successful test result. When all
// capabilities exist the caller still has to execute the obligation.
func RequireCapabilities(obligation string, required, available []string) Result {
	result := Result{Obligation: obligation, Status: NotRun}
	for _, capability := range required {
		if !slices.Contains(available, capability) && !slices.Contains(result.Missing, capability) {
			result.Missing = append(result.Missing, capability)
		}
	}
	slices.Sort(result.Missing)
	if len(result.Missing) != 0 {
		result.Status = ScaffoldOnly
	}
	return result
}

// ScenarioVersion fixes the symbolic scenario record format.
const ScenarioVersion = 1

// MaxSeed keeps recorded decimal seeds exact in JSON consumers.
const MaxSeed uint64 = 9007199254740991

// MaxSteps bounds one synthetic scheduler run.
const MaxSteps = 10000

// Actor identity bytes are supplied synthetic test input, not a production
// identity constructor. Only Name is retained in the value-free trace.
type Actor struct {
	Name     string
	Identity []byte
}

// Scenario declares synthetic identities, inputs, and the invariant being checked.
type Scenario struct {
	Version           int
	ID                string
	Seed              uint64
	ClockStart        time.Time
	Inputs            []string
	Actors            []Actor
	Capabilities      []string
	ExpectedInvariant string
}

var symbolic = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:/-]{0,95}$`)
var longHex = regexp.MustCompile(`[A-Fa-f0-9]{64}`)

func validSymbol(value string) bool {
	return symbolic.MatchString(value) && !longHex.MatchString(value)
}

func validNode(node NodeID) bool { return slices.Contains([]NodeID{L1, L2, R, D, W, C}, node) }

// ErrInvalidScenario refuses unknown versions, nonsymbolic inputs, or invalid actors.
var ErrInvalidScenario = errors.New("sync harness: invalid synthetic scenario")

func (s Scenario) validate() error {
	if s.Version != ScenarioVersion || s.Seed > MaxSeed || s.ClockStart.IsZero() || !validSymbol(s.ID) || !validSymbol(s.ExpectedInvariant) || len(s.Actors) == 0 {
		return ErrInvalidScenario
	}
	seen := make(map[string]bool)
	for _, actor := range s.Actors {
		if !validSymbol(actor.Name) || seen[actor.Name] || len(actor.Identity) == 0 || len(actor.Identity) > 64 {
			return ErrInvalidScenario
		}
		seen[actor.Name] = true
	}
	for _, values := range [][]string{s.Inputs, s.Capabilities} {
		seen = make(map[string]bool)
		for _, value := range values {
			if !validSymbol(value) || seen[value] {
				return ErrInvalidScenario
			}
			seen[value] = true
		}
	}
	return nil
}

var (
	// ErrCapabilityMissing is a harness capability error, not a domain refusal.
	ErrCapabilityMissing = errors.New("sync harness: transport adapter missing")
	// ErrPartitioned means the selected directed edge is disabled.
	ErrPartitioned = errors.New("sync harness: directed transport edge disabled")
	// ErrInvalidEdge rejects unknown nodes, self edges, and nonsymbolic operations.
	ErrInvalidEdge = errors.New("sync harness: invalid transport edge")
)

// Delivery identifies symbolic work without carrying domain payloads.
type Delivery struct {
	From      NodeID
	To        NodeID
	Operation string
}

// Adapter is a test seam only. No implementation is supplied by this package.
type Adapter interface{ Deliver(Delivery) error }

type edge struct{ from, to NodeID }

// Transport exposes only directed edge control and an optional test adapter.
type Transport struct {
	nodes   []NodeID
	edges   map[edge]bool
	adapter Adapter
}

// NewTransport starts with every edge disabled and no adapter.
func NewTransport(nodes ...NodeID) *Transport {
	return &Transport{nodes: slices.Clone(nodes), edges: make(map[edge]bool)}
}

// HasAdapter reports whether a test explicitly supplied an adapter.
func (t *Transport) HasAdapter() bool { return t.adapter != nil }

// EdgeEnabled reports a single directed edge; unknown edges are disabled.
func (t *Transport) EdgeEnabled(from, to NodeID) bool { return t.edges[edge{from, to}] }

// InstallAdapter explicitly supplies or removes a test-only adapter.
func (t *Transport) InstallAdapter(adapter Adapter) { t.adapter = adapter }

// SetEdge changes exactly one directed edge without deleting model work.
func (t *Transport) SetEdge(from, to NodeID, enabled bool) error {
	if from == to || !validNode(from) || !validNode(to) || !slices.Contains(t.nodes, from) || !slices.Contains(t.nodes, to) {
		return ErrInvalidEdge
	}
	t.edges[edge{from, to}] = enabled
	return nil
}

// Deliver checks capability and directed connectivity before invoking the adapter.
func (t *Transport) Deliver(delivery Delivery) error {
	if t.adapter == nil {
		return ErrCapabilityMissing
	}
	if delivery.From == delivery.To || !slices.Contains(t.nodes, delivery.From) || !slices.Contains(t.nodes, delivery.To) || !validSymbol(delivery.Operation) {
		return ErrInvalidEdge
	}
	if !t.EdgeEnabled(delivery.From, delivery.To) {
		return ErrPartitioned
	}
	return t.adapter.Deliver(delivery)
}
