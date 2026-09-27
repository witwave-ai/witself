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
		{""}, {"UPPER"}, {"bad_name"}, {"-leading"}, {"trailing-"},
		{"double--hyphen"}, {"with.dot"}, {"with/slash"}, {"with space"},
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
