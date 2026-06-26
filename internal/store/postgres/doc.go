// Package postgres is the production relational storage adapter, including the
// pgvector integration. Identity records and their embedding vectors live in
// Postgres; migrations run through `witself-server migrate` (Goose, advisory
// lock).
package postgres
