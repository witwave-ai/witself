package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The command adapter classifies sync archive refusals as ErrBadArchive and
// destructive refusals as ErrConflict. Exercise those classes through the real
// routes, freezing their existing public response bytes (including the newline).
func TestSyncRefusalMappingResponses(t *testing.T) {
	for _, tc := range []struct {
		name, action, requestBody, responseBody string
		mappedError                             error
		status                                  int
	}{
		{"import", "import-evacuation", "archive", "{\"error\":\"invalid or corrupt archive\",\"schema_version\":\"witself.v0\"}\n", ErrBadArchive, http.StatusBadRequest},
		{"backup", "validate-backup", "archive", "{\"error\":\"invalid or mismatched backup archive\",\"schema_version\":\"witself.v0\"}\n", ErrBadArchive, http.StatusBadRequest},
		{"finalization", "finalize-evacuation", `{"evacuation_id":"evac_01J00000000000000000000000"}`, "{\"error\":\"account is not this evacuation's source\",\"schema_version\":\"witself.v0\"}\n", ErrConflict, http.StatusConflict},
	} {
		for _, wrapped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/wrapped=%t", tc.name, wrapped), func(t *testing.T) {
				err := tc.mappedError
				if wrapped {
					err = fmt.Errorf("private sync diagnostic: %w", err)
				}
				calls := 0
				cfg := Config{
					ProvisionToken:          "fixture-provision",
					BackupToken:             "fixture-backup",
					BackupValidationEnabled: true,
					ProvisionAccountExact: func(context.Context, string, string, string, string, string) (ProvisionedAccount, error) {
						t.Fatal("refused movement must not provision an account")
						return ProvisionedAccount{}, errors.New("unused")
					},
					ImportAccountArchive: func(context.Context, string, string, io.Reader) (ImportSummary, error) {
						calls++
						return ImportSummary{}, err
					},
					ValidateAccountBackup: func(context.Context, string, string, io.Reader) (ImportSummary, error) {
						calls++
						return ImportSummary{}, err
					},
					FinalizeAccountEvacuation: func(context.Context, string, string) (AccountEvacuationFinalizationRecord, error) {
						calls++
						return AccountEvacuationFinalizationRecord{}, err
					},
				}
				req := httptest.NewRequest(http.MethodPost, "/v1/accounts/acc_fixture:"+tc.action, strings.NewReader(tc.requestBody))
				req.Header.Set("Authorization", "Bearer fixture-provision")
				req.Header.Set(AccountEvacuationIDHeader, "evac_01J00000000000000000000000")
				if tc.name == "backup" {
					req.Header.Set("Authorization", "Bearer fixture-backup")
					req.Header.Set(AccountBackupIDHeader, "bkp_01J00000000000000000000000")
				}
				response := httptest.NewRecorder()
				apiMux(cfg).ServeHTTP(response, req)
				if calls != 1 {
					t.Fatal("request did not reach exactly one movement callback")
				}
				if response.Code != tc.status || response.Body.String() != tc.responseBody || response.Header().Get("Content-Type") != "application/json" {
					t.Fatal("sync refusal status or exact public response bytes changed")
				}
			})
		}
	}
}
