package syncharness

import (
	"regexp"
	"testing"
)

func emptyMovement(r HarnessMovementRegistry) []MovementStream {
	streams := make([]MovementStream, len(r.Streams))
	for i, stream := range r.Streams {
		streams[i] = MovementStream{Name: stream.Name, Account: "account_a"}
	}
	return streams
}

func TestMovementRegistryCompleteness(t *testing.T) {
	registry, err := EmbeddedHarnessMovement()
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]bool{}
	for _, match := range regexp.MustCompile(`CREATE TABLE (h_[a-z_]+)`).FindAllStringSubmatch(repositoryFile(t, "internal/store/testdata/sync_harness/schema_v1.sql"), -1) {
		expected[match[1]] = true
	}
	// A–C's minimal witness table is installed by the test-only topology, not
	// by the node schema. Non-table streams still require a movement policy.
	for _, name := range []string{"h_witness_prefixes", "stream_sessions", "stream_transport_capabilities", "stream_comparison_keys", "stream_executable_grants"} {
		expected[name] = true
	}
	actual := map[string]bool{}
	for _, stream := range registry.Streams {
		actual[stream.Name] = true
	}
	assertSameInventory(t, "harness movement", expected, actual)
	for _, purpose := range MovementPurposes() {
		streams := emptyMovement(registry)
		if registry.ValidatePublication("account_a", purpose, streams) != nil {
			t.Fatal("complete empty-stream inventory refused")
		}
		if registry.ValidatePublication("account_a", purpose, streams[1:]) == nil {
			t.Fatal("missing empty stream authorized publication")
		}
	}
}

func TestMovementRefusesCrossAccountReferencesAndAuthorityCopy(t *testing.T) {
	registry, err := EmbeddedHarnessMovement()
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"scope", "reference", "dangling", "invalidated", "external", "cell_local", "executable_grant"} {
		t.Run(mutation, func(t *testing.T) {
			streams := emptyMovement(registry)
			index := map[string]int{}
			for i, s := range streams {
				index[s.Name] = i
			}
			streams[index["h_fixture_objects"]].Objects = []MovementObject{{Account: "account_a", ID: "object_a"}}
			switch mutation {
			case "scope":
				streams[index["h_fixture_objects"]].Objects[0].Account = "account_b"
			case "reference":
				streams[index["h_fixture_objects"]].Objects[0].References = []MovementReference{{Account: "account_b", Stream: "h_fixture_objects", Object: "object_a"}}
			case "dangling":
				streams[index["h_fixture_objects"]].Objects[0].References = []MovementReference{{Account: "account_a", Stream: "h_fixture_objects", Object: "missing"}}
			case "invalidated":
				streams[index["h_replica_cursors"]].Objects = []MovementObject{{Account: "account_a", ID: "cursor_a"}}
			case "external":
				streams[index["h_restore_intents"]].Objects = []MovementObject{{Account: "account_a", ID: "restore_a"}}
			case "cell_local":
				streams[index["h_recorded_traces"]].Objects = []MovementObject{{Account: "account_a", ID: "trace_a"}}
			case "executable_grant":
				streams[index["stream_executable_grants"]].Objects = []MovementObject{{Account: "account_a", ID: "grant_a"}}
			}
			if registry.ValidatePublication("account_a", ProvenMove, streams) == nil {
				t.Fatal("unsafe movement publication accepted")
			}
		})
	}
	streams := emptyMovement(registry)
	for i := range streams {
		if streams[i].Name == "h_fixture_objects" {
			streams[i].Objects = []MovementObject{{Account: "account_a", ID: "object_a", References: []MovementReference{{Account: "account_a", Stream: "h_fixture_objects", Object: "object_a"}}}}
		}
	}
	if registry.ValidatePublication("account_a", ProvenMove, streams) != nil {
		t.Fatal("valid scoped reference refused")
	}
}

func TestMovementRegistryRejectsMissingPurposeAndExternalReclassification(t *testing.T) {
	for _, mutation := range []string{"missing_purpose", "duplicate_stream", "empty_optional", "external_portable", "invalidated_preserve", "portable_external"} {
		t.Run(mutation, func(t *testing.T) {
			registry, err := EmbeddedHarnessMovement()
			if err != nil {
				t.Fatal(err)
			}
			first := &registry.Streams[0]
			switch mutation {
			case "missing_purpose":
				delete(first.Purposes, BackupRestore)
			case "duplicate_stream":
				registry.Streams[1].Name = first.Name
			case "empty_optional":
				first.EmptyRequired = false
			case "external_portable":
				for i := range registry.Streams {
					if registry.Streams[i].Location == "witness" {
						registry.Streams[i].Purposes[ProvenMove] = MovementPolicy{"portable", "preserve"}
						break
					}
				}
			case "invalidated_preserve":
				first.Purposes[ProvenMove] = MovementPolicy{"invalidated", "preserve"}
			case "portable_external":
				first.Purposes[ProvenMove] = MovementPolicy{"portable", "retain-external"}
			}
			if registry.Validate() == nil {
				t.Fatal("incomplete or unsafe movement policy accepted")
			}
		})
	}
}

func TestDatabaseVsWholeDiskRestoreDomain(t *testing.T) {
	catalog, err := EmbeddedFailureDomains()
	if err != nil {
		t.Fatal(err)
	}
	if !catalog.SimulatedWitnessSurvives([]NodeID{R, D}) || catalog.SimulatedWitnessSurvives([]NodeID{L1, L2, R, D, W}) || catalog.IndependentPhysicalWitness {
		t.Fatal("modeled witness promoted to independent physical durability")
	}
	t.Log("MODEL_PASS simulated restore sets; NOT_RUN native_restore,independent_physical_witness")
}

func TestFailureDomainCatalogRefusesInventedIndependence(t *testing.T) {
	for _, mutation := range []string{"physical", "host", "duplicate_schema", "missing_witness"} {
		catalog, err := EmbeddedFailureDomains()
		if err != nil {
			t.Fatal(err)
		}
		switch mutation {
		case "physical":
			catalog.IndependentPhysicalWitness = true
		case "host":
			catalog.Schemas[4].Host = "independent_host"
		case "duplicate_schema":
			catalog.Schemas[4].Schema = catalog.Schemas[0].Schema
		case "missing_witness":
			catalog.Schemas = catalog.Schemas[:4]
		}
		if catalog.Validate() == nil || catalog.SimulatedWitnessSurvives([]NodeID{R}) {
			t.Fatal("invalid catalog certified a surviving witness")
		}
	}
}
