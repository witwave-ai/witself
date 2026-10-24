package local

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Route is an explicit connection route for a named account. Credentials stay
// in their existing files and are never part of the connection route.
type Route struct {
	Kind     string `json:"kind"`
	Endpoint string `json:"endpoint"`
}

// Validate accepts only local HTTP endpoints with the exact loopback literals.
// Errors deliberately omit configuration values.
func (r Route) Validate() error {
	if r.Kind != "local" {
		return errors.New("unsupported connection route kind")
	}
	u, err := url.Parse(r.Endpoint)
	if err != nil || u.Scheme != "http" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(r.Endpoint, "#") {
		return errors.New("local connection route requires an http loopback endpoint")
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "::1" {
		return errors.New("local connection route requires an http loopback endpoint")
	}
	baseHost := host
	if host == "::1" {
		baseHost = "[::1]"
	}
	if u.Host != baseHost {
		port, err := strconv.ParseUint(u.Port(), 10, 16)
		if err != nil || port == 0 || u.Host != baseHost+":"+u.Port() {
			return errors.New("local connection route requires an http loopback endpoint")
		}
	}
	if strings.TrimSpace(r.Endpoint) != r.Endpoint {
		return errors.New("local connection route requires an http loopback endpoint")
	}
	return nil
}

func validateRoutes(cfg *Config) error {
	for _, acct := range cfg.Accounts {
		if acct.Route != nil {
			if err := acct.Route.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

// SetRoute updates only an existing account's connection route. A nil route
// clears it, restoring directory resolution. The caller probes an endpoint
// before activating it; this storage layer validates the persisted shape.
func SetRoute(name string, route *Route) error {
	if route != nil {
		if err := route.Validate(); err != nil {
			return err
		}
	}
	cfg, err := Load()
	if err != nil {
		return err
	}
	acct, ok := cfg.Accounts[name]
	if !ok {
		return fmt.Errorf("no local account named %q", name)
	}
	acct.Route = route
	cfg.Accounts[name] = acct
	return write(cfg)
}
