import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp, mkdir, writeFile, rm, symlink} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join, dirname} from 'node:path';
import {sourceDigest, verifyScreenshots, sha256, imageNames} from './verify.mjs';

test('UI changes invalidate screenshots; ordinary brochure copy and tests do not', async t => {
  const root = await mkdtemp(join(tmpdir(), 'product-digest-test-'));
  t.after(() => rm(root, {recursive: true, force: true}));
  async function file(path, bytes = 'fixture') {
    await mkdir(dirname(join(root, path)), {recursive: true});
    await writeFile(join(root, path), bytes);
  }
  for (const path of ['internal/agenttui/view.go', 'internal/dashboard/static/app.js',
    'web/product/screenshots/capture.mjs', 'go.mod', 'go.sum',
    'scripts/dashboard-acceptance/package.json', 'scripts/dashboard-acceptance/package-lock.json']) await file(path);
  const before = await sourceDigest(root);
  await file('web/product/content.mjs', 'new brochure copy');
  await file('internal/agenttui/view_test.go', 'new test');
  assert.equal(await sourceDigest(root), before);
  await file('internal/dashboard/static/app.js', 'new actual UI');
  assert.notEqual(await sourceDigest(root), before);
  await symlink(join(root, 'go.mod'), join(root, 'internal/agenttui/unsafe.go'));
  await assert.rejects(sourceDigest(root), /must not be symlinks/);
});

test('asset verification rejects stale sources and tampered image bytes', async t => {
  const dir = await mkdtemp(join(tmpdir(), 'product-manifest-test-'));
  t.after(() => rm(dir, {recursive: true, force: true}));
  // Minimal header fixture exercises metadata integrity only, not visual validity.
  const header = Buffer.alloc(32);
  Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]).copy(header);
  header.write('IHDR', 12);
  header.writeUInt32BE(1440, 16);
  header.writeUInt32BE(1000, 20);
  const manifest = {schema: 'witself.product-screenshots.v1', fixture: 'synthetic-demo-only', source_digest: await sourceDigest(), images: {}};
  for (const name of imageNames) {
    await writeFile(join(dir, name), header);
    manifest.images[name] = {width: 1440, height: 1000, sha256: sha256(header)};
  }
  const save = () => writeFile(join(dir, 'manifest.json'), JSON.stringify(manifest));
  await save();
  await verifyScreenshots(dir);
  const changed = Buffer.from(header); changed[31] = 1;
  await writeFile(join(dir, imageNames[0]), changed);
  await assert.rejects(verifyScreenshots(dir), /changed without capture/);
  await writeFile(join(dir, imageNames[0]), header);
  manifest.source_digest = 'stale';
  await save();
  await assert.rejects(verifyScreenshots(dir), /screenshots are stale/);
});
