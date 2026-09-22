package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/dashboard"
	"github.com/witwave-ai/witself/internal/version"
)

const consoleBootstrapLimit = 64 * 1024

type tuiConsoleBootstrap struct {
	AccessToken string              `json:"access_token"`
	Connection  agentConnection     `json:"connection"`
	Identity    client.SelfIdentity `json:"identity"`
	Poll        time.Duration       `json:"poll"`
}

func (c *tuiConsole) spawn(ctx context.Context) (*tuiConsoleProcess, <-chan bool, error) {
	accessToken, err := newDashboardAccessToken()
	if err != nil {
		return nil, nil, errConsoleStart
	}
	raw, err := json.Marshal(tuiConsoleBootstrap{Connection: c.conn, Identity: c.identity, Poll: c.poll, AccessToken: accessToken})
	if err != nil || len(raw)+1 > consoleBootstrapLimit {
		return nil, nil, errConsoleStart
	}
	command, err := c.command()
	if err != nil {
		return nil, nil, errConsoleStart
	}
	input, parentPipe, err := os.Pipe()
	if err != nil {
		return nil, nil, errConsoleStart
	}
	output, childOutput, err := os.Pipe()
	if err != nil {
		_ = input.Close()
		_ = parentPipe.Close()
		return nil, nil, errConsoleStart
	}
	command.Stdin, command.Stdout, command.Stderr = input, childOutput, io.Discard
	command.WaitDelay = time.Second
	// Credentials are carried only by this private pipe. Do not inherit token
	// environment variables or account selectors into the managed subprocess.
	command.Env = consoleChildEnvironment()
	if ctx.Err() != nil || c.parent.Err() != nil {
		err = errConsoleCanceled
	} else {
		err = command.Start()
	}
	_ = input.Close()
	_ = childOutput.Close()
	if err != nil {
		_ = parentPipe.Close()
		_ = output.Close()
		return nil, nil, errConsoleStart
	}
	process := &tuiConsoleProcess{cmd: command, pipe: parentPipe, done: make(chan struct{}), accessToken: accessToken}
	go func() { _ = command.Wait(); close(process.done) }()
	ready := make(chan bool, 1)
	go func() {
		defer func() { _ = output.Close() }()
		if _, err := parentPipe.Write(append(raw, '\n')); err != nil {
			ready <- false
			return
		}
		var response [6]byte
		_, err := io.ReadFull(output, response[:])
		ready <- err == nil && string(response[:]) == "ready\n"
	}()
	return process, ready, nil
}

func consoleChildEnvironment() []string {
	// Runtime/path/home values only. The resolved account and bearer never use
	// env/argv/files. Windows needs SystemRoot for ordinary native networking.
	keys := []string{"HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "WITSELF_HOME", "DSH_HOME", "TMPDIR", "TMP", "TEMP", "PATH", "SystemRoot", "SYSTEMROOT", "WINDIR"}
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// runTUIConsoleChild is reached before CLI startup migrations/credential
// resolution. Bootstrap and readiness are private, bounded, and never logged.
func runTUIConsoleChild(input io.Reader, output io.Writer) int {
	parent, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	reader := bufio.NewReaderSize(input, consoleBootstrapLimit)
	boot := make(chan []byte, 1)
	go func() {
		raw, err := reader.ReadSlice('\n')
		if err != nil || len(raw) > consoleBootstrapLimit {
			boot <- nil
			return
		}
		boot <- bytes.Clone(raw)
		// EOF (including a killed parent's closed fd) cancels the entire child.
		// Any extra input is a protocol failure and has the same result.
		_, _ = reader.ReadByte()
		cancel()
	}()
	var raw []byte
	select {
	case raw = <-boot:
	case <-time.After(consoleStartupTimeout):
		return 1
	case <-ctx.Done():
		return 1
	}
	var bootstrap tuiConsoleBootstrap
	if len(raw) == 0 || json.Unmarshal(raw, &bootstrap) != nil {
		return 1
	}
	endpoint, err := normalizeConsoleEndpoint(bootstrap.Connection.Endpoint)
	if err != nil || bootstrap.Connection.Token == "" || !consoleIdentityMatches(bootstrap.Identity, bootstrap.Identity) {
		return 1
	}
	if bootstrap.Connection.AccountID != "" && bootstrap.Connection.AccountID != bootstrap.Identity.AccountID {
		return 1
	}
	if bootstrap.Poll < time.Second || bootstrap.Poll > time.Minute {
		return 1
	}
	bootstrap.Connection.Endpoint = endpoint
	startup, finish := context.WithTimeout(ctx, 8*time.Second)
	err = verifyConsoleChildIdentity(startup, bootstrap)
	finish()
	if err != nil || ctx.Err() != nil {
		return 1
	}
	token := bootstrap.AccessToken
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 16 || token != strings.ToLower(token) {
		return 1
	}
	listener, err := listenDashboard(0, dashboard.DefaultPort(bootstrap.Identity.AgentID))
	if err != nil {
		return 1
	}
	cfg := dashboard.Config{Endpoint: endpoint, BearerToken: bootstrap.Connection.Token, AccessToken: token, Identity: bootstrap.Identity, Version: version.Version, PollInterval: bootstrap.Poll}
	entry := dashboard.RegistryEntry{AgentID: bootstrap.Identity.AgentID, AgentName: bootstrap.Identity.AgentName, Account: bootstrap.Connection.AccountName, Realm: bootstrap.Identity.RealmName}
	return serveDashboardLifecycle(ctx, listener, cfg, entry, false, io.Discard, func() error { _, err := io.WriteString(output, "ready\n"); return err })
}

func verifyConsoleChildIdentity(ctx context.Context, bootstrap tuiConsoleBootstrap) error {
	query := url.Values{"observational": {"true"}, "max_bytes": {"1024"}}
	for _, key := range []string{"include_facts", "include_salient", "include_sensitive", "include_counts", "include_checkpoint", "include_message_checkpoint", "include_email_checkpoint", "include_avatar_checkpoint", "include_plan_entitlements"} {
		query.Set(key, "false")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, bootstrap.Connection.Endpoint+"/v1/self?"+query.Encode(), nil)
	if err != nil {
		return errConsoleStart
	}
	request.Header.Set("Authorization", "Bearer "+bootstrap.Connection.Token)
	probe, closeProbe := consoleHTTPClient()
	defer closeProbe()
	response, err := probe.Do(request)
	if err != nil {
		return errConsoleStart
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return errConsoleStart
	}
	identity, err := decodeConsoleIdentity(response.Body)
	if err != nil || !consoleIdentityMatches(bootstrap.Identity, identity) {
		return errConsoleStart
	}
	return nil
}
