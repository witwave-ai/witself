package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestTranscriptStorageUsageSQLStaysInOneFile(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var importFunction *ast.FuncDecl
	archiveKeys := 0
	inventoryKeys := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var archiveKey *ast.BasicLit
		if path == "archive_tables.go" {
			archiveKey = transcriptStorageArchiveKey(t, file)
			if archiveKey != nil {
				archiveKeys++
			}
		}
		var inventoryKey *ast.BasicLit
		if path == "sync_writer_inventory.go" {
			inventoryKey = transcriptStorageSyncInventoryKey(file)
			if inventoryKey != nil {
				inventoryKeys++
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if fn, ok := node.(*ast.FuncDecl); ok && fn.Name.Name == "importAccountWithTracker" {
				if importFunction != nil {
					t.Error("multiple importAccountWithTracker declarations")
				}
				importFunction = fn
			}
			return true
		})
		for _, literal := range transcriptStorageForbiddenLiterals(path, file, archiveKey, inventoryKey) {
			t.Errorf("counter SQL or name outside its helper file at %s", fset.Position(literal.Pos()))
		}
	}
	if archiveKeys != 1 {
		t.Fatalf("archive exclusion keys = %d, want exactly one", archiveKeys)
	}
	if inventoryKeys != 1 {
		t.Fatalf("counter inventory exemption count = %d, want exactly one", inventoryKeys)
	}
	if importFunction == nil {
		t.Fatal("importAccountWithTracker not found")
	}
	assertTranscriptStorageImportPlacement(t, importFunction)
}

// The guard inventory is metadata, not another SQL writer. Exempt exactly one
// direct string element in the top-level var syncWriterTables = []string{...}
// declaration. A duplicate, nested declaration, array, map, or expression does
// not gain a filename-wide exemption.
func transcriptStorageSyncInventoryKey(file *ast.File) *ast.BasicLit {
	var key *ast.BasicLit
	declarations := 0
	for _, declaration := range file.Decls {
		group, ok := declaration.(*ast.GenDecl)
		if !ok || group.Tok != token.VAR {
			continue
		}
		for _, spec := range group.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range value.Names {
				if name.Name != "syncWriterTables" {
					continue
				}
				declarations++
				if declarations != 1 || len(value.Names) != 1 || len(value.Values) != 1 {
					return nil
				}
				list, ok := value.Values[0].(*ast.CompositeLit)
				if !ok {
					return nil
				}
				slice, ok := list.Type.(*ast.ArrayType)
				if !ok || slice.Len != nil {
					return nil
				}
				element, ok := slice.Elt.(*ast.Ident)
				if !ok || element.Name != "string" {
					return nil
				}
				for _, entry := range list.Elts {
					literal, ok := entry.(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						continue
					}
					text, err := strconv.Unquote(literal.Value)
					if err != nil {
						return nil
					}
					if text == "transcript_storage_usage" {
						if key != nil {
							return nil
						}
						key = literal
					}
				}
			}
		}
	}
	return key
}

func transcriptStorageForbiddenLiterals(path string, file *ast.File, archiveKey, inventoryKey *ast.BasicLit) []*ast.BasicLit {
	var forbidden []*ast.BasicLit
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err != nil {
			forbidden = append(forbidden, literal)
			return true
		}
		if !strings.Contains(value, "transcript_storage_usage") &&
			!strings.Contains(value, "witself_transcript_entry_storage_bytes") &&
			!strings.Contains(value, "witself_transcript_storage_shard") {
			return true
		}
		if path != "transcript_storage_usage.go" &&
			(path != "archive_tables.go" || literal != archiveKey) &&
			(path != "sync_writer_inventory.go" || literal != inventoryKey) {
			forbidden = append(forbidden, literal)
		}
		return true
	})
	return forbidden
}

func TestTranscriptStorageUsageInventoryExemptionIsExact(t *testing.T) {
	const allowed = `var syncWriterTables = []string{"transcript_storage_usage"}`
	for _, tc := range []struct {
		name, path, declarations string
		accepted                 bool
	}{
		{"one_inventory_entry", "sync_writer_inventory.go", allowed, true},
		{"SQL_elsewhere", "sync_writer_inventory.go", allowed + `; const query = "SELECT * FROM transcript_storage_usage"`, false},
		{"name_elsewhere", "sync_writer_inventory.go", allowed + `; var other = "transcript_storage_usage"`, false},
		{"helper_name_elsewhere", "sync_writer_inventory.go", allowed + `; const query = "SELECT witself_transcript_storage_shard()"`, false},
		{"extra_inventory_entry", "sync_writer_inventory.go", `var syncWriterTables = []string{"transcript_storage_usage", "transcript_storage_usage"}`, false},
		{"extra_declaration", "sync_writer_inventory.go", allowed + ";" + allowed, false},
		{"wrong_file", "other.go", allowed, false},
		{"wrong_declaration", "sync_writer_inventory.go", `var other = []string{"transcript_storage_usage"}`, false},
		{"nested_declaration", "sync_writer_inventory.go", `func writer() { ` + allowed + `; _ = syncWriterTables }`, false},
		{"array_declaration", "sync_writer_inventory.go", `var syncWriterTables = [1]string{"transcript_storage_usage"}`, false},
		{"SQL_inventory_entry", "sync_writer_inventory.go", `var syncWriterTables = []string{"SELECT * FROM transcript_storage_usage"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), tc.path, "package store\n"+tc.declarations, 0)
			if err != nil {
				t.Fatalf("counter exemption fixture parsing failed: %v", err)
			}
			key := transcriptStorageSyncInventoryKey(file)
			accepted := key != nil && len(transcriptStorageForbiddenLiterals(tc.path, file, nil, key)) == 0
			if accepted != tc.accepted {
				t.Fatalf("counter exemption fixture accepted = %t, want %t", accepted, tc.accepted)
			}
		})
	}
}

func transcriptStorageArchiveKey(t *testing.T, file *ast.File) *ast.BasicLit {
	t.Helper()
	var key *ast.BasicLit
	ast.Inspect(file, func(node ast.Node) bool {
		decl, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range decl.Names {
			if name.Name != "cellLocalArchiveExclusions" || i >= len(decl.Values) {
				continue
			}
			entries, ok := decl.Values[i].(*ast.CompositeLit)
			if !ok {
				t.Fatal("cellLocalArchiveExclusions is not a map literal")
			}
			for _, entry := range entries.Elts {
				pair, ok := entry.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				literal, ok := pair.Key.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal(err)
				}
				if value == "transcript_storage_usage" {
					if key != nil {
						t.Fatal("duplicate transcript_storage_usage archive exclusion")
					}
					key = literal
				}
			}
		}
		return true
	})
	return key
}

func assertTranscriptStorageImportPlacement(t *testing.T, fn *ast.FuncDecl) {
	t.Helper()
	var enable, pending, read []*ast.CallExpr
	var unreplayedBodies []*ast.BlockStmt
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.CallExpr:
			switch called := n.Fun.(type) {
			case *ast.Ident:
				switch called.Name {
				case "enableTranscriptStorageUsageBulkTx":
					enable = append(enable, n)
				case "markImportedTranscriptStorageUsagePendingTx":
					pending = append(pending, n)
				}
			case *ast.SelectorExpr:
				pkg, ok := called.X.(*ast.Ident)
				if ok && pkg.Name == "export" && called.Sel.Name == "Read" {
					read = append(read, n)
				}
			}
		case *ast.IfStmt:
			negation, ok := n.Cond.(*ast.UnaryExpr)
			if !ok || negation.Op != token.NOT {
				break
			}
			field, ok := negation.X.(*ast.SelectorExpr)
			if !ok || field.Sel.Name != "AlreadyImported" {
				break
			}
			variable, ok := field.X.(*ast.Ident)
			if ok && variable.Name == "disposition" {
				unreplayedBodies = append(unreplayedBodies, n.Body)
			}
		}
		return true
	})
	if len(enable) != 1 || len(pending) != 1 || len(read) != 1 {
		t.Fatalf("import calls: bulk=%d pending=%d export.Read=%d; want one each",
			len(enable), len(pending), len(read))
	}
	if enable[0].End() >= read[0].Pos() {
		t.Error("bulk setting must precede export.Read")
	}
	if pending[0].Pos() <= read[0].End() {
		t.Error("counter pending must follow the complete export.Read call")
	}
	for _, body := range unreplayedBodies {
		if body.Pos() < pending[0].Pos() && pending[0].End() < body.End() {
			return
		}
	}
	t.Error("counter pending must be inside if !disposition.AlreadyImported")
}

// Strip ordinary full-line comments, retaining Goose directives so structural
// checks cannot mistake documentation for executable SQL.
func transcriptStorageMigrationContractText(s string) string {
	var lines []string
	for line := range strings.SplitSeq(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") && !strings.HasPrefix(strings.ToLower(trimmed), "-- +goose") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.ToLower(strings.Join(lines, "\n"))
}

func TestTranscriptStorageUsageSchema101MigrationContract(t *testing.T) {
	raw, err := migrationsFS.ReadFile("migrations/0101_add_transcript_storage_usage.sql")
	if err != nil {
		t.Fatal(err)
	}
	upRaw, downRaw, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok {
		t.Fatal("migration has no Down directive")
	}
	up := transcriptStorageMigrationContractText(upRaw)
	down := transcriptStorageMigrationContractText(downRaw)
	first := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(up), "-- +goose up"))
	settings, afterSettings, ok := strings.Cut(first, ";")
	if !ok || !strings.HasPrefix(settings, "select set_config(") {
		t.Fatal("first Up statement must be the settings SELECT")
	}
	for _, setting := range []string{
		"set_config('statement_timeout', '120s', true)",
		"set_config('lock_timeout', '2s', true)",
		"set_config('idle_in_transaction_session_timeout', '5s', true)",
	} {
		if !strings.Contains(settings, setting) {
			t.Errorf("first SELECT missing %s", setting)
		}
	}
	const connectionCheckSetting = "set_config('client_connection_check_interval', '1s', true)"
	connectionCheckBlock := regexp.MustCompile(`(?s)^\s*-- \+goose statementbegin\s+do \$\$\s+begin\s+perform ` + regexp.QuoteMeta(connectionCheckSetting) + `;\s+exception when invalid_parameter_value then\s+null;\s+end\s+\$\$;\s+-- \+goose statementend`)
	if strings.Count(up, connectionCheckSetting) != 1 || !connectionCheckBlock.MatchString(afterSettings) {
		t.Error("settings SELECT must be followed by a Goose-wrapped DO that attempts client_connection_check_interval once and catches only invalid_parameter_value")
	}
	for _, banned := range []string{"lock table accounts", "references", "nowait", "no transaction", "md5(", "pg_settings", "max_val"} {
		if strings.Contains(up, banned) {
			t.Errorf("Up contains prohibited token %q", banned)
		}
	}
	const lock = "lock table transcript_entries in share row exclusive mode"
	if strings.Count(up, lock) != 1 {
		t.Fatal("Up must contain exactly one transcript_entries strong lock")
	}
	doBlocks := regexp.MustCompile(`(?s)-- \+goose statementbegin\s+do \$\$.*?-- \+goose statementend`).FindAllString(up, -1)
	if len(doBlocks) != 2 || !strings.Contains(doBlocks[1], lock) {
		t.Fatal("strong lock must be inside the second DO block, after the connection-check attempt")
	}
	do := doBlocks[1]
	firstCreate := regexp.MustCompile(`(?m)^\s*create\s`).FindStringIndex(up)
	if firstCreate == nil || strings.Index(up, do)+len(do) > firstCreate[0] {
		t.Fatal("lock loop must precede every CREATE")
	}
	for _, required := range []string{
		"lock_timeout", "deadlock_timeout", "2000), 4000)",
		"current_setting('witself.migration_0101_lock_attempts', true), '')::int, 20)",
		"current_setting('witself.migration_0101_long_holder_ms', true), '')::int, 10000)",
		"backend_type", "xact_start", "clock_timestamp() + interval '90 seconds'",
		"migration 0101: transcript_entries stayed busy for %s lock attempts",
	} {
		if !strings.Contains(do, required) {
			t.Errorf("DO block missing %q", required)
		}
	}
	loop := strings.Index(do, "\n    loop")
	refresh := strings.Index(do, "perform pg_stat_clear_snapshot();")
	probe := strings.Index(do, "if exists (")
	if loop < 0 || refresh <= loop || probe <= refresh {
		t.Error("each loop must refresh pg_stat_activity immediately before its EXISTS probe")
	}
	for _, suffix := range []string{"insert", "delete", "update"} {
		pattern := `(?s)-- \+goose statementbegin\s+create function witself_transcript_storage_usage_` + suffix + `\(\).*?-- \+goose statementend`
		body := regexp.MustCompile(pattern).FindString(up)
		if body == "" {
			t.Errorf("missing %s trigger function body", suffix)
		} else if strings.Contains(body, "greatest(") {
			t.Errorf("%s trigger clamps its signed delta", suffix)
		}
	}
	if !regexp.MustCompile(`reconcile_epoch\s+bigint\s+not null\s+default nextval\('transcript_storage_usage_epoch_seq'\)`).MatchString(up) {
		t.Error("row incarnations need a fresh default epoch")
	}
	if strings.Count(up, "from transcript_entries") != 1 ||
		!strings.Contains(up, "where exists (select 1 from transcript_entries e where e.account_id = a.id)") {
		t.Error("only the T0 EXISTS may read transcript_entries in Up")
	}
	if !strings.HasPrefix(strings.TrimSpace(down), "lock table transcript_entries in access exclusive mode nowait;") ||
		strings.Contains(down, "lock table accounts") || !strings.Contains(down, "drop table transcript_storage_usage;") {
		t.Error("Down must lock only transcript_entries first and remove the counter")
	}
}
