package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	archiveexport "github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/local"
)

const exportManifestReadLimit = 1 << 20

type exportSummary struct {
	SchemaVersion int
	ServerVersion string
	Tables        int
	TotalRows     int64
	Bytes         int64
}

// exportCmd downloads an account-wide customer archive with the managed
// account's operator credential. The cell is authoritative for the account
// scope; the CLI writes nothing to the requested destination until the
// archive's manifest and trailing chunk checksums have been verified.
func exportCmd(args []string) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configureCommandUsage(fs, "usage: witself export [--out FILE] [--force] [--account NAME] [--endpoint URL]")
	out := fs.String("out", "", "write the verified archive to FILE")
	force := fs.Bool("force", false, "replace an existing output file")
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	if parsed, exitCode := parseCommandFlags(fs, args); !parsed {
		return exitCode
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	accountName, _, err := local.ResolveAccount(*account)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: resolve export account: %v\n", err)
		return 1
	}
	outPath := *out
	if strings.TrimSpace(outPath) == "" {
		outPath = defaultExportFilename(accountName, time.Now().UTC())
	}
	if err := validateExportDestination(outPath, *force); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}

	ctx := context.Background()
	ep, tok, err := connect(ctx, accountName, strings.TrimSpace(*endpoint), "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: connect account export: %v\n", err)
		return 1
	}

	dir := filepath.Dir(outPath)
	tmp, err := os.CreateTemp(dir, ".witself-export-download-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: create temporary export beside %s: %v\n", outPath, err)
		return 1
	}
	tmpPath := tmp.Name()
	keepTemp := false
	defer func() {
		if !keepTemp {
			_ = os.Remove(tmpPath)
		}
	}()

	downloadStarted, downloadErr := downloadSelfExport(ctx, ep, tok, tmp)
	if downloadErr == nil {
		downloadErr = tmp.Sync()
	}
	if closeErr := tmp.Close(); downloadErr == nil {
		downloadErr = closeErr
	}
	if downloadErr != nil {
		if !downloadStarted {
			fmt.Fprintf(os.Stderr, "witself: %v\n", downloadErr)
			return 1
		}
		unverified, preserveErr := preserveUnverifiedExport(tmpPath, outPath)
		if preserveErr == nil {
			keepTemp = true
			fmt.Fprintf(os.Stderr, "witself: export download was incomplete and was kept at %s: %v\n", unverified, downloadErr)
		} else {
			fmt.Fprintf(os.Stderr, "witself: export download failed: %v (could not preserve the partial download: %v)\n", downloadErr, preserveErr)
		}
		return 1
	}

	info, err := os.Stat(tmpPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: inspect downloaded export: %v\n", err)
		return 1
	}
	summary, err := verifySelfExport(ctx, tmpPath, info.Size())
	if err != nil {
		unverified, preserveErr := preserveUnverifiedExport(tmpPath, outPath)
		if preserveErr == nil {
			keepTemp = true
			fmt.Fprintf(os.Stderr, "witself: export verification failed; unverified download kept at %s: %v\n", unverified, err)
		} else {
			fmt.Fprintf(os.Stderr, "witself: export verification failed: %v (could not preserve the download: %v)\n", err, preserveErr)
		}
		return 1
	}

	// Recheck after the potentially long download so a destination created in
	// the meantime is not silently replaced unless --force was explicit.
	if err := validateExportDestination(outPath, *force); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if err := installVerifiedExport(tmpPath, outPath, *force); err != nil {
		fmt.Fprintf(os.Stderr, "witself: install verified export at %s: %v\n", outPath, err)
		return 1
	}
	keepTemp = true

	fmt.Printf("exported: %s\n", outPath)
	fmt.Printf("schema_version: %d\n", summary.SchemaVersion)
	fmt.Printf("server_version: %s\n", summary.ServerVersion)
	fmt.Printf("tables: %d\n", summary.Tables)
	fmt.Printf("total_rows: %d\n", summary.TotalRows)
	fmt.Printf("bytes: %d\n", summary.Bytes)
	return 0
}

func defaultExportFilename(account string, now time.Time) string {
	return fmt.Sprintf("witself-export-%s-%s.tar.gz", account, now.UTC().Format("20060102"))
}

func validateExportDestination(path string, force bool) error {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("inspect export destination %s: %w", path, err)
	case info.IsDir():
		return fmt.Errorf("export destination %s is a directory", path)
	case !force:
		return fmt.Errorf("refusing to overwrite existing file %s without --force", path)
	default:
		return nil
	}
}

func downloadSelfExport(ctx context.Context, endpoint, token string, dst io.Writer) (bool, error) {
	requestURL := strings.TrimRight(endpoint, "/") + "/v1/export"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return false, fmt.Errorf("create export request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/gzip")

	// Account endpoints are credential audiences. A redirect must be handled
	// by fresh account placement discovery, never by forwarding the token.
	resp, err := (&http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}).Do(req)
	if err != nil {
		return false, fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, selfExportResponseError(resp)
	}
	if _, err := io.Copy(dst, resp.Body); err != nil {
		return true, fmt.Errorf("stream response: %w", err)
	}
	return true, nil
}

func selfExportResponseError(resp *http.Response) error {
	const responseLimit = 64 << 10
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, responseLimit))
	if readErr == nil {
		var body struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &body) == nil && strings.TrimSpace(body.Error) != "" {
			message := safeText(strings.TrimSpace(body.Error))
			if code := strings.TrimSpace(body.Code); code != "" {
				return fmt.Errorf("export request failed (%s, %s): %s", resp.Status, safeText(code), message)
			}
			return fmt.Errorf("export request failed (%s): %s", resp.Status, message)
		}
	}
	return fmt.Errorf("export request failed: %s", resp.Status)
}

func verifySelfExport(ctx context.Context, path string, archiveBytes int64) (exportSummary, error) {
	probe, err := readExportManifest(path)
	if err != nil {
		return exportSummary{}, err
	}
	if probe.SchemaVersion < 1 {
		return exportSummary{}, fmt.Errorf("manifest has invalid schema_version %d", probe.SchemaVersion)
	}

	f, err := os.Open(path)
	if err != nil {
		return exportSummary{}, err
	}
	defer func() { _ = f.Close() }()

	var totalRows int64
	manifest, err := archiveexport.Read(ctx, f, archiveexport.ImportOptions{
		CurrentSchema: probe.SchemaVersion,
		OnManifest: func(manifest archiveexport.Manifest) error {
			if manifest.FormatVersion != archiveexport.FormatVersion {
				return fmt.Errorf("manifest format_version is %d, want %d", manifest.FormatVersion, archiveexport.FormatVersion)
			}
			if manifest.Purpose != archiveexport.PurposeSelf {
				return fmt.Errorf("manifest purpose is %q, want %q", manifest.Purpose, archiveexport.PurposeSelf)
			}
			if manifest.BackupID != "" || manifest.EvacuationID != "" {
				return errors.New("self export manifest carries a backup_id or evacuation_id")
			}
			return nil
		},
		Row: func(_ string, _ []byte) error {
			totalRows++
			return nil
		},
	})
	if err != nil {
		return exportSummary{}, err
	}
	return exportSummary{
		SchemaVersion: manifest.SchemaVersion,
		ServerVersion: manifest.ServerVersion,
		Tables:        len(manifest.Tables),
		TotalRows:     totalRows,
		Bytes:         archiveBytes,
	}, nil
}

func readExportManifest(path string) (archiveexport.Manifest, error) {
	var manifest archiveexport.Manifest
	f, err := os.Open(path)
	if err != nil {
		return manifest, err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return manifest, fmt.Errorf("not a gzip stream: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tw := tar.NewReader(gz)
	header, err := tw.Next()
	if err != nil {
		return manifest, fmt.Errorf("read manifest header: %w", err)
	}
	if header.Name != "manifest.json" {
		return manifest, fmt.Errorf("first archive entry is %q, want manifest.json", header.Name)
	}
	if header.Size < 0 || header.Size > exportManifestReadLimit {
		return manifest, fmt.Errorf("manifest size %d exceeds the verification limit", header.Size)
	}
	raw, err := io.ReadAll(tw)
	if err != nil {
		return manifest, fmt.Errorf("read manifest: %w", err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return manifest, fmt.Errorf("decode manifest: %w", err)
	}
	return manifest, nil
}

func preserveUnverifiedExport(tmpPath, outPath string) (string, error) {
	for attempt := 0; attempt < 1000; attempt++ {
		candidate := outPath + ".unverified"
		if attempt > 0 {
			candidate = fmt.Sprintf("%s.%d.unverified", outPath, attempt)
		}
		if _, err := os.Lstat(candidate); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if err := os.Link(tmpPath, candidate); err != nil {
			if _, statErr := os.Lstat(candidate); statErr == nil {
				continue
			}
			return "", err
		}
		if err := os.Remove(tmpPath); err != nil {
			_ = os.Remove(candidate)
			return "", err
		}
		return candidate, nil
	}
	return "", errors.New("too many existing unverified export files")
}

func installVerifiedExport(tmpPath, outPath string, force bool) error {
	if !force {
		// The temporary lives beside the destination, so a hard link installs
		// the verified inode atomically while retaining O_EXCL-like no-clobber
		// behavior. A plain os.Rename would overwrite a file created after the
		// caller's final Lstat on Unix.
		if err := os.Link(tmpPath, outPath); err != nil {
			if _, statErr := os.Lstat(outPath); statErr == nil {
				return fmt.Errorf("refusing to overwrite existing file %s without --force", outPath)
			}
			return err
		}
		if err := os.Remove(tmpPath); err != nil {
			return fmt.Errorf("remove temporary export after installation: %w", err)
		}
		return nil
	}
	if _, err := os.Lstat(outPath); errors.Is(err, os.ErrNotExist) {
		return os.Rename(tmpPath, outPath)
	} else if err != nil {
		return err
	}

	// Moving the old file aside makes --force portable to systems where
	// os.Rename does not replace an existing destination. Restore it if the
	// verified archive cannot be installed.
	backup, err := os.CreateTemp(filepath.Dir(outPath), ".witself-export-replaced-*")
	if err != nil {
		return err
	}
	backupPath := backup.Name()
	if err := backup.Close(); err != nil {
		_ = os.Remove(backupPath)
		return err
	}
	if err := os.Remove(backupPath); err != nil {
		return err
	}
	if err := os.Rename(outPath, backupPath); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		if restoreErr := os.Rename(backupPath, outPath); restoreErr != nil {
			return fmt.Errorf("install: %v; restore prior file: %v", err, restoreErr)
		}
		return err
	}
	if err := os.Remove(backupPath); err != nil {
		return fmt.Errorf("remove replaced export %s: %w", backupPath, err)
	}
	return nil
}
