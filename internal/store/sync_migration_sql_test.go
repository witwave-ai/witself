package store

import (
	"errors"
	"slices"
	"strings"
)

var errSyncMigrationSQL = errors.New("unsupported sync migration SQL requires explicit registry review")

type syncSQLToken struct {
	text string
	kind byte // w: unquoted word, q: quoted identifier, s: literal, p: punctuation
}

func (t syncSQLToken) keyword(word string) bool {
	return t.kind == 'w' && strings.EqualFold(t.text, word)
}

// syncSQLStatements splits only on SQL statement boundaries. Literal contents
// are opaque: neither dynamic SQL nor comments are evidence of a created table.
// Unsupported/unterminated lexical forms return one value-free refusal.
func syncSQLStatements(sql string) ([][]syncSQLToken, error) {
	var statements [][]syncSQLToken
	var statement []syncSQLToken
	for i := 0; i < len(sql); {
		start := i
		switch {
		case strings.ContainsRune(" \t\r\n\f", rune(sql[i])):
			i++
		case strings.HasPrefix(sql[i:], "--"):
			for i < len(sql) && sql[i] != '\n' && sql[i] != '\r' {
				i++
			}
		case strings.HasPrefix(sql[i:], "/*"):
			i += 2
			depth := 1
			for i < len(sql) && depth > 0 {
				switch {
				case strings.HasPrefix(sql[i:], "/*"):
					depth++
					i += 2
				case strings.HasPrefix(sql[i:], "*/"):
					depth--
					i += 2
				default:
					i++
				}
			}
			if depth != 0 {
				return nil, errSyncMigrationSQL
			}
		case sql[i] == ';':
			if len(statement) > 0 {
				statements = append(statements, statement)
				statement = nil
			}
			i++
		case sql[i] == '\'' || sql[i] == '"' ||
			((sql[i] == 'E' || sql[i] == 'e') && i+1 < len(sql) && sql[i+1] == '\''):
			escaped := sql[i] != '\'' && sql[i] != '"'
			if escaped {
				i++
			}
			quote := sql[i]
			i++
			closed := false
			for i < len(sql) {
				if escaped && sql[i] == '\\' {
					i += 2
					continue
				}
				if sql[i] == quote {
					i++
					if i < len(sql) && sql[i] == quote {
						i++
						continue
					}
					closed = true
					break
				}
				// Backslashes in ordinary strings depend on server settings.
				if quote == '\'' && !escaped && sql[i] == '\\' {
					return nil, errSyncMigrationSQL
				}
				i++
			}
			if !closed {
				return nil, errSyncMigrationSQL
			}
			if quote == '"' {
				statement = append(statement, syncSQLToken{sql[start:i], 'q'})
			} else {
				statement = append(statement, syncSQLToken{"''", 's'})
			}
		case sql[i] == '$':
			i++
			if i < len(sql) && syncSQLWordStart(sql[i]) {
				for i < len(sql) && (syncSQLWordStart(sql[i]) || sql[i] >= '0' && sql[i] <= '9') {
					i++
				}
			}
			if i >= len(sql) || sql[i] != '$' {
				return nil, errSyncMigrationSQL
			}
			i++
			delimiter := sql[start:i]
			end := strings.Index(sql[i:], delimiter)
			if end < 0 {
				return nil, errSyncMigrationSQL
			}
			i += end + len(delimiter)
			statement = append(statement, syncSQLToken{"''", 's'})
		case syncSQLWordStart(sql[i]):
			i++
			for i < len(sql) && (syncSQLWordStart(sql[i]) || sql[i] >= '0' && sql[i] <= '9' || sql[i] == '$') {
				i++
			}
			statement = append(statement, syncSQLToken{sql[start:i], 'w'})
		case sql[i] >= '0' && sql[i] <= '9':
			i++
			for i < len(sql) && sql[i] >= '0' && sql[i] <= '9' {
				i++
			}
			statement = append(statement, syncSQLToken{sql[start:i], 'p'})
		case strings.ContainsRune("(),.[]=<>!~+-*/%^|&:#?", rune(sql[i])):
			statement = append(statement, syncSQLToken{sql[i : i+1], 'p'})
			i++
		default:
			return nil, errSyncMigrationSQL
		}
	}
	if len(statement) > 0 {
		statements = append(statements, statement)
	}
	return statements, nil
}

func syncSQLWordStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

func syncSQLText(tokens []syncSQLToken) string {
	parts := make([]string, len(tokens))
	for i, token := range tokens {
		parts[i] = token.text
	}
	return strings.Join(parts, " ")
}

// Keep qualified names qualified: a new schema cannot alias an unqualified
// registry entry. Quoted identifiers retain case and escaped identifier bytes.
func syncSQLRelation(tokens []syncSQLToken) (string, int, error) {
	var parts []string
	for i := 0; ; {
		if i >= len(tokens) || (tokens[i].kind != 'w' && tokens[i].kind != 'q') {
			return "", 0, errSyncMigrationSQL
		}
		name := strings.ToLower(tokens[i].text)
		if tokens[i].kind == 'q' {
			name = strings.ReplaceAll(tokens[i].text[1:len(tokens[i].text)-1], `""`, `"`)
		}
		if name == "" {
			return "", 0, errSyncMigrationSQL
		}
		parts = append(parts, name)
		i++
		if i == len(tokens) || tokens[i].text != "." {
			return strings.Join(parts, "."), i, nil
		}
		i++
	}
}

func syncSQLCreateTable(tokens []syncSQLToken) (name string, body []syncSQLToken, table bool, err error) {
	// tokens begins immediately after the unquoted CREATE statement head.
	i := 0
	if len(tokens) > 0 && (tokens[0].keyword("UNLOGGED") || tokens[0].keyword("TEMP") || tokens[0].keyword("TEMPORARY")) {
		i++
	}
	if i >= len(tokens) {
		return "", nil, false, errSyncMigrationSQL
	}
	if !tokens[i].keyword("TABLE") {
		// Only these non-table CREATE forms occur in this migration namespace.
		// Reject unfamiliar forms rather than silently losing a table inventory.
		if i == 0 && (tokens[i].keyword("FUNCTION") || tokens[i].keyword("TRIGGER") || tokens[i].keyword("INDEX")) {
			return "", nil, false, nil
		}
		return "", nil, false, errSyncMigrationSQL
	}
	i++
	if i < len(tokens) && tokens[i].keyword("IF") {
		if i+2 >= len(tokens) || !tokens[i+1].keyword("NOT") || !tokens[i+2].keyword("EXISTS") {
			return "", nil, true, errSyncMigrationSQL
		}
		i += 3
	}
	name, consumed, err := syncSQLRelation(tokens[i:])
	if err != nil {
		return "", nil, true, err
	}
	i += consumed
	if i >= len(tokens) || tokens[i].text != "(" {
		return "", nil, true, errSyncMigrationSQL
	}
	start := i + 1
	depth := 1
	for i++; i < len(tokens); i++ {
		switch tokens[i].text {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				// New table options/inheritance need a deliberate contract review.
				if i != len(tokens)-1 {
					return "", nil, true, errSyncMigrationSQL
				}
				return name, tokens[start:i], true, nil
			}
		}
	}
	return "", nil, true, errSyncMigrationSQL
}

// This is a deliberately bounded DDL pre-check, not a PostgreSQL parser or
// completeness proof. PostgreSQL catalog checks are authoritative. Every ALTER
// TABLE is routed here, and only one reviewed action per statement is accepted:
// multi-action execution order must never be simulated by written action order.
func syncSQLAlterTable(tokens []syncSQLToken, tables map[string]syncMigrationTableContract) error {
	p := syncSQLParser{tokens: tokens}
	name, ok := p.relation()
	table, found := tables[name]
	if !ok || !found {
		return errSyncMigrationSQL
	}
	// Refused statements must not partially change the lexical inventory.
	table.columns = slices.Clone(table.columns)
	table.primaryKey = slices.Clone(table.primaryKey)
	table.references = slices.Clone(table.references)
	// Preserve actual qualification, not dots inside a quoted identifier.
	prefix := ""
	if used := len(tokens) - len(p.tokens); used > 1 {
		parent, _, err := syncSQLRelation(tokens[:used-2])
		if err != nil {
			return errSyncMigrationSQL
		}
		prefix = parent + "."
	}
	if p.take("RENAME") {
		if p.take("TO") {
			newName, ok := p.identifier()
			if !ok || len(p.tokens) != 0 {
				return errSyncMigrationSQL
			}
			newName = prefix + newName
			if _, exists := tables[newName]; exists {
				return errSyncMigrationSQL
			}
			delete(tables, name)
			tables[newName] = table
			for key, related := range tables {
				for i := range related.references {
					if related.references[i].table == name {
						related.references[i].table = newName
					}
				}
				tables[key] = related
			}
			return nil
		}
		p.take("COLUMN")
		old, ok := p.identifier()
		if !ok || !p.take("TO") {
			return errSyncMigrationSQL
		}
		newName, ok := p.identifier()
		index := slices.Index(table.columns, old)
		if !ok || index < 0 || slices.Contains(table.columns, newName) || len(p.tokens) != 0 {
			return errSyncMigrationSQL
		}
		table.columns[index] = newName
		for i, column := range table.primaryKey {
			if column == old {
				table.primaryKey[i] = newName
			}
		}
	} else if !p.alterAction(&table) {
		return errSyncMigrationSQL
	}
	// A comma here starts another action, not a supported expression. Refuse
	// the whole statement instead of emulating PostgreSQL action passes.
	if len(p.tokens) != 0 {
		return errSyncMigrationSQL
	}
	tables[name] = table
	return nil
}

type syncSQLParser struct{ tokens []syncSQLToken }

func (p *syncSQLParser) peek(text string) bool {
	return len(p.tokens) > 0 && (p.tokens[0].keyword(text) || p.tokens[0].kind == 'p' && p.tokens[0].text == text)
}

func (p *syncSQLParser) take(text string) bool {
	if !p.peek(text) {
		return false
	}
	p.tokens = p.tokens[1:]
	return true
}

func (p *syncSQLParser) relation() (string, bool) {
	name, used, err := syncSQLRelation(p.tokens)
	if err != nil {
		return "", false
	}
	p.tokens = p.tokens[used:]
	return name, true
}

func (p *syncSQLParser) identifier() (string, bool) {
	if len(p.tokens) == 0 {
		return "", false
	}
	name, _, err := syncSQLRelation(p.tokens[:1])
	if err != nil {
		return "", false
	}
	p.tokens = p.tokens[1:]
	return name, true
}

func (p *syncSQLParser) alterAction(table *syncMigrationTableContract) bool {
	switch {
	case p.take("ADD"):
		if p.take("CONSTRAINT") {
			if _, ok := p.identifier(); !ok {
				return false
			}
			return p.constraint(table, "")
		}
		p.take("COLUMN")
		ifAbsent := p.take("IF")
		if ifAbsent && (!p.take("NOT") || !p.take("EXISTS")) {
			return false
		}
		column, ok := p.identifier()
		if !ok || !p.dataType() {
			return false
		}
		// IF NOT EXISTS skips the entire definition for an existing column.
		changed := *table
		changed.columns = slices.Clone(table.columns)
		changed.primaryKey = slices.Clone(table.primaryKey)
		changed.references = slices.Clone(table.references)
		for len(p.tokens) > 0 && !p.peek(",") {
			if p.take("DEFAULT") {
				if !p.expression() {
					return false
				}
			} else if p.take("COLLATE") {
				if _, ok := p.relation(); !ok {
					return false
				}
			} else {
				if p.take("CONSTRAINT") {
					if _, ok := p.identifier(); !ok {
						return false
					}
				}
				if !p.constraint(&changed, column) {
					return false
				}
			}
		}
		if slices.Contains(table.columns, column) {
			return ifAbsent
		}
		changed.columns = append(changed.columns, column)
		*table = changed
		return true
	case p.take("DROP"):
		p.take("COLUMN")
		ifPresent := p.take("IF")
		if ifPresent && !p.take("EXISTS") {
			return false
		}
		column, ok := p.identifier()
		if !ok {
			return false
		}
		p.take("RESTRICT")
		// CASCADE may change other tables/constraints: require explicit review.
		index := slices.Index(table.columns, column)
		if index < 0 {
			return ifPresent
		}
		table.columns = slices.Delete(table.columns, index, index+1)
		if slices.Contains(table.primaryKey, column) {
			table.primaryKey = nil
		}
		return true
	case p.take("ALTER"):
		p.take("COLUMN")
		column, ok := p.identifier()
		if !ok || !slices.Contains(table.columns, column) {
			return false
		}
		switch {
		case p.take("SET"):
			switch {
			case p.take("DEFAULT"):
				return p.expression()
			case p.take("NOT"):
				return p.take("NULL")
			case p.take("DATA"):
				return p.take("TYPE") && p.alterType()
			}
		case p.take("DROP"):
			return p.take("DEFAULT") || p.take("NOT") && p.take("NULL")
		case p.take("TYPE"):
			return p.alterType()
		}
	}
	return false
}

func (p *syncSQLParser) alterType() bool {
	if !p.dataType() {
		return false
	}
	if p.take("COLLATE") {
		if _, ok := p.relation(); !ok {
			return false
		}
	}
	return !p.take("USING") || p.expression()
}

func (p *syncSQLParser) dataType() bool {
	// Unquoted action/constraint keywords cannot stand in for a missing type.
	for _, word := range []string{"ADD", "DROP", "ALTER", "RENAME", "DEFAULT", "NOT", "NULL", "CONSTRAINT", "PRIMARY", "FOREIGN", "UNIQUE", "CHECK", "REFERENCES", "COLLATE"} {
		if p.peek(word) {
			return false
		}
	}
	name, ok := p.relation()
	if !ok {
		return false
	}
	switch name {
	case "double":
		if !p.take("PRECISION") {
			return false
		}
	case "character", "char", "bit":
		p.take("VARYING")
	}
	if p.take("(") {
		for {
			if !p.number() {
				return false
			}
			if !p.take(",") {
				break
			}
		}
		if !p.take(")") {
			return false
		}
	}
	if name == "timestamp" || name == "time" {
		if p.take("WITH") || p.take("WITHOUT") {
			if !p.take("TIME") || !p.take("ZONE") {
				return false
			}
		}
	}
	for p.take("[") {
		p.number()
		if !p.take("]") {
			return false
		}
	}
	return true
}

func (p *syncSQLParser) number() bool {
	if len(p.tokens) == 0 || p.tokens[0].kind != 'p' || p.tokens[0].text[0] < '0' || p.tokens[0].text[0] > '9' {
		return false
	}
	p.tokens = p.tokens[1:]
	return true
}

// Expressions support literals, identifiers, calls, parentheses, casts and
// common operators. Unconsumed words never become an opaque action suffix.
func (p *syncSQLParser) expression() bool {
	if !p.atom() {
		return false
	}
	for {
		if p.take(":") {
			if !p.take(":") || !p.dataType() {
				return false
			}
			continue
		}
		operator := false
		for _, op := range []string{"+", "-", "*", "/", "%", "=", "<", ">", "!", "|", "AND", "OR"} {
			if p.take(op) {
				operator = true
				if op == "<" || op == ">" || op == "!" {
					if !p.take("=") && op == "<" {
						p.take(">")
					}
				} else if op == "|" && !p.take("|") {
					return false
				}
				break
			}
		}
		if p.take("IS") {
			p.take("NOT")
			if !p.take("NULL") && !p.take("TRUE") && !p.take("FALSE") {
				return false
			}
			continue
		}
		if !operator {
			return true
		}
		if !p.atom() {
			return false
		}
	}
}

func (p *syncSQLParser) atom() bool {
	if p.take("+") || p.take("-") || p.take("NOT") {
		return p.atom()
	}
	if p.take("(") {
		return p.expression() && p.take(")")
	}
	if len(p.tokens) > 0 && p.tokens[0].kind == 's' {
		p.tokens = p.tokens[1:]
		return true
	}
	if p.number() {
		return !p.take(".") || p.number()
	}
	for _, word := range []string{"ADD", "DROP", "ALTER", "RENAME", "DEFAULT", "CONSTRAINT", "PRIMARY", "FOREIGN", "UNIQUE", "CHECK", "REFERENCES", "SET", "INHERIT"} {
		if p.peek(word) {
			return false
		}
	}
	if _, ok := p.relation(); !ok {
		return false
	}
	if !p.take("(") || p.take(")") {
		return true
	}
	for {
		if !p.expression() {
			return false
		}
		if !p.take(",") {
			return p.take(")")
		}
	}
}

func (p *syncSQLParser) columnList() ([]string, bool) {
	if !p.take("(") {
		return nil, false
	}
	var columns []string
	for {
		column, ok := p.identifier()
		if !ok {
			return nil, false
		}
		columns = append(columns, column)
		if !p.take(",") {
			return columns, p.take(")")
		}
	}
}

func (p *syncSQLParser) constraint(table *syncMigrationTableContract, column string) bool {
	var columns []string
	var ok bool
	switch {
	case p.take("NOT"):
		return column != "" && p.take("NULL")
	case p.take("NULL"):
		return column != ""
	case p.take("PRIMARY"):
		if !p.take("KEY") || len(table.primaryKey) != 0 {
			return false
		}
		if column != "" {
			table.primaryKey = []string{column}
		} else {
			columns, ok = p.columnList()
			if !ok || !syncSQLKnownColumns(table, columns) {
				return false
			}
			table.primaryKey = columns
		}
	case p.take("UNIQUE"):
		if column == "" {
			columns, ok = p.columnList()
			if !ok || !syncSQLKnownColumns(table, columns) {
				return false
			}
		}
	case p.take("CHECK"):
		if !p.take("(") || !p.expression() || !p.take(")") {
			return false
		}
	case p.peek("FOREIGN"), p.peek("REFERENCES"):
		if column == "" {
			if !p.take("FOREIGN") || !p.take("KEY") {
				return false
			}
			columns, ok = p.columnList()
			if !ok || !syncSQLKnownColumns(table, columns) {
				return false
			}
		} else {
			columns = []string{column}
		}
		if !p.take("REFERENCES") {
			return false
		}
		relation, ok := p.relation()
		if !ok {
			return false
		}
		referenced, ok := p.columnList()
		if !ok || len(referenced) != len(columns) {
			return false
		}
		deferred, ok := p.constraintTiming()
		if !ok {
			return false
		}
		table.references = append(table.references, syncMigrationReference{table: relation, deferred: deferred})
		return true
	default:
		return false
	}
	_, ok = p.constraintTiming()
	return ok
}

func (p *syncSQLParser) constraintTiming() (bool, bool) {
	deferrable := p.take("DEFERRABLE")
	if !deferrable && p.peek("NOT") && len(p.tokens) > 1 && p.tokens[1].keyword("DEFERRABLE") {
		p.tokens = p.tokens[2:]
	}
	deferred := false
	if p.take("INITIALLY") {
		deferred = p.take("DEFERRED")
		if !deferred && !p.take("IMMEDIATE") || deferred && !deferrable {
			return false, false
		}
	}
	if p.peek("NOT") && len(p.tokens) > 1 && p.tokens[1].keyword("VALID") {
		p.tokens = p.tokens[2:]
	}
	return deferred, true
}

func syncSQLKnownColumns(table *syncMigrationTableContract, columns []string) bool {
	for _, column := range columns {
		if !slices.Contains(table.columns, column) {
			return false
		}
	}
	return true
}
