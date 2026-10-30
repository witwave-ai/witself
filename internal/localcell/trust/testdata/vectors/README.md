# Frozen component trust v1 vectors

This corpus contains 75 cases. index.json is format_version 1. Each case names exact document/envelope bytes, expected_reason, positive, fixture signers, explicit roots, ordered setup records, optional clock/target/rollback options, and negative_signature for the independent OpenSSL failure subset. Setup records are authenticated before the case. The untrusted-clock case sets its clock only after trusted delegation setup.

Regeneration is explicit: go test ./internal/localcell/trust -run '^TestFrozenVectors$' -update-trust-vectors. It uses committed public TEST-ONLY private fixtures; ordinary tests do not create keys or rewrite vectors.

The positive-built-dist document and envelope use the [deterministic dist recipe](../build-vector/README.md). Its document is regenerated through the real component-manifest build command. Earlier synthetic vectors remain schema fixtures with their original frozen bytes.
