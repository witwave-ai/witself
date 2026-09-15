import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import test from "node:test";

const repository = new URL("../../../../", import.meta.url);
const published = JSON.parse(readFileSync(
  new URL("internal/legal/testdata/published-versions.json", repository),
  "utf8",
));

// Load the production Worker with the same Markdown bytes that Wrangler's
// Text imports bundle. Requests run directly through fetch; no server or
// external service is needed.
const workerURL = new URL("web/legal/index.js", repository);
const source = readFileSync(workerURL, "utf8").replace(
  /^import (\w+) from "([^"]+\.md)";$/gm,
  (_, binding, path) => `const ${binding} = ${JSON.stringify(
    readFileSync(new URL(path, workerURL), "utf8"),
  )};`,
);
const { default: worker } = await import(
  `data:text/javascript;base64,${Buffer.from(source).toString("base64")}`
);

function sha256(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

for (const slug of ["privacy", "dpa"]) {
  test(`${slug}: published version identifies served text and preserves prior terms`, async () => {
    const fixture = published[slug];
    const versions = Object.keys(fixture.versions).sort();
    const currentVersion = versions.at(-1);
    assert.ok(versions.length >= 2, "retain the prior published version fingerprint");

    const manifestResponse = await worker.fetch(new Request(
      "https://self.witwave.ai/legal/versions.json",
    ));
    assert.equal(manifestResponse.status, 200);
    const manifest = await manifestResponse.json();
    assert.equal(manifest[slug].version, currentVersion);
    assert.equal(manifest[slug].path, `/legal/${slug}`);

    const current = readFileSync(new URL(`docs/legal/${fixture.file}`, repository));
    for (const request of [
      new Request(`https://self.witwave.ai/legal/${slug}?format=md`),
      new Request(`https://self.witwave.ai/legal/${slug}`, {
        headers: { accept: "text/markdown" },
      }),
    ]) {
      const response = await worker.fetch(request);
      assert.equal(response.status, 200);
      assert.match(response.headers.get("content-type"), /^text\/markdown\b/);
      const served = Buffer.from(await response.arrayBuffer());
      assert.deepEqual(served, current, "serve the exact published document bytes");
      assert.ok(served.toString("utf8").includes(
        `**Version ${manifest[slug].version} · `,
      ));
      // This fails when the terms change under an existing version, even
      // when the document header and manifest still agree with each other.
      assert.equal(
        sha256(served), fixture.versions[manifest[slug].version],
        `${slug} text changed under a published version; assign a new version and preserve its predecessor`,
      );
    }

    for (const version of versions.slice(0, -1)) {
      const archived = readFileSync(new URL(
        `docs/legal/versions/${version}/${fixture.file}`, repository,
      ));
      assert.equal(
        sha256(archived), fixture.versions[version],
        `${slug} ${version} must preserve the exact prior published text`,
      );
      assert.ok(archived.toString("utf8").includes(`**Version ${version} · `));
      assert.notDeepEqual(archived, current);
    }
  });
}
