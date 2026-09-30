package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	identifierShare   = 0.05
	zipfExponent      = 1.1
	archiveChunkBytes = 32 << 20
)

type entry struct {
	ExternalID string          `json:"external_id"`
	Role       string          `json:"role"`
	Body       string          `json:"body"`
	Model      string          `json:"model,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

var vocabulary = makeVocabulary()

func makeVocabulary() [4096]string {
	rng := rand.New(rand.NewChaCha8(sha256.Sum256([]byte("wfl1-vocabulary"))))
	var words [4096]string
	for i := range words {
		word := make([]byte, 2+rng.IntN(8))
		for j := range word {
			word[j] = byte('a' + rng.IntN(26))
		}
		words[i] = string(word)
	}
	return words
}

func entryRNG(account string, transcript, position int) *rand.Rand {
	seed := sha256.Sum256([]byte("wfl1|" + account + "|" + strconv.Itoa(transcript) + "|" + strconv.Itoa(position)))
	return rand.New(rand.NewChaCha8(seed))
}

func entryArchiveIdentity(rng *rand.Rand) (string, int) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	var suffix [16]byte
	for i := range suffix {
		suffix[i] = alphabet[rng.IntN(len(alphabet))]
	}
	return "ent_" + string(suffix[:]), rng.IntN(250000)
}

func generateEntry(account string, t, p int) entry {
	rng := entryRNG(account, t, p)
	// Reserve the same draws for archive-only identity and timestamp fields.
	_, _ = entryArchiveIdentity(rng)
	roles := [...]string{"user", "assistant", "tool", "assistant"}
	e := entry{ExternalID: fmt.Sprintf("wfl1-t%03d-e%04d", t, p), Role: roles[p%4]}
	if e.Role == "assistant" {
		e.Model = "synthetic-model-1"
	}
	if e.Role == "tool" {
		e.Body = generatedText(rng, 64+rng.IntN(448))
		tool := fmt.Sprintf("synthetic_tool_%d", 1+rng.IntN(32))
		call := fmt.Sprintf("%016x", rng.Uint64())
		text := generatedText(rng, 512+rng.IntN(7389))
		// The alphabet needs no JSON escaping except for its line breaks.
		// Even at maximum length, escaping remains below the payload cap.
		e.Payload = json.RawMessage(`{"tool":` + quoteJSON(tool) + `,"call":` + quoteJSON(call) + `,"ok":true,"args":{"text":` + quoteJSON(text) + `}}`)
		return e
	}
	u := rng.Float64()
	var length int
	switch {
	case u < 0.50:
		length = 256 + rng.IntN(1280)
	case u < 0.85:
		length = 1536 + rng.IntN(4608)
	case u < 0.97:
		length = 6144 + rng.IntN(10240)
	default:
		length = 16384 + rng.IntN(32768)
	}
	e.Body = generatedText(rng, length)
	return e
}

func generatedText(rng *rand.Rand, length int) string {
	const identifierAlphabet = "abcdefghijklmnopqrstuvwxyz234567"
	zipf := rand.NewZipf(rng, zipfExponent, 1, uint64(len(vocabulary)-1))
	var text strings.Builder
	text.Grow(length + 16)
	wordsLeft := 9 + rng.IntN(7)
	sentencesLeft := 3 + rng.IntN(4)
	for text.Len() < length {
		if rng.Float64() < identifierShare {
			text.WriteByte('x')
			for range 11 {
				text.WriteByte(identifierAlphabet[rng.IntN(len(identifierAlphabet))])
			}
		} else {
			text.WriteString(vocabulary[zipf.Uint64()])
		}
		wordsLeft--
		if wordsLeft == 0 {
			wordsLeft = 9 + rng.IntN(7)
			sentencesLeft--
			if sentencesLeft == 0 {
				text.WriteString(".\n")
				sentencesLeft = 3 + rng.IntN(4)
			} else {
				text.WriteString(". ")
			}
		} else {
			text.WriteByte(' ')
		}
	}
	return text.String()[:length]
}

func logicalBytes(e entry) int64 {
	return int64(len(e.ExternalID) + len(e.Role) + len(e.Body) + len(e.Payload) + len(e.Model) + 2)
}

func transcriptExternalID(t int) string {
	return fmt.Sprintf("wfl1-t%03d", t)
}

func parseTranscriptExternalID(s string) (int, bool) {
	if len(s) != 9 || !strings.HasPrefix(s, "wfl1-t") {
		return 0, false
	}
	t, err := strconv.Atoi(s[6:])
	return t, err == nil && t >= 1 && t <= 90 && s == transcriptExternalID(t)
}

func parseEntryExternalID(s string, entriesPerTranscript int) (int, int, bool) {
	if len(s) != 15 || s[9:11] != "-e" {
		return 0, 0, false
	}
	t, ok := parseTranscriptExternalID(s[:9])
	p, err := strconv.Atoi(s[11:])
	return t, p, ok && err == nil && p >= 0 && p < entriesPerTranscript && s == fmt.Sprintf("wfl1-t%03d-e%04d", t, p)
}

func quoteJSON(s string) string {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	// Encoding a string into a bytes.Buffer cannot fail.
	if err := encoder.Encode(s); err != nil {
		return `""`
	}
	return strings.TrimSuffix(out.String(), "\n")
}

// postgresJSON models jsonb's length-then-bytewise key order and whitespace.
func postgresJSON(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case string:
		return quoteJSON(v)
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			if len(keys[i]) != len(keys[j]) {
				return len(keys[i]) < len(keys[j])
			}
			return keys[i] < keys[j]
		})
		parts := make([]string, len(keys))
		for i, key := range keys {
			parts[i] = quoteJSON(key) + ": " + postgresJSON(v[key])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = postgresJSON(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return "null"
		}
		return string(encoded)
	}
}

func emulatedRow(e entry, account, realm, agent, transcript string, t, p, entriesPerTranscript int) []byte {
	id, micros := entryArchiveIdentity(entryRNG(account, t, p))
	global := int64(t-1)*int64(entriesPerTranscript) + int64(p)
	created := time.Date(2026, time.September, 30, 0, 0, 0, 0, time.UTC).
		Add(time.Duration(global)*250*time.Millisecond + time.Duration(micros)*time.Microsecond)
	return emulatedRowWithIdentity(e, account, realm, agent, transcript, id,
		created.Format("2006-01-02T15:04:05.000000+00:00"), p+1)
}

func emulatedRowWithIdentity(e entry, account, realm, agent, transcript, id, created string, sequence int) []byte {
	var model any
	if e.Model != "" {
		model = e.Model
	}
	var payload any
	if len(e.Payload) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(e.Payload))
		decoder.UseNumber()
		if err := decoder.Decode(&payload); err != nil {
			payload = nil
		}
	}
	return []byte(postgresJSON(map[string]any{
		"id": id, "body": e.Body, "role": e.Role, "model": model, "payload": payload,
		"realm_id": realm, "sequence": sequence, "artifacts": []any{}, "account_id": account,
		"created_at": created, "external_id": e.ExternalID, "transcript_id": transcript,
		"reply_to_entry_id": nil, "recorded_by_agent_id": agent,
	}))
}

type archiveByteCounter struct{ n int64 }

func (c *archiveByteCounter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

type archiveEstimator struct {
	counter    archiveByteCounter
	writer     *gzip.Writer
	chunkBytes int64
}

func newArchiveEstimator() *archiveEstimator {
	e := &archiveEstimator{}
	e.writer = gzip.NewWriter(&e.counter)
	return e
}

func (e *archiveEstimator) add(row []byte) error {
	rowBytes := int64(len(row) + 1)
	if e.chunkBytes > 0 && e.chunkBytes+rowBytes > archiveChunkBytes {
		if err := e.flush(); err != nil {
			return err
		}
		e.chunkBytes = 0
	}
	if _, err := e.writer.Write(row); err != nil {
		return err
	}
	if _, err := e.writer.Write([]byte{'\n'}); err != nil {
		return err
	}
	e.chunkBytes += rowBytes
	return nil
}

func (e *archiveEstimator) flush() error     { return e.writer.Flush() }
func (e *archiveEstimator) gzipBytes() int64 { return e.counter.n }
func (e *archiveEstimator) estimate() int64  { return e.gzipBytes() + e.gzipBytes()/100 + 20000 }
func (e *archiveEstimator) close() error     { return e.writer.Close() }

func targetBytes(founder, floor, ceiling int64) (int64, int64, error) {
	t := new(big.Int).Mul(big.NewInt(founder), big.NewInt(11))
	t.Add(t, big.NewInt(9)).Quo(t, big.NewInt(10))
	if t.Cmp(big.NewInt(floor)) < 0 {
		t.SetInt64(floor)
	}
	if t.Cmp(big.NewInt(ceiling)) > 0 {
		var target int64
		if t.IsInt64() {
			target = t.Int64()
		}
		return target, 0, failure(3, "refused: target %s bytes exceeds the ceiling %d bytes", t.String(), ceiling)
	}
	target := t.Int64()
	goal := target + (target*3+99)/100
	return target, goal, nil
}
