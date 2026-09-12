package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optpreview"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
)

type protectionStack interface {
	Preview(context.Context, ...optpreview.Option) (auto.PreviewResult, error)
	Up(context.Context, ...optup.Option) (auto.UpResult, error)
}

// A newly enabled Protect option alone does not stop replacement of an old,
// unprotected resource. Review the provider's actual plan before any up, and
// constrain up to that saved plan. Never infer replacement safety from mocks.
func upWithDeletionProtection(ctx context.Context, stack protectionStack, cellName string, enabled bool, out io.Writer) (result auto.UpResult, err error) {
	dir, err := os.MkdirTemp("", "witself-infra-protection-plan-")
	if err != nil {
		return auto.UpResult{}, fmt.Errorf("create private protection plan directory: %w", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(dir); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove private protection plan directory: %w", cleanupErr))
		}
	}()
	plan := filepath.Join(dir, "plan.json")
	if _, err := previewWithDeletionProtection(ctx, stack, cellName, enabled,
		optpreview.ProgressStreams(out), optpreview.Plan(plan)); err != nil {
		return auto.UpResult{}, err
	}
	return stack.Up(ctx, optup.ProgressStreams(out), optup.Plan(plan))
}

func previewWithDeletionProtection(ctx context.Context, stack protectionStack, cellName string, enabled bool, opts ...optpreview.Option) (auto.PreviewResult, error) {
	stream := make(chan events.EngineEvent)
	done := make(chan struct{})
	reviewed := make(chan error, 1)
	go func() {
		violations := map[string]bool{}
		var streamErr error
		for {
			select {
			case event, ok := <-stream:
				if !ok {
					reviewed <- errors.Join(streamErr, protectionPlanError(cellName, violations))
					return
				}
				if event.Error != nil {
					streamErr = fmt.Errorf("cannot review deletion protection plan: %w", event.Error)
				}
				if event.ResourcePreEvent != nil {
					step := event.ResourcePreEvent.Metadata
					blocked, err := protectedStoreDestruction(step, enabled)
					if err != nil {
						streamErr = errors.Join(streamErr, fmt.Errorf("cannot review deletion protection plan: %w", err))
					}
					if blocked {
						name := step.URN
						if name == "" {
							name = step.Type
						}
						violations[name] = true
					}
				}
			case <-done:
				// Automation API finishes sending events before Preview returns.
				// The done path also handles failures before it opened the stream.
				reviewed <- errors.Join(streamErr, protectionPlanError(cellName, violations))
				return
			}
		}
	}()
	opts = append(opts, optpreview.EventStreams(stream))
	result, err := stack.Preview(ctx, opts...)
	close(done)
	return result, errors.Join(<-reviewed, err)
}

func protectedStoreResource(resourceType string) bool {
	switch resourceType {
	case "aws:rds/instance:Instance", "aws:secretsmanager/secret:Secret",
		"gcp:sql/databaseInstance:DatabaseInstance", "gcp:sql/database:Database",
		"gcp:secretmanager/secret:Secret", "azure-native:dbforpostgresql:Server",
		"azure-native:dbforpostgresql:Database", "azure-native:keyvault:Vault",
		"azure-native:keyvault:Secret":
		return true
	default:
		// Secret versions are deliberately excluded: normal credential rotation
		// creates a version, without replacing its protected parent store.
		return false
	}
}

func protectedStoreDestruction(step apitype.StepEventMetadata, enabled bool) (bool, error) {
	if !protectedStoreResource(step.Type) {
		return false, nil
	}
	switch step.Op {
	case apitype.OpDelete, apitype.OpReplace, apitype.OpCreateReplacement,
		apitype.OpDeleteReplaced, apitype.OpReadReplacement, apitype.OpDiscardReplaced,
		apitype.OpImportReplacement:
		if enabled || (step.Old != nil && step.Old.Protect) {
			return true, nil
		}
		if step.Old != nil {
			// Native protection can predate this CLI's Pulumi Protect option.
			// Removing that protection and replacing the store in one update
			// also requires the separate, durably applied unprotect step.
			return nativeStoreProtection(step.Type, step.Old.Inputs, step.Old.Outputs)
		}
	}
	return false, nil
}

func protectionPlanError(cellName string, violations map[string]bool) error {
	if len(violations) == 0 {
		return nil
	}
	resources := make([]string, 0, len(violations))
	for urn := range violations {
		resources = append(resources, urn)
	}
	sort.Strings(resources)
	return fmt.Errorf("deletion protection refuses a plan that would delete or force replacement of database/secret-store resources: %s; protection must be enabled in place, never by replacing an existing store. For intentional replacement, edit cell %q in the inventory to deletion_protection: false, then run a separate up that applies ONLY unprotection before the replacement or destroy. If the provider requires replacement to enable protection, use its supported in-place protection operation and refresh first, or arrange an explicitly reviewed migration to a new store", strings.Join(resources, ", "), cellName)
}
