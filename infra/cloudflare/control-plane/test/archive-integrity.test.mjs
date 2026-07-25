import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import test from "node:test";
import { gzipSync } from "node:zlib";

import {
  ArchiveIntegrityError,
  ArchiveReadError,
  commitVerifiedArchive,
  newArchiveObjectKey,
  retireSourceRouteForExistingArchive,
  validateAccountArchive,
  validateCommittedAccountArchive,
  verifyCommittedAccountArchive,
} from "../src/archive-integrity.mjs";

const BLOCK_BYTES = 512;
const ACCOUNT_ID = "acct_archive_integrity";

function writeString(buffer, offset, length, value) {
  const bytes = Buffer.from(value, "utf8");
  assert.ok(bytes.length <= length, `${value} does not fit in tar field`);
  bytes.copy(buffer, offset);
}

function writeOctal(buffer, offset, length, value) {
  writeString(
    buffer,
    offset,
    length,
    `${value.toString(8).padStart(length - 1, "0")}\0`,
  );
}

function tarHeader(name, size) {
  const header = Buffer.alloc(BLOCK_BYTES);
  writeString(header, 0, 100, name);
  writeOctal(header, 100, 8, 0o600);
  writeOctal(header, 108, 8, 0);
  writeOctal(header, 116, 8, 0);
  writeOctal(header, 124, 12, size);
  writeOctal(header, 136, 12, 0);
  header.fill(0x20, 148, 156);
  header[156] = "0".charCodeAt(0);
  writeString(header, 257, 6, "ustar\0");
  writeString(header, 263, 2, "00");

  let checksum = 0;
  for (const byte of header) checksum += byte;
  writeString(
    header,
    148,
    8,
    `${checksum.toString(8).padStart(6, "0")}\0 `,
  );
  return header;
}

function tarEntry(name, data) {
  const body = Buffer.from(data);
  const padding = Buffer.alloc(
    (BLOCK_BYTES - (body.length % BLOCK_BYTES)) % BLOCK_BYTES,
  );
  return Buffer.concat([tarHeader(name, body.length), body, padding]);
}

function makeArchive({
  accountID = ACCOUNT_ID,
  evacuationID,
  table = "accounts",
  chunk = Buffer.from(`{"id":"${ACCOUNT_ID}"}\n`),
  includeChunk = true,
  includeChecksums = true,
} = {}) {
  const chunkName = `${table}/000001.ndjson`;
  let rows = 0;
  for (const byte of chunk) {
    if (byte === 0x0a) rows++;
  }
  const manifestRecord = {
    format_version: 1,
    schema_version: 64,
    server_version: "test",
    compression: "gzip",
    account_id: accountID,
    status: "suspended",
    exported_at: "2026-07-25T00:00:00Z",
    tables: [table],
  };
  if (evacuationID !== undefined) {
    manifestRecord.evacuation_id = evacuationID;
  }
  const manifest = Buffer.from(JSON.stringify(manifestRecord));
  const entries = [tarEntry("manifest.json", manifest)];
  if (includeChunk) {
    entries.push(tarEntry(chunkName, chunk));
  }
  if (includeChecksums) {
    entries.push(tarEntry("checksums.json", Buffer.from(JSON.stringify({
      chunks: includeChunk
        ? [{
            name: chunkName,
            sha256: createHash("sha256").update(chunk).digest("hex"),
            bytes: chunk.length,
            rows,
          }]
        : [],
      table_rows: { [table]: includeChunk ? rows : 0 },
    }))));
  }
  entries.push(Buffer.alloc(BLOCK_BYTES * 2));
  return Buffer.concat(entries);
}

function makeControlOnlyArchive(manifestJSON, checksumsJSON) {
  return Buffer.concat([
    tarEntry("manifest.json", Buffer.from(manifestJSON)),
    tarEntry("checksums.json", Buffer.from(checksumsJSON)),
    Buffer.alloc(BLOCK_BYTES * 2),
  ]);
}

function bodyStream(bytes, chunkSize = 73) {
  const source = Buffer.from(bytes);
  return new ReadableStream({
    start(controller) {
      for (let offset = 0; offset < source.length; offset += chunkSize) {
        controller.enqueue(
          new Uint8Array(
            source.buffer,
            source.byteOffset + offset,
            Math.min(chunkSize, source.length - offset),
          ),
        );
      }
      // This is an ordinary clean EOF. A truncated payload must therefore be
      // caught by the archive contract, not by a network read exception.
      controller.close();
    },
  });
}

test("validates a complete archive with bounded streaming reads", async () => {
  const compressed = gzipSync(makeArchive());
  const result = await validateAccountArchive(
    bodyStream(compressed, 37),
    ACCOUNT_ID,
  );
  assert.equal(result.manifest.account_id, ACCOUNT_ID);
  assert.equal(result.entries, 3);
  assert.equal(result.chunks, 1);
});

test("keeps an ambiguous archive body read failure retryable", async () => {
  const failure = new Error("temporary R2 body stream timeout");
  const stream = new ReadableStream({
    pull(controller) {
      controller.error(failure);
    },
  });
  await assert.rejects(
    validateAccountArchive(stream, ACCOUNT_ID),
    (error) => {
      assert.ok(error instanceof ArchiveReadError);
      assert.equal(error instanceof ArchiveIntegrityError, false);
      assert.equal(error.cause, failure);
      assert.match(error.message, /temporary R2 body stream timeout/);
      return true;
    },
  );
});

test("rejects an archive whose manifest omits the canonical accounts table", async () => {
  const compressed = gzipSync(makeArchive({ table: "realms" }));
  await assert.rejects(
    validateAccountArchive(bodyStream(compressed), ACCOUNT_ID),
    /manifest is missing canonical accounts table/,
  );
});

test("requires exactly one canonical account row", async () => {
  const zeroRows = gzipSync(makeArchive({ includeChunk: false }));
  await assert.rejects(
    validateAccountArchive(bodyStream(zeroRows), ACCOUNT_ID),
    /checksums\.json table_rows\.accounts is 0, want 1/,
  );

  const twoRows = gzipSync(makeArchive({
    chunk: Buffer.from(
      `{"id":"${ACCOUNT_ID}"}\n{"id":"acct_unexpected"}\n`,
    ),
  }));
  await assert.rejects(
    validateAccountArchive(bodyStream(twoRows), ACCOUNT_ID),
    /checksums\.json table_rows\.accounts is 2, want 1/,
  );
});

test("rejects an archive whose manifest belongs to another account", async () => {
  const compressed = gzipSync(makeArchive({ accountID: "acct_other" }));
  await assert.rejects(
    validateAccountArchive(bodyStream(compressed), ACCOUNT_ID),
    /manifest account_id "acct_other" does not match/,
  );
});

test("pins a newly evacuated archive to the exact lifecycle epoch", async () => {
  const evacuationID = "11111111-2222-4333-8444-555555555555";
  const compressed = gzipSync(makeArchive({ evacuationID }));
  await validateAccountArchive(
    bodyStream(compressed),
    ACCOUNT_ID,
    { evacuationID },
  );
  await assert.rejects(
    validateAccountArchive(
      bodyStream(compressed),
      ACCOUNT_ID,
      { evacuationID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" },
    ),
    /evacuation_id does not match/,
  );
  await assert.rejects(
    validateAccountArchive(
      bodyStream(gzipSync(makeArchive())),
      ACCOUNT_ID,
      { evacuationID },
    ),
    /evacuation_id does not match/,
  );
  await validateAccountArchive(
    bodyStream(gzipSync(makeArchive())),
    ACCOUNT_ID,
    { evacuationID, allowLegacyEvacuationID: true },
  );

  const currentWithoutEpoch = makeArchive();
  const schemaMarker = Buffer.from('"schema_version":64');
  const currentSchemaMarker = Buffer.from('"schema_version":70');
  const schemaOffset = currentWithoutEpoch.indexOf(schemaMarker);
  assert.ok(schemaOffset >= 0);
  currentSchemaMarker.copy(currentWithoutEpoch, schemaOffset);
  await assert.rejects(
    validateAccountArchive(
      bodyStream(gzipSync(currentWithoutEpoch)),
      ACCOUNT_ID,
      { evacuationID, allowLegacyEvacuationID: true },
    ),
    /evacuation_id does not match/,
  );
});

test("validates SHA-256 across padding and multi-block boundaries", async () => {
  for (const size of [55, 56, 63, 64, 65, 64 * 1024 + 17]) {
    const chunk = Buffer.alloc(size, 0x61);
    chunk[chunk.length - 1] = 0x0a;
    const compressed = gzipSync(makeArchive({ chunk }));
    const result = await validateAccountArchive(
      bodyStream(compressed, 29),
      ACCOUNT_ID,
    );
    assert.equal(result.chunks, 1, `chunk size ${size}`);
  }
});

test("uses the Workers-compatible native DigestStream when available", async () => {
  const originalDescriptor = Object.getOwnPropertyDescriptor(
    globalThis.crypto,
    "DigestStream",
  );
  let writes = 0;
  class TestDigestStream extends WritableStream {
    constructor(algorithm) {
      assert.equal(algorithm, "SHA-256");
      const hash = createHash("sha256");
      let resolveDigest;
      let rejectDigest;
      const digest = new Promise((resolve, reject) => {
        resolveDigest = resolve;
        rejectDigest = reject;
      });
      super({
        write(chunk) {
          writes++;
          hash.update(chunk);
        },
        close() {
          const value = hash.digest();
          resolveDigest(
            value.buffer.slice(
              value.byteOffset,
              value.byteOffset + value.byteLength,
            ),
          );
        },
        abort(error) {
          rejectDigest(error);
        },
      });
      this.digest = digest;
    }
  }
  Object.defineProperty(globalThis.crypto, "DigestStream", {
    configurable: true,
    value: TestDigestStream,
  });
  try {
    await validateAccountArchive(
      bodyStream(gzipSync(makeArchive()), 31),
      ACCOUNT_ID,
    );
    assert.ok(writes > 0);
  } finally {
    if (originalDescriptor) {
      Object.defineProperty(
        globalThis.crypto,
        "DigestStream",
        originalDescriptor,
      );
    } else {
      delete globalThis.crypto.DigestStream;
    }
  }
});

test("rejects a complete gzip when tar cleanly ends at 1 MiB mid-export", async () => {
  const manifest = Buffer.from(JSON.stringify({
    format_version: 1,
    schema_version: 64,
    server_version: "test",
    compression: "gzip",
    account_id: ACCOUNT_ID,
    status: "suspended",
    exported_at: "2026-07-25T00:00:00Z",
    tables: ["accounts", "transcript_conversations"],
  }));
  const prefix = Buffer.concat([
    tarEntry("manifest.json", manifest),
    tarHeader("transcript_conversations/000001.ndjson", 2 * 1024 * 1024),
  ]);
  assert.ok(prefix.length < 1024 * 1024);
  const cleanEOFAtOneMiB = Buffer.concat([
    prefix,
    Buffer.alloc(1024 * 1024 - prefix.length, 0x61),
  ]);
  assert.equal(cleanEOFAtOneMiB.length, 1024 * 1024);

  // gzip itself is complete: only its tar payload is truncated. This pins the
  // production failure where fetch/body iteration returned done=true instead
  // of throwing and multipart completion incorrectly treated that as proof.
  const compressed = gzipSync(cleanEOFAtOneMiB);
  await assert.rejects(
    validateAccountArchive(bodyStream(compressed), ACCOUNT_ID),
    (error) => {
      assert.ok(error instanceof ArchiveIntegrityError);
      assert.match(error.message, /truncated tar entry/);
      return true;
    },
  );
});

test("rejects a source stream that cleanly ends before the gzip trailer", async () => {
  const complete = gzipSync(makeArchive());
  const truncated = complete.subarray(0, complete.length - 8);
  await assert.rejects(
    validateAccountArchive(bodyStream(truncated), ACCOUNT_ID),
    (error) => {
      assert.ok(error instanceof ArchiveIntegrityError);
      assert.match(error.message, /invalid gzip|unexpected end|terminated/i);
      return true;
    },
  );
});

test("rejects a structurally complete archive without final checksums", async () => {
  const compressed = gzipSync(makeArchive({ includeChecksums: false }));
  await assert.rejects(
    validateAccountArchive(bodyStream(compressed), ACCOUNT_ID),
    (error) => {
      assert.ok(error instanceof ArchiveIntegrityError);
      assert.match(error.message, /checksums\.json is missing/);
      return true;
    },
  );
});

test("rejects chunk content that does not match the checksum trailer", async () => {
  const tar = makeArchive();
  const marker = Buffer.from(`{"id":"${ACCOUNT_ID}"}\n`);
  const offset = tar.indexOf(marker);
  assert.ok(offset >= 0);
  const tampered = Buffer.from(tar);
  tampered[offset + '{"id":"'.length + 1] = "R".charCodeAt(0);

  await assert.rejects(
    validateAccountArchive(bodyStream(gzipSync(tampered)), ACCOUNT_ID),
    (error) => {
      assert.ok(error instanceof ArchiveIntegrityError);
      assert.match(error.message, /does not match checksums\.json/);
      return true;
    },
  );
});

test("rejects inconsistent checksum trailer row totals", async () => {
  const tar = makeArchive();
  const expected = Buffer.from('"table_rows":{"accounts":1}');
  const replacement = Buffer.from('"table_rows":{"accounts":9}');
  const offset = tar.indexOf(expected);
  assert.ok(offset >= 0);
  const inconsistent = Buffer.from(tar);
  replacement.copy(inconsistent, offset);
  await assert.rejects(
    validateAccountArchive(bodyStream(gzipSync(inconsistent)), ACCOUNT_ID),
    /chunk rows total 1, checksums\.json says 9/,
  );
});

test("rejects a chunk row count that disagrees with its checksum record", async () => {
  const tar = makeArchive();
  const expected = Buffer.from('"rows":1');
  const replacement = Buffer.from('"rows":9');
  const offset = tar.indexOf(expected);
  assert.ok(offset >= 0);
  const inconsistent = Buffer.from(tar);
  replacement.copy(inconsistent, offset);
  await assert.rejects(
    validateAccountArchive(bodyStream(gzipSync(inconsistent)), ACCOUNT_ID),
    /does not match checksums\.json/,
  );
});

test("rejects ambiguous control JSON accepted by JSON.parse", async () => {
  const duplicateManifest = makeControlOnlyArchive(
    `{
      "format_version": 999,
      "format_version": 1,
      "schema_version": 64,
      "server_version": "test",
      "compression": "gzip",
      "account_id": "${ACCOUNT_ID}",
      "status": "suspended",
      "tables": []
    }`,
    `{"chunks":[],"table_rows":{}}`,
  );
  await assert.rejects(
    validateAccountArchive(bodyStream(gzipSync(duplicateManifest)), ACCOUNT_ID),
    /duplicate object member "format_version"/,
  );

  const unpairedSurrogateManifest = makeControlOnlyArchive(
    `{
      "format_version": 1,
      "schema_version": 64,
      "server_version": "\\ud800",
      "compression": "gzip",
      "account_id": "${ACCOUNT_ID}",
      "status": "suspended",
      "tables": []
    }`,
    `{"chunks":[],"table_rows":{}}`,
  );
  await assert.rejects(
    validateAccountArchive(
      bodyStream(gzipSync(unpairedSurrogateManifest)),
      ACCOUNT_ID,
    ),
    /unpaired high surrogate escape/,
  );
});

test("deletes an invalid committed R2 object before returning failure", async () => {
  const objectKey = `archives/${ACCOUNT_ID}.tar.gz`;
  const invalid = gzipSync(makeArchive({ includeChecksums: false }));
  const deleted = [];
  const bucket = {
    async get(key) {
      assert.equal(key, objectKey);
      return { body: bodyStream(invalid) };
    },
    async delete(key) {
      deleted.push(key);
    },
  };

  await assert.rejects(
    verifyCommittedAccountArchive(bucket, objectKey, ACCOUNT_ID),
    /archive integrity validation failed/,
  );
  assert.deepEqual(deleted, [objectKey]);
});

test("does not delete an invalid object referenced by existing archive state", async () => {
  const objectKey = `archives/${ACCOUNT_ID}.tar.gz`;
  const invalid = gzipSync(makeArchive({ includeChecksums: false }));
  const deleted = [];
  const bucket = {
    async get(key) {
      assert.equal(key, objectKey);
      return { body: bodyStream(invalid) };
    },
    async delete(key) {
      deleted.push(key);
    },
  };

  await assert.rejects(
    validateCommittedAccountArchive(bucket, objectKey, ACCOUNT_ID),
    /checksums\.json is missing/,
  );
  assert.deepEqual(deleted, []);
});

test("existing archive state cannot retire a route on another cell", async () => {
  const objectKey = `archives/${ACCOUNT_ID}.tar.gz`;
  const valid = gzipSync(makeArchive());
  const deleted = [];
  const directory = {
    async get(key) {
      assert.equal(key, `acct:${ACCOUNT_ID}`);
      return { cell: "new-target-cell" };
    },
    async delete(key) {
      deleted.push(key);
    },
  };
  const bucket = {
    async get(key) {
      assert.equal(key, objectKey);
      return { body: bodyStream(valid) };
    },
  };

  await assert.rejects(
    retireSourceRouteForExistingArchive({
      directory,
      bucket,
      accountID: ACCOUNT_ID,
      cellName: "old-source-cell",
      archived: {
        cell: "old-source-cell",
        object: objectKey,
      },
    }),
    /now routes to new-target-cell/,
  );
  assert.deepEqual(deleted, []);
});

test("existing archive state retires only its verified source route", async () => {
  const objectKey = `archives/${ACCOUNT_ID}.tar.gz`;
  const valid = gzipSync(makeArchive());
  const deleted = [];
  const directory = {
    async get() {
      return { cell: "source-cell" };
    },
    async delete(key) {
      deleted.push(key);
    },
  };
  const bucket = {
    async get() {
      return { body: bodyStream(valid) };
    },
  };

  await retireSourceRouteForExistingArchive({
    directory,
    bucket,
    accountID: ACCOUNT_ID,
    cellName: "source-cell",
    archived: {
      cell: "source-cell",
      object: objectKey,
    },
  });
  assert.deepEqual(deleted, [`acct:${ACCOUNT_ID}`]);
});

test("keeps a valid committed R2 object", async () => {
  const objectKey = `archives/${ACCOUNT_ID}.tar.gz`;
  const valid = gzipSync(makeArchive());
  const deleted = [];
  const bucket = {
    async get() {
      return { body: bodyStream(valid) };
    },
    async delete(key) {
      deleted.push(key);
    },
  };

  const result = await verifyCommittedAccountArchive(
    bucket,
    objectKey,
    ACCOUNT_ID,
  );
  assert.equal(result.manifest.account_id, ACCOUNT_ID);
  assert.deepEqual(deleted, []);
});

test("overlapping attempts use isolated object keys and cleanup", async () => {
  const invalidKey = newArchiveObjectKey(
    ACCOUNT_ID,
    "00000000-0000-4000-8000-000000000001",
  );
  const validKey = newArchiveObjectKey(
    ACCOUNT_ID,
    "00000000-0000-4000-8000-000000000002",
  );
  assert.notEqual(invalidKey, validKey);

  const objects = new Map([
    [invalidKey, gzipSync(makeArchive({ includeChecksums: false }))],
    [validKey, gzipSync(makeArchive())],
  ]);
  const deleted = [];
  const bucket = {
    async get(key) {
      const bytes = objects.get(key);
      return bytes ? { body: bodyStream(bytes) } : null;
    },
    async delete(key) {
      deleted.push(key);
      objects.delete(key);
    },
  };

  const [invalid, valid] = await Promise.allSettled([
    verifyCommittedAccountArchive(bucket, invalidKey, ACCOUNT_ID),
    verifyCommittedAccountArchive(bucket, validKey, ACCOUNT_ID),
  ]);
  assert.equal(invalid.status, "rejected");
  assert.equal(valid.status, "fulfilled");
  assert.deepEqual(deleted, [invalidKey]);
  assert.equal(objects.has(validKey), true);
});

test("publishes archive state only after upload and validation", async () => {
  const calls = [];
  const result = await commitVerifiedArchive({
    async upload() {
      calls.push("upload");
      return { object: "attempt-object" };
    },
    async validate(uploaded) {
      calls.push(`validate:${uploaded.object}`);
      return { chunks: 1 };
    },
    async publish(uploaded, verification) {
      calls.push(`publish:${uploaded.object}:${verification.chunks}`);
    },
  });
  assert.deepEqual(calls, [
    "upload",
    "validate:attempt-object",
    "publish:attempt-object:1",
  ]);
  assert.deepEqual(result, {
    uploaded: { object: "attempt-object" },
    verification: { chunks: 1 },
  });

  const failedCalls = [];
  await assert.rejects(
    commitVerifiedArchive({
      async upload() {
        failedCalls.push("upload");
        return { object: "invalid-object" };
      },
      async validate() {
        failedCalls.push("validate");
        throw new Error("invalid archive");
      },
      async publish() {
        failedCalls.push("publish");
      },
    }),
    /invalid archive/,
  );
  assert.deepEqual(failedCalls, ["upload", "validate"]);
});
