package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func withLegacyImportRowDecodeForTest() Option {
	return func(s *Store) { s.importRowDecodeLegacy = true }
}

func withImportRowClobberForTest() Option {
	return func(s *Store) { s.importRowClobber = true }
}

type importRowDecodeCase struct {
	name string
	row  string
	fast bool
}

func importRowDecodeCases() []importRowDecodeCase {
	return []importRowDecodeCase{
		{"nested", `{"a":[1,{"b":[null,true,false,{"c":"text"}]}],"z":{}}`, true},
		{"duplicate-depth-one", `{"a":1,"a":2}`, false},
		{"duplicate-depth-two", `{"a":{"b":1,"b":2}}`, false},
		{"duplicate-depth-five-array", `{"a":[{"b":[{"c":1,"c":2}]}]}`, false},
		{"high-surrogate", `{"a":"\ud800"}`, false},
		{"low-surrogate", `{"a":"\udc00"}`, false},
		{"unpaired-high-surrogate", `{"a":"\ud800\u0041"}`, false},
		{"paired-surrogates", `{"a":"\ud800\udc00"}`, true},
		{"large-integer", `{"a":9007199254740993}`, true},
		{"negative-zero", `{"a":-0}`, true},
		{"large-exponent", `{"a":1e400}`, true},
		{"number-forms", `{"a":-12.34e+56,"b":0.01E-2}`, true},
		{"leading-zero", `{"a":01}`, false},
		{"number-without-fraction", `{"a":1.}`, false},
		{"number-without-exponent", `{"a":1e+}`, false},
		{"trailing-object", `{} {}`, false},
		{"trailing-text", `{}x`, false},
		{"trailing-bracket", `{}]`, false},
		{"array", `[]`, false},
		{"string", `"s"`, false},
		{"number", `1`, false},
		{"true", `true`, false},
		{"null", `null`, false},
		{"empty", ``, false},
		{"whitespace", " \t\r\n", false},
		{"bom", "\xef\xbb\xbf{}", false},
		{"invalid-utf8-key", "{\"\xff\":1}", false},
		{"invalid-utf8-value", "{\"a\":\"\xff\"}", false},
		{"unicode-key", `{"é":1}`, false},
		{"escaped-key", `{"\u0061":1}`, false},
		{"duplicate-escaped-key", `{"a":1,"\u0061":2}`, false},
		{"unterminated-string", `{"a":"open}`, false},
		{"raw-control", "{\"a\":\"\x01\"}", false},
		{"invalid-escape", `{"a":"\q"}`, false},
		{"escaped-values", `{"a":"\"\\\/\b\f\n\r\t\u0020"}`, true},
		{"too-deep", `{"a":` + strings.Repeat("[", 10000) + `0` + strings.Repeat("]", 10000) + `}`, false},
		{"missing-colon", `{"a" 1}`, false},
		{"missing-comma", `{"a":1 "b":2}`, false},
		{"object-trailing-comma", `{"a":1,}`, false},
		{"array-trailing-comma", `{"a":[1,]}`, false},
		{"invalid-literal", `{"a":tru}`, false},
		{"literal-suffix", `{"a":truex}`, false},
		{"raw-canonical", `{"raw_mime":"\\x00abcdef"}`, true},
		{"raw-empty", `{"raw_mime":"\\x"}`, true},
		{"raw-uppercase", `{"raw_mime":"\\x00ABCDEF"}`, false},
		{"raw-odd", `{"raw_mime":"\\xabc"}`, false},
		{"raw-uppercase-marker", `{"raw_mime":"\\X00ab"}`, false},
		{"raw-invalid-escape", `{"raw_mime":"\x00ab"}`, false},
		{"raw-noncanonical-escape", `{"raw_mime":"\\\u007800ab"}`, false},
		{"raw-null", `{"raw_mime":null}`, true},
		{"raw-number", `{"raw_mime":5}`, true},
		{"raw-nested", `{"nested":{"raw_mime":"\\x00ab"}}`, true},
		{"raw-duplicate", `{"raw_mime":"\\x00ab","raw_mime":"\\x00cd"}`, false},
	}
}

func normalizeImportRowRawMIMEForTest(obj map[string]any) map[string]any {
	if value, ok := obj["raw_mime"].(importedRawMIME); ok {
		obj = maps.Clone(obj)
		obj["raw_mime"] = `\x` + hex.EncodeToString(value.raw)
	}
	return obj
}

func checkImportRowDecodeDifferential(t testing.TB, table string, row []byte, requireFast bool) {
	t.Helper()
	before := bytes.Clone(row)
	legacy, legacyErr := decodeImportRow(row)
	fast, fastOK := decodeImportRowFast(table, row)
	if requireFast && !fastOK {
		t.Fatal("canonical row did not use fast path")
	}
	if fastOK && (legacyErr != nil || !reflect.DeepEqual(normalizeImportRowRawMIMEForTest(fast), legacy)) {
		t.Fatal("fast success disagrees with legacy object or validity")
	}
	once, onceErr := decodeImportRowOnce(table, row)
	if legacyErr != nil {
		if onceErr == nil || onceErr.Error() != legacyErr.Error() {
			t.Fatal("legacy refusal text changed")
		}
	} else if onceErr != nil || !reflect.DeepEqual(normalizeImportRowRawMIMEForTest(once), legacy) {
		t.Fatal("single decode disagrees with legacy object")
	}
	if !bytes.Equal(row, before) {
		t.Fatal("decoder changed reader-owned bytes")
	}
}

func TestImportRowDecodeDifferential(t *testing.T) {
	for _, tc := range importRowDecodeCases() {
		for _, table := range []string{"agent_email_messages", "transcript_entries"} {
			t.Run(tc.name+"/"+table, func(t *testing.T) {
				checkImportRowDecodeDifferential(t, table, []byte(tc.row), tc.fast)
			})
		}
	}
}

func FuzzImportRowDecodeDifferential(f *testing.F) {
	for _, tc := range importRowDecodeCases() {
		for _, selector := range []byte{0, 1} {
			f.Add(append([]byte{selector}, []byte(tc.row)...))
		}
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) == 0 {
			return
		}
		table := "transcript_entries"
		if input[0]&1 == 0 {
			table = "agent_email_messages"
		}
		checkImportRowDecodeDifferential(t, table, input[1:], false)
	})
}

func TestImportRowDecodeOwnsReturnedValues(t *testing.T) {
	row := []byte(`{"id":"ent_abcdefghijkl2345","transcript_id":"trn_abcdefghijkl2345","raw_mime":"\\x00ab","nested":[{"text":"retained"}]}`)
	obj, ok := decodeImportRowFast("agent_email_messages", row)
	if !ok {
		t.Fatal("fast path refused canonical row")
	}
	want, err := decodeImportRow(bytes.Clone(row))
	if err != nil {
		t.Fatal(err)
	}
	for i := range row {
		row[i] = 0xff
	}
	if !reflect.DeepEqual(normalizeImportRowRawMIMEForTest(obj), want) {
		t.Fatal("returned values retain reader-owned bytes")
	}
	for _, fail := range []bool{false, true} {
		s := &Store{}
		withImportRowClobberForTest()(s)
		original := []byte(`{"id":"entry"}`)
		before := bytes.Clone(original)
		var callbackRow []byte
		wantErr := errors.New("callback failure")
		err := s.wrapImportRowClobber(func(_ string, row []byte) error {
			callbackRow = row
			if fail {
				return wantErr
			}
			return nil
		})("transcript_entries", original)
		if fail && !errors.Is(err, wantErr) || !fail && err != nil {
			t.Fatal("callback outcome changed")
		}
		if !bytes.Equal(original, before) {
			t.Fatal("clobber touched reader buffer")
		}
		if !bytes.Equal(callbackRow, bytes.Repeat([]byte{0xff}, len(callbackRow))) {
			t.Fatal("callback copy was not clobbered")
		}
	}
}

func TestImportedAgentEmailRawMIMEParity(t *testing.T) {
	const accountID, realmID, agentID = "acc_1", "realm_abcdefghijkl2345", "agent_1"
	const addressID, mailboxID, messageID = "eaddr_aaaaaaaaaaaaaaaa", "emb_aaaaaaaaaaaaaaaa", "emsg_aaaaaaaaaaaaaaaa"
	ic := newImportCtx(accountID)
	ic.exportedAt = time.Date(2026, 7, 21, 14, 0, 0, 0, time.UTC)
	ic.realms[realmID], ic.agents[agentID], ic.liveAgents[agentID] = true, true, true
	ic.agentRealms[agentID] = realmID
	feedAgentEmailArchiveRow(t, ic, "agent_email_addresses", agentEmailArchiveAddressRow(accountID, realmID, agentID, addressID, false))
	feedAgentEmailArchiveRow(t, ic, "agent_email_mailboxes", agentEmailArchiveMailboxRow(accountID, realmID, agentID, addressID, mailboxID))
	raw := []byte("From: sender@example.com\r\nTo: owner@example.com\r\nSubject: parity\r\nDate: Tue, 21 Jul 2026 12:00:00 +0000\r\n\r\nemail body")
	base := agentEmailArchiveMessageRow(accountID, realmID, agentID, addressID, mailboxID, messageID, raw, agentEmailArchiveDuplicateGroup(raw), "")
	cases := []struct {
		name       string
		mutate     func(map[string]any)
		escape     bool
		wantString bool
		valid      bool
	}{
		{name: "retained", valid: true},
		{name: "wrong-sha", mutate: func(o map[string]any) { o["raw_sha256"] = strings.Repeat("0", 64) }},
		{name: "wrong-size", mutate: func(o map[string]any) { o["raw_size_bytes"] = len(raw) + 1 }},
		{name: "omitted-present", mutate: func(o map[string]any) { o["payload_retention_state"] = importedAgentEmailPayloadOmittedCapacity }},
		{name: "retained-null", mutate: func(o map[string]any) { o["raw_mime"] = nil }},
		{name: "uppercase", mutate: func(o map[string]any) { o["raw_mime"] = `\x` + strings.ToUpper(hex.EncodeToString(raw)) }, wantString: true},
		{name: "noncanonical-escape", escape: true, wantString: true, valid: true},
		{name: "projection", mutate: func(o map[string]any) { o["header_subject"] = "different" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := maps.Clone(base)
			if tc.mutate != nil {
				tc.mutate(row)
			}
			encoded, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			if tc.escape {
				encoded = bytes.Replace(encoded, []byte(`"raw_mime":"\\x`), []byte(`"raw_mime":"\\\u0078`), 1)
			}
			legacy, err := decodeImportRow(encoded)
			if err != nil {
				t.Fatal("legacy decoder unexpectedly refused fixture")
			}
			current, err := decodeImportRowOnce("agent_email_messages", encoded)
			if err != nil {
				t.Fatal("new decoder unexpectedly refused fixture")
			}
			if tc.wantString {
				if _, ok := current["raw_mime"].(string); !ok {
					t.Fatal("noncanonical raw MIME bypassed string validation")
				}
			}
			oldID, oldScope, oldErr := ic.validateImportedAgentEmailMessage(legacy)
			newID, newScope, newErr := ic.validateImportedAgentEmailMessage(current)
			if (oldErr == nil) != tc.valid {
				t.Fatal("fixture has unexpected validity")
			}
			if oldID != newID || !reflect.DeepEqual(oldScope, newScope) || (oldErr == nil) != (newErr == nil) {
				t.Fatal("raw MIME validation identity, scope or validity changed")
			}
			if oldErr != nil && oldErr.Error() != newErr.Error() {
				t.Fatal("raw MIME refusal text changed")
			}
		})
	}
}

type importByteaRecordingExec struct {
	recordingExec
	args []any
}

func (r *importByteaRecordingExec) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.args = append([]any(nil), args...)
	return r.recordingExec.Exec(ctx, sql, args...)
}

func TestInsertProjectedBytea(t *testing.T) {
	ctx := context.Background()
	t.Run("projection-and-args", func(t *testing.T) {
		fake := &importByteaRecordingExec{}
		obj := map[string]any{"id": "emsg_a", "raw_size_bytes": 2, "body_text": "text"}
		row := []byte(`{"id":"emsg_a","raw_size_bytes":2,"body_text":"text"}`)
		raw := []byte{0, 0xab}
		if err := insertProjectedBytea(ctx, fake, "agent_email_messages", obj, row, "raw_mime", raw); err != nil {
			t.Fatal(err)
		}
		want := `INSERT INTO agent_email_messages (body_text, id, raw_mime, raw_size_bytes) SELECT body_text, id, $2::bytea, raw_size_bytes FROM jsonb_populate_record(NULL::agent_email_messages, $1::jsonb)`
		if fake.calls != 1 || fake.lastSQL != want {
			t.Fatal("bytea SQL projection changed")
		}
		if !reflect.DeepEqual(fake.args, []any{row, raw}) {
			t.Fatal("bytea SQL args changed")
		}
		obj["raw_mime"] = importedRawMIME{raw: raw}
		if _, err := json.Marshal(obj); err == nil {
			t.Fatal("raw MIME sentinel marshalled without tripwire")
		}
		if err := insertProjectedBytea(ctx, fake, "agent_email_messages", obj, row, "raw_mime", raw); err != nil {
			t.Fatal(err)
		}
		if fake.lastSQL != want {
			t.Fatal("column already present was duplicated")
		}
	})
	for _, extraColumn := range []bool{false, true} {
		fake := &importByteaRecordingExec{}
		obj := map[string]any{"id": "acc_a"}
		column := "unknown"
		if extraColumn {
			column = "id"
			obj["unknown"] = true
		}
		err := insertProjectedBytea(ctx, fake, "accounts", obj, []byte(`{}`), column, nil)
		legacy := insertProjected(ctx, &recordingExec{}, "accounts", map[string]any{"unknown": true}, []byte(`{}`))
		if !errors.Is(err, ErrArchiveContent) || err.Error() != legacy.Error() {
			t.Fatal("unknown-column error text changed")
		}
		if fake.calls != 0 {
			t.Fatal("unknown column reached SQL")
		}
	}
	t.Run("error-mapping", func(t *testing.T) {
		for _, table := range []string{"accounts", "tokens"} {
			pgErr := &pgconn.PgError{Code: "23505"}
			fake := &importByteaRecordingExec{recordingExec: recordingExec{err: pgErr}}
			err := insertProjectedBytea(ctx, fake, table, map[string]any{"id": "example"}, []byte(`{}`), "id", nil)
			if table == "accounts" {
				if !errors.Is(err, ErrAccountExists) {
					t.Fatal("account collision mapping changed")
				}
			} else if !errors.Is(err, pgErr) || errors.Is(err, ErrAccountExists) || !strings.HasPrefix(err.Error(), "import tokens row: ") {
				t.Fatal("non-account error mapping changed")
			}
		}
	})
}
