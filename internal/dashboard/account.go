package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/witwave-ai/witself/internal/client"
)

// AccountSchema identifies the read-only account projection contract.
const AccountSchema = "witself.console.account.v1"

// AccountClientsSchema identifies the local client inventory report contract.
const AccountClientsSchema = "witself.console.clients.v1"

// AccountManager and AccountManagerIdentity are private process configuration
// and its verified public principal. No credential is serialized.
type AccountManager = client.AccountConsoleManager

// AccountManagerIdentity is the verified public account operator binding.
type AccountManagerIdentity = client.AccountConsoleIdentity

// AccountClientsScan is an explicit local metadata scan. It must honor ctx,
// perform no network/provider execution, and return only matching account
// records. Calls are serialized per Reader/Register collector; simultaneous
// scans fail unavailable, rather than queueing or sharing caller cancellation.
// Constructors and ordinary Clients reads never invoke it. The core copies and
// bounds the returned DTO before caching; the callback must not mutate it later.
type AccountClientsScan func(ctx context.Context, accountID string) (AccountClientsReport, error)

// AccountClientsReport contains metadata from an explicit local inventory scan.
type AccountClientsReport struct {
	SchemaVersion string               `json:"schema_version"`
	DeviceLabel   string               `json:"device_label"`
	CheckedAt     string               `json:"checked_at"`
	ScanStatus    string               `json:"scan_status"`
	Entries       []AccountClientEntry `json:"entries"`
	Truncated     bool                 `json:"truncated"`
}

// AccountClientEntry contains bounded metadata and check results for one runtime.
type AccountClientEntry struct {
	Runtime               string `json:"runtime"`
	RecordedVersion       string `json:"recorded_version"`
	ExecutableStatus      string `json:"executable_status"`
	ConfigurationStatus   string `json:"configuration_status"`
	ConfigurationScope    string `json:"configuration_scope"`
	EffectiveVerification string `json:"effective_verification"`
	InstalledAt           string `json:"installed_at,omitempty"`
}

var accountPaths = map[Resource]string{
	ResourceAccountContext: "/api/account/context", ResourceAccountOverview: "/api/account/overview",
	ResourceAccountPlan: "/api/account/plan", ResourceAccountBilling: "/api/account/billing",
	ResourceAccountSupport: "/api/account/support", ResourceAccountSupportTicket: "/api/account/support/",
	ResourceAccountAccess: "/api/account/access", ResourceAccountClients: "/api/account/clients",
	ResourceAccountClientsScan: "/api/account/clients/scan",
}

type accountCollector struct {
	manager       *AccountManager
	scan          AccountClientsScan
	scanGate      chan struct{}
	mu            sync.Mutex
	principal     AccountManagerIdentity
	epoch         uint64
	next, applied uint64
	cached        *AccountClientsReport
}

func newAccountCollector(cfg Config) *accountCollector {
	c := &accountCollector{scanGate: make(chan struct{}, 1)}
	if cfg.AccountManager == nil {
		return c
	}
	m := *cfg.AccountManager
	a, e1 := client.AccountConsoleOrigin(cfg.Endpoint)
	b, e2 := client.AccountConsoleOrigin(m.Endpoint)
	if e1 != nil || e2 != nil || a != b || cfg.Identity.AccountID == "" || cfg.Identity.AgentID == "" ||
		cfg.Identity.AccountID != m.Identity.AccountID || m.BearerToken == "" || m.BearerToken == cfg.BearerToken ||
		!client.AccountConsoleRole(m.Identity.Role) || !readerID(m.Identity.OperatorID) {
		return c
	}
	m.Endpoint = b
	c.manager, c.scan = &m, cfg.AccountClientsScan
	return c
}

// authority never holds the cache lock across I/O. Superseded checks retry
// within the caller deadline when authority is unchanged; failures and role
// changes still invalidate cached work and reject stale results.
func (c *accountCollector) authority(ctx context.Context) (AccountManagerIdentity, uint64, error) {
	if c.manager == nil {
		return AccountManagerIdentity{}, 0, client.ErrAccountConsoleForbidden
	}
	for {
		if ctx.Err() != nil {
			return AccountManagerIdentity{}, 0, client.ErrAccountConsoleUnavailable
		}
		c.mu.Lock()
		c.next++
		seq := c.next
		c.mu.Unlock()
		p, err := client.RevalidateAccountConsoleManager(ctx, *c.manager)
		c.mu.Lock()
		// Transport cancellation belongs to this caller, not shared authority.
		// An actual denial still invalidates even if cancellation raced it.
		if (ctx.Err() != nil && errors.Is(err, client.ErrAccountConsoleUnavailable)) || errors.Is(err, client.ErrAccountConsoleResponseTooLarge) {
			c.mu.Unlock()
			return AccountManagerIdentity{}, 0, err
		}
		if seq < c.applied {
			if err == nil && p == c.principal {
				c.mu.Unlock()
				continue
			}
			if err == nil {
				// The newer validation already fenced the changed authority.
				// Reject this old success without overwriting the latest principal.
				epoch := c.epoch
				c.mu.Unlock()
				return AccountManagerIdentity{}, epoch, client.ErrAccountConsoleForbidden
			}
			// Even an older response cannot hide an observed authorization
			// failure or role change behind a newer successful check.
			c.epoch++
			c.cached = nil
			c.principal = AccountManagerIdentity{}
			epoch := c.epoch
			c.mu.Unlock()
			return AccountManagerIdentity{}, epoch, client.ErrAccountConsoleForbidden
		}
		c.applied = seq
		if err != nil {
			p = AccountManagerIdentity{}
		}
		if c.principal != p || err != nil {
			c.epoch++
			c.cached = nil
			c.principal = p
		}
		epoch := c.epoch
		c.mu.Unlock()
		return p, epoch, err
	}
}
func (c *accountCollector) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	c.cached = nil
	c.principal = AccountManagerIdentity{}
}
func accountSections(role string) []string {
	sections := []string{"overview", "clients"}
	if client.AccountConsoleBillingRole(role) {
		sections = append(sections, "plan", "billing")
	}
	return append(sections, "support", "access")
}
func accountContext(p AccountManagerIdentity) map[string]any {
	return map[string]any{"schema_version": AccountSchema, "available": true, "account_id": p.AccountID, "operator_id": p.OperatorID, "role": p.Role, "sections": accountSections(p.Role)}
}
func accountFailure(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": AccountSchema, "available": false, "error": code})
}
func accountHandler(c *accountCollector, resource Resource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// ServeMux also matches HEAD to GET; particularly never let HEAD scan.
		if r.Method != http.MethodGet {
			accountFailure(w, 405, "forbidden")
			return
		}
		id := r.PathValue("id")
		if r.URL.RawQuery != "" || (resource == ResourceAccountSupportTicket && !readerID(id)) || (resource != ResourceAccountSupportTicket && id != "") {
			accountFailure(w, 400, "forbidden")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), readerTimeout)
		defer cancel()
		p, epoch, err := c.authority(ctx)
		if resource == ResourceAccountContext {
			out := map[string]any{"schema_version": AccountSchema, "available": false}
			if err == nil {
				out = accountContext(p)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		if err != nil {
			if errors.Is(err, client.ErrAccountConsoleResponseTooLarge) {
				accountFailure(w, 502, "response_too_large")
				return
			}
			accountFailure(w, 403, "forbidden")
			return
		}
		if (resource == ResourceAccountBilling || resource == ResourceAccountPlan) && !client.AccountConsoleBillingRole(p.Role) {
			accountFailure(w, 403, "forbidden")
			return
		}
		if resource == ResourceAccountClientsScan && c.scan != nil {
			select {
			case c.scanGate <- struct{}{}:
				defer func() { <-c.scanGate }()
			default:
				accountFailure(w, 502, "unavailable")
				return
			}
		}
		out, err := c.collect(ctx, resource, id, p)
		if err != nil {
			if errors.Is(err, client.ErrAccountConsoleForbidden) {
				c.invalidate()
				accountFailure(w, 403, "forbidden")
			} else if errors.Is(err, client.ErrAccountConsoleResponseTooLarge) {
				accountFailure(w, 502, "response_too_large")
			} else {
				accountFailure(w, 502, "unavailable")
			}
			return
		}
		// Recheck after slow upstream reads/scans, and reject results crossing any
		// authority invalidation observed by another request.
		current, now, e := c.authority(ctx)
		if errors.Is(e, client.ErrAccountConsoleResponseTooLarge) {
			accountFailure(w, 502, "response_too_large")
			return
		}
		if e != nil || current != p || now != epoch || ctx.Err() != nil {
			accountFailure(w, 403, "forbidden")
			return
		}
		c.mu.Lock()
		if c.epoch != epoch {
			c.mu.Unlock()
			accountFailure(w, 403, "forbidden")
			return
		}
		if report, ok := out["report"].(*AccountClientsReport); ok && resource == ResourceAccountClientsScan {
			c.cached = report
		}
		c.mu.Unlock()
		out["schema_version"] = AccountSchema
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}
func (c *accountCollector) collect(ctx context.Context, resource Resource, id string, p AccountManagerIdentity) (map[string]any, error) {
	if resource == ResourceAccountAccess {
		return accountContext(p), nil
	}
	if resource == ResourceAccountClients || resource == ResourceAccountClientsScan {
		if resource == ResourceAccountClientsScan && c.scan != nil {
			report, err := c.scan(ctx, p.AccountID)
			if err != nil || ctx.Err() != nil {
				return nil, client.ErrAccountConsoleUnavailable
			}
			safe, err := sanitizeAccountClients(report)
			if err != nil {
				return nil, err
			}
			return map[string]any{"available": true, "status": "checked", "report": safe}, nil
		}
		c.mu.Lock()
		cached := c.cached
		c.mu.Unlock()
		if cached == nil {
			return map[string]any{"available": true, "status": "not_checked", "report": nil}, nil
		}
		return map[string]any{"available": true, "status": "checked", "report": cached}, nil
	}
	if resource == ResourceAccountBilling {
		return c.billing(ctx)
	}
	upstream := map[Resource]client.AccountConsoleResource{
		ResourceAccountOverview: client.AccountConsoleOverview, ResourceAccountPlan: client.AccountConsolePlan,
		ResourceAccountSupport: client.AccountConsoleSupport, ResourceAccountSupportTicket: client.AccountConsoleSupportTicket,
	}[resource]
	raw, err := client.ReadAccountConsole(ctx, *c.manager, upstream, id)
	if err != nil {
		return nil, err
	}
	return projectAccount(resource, raw, p.AccountID, id)
}
