package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/clientinventory"
	"github.com/witwave-ai/witself/internal/dashboard"
	"github.com/witwave-ai/witself/internal/local"
)

// These seams keep directory and inventory tests entirely synthetic.
var accountConsoleConnect = connectAgent
var accountConsoleResolve = local.Resolve
var accountConsoleLocate accountLocator = client.LookupAccount
var accountConsoleScan = clientinventory.Scan

// Snapshot before secret-vault selectors can fill AccountName. Even an explicitly
// supplied empty endpoint/token-file flag selects an agent-only console.
func accountConsoleExplicit(fs *flag.FlagSet) bool {
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "endpoint" || f.Name == "token-file" {
			explicit = true
		}
	})
	return explicit
}

func resolveAccountConsoleManager(ctx context.Context, conn agentConnection, identity client.SelfIdentity, explicit bool, locate accountLocator) *dashboard.AccountManager {
	if explicit || conn.AccountName == "" || conn.AccountID == "" || conn.AccountID != identity.AccountID || !consoleIdentityMatches(identity, identity) || locate == nil {
		return nil
	}
	name, metadata, err := local.ResolveAccount(conn.AccountName)
	if err != nil || name != conn.AccountName || metadata.ID != identity.AccountID {
		return nil
	}
	_, directory, err := locate(ctx, defaultControlPlane, metadata.ID)
	if err != nil {
		return nil
	}
	trusted, e1 := client.AccountConsoleOrigin(directory)
	selected, e2 := client.AccountConsoleOrigin(conn.Endpoint)
	if e1 != nil || e2 != nil || trusted != selected {
		return nil
	}
	// Only now may the exact selected account's bearer be read.
	resolved, account, bearer, err := accountConsoleResolve(name)
	if err != nil || resolved != name || account.ID != metadata.ID || bearer == conn.Token {
		return nil
	}
	principal, err := client.VerifyAccountConsoleManager(ctx, trusted, bearer, identity.AccountID)
	if err != nil {
		return nil
	}
	return &dashboard.AccountManager{Endpoint: trusted, BearerToken: bearer, Identity: principal}
}

// Private roots are captured once, not resolved from ambient state by the child
// or scan callback. They are never included in a console projection.
type accountConsoleRoots struct {
	Home        string `json:"home"`
	WitselfHome string `json:"witself_home"`
	DSHHome     string `json:"dsh_home"`
}

func accountConsoleLocalRoots() accountConsoleRoots {
	home, err := os.UserHomeDir()
	if err != nil {
		return accountConsoleRoots{}
	}
	home = accountConsoleAbsoluteRoot(home)
	if home == "" {
		return accountConsoleRoots{}
	}
	// local.Home preserves overrides literally, including spaces and tilde.
	witself := os.Getenv("WITSELF_HOME")
	if witself == "" {
		witself = filepath.Join(home, ".witself")
	}
	// Match currentDSHConfigRoot's input semantics using this captured HOME.
	// Do not call its cleaner: it inspects disk and resolves symlinks.
	dsh := strings.TrimSpace(os.Getenv("DSH_HOME"))
	if dsh == "" {
		dsh = filepath.Join(home, ".dsh")
	} else if dsh == "~" {
		dsh = home
	} else if strings.HasPrefix(dsh, "~/") || strings.HasPrefix(dsh, "~\\") {
		dsh = filepath.Join(home, dsh[2:])
	}
	return accountConsoleRoots{home, accountConsoleAbsoluteRoot(witself), accountConsoleAbsoluteRoot(dsh)}
}

// Empty/invalid roots stay invalid, never filepath.Abs("") or a default root.
// This is lexical only; filesystem custody remains the inventory's concern.
func accountConsoleAbsoluteRoot(root string) string {
	if root == "" || len(root) > 4096 || strings.ContainsAny(root, "\x00\r\n") {
		return ""
	}
	absolute, err := filepath.Abs(root)
	if err != nil || !accountConsoleValidRoot(absolute) {
		return ""
	}
	return absolute
}

func accountConsoleValidRoot(root string) bool {
	return root != "" && len(root) <= 4096 && filepath.IsAbs(root) &&
		filepath.Clean(root) == root && !strings.ContainsAny(root, "\x00\r\n")
}
func (r accountConsoleRoots) valid() bool {
	return accountConsoleValidRoot(r.Home) && accountConsoleValidRoot(r.WitselfHome) && accountConsoleValidRoot(r.DSHHome)
}
func newAccountConsoleScanner(parent context.Context, roots accountConsoleRoots, manager *dashboard.AccountManager) dashboard.AccountClientsScan {
	if manager == nil || !roots.valid() {
		return nil
	}
	accountID := manager.Identity.AccountID
	scan := accountConsoleScan
	return func(ctx context.Context, requested string) (dashboard.AccountClientsReport, error) {
		if requested == "" || requested != accountID {
			return dashboard.AccountClientsReport{}, client.ErrAccountConsoleForbidden
		}
		ctx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(parent, cancel)
		defer func() { stop(); cancel() }()
		if parent.Err() != nil {
			cancel()
		}
		report, err := scan(ctx, clientinventory.Options{Home: roots.Home, WitselfHome: roots.WitselfHome, DSHHome: roots.DSHHome, AccountID: accountID})
		if err != nil || ctx.Err() != nil {
			return dashboard.AccountClientsReport{}, client.ErrAccountConsoleUnavailable
		}
		out := dashboard.AccountClientsReport{SchemaVersion: report.SchemaVersion, DeviceLabel: report.DeviceLabel, CheckedAt: report.CheckedAt.UTC().Format(time.RFC3339Nano), ScanStatus: string(report.ScanStatus), Entries: []dashboard.AccountClientEntry{}}
		for _, row := range report.Entries {
			entry := dashboard.AccountClientEntry{Runtime: string(row.Runtime), RecordedVersion: row.RecordedVersion, ExecutableStatus: string(row.ExecutableStatus), ConfigurationStatus: string(row.ConfigurationStatus), ConfigurationScope: string(row.ConfigurationScope), EffectiveVerification: string(row.EffectiveVerification)}
			if row.InstalledAt != nil {
				entry.InstalledAt = row.InstalledAt.UTC().Format(time.RFC3339Nano)
			}
			out.Entries = append(out.Entries, entry)
		}
		return out, nil
	}
}

type accountConsoleOptions struct {
	Manager *dashboard.AccountManager
	Roots   accountConsoleRoots
}

func accountConsoleManagerMatches(conn agentConnection, identity client.SelfIdentity, manager *dashboard.AccountManager) bool {
	if manager == nil {
		return true
	}
	a, e1 := client.AccountConsoleOrigin(conn.Endpoint)
	b, e2 := client.AccountConsoleOrigin(manager.Endpoint)
	return e1 == nil && e2 == nil && a == b && manager.Identity.AccountID == identity.AccountID &&
		manager.BearerToken != "" && manager.BearerToken != conn.Token && client.AccountConsoleRole(manager.Identity.Role)
}

// Late publication is captured only under the original private bootstrap
// binding. A modified manager fence must survive the old child's cleanup.
func consoleRegistryManagerMatches(entry dashboard.RegistryEntry, manager *dashboard.AccountManager) bool {
	if manager == nil {
		return entry.Manager == nil
	}
	return entry.ViewerContract == dashboard.ViewerSchema && entry.Manager != nil &&
		*entry.Manager == (dashboard.ViewerBinding{AccountID: manager.Identity.AccountID, OperatorID: manager.Identity.OperatorID, Role: manager.Identity.Role})
}
