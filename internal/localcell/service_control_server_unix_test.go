//go:build darwin || linux

package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The response must be complete before its handler returns. The barrier is
// independent of diagnostics: successful stop responses must remain silent.
func TestServiceControlStopResponseSilentAndCompleteUnix(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "lc-response-")
	if err != nil {
		t.Fatal("cannot create private response fixture")
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	path := filepath.Join(root, "control.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal("cannot listen for private response fixture")
	}
	var diagnostic bytes.Buffer
	responseWritten, release := make(chan struct{}), make(chan struct{})
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeServiceControlResponse(w, "stop", ServiceStatus{DesiredState: "stopped"}, &diagnostic)
		close(responseWritten)
		<-release
	})}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	defer func() { close(release); _ = server.Close(); <-done }()
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localcell/stop", nil)
	if err != nil {
		t.Fatal("cannot construct private response request")
	}
	result := make(chan bool, 1)
	go func() {
		response, err := (&http.Client{Transport: transport}).Do(request)
		if err != nil {
			result <- false
			return
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
		_ = response.Body.Close()
		var status ServiceStatus
		result <- err == nil && response.StatusCode == http.StatusOK && json.Unmarshal(body, &status) == nil && status.DesiredState == "stopped"
	}()
	select {
	case <-responseWritten:
	case <-ctx.Done():
		t.Fatal("stop handler never completed its response write and flush")
	}
	if diagnostic.Len() != 0 {
		t.Fatal("successful stop response emitted host diagnostic bytes")
	}
	select {
	case complete := <-result:
		if !complete {
			t.Fatal("flushed stop response was incomplete before handler return")
		}
	case <-ctx.Done():
		t.Fatal("flushed stop response required handler return to finish its framing")
	}
}
