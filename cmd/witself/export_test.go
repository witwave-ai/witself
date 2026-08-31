package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	archiveexport "github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/local"
)

func TestExportCommandDownloadsVerifiesAndPrintsSummary(t *testing.T) {
	archive := buildCLIExportArchive(t)
	home := filepath.Join(t.TempDir(), "witself-home")
	t.Setenv("WITSELF_HOME", home)
	if err := local.Save("team", local.Account{ID: "acc_export"}, "witself_opr_export"); err != nil {
		t.Fatal(err)
	}

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/export" {
			t.Errorf("request = %s %s, want GET /v1/export", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer witself_opr_export" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/gzip" {
			t.Errorf("Accept = %q", got)
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("X-Witself-Export-Format", "1")
		w.Header().Set("X-Witself-Export-Purpose", "self")
		_, _ = w.Write(archive)
	}))
	defer server.Close()

	outPath := filepath.Join(t.TempDir(), "account.tar.gz")
	var code int
	stdout := captureStdout(t, func() {
		code = run([]string{"export", "--account", "team", "--endpoint", server.URL, "--out", outPath})
	})
	if code != 0 {
		t.Fatalf("witself export = %d, want 0", code)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, archive) {
		t.Fatal("installed export differs from the verified response")
	}
	for _, want := range []string{
		"exported: " + outPath,
		"schema_version: 72",
		"server_version: 0.0.test",
		"tables: 2",
		"total_rows: 3",
		fmt.Sprintf("bytes: %d", len(archive)),
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("summary missing %q:\n%s", want, stdout)
		}
	}
}

func TestExportCommandKeepsCorruptDownloadUnverified(t *testing.T) {
	corrupt := corruptCLIExportChunk(t, buildCLIExportArchive(t))
	home := filepath.Join(t.TempDir(), "witself-home")
	t.Setenv("WITSELF_HOME", home)
	if err := local.Save("default", local.Account{ID: "acc_export"}, "witself_opr_export"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(corrupt)
	}))
	defer server.Close()

	outPath := filepath.Join(t.TempDir(), "corrupt.tar.gz")
	var code int
	stderr := captureExportStderr(t, func() {
		code = run([]string{"export", "--endpoint", server.URL, "--out", outPath})
	})
	if code == 0 {
		t.Fatal("corrupt export succeeded")
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Fatalf("verified destination exists after corruption: %v", err)
	}
	unverified := outPath + ".unverified"
	got, err := os.ReadFile(unverified)
	if err != nil {
		t.Fatalf("read preserved unverified archive: %v", err)
	}
	if !bytes.Equal(got, corrupt) {
		t.Fatal("preserved unverified archive differs from the response")
	}
	if !strings.Contains(stderr, "export verification failed") || !strings.Contains(stderr, unverified) {
		t.Fatalf("failure did not clearly identify the preserved download:\n%s", stderr)
	}
}

func TestExportCommandRefusesExistingFileWithoutForceBeforeRequest(t *testing.T) {
	home := filepath.Join(t.TempDir(), "witself-home")
	t.Setenv("WITSELF_HOME", home)
	if err := local.Save("default", local.Account{ID: "acc_export"}, "witself_opr_export"); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	dir := t.TempDir()
	outPath := filepath.Join(dir, "existing.tar.gz")
	if err := os.WriteFile(outPath, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"export", "--endpoint", server.URL, "--out", outPath}); code == 0 {
		t.Fatal("export overwrote an existing file without --force")
	}
	if requests != 0 {
		t.Fatalf("server received %d requests after local destination refusal", requests)
	}
	got, err := os.ReadFile(outPath)
	if err != nil || string(got) != "keep me" {
		t.Fatalf("existing file changed: %q / %v", got, err)
	}
}

func TestExportCommandForceReplacesOnlyAfterVerification(t *testing.T) {
	archive := buildCLIExportArchive(t)
	home := filepath.Join(t.TempDir(), "witself-home")
	t.Setenv("WITSELF_HOME", home)
	if err := local.Save("default", local.Account{ID: "acc_export"}, "witself_opr_export"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	defer server.Close()

	outPath := filepath.Join(t.TempDir(), "existing.tar.gz")
	if err := os.WriteFile(outPath, []byte("old export"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"export", "--endpoint", server.URL, "--out", outPath, "--force"}); code != 0 {
		t.Fatalf("forced export = %d, want 0", code)
	}
	got, err := os.ReadFile(outPath)
	if err != nil || !bytes.Equal(got, archive) {
		t.Fatalf("forced destination = %d bytes / %v", len(got), err)
	}
}

func TestInstallVerifiedExportWithoutForceIsAtomicNoClobber(t *testing.T) {
	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "download.tmp")
	outPath := filepath.Join(dir, "export.tar.gz")
	if err := os.WriteFile(tmpPath, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outPath, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installVerifiedExport(tmpPath, outPath, false); err == nil {
		t.Fatal("no-force install replaced an existing destination")
	}
	got, err := os.ReadFile(outPath)
	if err != nil || string(got) != "existing" {
		t.Fatalf("destination changed: %q / %v", got, err)
	}
	got, err = os.ReadFile(tmpPath)
	if err != nil || string(got) != "new" {
		t.Fatalf("verified temporary disappeared after refusal: %q / %v", got, err)
	}
}

func TestPreserveUnverifiedExportDoesNotClobberEarlierFailure(t *testing.T) {
	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "download.tmp")
	outPath := filepath.Join(dir, "export.tar.gz")
	if err := os.WriteFile(tmpPath, []byte("new failure"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outPath+".unverified", []byte("earlier failure"), 0o600); err != nil {
		t.Fatal(err)
	}
	preserved, err := preserveUnverifiedExport(tmpPath, outPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := outPath + ".1.unverified"; preserved != want {
		t.Fatalf("preserved path = %q, want %q", preserved, want)
	}
	got, err := os.ReadFile(outPath + ".unverified")
	if err != nil || string(got) != "earlier failure" {
		t.Fatalf("earlier failure changed: %q / %v", got, err)
	}
	got, err = os.ReadFile(preserved)
	if err != nil || string(got) != "new failure" {
		t.Fatalf("new failure = %q / %v", got, err)
	}
}

func TestVerifySelfExportChecksChecksumsAndSummary(t *testing.T) {
	archive := buildCLIExportArchive(t)
	validPath := filepath.Join(t.TempDir(), "valid.tar.gz")
	if err := os.WriteFile(validPath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := verifySelfExport(context.Background(), validPath, int64(len(archive)))
	if err != nil {
		t.Fatalf("verify valid archive: %v", err)
	}
	if summary.SchemaVersion != 72 || summary.ServerVersion != "0.0.test" ||
		summary.Tables != 2 || summary.TotalRows != 3 || summary.Bytes != int64(len(archive)) {
		t.Fatalf("summary = %+v", summary)
	}

	corrupt := corruptCLIExportChunk(t, archive)
	corruptPath := filepath.Join(t.TempDir(), "corrupt.tar.gz")
	if err := os.WriteFile(corruptPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySelfExport(context.Background(), corruptPath, int64(len(corrupt))); err == nil ||
		!strings.Contains(err.Error(), "does not match its checksum") {
		t.Fatalf("corrupt verification error = %v", err)
	}
}

func TestDefaultExportFilenameUsesUTCDate(t *testing.T) {
	now := time.Date(2026, 8, 31, 20, 30, 0, 0, time.FixedZone("west", -6*60*60))
	if got, want := defaultExportFilename("team", now), "witself-export-team-20260901.tar.gz"; got != want {
		t.Fatalf("default filename = %q, want %q", got, want)
	}
}

type cliExportSource struct {
	table string
	rows  [][]byte
	next  int
}

func (s *cliExportSource) Table() string { return s.table }

func (s *cliExportSource) Next(context.Context) ([]byte, error) {
	if s.next >= len(s.rows) {
		return nil, nil
	}
	row := s.rows[s.next]
	s.next++
	return row, nil
}

func buildCLIExportArchive(t *testing.T) []byte {
	t.Helper()
	var archive bytes.Buffer
	err := archiveexport.Write(context.Background(), &archive, archiveexport.Manifest{
		SchemaVersion: 72,
		ServerVersion: "0.0.test",
		Purpose:       archiveexport.PurposeSelf,
		AccountID:     "acc_export",
		Cell:          "test-cell",
		Status:        "active",
	}, []archiveexport.RowSource{
		&cliExportSource{table: "accounts", rows: [][]byte{
			[]byte(`{"id":"acc_export","status":"active","payload":"alpha"}`),
		}},
		&cliExportSource{table: "realms", rows: [][]byte{
			[]byte(`{"id":"rlm_one","account_id":"acc_export"}`),
			[]byte(`{"id":"rlm_two","account_id":"acc_export"}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func corruptCLIExportChunk(t *testing.T, archive []byte) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	tarBytes, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	corruptTar := bytes.Replace(tarBytes, []byte(`"payload":"alpha"`), []byte(`"payload":"omega"`), 1)
	if bytes.Equal(corruptTar, tarBytes) {
		t.Fatal("test archive did not contain the chunk payload to corrupt")
	}
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(corruptTar); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func captureExportStderr(t *testing.T, run func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	done := make(chan string)
	go func() {
		raw, _ := io.ReadAll(r)
		done <- string(raw)
	}()
	run()
	_ = w.Close()
	os.Stderr = old
	output := <-done
	_ = r.Close()
	return output
}
