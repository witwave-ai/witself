package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
)

type deploymentExporter interface {
	Export(context.Context) (apitype.UntypedDeployment, error)
}

// A false inventory value authorizes unprotection, but does not prove that the
// separate up applied it. Inspect persisted state before draining or removing
// the cell; provider/Pulumi rejection after fleet removal would be too late.
func runDestroyAfterUnprotect(ctx context.Context, stack deploymentExporter, cellName string, deletionProtection bool, remove, destroy func() error) error {
	// The CLI reloads inventory after its initial safety checks. Honor that
	// newer policy even when the previously applied stack is unprotected.
	if deletionProtection {
		return fmt.Errorf("deletion protection: refusing destroy of cell %q; set deletion_protection: false on that cell in the inventory, then apply a separate up BEFORE destroy", cellName)
	}
	deployment, err := stack.Export(ctx)
	if err != nil {
		// Export uses --show-secrets; SDK errors may contain decrypted stdout
		// and stderr. Do not return or wrap that error for callers to log.
		return fmt.Errorf("deletion protection: cannot verify applied unprotection for cell %q; refusing destroy before fleet removal: stack export failed", cellName)
	}
	protected, err := protectedDeploymentResources(deployment)
	if err != nil {
		return fmt.Errorf("deletion protection: cannot verify applied unprotection for cell %q; refusing destroy before fleet removal: %w", cellName, err)
	}
	if len(protected) != 0 {
		return fmt.Errorf("deletion protection: refusing destroy of cell %q before fleet removal; persisted protection remains on %s. Set deletion_protection: false on that cell in the inventory, then apply a separate up for that same cell and inventory BEFORE destroy", cellName, strings.Join(protected, ", "))
	}
	if err := remove(); err != nil {
		return err
	}
	return destroy()
}

func protectedDeploymentResources(deployment apitype.UntypedDeployment) ([]string, error) {
	// Versions 3 and 4 share DeploymentV3; version 4 adds feature metadata.
	// Unknown future schemas cannot establish that a destructive step is safe.
	if deployment.Version != 3 && deployment.Version != 4 {
		return nil, fmt.Errorf("unsupported exported deployment schema %d", deployment.Version)
	}
	var snapshot *apitype.DeploymentV3
	if err := json.Unmarshal(deployment.Deployment, &snapshot); err != nil {
		return nil, fmt.Errorf("decode exported deployment: %w", err)
	}
	if snapshot == nil {
		return nil, fmt.Errorf("decode exported deployment: missing deployment object")
	}
	resources := append([]apitype.ResourceV3(nil), snapshot.Resources...)
	for _, operation := range snapshot.PendingOperations {
		resources = append(resources, operation.Resource)
	}
	protected := map[string]bool{}
	for _, resource := range resources {
		if resource.External {
			continue
		}
		blocked, err := persistedResourceProtection(resource)
		if err != nil {
			return nil, err
		}
		if blocked {
			name := string(resource.URN)
			if name == "" {
				name = string(resource.Type)
			}
			protected[name] = true
		}
	}
	names := make([]string, 0, len(protected))
	for name := range protected {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func persistedResourceProtection(resource apitype.ResourceV3) (bool, error) {
	typ := string(resource.Type)
	if typ == "pulumi:pulumi:Stack" {
		return persistedProtectionBool(resource.Outputs, "deletionProtection")
	}
	if managedDatabaseLock(resource) {
		return true, nil
	}
	if !protectedStoreResource(typ) {
		return false, nil
	}
	if resource.Protect {
		return true, nil
	}
	return nativeStoreProtection(typ, resource.Inputs, resource.Outputs)
}

func nativeStoreProtection(typ string, inputs, outputs map[string]any) (bool, error) {
	switch typ {
	case "aws:rds/instance:Instance", "gcp:sql/databaseInstance:DatabaseInstance", "gcp:secretmanager/secret:Secret":
		for _, properties := range []map[string]any{inputs, outputs} {
			protected, err := persistedProtectionBool(properties, "deletionProtection")
			if err != nil || protected {
				return protected, err
			}
			if typ == "gcp:sql/databaseInstance:DatabaseInstance" {
				if raw, present := properties["settings"]; present && raw != nil {
					settings, ok := raw.(map[string]any)
					if !ok {
						return false, fmt.Errorf("cannot decode persisted Cloud SQL settings")
					}
					protected, err := persistedProtectionBool(settings, "deletionProtectionEnabled")
					if err != nil || protected {
						return protected, err
					}
				}
			}
		}
	}
	// Azure purge protection cannot be disabled and AWS recovery windows also
	// exist on legacy prod cells. Neither prevents the ordinary soft-delete
	// operation, so they cannot demonstrate an unapplied unprotect update.
	return false, nil
}

func persistedProtectionBool(properties map[string]any, key string) (bool, error) {
	value, present := properties[key]
	if !present {
		return false, nil // Legacy state predates the opt-in protection fields.
	}
	protected, ok := value.(bool)
	if !ok {
		// Never include the value: exported properties may contain secrets.
		return false, fmt.Errorf("cannot decode persisted protection field %s", key)
	}
	return protected, nil
}

func managedDatabaseLock(resource apitype.ResourceV3) bool {
	if string(resource.Type) != "azure-native:authorization:ManagementLockAtResourceLevel" {
		return false
	}
	if strings.HasSuffix(string(resource.URN), "::witself-db-deletion-protection") {
		return true
	}
	return resource.Inputs["resourceProviderNamespace"] == "Microsoft.DBforPostgreSQL" && resource.Inputs["resourceType"] == "flexibleServers"
}
