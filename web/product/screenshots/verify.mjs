import assert from 'node:assert/strict';
import {createHash} from 'node:crypto';
import {readFile, readdir} from 'node:fs/promises';
import {resolve, relative, join} from 'node:path';
import {fileURLToPath} from 'node:url';

export const repoRoot = fileURLToPath(new URL('../../../', import.meta.url));
export const assetDir = join(repoRoot, 'web/product/assets/screenshots');
export const imageNames = ['agent-console.png', 'agent-tui.png'];
export const sha256 = bytes => createHash('sha256').update(bytes).digest('hex');

// Only presentation, fixture, capture and locked renderer inputs invalidate the
// photographs. Ordinary brochure copy edits do not need Go or a browser.
export async function sourceDigest(root = repoRoot) {
  const files = [];
  async function walk(dir, accept) {
    for (const entry of await readdir(join(root, dir), {withFileTypes: true})) {
      const path = `${dir}/${entry.name}`;
      if (entry.name === 'node_modules') continue;
      assert(!entry.isSymbolicLink(), 'Screenshot inputs must not be symlinks');
      if (entry.isDirectory()) await walk(path, accept);
      else if (accept(path)) files.push(path);
    }
  }
  const productionGo = path => path.endsWith('.go') && !path.endsWith('_test.go');
  await walk('internal/agenttui', productionGo);
  await walk('internal/dashboard', path => productionGo(path) || path.startsWith('internal/dashboard/static/'));
  await walk('web/product/screenshots', path => /\.(mjs|go|json)$/.test(path) && !/(_test\.go|\.test\.mjs)$/.test(path));
  files.push('go.mod', 'go.sum', 'scripts/dashboard-acceptance/package.json', 'scripts/dashboard-acceptance/package-lock.json');
  const hash = createHash('sha256');
  for (const path of files.sort()) {
    hash.update(path + '\0');
    hash.update(await readFile(join(root, path)));
    hash.update('\0');
  }
  return hash.digest('hex');
}

export function pngSize(bytes) {
  assert(bytes.subarray(0, 8).equals(Buffer.from([137, 80, 78, 71, 13, 10, 26, 10])), 'Expected a PNG screenshot');
  assert(bytes.subarray(12, 16).toString() === 'IHDR', 'Missing PNG dimensions');
  return {width: bytes.readUInt32BE(16), height: bytes.readUInt32BE(20)};
}

export async function verifyScreenshots(dir = assetDir) {
  const manifest = JSON.parse(await readFile(join(dir, 'manifest.json'), 'utf8'));
  assert.equal(manifest.schema, 'witself.product-screenshots.v1');
  assert.equal(manifest.fixture, 'synthetic-demo-only');
  assert.equal(manifest.source_digest, await sourceDigest(),
    'Console screenshots are stale. Run node web/product/screenshots/capture.mjs and review the refreshed images.');
  assert.deepEqual(Object.keys(manifest.images).sort(), [...imageNames].sort());
  for (const name of imageNames) {
    const bytes = await readFile(join(dir, name));
    const {width, height} = pngSize(bytes);
    assert.equal(sha256(bytes), manifest.images[name].sha256, `Screenshot changed without capture: ${name}`);
    assert.equal(width, manifest.images[name].width);
    assert.equal(height, manifest.images[name].height);
    assert(width >= 800 && height >= 500, 'Screenshot resolution is too small');
  }
  return manifest;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await verifyScreenshots();
  console.log(`Verified current console screenshots in ${relative(repoRoot, assetDir)}`);
}
