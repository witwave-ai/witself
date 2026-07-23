package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func acquireRuntimeIntegrationOperationLock(runtimeName string) (func(), error) {
	release, _, err := acquireRuntimeIntegrationOperationLockWithProviderRoot(runtimeName)
	return release, err
}

func acquireRuntimeIntegrationOperationLockWithProviderRoot(runtimeName string) (func(), string, error) {
	// Every operation mutates both provider-owned state and the durable binding
	// under WITSELF_HOME. Take the durable-state lock first so two first installs
	// cannot evade serialization merely by selecting different provider roots.
	releaseDurable, err := acquireRuntimeDurableIntegrationOperationLock(runtimeName)
	if err != nil {
		return nil, "", err
	}
	providerRoot := ""
	var releaseProvider func()
	if isGenericProviderRuntime(runtimeName) {
		providerRoot, err = genericProviderOperationLockRoot(runtimeName)
		if err == nil {
			releaseProvider, err = acquireIntegrationOperationLock(
				filepath.Join(providerRoot, ".witself-"+runtimeName+"-operation.lock"),
				integrationDisplayName(runtimeName),
				fmt.Sprintf("another %s install, uninstall, or routing refresh is already running", integrationDisplayName(runtimeName)),
			)
		}
	} else {
		releaseProvider, err = acquireProviderIntegrationOperationLock(runtimeName)
	}
	if err != nil {
		releaseDurable()
		return nil, "", err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			releaseProvider()
			releaseDurable()
		})
	}, providerRoot, nil
}

func acquireRuntimeDurableIntegrationOperationLock(runtimeName string) (func(), error) {
	home, err := local.Home()
	if err != nil {
		return nil, fmt.Errorf("resolve WITSELF_HOME for integration locking: %w", err)
	}
	home, err = cleanCopilotAbsolutePath("WITSELF_HOME integration lock root", home)
	if err != nil {
		return nil, err
	}
	return acquireIntegrationOperationLock(
		filepath.Join(home, "integrations", ".operation-locks", runtimeName+".lock"),
		integrationDisplayName(runtimeName)+" durable state",
		fmt.Sprintf("another %s install, uninstall, or routing refresh is already running", integrationDisplayName(runtimeName)),
	)
}

func acquireProviderIntegrationOperationLock(runtimeName string) (func(), error) {
	switch runtimeName {
	case transcriptcapture.RuntimeAntigravity:
		return acquireAntigravityOperationLock()
	case transcriptcapture.RuntimeCopilot:
		return acquireCopilotOperationLock()
	case transcriptcapture.RuntimeOpenClaw:
		root, err := openClawOperationLockRoot()
		if err != nil {
			return nil, err
		}
		return acquireIntegrationOperationLock(
			filepath.Join(root, ".witself-openclaw-operation.lock"),
			"OpenClaw",
			"another OpenClaw install, uninstall, or routing refresh is already running",
		)
	default:
		if !isGenericProviderRuntime(runtimeName) {
			return nil, fmt.Errorf("runtime %q has no integration operation lock", runtimeName)
		}
		root, err := genericProviderOperationLockRoot(runtimeName)
		if err != nil {
			return nil, err
		}
		return acquireIntegrationOperationLock(
			filepath.Join(root, ".witself-"+runtimeName+"-operation.lock"),
			integrationDisplayName(runtimeName),
			fmt.Sprintf("another %s install, uninstall, or routing refresh is already running", integrationDisplayName(runtimeName)),
		)
	}
}

func genericProviderOperationLockRoot(runtimeName string) (string, error) {
	if installed, err := transcriptcapture.LoadConfig(runtimeName); err == nil {
		if installed.RuntimeConfigRoot != "" {
			return cleanCopilotAbsolutePath(runtimeName+" installed config root", installed.RuntimeConfigRoot)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read existing %s integration before locking: %w", runtimeName, err)
	}
	root, _, err := genericProviderConfigPaths(runtimeName)
	return root, err
}

func antigravityOperationLockRoot() (string, error) {
	if installed, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity); err == nil {
		if installed.RuntimeConfigRoot == "" {
			return "", errors.New("installed Antigravity integration has no config root")
		}
		root, cleanErr := cleanAntigravityAbsolutePath("installed Antigravity config root", installed.RuntimeConfigRoot)
		if cleanErr != nil {
			return "", cleanErr
		}
		if root != installed.RuntimeConfigRoot {
			return "", errors.New("installed Antigravity config root is not canonical")
		}
		return root, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read existing Antigravity integration before locking: %w", err)
	}
	return currentAntigravityConfigRoot()
}

func copilotOperationLockRoot() (string, error) {
	if installed, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCopilot); err == nil {
		if installed.RuntimeConfigRoot == "" {
			return "", errors.New("installed GitHub Copilot integration has no config root")
		}
		root, cleanErr := cleanCopilotAbsolutePath("installed GitHub Copilot config root", installed.RuntimeConfigRoot)
		if cleanErr != nil {
			return "", cleanErr
		}
		if root != installed.RuntimeConfigRoot {
			return "", errors.New("installed GitHub Copilot config root is not canonical")
		}
		return root, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read existing GitHub Copilot integration before locking: %w", err)
	}
	return currentCopilotConfigRoot()
}

func openClawOperationLockRoot() (string, error) {
	if installed, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw); err == nil {
		root, rootErr := openClawTransactionRootFromConfig(installed)
		if rootErr != nil {
			return "", fmt.Errorf("resolve installed OpenClaw transaction root: %w", rootErr)
		}
		return root, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read existing OpenClaw integration before locking: %w", err)
	}
	environment, err := captureOpenClawMCPEnvironment()
	if err != nil {
		return "", err
	}
	root := environment["OPENCLAW_STATE_DIR"]
	if root == "" {
		return "", errors.New("OpenClaw state directory is unavailable for operation locking")
	}
	return root, nil
}
