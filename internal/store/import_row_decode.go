package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// importedRawMIME owns its bytes independently of the archive reader's row.
// It must leave the object through the bytea insert path, never JSON encoding.
type importedRawMIME struct{ raw []byte }

func (importedRawMIME) MarshalJSON() ([]byte, error) {
	return nil, errors.New("imported raw MIME must be inserted as bytea")
}

func importedRawMIMELength(value any) (int, bool) {
	if v, ok := value.(importedRawMIME); ok {
		return len(v.raw), true
	}
	return importedByteaLength(value)
}

// The caller checks the storage shape before decoding, preserving refusal order.
func decodeImportedRawMIME(value any) ([]byte, error) {
	if v, ok := value.(importedRawMIME); ok {
		return v.raw, nil
	}
	return hex.DecodeString(value.(string)[2:])
}

// decodeImportRowOnce only accepts a fast result proven safe by the scanner.
// Every other input goes to the unchanged oracle, including all error text.
func decodeImportRowOnce(table string, row []byte) (map[string]any, error) {
	if obj, ok := decodeImportRowFast(table, row); ok {
		return obj, nil
	}
	return decodeImportRow(row)
}

func decodeImportRowFast(table string, row []byte) (map[string]any, bool) {
	if rejectUnpairedJSONSurrogates(row) != nil {
		return nil, false
	}
	start, end, ok := scanImportRow(row, table == "agent_email_messages")
	if !ok {
		return nil, false
	}
	var reader io.Reader = bytes.NewReader(row)
	var raw []byte
	if start >= 0 {
		encoded := row[start+4 : end-1]
		raw = make([]byte, len(encoded)/2)
		if _, err := hex.Decode(raw, encoded); err != nil {
			return nil, false
		}
		reader = io.MultiReader(bytes.NewReader(row[:start]), strings.NewReader("null"), bytes.NewReader(row[end:]))
	}
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	var obj map[string]any
	if err := decoder.Decode(&obj); err != nil || obj == nil {
		return nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, false
	}
	if start >= 0 {
		obj["raw_mime"] = importedRawMIME{raw: raw}
	}
	return obj, true
}

// Frames retain only object keys, never string values. Their lifetime ends
// before the row callback returns; decoding copies all returned strings.
type importJSONFrame struct {
	kind    byte
	state   byte
	keys    map[string]struct{}
	rawMIME bool
}

// scanImportRow validates complete JSON grammar without copying value strings.
// Simple keys have one byte spelling per decoded name, so raw-key comparison
// proves uniqueness. Escaped and non-ASCII keys conservatively use the oracle.
func scanImportRow(row []byte, extractRaw bool) (rawStart, rawEnd int, ok bool) {
	rawStart, rawEnd = -1, -1
	i := importJSONSpace(row, 0)
	if i == len(row) || row[i] != '{' {
		return rawStart, rawEnd, false
	}
	stack := []importJSONFrame{{kind: '{'}}
	i++
	for len(stack) > 0 {
		i = importJSONSpace(row, i)
		if i == len(row) {
			return rawStart, rawEnd, false
		}
		frame := &stack[len(stack)-1]
		if frame.kind == '{' {
			switch frame.state {
			case 0, 1: // first key (or end), or mandatory key after comma
				if frame.state == 0 && row[i] == '}' {
					stack = stack[:len(stack)-1]
					i++
					continue
				}
				if row[i] != '"' {
					return rawStart, rawEnd, false
				}
				end, simple, valid := scanImportJSONString(row, i)
				if !valid || !simple {
					return rawStart, rawEnd, false
				}
				key := string(row[i+1 : end-1])
				if _, duplicate := frame.keys[key]; duplicate {
					return rawStart, rawEnd, false
				}
				if frame.keys == nil {
					frame.keys = make(map[string]struct{})
				}
				frame.keys[key] = struct{}{}
				frame.rawMIME = extractRaw && len(stack) == 1 && key == "raw_mime"
				frame.state = 2
				i = end
				continue
			case 2: // colon
				if row[i] != ':' {
					return rawStart, rawEnd, false
				}
				frame.state = 3
				i++
				continue
			case 4: // comma or end
				switch row[i] {
				case ',':
					frame.state = 1
				case '}':
					stack = stack[:len(stack)-1]
				default:
					return rawStart, rawEnd, false
				}
				i++
				continue
			}
		} else {
			if frame.state == 0 && row[i] == ']' {
				stack = stack[:len(stack)-1]
				i++
				continue
			}
			if frame.state == 2 {
				switch row[i] {
				case ',':
					frame.state = 1
				case ']':
					stack = stack[:len(stack)-1]
				default:
					return rawStart, rawEnd, false
				}
				i++
				continue
			}
		}
		// Consume one value, then its parent's next state is comma-or-end.
		isRaw := frame.kind == '{' && frame.rawMIME
		if frame.kind == '{' {
			frame.state = 4
		} else {
			frame.state = 2
		}
		switch row[i] {
		case '{', '[':
			if len(stack) >= 10000 {
				return rawStart, rawEnd, false
			}
			stack = append(stack, importJSONFrame{kind: row[i]})
			i++
		case '"':
			end, _, valid := scanImportJSONString(row, i)
			if !valid {
				return rawStart, rawEnd, false
			}
			if isRaw && canonicalImportRawMIME(row[i:end]) {
				rawStart, rawEnd = i, end
			}
			i = end
		case 't':
			if !bytes.HasPrefix(row[i:], []byte("true")) {
				return rawStart, rawEnd, false
			}
			i += 4
		case 'f':
			if !bytes.HasPrefix(row[i:], []byte("false")) {
				return rawStart, rawEnd, false
			}
			i += 5
		case 'n':
			if !bytes.HasPrefix(row[i:], []byte("null")) {
				return rawStart, rawEnd, false
			}
			i += 4
		default:
			end, valid := scanImportJSONNumber(row, i)
			if !valid {
				return rawStart, rawEnd, false
			}
			i = end
		}
	}
	return rawStart, rawEnd, importJSONSpace(row, i) == len(row)
}

func importJSONSpace(row []byte, i int) int {
	for i < len(row) {
		switch row[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

func scanImportJSONString(row []byte, start int) (end int, simple, ok bool) {
	simple = true
	for i := start + 1; i < len(row); i++ {
		c := row[i]
		switch {
		case c == '"':
			return i + 1, simple, true
		case c < 0x20:
			return 0, false, false
		case c == '\\':
			simple = false
			i++
			if i == len(row) {
				return 0, false, false
			}
			switch row[i] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				if _, valid := parseJSONHexCodeUnit(row, i+1); !valid {
					return 0, false, false
				}
				i += 4
			default:
				return 0, false, false
			}
		case c > 0x7e:
			simple = false
		}
	}
	return 0, false, false
}

func scanImportJSONNumber(row []byte, i int) (int, bool) {
	if row[i] == '-' {
		i++
	}
	if i == len(row) {
		return i, false
	}
	if row[i] == '0' {
		i++
	} else {
		if row[i] < '1' || row[i] > '9' {
			return i, false
		}
		for i < len(row) && row[i] >= '0' && row[i] <= '9' {
			i++
		}
	}
	if i < len(row) && row[i] == '.' {
		i++
		start := i
		for i < len(row) && row[i] >= '0' && row[i] <= '9' {
			i++
		}
		if i == start {
			return i, false
		}
	}
	if i < len(row) && (row[i] == 'e' || row[i] == 'E') {
		i++
		if i < len(row) && (row[i] == '+' || row[i] == '-') {
			i++
		}
		start := i
		for i < len(row) && row[i] >= '0' && row[i] <= '9' {
			i++
		}
		if i == start {
			return i, false
		}
	}
	return i, true
}

func canonicalImportRawMIME(value []byte) bool {
	if len(value) < 5 || value[0] != '"' || value[1] != '\\' || value[2] != '\\' || value[3] != 'x' || value[len(value)-1] != '"' || (len(value)-5)%2 != 0 {
		return false
	}
	for _, c := range value[4 : len(value)-1] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// A copy belongs only to the inner callback. Clobbering never changes the
// reader's row, and wrapImportRow continues to observe the original length.
func (s *Store) wrapImportRowClobber(callback func(string, []byte) error) func(string, []byte) error {
	if !s.importRowClobber {
		return callback
	}
	return func(table string, row []byte) error {
		owned := bytes.Clone(row)
		defer func() {
			for i := range owned {
				owned[i] = 0xff
			}
		}()
		return callback(table, owned)
	}
}

func (s *Store) decodeImportRow(table string, row []byte) (map[string]any, error) {
	if s.importRowDecodeLegacy {
		return decodeImportRow(row)
	}
	return decodeImportRowOnce(table, row)
}

// insertProjectedBytea preserves additive-migration defaults while sending the
// extracted column as a binary bytea parameter instead of a JSON hex string.
func insertProjectedBytea(ctx context.Context, tx pgxExec, table string, obj map[string]any, raw []byte, column string, value []byte) error {
	allowed := importColumns[table]
	keys := make([]string, 0, len(obj)+1)
	for key := range obj {
		if !allowed[key] {
			return fmt.Errorf("%w: %s row has unknown column %q", ErrArchiveContent, table, key)
		}
		keys = append(keys, key)
	}
	if !allowed[column] {
		return fmt.Errorf("%w: %s row has unknown column %q", ErrArchiveContent, table, column)
	}
	if _, exists := obj[column]; !exists {
		keys = append(keys, column)
	}
	sort.Strings(keys)
	cols := strings.Join(keys, ", ")
	selects := make([]string, len(keys))
	for i, key := range keys {
		selects[i] = key
		if key == column {
			selects[i] = "$2::bytea"
		}
	}
	stmt := fmt.Sprintf(`INSERT INTO %s (%s) SELECT %s FROM jsonb_populate_record(NULL::%s, $1::jsonb)`, table, cols, strings.Join(selects, ", "), table)
	if _, err := tx.Exec(ctx, stmt, raw, value); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && table == "accounts" {
			return ErrAccountExists
		}
		return fmt.Errorf("import %s row: %w", table, err)
	}
	return nil
}
