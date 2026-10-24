package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/local"
)

func accountRouteCmd(args []string) int {
	const usage = "usage: witself account route show|clear NAME; witself account route set --local-endpoint URL NAME"
	if commandHelpRequested(args) {
		fmt.Fprintln(os.Stderr, usage)
		return 0
	}
	if len(args) == 0 || (args[0] != "show" && args[0] != "set" && args[0] != "clear") {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	verb := args[0]
	fs := flag.NewFlagSet("account route "+verb, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var endpoint string
	if verb == "set" {
		fs.StringVar(&endpoint, "local-endpoint", "", "HTTP endpoint with host 127.0.0.1 or [::1]")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 || fs.Arg(0) == "" || (verb == "set" && endpoint == "") {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	name, acct, err := local.ResolveAccount(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "witself: cannot resolve connection route account")
		return 1
	}
	if verb == "show" {
		return printJSON(acct.Route)
	}
	var route *local.Route
	if verb == "set" {
		route = &local.Route{Kind: "local", Endpoint: endpoint}
		if err := probeLocalConnectionRoute(context.Background(), *route); err != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
	}
	if err := local.SetRoute(name, route); err != nil {
		fmt.Fprintln(os.Stderr, "witself: cannot save connection route")
		return 1
	}
	if verb == "clear" {
		_, _ = fmt.Fprintln(os.Stdout, "connection route cleared")
	} else {
		_, _ = fmt.Fprintln(os.Stdout, "connection route set")
	}
	return 0
}

// Activation probes carry no credentials, cannot follow redirects, and never
// consult an ambient proxy. Error bodies and supplied URLs stay out of errors.
func probeLocalConnectionRoute(ctx context.Context, route local.Route) error {
	if err := route.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(route.Endpoint, "/")+"/v1/version", nil)
	if err != nil {
		return errors.New("local connection route version probe failed")
	}
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	hc := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := hc.Do(req)
	if err != nil {
		return errors.New("local connection route version probe failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return errors.New("local connection route version probe failed")
	}
	const maxVersionBytes = 64 << 10
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxVersionBytes+1))
	var version struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err != nil || len(body) > maxVersionBytes || json.Unmarshal(body, &version) != nil || version.SchemaVersion != "witself.v0" {
		return errors.New("local connection route version probe requires witself.v0")
	}
	return nil
}
