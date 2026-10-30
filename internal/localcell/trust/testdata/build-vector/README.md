# Deterministic TEST-ONLY component dist

`main.go` is the archive recipe for `../vectors/positive-built-dist.json` and
its adjacent signature envelope. It reads the frozen `positive-base.json` only
as unsigned metadata and a platform template. The golden document is never an
input to the recipe or builder. `component-manifest build` computes the archive
sizes, archive SHA-256 values, inventory sizes and inventory SHA-256 values from
the generated archive bytes.

The recipe fixes component order to `app`, `service_host`, `server`, `worker`,
`postgresql`. For each role it writes `<role>.tar.gz` containing exactly one
regular `bin/<role>` member with the four-byte body `data` and mode `0755`.
The tar format is USTAR, uid and gid are zero, uname and gname are empty, and
mtime is the Unix epoch. There are no directory, link, PAX or GNU entries;
access/change times and device fields are zero. Each archive ends with the
standard tar padding and two zero blocks. Gzip uses stored DEFLATE blocks
(`gzip.NoCompression`), zero MTIME, OS 255, no name/comment/extra fields and the
normal CRC/size trailer. File-system timestamps and the current username are
never consulted. The stored golden was generated using Go 1.27.3; the tests
require exact bytes on the executing toolchain rather than silently updating it.

From the repository root, reproduce and compare without printing documents,
identifiers, signatures or key material:

```sh
vector_scratch=$(mktemp -d "$PWD/.component-vector.XXXXXX")
go run ./internal/localcell/trust/testdata/build-vector \
  --metadata internal/localcell/trust/testdata/vectors/positive-base.json \
  --out "$vector_scratch/dist"
go run ./tools/component-manifest build \
  --dist "$vector_scratch/dist" --tag v1.2.3 --sequence 1 --channel stable \
  --platform "linux/amd64=$vector_scratch/dist/platform.json" \
  --out "$vector_scratch/component-set.json"
cmp "$vector_scratch/component-set.json" \
  internal/localcell/trust/testdata/vectors/positive-built-dist.json
go run ./tools/component-manifest sign \
  --key internal/localcell/trust/testdata/keys/TEST-ONLY-release-1.private.pem \
  --kind component_set --in "$vector_scratch/component-set.json" \
  --out "$vector_scratch/component-set.json.sig.json"
cmp "$vector_scratch/component-set.json.sig.json" \
  internal/localcell/trust/testdata/vectors/positive-built-dist.json.sig.json
rm -rf "$vector_scratch"
```

The signing key is a committed public TEST-ONLY fixture. The release/source,
provenance, compatibility and ownership fields remain fixture metadata; this
recipe supplies neither real release artifacts nor production trust authority.
Earlier positive vectors intentionally retain their synthetic digests for
isolated schema tests.

`TestBuildMatchesCommittedVector` runs the recipe twice, requires exact document
and envelope equality on both builds, and verifies each result through the
fixture delegation. `TestPositiveVectorSignatureRegeneration` invokes the real
build CLI before comparing the document and regenerating its envelope. The
index also enrolls the new pair in frozen verification, profile-reference checks
and the OpenSSL oracle. Explicit full corpus regeneration includes this recipe;
ordinary tests only write temporary output.
