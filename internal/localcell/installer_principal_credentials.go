package localcell

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net/url"
	"os"
	"reflect"
	"runtime"
	"strings"

	"github.com/witwave-ai/witself/internal/store"
)

// credentialPurpose is a closed custody selector. A caller cannot supply a role,
// path or connection URL, and no selected runtime purpose falls back to bootstrap.
type credentialPurpose uint8

const (
	credentialBootstrap credentialPurpose = iota
	credentialServer
	credentialWorker
	credentialMaintenance
	credentialSnapshot
	credentialLegacy
	credentialMigrator
)

const installerMigratorRole = "witself_sync_migrator"

var principalCredentialPurposes = [...]credentialPurpose{
	credentialServer, credentialWorker, credentialMaintenance,
	credentialSnapshot, credentialLegacy, credentialMigrator,
}

func credentialRole(purpose credentialPurpose) (string, error) {
	switch purpose {
	case credentialBootstrap:
		return "localcell", nil
	case credentialServer:
		return string(store.SyncPrincipalServer), nil
	case credentialWorker:
		return string(store.SyncPrincipalWorker), nil
	case credentialMaintenance:
		return string(store.SyncPrincipalMaintenance), nil
	case credentialSnapshot:
		return string(store.SyncPrincipalSnapshot), nil
	case credentialLegacy:
		return string(store.SyncPrincipalLegacy), nil
	case credentialMigrator:
		return installerMigratorRole, nil
	default:
		return "", ErrLocalPrincipalCredentialsRefused
	}
}

func (c *Cell) credentialPath(purpose credentialPurpose) (string, error) {
	role, err := credentialRole(purpose)
	if err != nil {
		return "", err
	}
	if purpose == credentialBootstrap {
		return c.path("run", "postgres.password"), nil
	}
	return c.path("run", "credentials", role), nil
}

func (c *Cell) credentialPassword(m *Manifest, purpose credentialPurpose) (string, error) {
	if err := validatePrincipalLayout(m); err != nil {
		return "", err
	}
	if purpose == credentialBootstrap {
		return c.password()
	}
	if m.PrincipalLayoutVersion == 0 {
		return "", ErrLocalPrincipalCredentialsRefused
	}
	path, err := c.credentialPath(purpose)
	if err != nil {
		return "", err
	}
	for _, directory := range []string{c.cfg.Root, c.path("run"), c.path("run", "credentials")} {
		if privateDir(directory, false) != nil {
			return "", ErrLocalPrincipalCredentialsRefused
		}
	}
	before, err := os.Lstat(path)
	if err != nil || !privatePrincipalCredential(path, before) {
		return "", ErrLocalPrincipalCredentialsRefused
	}
	f, err := openPrivateFile(path, os.O_RDONLY, 0)
	if err != nil {
		return "", ErrLocalPrincipalCredentialsRefused
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) || !privatePrincipalCredential(path, opened) {
		return "", ErrLocalPrincipalCredentialsRefused
	}
	data, err := io.ReadAll(io.LimitReader(f, 129))
	after, statErr := os.Lstat(path)
	if err != nil || statErr != nil || !os.SameFile(opened, after) || !privatePrincipalCredential(path, after) {
		return "", ErrLocalPrincipalCredentialsRefused
	}
	value := strings.TrimSuffix(string(data), "\n")
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != 32 || len(data) != 44 || data[len(data)-1] != '\n' {
		return "", ErrLocalPrincipalCredentialsRefused
	}
	return value, nil
}

func privatePrincipalCredential(path string, info os.FileInfo) bool {
	if !info.Mode().IsRegular() || info.Size() != 44 || !privatePassword(path, info) {
		return false
	}
	if runtime.GOOS == "windows" {
		// privatePassword and openPrivateFile enforce the current user's protected
		// DACL and reject reparse points, including at the open handle boundary.
		return true
	}
	// Stat_t differs by OS. Read only its public UID/link fields so this common
	// source retains the native Windows implementation without Unix syscall types.
	stat := reflect.ValueOf(info.Sys())
	if stat.Kind() != reflect.Pointer || stat.IsNil() {
		return false
	}
	stat = stat.Elem()
	if stat.Kind() != reflect.Struct {
		return false
	}
	uid, links := stat.FieldByName("Uid"), stat.FieldByName("Nlink")
	return uid.IsValid() && uid.CanUint() && uid.Uint() == uint64(os.Geteuid()) &&
		links.IsValid() && links.CanUint() && links.Uint() == 1
}

// makePrincipalPasswords never rotates one side of a database/file pair. A full
// published set can be reused after authentication; a partial set needs explicit
// recovery and cannot become launchable by generating the missing values.
func (c *Cell) makePrincipalPasswords(m *Manifest) error {
	if validatePrincipalLayout(m) != nil || m.PrincipalLayoutVersion == 0 {
		return ErrLocalPrincipalCredentialsRefused
	}
	if privateDir(c.cfg.Root, false) != nil || privateDir(c.path("run"), false) != nil ||
		privateDir(c.path("run", "credentials"), true) != nil {
		return ErrLocalPrincipalCredentialsRefused
	}
	entries, err := os.ReadDir(c.path("run", "credentials"))
	if err != nil {
		return ErrLocalPrincipalCredentialsRefused
	}
	allowed := make(map[string]bool, len(principalCredentialPurposes))
	for _, purpose := range principalCredentialPurposes {
		role, _ := credentialRole(purpose)
		allowed[role] = true
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return ErrLocalPrincipalCredentialsRefused
		}
	}
	present := 0
	for _, purpose := range principalCredentialPurposes {
		path, _ := c.credentialPath(purpose)
		if _, err := os.Lstat(path); err == nil {
			present++
		} else if !errors.Is(err, os.ErrNotExist) {
			return ErrLocalPrincipalCredentialsRefused
		}
	}
	if present != 0 && present != len(principalCredentialPurposes) {
		return ErrLocalPrincipalCredentialsRefused
	}
	if present != 0 {
		return c.validatePrincipalPasswords(m)
	}
	for _, purpose := range principalCredentialPurposes {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return ErrLocalPrincipalCredentialsRefused
		}
		path, _ := c.credentialPath(purpose)
		if err := writeAtomicBytes(path, []byte(base64.RawURLEncoding.EncodeToString(secret[:])+"\n"), nil); err != nil {
			return ErrLocalPrincipalCredentialsRefused
		}
	}
	return c.validatePrincipalPasswords(m)
}

func (c *Cell) validatePrincipalPasswords(m *Manifest) error {
	if validatePrincipalLayout(m) != nil || m.PrincipalLayoutVersion == 0 || privateDir(c.path("run", "credentials"), false) != nil {
		return ErrLocalPrincipalCredentialsRefused
	}
	entries, err := os.ReadDir(c.path("run", "credentials"))
	if err != nil || len(entries) != len(principalCredentialPurposes) {
		return ErrLocalPrincipalCredentialsRefused
	}
	seen := make(map[string]bool, len(principalCredentialPurposes))
	for _, purpose := range principalCredentialPurposes {
		value, err := c.credentialPassword(m, purpose)
		if err != nil || seen[value] {
			return ErrLocalPrincipalCredentialsRefused
		}
		seen[value] = true
	}
	return nil
}

func (c *Cell) principalDSN(m *Manifest, purpose credentialPurpose) (string, error) {
	password, err := c.credentialPassword(m, purpose)
	if err != nil {
		return "", err
	}
	role, err := credentialRole(purpose)
	if err != nil {
		return "", err
	}
	dsn := url.URL{Scheme: "postgres", Host: addr(m.Ports.Postgres), Path: "/" + localDatabaseName,
		User: url.UserPassword(role, password), RawQuery: "sslmode=disable"}
	if purpose == credentialMigrator {
		// Goose opens new physical connections from this DSN. The startup option
		// applies owner custody to every connection, not just the initial pool.
		query := dsn.Query()
		query.Set("options", "-c role="+string(store.SyncPrincipalOwner))
		dsn.RawQuery = query.Encode()
	}
	return dsn.String(), nil
}
