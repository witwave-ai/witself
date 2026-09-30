package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"reflect"
	"regexp"
	"strings"
	"testing"

	accountexport "github.com/witwave-ai/witself/internal/export"
)

const generatorAccount = "acc_aaaaaaaaaaaaaaaa"

func TestGeneratorIsDeterministicPerAccount(t *testing.T) {
	for _, position := range []int{0, 1, 2, 37, 4999} {
		a := generateEntry(generatorAccount, 1, position)
		b := generateEntry(generatorAccount, 1, position)
		if !reflect.DeepEqual(a, b) {
			t.Fatal("identical coordinates produced different entries")
		}
		if a.Body == generateEntry("acc_bbbbbbbbbbbbbbbb", 1, position).Body {
			t.Fatal("different accounts produced identical content")
		}
	}
	// Generation order must not affect a position's content.
	before := generateEntry(generatorAccount, 9, 12)
	_ = generateEntry(generatorAccount, 1, 10)
	if !reflect.DeepEqual(before, generateEntry(generatorAccount, 9, 12)) {
		t.Fatal("generation order changed content")
	}
}

func TestGeneratorSizeDistribution(t *testing.T) {
	var total int64
	roles := map[string]int{}
	for p := range 2000 {
		e := generateEntry(generatorAccount, 1, p)
		total += logicalBytes(e)
		roles[e.Role]++
		if len(e.Body) > 49151 {
			t.Fatal("generated body exceeds the pinned bound")
		}
		if (e.Role == "tool") != (len(e.Payload) > 0) {
			t.Fatal("payload presence does not match the role")
		}
		if e.Role == "tool" {
			var payload map[string]any
			if len(e.Payload) > 8191 || json.Unmarshal(e.Payload, &payload) != nil || payload == nil {
				t.Fatal("tool payload exceeds its bound or is not an object")
			}
		}
	}
	mean := float64(total) / 2000
	t.Logf("mean logical bytes: %.3f", mean)
	if mean < 3700 || mean > 4700 {
		t.Fatalf("mean logical bytes %.3f outside the pinned band", mean)
	}
	if roles["user"] != 500 || roles["assistant"] != 1000 || roles["tool"] != 500 || len(roles) != 3 {
		t.Fatal("role shares differ from the pinned proportions")
	}
}

func TestGeneratorContentIsSafe(t *testing.T) {
	longHex := regexp.MustCompile(`[0-9a-fA-F]{17,}`)
	checkText := func(text string) {
		t.Helper()
		for _, c := range text {
			if c != '\n' && (c < 0x20 || c > 0x7e || strings.ContainsRune(`<>&"\`, c)) {
				t.Fatal("generated text contains a forbidden character")
			}
		}
		if longHex.MatchString(text) {
			t.Fatal("generated text contains an overlong hexadecimal run")
		}
	}
	for p := range 200 {
		e := generateEntry(generatorAccount, 1, p)
		if !validFakeEntry(e) {
			t.Fatal("generated entry fails the fake store validation contract")
		}
		checkText(e.Body)
		if len(e.Payload) > 0 {
			var payload struct {
				Tool string `json:"tool"`
				Call string `json:"call"`
				Args struct {
					Text string `json:"text"`
				} `json:"args"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				t.Fatal("generated payload is invalid")
			}
			checkText(payload.Tool)
			checkText(payload.Call)
			checkText(payload.Args.Text)
		}
	}
}

func generatorRows(count int) [][]byte {
	rows := make([][]byte, count)
	for p := range count {
		rows[p] = emulatedRow(generateEntry(generatorAccount, 1, p), generatorAccount,
			"rlm_aaaaaaaaaaaaaaaa", "agt_aaaaaaaaaaaaaaaa", "trn_aaaaaaaaaaaaaaaa", 1, p, 5000)
	}
	return rows
}

func estimateRows(t *testing.T, rows [][]byte) *archiveEstimator {
	t.Helper()
	e := newArchiveEstimator()
	for i, row := range rows {
		if err := e.add(row); err != nil {
			t.Fatal("estimator could not accept a row")
		}
		if (i+1)%100 == 0 {
			if err := e.flush(); err != nil {
				t.Fatal("estimator could not flush a batch")
			}
		}
	}
	if err := e.close(); err != nil {
		t.Fatal("estimator could not close")
	}
	return e
}

func TestCompressionRatioBand(t *testing.T) {
	rows := generatorRows(500)
	e := estimateRows(t, rows)
	var ndjson int64
	for _, row := range rows {
		ndjson += int64(len(row) + 1)
	}
	ratio := float64(ndjson) / float64(e.gzipBytes())
	t.Logf("NDJSON/gzip ratio: %.6f", ratio)
	if ratio < 2.5 || ratio > 4.0 {
		t.Fatalf("compression ratio %.6f outside the pinned band", ratio)
	}
}

type generatorRowSource struct {
	rows     [][]byte
	position int
}

func (*generatorRowSource) Table() string { return "transcript_entries" }
func (s *generatorRowSource) Next(context.Context) ([]byte, error) {
	if s.position == len(s.rows) {
		return nil, nil
	}
	row := s.rows[s.position]
	s.position++
	return row, nil
}

func TestEstimatorTracksArchiveWriter(t *testing.T) {
	rows := generatorRows(500)
	e := estimateRows(t, rows)
	var archive bytes.Buffer
	manifest := accountexport.Manifest{AccountID: generatorAccount, SchemaVersion: 1, Purpose: accountexport.PurposeSelf}
	if err := accountexport.Write(context.Background(), &archive, manifest, []accountexport.RowSource{&generatorRowSource{rows: rows}}); err != nil {
		t.Fatal("archive writer failed")
	}
	relative := math.Abs(float64(e.gzipBytes())/float64(archive.Len()) - 1)
	t.Logf("raw estimator relative error: %.6f", relative)
	if relative > 0.03 {
		t.Fatalf("raw estimator relative error %.6f exceeds 0.03", relative)
	}
}

func TestEstimateAdjustment(t *testing.T) {
	for _, n := range []int64{0, 101, 100099} {
		e := &archiveEstimator{counter: archiveByteCounter{n: n}}
		if got := e.estimate(); got != n+n/100+20000 {
			t.Fatalf("estimate adjustment differs at counter %d", n)
		}
	}
}

func TestEmulatedRowShape(t *testing.T) {
	e := entry{ExternalID: "wfl1-t001-e0000", Role: "tool", Body: "one\ntwo",
		Payload: json.RawMessage(`{"tool":"synthetic_tool_1","call":"0123456789abcdef","ok":true,"args":{"text":"three"}}`)}
	row := emulatedRowWithIdentity(e, "acc_aaaaaaaaaaaaaaaa", "rlm_aaaaaaaaaaaaaaaa", "agt_aaaaaaaaaaaaaaaa",
		"trn_aaaaaaaaaaaaaaaa", "ent_aaaaaaaaaaaaaaaa", "2026-09-30T00:00:00.123456+00:00", 1)
	want := `{"id": "ent_aaaaaaaaaaaaaaaa", "body": "one\ntwo", "role": "tool", "model": null, "payload": {"ok": true, "args": {"text": "three"}, "call": "0123456789abcdef", "tool": "synthetic_tool_1"}, "realm_id": "rlm_aaaaaaaaaaaaaaaa", "sequence": 1, "artifacts": [], "account_id": "acc_aaaaaaaaaaaaaaaa", "created_at": "2026-09-30T00:00:00.123456+00:00", "external_id": "wfl1-t001-e0000", "transcript_id": "trn_aaaaaaaaaaaaaaaa", "reply_to_entry_id": null, "recorded_by_agent_id": "agt_aaaaaaaaaaaaaaaa"}`
	if string(row) != want {
		t.Fatal("emulated row differs from the exact ordered JSON contract")
	}
	if quoteJSON("<>&") != `"<>&"` {
		t.Fatal("JSON encoder enabled HTML escaping")
	}
	a := emulatedRow(e, generatorAccount, "realm", "agent", "transcript", 1, 0, 5000)
	b := emulatedRow(e, generatorAccount, "realm", "agent", "transcript", 1, 0, 5000)
	if !bytes.Equal(a, b) {
		t.Fatal("archive identity fields are not deterministic")
	}
}

func TestExternalIDs(t *testing.T) {
	for transcript := 1; transcript <= 90; transcript++ {
		tID := transcriptExternalID(transcript)
		if parsed, ok := parseTranscriptExternalID(tID); !ok || parsed != transcript {
			t.Fatal("transcript external identifier did not round-trip")
		}
		for _, position := range []int{0, 9, 4999} {
			e := generateEntry(generatorAccount, transcript, position)
			gotT, gotP, ok := parseEntryExternalID(e.ExternalID, 5000)
			if !ok || gotT != transcript || gotP != position {
				t.Fatal("entry external identifier did not round-trip")
			}
		}
	}
	for _, invalid := range []string{"wfl1-t000", "wfl1-t091", "wfl1-t+01", "wfl1-t1", "wfl1-t001-extra"} {
		if _, ok := parseTranscriptExternalID(invalid); ok {
			t.Fatal("invalid transcript external identifier accepted")
		}
	}
	for _, invalid := range []string{"wfl1-t000-e0000", "wfl1-t091-e0000", "wfl1-t001-e5000", "wfl1-t1-e1", "wfl1-t001-e-001", "wfl1-t001-e+001", ""} {
		if _, _, ok := parseEntryExternalID(invalid, 5000); ok {
			t.Fatal("invalid entry external identifier accepted")
		}
	}
	if _, _, ok := parseEntryExternalID("wfl1-t001-e0010", 10); ok {
		t.Fatal("entry identifier exceeded the injected transcript capacity")
	}
}

func TestTargetArithmetic(t *testing.T) {
	for _, tc := range []struct {
		founder, target int64
		refused         bool
	}{
		{333600000, 367001600, false}, {340000000, 374000000, false},
		{454545455, 500000001, true}, {454545454, 500000000, false},
	} {
		target, goal, err := targetBytes(tc.founder, 367001600, 500000000)
		if target != tc.target || (err != nil) != tc.refused {
			t.Fatalf("target arithmetic differs for founder size %d", tc.founder)
		}
		if tc.refused {
			if err.Error() != "refused: target 500000001 bytes exceeds the ceiling 500000000 bytes" {
				t.Fatal("ceiling refusal differs from its exact contract")
			}
		} else if goal != target+(target*3+99)/100 {
			t.Fatal("goal did not add the rounded three-percent margin")
		}
	}
	if _, _, err := targetBytes(math.MaxInt64, 367001600, 500000000); err == nil {
		t.Fatal("overflow-sized founder input did not refuse")
	}
}
