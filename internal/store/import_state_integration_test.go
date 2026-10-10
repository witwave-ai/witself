package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/agentemail"
	archiveexport "github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/memlimit"
	"github.com/witwave-ai/witself/internal/testenv"
)

type importStateFixture struct {
	AccountID, BackupID, EvacuationID string
	BackupPath, EvacuationPath        string
	// The evacuation ID is the archive's opaque epoch, not a numeric counter.
	Epoch string
}

// seedImportStateTranscripts issues exactly one bulk INSERT for all entries.
// Body entropy keeps gzip close to real transcript archives. Every eighth row
// within a conversation replies to its preceding row, in exporter sequence order.
func seedImportStateTranscripts(ctx context.Context, t *testing.T, st *Store, p Principal, transcriptCount, entries int) {
	t.Helper()
	transcripts := make([]string, transcriptCount)
	for i := range transcripts {
		tr, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID, CreateTranscriptInput{ExternalID: fmt.Sprintf("s112-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		transcripts[i] = tr.ID
	}
	_, err := st.pool.Exec(ctx, `
 WITH fixture AS (
  SELECT g, (($5::text[])[1+(g-1)%$6]) AS transcript_id,
    1+(g-1)/$6 AS sequence,
    'ent_' || translate(substr(md5('s112-' || g),1,16),'0189','wxyz') AS id
  FROM generate_series(1,$4::int) g
 )
 INSERT INTO transcript_entries
  (id,account_id,transcript_id,realm_id,recorded_by_agent_id,sequence,
   external_id,role,body,payload,model,reply_to_entry_id,artifacts)
 SELECT id,$1,transcript_id,$2,$3,sequence,'s112-' || g,'user',
  (SELECT string_agg(md5(g || '-' || i),'') FROM generate_series(1,100) i),
  NULL,NULL,CASE WHEN sequence%8=0 THEN
   'ent_' || translate(substr(md5('s112-' || (g-$6)),1,16),'0189','wxyz') END,'[]'::jsonb
 FROM fixture`, p.AccountID, p.RealmID, p.ID, entries, transcripts, transcriptCount)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.pool.Exec(ctx, `UPDATE transcript_conversations c SET next_sequence=
  1+(SELECT count(*) FROM transcript_entries e WHERE e.transcript_id=c.id)
  WHERE c.id=ANY($1)`, transcripts)
	if err != nil {
		t.Fatal(err)
	}
}

// importStateMIME produces a valid multipart message just below target bytes.
// The attachment is seeded random data and base64 lines are MIME-sized.
func importStateMIME(t *testing.T, target int, seed uint64) []byte {
	t.Helper()
	const head = "From: sender@example.com\r\nTo: owner@example.com\r\nSubject: import state fixture\r\nDate: Fri, 09 Oct 2026 12:00:00 +0000\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=s112-boundary\r\n\r\n--s112-boundary\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nSynthetic import state body.\r\n--s112-boundary\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=fixture.bin\r\nContent-Transfer-Encoding: base64\r\n\r\n"
	const tail = "--s112-boundary--\r\n"
	// Each full 57-byte input line encodes to 76 bytes plus CRLF.
	n := (target - len(head) - len(tail)) / 78 * 57
	if n <= 0 {
		t.Fatal("MIME fixture target too small")
	}
	var seedBytes [32]byte
	for i := range seedBytes {
		seedBytes[i] = byte(seed>>uint((i%8)*8)) + byte(i)
	}
	rng := rand.NewChaCha8(seedBytes)
	attachment := make([]byte, n)
	for i := 0; i < len(attachment); i += 8 {
		v := rng.Uint64()
		for j := 0; j < 8 && i+j < len(attachment); j++ {
			attachment[i+j] = byte(v >> uint(j*8))
		}
	}
	var buf bytes.Buffer
	buf.Grow(target)
	buf.WriteString(head)
	var encoded [76]byte
	for len(attachment) > 0 {
		base64.StdEncoding.Encode(encoded[:], attachment[:57])
		buf.Write(encoded[:])
		buf.WriteString("\r\n")
		attachment = attachment[57:]
	}
	buf.WriteString(tail)
	if buf.Len() > target || target-buf.Len() >= 64<<10 {
		t.Fatal("MIME fixture size outside required range")
	}
	return buf.Bytes()
}

// The returned files are calibration inputs. No MIME byte slice survives this helper.
func seedImportStateEmails(ctx context.Context, t *testing.T, st *Store, p Principal, rawSizes []int, omitted bool, seed uint64, dir string) []string {
	t.Helper()
	if len(rawSizes) == 0 && !omitted {
		return nil
	}
	// The pilot contract requires five to ten enrolled agents, as in the
	// archive roundtrip fixture; only the owner needs a mailbox here.
	enrolled := map[string]bool{p.ID: true}
	for i := 0; i < 4; i++ {
		agent, err := st.CreateAgent(ctx, p.AccountID, p.RealmID, fmt.Sprintf("email-pilot-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		enrolled[agent.ID] = true
	}
	scope := AgentEmailPilotScope{Enabled: true, Domain: "agent-mail.witwave.ai", Audience: "archive-pilot", RealmIDs: map[string]bool{p.RealmID: true}, AgentIDs: enrolled}
	address, err := st.EnsureAgentEmailMailbox(ctx, scope, p.AccountID, p.RealmID, p.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	files := make([]string, 0, len(rawSizes))
	sizes := append([]int(nil), rawSizes...)
	if omitted {
		sizes = append(sizes, 1024)
	}
	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	for i, size := range sizes {
		raw := importStateMIME(t, size, seed+uint64(i))
		parsed, err := agentemail.ParseMessage(raw, true)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(raw)
		hash := hex.EncodeToString(digest[:])
		messageID, err := id.New("emsg")
		if err != nil {
			t.Fatal(err)
		}
		var stored any = raw
		state := "retained"
		retained := len(raw)
		if i == len(rawSizes) {
			stored = nil
			state = "omitted_capacity"
			retained = 0
		} else {
			path := filepath.Join(dir, fmt.Sprintf("raw-%d.mime", i))
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			files = append(files, path)
		}
		_, err = st.pool.Exec(ctx, `INSERT INTO agent_email_messages
   (id,account_id,realm_id,mailbox_id,owner_agent_id,address_id,
    provider,envelope_sender,envelope_recipient,agent_segment,realm_label,
    raw_mime,raw_size_bytes,raw_sha256,parse_state,header_from,header_to,
    header_subject,mime_message_id,message_date,attachment_count,body_text,
    body_text_kind,attachment_storage_bytes,retained_attachment_storage_bytes,
    payload_retention_state,spf_result,dkim_result,dmarc_result,spam_verdict,
    sender_verification_state,duplicate_group_sha256,received_at)
   VALUES ($1,$2,$3,$4,$5,$6,'cloudflare_email_routing','sender@example.com',
    $7,$8,$9,$10,$11,$12,'parsed',$13,$14,$15,$16,$17,$18,$19,$20,$11,$21,$22,
    'unknown','unknown','unknown','unknown','unverified',$23,clock_timestamp())`,
			messageID, p.AccountID, p.RealmID, address.MailboxID, p.ID, address.ID, address.Address,
			address.AgentSegment, address.RealmLabel, stored, len(raw), hash, nullable(parsed.HeaderFrom),
			nullable(parsed.HeaderTo), nullable(parsed.HeaderSubject), nullable(parsed.MIMEMessageID), parsed.MessageDate,
			parsed.AttachmentCount, nullable(parsed.Text), nullable(parsed.TextKind), retained, state,
			agentEmailDuplicateGroup(hash, address.Address, "sender@example.com"))
		if err != nil {
			t.Fatal(err)
		}
		// Every portable email requires its mailbox delivery; keep it available
		// so importing never performs lease or processing-state normalization.
		if _, err := st.pool.Exec(ctx, `INSERT INTO agent_email_deliveries
			(message_id,account_id,realm_id,mailbox_id,owner_agent_id)
			VALUES ($1,$2,$3,$4,$5)`, messageID, p.AccountID, p.RealmID, address.MailboxID, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func writeImportStateArchives(ctx context.Context, t *testing.T, st *Store, accountID, dir string) importStateFixture {
	t.Helper()
	f := importStateFixture{AccountID: accountID, BackupID: "backup_s112_" + accountID, EvacuationID: "evac_s112_" + accountID,
		BackupPath: filepath.Join(dir, "backup.tar.gz"), EvacuationPath: filepath.Join(dir, "evacuation.tar.gz")}
	f.Epoch = f.EvacuationID
	write := func(path string, fn func(io.Writer) error) {
		out, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		writeErr := fn(out)
		closeErr := out.Close()
		if writeErr != nil {
			t.Fatal(writeErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	write(f.BackupPath, func(w io.Writer) error {
		return st.ExportAccountBackup(ctx, accountID, f.BackupID, "s112-source", "test", w)
	})
	if _, err := st.BeginAccountEvacuation(ctx, accountID, f.EvacuationID, "import state fixture"); err != nil {
		t.Fatal(err)
	}
	write(f.EvacuationPath, func(w io.Writer) error {
		return st.ExportAccountEvacuation(ctx, accountID, f.EvacuationID, "s112-source", "test", w)
	})
	if _, err := st.AbortAccountEvacuation(ctx, accountID, f.EvacuationID); err != nil {
		t.Fatal(err)
	}
	return f
}

type importStateSeed struct {
	source   *Store
	fixture  importStateFixture
	rawPaths []string
}

func seedImportStateFixture(ctx context.Context, t *testing.T, dsn string, transcripts, entries, memories, versions int, rawSizes []int, omitted, message bool) importStateSeed {
	t.Helper()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	account := provisionActiveEvacuationTestAccount(ctx, t, st, "s112")
	// Run only after all assertions and before the provisioning helper's older
	// cleanup. Row-wise transcript deletion repeatedly checks its reply FK and
	// evidence triggers, making the large fixture's teardown potentially
	// quadratic. Truncate exactly its three-table incoming FK closure, then
	// delete the remaining portable graph normally. In particular, email DELETE
	// triggers must run to preserve schema 0091's cell-storage accounting guard.
	// newMigrationTestStore remains responsible for dropping the whole schema.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		tx, err := st.pool.Begin(cleanupCtx)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = tx.Rollback(cleanupCtx) }()
		var schema string
		if err := tx.QueryRow(cleanupCtx, `SELECT current_schema()`).Scan(&schema); err != nil {
			t.Error(err)
			return
		}
		if !strings.HasPrefix(schema, "witself_migration_") {
			t.Error("import-state cleanup requires its dedicated migration schema")
			return
		}
		var epoch *string
		if err := tx.QueryRow(cleanupCtx, `SELECT evacuation_id FROM accounts WHERE id=$1`, account.AccountID).Scan(&epoch); err != nil {
			t.Error(err)
			return
		}
		if epoch != nil {
			if err := setEvacuationAuthorityTx(cleanupCtx, tx, *epoch); err != nil {
				t.Error(err)
				return
			}
		}
		// The closure is transcript_entries (including its self reply FK),
		// memory_evidence (0029), and memory_curation_run_inputs (0030).
		// Spell it out without CASCADE so future schema changes cannot silently
		// expand teardown into guarded email tables.
		tables := []string{
			pgx.Identifier{schema, "transcript_entries"}.Sanitize(),
			pgx.Identifier{schema, "memory_evidence"}.Sanitize(),
			pgx.Identifier{schema, "memory_curation_run_inputs"}.Sanitize(),
		}
		if _, err := tx.Exec(cleanupCtx, "TRUNCATE "+strings.Join(tables, ", ")); err != nil {
			t.Error(err)
			return
		}
		if _, err := purgePortableAccountRowsTx(cleanupCtx, tx, account.AccountID, SchemaVersion()); err != nil {
			t.Error(err)
			return
		}
		if _, err := tx.Exec(cleanupCtx, `DELETE FROM accounts WHERE id=$1`, account.AccountID); err != nil {
			t.Error(err)
			return
		}
		if err := tx.Commit(cleanupCtx); err != nil {
			t.Error(err)
		}
	})
	realm, err := st.CreateRealm(ctx, account.AccountID, "import-state")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: account.AccountID, RealmID: realm.ID, AgentName: agent.Name, RealmName: realm.Name, AccountStatus: "active"}
	seedImportStateTranscripts(ctx, t, st, p, transcripts, entries)
	for i := 0; i < memories; i++ {
		captured, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{Content: fmt.Sprintf("Synthetic import state memory %d revision 1.", i), Kind: "observation", CaptureReason: "load_test", IdempotencyKey: fmt.Sprintf("s112-capture-%d", i), Evidence: []MemoryEvidenceInput{{Type: "system", ResolutionState: MemoryEvidenceUnavailable, TerminalReasonCode: "synthetic_fixture"}}})
		if err != nil {
			t.Fatal(err)
		}
		current := captured.Memory
		for version := 2; version <= versions; version++ {
			content := fmt.Sprintf("Synthetic import state memory %d revision %d.", i, version)
			adjusted, err := st.AdjustMemory(ctx, p, current.ID, AdjustMemoryInput{ExpectedVersion: current.Version, Content: &content, Reason: "import state fixture", IdempotencyKey: fmt.Sprintf("s112-adjust-%d-%d", i, version)})
			if err != nil {
				t.Fatal(err)
			}
			current = adjusted.Memory
		}
	}
	if message {
		// Three extra early replies supplement the six every-eighth replies.
		if _, err := st.pool.Exec(ctx, `UPDATE transcript_entries e SET reply_to_entry_id=p.id
   FROM transcript_entries p WHERE e.account_id=$1 AND e.sequence=2
    AND p.transcript_id=e.transcript_id AND p.sequence=1`, p.AccountID); err != nil {
			t.Fatal(err)
		}
		peer, err := st.CreateAgent(ctx, p.AccountID, p.RealmID, "peer")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.SendMessage(ctx, p, SendMessageInput{ToAgent: peer.ID, Kind: "request", Body: "Synthetic import ownership request", IdempotencyKey: "s112-message"}); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	rawPaths := seedImportStateEmails(ctx, t, st, p, rawSizes, omitted, 112, dir)
	fixture := writeImportStateArchives(ctx, t, st, p.AccountID, dir)
	return importStateSeed{source: st, fixture: fixture, rawPaths: rawPaths}
}

func importStateDestination(ctx context.Context, t *testing.T, dsn string, options ...Option) (*Store, string) {
	t.Helper()
	st, scoped := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if len(options) == 0 {
		return st, scoped
	}
	configured, err := Open(ctx, scoped, options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(configured.Close)
	return configured, scoped
}

func runImportStateOperation(ctx context.Context, st *Store, f importStateFixture, operation string) error {
	path := f.EvacuationPath
	if operation == "validate" {
		path = f.BackupPath
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if operation == "validate" {
		lease, err := st.AcquireBackupValidationLease(ctx, f.AccountID, f.BackupID)
		if err != nil {
			return err
		}
		defer lease.Close()
		_, err = lease.Validate(ctx, file)
		return err
	}
	lease, err := st.AcquireAccountImportLease(ctx, f.AccountID, f.EvacuationID)
	if err != nil {
		return err
	}
	defer lease.Close()
	_, _, err = lease.Import(ctx, file)
	return err
}

// Project only portable columns: generated search columns and newly added
// destination defaults are deliberately outside the archive's content contract.
// All fixture-filled portable tables are included, beyond the load harness's
// memory/transcript content list. No import-time derived-column exclusion is
// needed: this fixture has no active leases or claimed deliveries.
func importStateContentDigests(ctx context.Context, t *testing.T, st *Store, accountID string) map[string]string {
	t.Helper()
	digests := make(map[string]string, len(canonicalArchiveTables))
	for _, table := range canonicalArchiveTables {
		if table.name == "accounts" {
			continue
		}
		columns := make([]string, 0, len(importColumns[table.name]))
		for column := range importColumns[table.name] {
			columns = append(columns, column)
		}
		sort.Strings(columns)
		predicate := "t.account_id=$1"
		switch table.name {
		case "agents":
			predicate = "t.realm_id IN (SELECT id FROM realms WHERE account_id=$1)"
		case "agent_activity":
			predicate = "t.agent_id IN (SELECT a.id FROM agents a JOIN realms r ON r.id=a.realm_id WHERE r.account_id=$1)"
		}
		query := fmt.Sprintf(`SELECT md5(COALESCE(string_agg(row_hash,'' ORDER BY row_hash),''))
   FROM (SELECT md5((SELECT jsonb_object_agg(key,value)
    FROM jsonb_each(to_jsonb(t)) WHERE key=ANY($2))::text) AS row_hash
    FROM %s t WHERE %s) rows`, pgx.Identifier{table.name}.Sanitize(), predicate)
		var digest string
		if err := st.pool.QueryRow(ctx, query, accountID, columns).Scan(&digest); err != nil {
			t.Fatalf("digest %s: %v", table.name, err)
		}
		digests[table.name] = digest
	}
	return digests
}

func assertImportStateDigests(t *testing.T, want, got map[string]string) {
	t.Helper()
	for table, expected := range want {
		if got[table] != expected {
			t.Errorf("portable content differs in table %s", table)
		}
	}
}

type importStateEmailRow struct {
	ID                   string
	Raw                  []byte
	RawSize              int64
	Hash                 string
	Attachment, Retained int64
	State                string
	Body                 *string
}

func importStateEmailRows(ctx context.Context, t *testing.T, st *Store, accountID string) []importStateEmailRow {
	t.Helper()
	rows, err := st.pool.Query(ctx, `SELECT id,raw_mime,raw_size_bytes,raw_sha256,
  attachment_storage_bytes,retained_attachment_storage_bytes,payload_retention_state,body_text
  FROM agent_email_messages WHERE account_id=$1 ORDER BY id`, accountID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []importStateEmailRow
	for rows.Next() {
		var row importStateEmailRow
		if err := rows.Scan(&row.ID, &row.Raw, &row.RawSize, &row.Hash, &row.Attachment, &row.Retained, &row.State, &row.Body); err != nil {
			t.Fatal(err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func assertImportStateStatus(ctx context.Context, t *testing.T, st *Store, accountID string, exists bool) {
	t.Helper()
	var count int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE id=$1`, accountID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if !exists {
		if count != 0 {
			t.Fatal("validation retained an account")
		}
		return
	}
	status, _, err := memoryArchiveLoadAccountStatus(ctx, st, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || status != "suspended" {
		t.Fatalf("account status=%q count=%d", status, count)
	}
}

// Expected wall time under -race: less than three minutes.
func TestImportRawMIMEPathParityPostgres(t *testing.T) {
	started := time.Now()
	defer func() { t.Logf("parity wall_time=%s race=%t", time.Since(started), importPeakRaceEnabled) }()
	dsn := testenv.RequirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	seed := seedImportStateFixture(ctx, t, dsn, 2, 40, 5, 1, []int{1024, 3 << 19}, true, false)
	expected := importStateContentDigests(ctx, t, seed.source, seed.fixture.AccountID)
	// BeginAccountEvacuation (evacuation.go:214) adds the suspension event;
	// AbortAccountEvacuation (evacuation.go:500) later adds a resume event that
	// is absent from the archive. Compare events across all destinations below,
	// and compare every other portable table against the retained source.
	delete(expected, "account_events")
	var importedExpected map[string]string
	expectedEmails := importStateEmailRows(ctx, t, seed.source, seed.fixture.AccountID)
	for _, mode := range []string{"new", "legacy", "clobber"} {
		t.Run(mode, func(t *testing.T) {
			var options []Option
			if mode == "legacy" {
				options = append(options, withLegacyImportRowDecodeForTest())
			}
			if mode == "clobber" {
				options = append(options, withImportRowClobberForTest())
			}
			destination, _ := importStateDestination(ctx, t, dsn, options...)
			if err := runImportStateOperation(ctx, destination, seed.fixture, "import"); err != nil {
				t.Fatal(err)
			}
			got := importStateContentDigests(ctx, t, destination, seed.fixture.AccountID)
			assertImportStateDigests(t, expected, got)
			if importedExpected == nil {
				importedExpected = got
			} else {
				assertImportStateDigests(t, importedExpected, got)
			}
			if !reflect.DeepEqual(expectedEmails, importStateEmailRows(ctx, t, destination, seed.fixture.AccountID)) {
				t.Fatal("email bytea or projection differs from source")
			}
			assertImportStateStatus(ctx, t, destination, seed.fixture.AccountID, true)
		})
	}
	for _, clobber := range []bool{false, true} {
		var options []Option
		if clobber {
			options = append(options, withImportRowClobberForTest())
		}
		destination, _ := importStateDestination(ctx, t, dsn, options...)
		if err := runImportStateOperation(ctx, destination, seed.fixture, "validate"); err != nil {
			t.Fatal(err)
		}
		assertImportStateStatus(ctx, t, destination, seed.fixture.AccountID, false)
	}
}

// Expected wall time under -race: less than two minutes. The deliberately small
// fixture still keeps transcript IDs, reply IDs, evidence and message scopes
// alive beyond the row callback, so row aliasing fails on subsequent lookups.
func TestImportRowOwnershipPostgres(t *testing.T) {
	started := time.Now()
	defer func() { t.Logf("ownership wall_time=%s race=%t", time.Since(started), importPeakRaceEnabled) }()
	dsn := testenv.RequirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// Nine replies among 60 entries exercise delayed lookups with a small fixture.
	seed := seedImportStateFixture(ctx, t, dsn, 3, 60, 5, 2, []int{64 << 10}, true, true)
	baseline, _ := importStateDestination(ctx, t, dsn)
	if err := runImportStateOperation(ctx, baseline, seed.fixture, "import"); err != nil {
		t.Fatal(err)
	}
	expected := importStateContentDigests(ctx, t, baseline, seed.fixture.AccountID)
	for _, operation := range []string{"validate", "import"} {
		destination, _ := importStateDestination(ctx, t, dsn, withImportRowClobberForTest())
		if err := runImportStateOperation(ctx, destination, seed.fixture, operation); err != nil {
			t.Fatal(err)
		}
		assertImportStateStatus(ctx, t, destination, seed.fixture.AccountID, operation == "import")
		if operation == "import" {
			assertImportStateDigests(t, expected, importStateContentDigests(ctx, t, destination, seed.fixture.AccountID))
		}
	}
}

type importStateMemoryResult struct {
	Outcome        string `json:"outcome"`
	SoftLimit      int64  `json:"soft_limit"`
	PeakGoTotal    int64  `json:"peak_go_total"`
	MaxGoLive      int64  `json:"max_go_live"`
	EmailRowBytes  int64  `json:"email_row_bytes"`
	EmailRowAllocs int64  `json:"email_row_allocs"`
	ParseAllocs    int64  `json:"parse_allocs"`
	VMHWM          int64  `json:"vm_hwm"`
}

type importStateMemoryObserver struct {
	mu        sync.Mutex
	samples   []metrics.Sample
	result    importStateMemoryResult
	rowBefore uint64
}

func newImportStateMemoryObserver() *importStateMemoryObserver {
	return &importStateMemoryObserver{samples: []metrics.Sample{
		{Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"},
		{Name: "/gc/heap/live:bytes"}, {Name: "/gc/heap/allocs:bytes"},
	}}
}

func (o *importStateMemoryObserver) sampleLocked() uint64 {
	metrics.Read(o.samples)
	o.result.PeakGoTotal = max(o.result.PeakGoTotal, int64(o.samples[0].Value.Uint64()-o.samples[1].Value.Uint64()))
	o.result.MaxGoLive = max(o.result.MaxGoLive, int64(o.samples[2].Value.Uint64()))
	return o.samples[3].Value.Uint64()
}
func (o *importStateMemoryObserver) sample()                        { o.mu.Lock(); defer o.mu.Unlock(); o.sampleLocked() }
func (o *importStateMemoryObserver) Begin(ImportInfo) ImportTracker { return o }
func (o *importStateMemoryObserver) EntryStart(string, int, int64)  { o.sample() }
func (o *importStateMemoryObserver) Entry(archiveexport.EntryStats) { o.sample() }
func (o *importStateMemoryObserver) Finish(string)                  { o.sample() }
func (o *importStateMemoryObserver) Row(table string, n int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	allocated := o.sampleLocked()
	if table == "agent_email_messages" && n >= 1<<20 {
		o.rowBefore = allocated
	}
}
func (o *importStateMemoryObserver) RowDone(table string, n int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	allocated := o.sampleLocked()
	if table == "agent_email_messages" && n >= 1<<20 {
		delta := int64(allocated - o.rowBefore)
		if delta > o.result.EmailRowAllocs {
			o.result.EmailRowAllocs = delta
			o.result.EmailRowBytes = int64(n)
		}
	}
}

func importStateParseCalibration(t *testing.T, path string) int64 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	samples := []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}}
	metrics.Read(samples)
	before := samples[0].Value.Uint64()
	parsed, err := agentemail.ParseMessage(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	_, present, err := agentemail.RetryCanaryChallenge(raw)
	if err != nil || present {
		t.Fatal("invalid calibration retry-canary shape")
	}
	metrics.Read(samples)
	after := samples[0].Value.Uint64()
	runtime.KeepAlive(parsed)
	runtime.KeepAlive(raw)
	return int64(after - before)
}

func runImportStateMemoryChild(t *testing.T, marker string) importStateMemoryResult {
	t.Helper()
	operation, mode, ok := strings.Cut(marker, ":")
	if !ok || (operation != "validate" && operation != "import") || (mode != "new" && mode != "legacy") {
		t.Fatal("invalid import-state child mode")
	}
	// Calibration inputs and parser results must go out of scope before the two
	// collections. Otherwise calibration itself inflates the measured live set.
	parseAllocs := importStateParseCalibration(t, os.Getenv("WITSELF_IMPORT_STATE_RAW"))
	runtime.GC()
	runtime.GC()
	configuration := memlimit.ConfigureFrom(os.Getenv("WITSELF_IMPORT_STATE_ROOT"), os.LookupEnv, debug.SetMemoryLimit, io.Discard, "import-state-child")
	observer := newImportStateMemoryObserver()
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				observer.sample()
			case <-stop:
				return
			}
		}
	}()
	observer.sample()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	options := []Option{WithImportObserver(observer)}
	if mode == "legacy" {
		options = append(options, withLegacyImportRowDecodeForTest())
	}
	st, err := Open(ctx, os.Getenv("WITSELF_IMPORT_STATE_DSN"), options...)
	if err != nil {
		t.Fatal(err)
	}
	fixture := importStateFixture{AccountID: os.Getenv("WITSELF_IMPORT_STATE_ACCOUNT"), BackupID: os.Getenv("WITSELF_IMPORT_STATE_BACKUP"), EvacuationID: os.Getenv("WITSELF_IMPORT_STATE_EVACUATION"), BackupPath: os.Getenv("WITSELF_IMPORT_STATE_ARCHIVE"), EvacuationPath: os.Getenv("WITSELF_IMPORT_STATE_ARCHIVE")}
	err = runImportStateOperation(ctx, st, fixture, operation)
	observer.sample()
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	result := observer.result
	result.Outcome = "ok"
	result.SoftLimit = configuration.SoftLimit
	result.ParseAllocs = parseAllocs
	result.VMHWM = -1
	if runtime.GOOS == "linux" {
		result.VMHWM = memlimit.SampleFrom("/", "").RSSHWM
	}
	st.Close()
	return result
}

func assertImportStateRealRows(t *testing.T, path string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	count := 0
	_, err = archiveexport.Read(context.Background(), file, archiveexport.ImportOptions{CurrentSchema: SchemaVersion(), Row: func(table string, row []byte) error {
		fast, ok := decodeImportRowFast(table, row)
		if !ok {
			return fmt.Errorf("real exporter row fell back: table=%s row=%d", table, count)
		}
		legacy, err := decodeImportRow(row)
		if err != nil {
			return fmt.Errorf("real exporter row refused by legacy: table=%s", table)
		}
		if raw, ok := fast["raw_mime"].(importedRawMIME); ok {
			fast["raw_mime"] = "\\x" + hex.EncodeToString(raw.raw)
		}
		if !reflect.DeepEqual(fast, legacy) {
			return fmt.Errorf("real exporter row differs: table=%s row=%d", table, count)
		}
		count++
		return nil
	}})
	if err != nil {
		t.Error(err)
		return // Keep measuring children so fallback mutations also exercise allocation gates.
	}
	if count == 0 {
		t.Fatal("archive has no real rows")
	}
	t.Logf("real exporter fast-path rows=%d archive=%s", count, filepath.Base(path))
}

// Expected wall time without -race: less than twelve minutes. DSN skip policy
// belongs only to testenv.RequirePostgres; REQUIRE_PEAK_MEMORY turns only the
// race-detector skip into a failure.
func TestImportStateMemoryPostgres(t *testing.T) {
	if importPeakRaceEnabled {
		message := "NOT RUN TestImportStateMemoryPostgres: race detector"
		if os.Getenv("WITSELF_TEST_REQUIRE_PEAK_MEMORY") == "1" {
			t.Fatal(message)
		}
		t.Skip(message)
	}
	if marker := os.Getenv("WITSELF_IMPORT_STATE_CHILD"); marker != "" {
		result := runImportStateMemoryChild(t, marker)
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}
	started := time.Now()
	defer func() { t.Logf("memory tiers wall_time=%s", time.Since(started)) }()
	dsn := testenv.RequirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	tiers := []struct {
		name                                  string
		limit, bound                          int64
		email, transcripts, entries, memories int
	}{
		{"ci", 167772160, 150994944, 4 << 20, 16, 60000, 200},
		{"large", 268435456, 251658240, agentemail.RelayMaximumRawBytes, 1, 8, 2},
	}
	for _, tier := range tiers {
		t.Run(tier.name, func(t *testing.T) {
			seed := seedImportStateFixture(ctx, t, dsn, tier.transcripts, tier.entries, tier.memories, 2, []int{tier.email}, false, false)
			if tier.name == "ci" {
				t.Run("real_rows", func(t *testing.T) {
					assertImportStateRealRows(t, seed.fixture.BackupPath)
					assertImportStateRealRows(t, seed.fixture.EvacuationPath)
				})
			}
			rawInfo, err := os.Stat(seed.rawPaths[0])
			if err != nil {
				t.Fatal(err)
			}
			n := rawInfo.Size()
			if n < int64(tier.email)-(64<<10) || n > int64(tier.email) {
				t.Fatal("raw fixture outside tier size bounds")
			}
			validation, validationDSN := importStateDestination(ctx, t, dsn)
			importing, importDSN := importStateDestination(ctx, t, dsn)
			root := filepath.Join(t.TempDir(), "fake-cgroup")
			group := filepath.Join(root, "sys", "fs", "cgroup")
			if err := os.MkdirAll(group, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(group, "memory.max"), []byte(strconv.FormatInt(tier.limit, 10)), 0600); err != nil {
				t.Fatal(err)
			}
			for _, marker := range []string{"validate:new", "import:new", "validate:legacy"} {
				operation, mode, _ := strings.Cut(marker, ":")
				archive, destinationDSN := seed.fixture.BackupPath, validationDSN
				if operation == "import" {
					archive, destinationDSN = seed.fixture.EvacuationPath, importDSN
				}
				command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestImportStateMemoryPostgres$")
				for _, env := range os.Environ() {
					key, _, _ := strings.Cut(env, "=")
					if key == "GOGC" || key == "GOMEMLIMIT" || key == "GODEBUG" || strings.HasPrefix(key, "WITSELF_IMPORT_STATE_") {
						continue
					}
					command.Env = append(command.Env, env)
				}
				command.Env = append(command.Env, "WITSELF_IMPORT_STATE_CHILD="+marker, "WITSELF_IMPORT_STATE_DSN="+destinationDSN,
					"WITSELF_IMPORT_STATE_ARCHIVE="+archive, "WITSELF_IMPORT_STATE_RAW="+seed.rawPaths[0], "WITSELF_IMPORT_STATE_ROOT="+root,
					"WITSELF_IMPORT_STATE_ACCOUNT="+seed.fixture.AccountID, "WITSELF_IMPORT_STATE_BACKUP="+seed.fixture.BackupID, "WITSELF_IMPORT_STATE_EVACUATION="+seed.fixture.EvacuationID)
				var stderr bytes.Buffer
				command.Stderr = &stderr
				output, err := command.Output()
				if err != nil {
					t.Fatalf("%s child failed: %v\n%s%s", marker, err, output, stderr.String())
				}
				var result importStateMemoryResult
				if err := json.Unmarshal(bytes.TrimSpace(output), &result); err != nil {
					t.Fatalf("%s child measurement invalid: %v", marker, err)
				}
				t.Logf("tier=%s child=%s N=%d bound=%d %s", tier.name, marker, n, tier.bound, bytes.TrimSpace(output))
				if result.Outcome != "ok" || result.SoftLimit != tier.limit/4*3 {
					t.Fatalf("%s outcome or soft limit invalid", marker)
				}
				allocs := result.EmailRowAllocs - result.ParseAllocs
				if result.EmailRowBytes < n*2 {
					t.Fatal("large email row not observed")
				}
				// New: N decoded bytes + N pgx param bytes + <=2.25N Bind growth,
				// plus <2 MiB of other columns: approximately 4.25N. The 6N bound
				// allows allocator size classes. Legacy's two decoder buffers/strings,
				// hex input/output, Marshal buffer/clone and pgx copies total >=18N;
				// 14N is the conservative fixture-strength gate.
				// Peak uses slice108's L-16MiB form. CI: <=20MiB baseline+32MiB
				// entry+<=5MiB state gives ~57MiB live, ~114MiB GC goal, and a
				// 32MiB allocation overshoot (~146MiB). 128MiB is not reachable
				// until slice111 removes that entry buffer. Large new: ~20+50+25+
				// 2*25=145MiB live at Exec, below 192MiB soft; soft+25MiB overshoot
				// and non-heap stays ~225MiB (<240MiB). Legacy has at least five
				// ~50MiB representations plus baseline (~270MiB), exceeding240MiB.
				// Slice111's <=64MiB row buffer fits the same estimates.
				boundFailed := false
				if mode == "new" {
					if result.PeakGoTotal > tier.bound {
						t.Errorf("new peak_go_total=%d exceeds %d", result.PeakGoTotal, tier.bound)
						boundFailed = true
					}
					if allocs > 6*n+(2<<20) {
						t.Errorf("new email allocation delta=%d exceeds %d", allocs, 6*n+(2<<20))
						boundFailed = true
					}
				} else {
					if allocs < 14*n {
						t.Errorf("legacy fixture too weak: email allocation delta=%d below %d", allocs, 14*n)
						boundFailed = true
					}
					if tier.name == "large" {
						// 112-fix-review.md: slice 108's soft limit bounds legacy peaks;
						// the 14N allocation floor above proves fixture strength.
						t.Logf("legacy peak_go_total=%d max_go_live=%d (information only: bounded by the soft limit (slice 108))", result.PeakGoTotal, result.MaxGoLive)
					}
				}
				if boundFailed {
					return
				} // Report every bound for this child before stopping.
				if operation == "validate" {
					assertImportStateStatus(ctx, t, validation, seed.fixture.AccountID, false)
				} else {
					assertImportStateStatus(ctx, t, importing, seed.fixture.AccountID, true)
				}
			}
		})
		if t.Failed() {
			return
		}
	}
}
