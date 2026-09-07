package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	archiveexport "github.com/witwave-ai/witself/internal/export"
)

// TestAccountSelfExportCancellationReleasesResources exercises the actual HTTP
// handler and its real spool with a synthetic stream. It proves request-context
// cancellation, not network-disconnect or PostgreSQL-cancellation behavior.
func TestAccountSelfExportCancellationReleasesResources(t *testing.T) {
	const accountID = "acc_cancel_export"
	const partial = "Harmless unfinished self export fixture."
	archive := selfExportTestArchive(t, accountID)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	firstCtx, cancelFirst := context.WithCancel(ctx)
	emergency := make(chan struct{})
	entered := make(chan selfExportCancellationObservation, 1)
	var calls atomic.Int32
	var filesMu sync.Mutex
	var files []*os.File
	var requests []selfExportCancellationRequest

	// Even a detached-request-context mutant must be able to exit its stream.
	// Join every started handler before inspecting or cleaning its exact files.
	// Keeping these *os.File values alive also prevents finalizers from hiding
	// an omitted production Close. No temp-directory glob or environment change.
	t.Cleanup(func() {
		cancelFirst()
		cancel()
		close(emergency)
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		for _, request := range requests {
			select {
			case <-request.done:
			case <-cleanupCtx.Done():
				t.Error("self export cancellation fixture did not drain")
				return
			}
		}
		filesMu.Lock()
		defer filesMu.Unlock()
		for _, file := range files {
			if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				t.Error("could not close exact self export fixture spool during cleanup")
			}
			if err := os.Remove(file.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Error("could not remove exact self export fixture spool during cleanup")
			}
		}
	})

	handler := apiMux(Config{
		Authenticate: func(_ context.Context, token string) (string, string, string, bool, error) {
			return "opr_cancel_export", accountID, "active", token == "operator-token", nil
		},
		StreamAccountSelf: func(snapshotCtx context.Context, gotAccount string, w io.Writer) error {
			call := calls.Add(1)
			file, ok := w.(*os.File)
			if !ok || file == nil || gotAccount != accountID {
				return errors.New("self export fixture received an unexpected stream target")
			}
			filesMu.Lock()
			files = append(files, file)
			filesMu.Unlock()
			payload := archive
			if call == 1 {
				payload = []byte(partial)
			}
			if n, err := w.Write(payload); err != nil {
				return err
			} else if n != len(payload) {
				return io.ErrShortWrite
			}
			if call != 1 {
				return nil
			}
			// Publish only after a successful nonempty write to the real spool.
			entered <- selfExportCancellationObservation{ctx: snapshotCtx, file: file}
			select {
			case <-snapshotCtx.Done():
				return snapshotCtx.Err()
			case <-emergency:
				return errors.New("self export fixture emergency release")
			}
		},
	})
	start := func(requestCtx context.Context) selfExportCancellationRequest {
		request := selfExportCancellationRequest{recorder: httptest.NewRecorder(), done: make(chan struct{})}
		requests = append(requests, request)
		req := httptest.NewRequestWithContext(requestCtx, http.MethodGet, "/v1/export", nil)
		req.Header.Set("Authorization", "Bearer operator-token")
		go func() {
			defer close(request.done)
			handler.ServeHTTP(request.recorder, req)
		}()
		return request
	}

	first := start(firstCtx)
	var observation selfExportCancellationObservation
	enterCtx, enterCancel := context.WithTimeout(ctx, 10*time.Second)
	select {
	case observation = <-entered:
	case <-first.done:
		enterCancel()
		t.Fatal("self export ended before the nonempty spool baseline")
	case <-enterCtx.Done():
		enterCancel()
		t.Fatal("self export did not reach the nonempty spool baseline")
	}
	enterCancel()
	if observation.ctx.Err() != nil || firstCtx.Err() != nil {
		t.Fatal("self export fixture was canceled before the overlap baseline")
	}
	info, err := observation.file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(partial)) {
		t.Fatal("self export did not create an open nonempty regular spool")
	}
	pathInfo, err := os.Stat(observation.file.Name())
	if err != nil || !os.SameFile(info, pathInfo) {
		t.Fatal("self export spool path did not name the captured open file")
	}
	spooled, err := os.ReadFile(observation.file.Name())
	if err != nil || string(spooled) != partial {
		t.Fatal("self export spool baseline did not preserve the harmless prefix")
	}

	overlap := start(ctx)
	waitSelfExportCancellationRequest(ctx, t, overlap, "overlap refusal")
	var refusal struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(overlap.recorder.Body.Bytes(), &refusal); err != nil ||
		overlap.recorder.Code != http.StatusConflict || refusal.Code != "account_export_in_progress" || calls.Load() != 1 {
		t.Fatal("self export did not hold the account slot during the active stream")
	}
	t.Log("nonempty self export spool and overlapping account refusal verified")

	// Parent cancellation synchronously cancels its registered child context.
	// Inspect the context actually received by the stream, not an elapsed-time
	// guess. A detached context fails here; emergency cleanup still drains it.
	cancelFirst()
	if !errors.Is(observation.ctx.Err(), context.Canceled) {
		t.Fatal("self export request cancellation did not reach the stream")
	}
	waitSelfExportCancellationRequest(ctx, t, first, "canceled handler completion")
	assertSelfExportCancellationSpoolReleased(t, observation.file)
	var failure struct {
		SchemaVersion string `json:"schema_version"`
		Code          string `json:"code"`
		Error         string `json:"error"`
		Retryable     bool   `json:"retryable"`
	}
	if err := json.Unmarshal(first.recorder.Body.Bytes(), &failure); err != nil ||
		first.recorder.Code != http.StatusInternalServerError ||
		failure.SchemaVersion != "witself.v0" || failure.Code != "account_export_failed" ||
		failure.Error != "could not export account" || !failure.Retryable ||
		bytes.Contains(first.recorder.Body.Bytes(), []byte(partial)) {
		t.Fatal("canceled self export did not preserve the fixed error boundary")
	}
	if first.recorder.Header().Get("Content-Type") != "application/json" ||
		first.recorder.Header().Get("Content-Disposition") != "" ||
		first.recorder.Header().Get("X-Witself-Export-Format") != "" ||
		first.recorder.Header().Get("X-Witself-Export-Purpose") != "" {
		t.Fatal("canceled self export exposed archive response headers")
	}

	// This is the same handler closure, with a fresh uncanceled request. A new
	// handler would erase the guard and make the retry oracle vacuous.
	retry := start(ctx)
	waitSelfExportCancellationRequest(ctx, t, retry, "same-handler retry")
	if retry.recorder.Code != http.StatusOK || calls.Load() != 2 {
		t.Fatal("self export cancellation did not release the account slot")
	}
	if !bytes.Equal(retry.recorder.Body.Bytes(), archive) ||
		retry.recorder.Header().Get("Content-Type") != "application/gzip" ||
		retry.recorder.Header().Get("Content-Length") != strconv.Itoa(len(archive)) ||
		retry.recorder.Header().Get("X-Witself-Export-Format") != "1" ||
		retry.recorder.Header().Get("X-Witself-Export-Purpose") != archiveexport.PurposeSelf {
		t.Fatal("self export retry did not return the complete synthetic archive")
	}
	// The existing synthetic helper emits schema 72 and no row sources; this
	// proves complete archive transport/checksums, not account memory coverage.
	manifest, err := archiveexport.Read(ctx, bytes.NewReader(retry.recorder.Body.Bytes()),
		archiveexport.ImportOptions{CurrentSchema: 72})
	if err != nil || manifest.AccountID != accountID || manifest.Purpose != archiveexport.PurposeSelf ||
		manifest.Status != "active" || manifest.FormatVersion != archiveexport.FormatVersion {
		t.Fatal("self export retry archive failed checksum or identity verification")
	}
	filesMu.Lock()
	captured := append([]*os.File(nil), files...)
	filesMu.Unlock()
	if len(captured) != 2 || captured[0] != observation.file || captured[1] == captured[0] {
		t.Fatal("self export retry did not create exactly one distinct spool")
	}
	assertSelfExportCancellationSpoolReleased(t, captured[1])
	t.Log("canceled spool released and same-handler checksum-valid retry completed")
}

type selfExportCancellationObservation struct {
	ctx  context.Context
	file *os.File
}

type selfExportCancellationRequest struct {
	recorder *httptest.ResponseRecorder
	done     chan struct{}
}

func waitSelfExportCancellationRequest(ctx context.Context, t *testing.T, request selfExportCancellationRequest, phase string) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case <-request.done:
	case <-waitCtx.Done():
		t.Fatalf("self export cancellation fixture did not complete %s", phase)
	}
}

func assertSelfExportCancellationSpoolReleased(t *testing.T, file *os.File) {
	t.Helper()
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("self export cancellation did not close the spool")
	}
	if _, err := os.Stat(file.Name()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("self export cancellation did not remove the spool")
	}
}
