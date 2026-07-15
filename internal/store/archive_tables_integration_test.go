package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	archiveexport "github.com/witwave-ai/witself/internal/export"
)

// TestImportAccountRejectsStructurallyValidIncompleteManifests is opt-in
// because it exercises the actual transaction boundary against PostgreSQL.
// Each fixture is valid according to the generic archive reader, including
// checksums for every empty stream, but is not a complete account archive for
// the schema it claims.
func TestImportAccountRejectsStructurallyValidIncompleteManifests(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	current := canonicalArchiveTableNamesForSchema(SchemaVersion())
	tests := []struct {
		name   string
		tables []string
		want   string
	}{
		{
			name:   "omitted empty narrative memory stream",
			tables: withoutArchiveTable(current, "memory_evidence"),
			want:   "missing memory_evidence",
		},
		{
			name:   "added unknown empty stream",
			tables: append(current, "shadow_memories"),
			want:   "unexpected shadow_memories",
		},
		{
			name:   "duplicated empty canonical stream",
			tables: append(current, "memories"),
			want:   `table "memories" more than once`,
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			accountID := fmt.Sprintf("acc_manifest_guard_%d_%d", time.Now().UnixNano(), i)
			archive := emptyTableArchive(t, accountID, SchemaVersion(), tc.tables)

			if _, err := archiveexport.Read(ctx, bytes.NewReader(archive), archiveexport.ImportOptions{
				CurrentSchema: SchemaVersion(),
			}); err != nil {
				t.Fatalf("fixture is not a structurally valid archive: %v", err)
			}

			_, err := st.ImportAccount(ctx, accountID, bytes.NewReader(archive))
			if err == nil || !errors.Is(err, ErrArchiveContent) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ImportAccount error = %v, want ErrArchiveContent containing %q", err, tc.want)
			}

			var landed bool
			if err := st.pool.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM accounts WHERE id = $1)`, accountID,
			).Scan(&landed); err != nil {
				t.Fatal(err)
			}
			if landed {
				t.Fatal("rejected archive left an account row behind")
			}
		})
	}
}

type emptyArchiveTableSource string

func (s emptyArchiveTableSource) Table() string { return string(s) }

func (s emptyArchiveTableSource) Next(context.Context) ([]byte, error) { return nil, nil }

func emptyTableArchive(t *testing.T, accountID string, schema int, tables []string) []byte {
	t.Helper()
	sources := make([]archiveexport.RowSource, 0, len(tables))
	for _, table := range tables {
		sources = append(sources, emptyArchiveTableSource(table))
	}
	var archive bytes.Buffer
	if err := archiveexport.Write(context.Background(), &archive, archiveexport.Manifest{
		SchemaVersion: schema,
		AccountID:     accountID,
		Status:        "suspended",
	}, sources); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}
