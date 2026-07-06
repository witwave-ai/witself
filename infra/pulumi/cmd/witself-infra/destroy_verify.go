package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
)

type deploymentResource struct {
	URN  string `json:"urn"`
	Type string `json:"type"`
}

type deploymentOperation struct {
	Type     string             `json:"type"`
	URN      string             `json:"urn"`
	Resource deploymentResource `json:"resource"`
}

type deploymentSnapshot struct {
	Resources         []deploymentResource  `json:"resources"`
	PendingOperations []deploymentOperation `json:"pending_operations"`
}

func verifyPulumiDestroyEmpty(ctx context.Context, stack auto.Stack) error {
	dep, err := stack.Export(ctx)
	if err != nil {
		return fmt.Errorf("verify Pulumi destroy state: %w", err)
	}
	resources, pending, err := remainingPulumiResources(dep.Deployment)
	if err != nil {
		return fmt.Errorf("verify Pulumi destroy state: %w", err)
	}
	if len(pending) > 0 {
		return fmt.Errorf("Pulumi destroy left %d pending operation(s): %s", len(pending), summarizePendingOperations(pending, 5))
	}
	if len(resources) > 0 {
		return fmt.Errorf("Pulumi destroy left %d resource(s) in state: %s", len(resources), summarizeDeploymentResources(resources, 5))
	}
	fmt.Fprintln(os.Stderr, "Pulumi state verified empty after destroy")
	return nil
}

func remainingPulumiResources(raw json.RawMessage) ([]deploymentResource, []deploymentOperation, error) {
	if len(raw) == 0 {
		return nil, nil, nil
	}
	var snap deploymentSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, nil, fmt.Errorf("decode exported deployment: %w", err)
	}
	return snap.Resources, snap.PendingOperations, nil
}

func summarizeDeploymentResources(resources []deploymentResource, limit int) string {
	if len(resources) == 0 {
		return ""
	}
	if limit <= 0 || limit > len(resources) {
		limit = len(resources)
	}
	parts := make([]string, 0, limit+1)
	for _, r := range resources[:limit] {
		label := r.URN
		if label == "" {
			label = r.Type
		}
		if label == "" {
			label = "<unknown>"
		}
		parts = append(parts, label)
	}
	if len(resources) > limit {
		parts = append(parts, fmt.Sprintf("...and %d more", len(resources)-limit))
	}
	return strings.Join(parts, ", ")
}

func summarizePendingOperations(ops []deploymentOperation, limit int) string {
	if len(ops) == 0 {
		return ""
	}
	if limit <= 0 || limit > len(ops) {
		limit = len(ops)
	}
	parts := make([]string, 0, limit+1)
	for _, op := range ops[:limit] {
		label := op.URN
		if label == "" {
			label = op.Resource.URN
		}
		if label == "" {
			label = op.Type
		}
		if label == "" {
			label = "<unknown>"
		}
		parts = append(parts, label)
	}
	if len(ops) > limit {
		parts = append(parts, fmt.Sprintf("...and %d more", len(ops)-limit))
	}
	return strings.Join(parts, ", ")
}
