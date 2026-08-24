export const MIME_BODY_MAX_BYTES = 64 * 1024;
export const MIME_SUBJECT_MAX_CHARS = 200;

const MAX_MULTIPART_DEPTH = 8;
const MAX_MULTIPART_PARTS = 64;
const textEncoder = new TextEncoder();
const utf8Decoder = new TextDecoder("utf-8", { fatal: true });

function bytesOf(value) {
  if (value instanceof Uint8Array) return value;
  if (value instanceof ArrayBuffer) return new Uint8Array(value);
  if (typeof value === "string") return textEncoder.encode(value);
  return null;
}

function byteString(bytes) {
  let value = "";
  for (let offset = 0; offset < bytes.length; offset += 0x8000) {
    value += String.fromCharCode(...bytes.subarray(offset, offset + 0x8000));
  }
  return value;
}

function latin1Bytes(value) {
  const bytes = new Uint8Array(value.length);
  for (let index = 0; index < value.length; index += 1) {
    bytes[index] = value.charCodeAt(index) & 0xff;
  }
  return bytes;
}

function decodeText(bytes) {
  try {
    return utf8Decoder.decode(bytes);
  } catch {
    return byteString(bytes);
  }
}

function headerBoundary(bytes) {
  for (let index = 0; index < bytes.length; index += 1) {
    if (
      bytes[index] === 13 && bytes[index + 1] === 10 &&
      bytes[index + 2] === 13 && bytes[index + 3] === 10
    ) {
      return { headerEnd: index, bodyStart: index + 4 };
    }
    if (bytes[index] === 10 && bytes[index + 1] === 10) {
      return { headerEnd: index, bodyStart: index + 2 };
    }
  }
  return null;
}

function parseHeaders(bytes) {
  const boundary = headerBoundary(bytes);
  if (boundary === null) return null;
  const block = byteString(bytes.subarray(0, boundary.headerEnd))
    .replaceAll("\r\n", "\n");
  if (block.includes("\r")) return null;
  const fields = new Map();
  let name = "";
  let value = "";

  function finish() {
    if (!name) return;
    const values = fields.get(name) ?? [];
    values.push(value.trim());
    fields.set(name, values);
  }

  for (const line of block.split("\n")) {
    if (/^[ \t]/.test(line)) {
      if (!name) return null;
      value += ` ${line.trim()}`;
      continue;
    }
    finish();
    const colon = line.indexOf(":");
    if (colon < 1) return null;
    name = line.slice(0, colon).toLowerCase();
    if (!/^[\x21-\x39\x3b-\x7e]+$/.test(name)) return null;
    value = line.slice(colon + 1).trim();
  }
  finish();
  return {
    body: bytes.subarray(boundary.bodyStart),
    fields,
  };
}

function firstHeader(entity, name) {
  return entity.fields.get(name)?.[0] ?? "";
}

function splitParameters(value) {
  const output = [];
  let start = 0;
  let quoted = false;
  let escaped = false;
  for (let index = 0; index < value.length; index += 1) {
    const character = value[index];
    if (escaped) {
      escaped = false;
    } else if (quoted && character === "\\") {
      escaped = true;
    } else if (character === "\"") {
      quoted = !quoted;
    } else if (!quoted && character === ";") {
      output.push(value.slice(start, index));
      start = index + 1;
    }
  }
  if (quoted || escaped) return null;
  output.push(value.slice(start));
  return output;
}

function contentType(value) {
  if (!value) return { mediaType: "text/plain", boundary: "" };
  const segments = splitParameters(value);
  if (segments === null) return null;
  const mediaType = segments.shift().trim().toLowerCase();
  if (!/^[a-z0-9!#$&^_.+-]+\/[a-z0-9!#$&^_.+-]+$/.test(mediaType)) return null;
  let boundary = "";
  for (const segment of segments) {
    const equals = segment.indexOf("=");
    if (equals < 1) continue;
    const name = segment.slice(0, equals).trim().toLowerCase();
    let parameter = segment.slice(equals + 1).trim();
    if (parameter.startsWith("\"")) {
      if (!parameter.endsWith("\"") || parameter.length < 2) return null;
      parameter = parameter.slice(1, -1).replace(/\\([\\"])/g, "$1");
    }
    if (name === "boundary" && boundary === "") boundary = parameter;
  }
  if (boundary.length > 200 || /[\r\n\0]/.test(boundary)) return null;
  return { mediaType, boundary };
}

function decodeQuotedPrintable(bytes) {
  const output = [];
  for (let index = 0; index < bytes.length; index += 1) {
    if (bytes[index] !== 0x3d) {
      output.push(bytes[index]);
      continue;
    }
    if (bytes[index + 1] === 13 && bytes[index + 2] === 10) {
      index += 2;
      continue;
    }
    if (bytes[index + 1] === 10) {
      index += 1;
      continue;
    }
    const encoded = byteString(bytes.subarray(index + 1, index + 3));
    if (/^[0-9a-f]{2}$/i.test(encoded)) {
      output.push(Number.parseInt(encoded, 16));
      index += 2;
      continue;
    }
    output.push(bytes[index]);
  }
  return Uint8Array.from(output);
}

function decodeBase64(bytes) {
  let encoded = byteString(bytes).replace(/[\t\r\n ]/g, "");
  if (encoded.length % 4 === 1 || !/^[A-Za-z0-9+/]*={0,2}$/.test(encoded) ||
      /=/.test(encoded.slice(0, -2))) return null;
  encoded += "=".repeat((4 - (encoded.length % 4)) % 4);
  try {
    const decoded = atob(encoded);
    return latin1Bytes(decoded);
  } catch {
    return null;
  }
}

function decodedBody(entity) {
  const encoding = firstHeader(entity, "content-transfer-encoding")
    .trim().toLowerCase();
  if (encoding === "base64") return decodeBase64(entity.body);
  if (encoding === "quoted-printable") return decodeQuotedPrintable(entity.body);
  return entity.body;
}

function multipartParts(bytes, boundary) {
  if (!boundary) return [];
  const marker = `--${boundary}`;
  const closing = `${marker}--`;
  const lines = byteString(bytes).replaceAll("\r\n", "\n").split("\n");
  const parts = [];
  let current = null;
  const encodedPart = () => latin1Bytes(
    `${current.join("\n")}${current.at(-1) === "" ? "\n" : ""}`,
  );
  for (const line of lines) {
    const candidate = line.replace(/[ \t]+$/, "");
    if (candidate === marker || candidate === closing) {
      if (current !== null) parts.push(encodedPart());
      if (parts.length >= MAX_MULTIPART_PARTS || candidate === closing) {
        current = null;
        break;
      }
      current = [];
      continue;
    }
    if (current !== null) current.push(line);
  }
  if (current !== null && parts.length < MAX_MULTIPART_PARTS) {
    parts.push(encodedPart());
  }
  return parts;
}

function firstPlainBody(entity, depth = 0) {
  if (depth > MAX_MULTIPART_DEPTH) return null;
  const type = contentType(firstHeader(entity, "content-type"));
  if (type === null) return null;
  if (type.mediaType.startsWith("multipart/")) {
    for (const rawPart of multipartParts(entity.body, type.boundary)) {
      const part = parseHeaders(rawPart);
      if (part === null) continue;
      const body = firstPlainBody(part, depth + 1);
      if (body !== null) return body;
    }
    return null;
  }
  if (type.mediaType !== "text/plain") return null;
  const decoded = decodedBody(entity);
  return decoded === null ? null : decodeText(decoded);
}

function decodeEncodedWord(charset, encoding, value) {
  let bytes;
  if (encoding.toLowerCase() === "b") {
    bytes = decodeBase64(latin1Bytes(value));
  } else {
    bytes = decodeQuotedPrintable(latin1Bytes(value.replaceAll("_", " ")));
  }
  if (bytes === null || !charset) return null;
  return decodeText(bytes);
}

function decodeSubject(value) {
  const pattern = /=\?([^?\s]+)\?([bqBQ])\?([^?]*)\?=/g;
  let output = "";
  let offset = 0;
  let match;
  while ((match = pattern.exec(value)) !== null) {
    output += decodeText(latin1Bytes(value.slice(offset, match.index)));
    const decoded = decodeEncodedWord(match[1], match[2], match[3]);
    output += decoded === null ? decodeText(latin1Bytes(match[0])) : decoded;
    offset = match.index + match[0].length;
  }
  output += decodeText(latin1Bytes(value.slice(offset)));
  return output;
}

function sanitizedText(value) {
  return value.replaceAll("\r\n", "\n")
    .replace(/[\u0000-\u0008\u000b-\u001f]/gu, "");
}

function truncateUTF8(value, maximumBytes) {
  const encoded = textEncoder.encode(value);
  if (encoded.byteLength <= maximumBytes) return value;
  let end = maximumBytes;
  while (end > 0) {
    try {
      return utf8Decoder.decode(encoded.subarray(0, end));
    } catch {
      end -= 1;
    }
  }
  return "";
}

function truncateCharacters(value, maximumCharacters) {
  return Array.from(value).slice(0, maximumCharacters).join("");
}

// extractMimeText returns the first decoded text/plain body and the decoded
// top-level subject. HTML-only and malformed MIME messages return null.
export function extractMimeText(rawMessage) {
  try {
    const bytes = bytesOf(rawMessage);
    if (bytes === null) return null;
    const entity = parseHeaders(bytes);
    if (entity === null) return null;
    const body = firstPlainBody(entity);
    if (body === null) return null;
    const subject = sanitizedText(decodeSubject(firstHeader(entity, "subject")));
    return Object.freeze({
      subject: truncateCharacters(subject, MIME_SUBJECT_MAX_CHARS),
      body: truncateUTF8(sanitizedText(body), MIME_BODY_MAX_BYTES),
    });
  } catch {
    return null;
  }
}
