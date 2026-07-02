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
//	~/.witself/config.json                    non-secret bindings, CLI-managed
//	~/.witself/tokens/accounts/<name>.token   the operator token, 0600
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

// TokenPath returns where the named account's operator token lives.
func TokenPath(name string) (string, error) {
	r, err := root()
	if err != nil {
		return "", err
	}
	return filepath.Join(r, "tokens", "accounts", name+".token"), nil
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
	return nil
}

// Resolve picks a named account — explicit flag, then WITSELF_ACCOUNT, then
// "default" — and returns its binding plus the operator token.
func Resolve(explicit string) (name string, acct Account, operatorToken string, err error) {
	name = explicit
	if name == "" {
		name = strings.TrimSpace(os.Getenv("WITSELF_ACCOUNT"))
	}
	if name == "" {
		name = "default"
	}
	cfg, err := Load()
	if err != nil {
		return "", Account{}, "", err
	}
	a, ok := cfg.Accounts[name]
	if !ok {
		return "", Account{}, "", fmt.Errorf("no local account named %q (create one with `ws account create --name %s …`, or pass --endpoint/--token-file)", name, name)
	}
	tp, err := TokenPath(name)
	if err != nil {
		return "", Account{}, "", err
	}
	b, err := os.ReadFile(tp)
	if err != nil {
		return "", Account{}, "", fmt.Errorf("account %q token: %w", name, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", Account{}, "", fmt.Errorf("account %q token file %s is empty", name, tp)
	}
	return name, a, tok, nil
}
