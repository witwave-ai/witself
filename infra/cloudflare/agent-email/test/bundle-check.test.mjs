import assert from "node:assert/strict";
import test from "node:test";

import {
  normalizedMetafile,
  normalizedSourceMap,
} from "../scripts/bundle-check.mjs";

test("bundle attestation removes temporary paths without changing source content", () => {
  const repositoryRoot = "/workspace/witself";
  const source = `${repositoryRoot}/infra/cloudflare/agent-email/src/index.js`;
  const firstRoot = "/tmp/bundle-first";
  const secondRoot = "/tmp/bundle-second";
  const sourceMap = (outputRoot) => JSON.stringify({
    version: 3,
    sources: [source],
    sourceRoot: outputRoot,
    sourcesContent: ["export default {};\n"],
    mappings: "AAAA",
    names: [],
  });

  const first = normalizedSourceMap(sourceMap(firstRoot), {
    repositoryRoot,
    outputRoot: firstRoot,
  });
  const second = normalizedSourceMap(sourceMap(secondRoot), {
    repositoryRoot,
    outputRoot: secondRoot,
  });
  assert.equal(first, second);
  assert.deepEqual(JSON.parse(first).sources, [
    "infra/cloudflare/agent-email/src/index.js",
  ]);
  assert.equal(JSON.parse(first).sourceRoot, ".");
});

test("bundle metafile attestation normalizes only output paths", () => {
  const metafile = (root) => JSON.stringify({
    inputs: { "src/index.js": { bytes: 10, imports: [] } },
    outputs: {
      [`${root}/index.js.map`]: { bytes: 20, imports: [], exports: [] },
      [`${root}/index.js`]: {
        bytes: 10,
        imports: [],
        exports: ["default"],
        entryPoint: "src/index.js",
      },
    },
  });
  assert.equal(
    normalizedMetafile(metafile("/tmp/first")),
    normalizedMetafile(metafile("/tmp/second")),
  );
  assert.deepEqual(Object.keys(JSON.parse(normalizedMetafile(
    metafile("/tmp/first"),
  )).outputs), ["index.js", "index.js.map"]);
});

test("bundle source-map normalization refuses sources outside the repository", () => {
  assert.throws(() => normalizedSourceMap(JSON.stringify({
    version: 3,
    sources: ["/tmp/untrusted.js"],
    sourceRoot: "/tmp/bundle",
    sourcesContent: ["bad"],
    mappings: "",
    names: [],
  }), {
    repositoryRoot: "/workspace/witself",
    outputRoot: "/tmp/bundle",
  }), /outside the repository/);
});
