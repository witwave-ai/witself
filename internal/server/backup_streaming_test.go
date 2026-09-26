package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestAccountBackupFlushReachesHTTPBeforeCompletion(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	srv := httptest.NewServer(apiMux(Config{
		BackupToken: "witself_bkp_test",
		StreamAccountBackup: func(ctx context.Context, _, _ string, w io.Writer, flush func() error) error {
			if _, err := io.WriteString(w, "chunk"); err != nil {
				return err
			}
			if err := flush(); err != nil {
				return err
			}
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			_, err := io.WriteString(w, "end")
			return err
		},
	}))
	defer srv.Close()
	defer unblock()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/accounts/acc_backup:export-backup", nil)
	req.Header.Set("Authorization", "Bearer witself_bkp_test")
	req.Header.Set(AccountBackupIDHeader, "bkp_streaming_test")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	chunk := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, chunk); err != nil {
		t.Fatal(err)
	}
	if string(chunk) != "chunk" {
		t.Fatal("wrong first chunk")
	}
	unblock()
	tail, err := io.ReadAll(resp.Body)
	if err != nil || string(tail) != "end" {
		t.Fatalf("tail=%q error=%v", tail, err)
	}
}

type backupNoFlusher struct{ http.ResponseWriter }
type backupFailedFlusher struct {
	http.ResponseWriter
	err error
}

func (w backupFailedFlusher) FlushError() error { return w.err }

func TestAccountBackupFlushUnavailableOrFailed(t *testing.T) {
	sentinel := errors.New("flush failed")
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "unavailable", true: "failed"}[fail], func(t *testing.T) {
			var reported error
			handler := accountBackupHandler(Config{
				BackupToken: "witself_bkp_test",
				StreamAccountBackup: func(_ context.Context, _, _ string, w io.Writer, flush func() error) error {
					_, _ = io.WriteString(w, "chunk")
					return flush()
				},
				ReportAccountExportFailure: func(_ context.Context, _ string, err error) { reported = err },
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/accounts/acc_backup:export-backup", nil)
			req.Header.Set("Authorization", "Bearer witself_bkp_test")
			req.Header.Set(AccountBackupIDHeader, "bkp_streaming_test")
			recorder := httptest.NewRecorder()
			var w http.ResponseWriter = backupNoFlusher{recorder}
			if fail {
				w = backupFailedFlusher{recorder, sentinel}
			}
			handler(w, req)
			if fail && !errors.Is(reported, sentinel) {
				t.Fatalf("reported = %v", reported)
			}
			if !fail && reported != nil {
				t.Fatalf("unsupported flush: %v", reported)
			}
			if recorder.Body.String() != "chunk" {
				t.Fatalf("body=%q", recorder.Body.String())
			}
		})
	}
}
