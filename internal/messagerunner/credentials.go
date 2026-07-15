package messagerunner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	providerCredentialsSchemaV1    = "witself.message-runner-provider-credentials.v1"
	maximumProviderCredentialBytes = 64 * 1024
	maximumProviderCredentialFile  = 128 * 1024
)

type persistedProviderCredentials struct {
	Schema   string            `json:"schema"`
	Provider string            `json:"provider"`
	Values   map[string]string `json:"values"`
}

// CaptureProviderCredentials stores only recognized provider authentication
// environment values in a separate private file. This keeps service
// definitions and value-free runner configuration credential-free while
// allowing launchd/systemd jobs to authenticate after the enabling shell exits.
func (s ConfigStore) CaptureProviderCredentials(provider string, environment []string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	allowed, err := providerCredentialNames(provider)
	if err != nil {
		return err
	}
	values := map[string]string{}
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		name = strings.ToUpper(strings.TrimSpace(name))
		if _, ok := allowed[name]; !ok || value == "" {
			continue
		}
		if strings.ContainsRune(value, 0) || len(value) > maximumProviderCredentialBytes {
			return errors.New("message runner provider credential value is invalid")
		}
		values[name] = value
	}
	if len(values) == 0 {
		existing, loadErr := s.loadProviderCredentials()
		if loadErr == nil && existing.Provider == provider {
			return nil
		}
		if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
			return loadErr
		}
		if err := os.Remove(s.credentialsPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	credentials := persistedProviderCredentials{
		Schema: providerCredentialsSchemaV1, Provider: provider, Values: values,
	}
	if err := validateProviderCredentials(credentials); err != nil {
		return err
	}
	return writePrivateJSONAtomic(s.credentialsPath(), credentials)
}

// ProviderEnvironment loads the recognized authentication values captured for
// this runner's provider. Returned values are intended only for the sanitized
// native provider child environment.
func (s ConfigStore) ProviderEnvironment(provider string) ([]string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if _, err := providerCredentialNames(provider); err != nil {
		return nil, err
	}
	credentials, err := s.loadProviderCredentials()
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if credentials.Provider != provider {
		return nil, errors.New("message runner provider credential binding does not match its configuration")
	}
	names := make([]string, 0, len(credentials.Values))
	for name := range credentials.Values {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+credentials.Values[name])
	}
	return result, nil
}

func (s ConfigStore) credentialsPath() string {
	return filepath.Join(s.Root, s.Runtime, "provider-credentials.json")
}

func (s ConfigStore) loadProviderCredentials() (persistedProviderCredentials, error) {
	path := s.credentialsPath()
	info, err := os.Lstat(path)
	if err != nil {
		return persistedProviderCredentials{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return persistedProviderCredentials{}, fmt.Errorf("message runner provider credentials %s must be a private regular file", path)
	}
	if info.Size() > maximumProviderCredentialFile {
		return persistedProviderCredentials{}, errors.New("message runner provider credentials exceed their size limit")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return persistedProviderCredentials{}, err
	}
	var credentials persistedProviderCredentials
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&credentials); err != nil {
		return persistedProviderCredentials{}, errors.New("parse message runner provider credentials")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return persistedProviderCredentials{}, errors.New("parse message runner provider credentials: trailing data")
	}
	if err := validateProviderCredentials(credentials); err != nil {
		return persistedProviderCredentials{}, err
	}
	return credentials, nil
}

func validateProviderCredentials(credentials persistedProviderCredentials) error {
	if credentials.Schema != providerCredentialsSchemaV1 {
		return errors.New("message runner provider credentials have an unsupported schema")
	}
	allowed, err := providerCredentialNames(credentials.Provider)
	if err != nil {
		return err
	}
	total := 0
	for name, value := range credentials.Values {
		if _, ok := allowed[name]; !ok || value == "" || strings.ContainsRune(value, 0) {
			return errors.New("message runner provider credentials contain an unsupported value")
		}
		total += len(name) + len(value)
	}
	if total > maximumProviderCredentialBytes {
		return errors.New("message runner provider credentials exceed their size limit")
	}
	return nil
}

func providerCredentialNames(provider string) (map[string]struct{}, error) {
	var names []string
	switch provider {
	case string(ProviderClaudeCode):
		names = []string{
			"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN",
		}
	case string(ProviderGrokBuild):
		names = []string{"XAI_API_KEY"}
	default:
		return nil, fmt.Errorf("%w: provider credentials are unsupported for %q", ErrInvalidConfiguration, provider)
	}
	result := make(map[string]struct{}, len(names))
	for _, name := range names {
		result[name] = struct{}{}
	}
	return result, nil
}
