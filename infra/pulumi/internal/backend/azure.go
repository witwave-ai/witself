package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	azureStateContainer = "pulumi-state"
	azureStateKeyName   = "pulumi-secrets"
)

type azureAccount struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	TenantID string `json:"tenantId"`
}

// AzureFeature names a subscription-level preview feature that must be
// registered before AKS can use it.
type AzureFeature struct {
	Namespace string
	Name      string
}

func azureNames(subscriptionID, regionCode string) *Info {
	shortID := strings.ToLower(strings.ReplaceAll(subscriptionID, "-", ""))
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	storageAccount := fmt.Sprintf("witselfstate%s%s", shortID, regionCode)
	vaultName := fmt.Sprintf("witself-state-%s-%s", shortID[:4], regionCode)
	keyURI := fmt.Sprintf("%s.vault.azure.net/keys/%s", vaultName, azureStateKeyName)
	return &Info{
		Bucket:          storageAccount,
		BackendURL:      fmt.Sprintf("azblob://%s?storage_account=%s", azureStateContainer, storageAccount),
		KeyAlias:        keyURI,
		SecretsProvider: "azurekeyvault://" + keyURI,
		SubscriptionID:  subscriptionID,
	}
}

// EnsureAzureCLI verifies that Azure CLI credentials are usable. Pulumi's
// azblob backend and azurekeyvault secrets provider can use Azure CLI auth, and
// bootstrap uses az directly so there is one operator login path.
func EnsureAzureCLI(ctx context.Context, subscription string) error {
	if _, err := exec.LookPath("az"); err != nil {
		return fmt.Errorf("azure CLI is required for Azure backends: %w\nrun: brew install azure-cli && az login --tenant <tenant-id>", err)
	}
	if subscription != "" {
		if _, err := runAzure(ctx, nil, "account", "set", "--subscription", subscription); err != nil {
			return err
		}
	}
	if _, err := currentAzureAccount(ctx); err != nil {
		return err
	}
	return nil
}

func currentAzureAccount(ctx context.Context) (*azureAccount, error) {
	out, err := runAzure(ctx, nil, "account", "show", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("read Azure account (run `az login`): %w", err)
	}
	var acct azureAccount
	if err := json.Unmarshal(out, &acct); err != nil {
		return nil, fmt.Errorf("parse Azure account: %w", err)
	}
	if acct.ID == "" {
		return nil, errors.New("azure CLI is not logged into a subscription; run `az login` and select the Witwave subscription")
	}
	return &acct, nil
}

// ResolveAzure computes the Azure Blob/Key Vault backend names and reports
// whether the storage account already exists. One subscription+region backend
// can hold many cell stacks; the stack name remains the cell boundary.
// The middle parameter (region) is unused — names key off regionCode —
// but kept for signature symmetry with ResolveAWS/ResolveGCP.
func ResolveAzure(ctx context.Context, subscription string, _ string, regionCode string) (*Info, bool, error) {
	if err := EnsureAzureCLI(ctx, subscription); err != nil {
		return nil, false, err
	}
	acct, err := currentAzureAccount(ctx)
	if err != nil {
		return nil, false, err
	}
	info := azureNames(acct.ID, regionCode)
	if _, err := runAzure(ctx, nil, "storage", "account", "show", "--subscription", acct.ID, "--name", info.Bucket, "-o", "none"); err != nil {
		return info, false, nil
	}
	key, err := azureStorageKey(ctx, acct.ID, azureStateResourceGroup(regionCode), info.Bucket)
	if err != nil {
		return nil, false, err
	}
	info.StorageKey = key
	return info, true, nil
}

// EnsureAzureProviders idempotently registers Azure resource providers needed by
// the cell graph. Keeping this outside the Pulumi graph preserves one-shot first
// runs in newly prepared subscriptions.
func EnsureAzureProviders(ctx context.Context, log func(string), namespaces ...string) error {
	if err := EnsureAzureCLI(ctx, ""); err != nil {
		return err
	}
	for _, namespace := range namespaces {
		if err := ensureAzureProvider(ctx, namespace, log); err != nil {
			return err
		}
	}
	return nil
}

// EnsureAzureFeatures idempotently registers preview/flighted Azure features
// required by workload resources before the Pulumi graph starts.
func EnsureAzureFeatures(ctx context.Context, log func(string), features ...AzureFeature) error {
	if err := EnsureAzureCLI(ctx, ""); err != nil {
		return err
	}
	for _, feature := range features {
		if err := ensureAzureFeature(ctx, feature.Namespace, feature.Name, log); err != nil {
			return err
		}
	}
	return nil
}

// BootstrapAzure idempotently ensures the Azure state backend exists: a
// versioned private Blob container plus a Key Vault key for Pulumi's
// azurekeyvault secrets provider. Safe to re-run.
func BootstrapAzure(ctx context.Context, subscription, region, regionCode string, log func(string)) (*Info, error) {
	if err := EnsureAzureCLI(ctx, subscription); err != nil {
		return nil, err
	}
	acct, err := currentAzureAccount(ctx)
	if err != nil {
		return nil, err
	}
	info := azureNames(acct.ID, regionCode)
	resourceGroup := azureStateResourceGroup(regionCode)
	tags := []string{"app=witself", "witself_component=state", "witself_managed_by=witself-infra"}

	if err := ensureAzureProvider(ctx, "Microsoft.Storage", log); err != nil {
		return nil, err
	}
	if err := ensureAzureProvider(ctx, "Microsoft.KeyVault", log); err != nil {
		return nil, err
	}

	if _, err := runAzure(ctx, nil, "group", "create",
		"--subscription", acct.ID,
		"--name", resourceGroup,
		"--location", region,
		"--tags", tags[0], tags[1], tags[2],
		"-o", "none"); err != nil {
		return nil, fmt.Errorf("ensure Azure resource group: %w", err)
	}
	log("azure: ensured resource group " + resourceGroup)

	if err := ensureAzureStorage(ctx, acct.ID, resourceGroup, region, info.Bucket, tags, log); err != nil {
		return nil, err
	}
	key, err := azureStorageKey(ctx, acct.ID, resourceGroup, info.Bucket)
	if err != nil {
		return nil, err
	}
	info.StorageKey = key
	if err := ensureAzureStateContainer(ctx, info.Bucket, key, log); err != nil {
		return nil, err
	}
	if err := ensureAzureKeyVault(ctx, acct.ID, resourceGroup, region, info.KeyAlias, tags, log); err != nil {
		return nil, err
	}
	return info, nil
}

func azureStateResourceGroup(regionCode string) string {
	return "witself-state-" + regionCode
}

func ensureAzureProvider(ctx context.Context, namespace string, log func(string)) error {
	state, err := azureProviderState(ctx, namespace)
	if err != nil {
		return err
	}
	if state != "Registered" {
		if _, err := runAzure(ctx, nil, "provider", "register", "--namespace", namespace, "--wait", "-o", "none"); err != nil {
			return fmt.Errorf("register Azure provider %s: %w", namespace, err)
		}
	}
	log("azure: ensured provider " + namespace)
	return nil
}

func ensureAzureFeature(ctx context.Context, namespace, name string, log func(string)) error {
	state, err := azureFeatureState(ctx, namespace, name)
	if err != nil || state == "" {
		state = "NotRegistered"
	}
	if state != "Registered" && state != "Registering" {
		if _, err := runAzure(ctx, nil, "feature", "register", "--namespace", namespace, "--name", name, "-o", "none"); err != nil {
			return fmt.Errorf("register Azure feature %s/%s: %w", namespace, name, err)
		}
	}
	deadline := time.Now().Add(30 * time.Minute)
	for {
		state, err = azureFeatureState(ctx, namespace, name)
		if err == nil && state == "Registered" {
			log("azure: ensured feature " + namespace + "/" + name)
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("wait for Azure feature %s/%s: %w", namespace, name, err)
			}
			return fmt.Errorf("azure feature %s/%s is %q after waiting 30m", namespace, name, state)
		}
		time.Sleep(15 * time.Second)
	}
}

func azureProviderState(ctx context.Context, namespace string) (string, error) {
	out, err := runAzure(ctx, nil, "provider", "show", "--namespace", namespace, "--query", "registrationState", "-o", "tsv")
	if err != nil {
		return "", fmt.Errorf("read Azure provider %s: %w", namespace, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func azureFeatureState(ctx context.Context, namespace, name string) (string, error) {
	out, err := runAzure(ctx, nil, "feature", "show", "--namespace", namespace, "--name", name, "--query", "properties.state", "-o", "tsv")
	if err != nil {
		return "", fmt.Errorf("read Azure feature %s/%s: %w", namespace, name, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func ensureAzureStorage(ctx context.Context, subscriptionID, resourceGroup, region, account string, tags []string, log func(string)) error {
	if _, err := runAzure(ctx, nil, "storage", "account", "show", "--subscription", subscriptionID, "--name", account, "-o", "none"); err == nil {
		log("storage: reusing " + account)
	} else {
		if _, err := runAzure(ctx, nil, "storage", "account", "create",
			"--subscription", subscriptionID,
			"--resource-group", resourceGroup,
			"--name", account,
			"--location", region,
			"--sku", "Standard_LRS",
			"--kind", "StorageV2",
			"--https-only", "true",
			"--min-tls-version", "TLS1_2",
			"--allow-blob-public-access", "false",
			"--tags", tags[0], tags[1], tags[2],
			"-o", "none"); err != nil {
			return fmt.Errorf("create Azure storage account %s: %w", account, err)
		}
		log("storage: created " + account)
	}
	if _, err := runAzure(ctx, nil, "storage", "account", "update",
		"--subscription", subscriptionID,
		"--resource-group", resourceGroup,
		"--name", account,
		"--https-only", "true",
		"--min-tls-version", "TLS1_2",
		"--allow-blob-public-access", "false",
		"-o", "none"); err != nil {
		return fmt.Errorf("harden Azure storage account %s: %w", account, err)
	}
	if _, err := runAzure(ctx, nil, "storage", "account", "blob-service-properties", "update",
		"--subscription", subscriptionID,
		"--resource-group", resourceGroup,
		"--account-name", account,
		"--enable-versioning", "true",
		"--enable-delete-retention", "true",
		"--delete-retention-days", "30",
		"--enable-container-delete-retention", "true",
		"--container-delete-retention-days", "30",
		"-o", "none"); err != nil {
		return fmt.Errorf("enable Azure blob versioning/retention for %s: %w", account, err)
	}
	return nil
}

func azureStorageKey(ctx context.Context, subscriptionID, resourceGroup, account string) (string, error) {
	out, err := runAzure(ctx, nil, "storage", "account", "keys", "list",
		"--subscription", subscriptionID,
		"--resource-group", resourceGroup,
		"--account-name", account,
		"--query", "[0].value",
		"-o", "tsv")
	if err != nil {
		return "", fmt.Errorf("read Azure storage account key: %w", err)
	}
	key := strings.TrimSpace(string(out))
	if key == "" {
		return "", errors.New("azure storage account key was empty")
	}
	return key, nil
}

func ensureAzureStateContainer(ctx context.Context, account, key string, log func(string)) error {
	env := map[string]string{
		"AZURE_STORAGE_ACCOUNT": account,
		"AZURE_STORAGE_KEY":     key,
	}
	if _, err := runAzure(ctx, env, "storage", "container", "create",
		"--name", azureStateContainer,
		"--public-access", "off",
		"-o", "none"); err != nil {
		return fmt.Errorf("ensure Azure blob container %s: %w", azureStateContainer, err)
	}
	log("blob: ensured container " + azureStateContainer)
	return nil
}

func ensureAzureKeyVault(ctx context.Context, subscriptionID, resourceGroup, region, keyURI string, tags []string, log func(string)) error {
	vaultName := strings.TrimSuffix(strings.Split(keyURI, ".vault.azure.net/keys/")[0], ".vault.azure.net")
	if _, err := runAzure(ctx, nil, "keyvault", "show", "--subscription", subscriptionID, "--name", vaultName, "-o", "none"); err == nil {
		log("keyvault: reusing " + vaultName)
	} else {
		if _, err := runAzure(ctx, nil, "keyvault", "create",
			"--subscription", subscriptionID,
			"--resource-group", resourceGroup,
			"--name", vaultName,
			"--location", region,
			"--sku", "standard",
			"--retention-days", "90",
			"--enable-purge-protection", "true",
			"--tags", tags[0], tags[1], tags[2],
			"-o", "none"); err != nil {
			return fmt.Errorf("create Azure Key Vault %s: %w", vaultName, err)
		}
		log("keyvault: created " + vaultName)
	}
	objectID, err := azureSignedInObjectID(ctx)
	if err != nil {
		return err
	}
	rbac, err := azureKeyVaultRBACEnabled(ctx, subscriptionID, vaultName)
	if err != nil {
		return err
	}
	if rbac {
		vaultID, err := azureKeyVaultID(ctx, subscriptionID, vaultName)
		if err != nil {
			return err
		}
		if err := ensureAzureRoleAssignment(ctx, objectID, "Key Vault Crypto Officer", vaultID); err != nil {
			return err
		}
		log("keyvault: ensured Key Vault Crypto Officer for current user")
	} else {
		if _, err := runAzure(ctx, nil, "keyvault", "set-policy",
			"--subscription", subscriptionID,
			"--name", vaultName,
			"--object-id", objectID,
			"--key-permissions", "get", "list", "create", "encrypt", "decrypt", "wrapKey", "unwrapKey",
			"-o", "none"); err != nil {
			return fmt.Errorf("ensure Azure Key Vault policy for current user: %w", err)
		}
	}
	keyName := strings.TrimPrefix(keyURI, vaultName+".vault.azure.net/keys/")
	return ensureAzureKeyVaultKey(ctx, vaultName, keyName, log)
}

func azureKeyVaultRBACEnabled(ctx context.Context, subscriptionID, vaultName string) (bool, error) {
	out, err := runAzure(ctx, nil, "keyvault", "show",
		"--subscription", subscriptionID,
		"--name", vaultName,
		"--query", "properties.enableRbacAuthorization",
		"-o", "tsv")
	if err != nil {
		return false, fmt.Errorf("read Azure Key Vault authorization mode: %w", err)
	}
	return strings.EqualFold(strings.TrimSpace(string(out)), "true"), nil
}

func azureKeyVaultID(ctx context.Context, subscriptionID, vaultName string) (string, error) {
	out, err := runAzure(ctx, nil, "keyvault", "show",
		"--subscription", subscriptionID,
		"--name", vaultName,
		"--query", "id",
		"-o", "tsv")
	if err != nil {
		return "", fmt.Errorf("read Azure Key Vault ID: %w", err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", errors.New("azure Key Vault ID was empty")
	}
	return id, nil
}

func ensureAzureRoleAssignment(ctx context.Context, objectID, role, scope string) error {
	out, err := runAzure(ctx, nil, "role", "assignment", "list",
		"--assignee", objectID,
		"--role", role,
		"--scope", scope,
		"--query", "length(@)",
		"-o", "tsv")
	if err != nil {
		return fmt.Errorf("check Azure role assignment %s: %w", role, err)
	}
	if strings.TrimSpace(string(out)) != "0" {
		return nil
	}
	if _, err := runAzure(ctx, nil, "role", "assignment", "create",
		"--assignee-object-id", objectID,
		"--assignee-principal-type", "User",
		"--role", role,
		"--scope", scope,
		"-o", "none"); err != nil {
		return fmt.Errorf("create Azure role assignment %s: %w", role, err)
	}
	return nil
}

func ensureAzureKeyVaultKey(ctx context.Context, vaultName, keyName string, log func(string)) error {
	if _, err := runAzure(ctx, nil, "keyvault", "key", "show",
		"--vault-name", vaultName,
		"--name", keyName,
		"-o", "none"); err == nil {
		log("keyvault: reusing key " + keyName)
		return nil
	}
	var lastErr error
	for attempt := 0; attempt < 18; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Second):
			}
		}
		if _, err := runAzure(ctx, nil, "keyvault", "key", "create",
			"--vault-name", vaultName,
			"--name", keyName,
			"--kty", "RSA",
			"--size", "2048",
			"--ops", "encrypt", "decrypt", "wrapKey", "unwrapKey",
			"-o", "none"); err == nil {
			log("keyvault: created key " + keyName)
			return nil
		} else { //nolint:revive // err is scoped to the if — restructuring would widen it
			lastErr = err
		}
	}
	return fmt.Errorf("create Azure Key Vault key %s: %w", keyName, lastErr)
}

func azureSignedInObjectID(ctx context.Context) (string, error) {
	out, err := runAzure(ctx, nil, "ad", "signed-in-user", "show", "--query", "id", "-o", "tsv")
	if err != nil {
		return "", fmt.Errorf("read Azure signed-in user object ID: %w", err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", errors.New("azure signed-in user object ID was empty")
	}
	return id, nil
}

func runAzure(ctx context.Context, extraEnv map[string]string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "az", args...)
	if extraEnv != nil {
		env := os.Environ()
		for k, v := range extraEnv {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("az %s: %w: %s", redactedAzureArgs(args), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func redactedAzureArgs(args []string) string {
	cp := append([]string(nil), args...)
	for i := 0; i < len(cp)-1; i++ {
		if cp[i] == "--account-key" {
			cp[i+1] = "[secret]"
		}
	}
	return strings.Join(cp, " ")
}
