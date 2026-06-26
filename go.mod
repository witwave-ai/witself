module github.com/witwave-ai/witself

go 1.26

toolchain go1.26.4

// The skeleton is intentionally stdlib-only so go.mod stays clean and
// `go mod tidy` is trivial. Real dependencies (e.g. cobra for the CLI,
// pgx/pgvector for storage, KMS and embedding-provider SDKs) arrive with
// the real implementation, not with this scaffold.
