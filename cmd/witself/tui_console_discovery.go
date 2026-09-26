package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/agenttui"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/dashboard"
)

func normalizeConsoleEndpoint(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", errConsoleUnavailable
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

func consoleIdentityMatches(a, b client.SelfIdentity) bool {
	return a.AccountID != "" && a.RealmID != "" && a.AgentID != "" && a.AccountID == b.AccountID && a.RealmID == b.RealmID && a.AgentID == b.AgentID
}

// Local discovery traffic uses constructed loopback URLs, never redirects or
// proxy environment settings. Each client has a private, promptly closed pool.
func consoleHTTPClient() (*http.Client, func()) {
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext, ResponseHeaderTimeout: 2 * time.Second}
	return &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, transport.CloseIdleConnections
}

func (c *tuiConsole) validEntry(entry dashboard.RegistryEntry) bool {
	if entry.SchemaVersion != dashboard.RegistrySchemaVersion || entry.AgentID != c.identity.AgentID || entry.PID <= 1 || entry.PID == os.Getpid() || uint64(entry.PID) > uint64(^uint32(0)) || entry.Port < 1 || entry.Port > 65535 || entry.StartedAt.IsZero() {
		return false
	}
	base := fmt.Sprintf("http://127.0.0.1:%d/", entry.Port)
	if entry.URL != base || !strings.HasPrefix(entry.AccessURL, base+"?token=") {
		return false
	}
	token := strings.TrimPrefix(entry.AccessURL, base+"?token=")
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 16 || token != strings.ToLower(token) {
		return false
	}
	if entry.AccountID != "" && entry.AccountID != c.identity.AccountID {
		return false
	}
	if entry.RealmID != "" && entry.RealmID != c.identity.RealmID {
		return false
	}
	if entry.Endpoint != "" {
		actual, err := normalizeConsoleEndpoint(entry.Endpoint)
		expected, expectedErr := normalizeConsoleEndpoint(c.conn.Endpoint)
		if err != nil || expectedErr != nil || actual != expected {
			return false
		}
	}
	return true
}

func (c *tuiConsole) discover(ctx context.Context) (agenttui.ConsoleStatus, dashboard.RegistryEntry, error) {
	empty := dashboard.RegistryEntry{}
	if !accountConsoleManagerMatches(c.conn, c.identity, c.manager) || !consoleIdentityMatches(c.identity, c.identity) || c.conn.Token == "" || (c.conn.AccountID != "" && c.conn.AccountID != c.identity.AccountID) {
		return consoleUnavailable(), empty, errConsoleUnavailable
	}
	if _, err := normalizeConsoleEndpoint(c.conn.Endpoint); err != nil {
		return consoleUnavailable(), empty, errConsoleUnavailable
	}
	if ctx.Err() != nil {
		return consoleUnavailable(), empty, errConsoleCanceled
	}
	entry, err := dashboard.ReadRegistryInstance(c.identity.AgentID)
	if errors.Is(err, os.ErrNotExist) {
		return agenttui.ConsoleStatus{State: agenttui.ConsoleStopped}, empty, nil
	}
	conflict := agenttui.ConsoleStatus{State: agenttui.ConsoleConflict}
	if err != nil || !c.validEntry(entry) {
		return conflict, empty, errConsoleConflict
	}
	if consolePIDDead(entry.PID) {
		removed, err := dashboard.WithRegistryInstance(ctx, entry, func() error {
			// Recheck after waiting for the claim lock: a reused PID is not dead.
			if !consolePIDDead(entry.PID) {
				return errConsoleConflict
			}
			return dashboard.RemoveRegistryEntry(entry.AgentID)
		})
		if err == nil && removed {
			return agenttui.ConsoleStatus{State: agenttui.ConsoleStopped}, empty, nil
		}
		return conflict, empty, errConsoleConflict
	}
	if !dashboard.RegistryPIDRunning(entry.PID) || !verifyConsoleEntry(ctx, entry, c.identity, c.manager, c.authority) {
		if ctx.Err() != nil {
			return consoleUnavailable(), empty, errConsoleCanceled
		}
		return conflict, empty, errConsoleConflict
	}
	current, err := dashboard.ReadRegistryInstance(c.identity.AgentID)
	if err != nil || !dashboard.SameRegistryInstance(current, entry) {
		return conflict, empty, errConsoleConflict
	}
	return agenttui.ConsoleStatus{State: agenttui.ConsoleRunning, Owned: c.owns(entry), Port: entry.Port}, entry, nil
}

func verifyConsoleEntry(ctx context.Context, entry dashboard.RegistryEntry, expected client.SelfIdentity, manager *dashboard.AccountManager, authority func(context.Context, dashboard.AccountManagerIdentity) error) bool {
	probe, closeProbe := consoleHTTPClient()
	defer closeProbe()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, entry.AccessURL, nil)
	if err != nil {
		return false
	}
	response, err := probe.Do(request)
	if err != nil {
		return false
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get(dashboard.MarkerHeader) != dashboard.RegistrySchemaVersion || response.Header.Get("Location") != "/" {
		return false
	}
	cookies := response.Cookies()
	if len(cookies) != 1 || cookies[0].Value == "" {
		return false
	}
	request, err = http.NewRequestWithContext(ctx, http.MethodGet, entry.URL+"api/self", nil)
	if err != nil {
		return false
	}
	request.AddCookie(cookies[0])
	response, err = probe.Do(request)
	if err != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || response.Header.Get(dashboard.MarkerHeader) != dashboard.RegistrySchemaVersion {
		return false
	}
	identity, err := decodeConsoleIdentity(response.Body)
	if err != nil || !consoleIdentityMatches(expected, identity) {
		return false
	}
	// Registry metadata can refuse reuse, but cannot authorize it or hide the
	// immutable live session binding. Even available:false retains that binding.
	request, err = http.NewRequestWithContext(ctx, http.MethodGet, entry.URL+"api/console/viewer", nil)
	if err != nil {
		return false
	}
	request.AddCookie(cookies[0])
	viewerResponse, err := probe.Do(request)
	if err != nil {
		return false
	}
	defer func() { _ = viewerResponse.Body.Close() }()
	if viewerResponse.Header.Get(dashboard.MarkerHeader) != dashboard.RegistrySchemaVersion {
		return false
	}
	if viewerResponse.StatusCode == http.StatusNotFound {
		return manager == nil && entry.Manager == nil && entry.ViewerContract == ""
	}
	if viewerResponse.StatusCode != http.StatusOK {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(viewerResponse.Body, 4097))
	if err != nil || len(raw) > 4096 {
		return false
	}
	var viewer dashboard.ViewerContext
	if json.Unmarshal(raw, &viewer) != nil || viewer.SchemaVersion != dashboard.ViewerSchema ||
		viewer.AccountID != expected.AccountID || viewer.RealmID != expected.RealmID || viewer.AgentID != expected.AgentID {
		return false
	}
	if entry.ViewerContract != "" && entry.ViewerContract != dashboard.ViewerSchema {
		return false
	}
	if manager == nil {
		return viewer.Manager == nil && entry.Manager == nil && !viewer.Available
	}
	if viewer.Manager == nil || !viewer.Available {
		return false
	}
	binding := dashboard.ViewerBinding{AccountID: manager.Identity.AccountID, OperatorID: manager.Identity.OperatorID, Role: manager.Identity.Role}
	if *viewer.Manager != binding || (entry.Manager != nil && *entry.Manager != binding) {
		return false
	}
	// Verify the caller's private authority separately from the serving session.
	if authority != nil {
		return authority(ctx, manager.Identity) == nil
	}
	_, err = client.RevalidateAccountConsoleManager(ctx, *manager)
	return err == nil
}

func decodeConsoleIdentity(reader io.Reader) (client.SelfIdentity, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, 128*1024+1))
	if err != nil || len(raw) > 128*1024 {
		return client.SelfIdentity{}, errConsoleUnavailable
	}
	var envelope struct {
		Identity client.SelfIdentity `json:"identity"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return client.SelfIdentity{}, errConsoleUnavailable
	}
	return envelope.Identity, nil
}
