package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	archiveexport "github.com/witwave-ai/witself/internal/export"
)

func TestAccountBackupCellManifestAndEcho(t *testing.T) {
	const configuredCell = "civo-sandbox-use1-serving"
	t.Setenv("WITSELF_CELL_NAME", configuredCell)
	for _, tc := range []struct {
		name, header, want string
	}{
		{"registered-name", "civo-sandbox-usw2-dev", "civo-sandbox-usw2-dev"},
		{"absent", "", configuredCell},
		{"configured-name", configuredCell, configuredCell},
		{"leading-hyphen", "-leading", "-leading"},
		{"trailing-hyphen", "trailing-", "trailing-"},
		{"double-hyphen", "double--hyphen", "double--hyphen"},
		{"max-length", strings.Repeat("z", 64), strings.Repeat("z", 64)},
		{"minimum-length", "0", "0"},
		{"maximum-length", strings.Repeat("z", 64), strings.Repeat("z", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			handler := accountBackupHandler(Config{
				BackupToken: "witself_bkp_test",
				StreamAccountBackup: func(ctx context.Context, accountID, backupID, cellName string, w io.Writer, _ func() error) error {
					calls++
					return archiveexport.Write(ctx, w, archiveexport.Manifest{
						SchemaVersion: 73,
						Purpose:       archiveexport.PurposeBackup,
						AccountID:     accountID,
						BackupID:      backupID,
						Cell:          cellName,
						Status:        "active",
					}, nil)
				},
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/accounts/acc_backup:export-backup", nil)
			req.Header.Set("Authorization", "Bearer witself_bkp_test")
			req.Header.Set(AccountBackupIDHeader, "bkp_cell_test")
			if tc.header != "" {
				req.Header.Set(AccountBackupCellHeader, tc.header)
			}
			recorder := httptest.NewRecorder()
			handler(recorder, req)
			response := recorder.Result()
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != http.StatusOK || calls != 1 {
				t.Fatalf("status=%d calls=%d", response.StatusCode, calls)
			}
			if response.Header.Get(AccountBackupCellHeader) != tc.want {
				t.Fatal("response did not acknowledge the effective cell before streaming")
			}
			manifest, err := archiveexport.Read(req.Context(), response.Body, archiveexport.ImportOptions{CurrentSchema: 73})
			if err != nil {
				t.Fatal(err)
			}
			if manifest.Cell != tc.want || manifest.AccountID != "acc_backup" || manifest.BackupID != "bkp_cell_test" || manifest.Purpose != archiveexport.PurposeBackup {
				t.Fatal("archive manifest did not preserve the exact backup identity")
			}
		})
	}
}

func TestAccountBackupCellInvalidDoesNotStartExport(t *testing.T) {
	t.Setenv("WITSELF_CELL_NAME", "configured-cell")
	for _, names := range [][]string{
		{""}, {"UPPER"}, {"bad_name"},
		{"with.dot"}, {"with/slash"}, {"with space"},
		{" padded"}, {"padded "}, {"nonascii-é"}, {"newline\n"},
		{strings.Repeat("z", 65)}, {"one", "two"}, {"one", "one"},
	} {
		t.Run(strings.Join(names, ","), func(t *testing.T) {
			handler := accountBackupHandler(Config{
				BackupToken: "witself_bkp_test",
				StreamAccountBackup: func(_ context.Context, _, _, _ string, _ io.Writer, _ func() error) error {
					t.Fatal("invalid cell started an export")
					return nil
				},
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/accounts/acc_backup:export-backup", nil)
			req.Header.Set("Authorization", "Bearer witself_bkp_test")
			req.Header.Set(AccountBackupIDHeader, "bkp_cell_test")
			for _, name := range names {
				req.Header.Add(AccountBackupCellHeader, name)
			}
			recorder := httptest.NewRecorder()
			handler(recorder, req)
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != http.StatusBadRequest || body["error"] != "a valid backup cell name is required" {
				t.Fatalf("status=%d body=%v", recorder.Code, body)
			}
			if recorder.Header().Get(AccountBackupCellHeader) != "" || recorder.Header().Get("X-Witself-Export-Format") != "" {
				t.Fatal("invalid cell received archive response headers")
			}
		})
	}
}

func TestValidateAccountBackupIgnoresBackupCellHeader(t *testing.T) {
	for _, header := range []string{"", "registered-cell", "invalid_cell"} {
		t.Run(header, func(t *testing.T) {
			calls := 0
			handler := accountBackupHandler(Config{
				BackupToken:             "witself_bkp_test",
				BackupValidationEnabled: true,
				ValidateAccountBackup: func(_ context.Context, accountID, backupID string, body io.Reader) (ImportSummary, error) {
					calls++
					data, err := io.ReadAll(body)
					if err != nil || string(data) != "archive" || accountID != "acc_backup" || backupID != "bkp_cell_test" {
						t.Fatal("validation inputs changed")
					}
					return ImportSummary{AccountID: accountID, BackupID: backupID, Status: "active", SchemaVersion: 73}, nil
				},
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/accounts/acc_backup:validate-backup", strings.NewReader("archive"))
			req.Header.Set("Authorization", "Bearer witself_bkp_test")
			req.Header.Set(AccountBackupIDHeader, "bkp_cell_test")
			if header != "" {
				req.Header.Set(AccountBackupCellHeader, header)
			}
			recorder := httptest.NewRecorder()
			handler(recorder, req)
			if recorder.Code != http.StatusOK || calls != 1 || recorder.Header().Get(AccountBackupCellHeader) != "" {
				t.Fatalf("validation status=%d calls=%d", recorder.Code, calls)
			}
		})
	}
}

func TestAccountBackupArchivePull(t *testing.T) {
	for _, name := range []string{"success", "wrong-origin", "http", "missing-config", "bad-token", "bad-size", "too-large", "zero", "negative", "empty-url", "userinfo", "fragment", "size-mismatch", "non-200", "short-body", "redirect", "early-validator"} {
		t.Run(name, func(t *testing.T) {
			calls, validations := 0, 0
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
			origin, source, token, size := cp.URL, cp.URL+"/v1/backups:archive?account_id=acc_backup", capability, "7"
			want := http.StatusBadRequest
			switch name {
			case "wrong-origin":
				origin = "https://wrong.invalid"
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
			case "size-mismatch", "non-200", "short-body", "redirect":
				want = http.StatusBadGateway
			default:
				want = http.StatusOK
			}
			cfg := Config{
				BackupToken: "witself_bkp_test", BackupValidationEnabled: true, BackupValidationArchiveOrigin: origin,
				ValidateAccountBackup: func(_ context.Context, accountID, backupID string, body io.Reader) (ImportSummary, error) {
					validations++
					if name != "early-validator" {
						data, err := io.ReadAll(body)
						if name != "short-body" && (err != nil || string(data) != "archive") {
							t.Error("archive did not stream intact")
						}
					}
					return ImportSummary{AccountID: accountID, BackupID: backupID, Status: "active", SchemaVersion: 73}, nil
				},
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/accounts/acc_backup:validate-backup", nil)
			req.Header.Set("Authorization", "Bearer witself_bkp_test")
			req.Header.Set(AccountBackupIDHeader, "bkp_cell_test")
			req.Header.Set(AccountBackupCellHeader, "INVALID IGNORED CELL")
			req.Header.Set("X-Witself-Backup-Archive-URL", source)
			req.Header.Set("X-Witself-Backup-Archive-Token", token)
			req.Header.Set("X-Witself-Backup-Archive-Size", size)
			recorder := httptest.NewRecorder()
			accountBackupHandlerWithArchiveClient(cfg, client)(recorder, req)
			if recorder.Code != want {
				t.Fatalf("status=%d want=%d", recorder.Code, want)
			}
			if want == http.StatusBadRequest && (calls != 0 || validations != 0) {
				t.Fatal("invalid source downloaded or validated")
			}
			if want != http.StatusBadRequest && calls != 1 {
				t.Fatal("expected exactly one request without redirect")
			}
			var result map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if want == http.StatusOK {
				if result["validated"] != true || result["account_id"] != "acc_backup" || result["backup_id"] != "bkp_cell_test" || result["purpose"] != "backup" || result["archive_schema_version"] != float64(73) {
					t.Fatal("missing exact acknowledgement")
				}
			} else {
				message := "a valid backup archive source is required"
				if want == http.StatusBadGateway {
					message = "backup archive download failed"
				}
				if result["error"] != message || result["validated"] != nil {
					t.Fatal("failure leaked details or acknowledged validation")
				}
			}
		})
	}
}
