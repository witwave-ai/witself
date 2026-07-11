// Package local tracks this machine's named accounts. A local name binds an
// account id and its operator token, so commands can say --account NAME (or
// nothing at all, which means the name "default") instead of carrying
// --endpoint/--token-file ceremony. Nothing here is server-side: local names
// are how ONE MACHINE keeps track of its accounts.
//
// Deliberately NOT stored: the cell endpoint. Accounts can move between
// cells, so every command asks the control plane directory where the account
// lives right now — a migrated account keeps working with no local edits.
//
// Layout (WITSELF_HOME overrides ~/.witself):
//
//	~/.witself/config.json                                                     non-secret bindings, CLI-managed
//	~/.witself/tokens/accounts/<name>/owner.token                              the owner's operator token, 0600
//	~/.witself/tokens/accounts/<name>/realms/<realm>/agents/<agent>.token      agent tokens, 0600
//
// Each account gets a directory so future per-operator tokens have a home
// beside the owner's. (One legacy generation is still read: the flat
// accounts/<name>.token files written before the per-account directories.)
//
// The name "default" is what commands use when no --account flag and no
// WITSELF_ACCOUNT env are given. There is no separate default pointer: the
// name IS the defaultness.
package local

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// namePattern: lowercase letters, digits, hyphens. No underscore — so a raw
// account id (acc_...) can never collide with a local name.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Account is one named binding (the non-secret half).
type Account struct {
	ID    string `json:"id"`
	Email string `json:"email,omitempty"`
}

// Config is the CLI-managed local configuration file.
type Config struct {
	Accounts map[string]Account `json:"accounts"`
}

// ErrNameTaken is returned by Save for an already-used name — never silently
// overwrite a binding (that orphans the old account's credential).
var ErrNameTaken = errors.New("local account name already exists")

func root() (string, error) {
	if r := os.Getenv("WITSELF_HOME"); r != "" {
		return r, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".witself"), nil
}

func configPath() (string, error) {
	r, err := root()
	if err != nil {
		return "", err
	}
	return filepath.Join(r, "config.json"), nil
}

// TokenPath returns where the named account's owner token lives.
func TokenPath(name string) (string, error) {
	r, err := root()
	if err != nil {
		return "", err
	}
	return filepath.Join(r, "tokens", "accounts", name, "owner.token"), nil
}

// AgentTokenPath returns the canonical realm-scoped token path for a named
// agent. Every component is a local selector, not a server-side identity
// claim; the token loaded from the path remains the authenticated identity.
func AgentTokenPath(account, realm, agent string) (string, error) {
	for label, value := range map[string]string{
		"account": account,
		"realm":   realm,
		"agent":   agent,
	} {
		if !namePattern.MatchString(value) {
			return "", fmt.Errorf("invalid local %s name %q (lowercase letters, digits, hyphens)", label, value)
		}
	}
	r, err := root()
	if err != nil {
		return "", err
	}
	return filepath.Join(r, "tokens", "accounts", account, "realms", realm, "agents", agent+".token"), nil
}

// legacyAgentTokenPath is the pre-realm agent token layout emitted by the
// first token-create implementation. Reads retain this fallback so upgrading
// the CLI does not strand an existing agent credential.
func legacyAgentTokenPath(account, agent string) (string, error) {
	r, err := root()
	if err != nil {
		return "", err
	}
	return filepath.Join(r, "tokens", "accounts", account, "agents", agent+".token"), nil
}

// ReadAgentToken loads a named agent's credential from the canonical path,
// falling back to the original account-level agent path. The returned path is
// the file that supplied the credential and is safe to report in errors.
func ReadAgentToken(account, realm, agent string) (token, path string, err error) {
	tp, err := AgentTokenPath(account, realm, agent)
	if err != nil {
		return "", "", err
	}
	b, err := os.ReadFile(tp)
	if errors.Is(err, os.ErrNotExist) {
		lp, lerr := legacyAgentTokenPath(account, agent)
		if lerr == nil {
			if lb, lerr := os.ReadFile(lp); lerr == nil {
				tok := strings.TrimSpace(string(lb))
				if tok == "" {
					return "", "", fmt.Errorf("agent token file %s is empty", lp)
				}
				return tok, lp, nil
			}
		}
	}
	if err != nil {
		return "", "", fmt.Errorf("agent token %s: %w", tp, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", "", fmt.Errorf("agent token file %s is empty", tp)
	}
	return tok, tp, nil
}

// legacyTokenPath is the flat pre-directory layout, still honored on reads.
func legacyTokenPath(name string) (string, error) {
	r, err := root()
	if err != nil {
		return "", err
	}
	return filepath.Join(r, "tokens", "accounts", name+".token"), nil
}

// readTokenFile reads the named account's token, preferring the per-account
// directory and falling back to the legacy flat file.
func readTokenFile(name string) (string, string, error) {
	tp, err := TokenPath(name)
	if err != nil {
		return "", "", err
	}
	b, err := os.ReadFile(tp)
	if errors.Is(err, os.ErrNotExist) {
		if lp, lerr := legacyTokenPath(name); lerr == nil {
			if lb, lerr := os.ReadFile(lp); lerr == nil {
				return string(lb), lp, nil
			}
		}
	}
	if err != nil {
		return "", "", err
	}
	return string(b), tp, nil
}

// Load reads the config; a missing file is an empty config.
func Load() (*Config, error) {
	p, err := configPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{Accounts: map[string]Account{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	if cfg.Accounts == nil {
		cfg.Accounts = map[string]Account{}
	}
	return &cfg, nil
}

func write(cfg *Config) error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0o600)
}

// Available returns an error if the name is invalid or already taken.
func Available(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("invalid local account name %q (lowercase letters, digits, hyphens)", name)
	}
	cfg, err := Load()
	if err != nil {
		return err
	}
	if _, ok := cfg.Accounts[name]; ok {
		return fmt.Errorf("%w: %q — pass --name to pick another", ErrNameTaken, name)
	}
	tp, err := TokenPath(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(tp); err == nil {
		return fmt.Errorf("%w: token file %s", ErrNameTaken, tp)
	}
	if lp, err := legacyTokenPath(name); err == nil {
		if _, err := os.Stat(lp); err == nil {
			return fmt.Errorf("%w: token file %s", ErrNameTaken, lp)
		}
	}
	return nil
}

// Save records a new named account: token first (the secret), then the binding.
func Save(name string, acct Account, operatorToken string) error {
	if err := Available(name); err != nil {
		return err
	}
	tp, err := TokenPath(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(tp), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(tp, []byte(operatorToken+"\n"), 0o600); err != nil {
		return err
	}
	cfg, err := Load()
	if err != nil {
		return err
	}
	cfg.Accounts[name] = acct
	return write(cfg)
}

// RefreshToken replaces the stored token for an EXISTING binding — the
// recovery write path. Save refuses taken names; this is its counterpart.
func RefreshToken(name, operatorToken string) error {
	cfg, err := Load()
	if err != nil {
		return err
	}
	if _, ok := cfg.Accounts[name]; !ok {
		return fmt.Errorf("no local account named %q", name)
	}
	tp, err := TokenPath(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(tp), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(tp, []byte(operatorToken+"\n"), 0o600); err != nil {
		return err
	}
	// A legacy flat file would now be stale; reads prefer the new path, but
	// clean it up anyway.
	if lp, err := legacyTokenPath(name); err == nil {
		_ = os.Remove(lp)
	}
	return nil
}

// SetEmail updates an existing binding's contact email — the local mirror of
// a server-side email change.
func SetEmail(name, email string) error {
	cfg, err := Load()
	if err != nil {
		return err
	}
	a, ok := cfg.Accounts[name]
	if !ok {
		return fmt.Errorf("no local account named %q", name)
	}
	a.Email = email
	cfg.Accounts[name] = a
	return write(cfg)
}

// Delete removes a named account's binding and token (used when it closes).
func Delete(name string) error {
	cfg, err := Load()
	if err != nil {
		return err
	}
	delete(cfg.Accounts, name)
	if err := write(cfg); err != nil {
		return err
	}
	tp, err := TokenPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(tp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = os.Remove(filepath.Dir(tp)) // the account's dir, if now empty
	if lp, err := legacyTokenPath(name); err == nil {
		if err := os.Remove(lp); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// ResolveAccount picks a named account — explicit flag, then WITSELF_ACCOUNT,
// then "default" — and returns its non-secret binding.
func ResolveAccount(explicit string) (name string, acct Account, err error) {
	name = explicit
	if name == "" {
		name = strings.TrimSpace(os.Getenv("WITSELF_ACCOUNT"))
	}
	if name == "" {
		name = "default"
	}
	cfg, err := Load()
	if err != nil {
		return "", Account{}, err
	}
	a, ok := cfg.Accounts[name]
	if !ok {
		return "", Account{}, fmt.Errorf("no local account named %q (create one with `witself account create --name %s …`, or pass --endpoint/--token-file)", name, name)
	}
	return name, a, nil
}

// Resolve returns a named account binding plus its owner/operator token.
func Resolve(explicit string) (name string, acct Account, operatorToken string, err error) {
	name, a, err := ResolveAccount(explicit)
	if err != nil {
		return "", Account{}, "", err
	}
	raw, tp, err := readTokenFile(name)
	if err != nil {
		return "", Account{}, "", fmt.Errorf("account %q token: %w", name, err)
	}
	tok := strings.TrimSpace(raw)
	if tok == "" {
		return "", Account{}, "", fmt.Errorf("account %q token file %s is empty", name, tp)
	}
	return name, a, tok, nil
}
