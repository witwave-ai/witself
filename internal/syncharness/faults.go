package syncharness

import (
	"errors"
	"slices"
)

// Checkpoint is a named boundary, never a wall-clock sleep.
type Checkpoint string

// Checkpoints cover the design's admission, capture, graph, and snapshot boundaries.
const (
	BeforeAdmission           Checkpoint = "before_admission"
	AfterAdmission            Checkpoint = "after_admission"
	DomainWrite               Checkpoint = "domain_write"
	JournalSeal               Checkpoint = "journal_seal"
	CaptureCommit             Checkpoint = "capture_commit"
	FragmentPersistence       Checkpoint = "fragment_persistence"
	InboundMutationCommit     Checkpoint = "inbound_mutation_commit"
	ReceiptCommit             Checkpoint = "receipt_commit"
	PublicationOutbox         Checkpoint = "publication_outbox"
	WitnessCAS                Checkpoint = "witness_cas"
	ReturnedACK               Checkpoint = "returned_ack"
	SourceTerminalPersistence Checkpoint = "source_terminal_state_persistence"
	GraphFence                Checkpoint = "graph_fence"
	GraphInstall              Checkpoint = "graph_install"
	GraphPublish              Checkpoint = "graph_publish"
	GraphAbort                Checkpoint = "graph_abort"
	SnapshotGate              Checkpoint = "snapshot_gate"
	SnapshotPin               Checkpoint = "snapshot_pin"
	SnapshotAcknowledgment    Checkpoint = "snapshot_acknowledgment"
)

// Checkpoints returns a fresh ordered inventory for validation and manifests.
func Checkpoints() []Checkpoint {
	return []Checkpoint{BeforeAdmission, AfterAdmission, DomainWrite, JournalSeal, CaptureCommit, FragmentPersistence, InboundMutationCommit, ReceiptCommit, PublicationOutbox, WitnessCAS, ReturnedACK, SourceTerminalPersistence, GraphFence, GraphInstall, GraphPublish, GraphAbort, SnapshotGate, SnapshotPin, SnapshotAcknowledgment}
}

// FaultClass names a modeled failure obligation.
type FaultClass string

// Fault classes retain the seven distinct injection/observable categories.
const (
	Partition                  FaultClass = "partition"
	DuplicateReorderedDelivery FaultClass = "duplicate_reordered_delivery"
	CrashBetweenWrites         FaultClass = "crash_between_writes"
	DelayedLostACK             FaultClass = "delayed_lost_ack"
	ExpiredHistory             FaultClass = "expired_history"
	IncompatibleVersions       FaultClass = "incompatible_versions"
	WitnessRestore             FaultClass = "witness_restore"
)

// FaultContract is the observable obligation, not a fault implementation or a
// claim that a production adapter satisfies it.
func FaultContract(class FaultClass) (string, bool) {
	switch class {
	case Partition:
		return "Independent local work proceeds; remote ownership and transfer remain pending", true
	case DuplicateReorderedDelivery:
		return "Stable operation/fragment identity; no duplicate logical action or partial graph", true
	case CrashBetweenWrites:
		return "Before commit: none of the atomic group; after commit: complete group and original outcome", true
	case DelayedLostACK:
		return "Retry original operation; no fresh ID or repeat external effect", true
	case ExpiredHistory:
		return "Unresolved work remains pinned; missing acceptance/deletion/frontier evidence blocks convergence", true
	case IncompatibleVersions:
		return "Explicit refusal or pending compatibility recovery; no implicit acceptance", true
	case WitnessRestore:
		return "Distinct publication-pending, proven-gap, evidence-conflict and continuity-unproven results", true
	default:
		return "", false
	}
}

// FaultClasses returns all seven classes in design order.
func FaultClasses() []FaultClass {
	return []FaultClass{Partition, DuplicateReorderedDelivery, CrashBetweenWrites, DelayedLostACK, ExpiredHistory, IncompatibleVersions, WitnessRestore}
}

// Fault selects an exact node/channel/operation/checkpoint and its observable.
type Fault struct {
	Class      FaultClass `json:"class"`
	Node       NodeID     `json:"node"`
	Channel    string     `json:"channel"`
	Operation  string     `json:"operation"`
	Checkpoint Checkpoint `json:"checkpoint"`
	Observable string     `json:"observable"`
}

// FaultPlan preserves fault choice order in a replay record.
type FaultPlan []Fault

// ErrInvalidFault refuses unknown, duplicated, or incorrectly specified faults.
var ErrInvalidFault = errors.New("sync harness: invalid fault plan")

// Validate requires exact selectors and the canonical observable for every fault.
func (plan FaultPlan) Validate() error {
	seen := make(map[Fault]bool)
	for _, fault := range plan {
		contract, ok := FaultContract(fault.Class)
		if !ok || !validNode(fault.Node) || !validSymbol(fault.Channel) || !validSymbol(fault.Operation) || !slices.Contains(Checkpoints(), fault.Checkpoint) || fault.Observable != contract || seen[fault] {
			return ErrInvalidFault
		}
		seen[fault] = true
	}
	return nil
}

func (plan FaultPlan) matching(event Event) []Fault {
	var result []Fault
	for _, fault := range plan {
		if fault.Node == event.Node && fault.Channel == event.Channel && fault.Operation == event.Operation && fault.Checkpoint == event.Checkpoint {
			result = append(result, fault)
		}
	}
	return result
}
