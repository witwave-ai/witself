package syncharness

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"slices"
)

// MovementPurpose separates projection replacement from authority and retention.
type MovementPurpose string

// The five purposes have independent preservation and authority policies.
const (
	SnapshotProjection MovementPurpose = "snapshot_projection"
	ProvenMove         MovementPurpose = "proven_move"
	BackupRestore      MovementPurpose = "backup_restore"
	SourceFinalization MovementPurpose = "source_finalization"
	AccountPurge       MovementPurpose = "account_purge"
)

// MovementPolicy is a reviewed harness policy, never production archive wiring.
type MovementPolicy struct {
	Classification string `json:"classification"`
	Action         string `json:"action"`
}

// HarnessStreamPolicy requires an explicit entry even for a zero-row stream.
type HarnessStreamPolicy struct {
	Name          string                             `json:"name"`
	Location      string                             `json:"location"`
	EmptyRequired bool                               `json:"empty_required"`
	Purposes      map[MovementPurpose]MovementPolicy `json:"purposes"`
}

// HarnessMovementRegistry inventories miniature tables and non-table streams.
type HarnessMovementRegistry struct {
	Version int                   `json:"version"`
	Scope   string                `json:"scope"`
	Streams []HarnessStreamPolicy `json:"streams"`
}

//go:embed testdata/harness_movement.v1.json testdata/failure_domains.v1.json
var movementFiles embed.FS

// MovementPurposes returns the five independently reviewed movement contracts.
func MovementPurposes() []MovementPurpose {
	return []MovementPurpose{SnapshotProjection, ProvenMove, BackupRestore, SourceFinalization, AccountPurge}
}

// EmbeddedHarnessMovement loads the explicit miniature registry only.
func EmbeddedHarnessMovement() (HarnessMovementRegistry, error) {
	var registry HarnessMovementRegistry
	data, err := movementFiles.ReadFile("testdata/harness_movement.v1.json")
	if err != nil {
		return registry, err
	}
	if decodeMovementManifest(data, &registry) != nil || registry.Validate() != nil {
		return HarnessMovementRegistry{}, ErrMovementRegistry
	}
	return registry, nil
}

// ErrMovementRegistry is value-free because rows may contain retained payloads.
var ErrMovementRegistry = errors.New("sync harness: movement registry or publication invalid")

func decodeMovementManifest(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) {
		return ErrMovementRegistry
	}
	return nil
}

// Validate requires a complete, explicit policy for every stream and purpose.
// The receiver must be the complete reviewed versioned registry returned by
// EmbeddedHarnessMovement: this method checks the entries it is given and does
// not independently establish that every required stream is present. Registry
// completeness is proven by TestMovementRegistryCompleteness against the fixture
// DDL and catalog; callers must not reconstruct or trim a registry.
func (r HarnessMovementRegistry) Validate() error {
	if r.Version != 1 || r.Scope != "harness_only" || len(r.Streams) == 0 {
		return ErrMovementRegistry
	}
	names := map[string]bool{}
	for _, stream := range r.Streams {
		if !validSymbol(stream.Name) || names[stream.Name] || !stream.EmptyRequired || !slices.Contains([]string{"node", "witness", "transport", "key_provider"}, stream.Location) || len(stream.Purposes) != 5 {
			return ErrMovementRegistry
		}
		names[stream.Name] = true
		for _, purpose := range MovementPurposes() {
			policy, ok := stream.Purposes[purpose]
			if !ok || !slices.Contains([]string{"portable", "invalidated", "cell-local"}, policy.Classification) || !slices.Contains([]string{"preserve", "rebuild", "invalidate", "refuse", "purge", "retain-external"}, policy.Action) {
				return ErrMovementRegistry
			}
			if policy.Classification == "invalidated" && policy.Action != "invalidate" {
				return ErrMovementRegistry
			}
			if policy.Classification == "portable" && !slices.Contains([]string{"preserve", "rebuild", "refuse", "purge"}, policy.Action) {
				return ErrMovementRegistry
			}
			if (stream.Location == "witness" || stream.Location == "key_provider") && (policy.Classification != "cell-local" || policy.Action != "retain-external") {
				return ErrMovementRegistry
			}
		}
	}
	return nil
}

// MovementReference retains complete account scope, even for empty targets.
type MovementReference struct{ Account, Stream, Object string }

// MovementObject carries synthetic identity and scoped dependency references.
type MovementObject struct {
	Account, ID string
	References  []MovementReference
}

// MovementStream includes an account-scoped entry even when Objects is empty.
type MovementStream struct {
	Name, Account string
	Objects       []MovementObject
}

// ValidatePublication refuses incomplete empty-stream inventories, cross-account
// references, dangling identities and copied invalidated/external authority,
// relative to the receiver registry, which must be the complete reviewed
// embedded registry (see Validate).
// It neither copies data nor activates any destination capability.
func (r HarnessMovementRegistry) ValidatePublication(account string, purpose MovementPurpose, streams []MovementStream) error {
	if r.Validate() != nil || !validSymbol(account) || !slices.Contains(MovementPurposes(), purpose) || len(streams) != len(r.Streams) {
		return ErrMovementRegistry
	}
	policies := map[string]MovementPolicy{}
	for _, stream := range r.Streams {
		policies[stream.Name] = stream.Purposes[purpose]
	}
	objects := map[string]map[string]bool{}
	for _, stream := range streams {
		policy, ok := policies[stream.Name]
		if !ok || stream.Account != account || objects[stream.Name] != nil {
			return ErrMovementRegistry
		}
		objects[stream.Name] = map[string]bool{}
		if len(stream.Objects) > 0 && (policy.Classification != "portable" || policy.Action == "refuse") {
			return ErrMovementRegistry
		}
		for _, object := range stream.Objects {
			if object.Account != account || !validSymbol(object.ID) || objects[stream.Name][object.ID] {
				return ErrMovementRegistry
			}
			objects[stream.Name][object.ID] = true
		}
	}
	for _, stream := range streams {
		for _, object := range stream.Objects {
			for _, reference := range object.References {
				if reference.Account != account || !objects[reference.Stream][reference.Object] {
					return ErrMovementRegistry
				}
			}
		}
	}
	return nil
}

// FailureDomain records physical sharing separately from simulated restore sets.
type FailureDomain struct {
	Node   NodeID `json:"node"`
	Schema string `json:"schema"`
	Host   string `json:"host"`
	Domain string `json:"domain"`
}

// FailureDomainCatalog retains the shared-instance limitation in portable data.
type FailureDomainCatalog struct {
	Version                    int             `json:"version"`
	IndependentPhysicalWitness bool            `json:"independent_physical_witness"`
	Limitation                 string          `json:"limitation"`
	Schemas                    []FailureDomain `json:"schemas"`
}

// EmbeddedFailureDomains loads the five schema-level failure-domain records.
func EmbeddedFailureDomains() (FailureDomainCatalog, error) {
	var catalog FailureDomainCatalog
	data, err := movementFiles.ReadFile("testdata/failure_domains.v1.json")
	if err != nil {
		return catalog, err
	}
	if decodeMovementManifest(data, &catalog) != nil || catalog.Validate() != nil {
		return FailureDomainCatalog{}, ErrMovementRegistry
	}
	return catalog, nil
}

// Validate rejects invented physical independence or aliased fixture schemas.
func (c FailureDomainCatalog) Validate() error {
	if c.Version != 1 || c.IndependentPhysicalWitness || c.Limitation == "" || len(c.Schemas) != 5 {
		return ErrMovementRegistry
	}
	seen := map[NodeID]bool{}
	schemas := map[string]bool{}
	for _, schema := range c.Schemas {
		if !slices.Contains([]NodeID{L1, L2, R, D, W}, schema.Node) || seen[schema.Node] || schemas[schema.Schema] || !validSymbol(schema.Schema) || schema.Host != "shared_postgres_host" || schema.Domain != "shared_postgres_instance" {
			return ErrMovementRegistry
		}
		seen[schema.Node] = true
		schemas[schema.Schema] = true
	}
	return nil
}

// SimulatedWitnessSurvives is only a model observation. Even a true result says
// nothing about physical independence: the catalog explicitly shares one host.
func (c FailureDomainCatalog) SimulatedWitnessSurvives(restored []NodeID) bool {
	if c.Validate() != nil {
		return false
	}
	for _, node := range restored {
		if !slices.Contains([]NodeID{L1, L2, R, D, W}, node) {
			return false
		}
	}
	return !slices.Contains(restored, W)
}
