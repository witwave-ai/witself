package store

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	archiveexport "github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/loadquality"
	"github.com/witwave-ai/witself/internal/memlimit"
)

const founderEnv = "WITSELF_MEMORY_ARCHIVE_LOAD_FOUNDER_"

type founderArchiveOptions struct {
	phase, dir, decode                                 string
	entries, emails, emailBytes, memories              int
	fakeLimit, expectLimit, maxTotal, maxLive, maxAnon int64
}

// Empty optional settings are unset: make exports even its empty variables.
func parseFounderArchiveOptions(getenv func(string) string) (founderArchiveOptions, error) {
	o := founderArchiveOptions{phase: getenv(founderEnv + "PHASE"), dir: getenv(founderEnv + "DIR"), decode: getenv(founderEnv + "DECODE")}
	if o.phase != "seed-export" && o.phase != "import" {
		return o, fmt.Errorf("%sPHASE must be seed-export or import", founderEnv)
	}
	if !filepath.IsAbs(o.dir) {
		return o, fmt.Errorf("%sDIR must be an absolute existing directory", founderEnv)
	}
	if st, err := os.Stat(o.dir); err != nil || !st.IsDir() {
		return o, fmt.Errorf("%sDIR must be an absolute existing directory", founderEnv)
	}
	if o.decode == "" {
		o.decode = "new"
	}
	if o.decode != "new" && o.decode != "legacy" {
		return o, fmt.Errorf("%sDECODE must be new or legacy", founderEnv)
	}
	number := func(name string, fallback, lo, hi int64) (int64, error) {
		s := getenv(founderEnv + name)
		if s == "" {
			return fallback, nil
		}
		if strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return 0, fmt.Errorf("%s%s must contain decimal digits", founderEnv, name)
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < lo || n > hi {
			return 0, fmt.Errorf("%s%s must be between %d and %d", founderEnv, name, lo, hi)
		}
		return n, nil
	}
	for _, v := range []struct {
		name        string
		def, lo, hi int64
		dst         *int
	}{
		{"TRANSCRIPT_ENTRIES", 300000, 1000, 2000000, &o.entries}, {"EMAILS", 2, 0, 8, &o.emails},
		{"EMAIL_BYTES", 10485760, 1024, 26214400, &o.emailBytes}, {"MEMORIES", 2000, 10, 100000, &o.memories},
	} {
		n, err := number(v.name, v.def, v.lo, v.hi)
		if err != nil {
			return o, err
		}
		*v.dst = int(n)
	}
	for _, v := range []struct {
		name        string
		def, lo, hi int64
		dst         *int64
	}{
		{"FAKE_CGROUP_BYTES", 0, 1, 1<<62 - 1, &o.fakeLimit}, {"EXPECT_CGROUP_BYTES", 0, 1, 1<<62 - 1, &o.expectLimit},
		{"MAX_GO_TOTAL_BYTES", 234881024, 67108864, 1073741824, &o.maxTotal},
		{"MAX_GO_LIVE_BYTES", 134217728, 33554432, 1073741824, &o.maxLive},
		{"MAX_ANON_BYTES", 251658240, 67108864, 1073741824, &o.maxAnon},
	} {
		n, err := number(v.name, v.def, v.lo, v.hi)
		if err != nil {
			return o, err
		}
		*v.dst = n
	}
	return o, nil
}

func validateFounderMemoryBounds(o founderArchiveOptions, cgroupLimit int64) error {
	if o.decode == "legacy" || cgroupLimit <= 0 {
		return nil
	}
	for _, bound := range []struct {
		name  string
		bytes int64
	}{
		{"MAX_GO_TOTAL_BYTES", o.maxTotal},
		{"MAX_GO_LIVE_BYTES", o.maxLive},
		{"MAX_ANON_BYTES", o.maxAnon},
	} {
		if bound.bytes > cgroupLimit {
			return fmt.Errorf("%s%s must not exceed the detected cgroup limit %d", founderEnv, bound.name, cgroupLimit)
		}
	}
	return nil
}

type founderArchiveFile struct {
	GzipBytes         int64          `json:"gzip_bytes"`
	UncompressedBytes int64          `json:"uncompressed_bytes"`
	Entries           int            `json:"entries"`
	Rows              map[string]int `json:"rows"`
}

type founderArchiveManifest struct {
	AccountID    string `json:"account_id"`
	BackupID     string `json:"backup_id"`
	EvacuationID string `json:"evacuation_id"`
	// The evacuation ID is the opaque lifecycle epoch; it is not a counter.
	Epoch      string             `json:"epoch"`
	Backup     founderArchiveFile `json:"backup"`
	Evacuation founderArchiveFile `json:"evacuation"`
}

func runFounderArchiveLoad(t *testing.T, dsn string) {
	t.Helper()
	o, err := parseFounderArchiveOptions(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("founder profile requires a test timeout")
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline.Add(-time.Minute))
	defer cancel()
	if o.phase == "seed-export" {
		seedFounderArchiveLoad(ctx, t, dsn, o)
		return
	}
	importFounderArchiveLoad(ctx, t, dsn, o)
}

func seedFounderArchiveLoad(ctx context.Context, t *testing.T, dsn string, o founderArchiveOptions) {
	started := time.Now()
	opts, err := loadquality.ParseArchiveOptions(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	p, _, err := provisionMemoryArchiveLoadPrincipals(ctx, st, opts.Seed, "founder", false)
	if err != nil {
		t.Fatal(err)
	}
	seedImportStateTranscripts(ctx, t, st, p, 64, o.entries)
	sizes := make([]int, o.emails)
	for i := range sizes {
		sizes[i] = o.emailBytes
	}
	// Calibration files are not durable founder artifacts.
	seedImportStateEmails(ctx, t, st, p, sizes, false, uint64(opts.Seed), t.TempDir())
	if err := seedFounderMemories(ctx, st, p, opts, o.memories); err != nil {
		t.Fatal(err)
	}
	fixture := writeImportStateArchives(ctx, t, st, p.AccountID, o.dir)
	m := founderArchiveManifest{AccountID: p.AccountID, BackupID: fixture.BackupID, EvacuationID: fixture.EvacuationID, Epoch: fixture.EvacuationID}
	m.Backup = measureFounderArchive(ctx, t, fixture.BackupPath)
	m.Evacuation = measureFounderArchive(ctx, t, fixture.EvacuationPath)
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.dir, "founder.json"), append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	logFounderResult(t, map[string]any{"phase": o.phase, "decode": o.decode, "seed_export_ms": time.Since(started).Milliseconds(), "backup": m.Backup, "evacuation": m.Evacuation})
}

func measureFounderArchive(ctx context.Context, t *testing.T, path string) founderArchiveFile {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, z)
	if err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	r := founderArchiveFile{GzipBytes: info.Size(), UncompressedBytes: n, Rows: map[string]int{}}
	_, err = archiveexport.Read(ctx, f, archiveexport.ImportOptions{CurrentSchema: SchemaVersion(), Row: func(table string, _ []byte) error { r.Rows[table]++; return nil }, OnEntry: func(_ archiveexport.EntryStats) { r.Entries++ }})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Use the ladder's APIs and ratios without its 10,000-memory cardinality cap.
// The founder's transcript shape is independently and exactly controlled above;
// evidence here is synthetic system evidence, not additional transcript rows.
func seedFounderMemories(ctx context.Context, st *Store, p Principal, opts loadquality.ArchiveOptions, count int) error {
	memories := make([]Memory, 0, count)
	for i := 0; i < count; i++ {
		tags := make([]string, opts.TagsPerVersion)
		for j := range tags {
			tags[j] = fmt.Sprintf("founder_%02d", j)
		}
		evidence := make([]MemoryEvidenceInput, opts.EvidencePerMemory)
		for j := range evidence {
			evidence[j] = MemoryEvidenceInput{Type: "system", ResolutionState: MemoryEvidenceUnavailable, TerminalReasonCode: "synthetic_fixture"}
		}
		stem := fmt.Sprintf("Synthetic founder archive memory %s ordinal %d", memoryArchiveLoadToken(opts.Seed, "founder", fmt.Sprint(i)), i)
		captured, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{Content: stem + " revision 1.", Kind: "observation", Tags: tags, Evidence: evidence, CaptureReason: "load_test", Client: MemoryClientProvenance{Runtime: "archive-load", Recipe: loadquality.ArchiveResultSchemaV1, RecipeVersion: loadquality.ArchiveHarnessVersion}, IdempotencyKey: fmt.Sprintf("founder-capture-%d-%d", opts.Seed, i)})
		if err != nil {
			return fmt.Errorf("capture founder memory %d: %w", i, err)
		}
		current := captured.Memory
		for v := 2; v <= opts.VersionsPerMemory; v++ {
			content := fmt.Sprintf("%s revision %d.", stem, v)
			adjusted, err := st.AdjustMemory(ctx, p, current.ID, AdjustMemoryInput{ExpectedVersion: current.Version, Content: &content, Reason: "archive load version fixture", Client: MemoryClientProvenance{Runtime: "archive-load", Recipe: loadquality.ArchiveResultSchemaV1, RecipeVersion: loadquality.ArchiveHarnessVersion}, IdempotencyKey: fmt.Sprintf("founder-adjust-%d-%d-%d", opts.Seed, i, v)})
			if err != nil {
				return fmt.Errorf("adjust founder memory %d: %w", i, err)
			}
			current = adjusted.Memory
		}
		memories = append(memories, current)
	}
	if err := seedMemoryArchiveLoadRelations(ctx, st, p, opts, 0, memories); err != nil {
		return err
	}
	profile, err := st.CreateMemoryVectorProfile(ctx, p, CreateMemoryVectorProfileInput{Provider: "synthetic", Model: "sha256-expanded-unit", Recipe: "archive-load", RecipeVersion: "founder", Dimensions: opts.VectorDimensions, DistanceMetric: MemoryVectorMetricCosine, Normalization: MemoryVectorNormalizationL2})
	if err != nil {
		return err
	}
	for i, m := range memories {
		vector, err := loadquality.DeterministicVector(opts.Seed, int64(i), opts.VectorDimensions)
		if err != nil {
			return err
		}
		if _, err := st.PutMemoryVector(ctx, p, PutMemoryVectorInput{ProfileID: profile.ID, MemoryID: m.ID, MemoryVersion: m.Version, ContentHash: m.ContentHash, Vector: vector}); err != nil {
			return err
		}
	}
	return nil
}

type founderMemoryPeaks struct {
	mu                                 sync.Mutex
	total, live, anon, file, peak, hwm int64
}

func (p *founderMemoryPeaks) sample(sample func() memlimit.Snapshot) memlimit.Snapshot {
	s := sample()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.total = max(p.total, s.GoTotal)
	p.live = max(p.live, s.GoLive)
	p.anon = max(p.anon, s.Anon)
	p.file = max(p.file, s.File)
	p.peak = s.MemoryPeak
	p.hwm = s.RSSHWM
	return s
}

func importFounderArchiveLoad(ctx context.Context, t *testing.T, dsn string, o founderArchiveOptions) {
	raw, err := os.ReadFile(filepath.Join(o.dir, "founder.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m founderArchiveManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	oldLimit := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(oldLimit)
	var limit memlimit.Result
	sample := memlimit.Sample
	if o.fakeLimit > 0 {
		root := t.TempDir()
		dir := filepath.Join(root, "sys/fs/cgroup")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(strconv.FormatInt(o.fakeLimit, 10)), 0600); err != nil {
			t.Fatal(err)
		}
		limit = memlimit.ConfigureFrom(root, os.LookupEnv, debug.SetMemoryLimit, os.Stderr, "founder-archive-load")
		sample = func() memlimit.Snapshot { return memlimit.SampleFrom(root, limit.CgroupDir) }
	} else {
		limit = memlimit.Configure(os.Stderr, "founder-archive-load")
	}
	if err := validateFounderMemoryBounds(o, limit.CgroupLimit); err != nil {
		t.Fatal(err)
	}
	p := &founderMemoryPeaks{total: -1, live: -1, anon: -1, file: -1, peak: -1, hwm: -1}
	measuredSample := func() memlimit.Snapshot { return p.sample(sample) }
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				measuredSample()
			case <-stop:
				return
			}
		}
	}()
	defer func() { close(stop); <-done }()
	observer := NewImportTelemetryLogger(os.Stderr, "founder-archive-load", measuredSample)
	dest, destDSN := newMigrationTestStore(t, dsn)
	if err := dest.Migrate(); err != nil {
		t.Fatal(err)
	}
	options := []Option{WithImportObserver(observer)}
	if o.decode == "legacy" {
		options = append(options, withLegacyImportRowDecodeForTest())
	}
	st, err := Open(ctx, destDSN, options...)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	started := time.Now()
	validateErr := withFounderSpool(ctx, filepath.Join(o.dir, "backup.tar.gz"), "witself-backup-validate-*.tar.gz", func(f *os.File) error {
		lease, err := st.AcquireBackupValidationLease(ctx, m.AccountID, m.BackupID)
		if err != nil {
			return err
		}
		defer lease.Close()
		_, err = lease.Validate(ctx, f)
		return err
	})
	validateDuration := time.Since(started)
	// Successful validation must leave the destination account absent.
	if validateErr == nil {
		var count int
		if err := st.pool.QueryRow(ctx, "SELECT count(*) FROM accounts WHERE id=$1", m.AccountID).Scan(&count); err != nil {
			validateErr = err
		} else if count != 0 {
			validateErr = fmt.Errorf("validation retained an account")
		}
	}
	started = time.Now()
	importErr := withFounderSpool(ctx, filepath.Join(o.dir, "evacuation.tar.gz"), "witself-account-import-*.tar.gz", func(f *os.File) error {
		lease, err := st.AcquireAccountImportLease(ctx, m.AccountID, m.EvacuationID)
		if err != nil {
			return err
		}
		defer lease.Close()
		_, _, err = lease.Import(ctx, f)
		return err
	})
	importDuration := time.Since(started)
	measuredSample()
	p.mu.Lock()
	total, live, anon := p.total, p.live, p.anon
	result := map[string]any{"phase": o.phase, "decode": o.decode, "source": limit.Source, "cgroup_limit": limit.CgroupLimit, "soft_limit": limit.SoftLimit, "validate_outcome": importOutcome(validateErr), "import_outcome": importOutcome(importErr), "peak_go_total": total, "max_go_live": live, "max_anon": anon, "max_file": p.file, "memory_peak": p.peak, "vm_hwm": p.hwm, "max_go_total_bytes": o.maxTotal, "max_go_live_bytes": o.maxLive, "max_anon_bytes": o.maxAnon, "validate_ms": validateDuration.Milliseconds(), "import_ms": importDuration.Milliseconds(), "backup_rows": m.Backup.Rows, "import_rows": m.Evacuation.Rows, "backup_entries": m.Backup.Entries, "import_entries": m.Evacuation.Entries}
	p.mu.Unlock()
	logFounderResult(t, result)
	// Defaults are interim bounds for 112 without 111: total soft+32 MiB,
	// live 128 MiB, anon L-16 MiB at L=256 MiB. Email live is about
	// 25+32+10+22=89 MiB (under the conservative 94 MiB estimate); transcript
	// live is about 25+32+14+5=76 MiB plus memory maps. The dispatcher sets
	// total and anon to 180 MiB on the combined 111+112 head. Page cache makes
	// memory.peak informational. The legacy run is a recorded, ungated control.
	if o.decode == "new" {
		if o.expectLimit > 0 && limit.SoftLimit != o.expectLimit/4*3 {
			t.Errorf("soft limit %d does not match expected three-quarter policy", limit.SoftLimit)
		}
		if o.expectLimit > 0 && (limit.CgroupLimit != o.expectLimit || (limit.Source != "cgroup-v1" && limit.Source != "cgroup-v2")) {
			t.Errorf("detected cgroup does not match expected limit/source")
		}
		if validateErr != nil {
			t.Errorf("founder validation: %v", validateErr)
		}
		if importErr != nil {
			t.Errorf("founder import: %v", importErr)
		}
		if total > o.maxTotal {
			t.Errorf("peak Go total %d exceeds %d", total, o.maxTotal)
		}
		if live > o.maxLive {
			t.Errorf("max Go live %d exceeds %d", live, o.maxLive)
		}
		if anon >= 0 && anon > o.maxAnon {
			t.Errorf("max anon %d exceeds %d", anon, o.maxAnon)
		}
	}
}

func withFounderSpool(ctx context.Context, path, pattern string, run func(*os.File) error) error {
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	spool, err := os.CreateTemp("", pattern)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(spool.Name()) }()
	defer func() { _ = spool.Close() }()
	// Interface-only wrappers prevent WriterTo/ReaderFrom bypassing the 32 KiB
	// buffer; the copy still writes a real file, charging the spool page cache.
	if _, err := io.CopyBuffer(struct{ io.Writer }{spool}, struct{ io.Reader }{source}, make([]byte, 32<<10)); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return run(spool)
}

func logFounderResult(t *testing.T, result any) {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("founder-archive-load result %s\n", raw)
}

func TestFounderArchiveOptions(t *testing.T) {
	dir := t.TempDir()
	base := map[string]string{founderEnv + "PHASE": "import", founderEnv + "DIR": dir}
	parse := func(changes map[string]string) (founderArchiveOptions, error) {
		return parseFounderArchiveOptions(func(key string) string {
			if value, ok := changes[key]; ok {
				return value
			}
			return base[key]
		})
	}
	want := founderArchiveOptions{phase: "import", dir: dir, decode: "new", entries: 300000, emails: 2, emailBytes: 10485760, memories: 2000, maxTotal: 234881024, maxLive: 134217728, maxAnon: 251658240}
	got, err := parse(nil)
	if err != nil || got != want {
		t.Fatalf("default founder options = %+v, %v; want %+v", got, err, want)
	}
	optional := []string{"DECODE", "TRANSCRIPT_ENTRIES", "EMAILS", "EMAIL_BYTES", "MEMORIES", "FAKE_CGROUP_BYTES", "EXPECT_CGROUP_BYTES", "MAX_GO_TOTAL_BYTES", "MAX_GO_LIVE_BYTES", "MAX_ANON_BYTES"}
	empty := make(map[string]string)
	for _, name := range optional {
		empty[founderEnv+name] = ""
	}
	if got, err := parse(empty); err != nil || got != want {
		t.Fatalf("exported empty options = %+v, %v; want defaults", got, err)
	}
	for _, test := range []struct{ name, min, max, below, above string }{
		{"TRANSCRIPT_ENTRIES", "1000", "2000000", "999", "2000001"},
		{"EMAILS", "0", "8", "-1", "9"},
		{"EMAIL_BYTES", "1024", "26214400", "1023", "26214401"},
		{"MEMORIES", "10", "100000", "9", "100001"},
		{"FAKE_CGROUP_BYTES", "1", "4611686018427387903", "0", "4611686018427387904"},
		{"EXPECT_CGROUP_BYTES", "1", "4611686018427387903", "0", "4611686018427387904"},
		{"MAX_GO_TOTAL_BYTES", "67108864", "1073741824", "67108863", "1073741825"},
		{"MAX_GO_LIVE_BYTES", "33554432", "1073741824", "33554431", "1073741825"},
		{"MAX_ANON_BYTES", "67108864", "1073741824", "67108863", "1073741825"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, value := range []string{test.min, test.max} {
				if _, err := parse(map[string]string{founderEnv + test.name: value}); err != nil {
					t.Errorf("valid endpoint %q: %v", value, err)
				}
			}
			for _, value := range []string{test.below, test.above, "+1", " 1", "1 ", "1.0", "1e3", "１", "9223372036854775808"} {
				if _, err := parse(map[string]string{founderEnv + test.name: value}); err == nil {
					t.Errorf("accepted invalid numeric value %q", value)
				}
			}
		})
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, value string }{
		{"PHASE", ""}, {"PHASE", "seed"}, {"DECODE", "fast"},
		{"DIR", ""}, {"DIR", "."}, {"DIR", file}, {"DIR", filepath.Join(dir, "missing")},
	} {
		if _, err := parse(map[string]string{founderEnv + test.name: test.value}); err == nil {
			t.Errorf("accepted invalid %s", test.name)
		}
	}
	if got, err := parse(map[string]string{founderEnv + "PHASE": "seed-export", founderEnv + "DECODE": "legacy"}); err != nil || got.phase != "seed-export" || got.decode != "legacy" {
		t.Fatalf("valid alternate phase/decode: %+v, %v", got, err)
	}
}

func TestFounderMemoryBounds(t *testing.T) {
	const limit int64 = 268435456
	for _, test := range []struct {
		name, decode, invalid    string
		limit, total, live, anon int64
	}{
		{"below_limit", "new", "", limit, 234881024, 134217728, 251658240},
		{"equal_limit", "new", "", limit, limit, limit, limit},
		{"total_above_limit", "new", "MAX_GO_TOTAL_BYTES", limit, limit + 1, limit, limit},
		{"live_above_limit", "new", "MAX_GO_LIVE_BYTES", limit, limit, limit + 1, limit},
		{"anon_above_limit", "new", "MAX_ANON_BYTES", limit, limit, limit, limit + 1},
		{"unknown_zero", "new", "", 0, limit, limit, limit},
		{"unknown_negative", "new", "", -1, limit, limit, limit},
		{"legacy_control", "legacy", "", limit, limit + 1, limit + 1, limit + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := founderArchiveOptions{decode: test.decode, maxTotal: test.total, maxLive: test.live, maxAnon: test.anon}
			err := validateFounderMemoryBounds(o, test.limit)
			if test.invalid == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), founderEnv+test.invalid) {
				t.Fatalf("expected refusal for %s, got %v", test.invalid, err)
			}
		})
	}
}
