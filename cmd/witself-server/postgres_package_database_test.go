package main

import (
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/testenv"
)

// This common test name provides a small cross-package isolation receipt. The
// environment value is used only to derive the expected name, never to connect.
func TestPostgresPackageDatabaseIdentity(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	base, err := pgx.ParseConfig(os.Getenv("WITSELF_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal("package database base configuration unavailable")
	}
	want := testenv.PostgresDatabaseName(base.Database, "github.com/witwave-ai/witself/cmd/witself-server")
	connection, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal("package database identity connection unavailable")
	}
	defer func() {
		if connection.Close(t.Context()) != nil {
			t.Error("package database identity connection close failed")
		}
	}()
	var database string
	if connection.QueryRow(t.Context(), "SELECT current_database()").Scan(&database) != nil {
		t.Fatal("package database identity query failed")
	}
	if database == base.Database {
		t.Skip("package database isolation proof requires provisioning")
	}
	if database != want {
		t.Fatal("package database identity differs from its stable derivation")
	}
	t.Logf("package database=%s", database)
}
