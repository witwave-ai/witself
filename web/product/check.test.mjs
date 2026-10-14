import assert from 'node:assert/strict';
import test from 'node:test';
import {validateSite} from './check.mjs';

const catalog = {plans: [{id: 'free', price_monthly: 0}]};
function fixture(production = true) {
  const files = new Map(Object.entries({
    'index.html': `<meta name="robots" content="${production ? 'index, follow, max-snippet:-1' : 'noindex, nofollow'}">\n` +
      (production ? '<link rel="canonical" href="https://witself.com/">\n' : '') +
      '<script type="application/ld+json">{"@context":"https://schema.org"}</script>',
    'styles.css': 'body {}', 'app.js': '// app', 'favicon.svg': '<svg/>',
    'product.md': '# Product', 'llms.txt': 'https://witself.com/product.md',
    'plans.json': JSON.stringify(catalog),
    'robots.txt': production ? 'User-agent: *\nAllow: /\n\nSitemap: https://witself.com/sitemap.xml\n' : 'User-agent: *\nDisallow: /\n',
  }).map(([name, value]) => [name, Buffer.from(value)]));
  if (production) files.set('sitemap.xml', Buffer.from('<urlset><url><loc>https://witself.com/</loc></url></urlset>'));
  // A header fixture tests the output format; screenshot verification separately
  // checks the real asset hashes, dimensions, and rendering-source freshness.
  const png = Buffer.alloc(24);
  Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]).copy(png);
  png.write('IHDR', 12);
  png.writeUInt32BE(1440, 16);
  png.writeUInt32BE(1000, 20);
  for (const name of ['agent-console.png', 'agent-tui.png']) files.set(`assets/screenshots/${name}`, png);
  return files;
}
const check = (files, production = true) => validateSite(files, {production, catalog});

test('preview rejects all routes to accidental indexing', () => {
  check(fixture(false), false);
  for (const [name, value, error] of [
    ['index.html', '<meta name="robots" content="index, follow">', /Preview must be noindex/],
    ['index.html', `${fixture(false).get('index.html')}<link rel="canonical" href="https://witself.com/">`, /must not advertise a canonical/],
    ['robots.txt', 'User-agent: *\nAllow: /\n', /must disallow crawling/],
    ['sitemap.xml', '<urlset/>', /must not publish a sitemap/],
  ]) {
    const files = fixture(false);
    files.set(name, Buffer.from(value));
    assert.throws(() => check(files, false), error);
  }
});

test('production rejects missing discovery, pricing drift, and invalid assets', () => {
  check(fixture());
  for (const [name, value, error] of [
    ['index.html', '<meta name="robots" content="noindex, nofollow">', /canonical URL is missing/],
    ['robots.txt', 'User-agent: *\nDisallow: /\n', /must allow crawling/],
    ['sitemap.xml', '<urlset/>', /must identify the production homepage/],
    ['llms.txt', 'https://example.com/product.md', /must link to the product guide/],
    ['plans.json', '{"plans":[]}', /must match the shared catalog/],
    ['assets/screenshots/agent-console.png', '<html>challenge</html>', /Expected a PNG/],
    ['assets/screenshots/agent-tui.png', null, /Missing site file/],
  ]) {
    const files = fixture();
    if (value === null) files.delete(name);
    else files.set(name, Buffer.from(value));
    assert.throws(() => check(files), error);
  }
  const files = fixture();
  files.set('index.html', Buffer.from(files.get('index.html').toString().replace('{"@context":"https://schema.org"}', '{bad json')));
  assert.throws(() => check(files), SyntaxError);
});
