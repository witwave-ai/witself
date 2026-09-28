package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAccountImportArchivePull(t *testing.T) {
	for _, name := range []string{"success", "wrong-origin", "wrong-port", "http", "missing-config", "invalid-config", "connection-failure", "bad-token", "bad-size", "too-large", "zero", "negative", "empty-url", "userinfo", "fragment", "size-mismatch", "non-200", "short-body", "redirect", "early-importer"} {
		t.Run(name, func(t *testing.T) {
			calls, imports, reports := 0, 0, 0
			capability := "cap_" + strings.Repeat("a", 64)
			cp := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.ProtoMajor != 1 {
					t.Errorf("download negotiated %s; the archive client must pin HTTP/1.1 through ALPN", r.Proto)
				}
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+capability {
					t.Error("download did not use the capability bearer")
				}
				if name == "redirect" {
					w.Header().Set("Location", "/redirected")
					w.WriteHeader(http.StatusFound)
					return
				}
				if name == "non-200" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if name == "size-mismatch" {
					w.Header().Set("Content-Length", "8")
				} else {
					w.Header().Set("Content-Length", "7")
				}
				if name == "short-body" {
					_, _ = io.WriteString(w, "short")
					return
				}
				_, _ = io.WriteString(w, "archive")
			}))
			// The real control plane sits behind an h2-capable edge; the fake must
			// offer h2 too so an ALPN mismatch in the client cannot hide here.
			cp.EnableHTTP2 = true
			cp.StartTLS()
			defer cp.Close()
			client := newBackupArchiveClient()
			client.Transport.(*http.Transport).TLSClientConfig.RootCAs = cp.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
			defer client.CloseIdleConnections()
			origin, source, token, size := cp.URL, cp.URL+"/v1/archives:pull?account_id=acc_backup", capability, "7"
			want := http.StatusBadRequest
			switch name {
			case "wrong-origin":
				origin = "https://wrong.invalid"
			case "wrong-port":
				origin = "https://127.0.0.1:1"
			case "invalid-config":
				origin = cp.URL + "/path"
			case "http":
				source = strings.Replace(source, "https:", "http:", 1)
			case "missing-config":
				origin = ""
			case "bad-token":
				token = "cap_bad"
			case "bad-size":
				size = "1.5"
			case "too-large":
				size = "8589934593"
			case "zero":
				size = "0"
			case "negative":
				size = "-1"
			case "empty-url":
				source = ""
			case "userinfo":
				source = strings.Replace(source, "https://", "https://user@", 1)
			case "fragment":
				source += "#fragment"
			case "size-mismatch", "non-200", "redirect":
				want = http.StatusOK
			default:
				// short-body fails after the streamed headers: status 200 with an
				// error object instead of the acknowledgement.
				want = http.StatusOK
			}
			downloadFailedAfterHeaders := name == "short-body" || name == "size-mismatch" || name == "non-200" || name == "redirect" || name == "connection-failure"
			if name == "connection-failure" {
				cp.Close()
			}
			cfg := Config{
				ProvisionToken: "witself_prv_test", BackupValidationArchiveOrigin: origin,
				ReportAccountImportFailure: func(_ context.Context, _ string, err error) {
					reports++
					if strings.Contains(err.Error(), capability) || strings.Contains(err.Error(), cp.URL) {
						t.Fatal("download diagnostic leaked capability or URL")
					}
				},
				ImportAccountArchive: func(_ context.Context, accountID, evacuationID string, body io.Reader) (ImportSummary, error) {
					imports++
					if name != "early-importer" {
						data, err := io.ReadAll(body)
						if name != "short-body" && (err != nil || string(data) != "archive") {
							t.Error("archive did not stream intact")
						}
					}
					return ImportSummary{AccountID: accountID, EvacuationID: evacuationID, EvacuationRole: "target", Status: "active", SchemaVersion: 73}, nil
				},
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/accounts/acc_backup:import-evacuation", nil)
			req.Header.Set("Authorization", "Bearer witself_prv_test")
			req.Header.Set(AccountEvacuationIDHeader, "evac_cell_test")
			req.Header.Set("X-Witself-Archive-URL", source)
			req.Header.Set("X-Witself-Archive-Token", token)
			req.Header.Set("X-Witself-Archive-Size", size)
			recorder := httptest.NewRecorder()
			accountLifecycleHandlerWithArchiveClient(cfg, client)(recorder, req)
			if recorder.Code != want {
				t.Fatalf("status=%d want=%d", recorder.Code, want)
			}
			if want == http.StatusBadRequest && (calls != 0 || imports != 0) {
				t.Fatal("invalid source downloaded or validated")
			}
			if want != http.StatusBadRequest && name != "connection-failure" && calls != 1 {
				t.Fatal("expected exactly one request without redirect")
			}
			var result map[string]any
			if err := json.NewDecoder(recorder.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
			if downloadFailedAfterHeaders {
				if imports != 0 || reports != 1 {
					t.Fatal("download failure must be reported without importing")
				}
				if result["error"] != "archive download failed" || result["evacuation_role"] != nil {
					t.Fatal("short body must fail closed after the streamed headers")
				}
			} else if want == http.StatusOK {
				if result["evacuation_role"] != "target" || result["account_id"] != "acc_backup" || result["evacuation_id"] != "evac_cell_test" || result["archive_schema_version"] != float64(73) {
					t.Fatal("missing exact acknowledgement")
				}
			} else {
				message := "a valid archive source is required"
				if result["error"] != message || result["evacuation_role"] != nil {
					t.Fatal("failure leaked details or acknowledged validation")
				}
			}
		})
	}
}

func TestAccountImportArchivePullSpoolsBeforeImport(t *testing.T) {
	previous := backupValidationHeartbeat
	backupValidationHeartbeat = 20 * time.Millisecond
	defer func() { backupValidationHeartbeat = previous }()
	spoolGlob := filepath.Join(os.TempDir(), "witself-account-import-*.tar.gz")
	before, _ := filepath.Glob(spoolGlob)
	for _, name := range []string{"slow-validator", "bad-archive"} {
		t.Run(name, func(t *testing.T) {
			capability := "cap_" + strings.Repeat("c", 64)
			served := make(chan struct{})
			cp := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", "7")
				_, _ = io.WriteString(w, "archive")
				close(served)
			}))
			cp.EnableHTTP2 = true
			cp.StartTLS()
			defer cp.Close()
			client := newBackupArchiveClient()
			client.Transport.(*http.Transport).TLSClientConfig.RootCAs = cp.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
			defer client.CloseIdleConnections()
			var reported error
			cfg := Config{
				ProvisionToken: "witself_prv_test", BackupValidationArchiveOrigin: cp.URL,
				ReportAccountImportFailure: func(_ context.Context, accountID string, err error) {
					if accountID != "acc_backup" {
						t.Errorf("failure reported for %q", accountID)
					}
					reported = err
				},
				ImportAccountArchive: func(_ context.Context, accountID, evacuationID string, body io.Reader) (ImportSummary, error) {
					// The control plane's stream must already be complete: the
					// validator only starts reading after the fake server has
					// finished serving and several heartbeats have passed.
					<-served
					time.Sleep(4 * backupValidationHeartbeat)
					data, err := io.ReadAll(body)
					if err != nil || string(data) != "archive" {
						t.Errorf("spooled archive did not read back intact: %q %v", data, err)
					}
					if name == "bad-archive" {
						return ImportSummary{}, ErrBadArchive
					}
					return ImportSummary{AccountID: accountID, EvacuationID: evacuationID, EvacuationRole: "target", Status: "active", SchemaVersion: 73}, nil
				},
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/accounts/acc_backup:import-evacuation", nil)
			req.Header.Set("Authorization", "Bearer witself_prv_test")
			req.Header.Set(AccountEvacuationIDHeader, "evac_cell_test")
			req.Header.Set("X-Witself-Archive-URL", cp.URL+"/v1/archives:pull?account_id=acc_backup")
			req.Header.Set("X-Witself-Archive-Token", capability)
			req.Header.Set("X-Witself-Archive-Size", "7")
			recorder := httptest.NewRecorder()
			accountLifecycleHandlerWithArchiveClient(cfg, client)(recorder, req)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d want=200", recorder.Code)
			}
			var result map[string]any
			if err := json.NewDecoder(strings.NewReader(recorder.Body.String())).Decode(&result); err != nil {
				t.Fatal(err)
			}
			if name == "bad-archive" {
				if result["error"] != "invalid or corrupt archive" || !errors.Is(reported, ErrBadArchive) {
					t.Fatalf("classified failure must answer an error object and reach the report hook (reported=%v)", reported)
				}
			} else if result["evacuation_role"] != "target" || reported != nil {
				t.Fatalf("expected acknowledgement without a reported failure (reported=%v)", reported)
			}
		})
	}
	after, _ := filepath.Glob(spoolGlob)
	if len(after) != len(before) {
		t.Fatalf("spool files leaked: before=%d after=%d", len(before), len(after))
	}
}

func TestAccountImportLegacyAndCompleted(t *testing.T) {
	for _, completed := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodPost, "/v1/accounts/acc_import:import-evacuation", strings.NewReader("legacy"))
		req.Header.Set("Authorization", "Bearer test-provision")
		req.Header.Set(AccountEvacuationIDHeader, "evac_test")
		cfg := Config{ProvisionToken: "test-provision", ImportAccountArchive: func(_ context.Context, accountID, evacuationID string, body io.Reader) (ImportSummary, error) {
			data, err := io.ReadAll(body)
			if err != nil || string(data) != "legacy" {
				t.Fatal("legacy request body changed")
			}
			return ImportSummary{AccountID: accountID, EvacuationID: evacuationID, EvacuationRole: "target", Status: "active", AlreadyImported: true, EvacuationCompleted: completed}, nil
		}}
		recorder := httptest.NewRecorder()
		accountLifecycleHandler(cfg)(recorder, req)
		var result map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != 200 || result["already_imported"] != true || result["evacuation_completed"] != completed {
			t.Fatal("legacy acknowledgement changed")
		}
	}
}

func TestAccountImportHeartbeatsBeforeDownloadHeaders(t *testing.T) {
	previous := backupValidationHeartbeat
	backupValidationHeartbeat = 10 * time.Millisecond
	defer func() { backupValidationHeartbeat = previous }()
	release := make(chan struct{})
	cp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.Header().Set("Content-Length", "7")
		_, _ = io.WriteString(w, "archive")
	}))
	defer cp.Close()
	cfg := Config{ProvisionToken: "test", BackupValidationArchiveOrigin: cp.URL,
		ImportAccountArchive: func(_ context.Context, accountID, evacuationID string, _ io.Reader) (ImportSummary, error) {
			return ImportSummary{AccountID: accountID, EvacuationID: evacuationID, EvacuationRole: "target", Status: "suspended"}, nil
		},
	}
	server := httptest.NewServer(accountLifecycleHandlerWithArchiveClient(cfg, cp.Client()))
	defer server.Close()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/accounts/acc_import:import-evacuation", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set(AccountEvacuationIDHeader, "evac_test")
	req.Header.Set("X-Witself-Archive-URL", cp.URL)
	req.Header.Set("X-Witself-Archive-Token", "cap_"+strings.Repeat("a", 64))
	req.Header.Set("X-Witself-Archive-Size", "7")
	client := &http.Client{Timeout: time.Second}
	response, err := client.Do(req)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var heartbeat [1]byte
	_, err = io.ReadFull(response.Body, heartbeat[:])
	close(release)
	if err != nil || heartbeat[0] != '\n' {
		t.Fatal("missing heartbeat before download headers")
	}
	var result map[string]any
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["evacuation_id"] != "evac_test" || result["evacuation_role"] != "target" {
		t.Fatal("missing exact acknowledgement after heartbeats")
	}
}

func TestAccountEvacuationExportFlush(t *testing.T) {
	recorder := httptest.NewRecorder()
	cfg := Config{ProvisionToken: "test", StreamAccountExport: func(_ context.Context, _, _ string, w io.Writer, flush func() error) error {
		for _, chunk := range []string{"manifest", "chunk1", "chunk2"} {
			recorder.Flushed = false
			if _, err := io.WriteString(w, chunk); err != nil {
				return err
			}
			if err := flush(); err != nil {
				return err
			}
			if !recorder.Flushed {
				t.Fatal("flush hook did not flush response")
			}
		}
		return nil
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/accounts/acc_export:export-evacuation", nil)
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set(AccountEvacuationIDHeader, "evac_test")
	accountLifecycleHandler(cfg)(recorder, req)
	if recorder.Body.String() != "manifestchunk1chunk2" {
		t.Fatal("export did not stream all chunks")
	}
}
