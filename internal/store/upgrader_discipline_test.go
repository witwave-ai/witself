package store

import (
	"embed"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/export"
)

// TestNonAdditiveMigrationsHaveUpgraders is the CI gate on archive
// forward-compatibility. Any migration whose SQL changes the shape of
// data on the wire (rename a column, drop a column, change a column's
// type, rename a table, add a check/uniqueness constraint that existing
// archives might not satisfy) must ship with a matching upgrader entry
// in internal/export/upgrade.go — otherwise archives written before that
// migration become unimportable the first time someone restores them.
//
// The failure mode the test defends against is silent-until-it-matters:
// nothing goes wrong at deploy time; the stranded archive doesn't
// surface until someone runs `witself-infra up -restore-archives` months
// later on a new cell, at which point the fix requires writing the
// upgrader retroactively and redeploying. This test catches the omission
// at PR time.
//
// See docs/upgrader-discipline.md for the conceptual model and
// GitHub issue #18 for the design decision on which SQL patterns count.
//
// Grandfathering: migrations at schema <= firstArchivableSchema predate
// the archive/restore arc. No archive at those schemas can exist, so
// their non-additive changes cannot strand anything. Migrations from
// firstArchivableSchema+1 onward are enforced.
const firstArchivableSchema = 13

func TestNonAdditiveMigrationsHaveUpgraders(t *testing.T) {
	entries, err := disciplineMigrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		version, err := migrationNumber(name)
		if err != nil {
			t.Errorf("migration %s has an unparseable version number: %v", name, err)
			continue
		}
		if version <= firstArchivableSchema {
			continue // grandfathered: no archive at this schema can exist
		}
		raw, err := disciplineMigrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		sql := stripSQLComments(stripGooseDown(string(raw)))

		hits := detectNonAdditive(sql)
		if len(hits) == 0 {
			continue
		}

		// A migration numbered N changes data at that version. Archives
		// written at any schema < N need an upgrader from that older
		// version onward to survive. The MINIMUM the migration owes the
		// registry is an entry keyed at N-1 (lifting archives written
		// immediately before this migration into the post-migration
		// shape). Older gaps in the chain are a separate concern;
		// this test only enforces the same-commit contract.
		if version < 1 {
			t.Errorf("%s: migration version %d has no predecessor slot; upgraders keyed at v-1 only make sense for v>=1", name, version)
			continue
		}
		if export.UpgraderFor(version-1) == nil {
			t.Errorf(
				"%s (schema %d) contains non-additive change(s) but "+
					"upgraders[%d] is not registered — a rename/drop/type-change/tightening "+
					"needs a matching upgrader in internal/export/upgrade.go, "+
					"registered at the schema version BEFORE this migration (%d), "+
					"or every archive at schema<=%d becomes unimportable.\n"+
					"  patterns detected:\n%s",
				name, version, version-1, version-1, version-1, formatHits(hits),
			)
		}
	}
}

// migrationNumber pulls the leading integer out of a filename like
// 0013_add_account_suspended.sql. The migration runner uses the same
// convention.
func migrationNumber(filename string) (int, error) {
	i := strings.IndexByte(filename, '_')
	if i <= 0 {
		return 0, fmt.Errorf("no version prefix in %q", filename)
	}
	n, err := strconv.Atoi(filename[:i])
	if err != nil {
		return 0, err
	}
	return n, nil
}

// stripGooseDown removes the rollback half of a goose-format migration.
// Down SQL is never applied by the migration runner in production — it
// exists so an operator can undo a migration by hand — so anything after
// the `-- +goose Down` marker is not a forward change and must not
// trigger the discipline check. Migrations that do not use the goose
// idiom (no marker) pass through unchanged.
func stripGooseDown(sql string) string {
	marker := regexp.MustCompile(`(?im)^\s*--\s*\+goose\s+Down\b`)
	if loc := marker.FindStringIndex(sql); loc != nil {
		return sql[:loc[0]]
	}
	return sql
}

// stripSQLComments removes -- line comments and /* ... */ block comments
// so a comment that happens to say "-- WARNING: never rename a column"
// doesn't trip the detector.
func stripSQLComments(sql string) string {
	// Remove /* ... */ blocks first (may span multiple lines).
	blockComment := regexp.MustCompile(`(?s)/\*.*?\*/`)
	sql = blockComment.ReplaceAllString(sql, " ")
	// Then -- line comments to end of line.
	lineComment := regexp.MustCompile(`--[^\n]*`)
	sql = lineComment.ReplaceAllString(sql, " ")
	return sql
}

// nonAdditivePattern names one shape of schema change that would strand
// existing archives if unaccompanied by an upgrader entry.
type nonAdditivePattern struct {
	name        string
	description string
	re          *regexp.Regexp
}

// clausePatterns match ONE clause of an ALTER TABLE. Postgres accepts
// comma-separated actions on the same table (ALTER TABLE X ADD COLUMN a,
// DROP COLUMN b, ALTER COLUMN c TYPE T), and the destructive clause is
// often not the first one. Every pattern here is written against a
// bare clause (no leading "ALTER TABLE X"), which the outer scanner
// splits ALTER TABLE statements into and feeds one at a time. The
// COLUMN keyword is optional in Postgres — `RENAME a TO b`,
// `DROP a`, and `ALTER a TYPE T` are all legal — so every pattern
// treats `column` as optional.
var clausePatterns = []nonAdditivePattern{
	{
		name:        "RENAME COLUMN",
		description: "renaming a column strands archives that still carry the old column name",
		re:          regexp.MustCompile(`(?is)^\s*rename\s+(?:column\s+)?\S+\s+to\s+\S+`),
	},
	{
		name:        "RENAME TO",
		description: "renaming a table strands archives that still name the old table",
		re:          regexp.MustCompile(`(?is)^\s*rename\s+to\s+\S+`),
	},
	{
		name:        "DROP COLUMN",
		description: "dropping a column leaves archived values with nowhere to land",
		re:          regexp.MustCompile(`(?is)^\s*drop\s+(?:column\s+)?(?:if\s+exists\s+)?\S+`),
	},
	{
		name:        "ALTER COLUMN TYPE",
		description: "changing a column type may make archived values un-castable or lossy",
		re:          regexp.MustCompile(`(?is)^\s*alter\s+(?:column\s+)?\S+\s+(?:set\s+data\s+)?type\s+`),
	},
	{
		name:        "SET NOT NULL",
		description: "tightening a nullable column may reject archives that carry NULL there",
		re:          regexp.MustCompile(`(?is)^\s*alter\s+(?:column\s+)?\S+\s+set\s+not\s+null`),
	},
	{
		name:        "SET DEFAULT",
		description: "changing a column default silently changes what absent-column archive rows land as",
		re:          regexp.MustCompile(`(?is)^\s*alter\s+(?:column\s+)?\S+\s+set\s+default\b`),
	},
	{
		name:        "ADD CHECK",
		description: "adding a CHECK constraint may reject archives that violate it",
		re:          regexp.MustCompile(`(?is)^\s*add\s+(?:constraint\s+\S+\s+)?check\b`),
	},
	{
		name:        "ADD UNIQUE",
		description: "adding a UNIQUE constraint may reject archives that violate it",
		re:          regexp.MustCompile(`(?is)^\s*add\s+(?:constraint\s+\S+\s+)?unique\b`),
	},
	{
		name:        "ADD EXCLUDE",
		description: "adding an EXCLUDE constraint may reject archives that violate it",
		re:          regexp.MustCompile(`(?is)^\s*add\s+(?:constraint\s+\S+\s+)?exclude\b`),
	},
	{
		name:        "ADD PRIMARY KEY",
		description: "adding a PRIMARY KEY tightens uniqueness AND nullability at once — archives may violate either",
		re:          regexp.MustCompile(`(?is)^\s*add\s+(?:constraint\s+\S+\s+)?primary\s+key\b`),
	},
	{
		name:        "ADD FOREIGN KEY",
		description: "adding a FOREIGN KEY rejects archives whose reference targets are absent",
		re:          regexp.MustCompile(`(?is)^\s*add\s+(?:constraint\s+\S+\s+)?foreign\s+key\b`),
	},
	{
		name:        "DROP CONSTRAINT",
		description: "dropping a constraint changes the shape of what the destination will accept — archives may have relied on it",
		re:          regexp.MustCompile(`(?is)^\s*drop\s+constraint\b`),
	},
}

// statementPatterns match against the whole preprocessed SQL, not per-clause.
// These are standalone statements a migration might contain outside
// ALTER TABLE.
var statementPatterns = []nonAdditivePattern{
	{
		name:        "DROP TABLE",
		description: "dropping a table strands every archived row of it",
		re:          regexp.MustCompile(`(?is)\bdrop\s+table\b`),
	},
	{
		name:        "CREATE UNIQUE INDEX",
		description: "creating a unique index may reject archives that violate it",
		re:          regexp.MustCompile(`(?is)\bcreate\s+unique\s+index\b`),
	},
	{
		name:        "CREATE TRIGGER",
		description: "a new trigger may reject or transform archive rows on INSERT",
		re:          regexp.MustCompile(`(?is)\bcreate\s+(?:or\s+replace\s+)?(?:constraint\s+)?trigger\b`),
	},
	{
		name:        "CREATE POLICY",
		description: "a row-level-security policy may hide archived-eligible rows on restore",
		re:          regexp.MustCompile(`(?is)\bcreate\s+policy\b`),
	},
	{
		name:        "REVOKE",
		description: "revoking a privilege the restore role needs blocks archive INSERTs",
		re:          regexp.MustCompile(`(?is)\brevoke\b`),
	},
	{
		name:        "TRUNCATE",
		description: "TRUNCATE inside a migration removes data an archived row may still reference",
		re:          regexp.MustCompile(`(?is)\btruncate\b`),
	},
	{
		name:        "ALTER TYPE RENAME VALUE",
		description: "renaming an enum value strands archives that carry the old label",
		re:          regexp.MustCompile(`(?is)\balter\s+type\s+\S+\s+rename\s+value\b`),
	},
	{
		name:        "ALTER TYPE DROP VALUE",
		description: "dropping an enum value strands archived rows carrying it (Postgres doesn't support this directly, but the pattern is worth flagging in case a workaround is scripted)",
		re:          regexp.MustCompile(`(?is)\balter\s+type\s+\S+\s+drop\s+value\b`),
	},
	{
		name:        "DROP TYPE",
		description: "dropping a type strands columns and archived values that use it",
		re:          regexp.MustCompile(`(?is)\bdrop\s+type\b`),
	},
	{
		name:        "ALTER SEQUENCE RESTART",
		description: "restarting a sequence may clash with archived IDs when they land back on restore",
		re:          regexp.MustCompile(`(?is)\balter\s+sequence\s+\S+\s+restart\b`),
	},
}

type detectorHit struct {
	pattern nonAdditivePattern
	match   string
}

// alterTableRe finds the head of an ALTER TABLE statement — everything from
// `ALTER TABLE <name>` up to the terminating semicolon. The body is fed to
// splitAlterTableClauses, which yields one entry per comma-separated
// action (respecting parentheses so a CHECK expression like `CHECK(a > 0,
// b < 5)` isn't split inside the expression).
var alterTableRe = regexp.MustCompile(`(?is)\balter\s+table\s+(?:only\s+)?(?:if\s+exists\s+)?\S+\s*([^;]*);`)

func detectNonAdditive(sql string) []detectorHit {
	var hits []detectorHit
	// Statement-level scan first: DROP TABLE, CREATE UNIQUE INDEX,
	// TRIGGER/POLICY/REVOKE/TRUNCATE, ALTER TYPE, DROP TYPE, ALTER SEQUENCE.
	for _, p := range statementPatterns {
		if loc := p.re.FindStringIndex(sql); loc != nil {
			snip := sql[loc[0]:loc[1]]
			snip = strings.Join(strings.Fields(snip), " ")
			hits = append(hits, detectorHit{pattern: p, match: snip})
		}
	}
	// Per-clause ALTER TABLE scan: split into individual actions so a
	// destructive clause hiding behind an additive first clause is caught.
	for _, tm := range alterTableRe.FindAllStringSubmatch(sql, -1) {
		body := tm[1]
		for _, clause := range splitAlterTableClauses(body) {
			for _, p := range clausePatterns {
				if loc := p.re.FindStringIndex(clause); loc != nil {
					snip := clause[loc[0]:loc[1]]
					snip = strings.Join(strings.Fields(snip), " ")
					hits = append(hits, detectorHit{pattern: p, match: snip})
				}
			}
		}
	}
	return hits
}

// splitAlterTableClauses splits an ALTER TABLE body on top-level commas —
// commas that appear inside parentheses (CHECK expressions, UNIQUE column
// lists, FOREIGN KEY references) do NOT split. Postgres's own parser
// treats the multi-clause form this way.
func splitAlterTableClauses(body string) []string {
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, body[start:i])
				start = i + 1
			}
		}
	}
	tail := body[start:]
	if strings.TrimSpace(tail) != "" {
		out = append(out, tail)
	}
	return out
}

func formatHits(hits []detectorHit) string {
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "    - %s: %s\n      %s\n", h.pattern.name, h.pattern.description, h.match)
	}
	return b.String()
}

// TestNonAdditiveDetectorCatchesWhatItShould pins the detector's own
// behavior against a set of hand-crafted example SQL snippets that
// SHOULD trip each pattern, plus additive snippets that SHOULD NOT.
// If someone edits nonAdditivePatterns and accidentally weakens a
// regex, this test fails immediately — long before the outer discipline
// test would misfire on a real migration.
func TestNonAdditiveDetectorCatchesWhatItShould(t *testing.T) {
	shouldTrip := map[string]string{
		// The original shortlist.
		"rename column":    `ALTER TABLE accounts RENAME COLUMN display_name TO label;`,
		"rename table":     `ALTER TABLE accounts RENAME TO tenants;`,
		"drop column":      `ALTER TABLE accounts DROP COLUMN closed_reason;`,
		"drop column if":   `ALTER TABLE accounts DROP COLUMN IF EXISTS closed_reason;`,
		"drop table":       `DROP TABLE tokens;`,
		"drop table if":    `DROP TABLE IF EXISTS tokens;`,
		"alter type":       `ALTER TABLE accounts ALTER COLUMN status TYPE varchar(32);`,
		"alter type SDT":   `ALTER TABLE accounts ALTER COLUMN status SET DATA TYPE varchar(32);`,
		"set not null":     `ALTER TABLE accounts ALTER COLUMN closed_at SET NOT NULL;`,
		"add check":        `ALTER TABLE accounts ADD CHECK (status IN ('active','closed'));`,
		"add named check":  `ALTER TABLE accounts ADD CONSTRAINT status_ck CHECK (status IN ('active','closed'));`,
		"add unique":       `ALTER TABLE accounts ADD UNIQUE (email);`,
		"add named unique": `ALTER TABLE accounts ADD CONSTRAINT email_uk UNIQUE (email);`,
		"create unique ix": `CREATE UNIQUE INDEX ON accounts(email);`,
		"multiline rename": "ALTER TABLE accounts\n  RENAME COLUMN display_name\n  TO label;",

		// Multi-clause ALTER TABLE — destructive clause hides behind an
		// additive first clause. Every one of these was a confirmed
		// evasion of the first-anchor-only detector.
		"multi drop after add":            `ALTER TABLE accounts ADD COLUMN nickname text, DROP COLUMN closed_reason;`,
		"multi type after add":            `ALTER TABLE accounts ADD COLUMN nickname text, ALTER COLUMN status TYPE varchar(8);`,
		"multi not-null after add":        `ALTER TABLE accounts ADD COLUMN nickname text, ALTER COLUMN closed_at SET NOT NULL;`,
		"multi unique after add":          `ALTER TABLE accounts ADD COLUMN nickname text, ADD UNIQUE (email);`,
		"multi rename after add":          `ALTER TABLE accounts ADD COLUMN nickname text, RENAME COLUMN display_name TO label;`,
		"multi kitchen sink":              `ALTER TABLE accounts ADD COLUMN nickname text, ALTER COLUMN status TYPE varchar(8), DROP COLUMN closed_reason;`,
		"multi drop then add same column": `ALTER TABLE accounts DROP COLUMN closed_reason, ADD COLUMN closed_reason integer;`,

		// COLUMN keyword omitted (Postgres accepts this).
		"drop without COLUMN":     `ALTER TABLE accounts DROP closed_reason;`,
		"rename without COLUMN":   `ALTER TABLE accounts RENAME display_name TO label;`,
		"alter type without COL":  `ALTER TABLE accounts ALTER status TYPE varchar(8);`,
		"set not null without CO": `ALTER TABLE accounts ALTER closed_at SET NOT NULL;`,

		// Additional per-clause patterns beyond the shortlist.
		"add primary key":          `ALTER TABLE accounts ADD PRIMARY KEY (email);`,
		"add named foreign key":    `ALTER TABLE tokens ADD CONSTRAINT tokens_op_fk FOREIGN KEY (operator_id) REFERENCES operators(id);`,
		"drop constraint":          `ALTER TABLE accounts DROP CONSTRAINT accounts_email_uk;`,
		"set default":              `ALTER TABLE accounts ALTER COLUMN status SET DEFAULT 'pending';`,
		"set default no COLUMN":    `ALTER TABLE accounts ALTER status SET DEFAULT 'pending';`,
		"multi set default hidden": `ALTER TABLE accounts ADD COLUMN nickname text, ALTER COLUMN status SET DEFAULT 'pending';`,

		// Statement-level additions.
		"create trigger":            `CREATE TRIGGER accounts_status_check BEFORE INSERT ON accounts FOR EACH ROW EXECUTE FUNCTION check_status();`,
		"create constraint trigger": `CREATE CONSTRAINT TRIGGER x AFTER INSERT ON accounts FOR EACH ROW EXECUTE FUNCTION f();`,
		"create policy":             `CREATE POLICY accounts_owner ON accounts USING (owner_id = current_setting('app.uid'));`,
		"revoke insert":             `REVOKE INSERT ON accounts FROM restore_role;`,
		"truncate":                  `TRUNCATE accounts;`,
		"alter type rename value":   `ALTER TYPE account_status RENAME VALUE 'pending' TO 'awaiting_verification';`,
		"drop type":                 `DROP TYPE account_status;`,
		"drop type cascade":         `DROP TYPE account_status CASCADE;`,
		"alter sequence restart":    `ALTER SEQUENCE accounts_id_seq RESTART WITH 1;`,

		// Parenthesized commas do NOT split — CHECK expression stays whole.
		"check with comma in expr": `ALTER TABLE accounts ADD CONSTRAINT status_ck CHECK (status IN ('a','b','c'));`,
	}
	for name, sql := range shouldTrip {
		if hits := detectNonAdditive(stripSQLComments(sql)); len(hits) == 0 {
			t.Errorf("%q: detector did NOT trip on %q — a non-additive change would slip past CI", name, sql)
		}
	}

	shouldNotTrip := map[string]string{
		"add column nullable":   `ALTER TABLE accounts ADD COLUMN nickname text;`,
		"add column not null":   `ALTER TABLE accounts ADD COLUMN plan text NOT NULL DEFAULT 'free';`,
		"add column if exists":  `ALTER TABLE accounts ADD COLUMN IF NOT EXISTS nickname text;`,
		"comment mentions drop": `-- careful: dropping columns is never additive` + "\n" + `ALTER TABLE accounts ADD COLUMN nickname text;`,
		"block comment rename":  `/* fixed rename in prior migration */` + "\n" + `ALTER TABLE accounts ADD COLUMN nickname text;`,
		"create table":          `CREATE TABLE new_table (id text primary key);`,
		"create index":          `CREATE INDEX ON accounts(email);`,
		"insert seed row":       `INSERT INTO accounts (id, is_default) VALUES ('acc_seed', true);`,
		"drop_index_is_ok":      `DROP INDEX accounts_email_ix;`,
		"drop_default_is_ok":    `ALTER TABLE accounts ALTER COLUMN plan DROP DEFAULT;`,
		// CREATE TABLE with an inline PRIMARY KEY is fine — the table
		// is fresh, no archived rows exist yet at that shape. Only
		// ADDING a primary key to an existing table strands archives.
		"create table w/ pk inline": `CREATE TABLE new_table (id text primary key, name text);`,
		"create table w/ fk inline": `CREATE TABLE new_table (id text primary key, account_id text REFERENCES accounts(id));`,
		"create function":           `CREATE FUNCTION check_status() RETURNS trigger AS $$ BEGIN NEW.status := lower(NEW.status); RETURN NEW; END; $$ LANGUAGE plpgsql;`,
		// Adding a nullable column stays additive regardless of what's
		// alongside — a multi-clause ADD + ADD is still additive.
		"multi additive only": `ALTER TABLE accounts ADD COLUMN nickname text, ADD COLUMN pronouns text;`,
		// Comments that literally say things like "drop column" and
		// "rename" but the SQL itself is additive must stay quiet.
		"comment says trigger":  `-- adds a trigger someday, not today` + "\n" + `ALTER TABLE accounts ADD COLUMN nickname text;`,
		"comment says truncate": `-- IMPORTANT: never TRUNCATE this table` + "\n" + `ALTER TABLE accounts ADD COLUMN nickname text;`,
	}
	for name, sql := range shouldNotTrip {
		if hits := detectNonAdditive(stripSQLComments(sql)); len(hits) > 0 {
			t.Errorf("%q: detector tripped on %q (patterns: %v) — an additive change would be forced through the upgrader gate", name, sql, hits)
		}
	}
}

//go:embed migrations
var disciplineMigrationsFS embed.FS
