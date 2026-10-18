package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"

	"github.com/witwave-ai/witself/infra/pulumi/internal/cell"
)

const (
	civoDefaultCoreQuota = 16
	civoMaxCoreQuota     = 1024
)

var civoNodeCores = map[string]int{"g4s.kube.medium": 2, "g4s.kube.large": 4}

// civoInventoryShape shares entry > defaults > built-in resolution between
// inventory validation and the planned core-quota calculation.
func civoInventoryShape(entry cellEntry, defaults *cellEntry) (profile, nodeSize string) {
	profile, nodeSize = "minimal", "g4s.kube.medium"
	for _, e := range []*cellEntry{defaults, &entry} {
		if e == nil {
			continue
		}
		if e.Profile != nil {
			profile = *e.Profile
		}
		if e.CivoNodeSize != nil {
			nodeSize = *e.CivoNodeSize
		}
	}
	return profile, nodeSize
}

func civoNodeCountFlagsError(cmd, cellName, accountID, profile, nodeSize string, nodeCount, quota int, inventory func() (*infraConfig, error)) error {
	if cmd != "up" && cmd != "preview" {
		return nil
	}
	minimum := cell.CivoDefaultNodeCount(profile)
	if nodeCount != 0 && (nodeCount < minimum || nodeCount > cell.CivoMaxNodeCount) {
		return fmt.Errorf("-civo-node-count %d must be from %d to %d for -profile %s (0 keeps the profile's default of %d)", nodeCount, minimum, cell.CivoMaxNodeCount, profile, minimum)
	}
	if quota < 1 || quota > civoMaxCoreQuota {
		return fmt.Errorf("-civo-core-quota %d must be from 1 to 1024", quota)
	}
	if nodeCount <= minimum {
		return nil
	}
	var inv *infraConfig
	if inventory != nil {
		var err error
		inv, err = inventory()
		if err != nil {
			return err
		}
	}
	return civoCoreQuotaError(cellName, accountID, profile, nodeSize, nodeCount, quota, inv)
}

// civoCoreQuotaError counts the planned inventory, not live usage. A destroyed
// cell left in the inventory still counts until config remove-cell. Civo still
// enforces the quota at apply time. Memory, disk and instance quotas are not checked.
func civoCoreQuotaError(cellName, accountID, profile, nodeSize string, nodeCount, quota int, inventory *infraConfig) error {
	if nodeCount <= cell.CivoDefaultNodeCount(profile) {
		return nil
	}
	cores, ok := civoNodeCores[nodeSize]
	if !ok {
		return fmt.Errorf("civo node size %q has no known core count, so the quota check cannot run", nodeSize)
	}
	need := nodeCount * cores
	if inventory == nil {
		if need > quota {
			return fmt.Errorf("civo node count %d for cell %s needs %d cores (%d x %s at %d each), above the Civo core quota of %d; without -cell no other cell is counted", nodeCount, cellName, need, nodeCount, nodeSize, cores, quota)
		}
		return nil
	}
	names := make([]string, 0, len(inventory.Cells))
	for name := range inventory.Cells {
		names = append(names, name)
	}
	sort.Strings(names)
	accountID = normalizeCivoAccountID(accountID)
	others := 0
	for _, name := range names {
		e := inventory.Cells[name]
		if e.Cloud == nil || *e.Cloud != "civo" || name == cellName {
			continue
		}
		if sc := e.SecurityContext; sc != nil && sc.Civo != nil && sc.Civo.ExpectedAccountID != nil {
			otherAccount := normalizeCivoAccountID(*sc.Civo.ExpectedAccountID)
			if accountID != "" && otherAccount != "" && accountID != otherAccount {
				continue
			}
		}
		otherProfile, otherSize := civoInventoryShape(e, inventory.Defaults)
		otherCores, ok := civoNodeCores[otherSize]
		if !ok {
			return fmt.Errorf("cell %q in the inventory has civo_node_size %q, whose core count is unknown; correct that record before raising a node count", name, otherSize)
		}
		otherCount := cell.CivoDefaultNodeCount(otherProfile)
		if e.CivoNodeCount != nil {
			otherCount = *e.CivoNodeCount
		}
		others += otherCount * otherCores
	}
	if need+others > quota {
		return fmt.Errorf("civo node count %d for cell %s needs %d cores (%d x %s at %d each); the other Civo cells in the inventory plan %d, so the total of %d is above the Civo core quota of %d. Retire a cell (destroy, then config remove-cell), or record a larger defaults.civo_core_quota once Civo has raised the account's quota", nodeCount, cellName, need, nodeCount, nodeSize, cores, others, need+others, quota)
	}
	return nil
}

type civoConfigWriter interface {
	SetConfig(context.Context, string, auto.ConfigValue) error
	RemoveConfig(context.Context, string) error
}

func applyCivoNodeCount(ctx context.Context, w civoConfigWriter, cmd string, count int) error {
	if cmd != "up" && cmd != "preview" {
		return nil
	}
	if count == 0 {
		if err := w.RemoveConfig(ctx, "witself:civoNodeCount"); err != nil {
			return fmt.Errorf("clear Civo config witself:civoNodeCount: %w", err)
		}
		return nil
	}
	if count > 0 {
		if err := w.SetConfig(ctx, "witself:civoNodeCount", auto.ConfigValue{Value: strconv.Itoa(count)}); err != nil {
			return fmt.Errorf("set config witself:civoNodeCount: %w", err)
		}
	}
	return nil
}
