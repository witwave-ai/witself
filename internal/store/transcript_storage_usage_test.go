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
		ast.Inspect(file, func(node ast.Node) bool {
			if fn, ok := node.(*ast.FuncDecl); ok && fn.Name.Name == "importAccountWithTracker" {
				if importFunction != nil {
					t.Error("multiple importAccountWithTracker declarations")
				}
				importFunction = fn
			}
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatalf("decode string at %s: %v", fset.Position(literal.Pos()), err)
			}
			if !strings.Contains(value, "transcript_storage_usage") &&
				!strings.Contains(value, "witself_transcript_entry_storage_bytes") {
				return true
			}
			if path != "transcript_storage_usage.go" && literal != archiveKey {
				t.Errorf("counter SQL or name outside its helper file at %s", fset.Position(literal.Pos()))
			}
			return true
		})
	}
	if archiveKeys != 1 {
		t.Fatalf("archive exclusion keys = %d, want exactly one", archiveKeys)
	}
	if importFunction == nil {
		t.Fatal("importAccountWithTracker not found")
	}
	assertTranscriptStorageImportPlacement(t, importFunction)
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
	var enable, recompute, read []*ast.CallExpr
	var unreplayedBodies []*ast.BlockStmt
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.CallExpr:
			switch called := n.Fun.(type) {
			case *ast.Ident:
				switch called.Name {
				case "enableTranscriptStorageUsageBulkTx":
					enable = append(enable, n)
				case "recomputeTranscriptStorageUsageTx":
					recompute = append(recompute, n)
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
	if len(enable) != 1 || len(recompute) != 1 || len(read) != 1 {
		t.Fatalf("import calls: bulk=%d recompute=%d export.Read=%d; want one each",
			len(enable), len(recompute), len(read))
	}
	if enable[0].End() >= read[0].Pos() {
		t.Error("bulk setting must precede export.Read")
	}
	if recompute[0].Pos() <= read[0].End() {
		t.Error("counter recompute must follow the complete export.Read call")
	}
	for _, body := range unreplayedBodies {
		if body.Pos() < recompute[0].Pos() && recompute[0].End() < body.End() {
			return
		}
	}
	t.Error("counter recompute must be inside if !disposition.AlreadyImported")
}

func TestTranscriptStorageUsageSchema101MigrationContract(t *testing.T) {
	raw, err := migrationsFS.ReadFile("migrations/0101_add_transcript_storage_usage.sql")
	if err != nil {
		t.Fatal(err)
	}
	migration := string(raw)
	const fences = "LOCK TABLE accounts IN EXCLUSIVE MODE NOWAIT;\n" +
		"LOCK TABLE transcript_entries IN SHARE ROW EXCLUSIVE MODE NOWAIT;"
	fenceIndex := strings.Index(migration, fences)
	if fenceIndex < 0 || strings.Count(migration, fences) != 1 {
		t.Fatal("migration 101 must have exactly one adjacent account-first NOWAIT fence pair")
	}
	firstCreate := regexp.MustCompile(`(?im)^\s*CREATE\s`).FindStringIndex(migration)
	if firstCreate == nil || fenceIndex+len(fences) > firstCreate[0] {
		t.Error("migration 101 fences must precede the first CREATE")
	}
	if strings.Contains(strings.ToLower(migration), "md5(") {
		t.Error("migration 101 must use a FIPS-safe shard hash")
	}
	if strings.Contains(strings.ToUpper(migration), "NO TRANSACTION") {
		t.Error("migration 101 must be transactional")
	}
	_, down, ok := strings.Cut(migration, "-- +goose Down")
	if !ok || !strings.Contains(down, "DROP TABLE transcript_storage_usage") {
		t.Error("migration 101 Down must drop transcript_storage_usage")
	}
	const downFences = "LOCK TABLE accounts IN ACCESS EXCLUSIVE MODE NOWAIT;\n" +
		"LOCK TABLE transcript_entries IN ACCESS EXCLUSIVE MODE NOWAIT;"
	if !strings.HasPrefix(strings.TrimSpace(down), downFences) || strings.Count(down, downFences) != 1 {
		t.Error("migration 101 Down must begin with adjacent account-first ACCESS EXCLUSIVE NOWAIT fences")
	}
}
