import assert from 'node:assert/strict';
import {existsSync, readFileSync, rmSync, statSync} from 'node:fs';
import {join, resolve} from 'node:path';
import {spawnSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import {assertWranglerVersion} from '../../infra/cloudflare/wrangler-version.mjs';
import {imageNames, pngSize} from './screenshots/verify.mjs';

const repoRoot = fileURLToPath(new URL('../../', import.meta.url));
const siteURL = 'https://witself.com/';
const requiredFiles = [
  'index.html', 'styles.css', 'app.js', 'favicon.svg', 'product.md', 'llms.txt',
  'plans.json', 'robots.txt', ...imageNames.map(name => `assets/screenshots/${name}`),
];

// Kept separate from the command runner so the indexing and discovery contract
// can be tested with deliberately broken output, without deploying anything.
export function validateSite(files, {production, catalog}) {
  for (const name of [...requiredFiles, ...(production ? ['sitemap.xml'] : [])]) {
    assert(files.has(name) && files.get(name).length > 0, `Missing site file: ${name}`);
  }
  const read = name => files.get(name).toString('utf8');
  const html = read('index.html');
  if (production) {
    assert(html.includes(`<link rel="canonical" href="${siteURL}">`), 'Production canonical URL is missing');
    assert(html.includes('<meta name="robots" content="index, follow, max-snippet:-1">'), 'Production robots metadata is missing');
    assert(!html.includes('noindex'), 'Production must not contain noindex');
    assert.equal(read('robots.txt'), `User-agent: *\nAllow: /\n\nSitemap: ${siteURL}sitemap.xml\n`, 'Production robots.txt must allow crawling and identify the sitemap');
    assert(read('sitemap.xml').includes(`<loc>${siteURL}</loc>`), 'Sitemap must identify the production homepage');
    assert(read('llms.txt').includes(`${siteURL}product.md`), 'AI reading index must link to the product guide');
  } else {
    assert(html.includes('<meta name="robots" content="noindex, nofollow">'), 'Preview must be noindex, nofollow');
    assert(!html.includes('rel="canonical"'), 'Preview must not advertise a canonical URL');
    assert.equal(read('robots.txt'), 'User-agent: *\nDisallow: /\n', 'Preview robots.txt must disallow crawling');
    assert(!files.has('sitemap.xml'), 'Preview must not publish a sitemap');
  }
  assert.deepEqual(JSON.parse(read('plans.json')), catalog, 'Published plans must match the shared catalog');
  const structuredData = html.match(/<script type="application\/ld\+json">([\s\S]*?)<\/script>/);
  assert(structuredData, 'Product structured data is missing');
  JSON.parse(structuredData[1]);
  for (const name of imageNames) pngSize(files.get(`assets/screenshots/${name}`));
}

function readSiteFiles(out) {
  const files = new Map();
  for (const name of [...requiredFiles, 'sitemap.xml']) {
    const path = join(out, name);
    if (existsSync(path)) {
      assert(statSync(path).isFile(), `Expected a site file: ${name}`);
      files.set(name, readFileSync(path));
    }
  }
  return files;
}

function run(command, args) {
  const result = spawnSync(command, args, {
    cwd: repoRoot,
    stdio: 'inherit',
    env: {...process.env, WRANGLER_SEND_METRICS: 'false'},
  });
  if (result.error) throw result.error;
  assert.equal(result.status, 0, `${command} failed${result.signal ? ` (${result.signal})` : ''}`);
}

export async function checkProductSite() {
  const out = join(repoRoot, 'web/product/dist');
  let catalog;
  const build = args => run(process.execPath, ['web/product/build.mjs', ...args]);
  try {
    catalog = JSON.parse(readFileSync(join(repoRoot, 'web/plans/plans.json'), 'utf8'));
    for (const file of ['build.mjs', 'content.mjs', 'app.js', 'check.mjs']) {
      run(process.execPath, ['--check', `web/product/${file}`]);
    }
    run(process.execPath, ['--test', 'web/product/check.test.mjs', 'web/product/screenshots/verify.test.mjs']);
    build([]); // Both builds also verify screenshot freshness and image hashes.
    validateSite(readSiteFiles(out), {production: false, catalog});
    build(['--site-url', siteURL, '--production']);
    const productionFiles = readSiteFiles(out);
    validateSite(productionFiles, {production: true, catalog});
    for (const name of imageNames) {
      assert(productionFiles.get(`assets/screenshots/${name}`).equals(
        readFileSync(join(repoRoot, 'web/product/assets/screenshots', name))),
      `Built screenshot differs from the reviewed asset: ${name}`);
    }
    // Dependencies are installed explicitly by the developer or CI. Never
    // install packages, request credentials, or publish from this check.
    await assertWranglerVersion('control-plane');
    run(process.execPath, [
      'infra/cloudflare/control-plane/node_modules/wrangler/bin/wrangler.js', 'deploy',
      '--config', 'web/product/wrangler.toml',
      '--env-file', 'infra/cloudflare/wrangler-production-empty.env', '--dry-run',
    ]);
  } finally {
    try {
      build([]);
      validateSite(readSiteFiles(out), {production: false, catalog});
    } catch (error) {
      // If a source/build failure prevents restoration, remove the generated
      // output rather than leave an indexable production build in the preview.
      rmSync(out, {recursive: true, force: true});
      throw new Error('Could not restore the safe preview; removed web/product/dist.', {cause: error});
    }
  }
  console.log('check-product-site: all gates green (dry run only; safe preview restored)');
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await checkProductSite();
}
