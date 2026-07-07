package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRemainingPulumiResources(t *testing.T) {
	t.Run("empty deployment is empty", func(t *testing.T) {
		resources, pending, err := remainingPulumiResources(nil)
		if err != nil {
			t.Fatalf("remainingPulumiResources: %v", err)
		}
		if len(resources) != 0 || len(pending) != 0 {
			t.Fatalf("resources=%v pending=%v, want empty", resources, pending)
		}
	})

	t.Run("decodes resources and pending operations", func(t *testing.T) {
		raw := json.RawMessage(`{
		  "resources": [
		    {"urn": "urn:pulumi:dev::witself-infra::gcp:container/cluster:Cluster::cell", "type": "gcp:container/cluster:Cluster"},
		    {"type": "gcp:sql/databaseInstance:DatabaseInstance"}
		  ],
		  "pending_operations": [
		    {"operation": "deleting", "resource": {"urn": "urn:pulumi:dev::witself-infra::gcp:compute/network:Network::cell"}}
		  ]
		}`)
		resources, pending, err := remainingPulumiResources(raw)
		if err != nil {
			t.Fatalf("remainingPulumiResources: %v", err)
		}
		if len(resources) != 2 {
			t.Fatalf("resources = %d, want 2", len(resources))
		}
		if len(pending) != 1 {
			t.Fatalf("pending = %d, want 1", len(pending))
		}
		if !strings.Contains(summarizeDeploymentResources(resources, 1), "...and 1 more") {
			t.Fatal("resource summary should mention truncated count")
		}
		if !strings.Contains(summarizePendingOperations(pending, 5), "compute/network") {
			t.Fatal("pending summary should include nested resource URN")
		}
	})

	t.Run("invalid JSON reports decode failure", func(t *testing.T) {
		_, _, err := remainingPulumiResources(json.RawMessage(`{`))
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "decode exported deployment") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestShouldClearPendingCreates(t *testing.T) {
	tests := []struct {
		name      string
		resources []deploymentResource
		pending   []deploymentOperation
		want      bool
	}{
		{
			name: "clears pending creates when state has no resources",
			pending: []deploymentOperation{
				{Operation: "creating", Resource: deploymentResource{URN: "urn:pulumi:dev::witself-infra::azure-native:containerservice:ManagedCluster::cell"}},
			},
			want: true,
		},
		{
			name: "also supports legacy type field",
			pending: []deploymentOperation{
				{Type: "creating", Resource: deploymentResource{URN: "urn:pulumi:dev::witself-infra::aws:eks/cluster:Cluster::cell"}},
			},
			want: true,
		},
		{
			name: "does not clear when resources remain",
			resources: []deploymentResource{
				{URN: "urn:pulumi:dev::witself-infra::gcp:compute/network:Network::cell"},
			},
			pending: []deploymentOperation{
				{Operation: "creating", Resource: deploymentResource{URN: "urn:pulumi:dev::witself-infra::gcp:container/cluster:Cluster::cell"}},
			},
			want: false,
		},
		{
			name: "does not clear pending deletes",
			pending: []deploymentOperation{
				{Operation: "deleting", Resource: deploymentResource{URN: "urn:pulumi:dev::witself-infra::gcp:compute/network:Network::cell"}},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldClearPendingCreates(tt.resources, tt.pending); got != tt.want {
				t.Fatalf("shouldClearPendingCreates() = %v, want %v", got, tt.want)
			}
		})
	}
}
