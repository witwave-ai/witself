// Command dashboard-stub-cell serves deterministic loopback-only fixtures for
// the cross-platform Agent Console release acceptance driver.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/dashboard/stubcell"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "stub-cell:", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:0", "loopback listen address")
	tokenFile := flag.String("token-file", "", "file containing the fixed bearer token")
	flag.Parse()
	if flag.NArg() != 0 || *tokenFile == "" {
		return errors.New("--token-file is required; positional arguments are unsupported")
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || host != "127.0.0.1" {
		return errors.New("--listen must use 127.0.0.1 and a port")
	}
	raw, err := os.ReadFile(*tokenFile)
	if err != nil {
		return errors.New("cannot read token file")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return errors.New("token file must contain one nonempty bearer token")
	}
	listener, err := net.Listen("tcp4", *listen)
	if err != nil {
		return errors.New("cannot bind loopback listener")
	}
	defer func() { _ = listener.Close() }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	server := &http.Server{Handler: stubcell.New(stubcell.Config{BearerToken: token}), ReadHeaderTimeout: 5 * time.Second}
	defer func() { _ = server.Close() }()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if _, err := fmt.Fprintln(os.Stdout, "stub-cell: http://"+listener.Addr().String()); err != nil {
		fmt.Fprintf(os.Stderr, "stub-cell: announce listener: %v\n", err)
		os.Exit(1)
	}
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.New("HTTP server failed")
	}
	return nil
}
