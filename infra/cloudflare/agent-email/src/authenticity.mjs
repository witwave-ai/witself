const MAX_HEADER_BYTES = 64 * 1024;
const MAX_AUTHENTICATION_RESULTS_HEADERS = 64;
const AUTHENTICATION_RESULTS = "authentication-results";

const DEFAULT_RESULTS = Object.freeze({
  spf: "unknown",
  dkim: "unknown",
  dmarc: "none",
});
const RESULT_DEFAULTS = {
  spf: "unknown",
  dkim: "unknown",
  dmarc: "none",
};
const RESULT_VALUES = {
  spf: new Set([
    "unknown", "none", "neutral", "pass", "fail", "softfail",
    "temperror", "permerror",
  ]),
  dkim: new Set([
    "unknown", "none", "neutral", "pass", "fail", "policy",
    "temperror", "permerror",
  ]),
  dmarc: new Set([
    "unknown", "none", "pass", "fail", "temperror", "permerror",
  ]),
};

const textEncoder = new TextEncoder();

function boundedBytes(value) {
  const maximumBytes = MAX_HEADER_BYTES + 4;
  if (typeof value === "string") {
    return textEncoder.encode(value.slice(0, maximumBytes)).subarray(0, maximumBytes);
  }
  if (value instanceof Uint8Array) return value.subarray(0, maximumBytes);
  return null;
}

function headerBlockBytes(bytes) {
  for (let index = 0; index <= MAX_HEADER_BYTES && index < bytes.length; index += 1) {
    if (
      bytes[index] === 13 &&
      bytes[index + 1] === 10 &&
      bytes[index + 2] === 13 &&
      bytes[index + 3] === 10
    ) {
      return index === 0 ? null : bytes.subarray(0, index);
    }
    if (bytes[index] === 10 && bytes[index + 1] === 10) {
      return index === 0 ? null : bytes.subarray(0, index);
    }
  }
  return null;
}

function byteString(bytes) {
  let value = "";
  for (let offset = 0; offset < bytes.length; offset += 0x8000) {
    value += String.fromCharCode(...bytes.subarray(offset, offset + 0x8000));
  }
  return value;
}

function trustedAuthservID(value) {
  if (typeof value !== "string" || value.length < 1 || value.length > 255) return null;
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index);
    if (code < 0x21 || code > 0x7e || value[index] === ";") return null;
  }
  return value.toLowerCase();
}

function isWhitespace(character) {
  return character === " " || character === "\t";
}

function skipWhitespace(value, index, end = value.length) {
  while (index < end && isWhitespace(value[index])) index += 1;
  return index;
}

function authservIDMatches(value, trustedID) {
  let index = skipWhitespace(value, 0);
  const start = index;
  while (index < value.length && value[index] !== ";" && !isWhitespace(value[index])) {
    index += 1;
  }
  if (index === start || index - start !== trustedID.length) return false;
  return value.slice(start, index).toLowerCase() === trustedID;
}

function payloadStart(value) {
  let index = skipWhitespace(value, 0);
  while (index < value.length && value[index] !== ";" && !isWhitespace(value[index])) {
    index += 1;
  }
  index = skipWhitespace(value, index);
  if (index < value.length && value[index] >= "0" && value[index] <= "9") {
    while (index < value.length && value[index] >= "0" && value[index] <= "9") {
      index += 1;
    }
    index = skipWhitespace(value, index);
  }
  return value[index] === ";" ? index + 1 : -1;
}

function validHeaderName(value) {
  if (value.length === 0) return false;
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index);
    if (code < 0x21 || code > 0x7e || code === 0x3a) return false;
  }
  return true;
}

function validHeaderLine(value) {
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index);
    if ((code < 0x20 && code !== 0x09) || code === 0x7f) return false;
  }
  return true;
}

// Authentication-Results fields stay individual and ordered. A malformed first
// trusted field is still the selected field; later fields can never replace it.
function selectedAuthenticationResults(block, trustedID) {
  const normalized = byteString(block).replaceAll("\r\n", "\n");
  if (normalized.includes("\r")) return null;

  let currentName = null;
  let currentValue = "";
  let authenticationResultsCount = 0;
  let selected = null;

  function finishField() {
    if (currentName !== AUTHENTICATION_RESULTS) return;
    authenticationResultsCount += 1;
    if (
      authenticationResultsCount <= MAX_AUTHENTICATION_RESULTS_HEADERS &&
      selected === null &&
      authservIDMatches(currentValue, trustedID)
    ) {
      selected = currentValue;
    }
  }

  for (const line of normalized.split("\n")) {
    if (line.length === 0 || !validHeaderLine(line)) return null;
    if (isWhitespace(line[0])) {
      if (currentName === null) return null;
      currentValue += line;
      continue;
    }

    finishField();
    const colon = line.indexOf(":");
    if (colon < 1) return null;
    const name = line.slice(0, colon);
    if (!validHeaderName(name)) return null;
    currentName = name.toLowerCase();
    currentValue = line.slice(colon + 1);
  }
  finishField();
  return selected;
}

function skipComment(value, index, end) {
  let depth = 0;
  let escaped = false;
  for (; index < end; index += 1) {
    const character = value[index];
    if (escaped) {
      escaped = false;
    } else if (character === "\\") {
      escaped = true;
    } else if (character === "(") {
      depth += 1;
    } else if (character === ")") {
      depth -= 1;
      if (depth === 0) return index + 1;
    }
  }
  return -1;
}

function skipCFWS(value, index, end) {
  while (index < end) {
    index = skipWhitespace(value, index, end);
    if (value[index] !== "(") return index;
    index = skipComment(value, index, end);
    if (index < 0) return -1;
  }
  return index;
}

function isTokenCharacter(character) {
  const code = character?.charCodeAt(0);
  return (
    code >= 0x21 &&
    code <= 0x7e &&
    !"()<>@,;:\\\"/[]?=".includes(character)
  );
}

function skipQuotedString(value, index, end) {
  let escaped = false;
  for (index += 1; index < end; index += 1) {
    const character = value[index];
    if (escaped) {
      escaped = false;
    } else if (character === "\\") {
      escaped = true;
    } else if (character === "\"") {
      return index + 1;
    }
  }
  return -1;
}

function validTrailingProperties(value, index, end) {
  while (index < end) {
    index = skipCFWS(value, index, end);
    if (index < 0 || index === end) return index === end;

    const nameStart = index;
    while (index < end && isTokenCharacter(value[index])) index += 1;
    if (index === nameStart) return false;
    index = skipCFWS(value, index, end);
    if (index < 0 || value[index] !== "=") return false;
    index = skipCFWS(value, index + 1, end);
    if (index < 0 || index === end) return false;

    if (value[index] === "\"") {
      index = skipQuotedString(value, index, end);
      if (index < 0) return false;
    } else {
      const valueStart = index;
      while (
        index < end &&
        !isWhitespace(value[index]) &&
        value[index] !== "(" &&
        value[index] !== "\"" &&
        value[index] !== "\\"
      ) {
        index += 1;
      }
      if (index === valueStart) return false;
    }
  }
  return true;
}

function parseMethodSegment(value, start, end) {
  let index = skipCFWS(value, start, end);
  if (index < 0 || index >= end) return null;

  const methodStart = index;
  while (index < end && isTokenCharacter(value[index])) index += 1;
  if (index === methodStart) return null;
  const method = value.slice(methodStart, index).toLowerCase();
  index = skipCFWS(value, index, end);
  if (index < 0) return null;

  if (method === "none" && index === end) return { type: "none" };
  if (method === "none") return null;

  let supportedVersion = true;

  if (value[index] === "/") {
    index = skipCFWS(value, index + 1, end);
    if (index < 0) return null;
    const versionStart = index;
    while (index < end && value[index] >= "0" && value[index] <= "9") index += 1;
    const version = value.slice(versionStart, index);
    index = skipCFWS(value, index, end);
    if (version.length === 0 || index < 0) return null;
    supportedVersion = version === "1";
  }

  if (value[index] !== "=") return null;
  index = skipCFWS(value, index + 1, end);
  if (index < 0 || index === end) return null;
  const resultStart = index;
  while (index < end && isTokenCharacter(value[index])) index += 1;
  if (index === resultStart || !validTrailingProperties(value, index, end)) return null;

  if (!Object.hasOwn(RESULT_VALUES, method)) return { type: "other" };
  const token = value.slice(resultStart, index).toLowerCase();
  const result = supportedVersion && RESULT_VALUES[method].has(token)
    ? token
    : RESULT_DEFAULTS[method];
  return { type: "method", method, result };
}

// Split only at top-level semicolons so quoted reasons, comments, and property
// values cannot be mistaken for independently attested method results.
function methodResults(value, start) {
  const results = { ...RESULT_DEFAULTS };
  const seen = new Set();
  const ambiguous = new Set();
  let segmentStart = start;
  let segmentCount = 0;
  let sawNone = false;
  let quote = false;
  let commentDepth = 0;
  let escaped = false;

  function consumeSegment(end) {
    const parsed = parseMethodSegment(value, segmentStart, end);
    if (parsed === null) return false;
    segmentCount += 1;
    if (parsed.type === "none") {
      if (segmentCount !== 1) return false;
      sawNone = true;
      return true;
    }
    if (sawNone) return false;
    if (parsed.type !== "method") return true;
    if (seen.has(parsed.method)) {
      ambiguous.add(parsed.method);
      results[parsed.method] = RESULT_DEFAULTS[parsed.method];
    } else {
      seen.add(parsed.method);
      results[parsed.method] = parsed.result;
    }
    return true;
  }

  for (let index = start; index < value.length; index += 1) {
    const character = value[index];
    if (escaped) {
      escaped = false;
    } else if (quote) {
      if (character === "\\") escaped = true;
      else if (character === "\"") quote = false;
    } else if (commentDepth > 0) {
      if (character === "\\") escaped = true;
      else if (character === "(") commentDepth += 1;
      else if (character === ")") commentDepth -= 1;
    } else if (character === "\"") {
      quote = true;
    } else if (character === "(") {
      commentDepth = 1;
    } else if (character === ")") {
      return null;
    } else if (character === ";") {
      if (!consumeSegment(index)) return null;
      segmentStart = index + 1;
    }
  }
  if (quote || commentDepth > 0 || escaped || !consumeSegment(value.length)) return null;
  if (sawNone && segmentCount !== 1) return null;
  for (const method of ambiguous) results[method] = RESULT_DEFAULTS[method];
  return results;
}

export function extractAuthenticationVerdicts(rawMessage, trustedAuthservIDValue) {
  try {
    const trustedID = trustedAuthservID(trustedAuthservIDValue);
    const bytes = boundedBytes(rawMessage);
    if (trustedID === null || bytes === null) return DEFAULT_RESULTS;
    const block = headerBlockBytes(bytes);
    if (block === null) return DEFAULT_RESULTS;
    const selected = selectedAuthenticationResults(block, trustedID);
    if (selected === null) return DEFAULT_RESULTS;
    const start = payloadStart(selected);
    if (start < 0) return DEFAULT_RESULTS;
    const results = methodResults(selected, start);
    return results === null ? DEFAULT_RESULTS : Object.freeze(results);
  } catch {
    return DEFAULT_RESULTS;
  }
}
