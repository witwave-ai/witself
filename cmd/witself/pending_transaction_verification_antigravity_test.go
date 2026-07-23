//go:build darwin || linux

package main

import (
	"path/filepath"
	"testing"
)

func platformPendingTransactionTestCases() []pendingTransactionTestCase {
	return []pendingTransactionTestCase{
		{
			name: "antigravity",
			setup: func(t *testing.T) pendingTransactionTestFixture {
				fixture := setupAntigravityIntegrationFixture(t)
				previous := installAntigravityFixtureConfig(t, fixture)
				if _, err := beginAntigravityTransaction(antigravityTransactionUninstall, &previous, nil); err != nil {
					t.Fatal(err)
				}
				return pendingTransactionTestFixture{
					runtime:   previous.Runtime,
					operation: antigravityTransactionUninstall,
					persisted: &previous,
					paths: []string{
						antigravityTransactionPath(previous.RuntimeConfigRoot),
						previous.RuntimeMCPConfigPath,
						filepath.Join(previous.RuntimePluginPath, "plugin.json"),
					},
				}
			},
		},
	}
}
