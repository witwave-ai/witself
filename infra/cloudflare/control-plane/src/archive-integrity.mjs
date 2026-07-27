const TAR_BLOCK_BYTES = 512;
const MAX_CONTROL_ENTRY_BYTES = 4 * 1024 * 1024;
const MAX_DATA_ENTRY_BYTES = 1024 * 1024 * 1024;
const MAX_ARCHIVE_ENTRIES = 100_000;
const FORMAT_VERSION = 1;

const SHA256_CONSTANTS = new Uint32Array([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5,
  0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3,
  0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc,
  0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7,
  0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13,
  0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3,
  0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5,
  0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208,
  0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
]);

export class ArchiveIntegrityError extends Error {
  constructor(message, options) {
    super(message, options);
    this.name = "ArchiveIntegrityError";
  }
}

// A failed R2/body read is not evidence that the immutable archive bytes are
// corrupt. Keep it outside ArchiveIntegrityError so callers retry instead of
// permanently quarantining an archive after an ambiguous transport failure.
export class ArchiveReadError extends Error {
  constructor(message, options) {
    super(message, options);
    this.name = "ArchiveReadError";
  }
}

export function newArchiveObjectKey(
  accountID,
  attemptID = globalThis.crypto.randomUUID(),
) {
  if (!/^[A-Za-z0-9_-]{1,128}$/.test(accountID)) {
    throw new ArchiveIntegrityError("invalid account id for archive object key");
  }
  if (!/^[0-9a-f-]{36}$/.test(attemptID)) {
    throw new ArchiveIntegrityError("invalid evacuation attempt id");
  }
  return `archives/${accountID}/${attemptID}.tar.gz`;
}

// commitVerifiedArchive is the small ordering seam for evacuation state:
// upload must finish, then validation must succeed, and only then may the
// caller publish archived:. Keeping that sequence testable prevents a future
// refactor from making an unverified R2 object authoritative.
export async function commitVerifiedArchive({ upload, validate, publish }) {
  const uploaded = await upload();
  const verification = await validate(uploaded);
  await publish(uploaded, verification);
  return { uploaded, verification };
}

// A small pull reader that retains only the current upstream chunk. Tar
// headers are read exactly, while entry bodies are consumed in the chunks
// supplied by the decompressor so large account archives never accumulate in
// Worker memory.
class StreamReader {
  constructor(stream, readError = (error) => error) {
    if (!stream || typeof stream.getReader !== "function") {
      throw new ArchiveReadError("archive body is not a readable stream");
    }
    this.reader = stream.getReader();
    this.readError = readError;
    this.chunk = null;
    this.offset = 0;
    this.ended = false;
  }

  async readAtMost(maxBytes) {
    while (!this.ended && (!this.chunk || this.offset === this.chunk.length)) {
      let next;
      try {
        next = await this.reader.read();
      } catch (error) {
        throw this.readError(error);
      }
      const { done, value } = next;
      if (done) {
        this.ended = true;
        this.chunk = null;
        return null;
      }
      const bytes = value instanceof Uint8Array
        ? value
        : new Uint8Array(value);
      if (bytes.length === 0) {
        continue;
      }
      this.chunk = bytes;
      this.offset = 0;
    }
    if (this.ended) {
      return null;
    }
    const count = Math.min(maxBytes, this.chunk.length - this.offset);
    const result = this.chunk.subarray(this.offset, this.offset + count);
    this.offset += count;
    return result;
  }

  async readExactly(byteLength, allowCleanEOF = false) {
    const result = new Uint8Array(byteLength);
    let filled = 0;
    while (filled < byteLength) {
      const next = await this.readAtMost(byteLength - filled);
      if (next === null) {
        if (allowCleanEOF && filled === 0) {
          return null;
        }
        throw new ArchiveIntegrityError(
          `truncated tar stream: wanted ${byteLength} bytes, received ${filled}`,
        );
      }
      result.set(next, filled);
      filled += next.length;
    }
    return result;
  }

  async cancel(reason) {
    try {
      await this.reader.cancel(reason);
    } catch {
      // The decompressor may already be errored or closed.
    }
  }
}

function monitorArchiveBody(stream) {
  if (!stream || typeof stream.getReader !== "function") {
    throw new ArchiveReadError("archive body is not a readable stream");
  }
  const reader = stream.getReader();
  let readFailure = null;
  const monitored = new ReadableStream({
    async pull(controller) {
      try {
        const { done, value } = await reader.read();
        if (done) {
          controller.close();
          return;
        }
        controller.enqueue(value);
      } catch (error) {
        readFailure = error;
        controller.error(error);
      }
    },
    async cancel(reason) {
      try {
        await reader.cancel(reason);
      } catch {
        // The source may already be errored or closed.
      }
    },
  });
  return {
    stream: monitored,
    readFailure: () => readFailure,
  };
}

class SHA256 {
  constructor() {
    this.state = new Uint32Array([
      0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
      0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
    ]);
    this.words = new Uint32Array(64);
    this.buffer = new Uint8Array(64);
    this.buffered = 0;
    this.bytes = 0;
    this.finished = false;
  }

  update(input) {
    if (this.finished) {
      throw new ArchiveIntegrityError("sha256 update after digest");
    }
    const bytes = input instanceof Uint8Array ? input : new Uint8Array(input);
    this.bytes += bytes.length;
    let offset = 0;

    if (this.buffered > 0) {
      const count = Math.min(64 - this.buffered, bytes.length);
      this.buffer.set(bytes.subarray(0, count), this.buffered);
      this.buffered += count;
      offset += count;
      if (this.buffered === 64) {
        this.#transform(this.buffer, 0);
        this.buffered = 0;
      }
    }

    while (offset + 64 <= bytes.length) {
      this.#transform(bytes, offset);
      offset += 64;
    }
    if (offset < bytes.length) {
      this.buffer.set(bytes.subarray(offset), 0);
      this.buffered = bytes.length - offset;
    }
    return this;
  }

  hexDigest() {
    if (this.finished) {
      throw new ArchiveIntegrityError("sha256 digest requested twice");
    }
    this.finished = true;
    const bitLength = BigInt(this.bytes) * 8n;
    this.buffer[this.buffered++] = 0x80;
    if (this.buffered > 56) {
      this.buffer.fill(0, this.buffered);
      this.#transform(this.buffer, 0);
      this.buffered = 0;
    }
    this.buffer.fill(0, this.buffered, 56);
    for (let index = 0; index < 8; index++) {
      this.buffer[63 - index] = Number(
        (bitLength >> BigInt(index * 8)) & 0xffn,
      );
    }
    this.#transform(this.buffer, 0);

    let hex = "";
    for (const word of this.state) {
      hex += word.toString(16).padStart(8, "0");
    }
    return hex;
  }

  #transform(bytes, offset) {
    const words = this.words;
    for (let index = 0; index < 16; index++) {
      const start = offset + index * 4;
      words[index] = (
        (bytes[start] << 24) |
        (bytes[start + 1] << 16) |
        (bytes[start + 2] << 8) |
        bytes[start + 3]
      ) >>> 0;
    }
    for (let index = 16; index < 64; index++) {
      const x = words[index - 15];
      const y = words[index - 2];
      const sigma0 = (
        rotateRight(x, 7) ^ rotateRight(x, 18) ^ (x >>> 3)
      ) >>> 0;
      const sigma1 = (
        rotateRight(y, 17) ^ rotateRight(y, 19) ^ (y >>> 10)
      ) >>> 0;
      words[index] = (
        words[index - 16] + sigma0 + words[index - 7] + sigma1
      ) >>> 0;
    }

    let a = this.state[0];
    let b = this.state[1];
    let c = this.state[2];
    let d = this.state[3];
    let e = this.state[4];
    let f = this.state[5];
    let g = this.state[6];
    let h = this.state[7];
    for (let index = 0; index < 64; index++) {
      const sum1 = (
        rotateRight(e, 6) ^ rotateRight(e, 11) ^ rotateRight(e, 25)
      ) >>> 0;
      const choice = ((e & f) ^ (~e & g)) >>> 0;
      const temp1 = (
        h + sum1 + choice + SHA256_CONSTANTS[index] + words[index]
      ) >>> 0;
      const sum0 = (
        rotateRight(a, 2) ^ rotateRight(a, 13) ^ rotateRight(a, 22)
      ) >>> 0;
      const majority = ((a & b) ^ (a & c) ^ (b & c)) >>> 0;
      const temp2 = (sum0 + majority) >>> 0;
      h = g;
      g = f;
      f = e;
      e = (d + temp1) >>> 0;
      d = c;
      c = b;
      b = a;
      a = (temp1 + temp2) >>> 0;
    }
    this.state[0] = (this.state[0] + a) >>> 0;
    this.state[1] = (this.state[1] + b) >>> 0;
    this.state[2] = (this.state[2] + c) >>> 0;
    this.state[3] = (this.state[3] + d) >>> 0;
    this.state[4] = (this.state[4] + e) >>> 0;
    this.state[5] = (this.state[5] + f) >>> 0;
    this.state[6] = (this.state[6] + g) >>> 0;
    this.state[7] = (this.state[7] + h) >>> 0;
  }
}

function rotateRight(value, bits) {
  return ((value >>> bits) | (value << (32 - bits))) >>> 0;
}

function hexBytes(bytes) {
  let result = "";
  for (const byte of bytes) {
    result += byte.toString(16).padStart(2, "0");
  }
  return result;
}

// Cloudflare's native DigestStream keeps hashing outside JavaScript CPU time.
// The small JS implementation remains as a standards-runtime fallback and is
// exercised against Node's crypto implementation by the test fixtures.
function createStreamingSHA256() {
  if (typeof globalThis.crypto?.DigestStream === "function") {
    const stream = new globalThis.crypto.DigestStream("SHA-256");
    const writer = stream.getWriter();
    const digest = stream.digest;
    return {
      async update(bytes) {
        await writer.write(bytes);
      },
      async hexDigest() {
        await writer.close();
        return hexBytes(new Uint8Array(await digest));
      },
      async abort(reason) {
        await writer.abort(reason).catch(() => {});
        await digest.catch(() => {});
      },
    };
  }

  const fallback = new SHA256();
  return {
    update(bytes) {
      fallback.update(bytes);
    },
    async hexDigest() {
      return fallback.hexDigest();
    },
    async abort() {},
  };
}

function allZero(bytes) {
  for (const value of bytes) {
    if (value !== 0) return false;
  }
  return true;
}

function decodeTarString(bytes) {
  let end = bytes.indexOf(0);
  if (end < 0) end = bytes.length;
  try {
    return new TextDecoder("utf-8", { fatal: true }).decode(
      bytes.subarray(0, end),
    );
  } catch (error) {
    throw new ArchiveIntegrityError("tar header contains invalid UTF-8", {
      cause: error,
    });
  }
}

function parseTarOctal(bytes, field) {
  const value = decodeTarString(bytes).trim();
  if (!/^[0-7]+$/.test(value)) {
    throw new ArchiveIntegrityError(`tar ${field} is not octal`);
  }
  const parsed = Number.parseInt(value, 8);
  if (!Number.isSafeInteger(parsed) || parsed < 0) {
    throw new ArchiveIntegrityError(`tar ${field} is out of range`);
  }
  return parsed;
}

function parseTarHeader(block) {
  const storedChecksum = parseTarOctal(block.subarray(148, 156), "checksum");
  let computedChecksum = 0;
  for (let index = 0; index < block.length; index++) {
    computedChecksum += index >= 148 && index < 156 ? 0x20 : block[index];
  }
  if (storedChecksum !== computedChecksum) {
    throw new ArchiveIntegrityError("tar header checksum mismatch");
  }

  const name = decodeTarString(block.subarray(0, 100));
  const prefix = decodeTarString(block.subarray(345, 500));
  const fullName = prefix ? `${prefix}/${name}` : name;
  if (!fullName) {
    throw new ArchiveIntegrityError("tar entry has no name");
  }
  const type = block[156];
  if (type !== 0 && type !== 0x30) {
    throw new ArchiveIntegrityError(
      `tar entry ${fullName} is not a regular file`,
    );
  }
  const size = parseTarOctal(block.subarray(124, 136), "entry size");
  if (size > MAX_DATA_ENTRY_BYTES) {
    throw new ArchiveIntegrityError(
      `tar entry ${fullName} exceeds the ${MAX_DATA_ENTRY_BYTES}-byte limit`,
    );
  }
  return { name: fullName, size };
}

async function consumeEntry(reader, entry, observer = null, collect = false) {
  if (collect && entry.size > MAX_CONTROL_ENTRY_BYTES) {
    throw new ArchiveIntegrityError(
      `${entry.name} exceeds the ${MAX_CONTROL_ENTRY_BYTES}-byte control-record limit`,
    );
  }
  const pieces = collect ? [] : null;
  let remaining = entry.size;
  while (remaining > 0) {
    const next = await reader.readAtMost(remaining);
    if (next === null) {
      throw new ArchiveIntegrityError(`truncated tar entry ${entry.name}`);
    }
    if (observer) await observer(next);
    if (collect) pieces.push(next.slice());
    remaining -= next.length;
  }

  const paddingLength =
    (TAR_BLOCK_BYTES - (entry.size % TAR_BLOCK_BYTES)) % TAR_BLOCK_BYTES;
  if (paddingLength > 0) {
    const padding = await reader.readExactly(paddingLength);
    if (!allZero(padding)) {
      throw new ArchiveIntegrityError(
        `tar entry ${entry.name} has non-zero padding`,
      );
    }
  }

  if (!collect) return null;
  const result = new Uint8Array(entry.size);
  let offset = 0;
  for (const piece of pieces) {
    result.set(piece, offset);
    offset += piece.length;
  }
  return result;
}

function parseJSONRecord(bytes, name) {
  let text;
  try {
    text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  } catch (error) {
    throw new ArchiveIntegrityError(`${name} is not valid UTF-8`, {
      cause: error,
    });
  }
  try {
    const value = JSON.parse(text);
    rejectAmbiguousJSON(text);
    if (!value || Array.isArray(value) || typeof value !== "object") {
      throw new Error("expected a JSON object");
    }
    return value;
  } catch (error) {
    throw new ArchiveIntegrityError(`${name} is not valid JSON: ${error.message}`, {
      cause: error,
    });
  }
}

// JSON.parse intentionally keeps the last duplicate object member and permits
// lone escaped UTF-16 surrogates. The canonical Go importer rejects both
// spellings before trusting archive control data, so the edge gate must not
// accept a different interpretation. JSON.parse has already proven the
// grammar valid when this scanner runs; it only tracks object scope and string
// escapes to detect those ambiguous cases.
function rejectAmbiguousJSON(text) {
  let offset = 0;

  const skipWhitespace = () => {
    while (
      offset < text.length &&
      (text[offset] === " " ||
        text[offset] === "\t" ||
        text[offset] === "\r" ||
        text[offset] === "\n")
    ) {
      offset++;
    }
  };

  const scanString = () => {
    const start = offset;
    offset++; // opening quote
    while (offset < text.length) {
      if (text[offset] === "\"") {
        offset++;
        return JSON.parse(text.slice(start, offset));
      }
      if (text[offset] !== "\\") {
        offset++;
        continue;
      }
      const escapeStart = offset;
      const escapeType = text[offset + 1];
      if (escapeType !== "u") {
        offset += 2;
        continue;
      }
      const first = Number.parseInt(text.slice(offset + 2, offset + 6), 16);
      if (first >= 0xd800 && first <= 0xdbff) {
        const secondEscape = text.slice(offset + 6, offset + 8);
        const second = Number.parseInt(text.slice(offset + 8, offset + 12), 16);
        if (
          secondEscape !== "\\u" ||
          !Number.isInteger(second) ||
          second < 0xdc00 ||
          second > 0xdfff
        ) {
          throw new Error(
            `unpaired high surrogate escape at byte ${escapeStart}`,
          );
        }
        offset += 12;
        continue;
      }
      if (first >= 0xdc00 && first <= 0xdfff) {
        throw new Error(`unpaired low surrogate escape at byte ${escapeStart}`);
      }
      offset += 6;
    }
    throw new Error("unterminated JSON string");
  };

  const scanValue = () => {
    skipWhitespace();
    if (text[offset] === "{") {
      offset++;
      skipWhitespace();
      const keys = new Set();
      if (text[offset] === "}") {
        offset++;
        return;
      }
      while (offset < text.length) {
        skipWhitespace();
        const key = scanString();
        if (keys.has(key)) {
          throw new Error(`duplicate object member ${JSON.stringify(key)}`);
        }
        keys.add(key);
        skipWhitespace();
        offset++; // colon; JSON.parse already validated it
        scanValue();
        skipWhitespace();
        if (text[offset] === "}") {
          offset++;
          return;
        }
        offset++; // comma
      }
      return;
    }
    if (text[offset] === "[") {
      offset++;
      skipWhitespace();
      if (text[offset] === "]") {
        offset++;
        return;
      }
      while (offset < text.length) {
        scanValue();
        skipWhitespace();
        if (text[offset] === "]") {
          offset++;
          return;
        }
        offset++; // comma
      }
      return;
    }
    if (text[offset] === "\"") {
      scanString();
      return;
    }
    while (
      offset < text.length &&
      ![" ", "\t", "\r", "\n", ",", "]", "}"].includes(text[offset])
    ) {
      offset++;
    }
  };

  scanValue();
  skipWhitespace();
  if (offset !== text.length) {
    throw new Error("multiple JSON values");
  }
}

function validateManifest(manifest, expectedAccountID, options = {}) {
  if (manifest.format_version !== FORMAT_VERSION) {
    throw new ArchiveIntegrityError(
      `manifest format_version is ${manifest.format_version}, want ${FORMAT_VERSION}`,
    );
  }
  if (manifest.account_id !== expectedAccountID) {
    throw new ArchiveIntegrityError(
      `manifest account_id ${JSON.stringify(manifest.account_id)} does not match ${JSON.stringify(expectedAccountID)}`,
    );
  }
  if (options.evacuationID) {
    const legacyAllowed =
      options.allowLegacyEvacuationID === true &&
      Number.isInteger(manifest.schema_version) &&
      manifest.schema_version < 70 &&
      (manifest.evacuation_id === undefined ||
        manifest.evacuation_id === null ||
        manifest.evacuation_id === "");
    if (
      manifest.evacuation_id !== options.evacuationID &&
      !legacyAllowed
    ) {
      throw new ArchiveIntegrityError(
        "manifest evacuation_id does not match the lifecycle epoch",
      );
    }
  }
  if (manifest.compression !== "gzip") {
    throw new ArchiveIntegrityError("manifest compression is not gzip");
  }
  if (!Array.isArray(manifest.tables)) {
    throw new ArchiveIntegrityError("manifest tables is not an array");
  }
  const tables = new Set();
  for (const table of manifest.tables) {
    if (
      typeof table !== "string" ||
      !/^[a-z][a-z0-9_]*$/.test(table) ||
      tables.has(table)
    ) {
      throw new ArchiveIntegrityError(
        `manifest contains invalid or duplicate table ${JSON.stringify(table)}`,
      );
    }
    tables.add(table);
  }
  if (!tables.has("accounts")) {
    throw new ArchiveIntegrityError(
      "manifest is missing canonical accounts table",
    );
  }
  return tables;
}

function parseChunkName(name) {
  const slash = name.indexOf("/");
  if (slash <= 0 || !name.endsWith(".ndjson")) {
    throw new ArchiveIntegrityError(`unexpected tar entry ${JSON.stringify(name)}`);
  }
  const table = name.slice(0, slash);
  const sequenceText = name.slice(slash + 1, -".ndjson".length);
  if (!/^[0-9]{6}$/.test(sequenceText)) {
    throw new ArchiveIntegrityError(`unexpected tar entry ${JSON.stringify(name)}`);
  }
  const sequence = Number.parseInt(sequenceText, 10);
  if (sequence < 1) {
    throw new ArchiveIntegrityError(`unexpected tar entry ${JSON.stringify(name)}`);
  }
  return { table, sequence };
}

function validateChecksums(checksums, manifest, seen) {
  if (!Array.isArray(checksums.chunks)) {
    throw new ArchiveIntegrityError("checksums.json chunks is not an array");
  }
  if (
    !checksums.table_rows ||
    Array.isArray(checksums.table_rows) ||
    typeof checksums.table_rows !== "object"
  ) {
    throw new ArchiveIntegrityError(
      "checksums.json table_rows is not an object",
    );
  }

  const verified = new Set();
  const trailerRowsByTable = new Map();
  for (const expected of checksums.chunks) {
    if (!expected || Array.isArray(expected) || typeof expected !== "object") {
      throw new ArchiveIntegrityError("checksums.json contains an invalid chunk");
    }
    if (
      typeof expected.name !== "string" ||
      !/^[0-9a-f]{64}$/.test(expected.sha256) ||
      !Number.isSafeInteger(expected.bytes) ||
      expected.bytes < 0 ||
      !Number.isSafeInteger(expected.rows) ||
      expected.rows < 0
    ) {
      throw new ArchiveIntegrityError(
        `checksums.json contains invalid metadata for ${JSON.stringify(expected.name)}`,
      );
    }
    if (verified.has(expected.name)) {
      throw new ArchiveIntegrityError(
        `checksums.json lists ${expected.name} more than once`,
      );
    }
    const actual = seen.get(expected.name);
    if (!actual) {
      throw new ArchiveIntegrityError(
        `checksums.json references missing chunk ${expected.name}`,
      );
    }
    if (
      actual.sha256 !== expected.sha256 ||
      actual.bytes !== expected.bytes ||
      actual.rows !== expected.rows
    ) {
      throw new ArchiveIntegrityError(
        `chunk ${expected.name} does not match checksums.json`,
      );
    }
    const tableRows =
      (trailerRowsByTable.get(actual.table) ?? 0) + expected.rows;
    if (!Number.isSafeInteger(tableRows)) {
      throw new ArchiveIntegrityError(
        `checksums.json row total for ${actual.table} is out of range`,
      );
    }
    trailerRowsByTable.set(actual.table, tableRows);
    verified.add(expected.name);
  }
  for (const name of seen.keys()) {
    if (!verified.has(name)) {
      throw new ArchiveIntegrityError(
        `chunk ${name} is not covered by checksums.json`,
      );
    }
  }

  const tableNames = new Set(manifest.tables);
  for (const [table, rows] of Object.entries(checksums.table_rows)) {
    if (!tableNames.has(table)) {
      throw new ArchiveIntegrityError(
        `checksums.json counts unknown table ${table}`,
      );
    }
    if (!Number.isSafeInteger(rows) || rows < 0) {
      throw new ArchiveIntegrityError(
        `checksums.json has an invalid row count for ${table}`,
      );
    }
  }
  for (const table of manifest.tables) {
    if (!Object.hasOwn(checksums.table_rows, table)) {
      throw new ArchiveIntegrityError(
        `checksums.json is missing table ${table}`,
      );
    }
    const trailerRows = trailerRowsByTable.get(table) ?? 0;
    if (checksums.table_rows[table] !== trailerRows) {
      throw new ArchiveIntegrityError(
        `table ${table} chunk rows total ${trailerRows}, checksums.json says ${checksums.table_rows[table]}`,
      );
    }
  }
  if (checksums.table_rows.accounts !== 1) {
    throw new ArchiveIntegrityError(
      `checksums.json table_rows.accounts is ${JSON.stringify(checksums.table_rows.accounts)}, want 1`,
    );
  }
}

// validateAccountArchive streams the committed gzip object through a strict
// reader. It verifies gzip completion, tar header checksums and terminators,
// the leading manifest, ordered chunks, every chunk SHA-256/byte length, and a
// final checksums.json with internally consistent row metadata and no later
// entries. The cell's transactional importer remains responsible for semantic
// row counting. Memory is bounded by the two small control records plus one
// decompressor-supplied chunk.
export async function validateAccountArchive(
  stream,
  expectedAccountID,
  options = {},
) {
  if (typeof DecompressionStream !== "function") {
    throw new ArchiveReadError("gzip decompression is unavailable");
  }

  const source = monitorArchiveBody(stream);
  let decompressed;
  try {
    decompressed = source.stream.pipeThrough(
      new DecompressionStream("gzip"),
    );
  } catch (error) {
    throw new ArchiveReadError(`could not open gzip stream: ${error.message}`, {
      cause: error,
    });
  }
  const reader = new StreamReader(decompressed, (error) => {
    const sourceFailure = source.readFailure();
    if (sourceFailure !== null) {
      return new ArchiveReadError(
        `archive body read failed: ${String(
          sourceFailure?.message ?? sourceFailure,
        )}`,
        { cause: sourceFailure },
      );
    }
    // The source completed or remained readable, so a decompressor read error
    // is deterministic evidence that the committed gzip bytes are invalid.
    return new ArchiveIntegrityError(
      `invalid gzip stream: ${String(error?.message ?? error)}`,
      { cause: error },
    );
  });
  let manifest = null;
  let manifestTables = null;
  let checksumsSeen = false;
  let trailerSHA256 = null;
  let entries = 0;
  let tableIndex = 0;
  let nextChunk = 0;
  const seen = new Map();

  try {
    let zeroBlocks = 0;
    while (true) {
      const block = await reader.readExactly(TAR_BLOCK_BYTES, true);
      if (block === null) {
        throw new ArchiveIntegrityError(
          zeroBlocks > 0
            ? "truncated tar terminator"
            : "tar stream ended before its terminator",
        );
      }
      if (allZero(block)) {
        zeroBlocks++;
        if (zeroBlocks < 2) continue;
        while (true) {
          const trailing = await reader.readAtMost(64 * 1024);
          if (trailing === null) break;
          if (!allZero(trailing)) {
            throw new ArchiveIntegrityError(
              "non-zero bytes follow the tar terminator",
            );
          }
        }
        break;
      }
      if (zeroBlocks > 0) {
        throw new ArchiveIntegrityError(
          "tar contains an entry after a zero terminator block",
        );
      }
      if (checksumsSeen) {
        throw new ArchiveIntegrityError("tar contains entries after checksums.json");
      }
      entries++;
      if (entries > MAX_ARCHIVE_ENTRIES) {
        throw new ArchiveIntegrityError(
          `archive exceeds the ${MAX_ARCHIVE_ENTRIES}-entry limit`,
        );
      }
      const entry = parseTarHeader(block);

      if (entries === 1) {
        if (entry.name !== "manifest.json") {
          throw new ArchiveIntegrityError(
            `first tar entry is ${JSON.stringify(entry.name)}, want manifest.json`,
          );
        }
        const raw = await consumeEntry(reader, entry, null, true);
        manifest = parseJSONRecord(raw, "manifest.json");
        manifestTables = validateManifest(
          manifest,
          expectedAccountID,
          options,
        );
        continue;
      }

      if (entry.name === "manifest.json") {
        throw new ArchiveIntegrityError("manifest.json appears more than once");
      }
      if (entry.name === "checksums.json") {
        const raw = await consumeEntry(reader, entry, null, true);
        const checksums = parseJSONRecord(raw, "checksums.json");
        validateChecksums(checksums, manifest, seen);
        trailerSHA256 = new SHA256().update(raw).hexDigest();
        checksumsSeen = true;
        continue;
      }
      if (!manifest) {
        throw new ArchiveIntegrityError("archive has no manifest.json");
      }

      const { table, sequence } = parseChunkName(entry.name);
      if (!manifestTables.has(table)) {
        throw new ArchiveIntegrityError(
          `chunk ${entry.name} belongs to a table absent from manifest.json`,
        );
      }
      while (
        tableIndex < manifest.tables.length &&
        manifest.tables[tableIndex] !== table
      ) {
        tableIndex++;
        nextChunk = 0;
      }
      if (tableIndex === manifest.tables.length) {
        throw new ArchiveIntegrityError(
          `table ${table} is out of manifest order`,
        );
      }
      nextChunk++;
      if (sequence !== nextChunk) {
        throw new ArchiveIntegrityError(
          `chunk ${entry.name} is out of sequence; want ${String(nextChunk).padStart(6, "0")}`,
        );
      }
      if (seen.has(entry.name)) {
        throw new ArchiveIntegrityError(`duplicate chunk ${entry.name}`);
      }

      const hash = createStreamingSHA256();
      let lastByte = -1;
      let rows = 0;
      try {
        await consumeEntry(reader, entry, async (piece) => {
          await hash.update(piece);
          for (const byte of piece) {
            if (byte === 0x0a) rows++;
          }
          if (piece.length > 0) lastByte = piece[piece.length - 1];
        });
      } catch (error) {
        await hash.abort(error);
        throw error;
      }
      if (entry.size === 0 || lastByte !== 0x0a) {
        await hash.abort(new Error("empty or unterminated NDJSON chunk"));
        throw new ArchiveIntegrityError(
          `chunk ${entry.name} is empty or has an unterminated row`,
        );
      }
      seen.set(entry.name, {
        table,
        sha256: await hash.hexDigest(),
        bytes: entry.size,
        rows,
      });
    }

    if (!manifest) {
      throw new ArchiveIntegrityError("archive has no manifest.json");
    }
    if (!checksumsSeen) {
      throw new ArchiveIntegrityError(
        "archive is truncated: checksums.json is missing",
      );
    }
    return {
      manifest,
      entries,
      chunks: seen.size,
      trailer_sha256: trailerSHA256,
    };
  } catch (error) {
    await reader.cancel(error);
    if (
      error instanceof ArchiveIntegrityError ||
      error instanceof ArchiveReadError
    ) {
      throw error;
    }
    // Parser violations above are deliberately typed ArchiveIntegrityError.
    // Anything else is an internal/ambiguous validator failure and must stay
    // retryable rather than becoming permanent quarantine evidence.
    throw error;
  }
}

// validateCommittedAccountArchive re-reads an existing object without
// mutating it. This is used when an archived: pointer already exists: a
// corrupt legacy pointer must never authorize deletion of a still-live acct:
// route, but the validator also must not destroy operator evidence or an
// object that predates attempt-isolated keys.
export async function validateCommittedAccountArchive(
  bucket,
  objectKey,
  expectedAccountID,
  options = {},
) {
  const object = await bucket.get(objectKey);
  if (!object?.body) {
    throw new ArchiveIntegrityError(
      `committed archive ${objectKey} is not readable from R2`,
    );
  }
  return validateAccountArchive(object.body, expectedAccountID, options);
}

// An existing archived: pointer may predate the integrity gate. It can retire
// a still-live route only after the object validates and the route continues
// to name the evacuation source cell. Keeping this ordering in a testable seam
// prevents an idempotency shortcut from turning corrupt legacy state or a
// later restore into route deletion.
export async function retireSourceRouteForExistingArchive({
  directory,
  bucket,
  accountID,
  cellName,
  archived,
}) {
  if (
    !archived ||
    archived.cell !== cellName ||
    typeof archived.object !== "string" ||
    archived.object.length === 0
  ) {
    throw new ArchiveIntegrityError(
      `archive pointer for ${accountID} does not belong to ${cellName}`,
    );
  }
  await validateCommittedAccountArchive(
    bucket,
    archived.object,
    accountID,
  );
  const routed = await directory.get(
    `acct:${accountID}`,
    { type: "json" },
  );
  if (!routed) {
    return { alreadyRetired: true };
  }
  if (routed.cell !== cellName) {
    throw new ArchiveIntegrityError(
      `acct:${accountID} now routes to ${routed.cell} — refusing to retire it from ${cellName}`,
    );
  }
  await directory.delete(`acct:${accountID}`);
  return { retired: true };
}

// verifyCommittedAccountArchive is deliberately called after multipart
// complete() and before archived:/acct: changes. A malformed object created
// by this attempt is removed from R2 and rejected; the live routing pointer
// therefore remains authoritative. Because the object key is unique to this
// attempt, cleanup cannot delete a valid archive created by another attempt.
export async function verifyCommittedAccountArchive(
  bucket,
  objectKey,
  expectedAccountID,
  options = {},
) {
  try {
    return await validateCommittedAccountArchive(
      bucket,
      objectKey,
      expectedAccountID,
      options,
    );
  } catch (error) {
    let cleanupError = null;
    try {
      await bucket.delete(objectKey);
    } catch (deleteError) {
      cleanupError = deleteError;
    }
    const reason = error instanceof ArchiveIntegrityError
      ? error.message
      : String(error?.message ?? error);
    const cleanup = cleanupError
      ? `; R2 cleanup also failed: ${String(cleanupError?.message ?? cleanupError)}`
      : "";
    throw new ArchiveIntegrityError(
      `archive integrity validation failed: ${reason}${cleanup}`,
      { cause: error },
    );
  }
}
