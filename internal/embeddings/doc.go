// Package embeddings is the open-plane embedding-provider abstraction. It
// carries the voyage (default), openai, and local-dev providers behind a
// capability boundary and reports active provider, model, and vector
// dimensionality. Sealed-plane carve-out: secret values and TOTP seeds are
// never embedded.
package embeddings
