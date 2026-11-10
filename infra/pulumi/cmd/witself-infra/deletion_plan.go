package main

import (
	"context"
	"encoding/json"
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
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/urn"
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

func previewWithDeletionProtection(ctx context.Context, stack protectionStack, cellName string, enabled bool, opts ...optpreview.Option) (result auto.PreviewResult, err error) {
	options := &optpreview.Options{}
	for _, opt := range opts {
		opt.ApplyOption(options)
	}
	if options.Plan == "" {
		dir, createErr := os.MkdirTemp("", "witself-infra-protection-plan-")
		if createErr != nil {
			return result, fmt.Errorf("create private protection plan directory: %w", createErr)
		}
		defer func() {
			if cleanupErr := os.RemoveAll(dir); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("remove private protection plan directory: %w", cleanupErr))
			}
		}()
		options.Plan = filepath.Join(dir, "plan.json")
		opts = append(opts, optpreview.Plan(options.Plan))
	}

	stream := make(chan events.EngineEvent)
	done := make(chan struct{})
	reviewed := make(chan error, 1)
	// Written by the stream reviewer and read only after receiving its result.
	resourceSteps := false
	oldProtectedURNs := map[string]bool{}
	go func() {
		violations := map[string]bool{}
		civoViolations := map[string]bool{}
		var streamErr error
		defer func() {
			reviewed <- errors.Join(streamErr, protectionPlanError(cellName, violations), civoProtectionPlanError(cellName, civoViolations))
		}()
		reviewStep := func(step apitype.StepEventMetadata, preEvent bool) {
			resourceSteps = true
			identity := resolveCivoPlanIdentity(step)
			oldProtected := step.Old != nil && step.Old.Protect
			if identity.protected && oldProtected {
				oldProtectedURNs[identity.urn] = true
			}
			blocked, identityErr := reviewCivoPlanStep(identity, step.Op, enabled || oldProtected)
			streamErr = errors.Join(streamErr, identityErr)
			if blocked {
				civoViolations[identity.urn] = true
			}
			// Preserve the existing store predicate and its pre-event boundary.
			if preEvent {
				blocked, storeErr := protectedStoreDestruction(step, enabled)
				if storeErr != nil {
					streamErr = errors.Join(streamErr, fmt.Errorf("cannot review deletion protection plan: %w", storeErr))
				}
				if blocked {
					name := step.URN
					if name == "" {
						name = step.Type
					}
					violations[name] = true
				}
			}
		}
		for {
			select {
			case event, ok := <-stream:
				if !ok {
					return
				}
				if event.Error != nil {
					streamErr = errors.Join(streamErr, fmt.Errorf("cannot review deletion protection plan: %w", event.Error))
				}
				if event.ResourcePreEvent != nil {
					reviewStep(event.ResourcePreEvent.Metadata, true)
				}
				if event.ResOutputsEvent != nil {
					reviewStep(event.ResOutputsEvent.Metadata, false)
				}
				if event.SummaryEvent != nil {
					for _, count := range event.SummaryEvent.ResourceChanges {
						resourceSteps = resourceSteps || count > 0
					}
				}
			case <-done:
				// Automation API finishes sending events before Preview returns.
				// The done path also handles failures before it opened the stream.
				return
			}
		}
	}()

	opts = append(opts, optpreview.EventStreams(stream))
	result, err = stack.Preview(ctx, opts...)
	close(done)
	streamErr := <-reviewed
	planErr := reviewCivoSavedPlan(options.Plan, cellName, enabled, resourceSteps, oldProtectedURNs)
	return result, errors.Join(streamErr, planErr, err)
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

const (
	civoClusterType       = "civo:index/kubernetesCluster:KubernetesCluster"
	workloadNamespaceType = "kubernetes:core/v1:Namespace"
)

// Keep Civo plan classification separate from protectedStoreResource: the latter
// also defines the persisted-unprotection check for destroy.
type civoPlanIdentity struct {
	typ, urn   string
	protected  bool
	unresolved bool
	err        error
}

// Resolve every available identity source once. An inconsistent source must not
// hide a protected resource behind an unrelated top-level type or URN.
func resolveCivoPlanIdentity(step apitype.StepEventMetadata) civoPlanIdentity {
	var identity civoPlanIdentity
	var cluster, namespace, workloadName bool
	addType := func(typ string) {
		if typ == "" {
			return
		}
		cluster = cluster || typ == civoClusterType
		namespace = namespace || typ == workloadNamespaceType
		if identity.typ != "" && identity.typ != typ {
			identity.err = errors.New("cannot review Civo deletion protection plan: conflicting resource identity")
		}
		if identity.typ == "" {
			identity.typ = typ
		}
	}
	addURN := func(value string) {
		if value == "" {
			return
		}
		if identity.urn != "" && identity.urn != value {
			identity.err = errors.New("cannot review Civo deletion protection plan: conflicting resource identity")
		}
		if identity.urn == "" {
			identity.urn = value
		}
		parsed, err := urn.Parse(value)
		if err != nil {
			// SDK parser errors include their input; never expose that value.
			identity.err = errors.New("cannot review Civo deletion protection plan: invalid resource identity")
			return
		}
		addType(string(parsed.Type()))
		workloadName = workloadName || parsed.Name() == "witself-namespace"
		if parsed.Stack() == "" || parsed.Project() == "" || parsed.Type() == "" || parsed.Name() == "" {
			identity.err = errors.New("cannot review Civo deletion protection plan: incomplete resource identity")
		}
	}
	addType(step.Type)
	addURN(step.URN)
	for _, state := range []*apitype.StepEventStateMetadata{step.Old, step.New} {
		if state != nil {
			addType(state.Type)
			addURN(state.URN)
		}
	}
	identity.protected = cluster || (namespace && (workloadName || identity.urn == ""))
	identity.unresolved = identity.typ == "" || (identity.protected && identity.urn == "")
	return identity
}

func civoPlanOperationAllowed(op apitype.OpType) bool {
	switch op {
	case apitype.OpSame, apitype.OpCreate, apitype.OpUpdate,
		apitype.OpRead, apitype.OpRefresh, apitype.OpImport:
		return true
	case apitype.OpDelete, apitype.OpReplace, apitype.OpCreateReplacement,
		apitype.OpDeleteReplaced, apitype.OpReadReplacement, apitype.OpReadDiscard,
		apitype.OpDiscardReplaced, apitype.OpRemovePendingReplace, apitype.OpImportReplacement:
		return false
	default:
		return false
	}
}

func reviewCivoPlanStep(identity civoPlanIdentity, op apitype.OpType, enabled bool) (bool, error) {
	if !enabled {
		return false, nil
	}
	// AWS/GCP/Azure store review retains its existing interpretation of metadata.
	// An additional Civo identity source still takes precedence over this bypass.
	if !identity.protected && protectedStoreResource(identity.typ) {
		return false, nil
	}
	if identity.err != nil {
		return false, identity.err
	}
	allowed := civoPlanOperationAllowed(op)
	if identity.unresolved && (identity.protected || !allowed) {
		return false, errors.New("cannot review Civo deletion protection plan: unresolved resource identity may identify a protected resource")
	}
	return identity.protected && !allowed, nil
}

func reviewCivoSavedPlan(path, cellName string, enabled, resourceSteps bool, oldProtectedURNs map[string]bool) error {
	if !enabled && len(oldProtectedURNs) == 0 {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return errors.New("cannot review Civo deletion protection plan: unreadable saved plan")
	}
	var plan *apitype.DeploymentPlanV1
	if err := json.Unmarshal(raw, &plan); err != nil || plan == nil {
		return errors.New("cannot review Civo deletion protection plan: undecodable saved plan")
	}
	// encoding/json accepts null for a non-pointer map value as a zero struct.
	// A null resource entry is missing review evidence, not an empty step list.
	var entries struct {
		ResourcePlans map[string]*apitype.ResourcePlanV1 `json:"resourcePlans"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return errors.New("cannot review Civo deletion protection plan: undecodable resource plans")
	}
	for resourceURN, entry := range entries.ResourcePlans {
		if entry == nil && (enabled || oldProtectedURNs[resourceURN]) {
			return errors.New("cannot review Civo deletion protection plan: null resource plan")
		}
	}
	if len(plan.ResourcePlans) == 0 && resourceSteps {
		return errors.New("cannot review Civo deletion protection plan: saved plan has no resource plans despite reported resource steps")
	}
	for resourceURN, resourcePlan := range plan.ResourcePlans {
		if !enabled && !oldProtectedURNs[string(resourceURN)] {
			continue
		}
		step := apitype.StepEventMetadata{URN: string(resourceURN)}
		if resourcePlan.Goal != nil {
			step.Type = string(resourcePlan.Goal.Type)
		}
		identity := resolveCivoPlanIdentity(step)
		if resourcePlan.Goal != nil && resourcePlan.Goal.Name != "" {
			if resourcePlan.Goal.Type == workloadNamespaceType && resourcePlan.Goal.Name == "witself-namespace" {
				identity.protected = true
			}
			parsed, parseErr := urn.Parse(string(resourceURN))
			if parseErr != nil || parsed.Name() != resourcePlan.Goal.Name {
				identity.err = errors.New("cannot review Civo deletion protection plan: conflicting saved resource identity")
			}
		}
		// Validate identity even if a malformed entry omitted its steps.
		if _, err := reviewCivoPlanStep(identity, apitype.OpSame, true); err != nil {
			return err
		}
		for _, op := range resourcePlan.Steps {
			blocked, err := reviewCivoPlanStep(identity, op, true)
			if err != nil {
				return err
			}
			if blocked {
				return civoProtectionPlanError(cellName, map[string]bool{identity.urn: true})
			}
		}
	}
	return nil
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

func civoProtectionPlanError(cellName string, violations map[string]bool) error {
	if len(violations) == 0 {
		return nil
	}
	resources := make([]string, 0, len(violations))
	for urn := range violations {
		resources = append(resources, urn)
	}
	sort.Strings(resources)
	return fmt.Errorf("deletion protection refuses a plan that would delete or force replacement of Civo cluster/workload-namespace resources: %s; in-cluster PostgreSQL volumes would be deleted. Recording deletion_protection: false for cell %q in the inventory permits replacement on the next up; there is no separate replacement interlock. Only destroy checks the persisted protection output", strings.Join(resources, ", "), cellName)
}
