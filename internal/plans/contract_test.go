package plans

import (
	"bytes"
	"maps"
	"os"
	"slices"
	"testing"
)

func TestWorkerPlanContractCurrent(t *testing.T) {
	want, err := WorkerValidationContractJSON()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../../infra/cloudflare/control-plane/src/plan-contract.json")
	if err != nil {
		t.Fatalf("read generated Worker plan contract: %v; run make plan-contract", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("Worker plan contract is stale; run make plan-contract")
	}
}

func TestWorkerPlanContractMatchesGoValidators(t *testing.T) {
	contract := WorkerValidationContract()
	if !slices.Equal(contract.FeatureKeys, SupportedFeatureKeys()) {
		t.Fatal("Worker feature keys differ from Go")
	}
	if got := slices.Sorted(maps.Keys(contract.LimitMaximums)); !slices.Equal(got, SupportedLimitKeys()) {
		t.Fatalf("Worker limit keys %v differ from Go %v", got, SupportedLimitKeys())
	}
	if got := slices.Sorted(maps.Keys(contract.PolicyBounds)); !slices.Equal(got, SupportedPolicyKeys()) {
		t.Fatalf("Worker policy keys %v differ from Go %v", got, SupportedPolicyKeys())
	}
	if err := ValidateFeatures(contract.FeatureKeys); err != nil {
		t.Fatalf("Worker features: %v", err)
	}
	for key, maximum := range contract.LimitMaximums {
		t.Run("limit/"+key, func(t *testing.T) {
			for _, value := range []int64{0, maximum} {
				if err := ValidateLimits(map[string]int64{key: value}); err != nil {
					t.Fatalf("exported boundary %d rejected: %v", value, err)
				}
			}
			for _, value := range []int64{-1, maximum + 1} {
				if err := ValidateLimits(map[string]int64{key: value}); err == nil {
					t.Fatalf("value %d outside exported bounds accepted", value)
				}
			}
		})
	}
	for key, bounds := range contract.PolicyBounds {
		t.Run("policy/"+key, func(t *testing.T) {
			for _, value := range []int64{bounds.Minimum, bounds.Maximum} {
				if err := ValidatePolicies(map[string]int64{key: value}); err != nil {
					t.Fatalf("exported boundary %d rejected: %v", value, err)
				}
			}
			for _, value := range []int64{bounds.Minimum - 1, bounds.Maximum + 1} {
				if err := ValidatePolicies(map[string]int64{key: value}); err == nil {
					t.Fatalf("value %d outside exported bounds accepted", value)
				}
			}
		})
	}
}
