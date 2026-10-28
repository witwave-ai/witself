package localcell

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"github.com/witwave-ai/witself/internal/store"
)

var errServiceHealth = errors.New("service_authenticated_health_failed")

type serviceHealthCredential struct {
	InstallationID string `json:"installation_id"`
	AccountID      string `json:"account_id"`
	OperatorID     string `json:"operator_id"`
	Bearer         string `json:"bearer"`
}

// ServiceHealth requires the existing native process fences, exact database
// schema/account readiness, and an authenticated loopback API identity response.
// No credential or underlying connection error is returned to the caller.
func (c *Cell) ServiceHealth(ctx context.Context) error {
	ctx, cancel := c.deadline(ctx)
	defer cancel()
	lock, err := c.own(false)
	if errors.Is(err, ErrLocked) {
		return err
	}
	if err != nil {
		return errServiceHealth
	}
	defer func() { _ = lock.Close() }()
	d, err := c.ServiceDescriptor()
	if err != nil || d.DesiredState != "running" || d.Faulted {
		return errServiceHealth
	}
	if err := c.serviceStartFence(); err != nil {
		return errServiceHealth
	}
	if err := c.serviceLiveTree(); err != nil {
		return errServiceHealth
	}
	m, err := readManifest(c.cfg.Root)
	if err != nil || m.AccountID == "" {
		return errServiceHealth
	}
	password, err := c.password()
	if err != nil {
		return errServiceHealth
	}
	dsn := url.URL{Scheme: "postgres", Host: addr(m.Ports.Postgres), Path: "/" + m.Database, User: url.UserPassword(m.Role, password), RawQuery: "sslmode=disable"}
	st, err := store.Open(ctx, dsn.String())
	if err != nil {
		return errServiceHealth
	}
	defer st.Close()
	if err := runtimeconfig.CheckReady(ctx, st, store.SchemaVersion(), m.AccountID); err != nil {
		return errServiceHealth
	}
	credential, err := c.serviceAPICredential(ctx, st, m)
	if err != nil {
		return errServiceHealth
	}
	client := localHTTPClient()
	defer client.CloseIdleConnections()
	if err := serviceAuthenticatedProbe(ctx, client, "http://"+addr(m.Ports.ServerAPI), credential); err != nil {
		return err
	}
	if !ready(ctx, client, m.Ports.ServerHealth) || !ready(ctx, client, m.Ports.WorkerHealth) {
		return errServiceHealth
	}
	return c.serviceLiveTree()
}

func (c *Cell) serviceLiveTree() error {
	if err := c.verifyAll(); err != nil {
		return errServiceHealth
	}
	for _, component := range []string{"postgres", "server", "worker"} {
		s, err := c.processState(component)
		if err != nil || !s.Alive || !s.Matches {
			return errServiceHealth
		}
	}
	return nil
}

// The current API has no health-only token profile. Use the existing operator
// credential contract, keep the dedicated credential private, and only issue
// read-only whoami requests. Scope reduction belongs to the future role split.
func (c *Cell) serviceAPICredential(ctx context.Context, st *store.Store, m *Manifest) (serviceHealthCredential, error) {
	var credential serviceHealthCredential
	path := c.path("service", "api.secret")
	err := servicePrivateRecord(path)
	if err == nil {
		if err := readJSON(path, &credential); err != nil {
			return credential, errServiceHealth
		}
		if credential.InstallationID != m.InstallationID || credential.AccountID != m.AccountID || credential.OperatorID == "" || credential.Bearer == "" {
			return credential, errServiceHealth
		}
		return credential, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return credential, errServiceHealth
	}
	owner, err := st.EnsureRootOperator(ctx, m.AccountID)
	if err != nil {
		return credential, errServiceHealth
	}
	bearer, tokenID, _, err := st.CreateOperatorToken(ctx, m.AccountID, owner, "local service health", nil)
	if err != nil {
		return credential, errServiceHealth
	}
	credential = serviceHealthCredential{m.InstallationID, m.AccountID, owner, bearer}
	if err := writeAtomic(path, credential, nil); err != nil {
		_ = st.RevokeToken(ctx, m.AccountID, owner, tokenID)
		return serviceHealthCredential{}, errServiceHealth
	}
	return credential, nil
}

func serviceAuthenticatedProbe(ctx context.Context, client *http.Client, endpoint string, credential serviceHealthCredential) error {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return errServiceHealth
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/auth/whoami?challenge="+base64.RawURLEncoding.EncodeToString(nonce[:]), nil)
	if err != nil {
		return errServiceHealth
	}
	req.Header.Set("Authorization", "Bearer "+credential.Bearer)
	req.Header.Set("Cache-Control", "no-store")
	res, err := client.Do(req)
	if err != nil {
		return errServiceHealth
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return errServiceHealth
	}
	var result struct {
		Principal struct {
			Kind       string `json:"kind"`
			AccountID  string `json:"account_id"`
			OperatorID string `json:"operator_id"`
		} `json:"principal"`
	}
	decoder := json.NewDecoder(io.LimitReader(res.Body, 8192))
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || result.Principal.Kind != "operator" || result.Principal.AccountID != credential.AccountID || result.Principal.OperatorID != credential.OperatorID {
		return errServiceHealth
	}
	return nil
}
